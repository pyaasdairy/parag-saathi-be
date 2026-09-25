package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// UPI AUTOPAY / e-MANDATE: SMART RECHARGE THAT FUNDS THE WALLET.
//
// Founder decision 1 (25 Sep 2026): AutoPay FUNDS the prepaid wallet; it never
// takes money out of it. The wallet is the retention engine, and an empty
// wallet at 5 AM is the main reason a daily customer churns. One Razorpay
// recurring mandate (a UPI AutoPay token) both tops the wallet up and, through
// the wallet, pays the Rs 99 Founding Family seat (founding.go).
//
// Registration (this file): POST /mandate/create makes (live) a Razorpay
// customer and a UPI registration order carrying the `token` object
// (max_amount = the member's per-debit cap, frequency as_presented: Smart
// Recharge debits when the wallet needs it, not on a calendar). The app runs
// the recurring checkout (key, order_id, customer_id, recurring "1") and
// posts the result to POST /mandate/verify. The registration payment is REAL
// money: it is CREDITED TO THE WALLET as a top-up, exactly once, keyed by the
// registration order id - by verify, by the payment.captured / order.paid
// webhook or by the reconciliation sweep, whichever lands first (the same
// ledger gate every top-up uses; the order is recorded with purpose
// "autopay"). Before this it was captured into nowhere.
//
// Smart Recharge (autopay.go): once the bank confirms the token, the server
// charges the mandate (POST /orders + POST /payments/create/recurring) when
// the member's wallet, less what the next locked and upcoming days will
// take, falls below their threshold, and credits the wallet only when the
// charge is CAPTURED.
//
// LIVE RAZORPAY NOTE: recurring payments must be ENABLED on the merchant
// account. With no key secret and OTP dev mode on, an OFFLINE seam mints a
// mock registration id so create -> verify -> pause/cancel is exercisable
// without moving money; nothing is ever charged without keys.

const collMandates = "consumer_mandates"

// mandatePlans are the plans the app may name at registration. Smart
// Recharge charges when the wallet needs money, not on this cadence; the
// plan is kept because the app sends it (and validates it).
var mandatePlans = map[string]time.Duration{
	"daily":     24 * time.Hour,
	"alternate": 48 * time.Hour, // every 2nd day — matches the FE cadence
	"weekly":    7 * 24 * time.Hour,
}

// mandateTransitions is the mandate state machine: current status → the set of
// statuses it may move to. `cancelled` is terminal (no outgoing edges). The
// initial status on create is `pending`; verify promotes it to `active`.
var mandateTransitions = map[string]map[string]bool{
	"pending":   {"active": true, "cancelled": true},
	"active":    {"paused": true, "cancelled": true},
	"paused":    {"active": true, "cancelled": true},
	"cancelled": {},
}

// mandateCanTransition reports whether `from`→`to` is a legal mandate move.
func mandateCanTransition(from, to string) bool {
	return mandateTransitions[from][to]
}

// mandateActionTarget maps a pause/resume/cancel action to its target status.
func mandateActionTarget(action string) (string, bool) {
	switch action {
	case "pause":
		return "paused", true
	case "resume":
		return "active", true
	case "cancel":
		return "cancelled", true
	default:
		return "", false
	}
}

// ── Document + wire shapes ──────────────────────────────────────────────────

// mandate is a consumer's recurring-payment authorization. Stored in
// consumer_mandates; scoped to the owning shopper. Amount is the Smart
// Recharge amount (what one top-up adds) and the registration payment;
// MaxAmount is the per-debit cap the member approved at their bank.
type mandate struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"          json:"-"`
	MandateID  string             `bson:"mandate_id"             json:"id"` // public id "mnd_…"
	ConsumerID primitive.ObjectID `bson:"consumer_id"            json:"-"`
	Plan       string             `bson:"plan"                   json:"plan"`   // daily | alternate | weekly (kept for the app; charges follow the wallet, not the plan)
	Status     string             `bson:"status"                 json:"status"` // pending|active|paused|cancelled
	Amount     float64            `bson:"amount"                 json:"amount"` // rupees one top-up adds
	MaxAmount  float64            `bson:"max_amount"             json:"max_amount"`
	// Threshold: Smart Recharge tops up when the wallet, less what the next
	// locked and upcoming days will take, falls below this (rupees).
	Threshold  float64 `bson:"threshold,omitempty"    json:"threshold,omitempty"`
	RegOrderID string  `bson:"reg_order_id,omitempty" json:"order_id,omitempty"` // Razorpay registration order id (signature anchor; the checkout's order_id)
	// RegAmountPaise is what the registration order charges (and the wallet
	// is credited): the recurring checkout's amount.
	RegAmountPaise int64 `bson:"reg_amount_paise,omitempty" json:"reg_amount_paise,omitempty"`
	// RzpCustomerID is the Razorpay customer the token belongs to (the
	// checkout's customer_id; every recurring charge names it).
	RzpCustomerID string `bson:"rzp_customer_id,omitempty" json:"customer_id,omitempty"`
	Token         string `bson:"token,omitempty"        json:"token,omitempty"` // recurring token id (verify, the payment, or the webhook)
	// TokenStatus is the bank's word on the token (Razorpay
	// recurring_details.status: initiated | confirmed | rejected | paused |
	// cancelled). Smart Recharge charges a confirmed token only.
	TokenStatus    string     `bson:"token_status,omitempty"     json:"token_status,omitempty"`
	TokenCheckedAt *time.Time `bson:"token_checked_at,omitempty" json:"-"`
	PaymentID      string     `bson:"payment_id,omitempty"   json:"-"`
	// TopupFailures counts Smart Recharge charges in a row the bank refused;
	// LastFailureDay (IST) holds the next automatic try to the next day.
	// Reset by a captured charge, a resume or a policy change.
	TopupFailures  int    `bson:"topup_failures,omitempty"   json:"topup_failures,omitempty"`
	LastFailureDay string `bson:"last_failure_day,omitempty" json:"-"`
	// CancelReason says who ended a cancelled mandate: the member (empty),
	// the bank ("bank_cancelled", "bank_rejected") or an erased account.
	CancelReason string `bson:"cancel_reason,omitempty" json:"cancel_reason,omitempty"`
	// Legacy fields of the retired wallet-debit schedule: read, never written.
	NextCharge     *time.Time `bson:"next_charge,omitempty"  json:"next_charge,omitempty"`
	LastChargeDate string     `bson:"last_charge_date,omitempty" json:"last_charge_date,omitempty"`
	LastChargeAt   *time.Time `bson:"last_charge_at,omitempty"   json:"last_charge_at,omitempty"`
	CreatedAt      time.Time  `bson:"created_at"             json:"created_at"`
	UpdatedAt      time.Time  `bson:"updated_at"             json:"updated_at"`
}

// effectiveThreshold is the member's threshold, or the default for a
// mandate registered before thresholds were stored.
func (m *mandate) effectiveThreshold() float64 {
	if m.Threshold > 0 {
		return m.Threshold
	}
	return autopayDefaultThreshold
}

// mandateOrderView is POST /consumer/mandate/create — what the app hands to
// Razorpay's recurring checkout: key, order_id (= token, the old name),
// customer_id and the amount. Additive keys over {id, token, keyId, status}.
type mandateOrderView struct {
	ID          string `json:"id"`                    // mandate id "mnd_…"
	Token       string `json:"token"`                 // registration order id (dev seam: a mock)
	KeyID       string `json:"keyId"`                 // Razorpay key id (empty in the dev seam)
	Status      string `json:"status"`                // "pending"
	OrderID     string `json:"orderId,omitempty"`     // the checkout's order_id (same as token)
	CustomerID  string `json:"customerId,omitempty"`  // the checkout's customer_id (live only)
	AmountPaise int64  `json:"amountPaise,omitempty"` // the registration payment, credited to the wallet
}

func newMandateID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "mnd_" + hex.EncodeToString(b)
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) ensureMandateIndexes(ctx context.Context) error {
	specs := []mongo.IndexModel{
		{Keys: bson.D{{Key: "mandate_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}}},
	}
	if _, err := r.mandates.Indexes().CreateMany(ctx, specs); err != nil {
		return err
	}
	return r.ensureAutopayIndexes(ctx)
}

func (r *repository) insertMandate(ctx context.Context, m *mandate) error {
	if _, err := r.mandates.InsertOne(ctx, m); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("MANDATE_EXISTS", "mandate already exists")
		}
		return errInternal("mandate create failed")
	}
	return nil
}

func (r *repository) findMandate(ctx context.Context, mandateID string, consumerID primitive.ObjectID) (*mandate, error) {
	var m mandate
	err := r.mandates.FindOne(ctx, bson.D{{Key: "mandate_id", Value: mandateID}, {Key: "consumer_id", Value: consumerID}}).Decode(&m)
	if isNoDocs(err) {
		return nil, errNotFound("mandate not found")
	}
	if err != nil {
		return nil, errInternal("mandate lookup failed")
	}
	return &m, nil
}

func (r *repository) listMandates(ctx context.Context, consumerID primitive.ObjectID) ([]mandate, error) {
	cur, err := r.mandates.Find(ctx, bson.D{{Key: "consumer_id", Value: consumerID}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100))
	if err != nil {
		return nil, errInternal("mandates lookup failed")
	}
	out := []mandate{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("mandates decode failed")
	}
	return out, nil
}

// transitionMandate applies a status move ATOMICALLY, guarded on the expected
// `from` status so a concurrent transition can't race (the guard admits the
// update only while the mandate is still in `from`). set carries any extra
// fields for the move. Returns the fresh doc, or a 409 if the guard failed.
func (r *repository) transitionMandate(ctx context.Context, mandateID string, consumerID primitive.ObjectID, from, to string, set bson.D) (*mandate, error) {
	set = append(set, bson.E{Key: "status", Value: to}, bson.E{Key: "updated_at", Value: time.Now().UTC()})
	after := options.After
	var m mandate
	err := r.mandates.FindOneAndUpdate(ctx,
		bson.D{{Key: "mandate_id", Value: mandateID}, {Key: "consumer_id", Value: consumerID}, {Key: "status", Value: from}},
		bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&m)
	if isNoDocs(err) {
		return nil, errConflict("MANDATE_STATE", "mandate is no longer in the expected state")
	}
	if err != nil {
		return nil, errInternal("mandate update failed")
	}
	return &m, nil
}

// ── Razorpay recurring seam ─────────────────────────────────────────────────

// autopayTokenFrequency is the token's debit cadence. Smart Recharge charges
// when the wallet needs money, not on a calendar, so the token is
// "as_presented" whatever plan the app names (a "daily" token allows one
// debit a day on a fixed cycle, which a top-up does not follow).
const autopayTokenFrequency = "as_presented"

// autopayDefaultThreshold is the Smart Recharge line when the member names
// none: the app's own default (lib/autoTopup DEFAULTS.threshold).
const autopayDefaultThreshold = 200.0

// autopayTokenLifeYears is how long the bank keeps the token (Razorpay's
// default is 10 years); the member can cancel it any day, here or in their
// UPI app.
const autopayTokenLifeYears = 10

// rzpCustomer makes (or, fail_existing "0", finds) the member's Razorpay
// customer: the token belongs to it and every recurring charge names it.
func (s *service) rzpCustomer(ctx context.Context, consumerID primitive.ObjectID) (string, error) {
	acct, err := s.repo.findAccountByID(ctx, consumerID)
	if err != nil || acct == nil {
		return "", errNotFound("account not found")
	}
	name := "PYAAS member"
	if acct.FullName != nil && strings.TrimSpace(*acct.FullName) != "" {
		name = strings.TrimSpace(*acct.FullName)
	}
	body := map[string]any{"name": name, "contact": acct.Phone, "fail_existing": "0",
		"notes": map[string]string{"consumer_id": consumerID.Hex()}}
	if acct.Email != nil && strings.TrimSpace(*acct.Email) != "" {
		body["email"] = strings.TrimSpace(*acct.Email)
	}
	var out struct {
		ID string `json:"id"`
	}
	status, err := s.rzpCall(ctx, http.MethodPost, "/customers", body, &out)
	if err != nil || out.ID == "" {
		if status == 0 {
			return "", errInternal("payment gateway unreachable")
		}
		return "", errInternal(fmt.Sprintf("payment gateway rejected the customer (%d)", status))
	}
	return out.ID, nil
}

// createRzpMandate registers the recurring authorization and returns its
// registration order id (and, live, the Razorpay customer id). Dev seam (no
// secret + OTP dev mode) mints a mock id; live creates a UPI registration
// order carrying the `token` object (requires recurring payments enabled on
// the merchant account). The registration order charges m.RegAmountPaise,
// which the wallet is credited with once the payment is verified or captured.
func (s *service) createRzpMandate(ctx context.Context, m *mandate, receipt string, now time.Time) (regOrderID, customerID string, err error) {
	if s.rzpKeySecret == "" {
		if !s.deps.Cfg.OTPDevMode {
			return "", "", errInternal("recurring payments are not configured")
		}
		b := make([]byte, 8)
		if _, e := rand.Read(b); e != nil {
			return "", "", errInternal("mandate token generation failed")
		}
		return "token_dev_" + hex.EncodeToString(b), "", nil
	}
	customerID, err = s.rzpCustomer(ctx, m.ConsumerID)
	if err != nil {
		return "", "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	status, e := s.rzpCall(ctx, http.MethodPost, "/orders", map[string]any{
		"amount":          m.RegAmountPaise,
		"currency":        "INR",
		"receipt":         receipt,
		"customer_id":     customerID,
		"method":          "upi",
		"payment_capture": true,
		"token": map[string]any{
			"max_amount": rupeesToPaise(m.MaxAmount),
			"expire_at":  now.AddDate(autopayTokenLifeYears, 0, 0).Unix(),
			"frequency":  autopayTokenFrequency,
		},
		"notes": map[string]string{"mandate_id": m.MandateID, "purpose": "autopay_registration"},
	}, &out)
	if e != nil || out.ID == "" {
		if status == 0 {
			return "", "", errInternal("payment gateway unreachable")
		}
		return "", "", errInternal(fmt.Sprintf("payment gateway rejected mandate (%d) — is recurring enabled on the account?", status))
	}
	return out.ID, customerID, nil
}

// rzpPaymentToken is the recurring token a registration payment created
// (GET /payments/{id} carries token_id), or "" when it cannot be read: the
// payment.captured webhook and the token lookup fill it later.
func (s *service) rzpPaymentToken(ctx context.Context, paymentID string) string {
	if s.rzpKeySecret == "" || paymentID == "" {
		return ""
	}
	var out struct {
		TokenID string `json:"token_id"`
	}
	if _, err := s.rzpCall(ctx, http.MethodGet, "/payments/"+url.PathEscape(paymentID), nil, &out); err != nil {
		return ""
	}
	return out.TokenID
}

// rupeesToPaise converts a rupee amount (paise-rounded) to integer paise.
func rupeesToPaise(r float64) int64 { return int64(math.Round(round2(r) * 100)) }

// ── Validation ──────────────────────────────────────────────────────────────

const (
	mandateAmountMin = 1.0
	mandateAmountMax = 5000.0   // one top-up (and the registration payment)
	mandateMaxCap    = 100000.0 // the per-debit authorization ceiling
	// mandateThresholdMax bounds the Smart Recharge threshold.
	mandateThresholdMax = 50000.0
)

func validateMandate(plan string, amount, maxAmount float64) *apiError {
	if _, ok := mandatePlans[plan]; !ok {
		return errBadRequest("plan must be one of: daily, alternate, weekly")
	}
	if amount < mandateAmountMin || amount > mandateAmountMax {
		return errBadRequest("amount must be between ₹1 and ₹5,000")
	}
	if maxAmount < amount || maxAmount > mandateMaxCap {
		return errBadRequest("max_amount must be at least the charge amount and at most ₹1,00,000")
	}
	return nil
}

// validateThreshold: 0 means "the default"; otherwise up to ₹50,000.
func validateThreshold(threshold float64) *apiError {
	if threshold < 0 || threshold > mandateThresholdMax {
		return errBadRequest("threshold must be between ₹0 and ₹50,000")
	}
	return nil
}

// ── Service ─────────────────────────────────────────────────────────────────

// createMandate registers a recurring authorization and returns what the
// app hands to Razorpay's recurring checkout. The mandate starts `pending`;
// verifyMandate promotes it to `active` after the registration payment is
// signature-verified (and credits that payment to the wallet). The
// registration order is recorded as an "autopay" payment order, so the
// webhook and the reconciliation sweep can credit it too when the app never
// comes back from the UPI app. One live mandate per member: an active or
// paused one refuses a second (409 MANDATE_EXISTS); an older pending one the
// member never approved is cancelled.
func (s *service) createMandate(ctx context.Context, consumerID primitive.ObjectID, plan string, amount, maxAmount, threshold float64) (mandateOrderView, error) {
	amount = round2(amount)
	if maxAmount <= 0 {
		maxAmount = amount // default the cap to a single charge
	}
	maxAmount = round2(maxAmount)
	if verr := validateMandate(plan, amount, maxAmount); verr != nil {
		return mandateOrderView{}, verr
	}
	threshold = round2(threshold)
	if verr := validateThreshold(threshold); verr != nil {
		return mandateOrderView{}, verr
	}
	if threshold == 0 {
		threshold = autopayDefaultThreshold
	}
	existing, err := s.repo.listMandates(ctx, consumerID)
	if err != nil {
		return mandateOrderView{}, err
	}
	for i := range existing {
		if st := existing[i].Status; st == "active" || st == "paused" {
			return mandateOrderView{}, errConflict("MANDATE_EXISTS", "AutoPay is already set up. Change its amounts or cancel it first.")
		}
	}
	now := s.now().UTC()
	m := &mandate{
		ID: primitive.NewObjectID(), MandateID: newMandateID(), ConsumerID: consumerID,
		Plan: plan, Status: "pending", Amount: amount, MaxAmount: maxAmount, Threshold: threshold,
		RegAmountPaise: rupeesToPaise(amount),
		CreatedAt:      now, UpdatedAt: now,
	}
	receipt := "mnd_" + m.MandateID
	regOrderID, customerID, err := s.createRzpMandate(ctx, m, receipt, now)
	if err != nil {
		return mandateOrderView{}, err
	}
	m.RegOrderID, m.RzpCustomerID = regOrderID, customerID
	// The registration payment is wallet money: bound here, like a top-up
	// order, so every credit path pays exactly this amount to this member.
	if err := s.repo.insertPaymentOrder(ctx, &paymentOrder{
		ID: primitive.NewObjectID(), OrderID: regOrderID, ConsumerID: consumerID,
		AmountPaise: m.RegAmountPaise, Receipt: receipt, Purpose: "autopay", RefID: autopayRegRefPrefix + m.MandateID,
		Status: "CREATED", CreatedAt: now,
	}); err != nil {
		return mandateOrderView{}, err
	}
	if err := s.repo.insertMandate(ctx, m); err != nil {
		return mandateOrderView{}, err
	}
	// A pending mandate the member never approved is superseded by this one.
	for i := range existing {
		if existing[i].Status == "pending" {
			_, _ = s.repo.transitionMandate(ctx, existing[i].MandateID, consumerID, "pending", "cancelled",
				bson.D{{Key: "cancel_reason", Value: "superseded"}})
		}
	}
	return mandateOrderView{
		ID: m.MandateID, Token: regOrderID, KeyID: s.rzpKeyID, Status: m.Status,
		OrderID: regOrderID, CustomerID: customerID, AmountPaise: m.RegAmountPaise,
	}, nil
}

// verifyMandate authoritatively verifies the registration payment and
// activates the mandate (pending→active). The signature is
// HMAC(regOrderId|paymentId), the same scheme as wallet/verify. A verified
// registration payment is CREDITED TO THE WALLET first (the money gate,
// keyed by the registration order id and shared with the webhook and the
// reconcile sweep, so whichever lands first credits it and the rest are
// no-ops), and that credit puts the mandate live with its recurring token
// (autopayRegistrationCaptured, the same step the webhook and the sweep
// take). A bad signature never activates, never credits and never 500s. The
// dev seam (no secret, OTP dev mode) activates without a signature, as a
// stand-in for the recurring checkout, and credits only a payment signed
// with the dev secret.
func (s *service) verifyMandate(ctx context.Context, consumerID primitive.ObjectID, mandateID, paymentID, signature, token string) (*mandate, error) {
	m, err := s.repo.findMandate(ctx, mandateID, consumerID)
	if err != nil {
		return nil, err
	}
	if m.Status == "active" {
		return m, nil // already verified — idempotent
	}
	if !mandateCanTransition(m.Status, "active") {
		return nil, errConflict("MANDATE_STATE", "mandate cannot be activated from its current state")
	}
	signed := s.verifyRzpSignature(m.RegOrderID, paymentID, signature)
	if !signed && !s.rzpDevMode() {
		return nil, errBadRequest("mandate signature verification failed")
	}
	token = strings.TrimSpace(token)
	if signed {
		// The money gate: the registration payment is wallet money, and its
		// credit activates the mandate.
		if _, err := s.creditCapturedPaymentToken(ctx, m.RegOrderID, paymentID, token, 0, "mandate_verify"); err != nil {
			return nil, err // transient: the app's retry (or the webhook) credits it
		}
		if cur, err := s.repo.findMandate(ctx, mandateID, consumerID); err == nil && cur.Status == "active" {
			return cur, nil
		}
	}
	if token == "" {
		token = s.rzpPaymentToken(ctx, paymentID)
	}
	set := bson.D{{Key: "payment_id", Value: paymentID}}
	if token != "" {
		set = append(set, bson.E{Key: "token", Value: token})
	}
	return s.repo.transitionMandate(ctx, mandateID, consumerID, m.Status, "active", set)
}

// autopayRegRefPrefix names an AutoPay REGISTRATION payment order (RefID =
// prefix + mandate id); a Smart Recharge charge's RefID is its autopayRef.
const autopayRegRefPrefix = "autopay:reg:"

// autopayRegistrationCaptured runs when an AutoPay registration payment is
// credited (creditCapturedPaymentToken: verify, the payment.captured /
// order.paid webhook or the reconcile sweep, whichever lands first, and every
// repeat). The bank mandate exists now, so:
//
//   - a PENDING mandate goes ACTIVE with the payment and its bank token (the
//     payment's token_id, the app's, one already noted, or GET
//     /payments/{id}), guarded on pending. A member whose app never came back
//     from the UPI app is no longer left "waiting for approval" with a live
//     bank mandate and their money taken;
//   - any other mandate that holds no token yet records it (once).
//
// A Smart Recharge charge's order is not a registration and is left alone.
// An error is transient only.
func (s *service) autopayRegistrationCaptured(ctx context.Context, ord *paymentOrder, paymentID, tokenID string) error {
	mandateID, ok := strings.CutPrefix(ord.RefID, autopayRegRefPrefix)
	if !ok || mandateID == "" {
		return nil
	}
	m, err := s.repo.findMandate(ctx, mandateID, ord.ConsumerID)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			return nil
		}
		return err
	}
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		tokenID = m.Token
	}
	if tokenID == "" {
		tokenID = s.rzpPaymentToken(ctx, paymentID)
	}
	now := s.now().UTC()
	if m.Status == "pending" {
		set := bson.D{{Key: "status", Value: "active"}, {Key: "updated_at", Value: now}}
		if paymentID != "" {
			set = append(set, bson.E{Key: "payment_id", Value: paymentID})
		}
		if tokenID != "" {
			set = append(set, bson.E{Key: "token", Value: tokenID})
		}
		res, err := s.repo.mandates.UpdateOne(ctx,
			bson.D{{Key: "mandate_id", Value: m.MandateID}, {Key: "status", Value: "pending"}},
			bson.D{{Key: "$set", Value: set}})
		if err != nil {
			return errInternal("mandate update failed")
		}
		if res.ModifiedCount == 1 {
			s.log.InfoContext(ctx, "autopay: registration captured, mandate active", "mandate", m.MandateID, "token_known", tokenID != "")
			return nil
		}
		// It moved meanwhile (a verify activated it, or it was cancelled).
		if m, err = s.repo.findMandate(ctx, mandateID, ord.ConsumerID); err != nil {
			return err
		}
	}
	if tokenID == "" {
		return nil
	}
	if _, err := s.repo.mandates.UpdateOne(ctx,
		bson.D{{Key: "mandate_id", Value: m.MandateID}, {Key: "token", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "token", Value: tokenID}, {Key: "updated_at", Value: now}}}}); err != nil {
		return errInternal("mandate update failed")
	}
	return nil
}

// setMandateStatus applies a pause/resume/cancel action, enforcing the state
// machine. `cancel` is terminal; a re-cancel is a no-op idempotent return.
// A cancel also asks the gateway to cancel the bank token (best effort: the
// local cancel alone already stops every charge). A resume clears the
// failure hold, so Smart Recharge tries again on its next tick.
func (s *service) setMandateStatus(ctx context.Context, consumerID primitive.ObjectID, mandateID, action string) (*mandate, error) {
	target, ok := mandateActionTarget(action)
	if !ok {
		return nil, errBadRequest("unknown mandate action")
	}
	m, err := s.repo.findMandate(ctx, mandateID, consumerID)
	if err != nil {
		return nil, err
	}
	if m.Status == target {
		return m, nil // already there — idempotent
	}
	if !mandateCanTransition(m.Status, target) {
		return nil, errConflict("MANDATE_STATE", fmt.Sprintf("cannot %s a %s mandate", action, m.Status))
	}
	set := bson.D{}
	if action == "resume" {
		set = append(set, bson.E{Key: "topup_failures", Value: 0}, bson.E{Key: "last_failure_day", Value: ""})
	}
	out, err := s.repo.transitionMandate(ctx, mandateID, consumerID, m.Status, target, set)
	if err != nil {
		return nil, err
	}
	if target == "cancelled" {
		s.rzpCancelToken(ctx, out)
	}
	return out, nil
}

// rzpCancelToken asks the gateway to cancel a mandate's bank token (PUT
// /customers/{id}/tokens/{token}/cancel). Best effort and logged: a mandate
// cancelled here is never charged again whatever the gateway answers.
func (s *service) rzpCancelToken(ctx context.Context, m *mandate) {
	if s.rzpKeySecret == "" || m == nil || m.Token == "" || m.RzpCustomerID == "" {
		return
	}
	path := "/customers/" + url.PathEscape(m.RzpCustomerID) + "/tokens/" + url.PathEscape(m.Token) + "/cancel"
	if status, err := s.rzpCall(ctx, http.MethodPut, path, nil, nil); err != nil {
		s.log.WarnContext(ctx, "autopay: bank token cancel not confirmed by the gateway (the mandate is cancelled here regardless)",
			"mandate", m.MandateID, "status", status, "err", err)
	}
}

func (s *service) listMandatesFor(ctx context.Context, consumerID primitive.ObjectID) ([]mandate, error) {
	return s.repo.listMandates(ctx, consumerID)
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *handler) createMandate(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Plan      string  `json:"plan"`
		Amount    float64 `json:"amount"`
		MaxAmount float64 `json:"max_amount"`
		Threshold float64 `json:"threshold"` // Smart Recharge line; 0 = the default
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	view, err := h.svc.createMandate(r.Context(), id, body.Plan, body.Amount, body.MaxAmount, body.Threshold)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (h *handler) verifyMandate(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		MandateID string `json:"mandate_id"`
		PaymentID string `json:"razorpay_payment_id"`
		Signature string `json:"razorpay_signature"`
		Token     string `json:"razorpay_token"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	m, err := h.svc.verifyMandate(r.Context(), id, body.MandateID, body.PaymentID, body.Signature, body.Token)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *handler) listMandates(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.svc.listMandatesFor(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// mandateAction backs the pause/resume/cancel routes (the action is fixed per
// route so a client can't smuggle an arbitrary transition).
func (h *handler) mandateAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, aerr := actorID(r)
		if aerr != nil {
			writeErr(w, aerr)
			return
		}
		m, err := h.svc.setMandateStatus(r.Context(), id, chi.URLParam(r, "id"), action)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

// executeMandate is POST /mandate/{id}/execute, the app's executeMandate: a
// "top up now" request. It charges the member's own mandate for body.amount
// (default: their recharge amount; never above the per-debit cap), keyed by
// body.ref so a retry answers the same charge, and answers 202 with the
// charge. It NEVER debits the wallet: the money is credited when the bank's
// payment is captured (webhook or reconcile). body.purpose is accepted for
// the app's contract and ignored. Needs live Razorpay keys (503 otherwise).
func (h *handler) executeMandate(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Amount  float64 `json:"amount"`
		Ref     string  `json:"ref"`
		Purpose string  `json:"purpose"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	t, err := h.svc.autopayTopupNow(r.Context(), id, chi.URLParam(r, "id"), body.Amount, body.Ref, h.svc.now())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, t.view())
}

// mandatePolicy is POST /mandate/{id}/policy: the member's Smart Recharge
// threshold and top-up amount (either may be omitted).
func (h *handler) mandatePolicy(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Threshold *float64 `json:"threshold"`
		Amount    *float64 `json:"amount"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	m, err := h.svc.setMandatePolicy(r.Context(), id, chi.URLParam(r, "id"), body.Threshold, body.Amount)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

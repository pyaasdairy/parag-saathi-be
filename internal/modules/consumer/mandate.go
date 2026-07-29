package consumer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// UPI-AUTOPAY / e-MANDATE seam for SUBSCRIPTION AUTO-RENEWAL.
//
// The "3+3" trial (3 free mornings, then 3 paid) needs a way to keep charging
// from day 3 without the shopper re-approving each delivery. A UPI-AutoPay /
// e-mandate is Razorpay's recurring-payment authorization: the shopper approves
// ONCE (a max per-debit amount + a cadence), and the merchant may then debit up
// to that cap on schedule until the mandate is paused or cancelled.
//
// Money model (matches the wallet): a mandate EXECUTION does NOT mint money by
// itself — it drives the SAME exactly-once wallet settle path as a delivery
// settle (service.debit), idempotent by (mandateId, chargeDate). So a retried or
// duplicated scheduler tick can never double-charge, exactly like the delivery
// sweep. The gate is the unique (consumer, ref, type) wallet-txn index; the ref
// is mandate:<id>:<YYYY-MM-DD>.
//
// LIVE RAZORPAY NOTE: real UPI-AutoPay / e-mandate requires the *recurring
// payments* feature to be ENABLED on the Razorpay merchant account, and live
// RAZORPAY_KEY_ID / RAZORPAY_KEY_SECRET. Registration is a two-step flow — create
// a registration order carrying a `token` object (max_amount + frequency), then
// the checkout collects the mandate and returns a recurring token id, and
// subsequent charges hit Razorpay's recurring `/payments` API server-side. When
// no secret is configured AND OTP dev mode is on, this file runs an OFFLINE seam
// that mints a mock registration token so the whole subscribe→charge→pause flow
// is exercisable without moving real money. In production the secret is set and
// the dev seam is unreachable.

const collMandates = "consumer_mandates"

// mandatePlans maps a subscription plan to its charge cadence.
var mandatePlans = map[string]time.Duration{
	"daily":  24 * time.Hour,
	"weekly": 7 * 24 * time.Hour,
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

// dayKey is the UTC calendar-day idempotency key a mandate charges at most once
// against (YYYY-MM-DD). Two ticks on the same UTC day collapse to one charge.
func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

// mandateChargeRef is the exactly-once wallet ref for a single day's execution.
// Stable per (mandateId, day) so a duplicate tick reuses the same gate row and
// the debit is idempotent — the money side can never double-charge.
func mandateChargeRef(mandateID, day string) string {
	return "mandate:" + mandateID + ":" + day
}

// nextChargeAfter returns from + the plan's cadence (and false for an unknown plan).
func nextChargeAfter(plan string, from time.Time) (time.Time, bool) {
	d, ok := mandatePlans[plan]
	if !ok {
		return time.Time{}, false
	}
	return from.Add(d), true
}

// ── Document + wire shapes ──────────────────────────────────────────────────

// mandate is a consumer's recurring-payment authorization. Stored in
// consumer_mandates; scoped to the owning shopper.
type mandate struct {
	ID             primitive.ObjectID `bson:"_id,omitempty"          json:"-"`
	MandateID      string             `bson:"mandate_id"             json:"id"` // public id "mnd_…"
	ConsumerID     primitive.ObjectID `bson:"consumer_id"            json:"-"`
	Plan           string             `bson:"plan"                   json:"plan"`   // daily | weekly
	Status         string             `bson:"status"                 json:"status"` // pending|active|paused|cancelled
	Amount         float64            `bson:"amount"                 json:"amount"` // per-charge rupees
	MaxAmount      float64            `bson:"max_amount"             json:"max_amount"`
	RegOrderID     string             `bson:"reg_order_id,omitempty" json:"-"`               // Razorpay registration order id (signature anchor)
	Token          string             `bson:"token,omitempty"        json:"token,omitempty"` // recurring token id (from verify)
	PaymentID      string             `bson:"payment_id,omitempty"   json:"-"`
	NextCharge     *time.Time         `bson:"next_charge,omitempty"  json:"next_charge,omitempty"`
	LastChargeDate string             `bson:"last_charge_date,omitempty" json:"last_charge_date,omitempty"`
	LastChargeAt   *time.Time         `bson:"last_charge_at,omitempty"   json:"last_charge_at,omitempty"`
	CreatedAt      time.Time          `bson:"created_at"             json:"created_at"`
	UpdatedAt      time.Time          `bson:"updated_at"             json:"updated_at"`
}

// mandateOrderView is POST /consumer/mandate/create — the registration token the
// FE hands to Razorpay's recurring checkout (or a mock in the dev seam).
type mandateOrderView struct {
	ID     string `json:"id"`     // mandate id "mnd_…"
	Token  string `json:"token"`  // registration order/token id
	KeyID  string `json:"keyId"`  // Razorpay key id (empty in the dev seam)
	Status string `json:"status"` // "pending"
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
	return nil
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

// advanceMandateCharge records a completed day's charge and moves next_charge
// forward — IDEMPOTENTLY by the calendar day. The guard `last_charge_date != day`
// means a duplicated execution for the same day never double-advances the
// schedule (the money side is already gated by the wallet ref). Returns whether
// THIS call advanced the schedule.
func (r *repository) advanceMandateCharge(ctx context.Context, mandateID, day string, at, next time.Time) (bool, error) {
	res, err := r.mandates.UpdateOne(ctx,
		bson.D{{Key: "mandate_id", Value: mandateID}, {Key: "last_charge_date", Value: bson.D{{Key: "$ne", Value: day}}}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "last_charge_date", Value: day},
			{Key: "last_charge_at", Value: at},
			{Key: "next_charge", Value: next},
			{Key: "updated_at", Value: at},
		}}})
	if err != nil {
		return false, errInternal("mandate charge advance failed")
	}
	return res.ModifiedCount == 1, nil
}

// ── Razorpay recurring seam ─────────────────────────────────────────────────

// createRzpMandate registers a recurring mandate and returns its registration
// order/token id. Dev seam (no secret + OTP dev mode) mints a mock token; live
// creates a Razorpay registration order carrying a `token` object (requires the
// recurring feature enabled on the merchant account).
func (s *service) createRzpMandate(ctx context.Context, m *mandate, receipt string) (regOrderID string, err error) {
	if s.rzpKeySecret == "" {
		if !s.deps.Cfg.OTPDevMode {
			return "", errInternal("recurring payments are not configured")
		}
		b := make([]byte, 8)
		if _, e := rand.Read(b); e != nil {
			return "", errInternal("mandate token generation failed")
		}
		return "token_dev_" + hex.EncodeToString(b), nil
	}
	// LIVE: a registration order carrying the mandate authorization (max debit +
	// cadence). The first authenticated payment against this order registers the
	// e-mandate; recurring debits then use Razorpay's recurring payments API.
	firstChargePaise := int64(round2(m.Amount) * 100)
	maxPaise := int64(round2(m.MaxAmount) * 100)
	body, _ := json.Marshal(map[string]any{
		"amount":          firstChargePaise,
		"currency":        "INR",
		"receipt":         receipt,
		"payment_capture": true,
		"token": map[string]any{
			"max_amount": maxPaise,
			"expire_at":  time.Now().AddDate(1, 0, 0).Unix(),
			"frequency":  rzpFrequency(m.Plan),
		},
	})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, razorpayOrdersURL, bytes.NewReader(body))
	if e != nil {
		return "", errInternal("mandate request build failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.rzpKeyID, s.rzpKeySecret)
	client := &http.Client{Timeout: 12 * time.Second}
	resp, e := client.Do(req)
	if e != nil {
		return "", errInternal("payment gateway unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", errInternal(fmt.Sprintf("payment gateway rejected mandate (%d) — is recurring enabled on the account?", resp.StatusCode))
	}
	var out struct {
		ID string `json:"id"`
	}
	if e := json.NewDecoder(resp.Body).Decode(&out); e != nil || out.ID == "" {
		return "", errInternal("payment gateway response invalid")
	}
	return out.ID, nil
}

// rzpFrequency maps a plan to Razorpay's recurring token frequency hint.
func rzpFrequency(plan string) string {
	switch plan {
	case "daily":
		return "daily"
	case "weekly":
		return "weekly"
	default:
		return "as_presented"
	}
}

// ── Validation ──────────────────────────────────────────────────────────────

const (
	mandateAmountMin = 1.0
	mandateAmountMax = 5000.0   // a single subscription charge
	mandateMaxCap    = 100000.0 // the per-debit authorization ceiling
)

func validateMandate(plan string, amount, maxAmount float64) *apiError {
	if _, ok := mandatePlans[plan]; !ok {
		return errBadRequest("plan must be one of: daily, weekly")
	}
	if amount < mandateAmountMin || amount > mandateAmountMax {
		return errBadRequest("amount must be between ₹1 and ₹5,000")
	}
	if maxAmount < amount || maxAmount > mandateMaxCap {
		return errBadRequest("max_amount must be at least the charge amount and at most ₹1,00,000")
	}
	return nil
}

// ── Service ─────────────────────────────────────────────────────────────────

// createMandate registers a recurring authorization and returns the token the
// FE hands to Razorpay checkout. The mandate starts `pending`; verifyMandate
// promotes it to `active` after the registration payment is signature-verified.
func (s *service) createMandate(ctx context.Context, consumerID primitive.ObjectID, plan string, amount, maxAmount float64) (mandateOrderView, error) {
	amount = round2(amount)
	if maxAmount <= 0 {
		maxAmount = amount // default the cap to a single charge
	}
	maxAmount = round2(maxAmount)
	if verr := validateMandate(plan, amount, maxAmount); verr != nil {
		return mandateOrderView{}, verr
	}
	now := time.Now().UTC()
	m := &mandate{
		ID: primitive.NewObjectID(), MandateID: newMandateID(), ConsumerID: consumerID,
		Plan: plan, Status: "pending", Amount: amount, MaxAmount: maxAmount,
		CreatedAt: now, UpdatedAt: now,
	}
	regOrderID, err := s.createRzpMandate(ctx, m, "mnd_"+consumerID.Hex())
	if err != nil {
		return mandateOrderView{}, err
	}
	m.RegOrderID = regOrderID
	if err := s.repo.insertMandate(ctx, m); err != nil {
		return mandateOrderView{}, err
	}
	return mandateOrderView{ID: m.MandateID, Token: regOrderID, KeyID: s.rzpKeyID, Status: m.Status}, nil
}

// verifyMandate authoritatively verifies the registration payment signature and
// activates the mandate (pending→active), seeding the first next_charge. The
// signature is HMAC(regOrderId|paymentId) — the same scheme as wallet/verify.
// A bad signature never activates and never 500s.
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
	// DEMO seam (no live secret + OTP dev mode): activation stands in for the real
	// Razorpay recurring-checkout approval, so the whole subscribe→approve→charge
	// flow is exercisable without moving real money — mirroring createRzpMandate's
	// mock token. In production the secret is set and the signature is authoritative.
	if !s.rzpDevMode() && !s.verifyRzpSignature(m.RegOrderID, paymentID, signature) {
		return nil, errBadRequest("mandate signature verification failed")
	}
	next, _ := nextChargeAfter(m.Plan, time.Now().UTC())
	set := bson.D{
		{Key: "payment_id", Value: paymentID},
		{Key: "next_charge", Value: next},
	}
	if token != "" {
		set = append(set, bson.E{Key: "token", Value: token})
	}
	return s.repo.transitionMandate(ctx, mandateID, consumerID, m.Status, "active", set)
}

// setMandateStatus applies a pause/resume/cancel action, enforcing the state
// machine. `cancel` is terminal; a re-cancel is a no-op idempotent return.
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
	return s.repo.transitionMandate(ctx, mandateID, consumerID, m.Status, target, bson.D{})
}

func (s *service) listMandatesFor(ctx context.Context, consumerID primitive.ObjectID) ([]mandate, error) {
	return s.repo.listMandates(ctx, consumerID)
}

// runMandateCharge executes ONE day's subscription charge for an ACTIVE mandate.
// It debits the wallet through the SAME exactly-once settle path as a delivery
// (service.debit), keyed by mandate:<id>:<day>, so a retried or duplicated
// scheduler tick charges AT MOST once per UTC day. Only `active` mandates charge
// (a paused/cancelled/pending mandate is a no-op error). The schedule advance is
// separately idempotent by the same day key.
func (s *service) runMandateCharge(ctx context.Context, consumerID primitive.ObjectID, mandateID string, at time.Time) (walletView, error) {
	m, err := s.repo.findMandate(ctx, mandateID, consumerID)
	if err != nil {
		return walletView{}, err
	}
	if m.Status != "active" {
		return walletView{}, errConflict("MANDATE_STATE", "only an active mandate can be charged")
	}
	day := dayKey(at)
	ref := mandateChargeRef(mandateID, day)
	// The money gate: idempotent by (consumer, ref, DEBIT). A duplicate tick for
	// the same day reuses this ref and does NOT move money a second time.
	view, err := s.debit(ctx, consumerID, m.Amount, ref, "subscription auto-renewal")
	if err != nil {
		return walletView{}, err
	}
	// Bookkeeping — advance the schedule at most once per day (guarded by the day
	// key), so a duplicate tick can't skip a future charge by double-advancing.
	if next, ok := nextChargeAfter(m.Plan, at.UTC()); ok {
		_, _ = s.repo.advanceMandateCharge(ctx, mandateID, day, at.UTC(), next)
	}
	return view, nil
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
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	view, err := h.svc.createMandate(r.Context(), id, body.Plan, body.Amount, body.MaxAmount)
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

// executeMandate triggers ONE day's charge. DEV-gated: in production the daily
// execution is driven by the backend scheduler calling runMandateCharge, never
// by a client. Exposed here so the subscribe→charge flow is testable end-to-end.
func (h *handler) executeMandate(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if !h.svc.deps.Cfg.OTPDevMode {
		writeErr(w, errForbidden("not available"))
		return
	}
	view, err := h.svc.runMandateCharge(r.Context(), id, chi.URLParam(r, "id"), time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

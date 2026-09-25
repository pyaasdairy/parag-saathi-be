package consumer

// SMART RECHARGE: AutoPay FUNDS the wallet (founder decision 1, 25 Sep 2026).
//
// The retired design ran a mandate "charge" that DEBITED the wallet a fixed
// amount every day (runMandateCharge), on top of the delivery debit: money
// out, not in. AutoPay now charges the member's bank through their Razorpay
// recurring token and CREDITS the wallet, so the prepaid wallet stays funded
// and the 12 noon lock never skips a morning for want of money.
//
// WHEN. Every tick (autopayWorker, 15 min, MANDATE_AUTODEBIT=true, live keys)
// looks at each ACTIVE mandate whose bank token is confirmed and asks: does
// the wallet, less what it still owes (locked orders not yet delivered, the
// still-editable previews and the plan days whose 12:00 noon lock falls in
// the next autopayFundingHorizon, and the Rs 99 Founding Family seat when its
// bill day falls in it), stay at or above the member's threshold? If not, one
// charge starts for max(the member's recharge amount, the shortfall), capped
// at the per-debit limit they approved. A UPI AutoPay debit lands about a day
// after it is created (the bank's pre-debit notice goes out at least 24 h
// before the debit; Razorpay reports it 24-36 h after creation), so the
// horizon is that lead plus a margin: the day that enters the window is the
// one a charge started now still funds before its lock.
//
// HOW (Razorpay recurring payments, UPI): POST /v1/orders for the amount,
// then POST /v1/payments/create/recurring with the order, the customer and
// the token. Our payment order is recorded (purpose "autopay") BEFORE the
// charge, so the charge is credited through the SAME funnel as every top-up:
// creditCapturedPayment on the payment.captured / order.paid webhook, or the
// reconciliation sweep, keyed by the Razorpay order id in the ledger's unique
// (consumer, ref, type) index. The wallet moves only when the money is
// CAPTURED, exactly once, and B-06 "money added" fires once with it.
//
// DAYTIME. The server starts its OWN charges (the sweep and the Founding
// Family seat top-up) only from 07:00 to 22:00 IST (founder decision 6: at
// night only order, delivery and "money added" messages go out): every
// charge makes the bank send its UPI pre-debit notice, and a refusal tells
// the member. A need that arises at night waits for 07:00. A plan day
// enters the funding horizon at 12:00 (48 h before its noon lock), always
// in daytime, so that charge is never held. The member's own "top up now"
// is theirs to time and is never held.
//
// SAFETY. One charge in flight per mandate (a partial unique index; Razorpay:
// "do not create another subsequent payment until you get the status of the
// previous one"); at most autopayMaxPerDay automatic charges a day; a bank
// refusal holds the next automatic try to the next IST day and, after
// autopayMaxFailures in a row, waits for the member (a resume, a policy
// change or a captured charge clears it). A refusal tells the member (B-03,
// AutoPay wording). A charge the gateway never answered stays open for the
// webhook and the sweep, and is closed "expired" after autopayInFlightTTL
// (the sweep still credits it for 7 days if it was captured). Nothing here
// ever debits the wallet, and nothing is sent without RAZORPAY keys.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	collAutopayTopups = "consumer_autopay_topups"

	// autopayChargeLead is how long a UPI AutoPay charge takes to land.
	autopayChargeLead = 36 * time.Hour
	// autopayFundingHorizon: the plan days whose noon lock falls within it
	// count against the wallet. The lead plus a 12-hour margin.
	autopayFundingHorizon = autopayChargeLead + 12*time.Hour
	// autopayInFlightTTL: a charge nobody reported on is closed after this.
	autopayInFlightTTL = 72 * time.Hour
	// autopayMaxFailures: bank refusals in a row before Smart Recharge waits
	// for the member.
	autopayMaxFailures = 3
	// autopayMaxPerDay: automatic (threshold) charges started per mandate per
	// IST day.
	autopayMaxPerDay = 2
	// autopayTokenRecheck: how often an unconfirmed token is looked up.
	autopayTokenRecheck = time.Hour
	// autopayTickEvery is the worker's cadence (the subscription worker's).
	autopayTickEvery = 15 * time.Minute
	// autopayFallbackEmail: Razorpay's recurring charge requires an email; a
	// member who gave none is charged under Razorpay's placeholder address.
	autopayFallbackEmail = "void@razorpay.com"

	// autopayDayStartHour / autopayDayEndHour: the IST hours [07:00, 22:00)
	// in which the server starts its own charges (autopayDaytime).
	autopayDayStartHour = 7
	autopayDayEndHour   = 22

	autopayReasonThreshold = "threshold" // the wallet fell below the member's line
	autopayReasonSeat      = "seat"      // the Rs 99 Founding Family bill found the wallet short
	autopayReasonApp       = "app"       // the member asked for a top-up now
)

// autopayTopup is one Smart Recharge charge. Ref is its idempotency key
// (unique); Open marks it in flight (at most one per mandate).
type autopayTopup struct {
	ID            primitive.ObjectID `bson:"_id"`
	Ref           string             `bson:"ref"`
	MandateID     string             `bson:"mandate_id"`
	ConsumerID    primitive.ObjectID `bson:"consumer_id"`
	Reason        string             `bson:"reason"`
	AmountPaise   int64              `bson:"amount_paise"`
	Status        string             `bson:"status"` // starting | initiated | captured | failed | expired
	Open          bool               `bson:"open,omitempty"`
	Day           string             `bson:"day"` // IST day it started
	RzpOrderID    string             `bson:"rzp_order_id,omitempty"`
	RzpPaymentID  string             `bson:"rzp_payment_id,omitempty"`
	FailureReason string             `bson:"failure_reason,omitempty"`
	CreatedAt     time.Time          `bson:"created_at"`
	UpdatedAt     time.Time          `bson:"updated_at"`
	SettledAt     *time.Time         `bson:"settled_at,omitempty"`
}

// autopayTopupView is the wire shape of a charge (POST /mandate/{id}/execute).
type autopayTopupView struct {
	Ref           string     `json:"ref"`
	MandateID     string     `json:"mandate_id"`
	Reason        string     `json:"reason"`
	Amount        float64    `json:"amount"`
	Status        string     `json:"status"`
	FailureReason string     `json:"failure_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`
}

func (t *autopayTopup) view() autopayTopupView {
	return autopayTopupView{
		Ref: t.Ref, MandateID: t.MandateID, Reason: t.Reason, Amount: round2(float64(t.AmountPaise) / 100),
		Status: t.Status, FailureReason: t.FailureReason, CreatedAt: t.CreatedAt, SettledAt: t.SettledAt,
	}
}

var (
	errAutopayUnavailable = &apiError{status: http.StatusServiceUnavailable, Code: "AUTOPAY_UNAVAILABLE",
		Message: "AutoPay top-ups are not available right now. Add money from the wallet instead."}
	errAutopayInFlight = errConflict("AUTOPAY_IN_FLIGHT", "An AutoPay top-up is already on its way to your wallet.")
	errAutopayGateway  = &apiError{status: http.StatusBadGateway, Code: "AUTOPAY_GATEWAY",
		Message: "AutoPay could not start the top-up. Nothing was charged. Try again, or add money from the wallet."}
	errAutopayRefused = errUnprocessable("AUTOPAY_REFUSED",
		"Your bank did not accept the AutoPay top-up. Nothing was charged. Add money from the wallet.")
)

// autopayRef is a charge's idempotency key: its reason, the mandate and a
// key that names one occasion (an IST day and its count, a bill, the app's
// own ref).
func autopayRef(reason, mandateID, key string) string {
	return "autopay:" + reason + ":" + mandateID + ":" + key
}

// autopayAmountFor is what one charge adds: the member's recharge amount, or
// the shortfall when that is more, never above the per-debit cap.
func autopayAmountFor(m *mandate, shortfall float64) float64 {
	amt := math.Max(m.Amount, math.Ceil(shortfall))
	if amt > m.MaxAmount {
		amt = m.MaxAmount
	}
	return round2(amt)
}

// autopayDaytime reports whether the server may START one of its own
// charges at now: 07:00 to 22:00 IST (founder decision 6, quiet hours).
func autopayDaytime(now time.Time) bool {
	h := now.In(istZone).Hour()
	return h >= autopayDayStartHour && h < autopayDayEndHour
}

// autopayAutoEnabled is the MANDATE_AUTODEBIT switch: true arms the server's
// OWN charges (the Smart Recharge sweep and the Founding Family seat
// top-up). Every charge FUNDS the wallet; none debits it. A member's own
// "top up now" (POST /mandate/{id}/execute) needs only the keys.
func autopayAutoEnabled() bool { return os.Getenv("MANDATE_AUTODEBIT") == "true" }

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) autopayTopups() *mongo.Collection {
	return r.accounts.Database().Collection(collAutopayTopups)
}

// ensureAutopayIndexes: the ref is the exactly-once key of a charge, and at
// most one charge per mandate may be open (in flight) at a time.
func (r *repository) ensureAutopayIndexes(ctx context.Context) error {
	_, err := r.autopayTopups().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "ref", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "mandate_id", Value: 1}}, Options: options.Index().SetUnique(true).
			SetPartialFilterExpression(bson.D{{Key: "open", Value: true}}).SetName("one_open_per_mandate")},
		{Keys: bson.D{{Key: "rzp_order_id", Value: 1}}, Options: options.Index().
			SetPartialFilterExpression(bson.D{{Key: "rzp_order_id", Value: bson.D{{Key: "$gt", Value: ""}}}})},
		{Keys: bson.D{{Key: "mandate_id", Value: 1}, {Key: "day", Value: 1}, {Key: "reason", Value: 1}}},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}}},
	})
	if err != nil {
		return err
	}
	_, err = r.mandates.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "token", Value: 1}},
		Options: options.Index().SetPartialFilterExpression(bson.D{{Key: "token", Value: bson.D{{Key: "$gt", Value: ""}}}})})
	return err
}

// insertAutopayTopup inserts a new charge row. dupRef: this ref exists
// already (a replay); inFlight: another charge is open on the mandate.
func (r *repository) insertAutopayTopup(ctx context.Context, t *autopayTopup) (dupRef, inFlight bool, err error) {
	if _, err := r.autopayTopups().InsertOne(ctx, t); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			if existing, _ := r.findAutopayTopupByRef(ctx, t.Ref); existing != nil {
				return true, false, nil
			}
			return false, true, nil
		}
		return false, false, errInternal("autopay top-up store failed")
	}
	return false, false, nil
}

func (r *repository) findAutopayTopup(ctx context.Context, filter bson.D) (*autopayTopup, error) {
	var t autopayTopup
	err := r.autopayTopups().FindOne(ctx, filter).Decode(&t)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("autopay top-up lookup failed")
	}
	return &t, nil
}

func (r *repository) findAutopayTopupByRef(ctx context.Context, ref string) (*autopayTopup, error) {
	return r.findAutopayTopup(ctx, bson.D{{Key: "ref", Value: ref}})
}

func (r *repository) findAutopayTopupByOrder(ctx context.Context, rzpOrderID string) (*autopayTopup, error) {
	if rzpOrderID == "" {
		return nil, nil
	}
	return r.findAutopayTopup(ctx, bson.D{{Key: "rzp_order_id", Value: rzpOrderID}})
}

func (r *repository) openAutopayTopup(ctx context.Context, mandateID string) (*autopayTopup, error) {
	return r.findAutopayTopup(ctx, bson.D{{Key: "mandate_id", Value: mandateID}, {Key: "open", Value: true}})
}

func (r *repository) countAutopayTopups(ctx context.Context, mandateID, day, reason string) (int64, error) {
	n, err := r.autopayTopups().CountDocuments(ctx, bson.D{
		{Key: "mandate_id", Value: mandateID}, {Key: "day", Value: day}, {Key: "reason", Value: reason}})
	if err != nil {
		return 0, errInternal("autopay top-up count failed")
	}
	return n, nil
}

// updateAutopayTopup applies set (and, close, ends the charge's in-flight
// hold) to one row matching guard. Reports whether THIS call changed it.
func (r *repository) updateAutopayTopup(ctx context.Context, id primitive.ObjectID, set bson.D, closeIt bool, guard bson.D) bool {
	upd := bson.D{{Key: "$set", Value: set}}
	if closeIt {
		upd = append(upd, bson.E{Key: "$unset", Value: bson.D{{Key: "open", Value: ""}}})
	}
	res, err := r.autopayTopups().UpdateOne(ctx, append(bson.D{{Key: "_id", Value: id}}, guard...), upd)
	return err == nil && res.ModifiedCount == 1
}

// listChargeableMandates: ACTIVE mandates holding a bank token, newest first
// (the sweep charges only a member's newest one).
func (r *repository) listChargeableMandates(ctx context.Context, limit int64) ([]mandate, error) {
	cur, err := r.mandates.Find(ctx, bson.D{
		{Key: "status", Value: "active"}, {Key: "token", Value: bson.D{{Key: "$gt", Value: ""}}},
	}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, errInternal("mandate scan failed")
	}
	out := []mandate{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("mandate scan decode failed")
	}
	return out, nil
}

// activeMandateFor is the member's newest ACTIVE mandate, or nil.
func (r *repository) activeMandateFor(ctx context.Context, consumerID primitive.ObjectID) (*mandate, error) {
	var m mandate
	err := r.mandates.FindOne(ctx, bson.D{{Key: "consumer_id", Value: consumerID}, {Key: "status", Value: "active"}},
		options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})).Decode(&m)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("mandate lookup failed")
	}
	return &m, nil
}

// autopayOwedStatuses are the order statuses whose money the wallet still
// owes: every wallet order is paid at the door (deliverDelivery).
var autopayOwedStatuses = bson.A{"placed", "confirmed", "preparing", "assigned", "out_for_delivery"}

// listWalletOwedOrders: the member's wallet-paid orders not delivered yet
// that the wallet will pay: locked subscription mornings and one-off morning
// orders for fromDay through toDay (the funding horizon's last day; a
// morning booked further ahead is funded once it enters the horizon), and
// instant orders placed since instantSince. A still-editable subscription
// preview is not here (the plan is costed by day in autopayNeed).
func (r *repository) listWalletOwedOrders(ctx context.Context, userID, fromDay, toDay string, instantSince time.Time) ([]order, error) {
	// IST YYYY-MM-DD days; "before the day after" also bounds a value that
	// carries a time of day.
	within := bson.D{{Key: "$gte", Value: fromDay}, {Key: "$lt", Value: addDaysIST(toDay, 1)}}
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "user_id", Value: userID},
		{Key: "status", Value: bson.D{{Key: "$in", Value: autopayOwedStatuses}}},
		{Key: "payment_method", Value: bson.D{{Key: "$in", Value: bson.A{"wallet", "prepaid"}}}},
		{Key: "$and", Value: bson.A{
			bson.D{{Key: "$or", Value: bson.A{
				bson.D{{Key: "sub_locked_at", Value: bson.D{{Key: "$gt", Value: ""}}}},
				bson.D{{Key: "subscription_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}}},
			}}},
			bson.D{{Key: "$or", Value: bson.A{
				bson.D{{Key: "delivery_date", Value: within}},
				bson.D{{Key: "scheduled_for", Value: within}},
				bson.D{{Key: "lane", Value: "instant"}, {Key: "placed_at", Value: bson.D{{Key: "$gte", Value: instantSince}}}},
			}}},
		}},
	}, options.Find().SetLimit(200))
	if err != nil {
		return nil, errInternal("owed orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("owed orders decode failed")
	}
	return out, nil
}

// ── The charge ──────────────────────────────────────────────────────────────

// autopayStartTopup starts ONE charge of amountPaise on the mandate and
// returns its row. Idempotent by ref (reason + mandate + key): a replay
// answers the row it made. Order, then our bound payment order, then the
// recurring charge, so a webhook that beats the charge's answer still finds
// the order it credits. The wallet is NOT credited here: only a CAPTURED
// payment is (creditCapturedPayment -> autopaySettleCaptured).
func (s *service) autopayStartTopup(ctx context.Context, m *mandate, amountPaise int64, reason, key string, now time.Time) (*autopayTopup, error) {
	if s.rzpKeySecret == "" {
		return nil, errAutopayUnavailable
	}
	if m.Status != "active" {
		return nil, errConflict("MANDATE_STATE", "AutoPay is not active.")
	}
	if m.Token == "" || m.RzpCustomerID == "" {
		return nil, errConflict("MANDATE_NOT_READY", "AutoPay is still being set up with your bank.")
	}
	if amountPaise < 100 || amountPaise > rupeesToPaise(m.MaxAmount) {
		return nil, errBadRequest("the amount must be between ₹1 and your AutoPay limit")
	}
	ref := autopayRef(reason, m.MandateID, key)
	if t, err := s.repo.findAutopayTopupByRef(ctx, ref); err != nil || t != nil {
		return t, err
	}
	now = now.UTC()
	t := &autopayTopup{
		ID: primitive.NewObjectID(), Ref: ref, MandateID: m.MandateID, ConsumerID: m.ConsumerID, Reason: reason,
		AmountPaise: amountPaise, Status: "starting", Open: true, Day: istToday(now), CreatedAt: now, UpdatedAt: now,
	}
	dupRef, inFlight, err := s.repo.insertAutopayTopup(ctx, t)
	switch {
	case err != nil:
		return nil, err
	case dupRef:
		return s.repo.findAutopayTopupByRef(ctx, ref)
	case inFlight:
		return nil, errAutopayInFlight
	}
	acct, _ := s.repo.findAccountByID(ctx, m.ConsumerID)
	if acct == nil {
		s.autopayFail(ctx, t, "account not found", false, now)
		return t, errNotFound("account not found")
	}

	// 1) The order the charge pays.
	var ord struct {
		ID string `json:"id"`
	}
	receipt := "apy_" + t.ID.Hex()
	status, err := s.rzpCall(ctx, http.MethodPost, "/orders", map[string]any{
		"amount": amountPaise, "currency": "INR", "receipt": receipt, "payment_capture": true,
		"notes": map[string]string{"mandate_id": m.MandateID, "autopay_ref": ref, "purpose": "autopay_topup"},
	}, &ord)
	if err != nil || ord.ID == "" {
		// Nothing reached the bank (a bad key, a gateway config error, the
		// gateway down): our failure, not a refusal. It is not counted toward
		// the pause, holds no day and messages no member; only a refused
		// recurring charge or a payment.failed does.
		s.autopayFail(ctx, t, "the AutoPay order was not created", false, now)
		s.log.WarnContext(ctx, "autopay: order not created (our side, not a bank refusal)", "mandate", m.MandateID, "status", status, "err", err)
		return t, errAutopayGateway
	}
	t.RzpOrderID = ord.ID
	s.repo.updateAutopayTopup(ctx, t.ID, bson.D{{Key: "rzp_order_id", Value: ord.ID}, {Key: "updated_at", Value: now}}, false, nil)

	// 2) Bound to the wallet BEFORE the charge: whichever of the webhook and
	//    the reconcile sweep reports the capture credits exactly this amount
	//    to this member, once (the ledger ref is the order id).
	if err := s.repo.insertPaymentOrder(ctx, &paymentOrder{
		ID: primitive.NewObjectID(), OrderID: ord.ID, ConsumerID: m.ConsumerID, AmountPaise: amountPaise,
		Receipt: receipt, Purpose: "autopay", RefID: ref, Status: "CREATED", CreatedAt: now,
	}); err != nil {
		s.autopayFail(ctx, t, "the AutoPay order was not recorded", false, now)
		return t, errAutopayGateway
	}

	// 3) The recurring charge on the member's token.
	email := autopayFallbackEmail
	if acct.Email != nil && strings.Contains(*acct.Email, "@") {
		email = strings.TrimSpace(*acct.Email)
	}
	var pay struct {
		PaymentID string `json:"razorpay_payment_id"`
	}
	status, err = s.rzpCall(ctx, http.MethodPost, "/payments/create/recurring", map[string]any{
		"email": email, "contact": acct.Phone, "amount": amountPaise, "currency": "INR",
		"order_id": ord.ID, "customer_id": m.RzpCustomerID, "token": m.Token, "recurring": true,
		"description": "PYAAS wallet top-up (AutoPay)",
		"notes":       map[string]string{"mandate_id": m.MandateID, "autopay_ref": ref},
	}, &pay)
	switch {
	case err == nil:
		t.Status, t.RzpPaymentID = "initiated", pay.PaymentID
		s.repo.updateAutopayTopup(ctx, t.ID, bson.D{{Key: "status", Value: "initiated"},
			{Key: "rzp_payment_id", Value: pay.PaymentID}, {Key: "updated_at", Value: now}}, false,
			bson.D{{Key: "status", Value: "starting"}})
		return t, nil
	case status == 0 || status >= 500:
		// Unknown outcome: the charge may exist. It stays open; the webhook,
		// the reconcile sweep or the in-flight expiry decide it.
		t.Status = "initiated"
		s.repo.updateAutopayTopup(ctx, t.ID, bson.D{{Key: "status", Value: "initiated"},
			{Key: "failure_reason", Value: "the gateway did not answer the charge"}, {Key: "updated_at", Value: now}}, false,
			bson.D{{Key: "status", Value: "starting"}})
		s.log.WarnContext(ctx, "autopay: charge outcome unknown, waiting for the gateway", "mandate", m.MandateID, "order", ord.ID, "status", status)
		return t, nil
	default:
		s.autopayFail(ctx, t, "the bank did not accept the AutoPay debit", true, now)
		s.log.WarnContext(ctx, "autopay: charge refused", "mandate", m.MandateID, "order", ord.ID, "status", status, "err", err)
		return t, errAutopayRefused
	}
}

// autopayFail closes a charge as failed (once). A counted failure (the bank
// or the gateway REFUSED it) holds the next automatic try to the next IST
// day, counts toward autopayMaxFailures and tells the member (B-03, AutoPay
// wording); a failure of ours (nothing reached the bank) does neither.
func (s *service) autopayFail(ctx context.Context, t *autopayTopup, reason string, counted bool, now time.Time) {
	now = now.UTC()
	closed := s.repo.updateAutopayTopup(ctx, t.ID, bson.D{
		{Key: "status", Value: "failed"}, {Key: "failure_reason", Value: reason},
		{Key: "settled_at", Value: now}, {Key: "updated_at", Value: now},
	}, true, bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"captured", "failed"}}}}})
	t.Status, t.Open, t.FailureReason = "failed", false, reason
	if !closed || !counted {
		return
	}
	_, _ = s.repo.mandates.UpdateOne(ctx, bson.D{{Key: "mandate_id", Value: t.MandateID}}, bson.D{
		{Key: "$inc", Value: bson.D{{Key: "topup_failures", Value: 1}}},
		{Key: "$set", Value: bson.D{{Key: "last_failure_day", Value: istToday(now)}, {Key: "updated_at", Value: now}}},
	})
	s.emitCRMEvent(ctx, "payment.failed", t.ConsumerID, map[string]any{
		"source": "autopay", "mandate_id": t.MandateID, "autopay_reason": t.Reason,
		"amount": round2(float64(t.AmountPaise) / 100), "reason": reason,
		"payment_order_id": t.RzpOrderID, "payment_id": t.RzpPaymentID, "scope_key": t.Ref,
	})
}

// autopaySettleCaptured closes the charge behind a captured AutoPay order
// once the wallet holds its money (called by creditCapturedPayment, after the
// credit). A registration order has no charge row and is left alone. A
// captured charge clears the mandate's failure hold, and a seat charge bills
// the Founding Family month straight away.
func (s *service) autopaySettleCaptured(ctx context.Context, orderID, paymentID string, now time.Time) {
	t, err := s.repo.findAutopayTopupByOrder(ctx, orderID)
	if err != nil || t == nil {
		return
	}
	now = now.UTC()
	set := bson.D{{Key: "status", Value: "captured"}, {Key: "failure_reason", Value: ""},
		{Key: "settled_at", Value: now}, {Key: "updated_at", Value: now}}
	if paymentID != "" {
		set = append(set, bson.E{Key: "rzp_payment_id", Value: paymentID})
	}
	if !s.repo.updateAutopayTopup(ctx, t.ID, set, true, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "captured"}}}}) {
		return
	}
	_, _ = s.repo.mandates.UpdateOne(ctx, bson.D{{Key: "mandate_id", Value: t.MandateID}}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "topup_failures", Value: 0}, {Key: "last_failure_day", Value: ""}, {Key: "updated_at", Value: now}}}})
	s.log.InfoContext(ctx, "autopay: top-up captured and credited", "mandate", t.MandateID, "reason", t.Reason,
		"amount", round2(float64(t.AmountPaise)/100))
	if t.Reason == autopayReasonSeat {
		s.foundingBillAfterTopup(ctx, t.ConsumerID, now)
	}
}

// autopayPaymentFailed handles payment.failed on an AutoPay order: the
// charge behind it is closed as refused (and the member told, once). A
// failed REGISTRATION checkout has no charge row: the member saw it fail in
// the checkout sheet, so nothing is said.
func (s *service) autopayPaymentFailed(ctx context.Context, ord *paymentOrder, paymentID, reason string, now time.Time) {
	if ord.Status == "PAID" {
		return
	}
	t, err := s.repo.findAutopayTopupByOrder(ctx, ord.OrderID)
	if err != nil || t == nil {
		return
	}
	if paymentID != "" {
		t.RzpPaymentID = paymentID
	}
	if strings.TrimSpace(reason) == "" {
		reason = "the bank did not accept the AutoPay debit"
	}
	s.autopayFail(ctx, t, reason, true, now)
}

// ── The bank token ──────────────────────────────────────────────────────────

// autopayApplyTokenStatus records the bank's word on a mandate's token; a
// token the bank cancelled or rejected cancels the mandate here too.
func (s *service) autopayApplyTokenStatus(ctx context.Context, m *mandate, status string, now time.Time) {
	now = now.UTC()
	m.TokenStatus = status
	m.TokenCheckedAt = &now
	_, _ = s.repo.mandates.UpdateOne(ctx, bson.D{{Key: "mandate_id", Value: m.MandateID}}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "token_status", Value: status}, {Key: "token_checked_at", Value: now}, {Key: "updated_at", Value: now}}}})
	if status == "cancelled" || status == "rejected" {
		if res, err := s.repo.mandates.UpdateOne(ctx,
			bson.D{{Key: "mandate_id", Value: m.MandateID}, {Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}, {Key: "cancel_reason", Value: "bank_" + status},
				{Key: "updated_at", Value: now}}}}); err == nil && res.ModifiedCount == 1 {
			m.Status = "cancelled"
			s.log.InfoContext(ctx, "autopay: the bank ended the mandate", "mandate", m.MandateID, "token_status", status)
		}
	}
}

// autopayTokenReady reports whether Smart Recharge may charge the mandate's
// token: the bank has CONFIRMED it. An unconfirmed token is looked up at the
// gateway (GET /customers/{id}/tokens) at most once per autopayTokenRecheck,
// so a missed token.confirmed webhook never strands a mandate.
func (s *service) autopayTokenReady(ctx context.Context, m *mandate, now time.Time) bool {
	if m.Token == "" || m.RzpCustomerID == "" {
		return false
	}
	switch m.TokenStatus {
	case "confirmed":
		return true
	case "rejected", "cancelled":
		return false
	}
	if m.TokenCheckedAt != nil && now.Sub(*m.TokenCheckedAt) < autopayTokenRecheck {
		return false
	}
	var out struct {
		Items []struct {
			ID               string `json:"id"`
			RecurringDetails struct {
				Status string `json:"status"`
			} `json:"recurring_details"`
		} `json:"items"`
	}
	status := m.TokenStatus
	if _, err := s.rzpCall(ctx, http.MethodGet, "/customers/"+url.PathEscape(m.RzpCustomerID)+"/tokens", nil, &out); err == nil {
		for _, it := range out.Items {
			if it.ID == m.Token && it.RecurringDetails.Status != "" {
				status = it.RecurringDetails.Status
			}
		}
	}
	s.autopayApplyTokenStatus(ctx, m, status, now)
	return status == "confirmed" && m.Status == "active"
}

// rzpTokenEnvelope is the slice of a token.* webhook the backend acts on.
type rzpTokenEnvelope struct {
	Event   string `json:"event"`
	Payload struct {
		Token struct {
			Entity struct {
				ID               string `json:"id"`
				RecurringDetails struct {
					Status string `json:"status"`
				} `json:"recurring_details"`
			} `json:"entity"`
		} `json:"token"`
	} `json:"payload"`
}

// autopayTokenEvent applies token.confirmed / rejected / paused / cancelled
// to the mandate that holds the token (the owner is our row, never the
// payload). An unknown token is ignored.
func (s *service) autopayTokenEvent(ctx context.Context, raw []byte, now time.Time) {
	var ev rzpTokenEnvelope
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	id := ev.Payload.Token.Entity.ID
	status := ev.Payload.Token.Entity.RecurringDetails.Status
	if status == "" {
		status = strings.TrimPrefix(ev.Event, "token.")
	}
	if id == "" || status == "" {
		return
	}
	var m mandate
	if err := s.repo.mandates.FindOne(ctx, bson.D{{Key: "token", Value: id}},
		options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})).Decode(&m); err != nil {
		return
	}
	s.autopayApplyTokenStatus(ctx, &m, status, now)
}

// ── What the wallet must cover ──────────────────────────────────────────────

// autopayNeed is what the member's wallet must still pay inside the funding
// horizon: the wallet orders not delivered yet, then per plan and day, from
// tomorrow through the last day whose 12:00 noon lock falls within
// autopayFundingHorizon of now, a still-editable preview at its own price
// (a free trial day costs nothing) or, with no preview yet, the plan's line
// at today's price; and the Rs 99 Founding Family month when its bill day
// falls within the horizon. A day the lock already decided (locked: counted
// among the orders; skipped: nothing) and a plan changed after a passed
// cut-off (the change belongs to a later day) add nothing.
func (s *service) autopayNeed(ctx context.Context, consumerID primitive.ObjectID, now time.Time) (float64, error) {
	today := istToday(now)
	horizonEnd := now.Add(autopayFundingHorizon)
	need := 0.0
	owed, err := s.repo.listWalletOwedOrders(ctx, consumerID.Hex(), today, autopayHorizonLastDay(now), now.Add(-24*time.Hour))
	if err != nil {
		return 0, err
	}
	for i := range owed {
		need += lockCharge(&owed[i], owed[i].TrialFree)
	}
	subs, err := s.repo.listSubscriptions(ctx, consumerID)
	if err != nil {
		return 0, err
	}
	for day := addDaysIST(today, 1); ; day = addDaysIST(day, 1) {
		lockAt := lockMomentFor(day)
		if lockAt.IsZero() || lockAt.After(horizonEnd) {
			break
		}
		for i := range subs {
			sub := &subs[i]
			if sub.claimed(day) {
				o, oerr := s.repo.findLiveSubscriptionOrder(ctx, sub.SubscriptionID, day)
				if oerr != nil {
					return 0, oerr
				}
				if o != nil && o.SubLockedAt == "" && o.Status == "placed" {
					need += lockCharge(o, o.TrialFree)
				}
				continue
			}
			if !subscriptionDueOn(sub, day) {
				continue
			}
			if !now.Before(lockAt) && !sub.subChangedBefore(lockAt) {
				continue
			}
			need += round2(s.subscriptionLinePrice(ctx, sub)*float64(sub.Qty) + subscriptionDeliveryFee)
		}
	}
	if fm, _ := s.repo.findFoundingMember(ctx, consumerID); fm != nil && fm.Status == memberActive &&
		fm.NextBillDate != "" && fm.NextBillDate <= istToday(horizonEnd) {
		need += s.foundingPriceMonth(ctx)
	}
	return round2(need), nil
}

// autopayHorizonLastDay is the last delivery day whose 12:00 noon lock falls
// within autopayFundingHorizon of now: the day autopayNeed's plan loop ends
// on, and at least tomorrow.
func autopayHorizonLastDay(now time.Time) string {
	horizonEnd := now.Add(autopayFundingHorizon)
	last := addDaysIST(istToday(now), 1)
	for day := addDaysIST(last, 1); ; day = addDaysIST(day, 1) {
		lockAt := lockMomentFor(day)
		if lockAt.IsZero() || lockAt.After(horizonEnd) {
			return last
		}
		last = day
	}
}

// ── The sweep ───────────────────────────────────────────────────────────────

// sweepAutopay is one Smart Recharge tick: close charges nobody reported on
// for autopayInFlightTTL, then check every chargeable mandate (a member's
// newest only). Returns how many charges it started. Inert without keys.
func (s *service) sweepAutopay(ctx context.Context, now time.Time) int {
	if s.rzpKeySecret == "" {
		return 0
	}
	now = now.UTC()
	expire := func(filter bson.D, reason string) {
		_, _ = s.repo.autopayTopups().UpdateMany(ctx, filter, bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: "expired"}, {Key: "failure_reason", Value: reason},
				{Key: "settled_at", Value: now}, {Key: "updated_at", Value: now}}},
			{Key: "$unset", Value: bson.D{{Key: "open", Value: ""}}},
		})
	}
	// A charge the gateway never reported on.
	expire(bson.D{{Key: "open", Value: true}, {Key: "created_at", Value: bson.D{{Key: "$lt", Value: now.Add(-autopayInFlightTTL)}}}},
		"no word from the gateway")
	// A start that never reached the gateway (no order: nothing to charge),
	// e.g. a restart mid-start, stops holding the mandate after half an hour.
	expire(bson.D{{Key: "open", Value: true}, {Key: "status", Value: "starting"},
		{Key: "rzp_order_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		{Key: "created_at", Value: bson.D{{Key: "$lt", Value: now.Add(-30 * time.Minute)}}}},
		"the charge never reached the gateway")
	ms, err := s.repo.listChargeableMandates(ctx, 2000)
	if err != nil {
		s.log.WarnContext(ctx, "autopay sweep: mandate scan failed", "err", err)
		return 0
	}
	seen := map[primitive.ObjectID]bool{}
	started := 0
	for i := range ms {
		m := &ms[i]
		if seen[m.ConsumerID] {
			continue
		}
		seen[m.ConsumerID] = true
		if s.autopayCheck(ctx, m, now) {
			started++
		}
	}
	if started > 0 {
		s.log.InfoContext(ctx, "autopay sweep started top-ups", "count", started)
	}
	return started
}

// autopayCheck decides one mandate on one tick and starts its charge when
// the wallet needs one. Reports whether a charge started.
func (s *service) autopayCheck(ctx context.Context, m *mandate, now time.Time) bool {
	if !autopayDaytime(now) {
		return false // quiet hours: the need waits for 07:00
	}
	today := istToday(now)
	if m.TopupFailures >= autopayMaxFailures || m.LastFailureDay == today {
		return false
	}
	if acct, _ := s.repo.findAccountByID(ctx, m.ConsumerID); acct == nil {
		return false // an erased account is never charged
	}
	if open, err := s.repo.openAutopayTopup(ctx, m.MandateID); err != nil || open != nil {
		return false
	}
	if !s.autopayTokenReady(ctx, m, now) {
		return false
	}
	need, err := s.autopayNeed(ctx, m.ConsumerID, now)
	if err != nil {
		return false
	}
	wv, err := s.wallet(ctx, m.ConsumerID)
	if err != nil {
		return false
	}
	short := round2(need + m.effectiveThreshold() - wv.Available)
	if short <= 0 {
		return false
	}
	n, err := s.repo.countAutopayTopups(ctx, m.MandateID, today, autopayReasonThreshold)
	if err != nil || n >= autopayMaxPerDay {
		return false
	}
	amount := autopayAmountFor(m, short)
	t, err := s.autopayStartTopup(ctx, m, rupeesToPaise(amount), autopayReasonThreshold, today+"-"+strconv.FormatInt(n+1, 10), now)
	if err != nil || t == nil {
		return false
	}
	s.log.InfoContext(ctx, "autopay: top-up started", "mandate", m.MandateID, "amount", amount,
		"available", wv.Available, "need", need, "threshold", m.effectiveThreshold())
	return t.Status != "failed"
}

// autopayWorker runs Smart Recharge for the process lifetime when
// MANDATE_AUTODEBIT=true and the Razorpay keys are set; otherwise it logs
// why it is off and returns (nothing is ever charged).
func (s *service) autopayWorker(ctx context.Context) {
	if !autopayAutoEnabled() {
		s.log.Info("autopay smart recharge OFF (MANDATE_AUTODEBIT != true): no automatic top-ups")
		return
	}
	if s.rzpKeySecret == "" {
		s.log.Info("autopay smart recharge OFF: no RAZORPAY_KEY_SECRET")
		return
	}
	select {
	case <-time.After(45 * time.Second): // let mongo and the indexes settle
	case <-ctx.Done():
		return
	}
	s.log.Info("autopay smart recharge ON", "tick", autopayTickEvery.String(), "horizon", autopayFundingHorizon.String())
	for {
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		s.sweepAutopay(runCtx, s.now())
		cancel()
		select {
		case <-time.After(autopayTickEvery):
		case <-ctx.Done():
			return
		}
	}
}

// ── Member-facing: top up now, and the policy ───────────────────────────────

// autopayTopupNow is POST /mandate/{id}/execute: the member asks AutoPay to
// add money now. amount defaults to their recharge amount and may not pass
// the per-debit cap; ref (the app's own key) makes a retry answer the same
// charge. It never debits the wallet: the money arrives when captured.
func (s *service) autopayTopupNow(ctx context.Context, consumerID primitive.ObjectID, mandateID string, amount float64, ref string, now time.Time) (*autopayTopup, error) {
	m, err := s.repo.findMandate(ctx, mandateID, consumerID)
	if err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > 80 {
		return nil, errBadRequest("a top-up reference (ref) of up to 80 characters is required")
	}
	amount = round2(amount)
	if amount <= 0 {
		amount = m.Amount
	}
	if amount < mandateAmountMin || amount > m.MaxAmount {
		return nil, errBadRequest(fmt.Sprintf("amount must be between ₹1 and your AutoPay limit of ₹%s", crmRupees(m.MaxAmount)))
	}
	if s.rzpKeySecret == "" {
		return nil, errAutopayUnavailable
	}
	if prior, err := s.repo.findAutopayTopupByRef(ctx, autopayRef(autopayReasonApp, m.MandateID, ref)); err != nil || prior != nil {
		return prior, err // the same request again
	}
	if m.Status != "active" {
		return nil, errConflict("MANDATE_STATE", "AutoPay is not active.")
	}
	if !s.autopayTokenReady(ctx, m, now) {
		return nil, errConflict("MANDATE_NOT_READY", "AutoPay is still being set up with your bank.")
	}
	return s.autopayStartTopup(ctx, m, rupeesToPaise(amount), autopayReasonApp, ref, now)
}

// setMandatePolicy is POST /mandate/{id}/policy: the member's Smart Recharge
// line (threshold) and top-up amount. The amount stays within ₹1..₹5,000 and
// the per-debit cap the bank approved. A change clears the failure hold.
func (s *service) setMandatePolicy(ctx context.Context, consumerID primitive.ObjectID, mandateID string, threshold, amount *float64) (*mandate, error) {
	m, err := s.repo.findMandate(ctx, mandateID, consumerID)
	if err != nil {
		return nil, err
	}
	if m.Status == "cancelled" {
		return nil, errConflict("MANDATE_STATE", "AutoPay is cancelled.")
	}
	set := bson.D{{Key: "topup_failures", Value: 0}, {Key: "last_failure_day", Value: ""}, {Key: "updated_at", Value: s.now().UTC()}}
	if threshold != nil {
		th := round2(*threshold)
		if verr := validateThreshold(th); verr != nil {
			return nil, verr
		}
		if th == 0 {
			th = autopayDefaultThreshold
		}
		set = append(set, bson.E{Key: "threshold", Value: th})
	}
	if amount != nil {
		a := round2(*amount)
		if a < mandateAmountMin || a > mandateAmountMax || a > m.MaxAmount {
			return nil, errBadRequest(fmt.Sprintf("amount must be between ₹1 and your AutoPay limit of ₹%s", crmRupees(math.Min(m.MaxAmount, mandateAmountMax))))
		}
		set = append(set, bson.E{Key: "amount", Value: a})
	}
	var out mandate
	err = s.repo.mandates.FindOneAndUpdate(ctx,
		bson.D{{Key: "mandate_id", Value: mandateID}, {Key: "consumer_id", Value: consumerID}, {Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}}},
		bson.D{{Key: "$set", Value: set}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if isNoDocs(err) {
		return nil, errConflict("MANDATE_STATE", "AutoPay is cancelled.")
	}
	if err != nil {
		return nil, errInternal("mandate update failed")
	}
	return &out, nil
}

// autopayKeepForErasure runs before an account's erasure cascade. An
// AutoPay payment the bank may still complete (a charge in flight, whose
// token cancel is asynchronous at the bank and whose debit was already
// notified, or a registration not yet paid) keeps its payment order,
// pseudonymised: no owner, marked erased. A capture that lands on it
// afterwards is then refunded at the gateway (autopayRefundErased) instead
// of being ignored as an unknown order with the member's money gone and no
// record. An error stops the erasure (retryable), so no charge is lost.
func (s *service) autopayKeepForErasure(ctx context.Context, consumerID primitive.ObjectID) error {
	if _, err := s.repo.payOrders.UpdateMany(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}, {Key: "purpose", Value: "autopay"}, {Key: "status", Value: "CREATED"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "consumer_id", Value: primitive.NilObjectID}, {Key: "erased", Value: true}}}},
	); err != nil {
		return errInternal("erasure failed")
	}
	return nil
}

// autopayRefundErased returns a payment captured on an erased account's
// kept AutoPay order to the member's bank (POST /payments/{id}/refund, the
// full amount). The order is claimed CREATED -> REFUNDING first, so the
// webhook and the reconcile sweep refund it once; a gateway that did not
// answer releases the claim and the error makes the webhook retry (the
// sweep tries again too); a refusal leaves it REFUND_FAILED and logged for
// ops. Nothing is ever credited.
func (s *service) autopayRefundErased(ctx context.Context, ord *paymentOrder, paymentID string) error {
	if paymentID == "" || s.rzpKeySecret == "" {
		return nil
	}
	res, err := s.repo.payOrders.UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: ord.OrderID}, {Key: "status", Value: "CREATED"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "REFUNDING"}, {Key: "payment_id", Value: paymentID}}}})
	if err != nil {
		return errInternal("payment order update failed")
	}
	if res.ModifiedCount == 0 {
		return nil // refunded, or being refunded, already
	}
	status, rerr := s.rzpCall(ctx, http.MethodPost, "/payments/"+url.PathEscape(paymentID)+"/refund", map[string]any{
		"amount": ord.AmountPaise, "speed": "normal",
		"notes": map[string]string{"reason": "account_erased", "order_id": ord.OrderID},
	}, nil)
	next := "REFUNDED"
	switch {
	case rerr == nil:
		s.log.InfoContext(ctx, "autopay: a capture on an erased account was refunded", "order", ord.OrderID, "payment", paymentID)
	case status == 0 || status >= 500:
		_, _ = s.repo.payOrders.UpdateOne(ctx,
			bson.D{{Key: "order_id", Value: ord.OrderID}, {Key: "status", Value: "REFUNDING"}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "CREATED"}}}})
		return errInternal("the refund was not confirmed by the gateway")
	default:
		next = "REFUND_FAILED"
		s.log.ErrorContext(ctx, "autopay: the refund of a capture on an erased account was refused; ops must return it",
			"order", ord.OrderID, "payment", paymentID, "status", status, "err", rerr)
	}
	_, _ = s.repo.payOrders.UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: ord.OrderID}, {Key: "status", Value: "REFUNDING"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: next}}}})
	return nil
}

// autopayCancelForErasure cancels every live mandate of an account being
// erased, at the gateway too (best effort): nothing may charge an erased
// account. The rows themselves are cancelled by the erasure cascade.
func (s *service) autopayCancelForErasure(ctx context.Context, consumerID primitive.ObjectID) {
	list, err := s.repo.listMandates(ctx, consumerID)
	if err != nil {
		return
	}
	for i := range list {
		if list[i].Status != "cancelled" {
			s.rzpCancelToken(ctx, &list[i])
		}
	}
}

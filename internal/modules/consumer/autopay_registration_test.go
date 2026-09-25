package consumer

// AutoPay registration (founder decision 1, 25 Sep): the registration
// payment is real money and lands in the WALLET as a top-up, exactly once,
// whichever of verify, the webhook and the reconcile sweep gets there first.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run AutopayRegistration -v

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func TestAutopayRegistrationCreditsTheWalletOnce(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	cid := w.customer(t, "9000011001", 0)

	view, err := w.svc.createMandate(ctx, cid, "daily", 500, 2000, 150)
	if err != nil {
		t.Fatalf("createMandate: %v", err)
	}
	if view.OrderID == "" || view.Token != view.OrderID || view.CustomerID == "" || view.AmountPaise != 50000 || view.KeyID != "rzp_test_key" {
		t.Fatalf("checkout view: %+v", view)
	}
	// The registration order is a UPI AutoPay registration for this member,
	// capped per debit, charged as presented (Smart Recharge, not a cadence).
	reg := f.orders[view.OrderID]
	tok, _ := reg["token"].(map[string]any)
	if reg["customer_id"] != view.CustomerID || reg["method"] != "upi" || reg["amount"] != float64(50000) ||
		tok["max_amount"] != float64(200000) || tok["frequency"] != "as_presented" {
		t.Fatalf("registration order body: %v", reg)
	}
	po, err := w.svc.repo.findPaymentOrderByID(ctx, view.OrderID)
	if err != nil || po.Purpose != "autopay" || po.AmountPaise != 50000 || po.ConsumerID != cid {
		t.Fatalf("registration payment order: %+v %v", po, err)
	}

	// A bad signature activates nothing and credits nothing.
	_, err = w.svc.verifyMandate(ctx, cid, view.ID, "pay_reg_1", "forged", "")
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != 400 {
		t.Fatalf("forged verify: %v", err)
	}
	if got := w.cash(t, cid); got != 0 {
		t.Fatalf("forged verify credited %v", got)
	}

	// The real one: credited, active, the bank token read from the payment.
	f.paymentTokens["pay_reg_1"] = "token_reg_1"
	sig := rzpTestSignature("rzp_test_secret", view.OrderID, "pay_reg_1")
	m, err := w.svc.verifyMandate(ctx, cid, view.ID, "pay_reg_1", sig, "")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if m.Status != "active" || m.Token != "token_reg_1" || m.Threshold != 150 || m.Amount != 500 || m.MaxAmount != 2000 {
		t.Fatalf("activated mandate: %+v", m)
	}
	if got := w.cash(t, cid); got != 500 {
		t.Fatalf("registration payment credited %v, want 500", got)
	}
	// Replays: verify again, then the webhook, then the sweep. One credit.
	if _, err := w.svc.verifyMandate(ctx, cid, view.ID, "pay_reg_1", sig, ""); err != nil {
		t.Fatalf("verify replay: %v", err)
	}
	if err := w.svc.razorpayWebhookEvent(ctx, rzpPaymentEvent("payment.captured", view.OrderID, "pay_reg_1", 50000, "token_reg_1", "")); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	f.captured[view.OrderID] = "pay_reg_1"
	w.svc.reconcilePendingPaymentsAt(ctx, time.Now().Add(time.Hour), f.base())
	if got := w.cash(t, cid); got != 500 {
		t.Fatalf("REPLAY DOUBLE-CREDITED the registration: %v", got)
	}
	n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: cid}, {Key: "ref_id", Value: view.OrderID}, {Key: "type", Value: "TOPUP"}, {Key: "ref_type", Value: "autopay"}})
	if n != 1 {
		t.Fatalf("registration ledger rows: %d", n)
	}
	// "Money added" once, worded as AutoPay.
	evs := crmEventsOf(t, w.db, cid, "wallet.credited")
	if len(evs) != 1 || evs[0].Payload["reason"] != "AutoPay recharge" || evs[0].Payload["account"] != "topup" {
		t.Fatalf("wallet.credited: %+v", evs)
	}
	// The app's own /wallet/verify cannot credit an AutoPay order (it is not
	// a checkout top-up), so it adds nothing either.
	if v, _ := w.svc.verifyPayment(ctx, cid, "pay_reg_1", view.OrderID, sig); v.Verified {
		t.Fatalf("/wallet/verify accepted an AutoPay order")
	}

	// One live mandate per member.
	if _, err := w.svc.createMandate(ctx, cid, "daily", 500, 2000, 0); !errors.As(err, &ae) || ae.Code != "MANDATE_EXISTS" {
		t.Fatalf("second mandate while one is active: %v", err)
	}
	// Cancel asks the bank to cancel the token too.
	if _, err := w.svc.setMandateStatus(ctx, cid, view.ID, "cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(f.cancelled) != 1 || f.cancelled[0] != "token_reg_1" {
		t.Fatalf("gateway token cancel: %v", f.cancelled)
	}
}

// The app never came back from the UPI app: the webhook (or the sweep)
// credits the registration payment and records the bank token; a verify
// that arrives later activates without a second credit.
func TestAutopayRegistrationWebhookBeforeVerify(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	cid := w.customer(t, "9000011002", 0)
	view, err := w.svc.createMandate(ctx, cid, "daily", 300, 1000, 0)
	if err != nil {
		t.Fatalf("createMandate: %v", err)
	}
	if err := w.svc.razorpayWebhookEvent(ctx, rzpPaymentEvent("payment.captured", view.OrderID, "pay_reg_2", 30000, "token_reg_2", "")); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if got := w.cash(t, cid); got != 300 {
		t.Fatalf("webhook credit %v, want 300", got)
	}
	// The bank mandate exists: the captured registration puts it live with
	// its token, verify or not (RV-AP-01).
	m, _ := w.svc.repo.findMandate(ctx, view.ID, cid)
	if m.Status != "active" || m.Token != "token_reg_2" || m.PaymentID != "pay_reg_2" || m.Threshold != autopayDefaultThreshold {
		t.Fatalf("mandate after the webhook: %+v", m)
	}
	m, err = w.svc.verifyMandate(ctx, cid, view.ID, "pay_reg_2", rzpTestSignature("rzp_test_secret", view.OrderID, "pay_reg_2"), "")
	if err != nil || m.Status != "active" || m.Token != "token_reg_2" {
		t.Fatalf("late verify: %+v %v", m, err)
	}
	if got := w.cash(t, cid); got != 300 {
		t.Fatalf("late verify double-credited: %v", got)
	}

	// The sweep alone (no webhook, no verify) also credits a registration,
	// once. A new create supersedes an unapproved pending mandate.
	b := w.customer(t, "9000011003", 0)
	first, err := w.svc.createMandate(ctx, b, "daily", 250, 1000, 0)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	second, err := w.svc.createMandate(ctx, b, "daily", 250, 1000, 0)
	if err != nil {
		t.Fatalf("second create b: %v", err)
	}
	if old, _ := w.svc.repo.findMandate(ctx, first.ID, b); old.Status != "cancelled" || old.CancelReason != "superseded" {
		t.Fatalf("an unapproved pending mandate must be superseded: %+v", old)
	}
	f.captured[second.OrderID] = "pay_reg_3"
	for i := 0; i < 2; i++ {
		w.svc.reconcilePendingPaymentsAt(ctx, time.Now().Add(time.Hour), f.base())
	}
	if got := w.cash(t, b); got != 250 {
		t.Fatalf("swept registration credit %v, want 250", got)
	}
}

// RV-AP-01: the member approved in the UPI app but the app never came back
// (process death, a checkout timeout, a lost network call). The webhook
// alone, or the reconcile sweep alone, credits the registration once and
// puts the mandate ACTIVE with its bank token; once the bank confirms the
// token, Smart Recharge charges it.
func TestAutopayRegistrationCapturedWithoutVerifyGoesLive(t *testing.T) {
	t.Setenv("MANDATE_AUTODEBIT", "true")
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	day := istDayAt("2026-10-05", 9, 0)

	// Webhook only.
	a := w.customer(t, "9000011011", 0)
	va, err := w.svc.createMandate(ctx, a, "daily", 300, 1000, 500)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := w.svc.razorpayWebhookEvent(ctx, rzpPaymentEvent("payment.captured", va.OrderID, "pay_wh_a", 30000, "token_wh_a", "")); err != nil {
			t.Fatalf("webhook: %v", err)
		}
	}
	ma, _ := w.svc.repo.findMandate(ctx, va.ID, a)
	if ma.Status != "active" || ma.Token != "token_wh_a" || ma.PaymentID != "pay_wh_a" {
		t.Fatalf("webhook-only registration: %+v", ma)
	}
	if got := w.cash(t, a); got != 300 {
		t.Fatalf("webhook-only registration credit %v, want 300", got)
	}
	// Not charged before the bank confirms the token.
	f.tokenStatus["token_wh_a"] = "initiated"
	if n := w.svc.sweepAutopay(ctx, day); n != 0 {
		t.Fatalf("charged an unconfirmed token")
	}
	if err := w.svc.razorpayWebhookEvent(ctx, rzpTokenEvent("token.confirmed", "token_wh_a", "confirmed")); err != nil {
		t.Fatalf("token webhook: %v", err)
	}
	// ₹300 is under the ₹500 line: Smart Recharge charges the new mandate.
	if n := w.svc.sweepAutopay(ctx, day.Add(15*time.Minute)); n != 1 {
		t.Fatalf("a webhook-only registration was not charged once confirmed")
	}
	if calls := f.recurringCalls(); len(calls) != 1 || calls[0]["token"] != "token_wh_a" {
		t.Fatalf("recurring charge on the webhook-only mandate: %v", calls)
	}

	// Reconcile sweep only: no webhook, no verify; the token comes from GET
	// /payments/{id}.
	b := w.customer(t, "9000011012", 0)
	vb, err := w.svc.createMandate(ctx, b, "daily", 250, 1000, 400)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	f.captured[vb.OrderID] = "pay_sw_b"
	f.paymentTokens["pay_sw_b"] = "token_sw_b"
	for i := 0; i < 2; i++ {
		w.svc.reconcilePendingPaymentsAt(ctx, time.Now().Add(time.Hour), f.base())
	}
	mb, _ := w.svc.repo.findMandate(ctx, vb.ID, b)
	if mb.Status != "active" || mb.Token != "token_sw_b" || mb.PaymentID != "pay_sw_b" {
		t.Fatalf("sweep-only registration: %+v", mb)
	}
	if got := w.cash(t, b); got != 250 {
		t.Fatalf("sweep-only registration credit %v, want 250", got)
	}
	n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: b}, {Key: "ref_id", Value: vb.OrderID}})
	if n != 1 {
		t.Fatalf("sweep-only registration ledger rows: %d", n)
	}
	f.tokenStatus["token_sw_b"] = "confirmed"
	before := len(f.recurringCalls())
	if n := w.svc.sweepAutopay(ctx, day.Add(2*time.Hour)); n != 1 {
		t.Fatalf("a sweep-only registration was not charged once confirmed")
	}
	if calls := f.recurringCalls(); len(calls) != before+1 || calls[len(calls)-1]["token"] != "token_sw_b" {
		t.Fatalf("recurring charge on the sweep-only mandate: %v", calls)
	}
	// A verify that arrives late answers the active mandate, credits nothing.
	got, err := w.svc.verifyMandate(ctx, b, vb.ID, "pay_sw_b", rzpTestSignature("rzp_test_secret", vb.OrderID, "pay_sw_b"), "")
	if err != nil || got.Status != "active" {
		t.Fatalf("late verify: %+v %v", got, err)
	}
	if got := w.cash(t, b); got != 250 {
		t.Fatalf("late verify double-credited: %v", got)
	}
}

// RV-AP-02: a PYAAS mandate PYAAS will never charge is never left live in
// the member's UPI app. A superseded pending mandate that holds a bank token
// has it cancelled at the gateway; a registration captured after its
// mandate was superseded or cancelled by the member records the token and
// cancels it at once (once, whatever repeats), and its money still lands in
// the wallet once.
func TestAutopayRegistrationCancelledMandateTokenIsCancelled(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)

	// Superseded while holding a token.
	a := w.customer(t, "9000011021", 0)
	first, err := w.svc.createMandate(ctx, a, "daily", 300, 1000, 0)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := w.db.Collection(collMandates).UpdateOne(ctx, bson.D{{Key: "mandate_id", Value: first.ID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "token", Value: "token_sup_a"}}}}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if _, err := w.svc.createMandate(ctx, a, "daily", 300, 1000, 0); err != nil {
		t.Fatalf("second create a: %v", err)
	}
	if old, _ := w.svc.repo.findMandate(ctx, first.ID, a); old.Status != "cancelled" || old.CancelReason != "superseded" {
		t.Fatalf("superseded: %+v", old)
	}
	if len(f.cancelled) != 1 || f.cancelled[0] != "token_sup_a" {
		t.Fatalf("a superseded mandate's bank token must be cancelled: %v", f.cancelled)
	}

	// Superseded before its token was known; the old registration is
	// captured later.
	b := w.customer(t, "9000011022", 0)
	oldB, err := w.svc.createMandate(ctx, b, "daily", 200, 1000, 0)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	newB, err := w.svc.createMandate(ctx, b, "daily", 200, 1000, 0)
	if err != nil {
		t.Fatalf("second create b: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := w.svc.razorpayWebhookEvent(ctx, rzpPaymentEvent("payment.captured", oldB.OrderID, "pay_late_b", 20000, "token_late_b", "")); err != nil {
			t.Fatalf("late capture: %v", err)
		}
	}
	gb, _ := w.svc.repo.findMandate(ctx, oldB.ID, b)
	if gb.Status != "cancelled" || gb.Token != "token_late_b" {
		t.Fatalf("a superseded mandate captured late: %+v", gb)
	}
	if nb, _ := w.svc.repo.findMandate(ctx, newB.ID, b); nb.Status != "pending" {
		t.Fatalf("the newer registration must be left alone: %+v", nb)
	}
	if got := w.cash(t, b); got != 200 {
		t.Fatalf("late registration credit %v, want 200", got)
	}
	if len(f.cancelled) != 2 || f.cancelled[1] != "token_late_b" {
		t.Fatalf("a late-captured superseded registration must cancel its token once: %v", f.cancelled)
	}

	// Cancelled by the member before its token was known.
	c := w.customer(t, "9000011023", 0)
	vc, err := w.svc.createMandate(ctx, c, "daily", 200, 1000, 0)
	if err != nil {
		t.Fatalf("create c: %v", err)
	}
	if _, err := w.svc.setMandateStatus(ctx, c, vc.ID, "cancel"); err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	if len(f.cancelled) != 2 {
		t.Fatalf("no token yet, nothing to cancel: %v", f.cancelled)
	}
	f.captured[vc.OrderID] = "pay_late_c"
	f.paymentTokens["pay_late_c"] = "token_late_c"
	for i := 0; i < 2; i++ {
		w.svc.reconcilePendingPaymentsAt(ctx, time.Now().Add(time.Hour), f.base())
	}
	gc, _ := w.svc.repo.findMandate(ctx, vc.ID, c)
	if gc.Status != "cancelled" || gc.Token != "token_late_c" {
		t.Fatalf("a member-cancelled mandate captured late: %+v", gc)
	}
	if len(f.cancelled) != 3 || f.cancelled[2] != "token_late_c" {
		t.Fatalf("a late-captured cancelled registration must cancel its token once: %v", f.cancelled)
	}
	if got := w.cash(t, c); got != 200 {
		t.Fatalf("late registration credit %v, want 200", got)
	}
}

// The offline dev seam (no key secret, OTP dev mode): the demo approval
// activates without moving money; only a payment signed with the dev
// secret is credited, once. Nothing reaches a gateway.
func TestAutopayRegistrationDevSeam(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000011004", 0)
	view, err := w.svc.createMandate(ctx, cid, "daily", 500, 2000, 0)
	if err != nil {
		t.Fatalf("dev create: %v", err)
	}
	if view.CustomerID != "" || view.OrderID == "" {
		t.Fatalf("dev view: %+v", view)
	}
	m, err := w.svc.verifyMandate(ctx, cid, view.ID, "demo_"+view.ID, "demo", "")
	if err != nil || m.Status != "active" {
		t.Fatalf("demo approval: %+v %v", m, err)
	}
	if got := w.cash(t, cid); got != 0 {
		t.Fatalf("a demo approval credited %v", got)
	}

	b := w.customer(t, "9000011005", 0)
	vb, err := w.svc.createMandate(ctx, b, "daily", 400, 2000, 0)
	if err != nil {
		t.Fatalf("dev create b: %v", err)
	}
	sig := rzpTestSignature(devRazorpaySecret, vb.OrderID, "pay_dev_1")
	for i := 0; i < 2; i++ {
		if _, err := w.svc.verifyMandate(ctx, b, vb.ID, "pay_dev_1", sig, ""); err != nil {
			t.Fatalf("dev-signed verify: %v", err)
		}
	}
	if got := w.cash(t, b); got != 400 {
		t.Fatalf("dev-signed registration credit %v, want 400", got)
	}
}

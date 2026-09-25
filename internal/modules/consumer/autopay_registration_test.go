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
	m, _ := w.svc.repo.findMandate(ctx, view.ID, cid)
	if m.Status != "pending" || m.Token != "token_reg_2" || m.Threshold != autopayDefaultThreshold {
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

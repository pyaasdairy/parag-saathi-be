package consumer

// B-03 for AutoPay: a Smart Recharge charge the bank refused is not a
// recharge the member tried, so it does not say "Your recharge didn't go
// through ... no money was deducted". It says what happened, with the
// amount, and makes no claim about the bank account (T-B03-AUTOPAY). A
// checkout top-up that fails keeps T-B03 and its registrations.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run CRMB03AutoPay -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func crmInboxRows(t *testing.T, w *chainWorld, cid primitive.ObjectID, trigger string) []struct{ EN, HI, Template string } {
	t.Helper()
	cur, err := w.db.Collection(collConsumerInbox).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}})
	if err != nil {
		t.Fatalf("inbox read: %v", err)
	}
	var rows []struct {
		EN       string `bson:"body_en"`
		HI       string `bson:"body_hi"`
		Template string `bson:"template_id"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	out := make([]struct{ EN, HI, Template string }, 0, len(rows))
	for _, r := range rows {
		out = append(out, struct{ EN, HI, Template string }{r.EN, r.HI, r.Template})
	}
	return out
}

func TestCRMB03AutoPayWording(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	auto := w.customer(t, "9000012001", 0)
	checkout := w.customer(t, "9000012002", 0)
	legacy := w.customer(t, "9000012003", 0)
	w.svc.emitCRMEvent(ctx, "payment.failed", auto, map[string]any{
		"source": "autopay", "mandate_id": "mnd_b03", "autopay_reason": "threshold", "amount": float64(500),
		"reason": "the bank did not accept the AutoPay debit", "payment_order_id": "", "payment_id": "",
		"scope_key": "autopay:threshold:mnd_b03:2026-10-06-1",
	})
	w.svc.emitCRMEvent(ctx, "payment.failed", checkout, map[string]any{
		"source": "razorpay", "payment_order_id": "", "payment_id": "pay_b03c", "amount": float64(500),
		"reason": "UPI app declined", "scope_key": "pay_b03c",
	})
	// An event with no source reads as a checkout.
	w.svc.emitCRMEvent(ctx, "payment.failed", legacy, map[string]any{"amount": float64(300), "scope_key": "pay_legacy"})
	w.svc.crmProcessEvents(ctx)
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(11*time.Minute))

	rows := crmInboxRows(t, w, auto, "B-03")
	if len(rows) != 1 || rows[0].Template != "T-B03-AUTOPAY" ||
		rows[0].EN != "AutoPay couldn't add ₹500 to your Wallet this time. Add money in the app so your next delivery isn't paused." {
		t.Fatalf("AutoPay B-03: %+v", rows)
	}
	for _, cid := range []primitive.ObjectID{checkout, legacy} {
		rows := crmInboxRows(t, w, cid, "B-03")
		if len(rows) != 1 || rows[0].Template != "T-B03" {
			t.Fatalf("checkout B-03 must keep T-B03: %+v", rows)
		}
	}
	// Once per refused charge, however often it is reported.
	w.svc.emitCRMEvent(ctx, "payment.failed", auto, map[string]any{
		"source": "autopay", "amount": float64(500), "scope_key": "autopay:threshold:mnd_b03:2026-10-06-1",
	})
	w.svc.crmProcessEvents(ctx)
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(22*time.Minute))
	if got := crmDispatchScopes(t, w.db, auto, "B-03"); len(got) != 1 {
		t.Fatalf("B-03 once per AutoPay charge: %v", got)
	}

	// The primary template keeps the trigger's registrations; B-03 reaches
	// the phone by push beside SMS and WhatsApp.
	cfg := crmConfigLoad()
	b03 := cfg.Triggers["B-03"]
	if b03.Template.String() != "T-B03" || b03.Template.Else != "T-B03-AUTOPAY" {
		t.Fatalf("B-03 template: %+v", b03.Template)
	}
	if p := b03.Delivery.Parallel; len(p) != 2 || p[0] != "whatsapp" || p[1] != "push" {
		t.Fatalf("B-03 parallel channels: %v", p)
	}
}

package consumer

// R1-03: a Razorpay checkout often fails one method and then succeeds with
// another on the SAME order (UPI declined, then card). The failed attempt's
// payment.failed webhook arrives first, and B-03 ("Your recharge didn't go
// through ... no money was deducted") went out on the next tick, next to the
// B-06 receipt for the money that did arrive. B-03 now waits a short while
// and is dropped when the top-up it is about has been paid by then.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMB03 -v

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMB03WithdrawnWhenTheCheckoutRetrySucceeds(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	w.svc.rzpKeyID, w.svc.rzpKeySecret, w.svc.rzpWebhookSecret = "key", "secret", "whsec_b03"

	seedOrder := func(cid primitive.ObjectID, orderID string) {
		t.Helper()
		if err := w.svc.repo.insertPaymentOrder(ctx, &paymentOrder{
			ID: primitive.NewObjectID(), OrderID: orderID, ConsumerID: cid, AmountPaise: 50000,
			Receipt: "wtu_" + cid.Hex(), Purpose: "topup", Status: "CREATED", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed payment order: %v", err)
		}
	}
	failed := func(orderID, paymentID string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"event": "payment.failed", "payload": map[string]any{"payment": map[string]any{"entity": map[string]any{
			"id": paymentID, "order_id": orderID, "status": "failed", "amount": 50000, "error_description": "UPI app declined",
		}}}})
		if err := w.svc.razorpayWebhookEvent(ctx, body); err != nil {
			t.Fatalf("payment.failed webhook: %v", err)
		}
	}

	// (1) Attempt 1 fails, attempt 2 on the same order is paid and verified.
	retried := w.customer(t, "9000007701", 0)
	seedOrder(retried, "order_b03_retry")
	failed("order_b03_retry", "pay_try1")
	if v, err := w.svc.verifyPayment(ctx, retried, "pay_try2", "order_b03_retry", rzpTestSignature("secret", "order_b03_retry", "pay_try2")); err != nil || !v.Verified {
		t.Fatalf("verifyPayment: %+v %v", v, err)
	}
	// (2) A failure that stands: nothing is paid on the order.
	stuck := w.customer(t, "9000007702", 0)
	seedOrder(stuck, "order_b03_stuck")
	failed("order_b03_stuck", "pay_stuck1")

	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchScopes(t, w.db, stuck, "B-03"); len(got) != 0 {
		t.Fatalf("B-03 must wait for a checkout retry before it speaks: %v", got)
	}
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(11*time.Minute))

	if n := inboxCount(t, w.db, retried, "B-03"); n != 0 {
		t.Fatalf("B-03 told a member whose retry was paid that the recharge failed: %d rows", n)
	}
	if n := inboxCount(t, w.db, retried, "B-06"); n != 1 {
		t.Fatalf("B-06 rows for the paid retry = %d, want 1", n)
	}
	if got := w.cash(t, retried); got != 500 {
		t.Fatalf("cash after the paid retry = %v, want 500", got)
	}
	if rows := crmScheduleRows(t, w.db, retried, "B-03"); len(rows) != 1 || rows[0].Status != "SKIPPED" {
		t.Fatalf("the B-03 schedule must be skipped once the order is paid: %+v", rows)
	}
	if got := crmDispatchScopes(t, w.db, stuck, "B-03"); len(got) != 1 || got["pay_stuck1"] != "SENT" {
		t.Fatalf("B-03 for a failure that stands, once: %v", got)
	}
}

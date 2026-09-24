package consumer

// R1-05: a delayed trigger re-evaluates its conditions when it fires, but the
// conditions read what the EVENT recorded (the change, the delivered-order
// count), so a member who acted during the delay still got the message:
//   - C-03 "We noticed a change. Everything okay?" an hour after a pause the
//     member had already resumed;
//   - A-05 "recharge your Wallet to start tomorrow's delivery" for a plan the
//     member had cancelled;
//   - A-01 "recharge your Wallet & place your first order" to a member who had
//     recharged and placed an order that was not delivered yet.
// The fire-time stale check now reads the live plan and the member's orders.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMDelayedRecheck -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMDelayedRecheckReadsWhatHappenedMeanwhile(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	t0 := time.Now()
	tomorrow := addDaysIST(istToday(time.Now()), 1)
	plan := func(cid primitive.ObjectID) *subscription {
		t.Helper()
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: tomorrow})
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		return sub
	}
	status := func(cid primitive.ObjectID, sub *subscription, action string) {
		t.Helper()
		if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, action); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}

	// C-03: one member pauses and resumes ten minutes later; one stays paused.
	resumed := w.customer(t, "9000007801", 500)
	subR := plan(resumed)
	stays := w.customer(t, "9000007802", 500)
	subS := plan(stays)
	status(resumed, subR, "pause")
	status(stays, subS, "pause")
	// A-05: an unfunded plan the member cancels inside the two hours.
	gaveUp := w.customer(t, "9000007803", 0)
	subG := plan(gaveUp)
	// A-01: a new member who recharges and orders inside the two hours, and one
	// who does nothing.
	shops := w.customer(t, "9000007804", 0)
	idle := w.customer(t, "9000007805", 0)
	for _, cid := range []primitive.ObjectID{shops, idle} {
		w.svc.emitCRMEvent(ctx, "user.registered", cid, map[string]any{"source": "otp"})
	}
	w.svc.crmProcessEventsAt(ctx, t0)

	status(resumed, subR, "resume")
	status(gaveUp, subG, "cancel")
	if _, err := w.svc.creditTopup(ctx, shops, 500, "razorpay", "order_recheck_a01"); err != nil {
		t.Fatalf("creditTopup: %v", err)
	}
	if _, err := w.svc.createOrder(ctx, shops.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1", Lane: "morning", ConsumerName: "New", Phone: "9000007804",
	}); err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	w.svc.crmProcessEventsAt(ctx, t0.Add(10*time.Minute))
	w.svc.crmFireDueSchedules(ctx, t0.Add(2*time.Hour+time.Minute))

	for _, c := range []struct {
		name    string
		cid     primitive.ObjectID
		trigger string
		want    int
	}{
		{"C-03 for a pause the member resumed", resumed, "C-03", 0},
		{"C-03 for a plan still paused", stays, "C-03", 1},
		{"A-05 for a plan the member cancelled", gaveUp, "A-05", 0},
		{"A-01 for a member who recharged and ordered", shops, "A-01", 0},
		{"A-01 for a member who did nothing", idle, "A-01", 1},
	} {
		if n := inboxCount(t, w.db, c.cid, c.trigger); n != c.want {
			t.Errorf("%s: %d inbox rows, want %d (schedules %+v)", c.name, n, c.want, crmScheduleRows(t, w.db, c.cid, c.trigger))
		}
	}
}

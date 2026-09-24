package consumer

// R2F-15: the LOCK step retried an unfunded preview on every tick until its
// day ended. A member who topped up on the delivery morning, after the
// 05:00-07:30 route had left, got a task minted for a round already gone;
// the next noon the missed-order close cancelled it and told the member
// "could not reach you (missed)" about 30 hours after the route. A day whose
// route has started is no longer locked: its preview expires instead.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run RouteStart -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSubscriptionPreviewNotLockedOnceItsRouteStarted(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)

	member := func(phone string) (primitive.ObjectID, *subscription) {
		t.Helper()
		cid := w.customer(t, phone, 0) // nothing in the wallet at the cut-off
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{
			ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: D1,
		})
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -2), 9, 0))
		return cid, sub
	}
	early, earlySub := member("9000011401") // tops up in the night, before the route
	late, lateSub := member("9000011402")   // tops up after the route has left

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 11, 0))  // preview D+1
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 15)) // the lock: unfunded, retried
	for _, s := range []*subscription{earlySub, lateSub} {
		if o := liveSubOrder(t, w, s.SubscriptionID, D1); o == nil || o.SubLockedAt != "" {
			t.Fatalf("setup: an unfunded D+1 must stay an unlocked preview: %+v", o)
		}
	}

	// Before the route: the retry still locks it (the grace the lock keeps).
	if _, err := w.svc.creditTopup(ctx, early, 500, "razorpay", "order_early_1"); err != nil {
		t.Fatalf("early top-up: %v", err)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 4, 30))
	eo := liveSubOrder(t, w, earlySub.SubscriptionID, D1)
	if eo == nil || eo.SubLockedAt == "" {
		t.Fatalf("a top-up before the route must still lock D+1: %+v", eo)
	}
	if tk, _ := w.svc.repo.findDeliveryByOrder(ctx, eo.OrderID); tk == nil {
		t.Fatalf("the early lock must mint the store task")
	}

	// After the route has left: no lock, no task; the preview expires.
	if _, err := w.svc.creditTopup(ctx, late, 500, "razorpay", "order_late_1"); err != nil {
		t.Fatalf("late top-up: %v", err)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 10, 15))
	if o := liveSubOrder(t, w, lateSub.SubscriptionID, D1); o != nil {
		t.Fatalf("a D+1 preview was locked after its route had left: %+v", o)
	}
	var lateOrders []order
	cur, err := w.db.Collection(collOrders).Find(ctx, bson.D{{Key: "subscription_id", Value: lateSub.SubscriptionID}, {Key: "scheduled_for", Value: D1}})
	if err != nil || cur.All(ctx, &lateOrders) != nil || len(lateOrders) != 1 {
		t.Fatalf("late member's D+1 orders: %d (%v)", len(lateOrders), err)
	}
	if o := lateOrders[0]; o.Status != "cancelled" || o.CancelledBy == orderCancelledByMissed {
		t.Fatalf("the late preview must expire (cancelled, not missed): status %s by %q", o.Status, o.CancelledBy)
	}
	if tk, _ := w.svc.repo.findDeliveryByOrder(ctx, lateOrders[0].OrderID); tk != nil {
		t.Fatalf("a store task was minted after the route: %+v", tk)
	}

	// The plan goes on: D+2 (funded now) locks at its own cut-off, D+1 12:00.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 15))
	if o := liveSubOrder(t, w, lateSub.SubscriptionID, D2); o == nil || o.SubLockedAt == "" {
		t.Fatalf("the late member's D+2 must lock as usual: %+v", o)
	}
	// The next noon: nothing of the late member's is closed as missed, and
	// the member is not told the day was missed.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D2, 12, 30))
	if evs := failedEventsFor(t, w, lateOrders[0].OrderID); len(evs) != 0 {
		t.Fatalf("order.failed for a day that was never locked: %+v", evs)
	}
}

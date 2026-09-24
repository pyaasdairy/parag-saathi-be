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

// PM-03: the CATCH-UP step (a due day no tick ever previewed, the server down
// over the preview and the cut-off) had no route-start guard. A boot sweep at
// 09:30 minted a LOCKED order and a store task for TODAY, whose 05:00 route
// had already left; the next noon the missed-order close cancelled it and
// told the member "not delivered" about 30 hours later. Before the route the
// catch-up still puts the day on the round.
func TestSubscriptionCatchUpSkipsADayWhoseRouteLeft(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-11-10"
	D1 := addDaysIST(D, 1)
	plan := func(phone string) *subscription {
		t.Helper()
		cid := w.customer(t, phone, 1000)
		sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{
			ProductID: "gold-500ml", Variant: "500ml", Qty: 1, Frequency: "daily", StartDate: D,
		}, istDayAt(addDaysIST(D, -2), 9, 0))
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		return sub
	}
	early := plan("9000011501") // the server is back at 03:00, before the route
	late := plan("9000011502")  // the server is back at 09:30, after it

	// No tick ran between the plans' creation and the delivery morning.
	w.svc.sweepOneSubscription(ctx, early, istDayAt(D, 3, 0))
	eo := liveSubOrder(t, w, early.SubscriptionID, D)
	if eo == nil || eo.SubLockedAt == "" {
		t.Fatalf("a catch-up before the route must still lock the day: %+v", eo)
	}
	if tk, _ := w.svc.repo.findDeliveryByOrder(ctx, eo.OrderID); tk == nil {
		t.Fatalf("the catch-up before the route must mint the store task")
	}

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 30))
	if n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
		{Key: "subscription_id", Value: late.SubscriptionID}, {Key: "scheduled_for", Value: D},
	}); n != 0 {
		o := liveSubOrder(t, w, late.SubscriptionID, D)
		t.Fatalf("the catch-up minted %d order(s) for %s after its route had left: %+v", n, D, o)
	}
	// The plan goes on: D+1 is previewed now and locks at its own cut-off.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 15))
	if o := liveSubOrder(t, w, late.SubscriptionID, D1); o == nil || o.SubLockedAt == "" {
		t.Fatalf("the late plan's D+1 must lock as usual: %+v", o)
	}
	// The next noon closes nothing of the late plan's and tells it nothing.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 30))
	cur, err := w.db.Collection(collOrders).Find(ctx, bson.D{{Key: "subscription_id", Value: late.SubscriptionID}})
	if err != nil {
		t.Fatalf("orders: %v", err)
	}
	var lateOrders []order
	if err := cur.All(ctx, &lateOrders); err != nil {
		t.Fatalf("orders decode: %v", err)
	}
	for _, o := range lateOrders {
		if o.CancelledBy == orderCancelledByMissed {
			t.Fatalf("the late plan's %s was closed as missed: %+v", o.ScheduledFor, o)
		}
		if evs := failedEventsFor(t, w, o.OrderID); len(evs) != 0 {
			t.Fatalf("order.failed for the late plan's %s: %+v", o.ScheduledFor, evs)
		}
	}
}

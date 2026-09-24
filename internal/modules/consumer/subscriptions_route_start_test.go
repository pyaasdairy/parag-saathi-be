package consumer

// R2F-15, under the low-wallet rule of 24 Sep: a day whose morning route has
// left is never locked. The noon lock decides a day once, on the wallet as
// it stood at 12:00, so an unfunded day is skipped at the lock and a top-up
// in the night does not mint a task for it. What can still reach a day's
// route start undecided is a preview no lock ever saw (the server down from
// before noon until after 05:00): it expires (cancelled, the day stays
// skipped, no task, no money, no order.failed) instead of minting a task for
// a round already gone, which the missed close cancelled the next noon and
// D-09 announced about 30 hours after the route.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run RouteStart -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func routeStartMember(t *testing.T, w *chainWorld, phone string, fund float64, D string) (primitive.ObjectID, *subscription) {
	t.Helper()
	cid := w.customer(t, phone, fund)
	sub, err := w.svc.createSubscription(context.Background(), cid, subscriptionInput{
		ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: addDaysIST(D, 1),
	})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -2), 9, 0))
	return cid, sub
}

// The server is down from 11:00 until after the next morning's route: the
// preview no lock decided expires at the first tick, and the plan goes on.
func TestSubscriptionPreviewNotLockedOnceItsRouteStarted(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	_, sub := routeStartMember(t, w, "9000011402", 500, D)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 11, 0)) // previews D+1; then no tick until 10:15 on D+1
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 10, 15))
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o != nil {
		t.Fatalf("a D+1 preview was locked after its route had left: %+v", o)
	}
	rows := subOrdersFor(t, w, sub.SubscriptionID, D1)
	if len(rows) != 1 {
		t.Fatalf("D+1 orders: %d", len(rows))
	}
	if o := rows[0]; o.Status != "cancelled" || o.CancelledBy == orderCancelledByMissed || o.CancelledBy == orderCancelledByWalletShort {
		t.Fatalf("the undecided preview must expire (cancelled, neither missed nor wallet_short): status %s by %q", o.Status, o.CancelledBy)
	}
	if tk, _ := w.svc.repo.findDeliveryByOrder(ctx, rows[0].OrderID); tk != nil {
		t.Fatalf("a store task was minted after the route: %+v", tk)
	}

	// The plan goes on: D+2 locks at its own cut-off, D+1 12:00, and the
	// next noon nothing is closed as missed and nobody is told a day was.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 15))
	assertLocked(t, w, sub, D2)
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D2, 12, 30))
	if evs := failedEventsFor(t, w, rows[0].OrderID); len(evs) != 0 {
		t.Fatalf("order.failed for a day that was never locked: %+v", evs)
	}
}

// The lock at noon skips a day the wallet did not cover; a top-up in the
// night, before the route, does not bring it back (no late task), and it
// reaches the next day whose cut-off it beat.
func TestSubscriptionNightTopUpDoesNotBringBackASkippedDay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	cid, sub := routeStartMember(t, w, "9000011401", 0, D)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 11, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 15))
	assertSkipped(t, w, sub, D1)
	chainCreditAt(t, w, cid, 500, "order_night_1", istDayAt(D1, 1, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 4, 30))
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o != nil {
		t.Fatalf("a night top-up brought back a day skipped at noon: %+v", o)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 15))
	assertLocked(t, w, sub, D2)
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D2, 12, 30))
	for _, o := range subOrdersFor(t, w, sub.SubscriptionID, D1) {
		if evs := failedEventsFor(t, w, o.OrderID); len(evs) != 0 {
			t.Fatalf("order.failed for a skipped day: %+v", evs)
		}
	}
}

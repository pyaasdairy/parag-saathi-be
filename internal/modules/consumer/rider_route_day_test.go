package consumer

// R4-03: under the noon lock tomorrow's morning tasks exist (and are
// assigned) from 12:00 the day before. The rider's day read "assigned today
// OR still open" with no delivery-date bound, so a rider given tomorrow's
// round at 15:00 saw those stops counted as today's: the header's pending
// count, POST /route/complete answering ROUTE_NOT_COMPLETE until the next
// morning, and /inventory/today listing tomorrow's crates (a sheet verified
// that day froze today's session with tomorrow's demand). A stop due on a
// later day is not part of today's route; it is part of its own day's.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run RiderRouteDay -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestRiderRouteDayLeavesTomorrowsStopsForTomorrow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	rider := w.riderID.Hex()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)

	task := func(id, status, deliveryDate, assignedAt, product string) {
		t.Helper()
		if _, err := w.db.Collection(collDeliveries).InsertOne(ctx, delivery{
			MongoID: primitive.NewObjectID(), ID: id, OrderID: "ord_" + id,
			StoreID: w.storeID.Hex(), RiderPartyID: rider, ConsumerID: primitive.NewObjectID().Hex(),
			Status: status, DeliveryDate: deliveryDate, AssignedAt: assignedAt, Lane: "morning",
			Items: []deliveryItem{{ProductID: product, Name: "Milk " + product, Variant: "500ml", Qty: 2}},
		}); err != nil {
			t.Fatalf("seed task %s: %v", id, err)
		}
	}
	// Today's work is done: the morning stop and an undated instant run.
	task("dlv_today", "DELIVERED", D, rfc3339(istDayAt(D, 5, 10)), "gold-500ml")
	task("dlv_instant", "DELIVERED", "", rfc3339(istDayAt(D, 11, 0)), "taaza-500ml")
	// Tomorrow's morning round, locked at noon and assigned at 15:00 today.
	task("dlv_tomorrow", "ASSIGNED", D1, rfc3339(istDayAt(D, 15, 0)), "taaza-1l")

	today, err := w.svc.repo.riderRouteTasksForDay(ctx, rider, D)
	if err != nil {
		t.Fatalf("route for %s: %v", D, err)
	}
	for _, tk := range today {
		if tk.ID == "dlv_tomorrow" {
			t.Errorf("tomorrow's stop is in today's route")
		}
	}
	if sum := riderSummariseRoute(today); sum.Pending != 0 || sum.Total != 2 {
		t.Errorf("today's header: total %d pending %d, want 2 and 0", sum.Total, sum.Pending)
	}
	if err := w.svc.repo.riderCompleteRouteDay(ctx, rider, D); err != nil {
		t.Errorf("closing a finished day refused because of tomorrow's stop: %v", err)
	}
	lines, err := w.svc.repo.riderInventoryDemandForDay(ctx, rider, D)
	if err != nil {
		t.Fatalf("inventory for %s: %v", D, err)
	}
	for _, l := range lines {
		if l.Name == "Milk taaza-1l" {
			t.Errorf("today's pickup sheet carries tomorrow's crate: %+v", l)
		}
	}

	// The next IST day, the stop is that day's work.
	next, err := w.svc.repo.riderRouteTasksForDay(ctx, rider, D1)
	if err != nil {
		t.Fatalf("route for %s: %v", D1, err)
	}
	found := false
	for _, tk := range next {
		found = found || tk.ID == "dlv_tomorrow"
	}
	if !found {
		t.Fatalf("tomorrow's stop missing from its own day's route: %+v", next)
	}
	if sum := riderSummariseRoute(next); sum.Pending != 1 {
		t.Fatalf("its day's header: pending %d, want 1", sum.Pending)
	}
}

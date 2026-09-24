package consumer

// The merge's coexistence check (handoff 5.5): the noon lock (founding), the
// per-order CRM claim scope (integration) and the subscription day index
// hold together in one world. A plan edited after noon keeps tomorrow's
// locked order as it was and applies to the day after tomorrow; the day
// index still refuses a second live order for a day; and two instant orders
// on one day each get their own D-01, as does the locked morning order.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run NoonScopeIndex -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestNoonScopeIndexCoexist(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)

	// A daily plan from before the cut-off: the noon tick locks D+1.
	planner := w.customer(t, "9000011501", 1000)
	sub, err := w.svc.createSubscriptionAt(ctx, planner, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 1, Frequency: "daily", StartDate: D1}, chainPlanMadeAt)
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -2), 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 11, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	locked := liveSubOrder(t, w, sub.SubscriptionID, D1)
	if locked == nil || locked.SubLockedAt == "" {
		t.Fatalf("D+1 must be locked at noon: %+v", locked)
	}

	// Edited after noon: D+1 stays as locked, D+2 takes the change.
	three := 3
	if _, err := w.svc.patchSubscriptionAt(ctx, planner, sub.SubscriptionID, subscriptionPatch{Qty: &three}, istDayAt(D, 12, 30)); err != nil {
		t.Fatalf("patch after noon: %v", err)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 45))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 13, 0))
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o == nil || o.OrderID != locked.OrderID || o.Items[0].Qty != 1 {
		t.Fatalf("the locked D+1 order changed after noon: %+v", o)
	}
	if o := liveSubOrder(t, w, sub.SubscriptionID, D2); o == nil || o.Items[0].Qty != 3 {
		t.Fatalf("the change made after noon must reach D+2: %+v", o)
	}

	// The day index still admits one live order per (plan, day).
	for _, day := range []string{D1, D2} {
		if n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
			{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: day},
			{Key: "status", Value: bson.D{{Key: "$in", Value: subscriptionLiveStatuses}}},
		}); n != 1 {
			t.Fatalf("live orders for %s: %d want 1", day, n)
		}
	}
	dup := *locked
	dup.MongoID, dup.OrderID = primitive.NewObjectID(), newOrderID()
	if placed, err := w.svc.repo.insertSubscriptionOrderDoc(ctx, &dup); err != nil || placed {
		t.Fatalf("the day index must refuse a second live order for D+1: placed=%v err=%v", placed, err)
	}

	// Two instant orders on one day: two D-01s, one per order; and the
	// locked morning order has its own.
	shopper := w.customer(t, "9000011502", 1000)
	var ids []string
	for i := 0; i < 2; i++ {
		o, err := w.svc.createOrder(ctx, shopper.Hex(), orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 1, Price: 35}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
			Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
		})
		if err != nil {
			t.Fatalf("instant order %d: %v", i, err)
		}
		ids = append(ids, o.OrderID)
	}
	w.svc.crmProcessEvents(ctx)
	got := crmDispatchScopes(t, w.db, shopper, "D-01")
	if len(got) != 2 || got[ids[0]] != "SENT" || got[ids[1]] != "SENT" {
		t.Fatalf("D-01 per instant order: %v (orders %v)", got, ids)
	}
	if got := crmDispatchScopes(t, w.db, planner, "D-01"); got[locked.OrderID] != "SENT" {
		t.Fatalf("D-01 for the locked morning order: %v", got)
	}
}

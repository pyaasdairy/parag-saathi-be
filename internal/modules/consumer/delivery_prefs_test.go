package consumer

// The doorstep instructions a customer saves ("don't ring the bell", "hand it
// to the guard") must reach the delivery task — that task is what the rider
// and the store manager read at the door.
//
// Run:
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run DoorstepPrefs -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestDoorstepPrefsReachTheDeliveryTask(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000004001", 500)
	// The saved address carries the customer's doorstep capture.
	if _, err := w.db.Collection(collAddresses).UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: cid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "preferences", Value: bson.M{
			"ring_bell": false, "call_before": true,
			"receiver_name": "Guard at Gate 2", "instructions": "Do not ring the bell, baby sleeping",
		}}}}}); err != nil {
		t.Fatalf("address prefs: %v", err)
	}
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1",
		Lane: "morning", ConsumerName: "Prefs Tester", Phone: "9000004001",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	d, err := w.svc.repo.findDeliveryByOrder(ctx, ord.OrderID)
	if err != nil || d == nil {
		t.Fatalf("delivery task: %v", err)
	}
	if d.DeliveryPrefs == nil {
		t.Fatal("the rider's task carries no doorstep instructions")
	}
	if d.DeliveryPrefs.Note != "Do not ring the bell, baby sleeping" {
		t.Fatalf("note = %q", d.DeliveryPrefs.Note)
	}
	if d.DeliveryPrefs.Receiver != "Guard at Gate 2" || !d.DeliveryPrefs.CallBefore {
		t.Fatalf("prefs not carried through: %+v", d.DeliveryPrefs)
	}
	// "Do not ring the bell" is an INSTRUCTION, not an absent preference: it
	// must survive as an explicit false all the way to the rider's task.
	if d.DeliveryPrefs.RingBell == nil {
		t.Fatal("the customer said DO NOT ring — the task dropped it entirely")
	}
	if *d.DeliveryPrefs.RingBell {
		t.Fatal("do-not-ring was flipped into ring-the-bell")
	}

	// The subscription lane resolves the same way (the worker sets no prefs on
	// the order itself), and an order-level pref still wins per field.
	//
	// The sweep runs at 09:00 on a fixed day, never on the wall clock: from
	// 12:00 IST the catch-up also locks tomorrow, so a sweep at the real time
	// placed two orders and this test failed every afternoon.
	const D = "2026-10-06"
	sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{
		ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D,
	})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	// A plan from before D's cut-off whose preview never ran is caught up by
	// the sweep (noon rule); a plan created just now would start on the first
	// editable day instead.
	chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -2), 9, 0))
	if placed := w.svc.sweepOneSubscription(ctx, sub, istDayAt(D, 9, 0)); placed != 1 {
		t.Fatalf("subscription order not placed: %d", placed)
	}
	var subOrder order
	if err := w.db.Collection(collOrders).FindOne(ctx,
		bson.D{{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: D}}).Decode(&subOrder); err != nil {
		t.Fatalf("subscription order: %v", err)
	}
	sd, _ := w.svc.repo.findDeliveryByOrder(ctx, subOrder.OrderID)
	if sd == nil || sd.DeliveryPrefs == nil || sd.DeliveryPrefs.Note == "" {
		t.Fatalf("morning task missing doorstep instructions: %+v", sd)
	}
}

func TestDoorstepPrefsOrderLevelWins(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000004002", 500)
	if _, err := w.db.Collection(collAddresses).UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: cid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "preferences", Value: bson.M{
			"instructions": "Leave with the guard", "call_before": true,
		}}}}}); err != nil {
		t.Fatalf("address prefs: %v", err)
	}
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1", Lane: "morning",
		ConsumerName: "Override", Phone: "9000004002",
		DeliveryPrefs: map[string]any{"note": "I am home today, hand it to me", "handover": "HAND_TO_CUSTOMER"},
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	d, _ := w.svc.repo.findDeliveryByOrder(ctx, ord.OrderID)
	if d == nil || d.DeliveryPrefs == nil {
		t.Fatal("no prefs on the task")
	}
	if d.DeliveryPrefs.Note != "I am home today, hand it to me" {
		t.Fatalf("this order's own note must win: %q", d.DeliveryPrefs.Note)
	}
	if !d.DeliveryPrefs.CallBefore {
		t.Fatal("the standing call-before flag must survive an order-level note")
	}
}

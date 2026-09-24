package consumer

// R4-01: the store console offers "Reassign rider" on every FAILED card, and
// assignRider took FAILED back to ASSIGNED without reading the order. A task
// failed because the CUSTOMER cancelled could therefore be picked up again,
// the pickup sync flipped the cancelled order to out_for_delivery, and the
// delivery debited the wallet for an order the member had cancelled.
//
// A task the delivery side failed (rider not-delivered, store cancel) is the
// one a reassign is for: the order walks back to live with it.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run StoreReassign -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func deliveryDebitRows(t *testing.T, w *chainWorld, orderID string) int64 {
	t.Helper()
	n, err := w.db.Collection(collWalletTxns).CountDocuments(context.Background(),
		bson.D{{Key: "ref_id", Value: "delivery:" + orderID}, {Key: "type", Value: "DEBIT"}})
	if err != nil {
		t.Fatalf("ledger count: %v", err)
	}
	return n
}

func TestStoreReassignOfCustomerCancelledTaskIsRefused(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	ord := chainMorningOrder(t, w, "9000005101", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	if _, err := w.svc.cancelOrderAt(ctx, ord.UserID, ord.OrderID, ord.PlacedAt); err != nil { // placed-at: its morning is still open
		t.Fatalf("customer cancel: %v", err)
	}
	if d := chainTaskFor(t, w, ord.OrderID); d.Status != "FAILED" {
		t.Fatalf("task after the customer's cancel: %q want FAILED", d.Status)
	}

	// The manager taps Reassign on the red card.
	_, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex())
	if code := geofenceCode(err); code != "ORDER_CANCELLED" {
		t.Fatalf("reassign of a customer-cancelled task: err=%v code=%q, want ORDER_CANCELLED", err, code)
	}
	if d := chainTaskFor(t, w, ord.OrderID); d.Status != "FAILED" || d.RiderPartyID != "" {
		t.Fatalf("a refused reassign changed the task: %q rider %q", d.Status, d.RiderPartyID)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "cancelled" {
		t.Fatalf("order after a refused reassign: %q want cancelled", o.Status)
	}
	if n := deliveryDebitRows(t, w, ord.OrderID); n != 0 {
		t.Fatalf("a cancelled order was charged: %d debit rows", n)
	}
}

// Belt and braces: even when a live task survives a customer cancel (the
// task-fail in cancelOrder is best-effort), the pickup must not resurrect the
// order, and the door refuses the delivery without moving money.
func TestPickupNeverResurrectsACustomerCancelledOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	ord := chainMorningOrder(t, w, "9000005102", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	if _, err := w.db.Collection(collOrders).UpdateOne(ctx, bson.D{{Key: "order_id", Value: ord.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}}}}); err != nil {
		t.Fatalf("stage the customer's cancel: %v", err)
	}
	chainOutForDelivery(t, w, task.ID)
	if o := w.orderByID(t, ord.OrderID); o.Status != "cancelled" {
		t.Fatalf("pickup resurrected a customer-cancelled order: %q", o.Status)
	}
	_, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true})
	if code := geofenceCode(err); code != "ORDER_CANCELLED" {
		t.Fatalf("deliver of a cancelled order: err=%v code=%q, want ORDER_CANCELLED", err, code)
	}
	if n := deliveryDebitRows(t, w, ord.OrderID); n != 0 {
		t.Fatalf("a cancelled order was charged: %d debit rows", n)
	}
}

// The reassign the button exists for: the rider could not deliver, the order
// was cancelled BY THE TASK, and the manager sends another rider. The member
// sees the order live again at once, and it is charged exactly once.
func TestStoreReassignAfterRiderFailWalksOrderBack(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	ord := chainMorningOrder(t, w, "9000005103", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	chainOutForDelivery(t, w, task.ID)
	if _, err := w.svc.failDelivery(ctx, w.rider, task.ID, "CUSTOMER_UNAVAILABLE | rang twice"); err != nil {
		t.Fatalf("failDelivery: %v", err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "cancelled" || o.CancelledBy != orderCancelledByDelivery {
		t.Fatalf("order after the rider's fail: %q by %q", o.Status, o.CancelledBy)
	}
	if _, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex()); err != nil {
		t.Fatalf("reassign after a rider fail: %v", err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "assigned" || o.CancelledBy != "" {
		t.Fatalf("order after the reassign: %q by %q, want assigned and no cancelled_by", o.Status, o.CancelledBy)
	}
	if _, err := w.svc.acceptDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if d, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true}); err != nil || d.Status != "DELIVERED" {
		t.Fatalf("deliver after the reassign: %v %v", d, err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "delivered" {
		t.Fatalf("order after redelivery: %q", o.Status)
	}
	if n := deliveryDebitRows(t, w, ord.OrderID); n != 1 {
		t.Fatalf("debit rows after one delivery: %d want 1", n)
	}
}

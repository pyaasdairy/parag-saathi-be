package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

func failedEventsFor(t *testing.T, w *chainWorld, orderID string) []bson.M {
	t.Helper()
	cur, err := w.db.Collection(collCRMEvents).Find(context.Background(),
		bson.D{{Key: "topic", Value: "order.failed"}, {Key: "payload.order_id", Value: orderID}})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []bson.M
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("events decode: %v", err)
	}
	return out
}

// A rider's FAILED marking used to leave the customer's order out_for_delivery
// forever. It now reads cancelled (the only terminal state the shipped app
// draws), emits order.failed once, moves no money, and a re-assign of the
// same task still walks the order forward to delivered.
func TestFailedTaskCancelsTheOrderAndEmitsOrderFailed(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	ord := chainMorningOrder(t, w, "9000005001", nil)
	cid := w.orderByID(t, ord.OrderID).UserID
	task := chainTaskFor(t, w, ord.OrderID)
	chainOutForDelivery(t, w, task.ID)

	d, err := w.svc.failDelivery(ctx, w.rider, task.ID, "Customer not home")
	if err != nil || d.Status != "FAILED" {
		t.Fatalf("failDelivery: %v %v", d, err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "cancelled" {
		t.Fatalf("order after a failed task: %q want cancelled", o.Status)
	}
	evs := failedEventsFor(t, w, ord.OrderID)
	if len(evs) != 1 {
		t.Fatalf("order.failed events: %d want 1", len(evs))
	}
	p, _ := evs[0]["payload"].(bson.M)
	if p["reason"] != "Customer not home" || p["labelled_product"] == "" || p["labelled_product"] == nil {
		t.Fatalf("order.failed payload: %v", p)
	}
	n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "ref_id", Value: "delivery:" + ord.OrderID}})
	if n != 0 {
		t.Fatalf("a failed delivery moved money: %d rows", n)
	}
	_ = cid

	// The manager re-assigns the FAILED task; the normal syncs carry the order
	// forward again and the delivery lands.
	chainOutForDelivery(t, w, task.ID)
	if o := w.orderByID(t, ord.OrderID); o.Status != "out_for_delivery" {
		t.Fatalf("order after re-assign + pickup: %q want out_for_delivery", o.Status)
	}
	far := &geoPt{Lat: task.Geo.Lat + 0.01, Lng: task.Geo.Lng}
	if d, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: far, GeofenceOK: true}); err != nil || d.Status != "DELIVERED" {
		t.Fatalf("deliver after a failure: %v %v", d, err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "delivered" {
		t.Fatalf("order after redelivery: %q want delivered", o.Status)
	}
	if evs := failedEventsFor(t, w, ord.OrderID); len(evs) != 1 {
		t.Fatalf("order.failed must not be re-emitted: %d", len(evs))
	}
}

// The store's own cancel goes through the same sync: cancelled + one event.
func TestStoreCancelCancelsTheOrderAndEmitsOrderFailed(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	ord := chainMorningOrder(t, w, "9000005002", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	d, err := w.svc.storeCancelDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, "Crate damaged")
	if err != nil || d.Status != "FAILED" {
		t.Fatalf("storeCancelDelivery: %v %v", d, err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "cancelled" {
		t.Fatalf("order after store cancel: %q want cancelled", o.Status)
	}
	evs := failedEventsFor(t, w, ord.OrderID)
	if len(evs) != 1 {
		t.Fatalf("order.failed events: %d want 1", len(evs))
	}
	if p, _ := evs[0]["payload"].(bson.M); p["reason"] != "Crate damaged" {
		t.Fatalf("payload: %v", p)
	}

	// A customer-cancelled order whose task then fails is already terminal:
	// no second flip, no event.
	ord2 := chainMorningOrder(t, w, "9000005003", nil)
	if _, err := w.svc.cancelOrder(ctx, ord2.UserID, ord2.OrderID); err != nil {
		t.Fatalf("cancelOrder: %v", err)
	}
	if evs := failedEventsFor(t, w, ord2.OrderID); len(evs) != 0 {
		t.Fatalf("customer cancel must not emit order.failed: %d", len(evs))
	}
	// ...and the customer's cancel must actually fail the live task (the
	// duplicate updated_at used to make Mongo reject that update silently).
	if d := chainTaskFor(t, w, ord2.OrderID); d.Status != "FAILED" {
		t.Fatalf("task after a customer cancel: %q want FAILED", d.Status)
	}
}

func riderUndo(t *testing.T, w *chainWorld, taskID string) int {
	t.Helper()
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodPost, "/delivery/tasks/"+taskID+"/undo", strings.NewReader(`{"reason":"marked by mistake"}`))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("deliveryId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req = req.WithContext(auth.WithActor(req.Context(), w.rider))
	rec := httptest.NewRecorder()
	h.riderTaskUndo(rec, req)
	return rec.Code
}

// Undoing a FAILED marking walks the order back to out_for_delivery, but only
// when the task itself cancelled it: a customer's cancel stays cancelled.
func TestUndoOfFailedMarkingRestoresTheOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	ord := chainMorningOrder(t, w, "9000005004", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	chainOutForDelivery(t, w, task.ID)
	if _, err := w.svc.failDelivery(ctx, w.rider, task.ID, "Wrong tap"); err != nil {
		t.Fatalf("failDelivery: %v", err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "cancelled" || o.CancelledBy != orderCancelledByDelivery {
		t.Fatalf("order after fail: %q by %q", o.Status, o.CancelledBy)
	}
	if code := riderUndo(t, w, task.ID); code != http.StatusOK {
		t.Fatalf("undo: %d", code)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "out_for_delivery" || o.CancelledBy != "" {
		t.Fatalf("order after undo: %q by %q want out_for_delivery", o.Status, o.CancelledBy)
	}
	near := &geoPt{Lat: task.Geo.Lat + 0.01, Lng: task.Geo.Lng}
	if d, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: near, GeofenceOK: true}); err != nil || d.Status != "DELIVERED" {
		t.Fatalf("deliver after undo: %v %v", d, err)
	}

	// Customer cancels; the task fails; the rider's undo must not bring it back.
	ord2 := chainMorningOrder(t, w, "9000005005", nil)
	task2 := chainTaskFor(t, w, ord2.OrderID)
	chainOutForDelivery(t, w, task2.ID)
	// Only placed/confirmed orders may cancel, so cancel before pickup would be
	// the real path; simulate the customer's cancel on the stored order.
	if _, err := w.db.Collection(collOrders).UpdateOne(ctx, bson.D{{Key: "order_id", Value: ord2.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}}}}); err != nil {
		t.Fatalf("customer cancel: %v", err)
	}
	if _, err := w.svc.failDelivery(ctx, w.rider, task2.ID, "Customer cancelled"); err != nil {
		t.Fatalf("failDelivery 2: %v", err)
	}
	if code := riderUndo(t, w, task2.ID); code != http.StatusOK {
		t.Fatalf("undo 2: %d", code)
	}
	if o := w.orderByID(t, ord2.OrderID); o.Status != "cancelled" {
		t.Fatalf("a customer-cancelled order was resurrected by the rider's undo: %q", o.Status)
	}
	if evs := failedEventsFor(t, w, ord2.OrderID); len(evs) != 0 {
		t.Fatalf("an already-cancelled order must not emit order.failed: %d", len(evs))
	}
}

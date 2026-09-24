package consumer

// G7 (owner, 24 Sep): from 12:00 IST the day before delivery a member can no
// longer cancel a MORNING order (one-off or subscription) for that day: 409
// ORDER_LOCKED, "Orders lock at 12 noon the day before delivery, so this one
// can no longer be cancelled." The store has procured by then, and a cancel
// in the seconds before the lock tick was the one input that could make the
// noon reservation differ between ticks. Before the cut-off the cancel works
// as it always did; store, rider and operator cancels and the instant lane
// are untouched.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run CancelAfterNoon -v

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func assertOrderLocked(t *testing.T, label string, err error) {
	t.Helper()
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != http.StatusConflict || ae.Code != "ORDER_LOCKED" ||
		ae.Message != "Orders lock at 12 noon the day before delivery, so this one can no longer be cancelled." {
		t.Fatalf("%s: want 409 ORDER_LOCKED with the lock message, got %v", label, err)
	}
}

func TestCancelAfterNoonKeepsTomorrow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	long := istDayAt(addDaysIST(D, -2), 9, 0)

	// A plan's tomorrow, locked at noon with its store task.
	cid := w.customer(t, "9000014201", 5000)
	sub := walletLockPlan(t, w, cid, "taaza-500ml", 1, D1, long)
	// A second member's tomorrow the lock tick has not reached yet.
	cid2 := w.customer(t, "9000014202", 5000)
	sub2 := walletLockPlan(t, w, cid2, "taaza-500ml", 1, D1, long)
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	pending := liveSubOrder(t, w, sub2.SubscriptionID, D1)
	if pending == nil {
		t.Fatalf("setup: no preview for tomorrow")
	}
	// 12:00:01, before any lock tick: the cut-off has passed, the day is the
	// lock's to decide.
	_, err := w.svc.cancelOrderAt(ctx, cid2.Hex(), pending.OrderID, istDayAt(D, 12, 0).Add(time.Second))
	assertOrderLocked(t, "tomorrow's preview at 12:00:01", err)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 0).Add(5*time.Second))
	locked := assertLocked(t, w, sub, D1)
	assertLocked(t, w, sub2, D1)
	_, err = w.svc.cancelOrderAt(ctx, cid.Hex(), locked.OrderID, istDayAt(D, 12, 1))
	assertOrderLocked(t, "tomorrow's locked order at 12:01", err)
	if o := w.orderByID(t, locked.OrderID); o.Status != "placed" || o.SubLockedAt == "" {
		t.Fatalf("a refused cancel changed the order: %+v", o)
	}
	if task, _ := w.svc.repo.findDeliveryByOrder(ctx, locked.OrderID); task == nil || task.Status != "ASSIGNED" {
		t.Fatalf("a refused cancel changed the task: %+v", task)
	}
	// The day after tomorrow is still open: its preview cancels (a skip).
	next := liveSubOrder(t, w, sub.SubscriptionID, D2)
	if next == nil {
		t.Fatalf("setup: no preview for the day after tomorrow")
	}
	if o, err := w.svc.cancelOrderAt(ctx, cid.Hex(), next.OrderID, istDayAt(D, 12, 2)); err != nil || o.Status != "cancelled" {
		t.Fatalf("the day after tomorrow's preview must still cancel: %v %+v", err, o)
	}
	// Late at night and on the morning itself, the day stays locked.
	_, err = w.svc.cancelOrderAt(ctx, cid.Hex(), locked.OrderID, istDayAt(D, 23, 59))
	assertOrderLocked(t, "tomorrow's order at 23:59", err)
	_, err = w.svc.cancelOrderAt(ctx, cid.Hex(), locked.OrderID, istDayAt(D1, 4, 30))
	assertOrderLocked(t, "today's order at 04:30", err)

	// One-off morning orders: the same rule.
	one := w.customer(t, "9000014203", 5000)
	mk := func(day string, at time.Time) *order {
		t.Helper()
		o, err := w.svc.createOrderAt(ctx, one.Hex(), morningOrderFor(day), at)
		if err != nil || o.DeliveryDate != day {
			t.Fatalf("one-off for %s: %v %+v", day, err, o)
		}
		return o
	}
	early := mk(D1, istDayAt(D, 10, 0))
	if o, err := w.svc.cancelOrderAt(ctx, one.Hex(), early.OrderID, istDayAt(D, 11, 59)); err != nil || o.Status != "cancelled" {
		t.Fatalf("a one-off for tomorrow cancels before noon: %v %+v", err, o)
	}
	late := mk(D1, istDayAt(D, 10, 5))
	_, err = w.svc.cancelOrderAt(ctx, one.Hex(), late.OrderID, istDayAt(D, 12, 0))
	assertOrderLocked(t, "a one-off for tomorrow at 12:00", err)
	if task, _ := w.svc.repo.findDeliveryByOrder(ctx, late.OrderID); task == nil || task.Status != "ASSIGNED" {
		t.Fatalf("a refused one-off cancel changed the task: %+v", task)
	}
	later := mk(D2, istDayAt(D, 12, 30))
	if o, err := w.svc.cancelOrderAt(ctx, one.Hex(), later.OrderID, istDayAt(D, 13, 0)); err != nil || o.Status != "cancelled" {
		t.Fatalf("a one-off for the day after tomorrow cancels after noon: %v %+v", err, o)
	}
	// Month end: the 1st locks at noon on the 31st.
	const M = "2026-10-31"
	nov1 := mk("2026-11-01", istDayAt(M, 9, 0))
	_, err = w.svc.cancelOrderAt(ctx, one.Hex(), nov1.OrderID, istDayAt(M, 12, 14).Add(59*time.Second))
	assertOrderLocked(t, "the 1st at 12:14:59 on the 31st", err)

	// The instant lane has no cut-off.
	instant := morningOrderFor(D1)
	instant.Lane = "instant"
	io, err := w.svc.createOrderAt(ctx, one.Hex(), instant, istDayAt(D, 13, 0))
	if err != nil {
		t.Fatalf("instant: %v", err)
	}
	if o, err := w.svc.cancelOrderAt(ctx, one.Hex(), io.OrderID, istDayAt(D, 13, 5)); err != nil || o.Status != "cancelled" {
		t.Fatalf("an instant order cancels after noon: %v %+v", err, o)
	}

	// The store can still cancel a locked morning task (crate damaged).
	task, _ := w.svc.repo.findDeliveryByOrder(ctx, late.OrderID)
	if d, err := w.svc.storeCancelDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, "Crate damaged"); err != nil || d.Status != "FAILED" {
		t.Fatalf("the store's cancel of a locked task: %v %+v", err, d)
	}
	if o := w.orderByID(t, late.OrderID); o.Status != "cancelled" {
		t.Fatalf("order after the store's cancel: %q", o.Status)
	}
}

// On the wire (POST /orders/{id}/cancel, the real clock): a morning order for
// today is always past its cut-off, so the refusal is date-independent.
func TestCancelAfterNoonOnTheWire(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000014204", 500)
	today := istToday(time.Now())
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: cid.Hex(), Status: "placed",
		PaymentMethod: "wallet", Lane: "morning", DeliveryDate: today, Total: 35,
		Items:     []orderItem{{ID: newItemID(), ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PlacedAt: time.Now().UTC(),
	}
	if err := w.svc.repo.insertOrder(ctx, o); err != nil {
		t.Fatalf("insert: %v", err)
	}
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodPost, "/orders/"+o.OrderID+"/cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", o.OrderID)
	req = req.WithContext(context.WithValue(context.WithValue(req.Context(), chi.RouteCtxKey, rctx),
		consumerCtxKey, consumerActor{ID: cid.Hex()}))
	rec := httptest.NewRecorder()
	h.cancelOrder(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body["code"] != "ORDER_LOCKED" ||
		!strings.HasPrefix(body["message"].(string), "Orders lock at 12 noon the day before delivery") {
		t.Fatalf("POST cancel of today's morning order: %d %s", rec.Code, rec.Body.String())
	}
	if got := w.orderByID(t, o.OrderID); got.Status != "placed" {
		t.Fatalf("a refused cancel changed the order: %q", got.Status)
	}
}

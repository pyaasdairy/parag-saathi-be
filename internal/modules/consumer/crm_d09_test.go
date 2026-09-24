package consumer

// D-09: the order.failed event the backend already emitted now reaches the
// member, from both product paths (a rider's not-delivered marking and a
// store cancel), once per order, with the picklist cause and no money claim
// that is untrue.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMD09 -v

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func TestCRMD09DeliveryFailedReachesTheMember(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007601", 1000)

	newInstant := func() (*order, delivery) {
		t.Helper()
		o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
			Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
		})
		if err != nil {
			t.Fatalf("createOrder: %v", err)
		}
		var task delivery
		if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&task); err != nil {
			t.Fatalf("task: %v", err)
		}
		return o, task
	}

	// (1) The rider marks it not delivered.
	o1, t1 := newInstant()
	if _, err := w.svc.claimOfferedDelivery(ctx, w.rider, t1.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.svc.failDelivery(ctx, w.rider, t1.ID, "Society gate / lift closed | guard would not open | photo=nd/1.jpg | called=true"); err != nil {
		t.Fatalf("failDelivery: %v", err)
	}
	// (2) The store cancels another one before it leaves.
	o2, t2 := newInstant()
	if _, err := w.svc.storeCancelDelivery(ctx, w.mgr, w.storeID.Hex(), t2.ID, ""); err != nil {
		t.Fatalf("storeCancelDelivery: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	// D-09 waits out the rider's undo window (PT15M) before it is sent.
	if got := crmDispatchScopes(t, w.db, cid, "D-09"); len(got) != 0 {
		t.Fatalf("D-09 must wait the undo window: %v", got)
	}
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(riderUndoWindow+time.Minute))

	got := crmDispatchScopes(t, w.db, cid, "D-09")
	if len(got) != 2 || got[o1.OrderID] != "SENT" || got[o2.OrderID] != "SENT" {
		t.Fatalf("D-09 once per failed order: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "D-09"); n != 2 {
		t.Fatalf("D-09 inbox rows = %d", n)
	}
	var rows []struct {
		BodyEN string `bson:"body_en"`
		BodyHI string `bson:"body_hi"`
		CTA    string `bson:"cta"`
	}
	cur, err := w.db.Collection(collConsumerInbox).Find(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "D-09"}})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	bodies := map[string]bool{}
	for _, r := range rows {
		bodies[r.BodyEN] = true
		if !strings.Contains(r.BodyEN, "Milk gold-500ml 500ml x2"+crmLabelledSuffix) || !strings.Contains(r.BodyEN, "Nothing has been charged for it.") ||
			!strings.Contains(r.BodyEN, "96672 60050") || strings.Contains(r.BodyEN, "[") {
			t.Fatalf("D-09 body_en = %q", r.BodyEN)
		}
		if !strings.Contains(r.BodyHI, "koi shulk nahi laga") || strings.Contains(r.BodyHI, "[") {
			t.Fatalf("D-09 body_hi = %q", r.BodyHI)
		}
		if r.CTA != "reorder" {
			t.Fatalf("D-09 cta = %q", r.CTA)
		}
	}
	want := map[string]string{
		"(Society gate / lift closed)": "the rider's picklist cause, never the remark or the evidence",
		"(Cancelled by the store)":     "the store cancel's default reason",
	}
	for frag, why := range want {
		found := false
		for b := range bodies {
			if strings.Contains(b, frag) {
				found = true
			}
		}
		if !found {
			t.Fatalf("D-09 must carry %s: %v", why, bodies)
		}
	}
	for b := range bodies {
		if strings.Contains(b, "guard would not open") || strings.Contains(b, "photo=") {
			t.Fatalf("the rider's remark or evidence leaked into the message: %q", b)
		}
	}
	// The failed orders are cancelled for the member, and no delivered
	// message ever went out for them.
	for _, id := range []string{o1.OrderID, o2.OrderID} {
		if o := w.orderByID(t, id); o.Status != "cancelled" {
			t.Fatalf("order %s status = %q", id, o.Status)
		}
	}
	if got := crmDispatchScopes(t, w.db, cid, "D-06"); len(got) != 0 {
		t.Fatalf("D-06 must not fire for a failed order: %v", got)
	}
}

// R4-10: a locked morning order whose day passed with no delivery is closed
// by the sweep's missed step. It used to write the machine token "missed" as
// the task's failure reason (the rider app prints it as "Reported: missed"
// for a task the rider never reported) and as the order.failed reason, so
// D-09 told the member "could not reach you (missed)". Both now say what
// happened in words.
func TestCRMD09MissedClosureSaysWhatHappened(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	cid := w.customer(t, "9000007603", 500)
	sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: D1}, chainPlanMadeAt)
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -2), 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 11, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 15)) // D+1 locks, its task is minted
	o := liveSubOrder(t, w, sub.SubscriptionID, D1)
	if o == nil || o.SubLockedAt == "" {
		t.Fatalf("setup: D+1 must be locked: %+v", o)
	}
	// Nobody delivers or reports it; the noon after, the sweep closes it.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D2, 12, 30))
	if got := w.orderByID(t, o.OrderID); got.Status != "cancelled" || got.CancelledBy != orderCancelledByMissed {
		t.Fatalf("setup: the missed order must close: %s by %q", got.Status, got.CancelledBy)
	}
	if task := chainTaskFor(t, w, o.OrderID); task.Status != "FAILED" || task.FailureReason != missedTaskFailureReason {
		t.Errorf("task after the missed close: %s %q, want FAILED %q", task.Status, task.FailureReason, missedTaskFailureReason)
	}
	w.svc.crmProcessEvents(ctx)
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(riderUndoWindow+time.Minute))
	cur, err := w.db.Collection(collConsumerInbox).Find(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "D-09"}})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	var rows []struct {
		BodyEN string `bson:"body_en"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("D-09 rows for the missed order: %d, want 1", len(rows))
	}
	if b := rows[0].BodyEN; strings.Contains(b, "(missed)") || !strings.Contains(b, "("+missedCustomerReason+")") {
		t.Fatalf("D-09 for a missed day must say what happened, not a machine token: %q", b)
	}
}

// R1-04: a rider who mis-taps "not delivered" can undo it for 15 minutes,
// which walks the order back to out_for_delivery. D-09 used to go out on the
// next worker tick, so the member read "Not delivered ... call support" next
// to "Delivered" for the same order. D-09 now waits out the undo window and is
// dropped when the order is live again by the time it comes due.
func TestCRMD09WithdrawnWhenTheRiderUndoesTheFailure(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007602", 1000)
	w.svc.crmProcessEvents(ctx)

	o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	task := chainTaskFor(t, w, o.OrderID)
	if _, err := w.svc.claimOfferedDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if _, err := w.svc.failDelivery(ctx, w.rider, task.ID, "CUSTOMER_UNAVAILABLE | rang twice | photo=nd/a.jpg"); err != nil {
		t.Fatalf("failDelivery: %v", err)
	}
	w.svc.crmProcessEvents(ctx) // the worker's next tick, well inside the undo window
	if code := riderUndo(t, w, task.ID); code != 200 {
		t.Fatalf("rider undo: %d", code)
	}
	if st := w.orderByID(t, o.OrderID).Status; st != "out_for_delivery" {
		t.Fatalf("order after the undo: %q", st)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "https://x/p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true}); err != nil {
		t.Fatalf("deliver after the undo: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	w.svc.crmFireDueSchedules(ctx, time.Now().Add(riderUndoWindow+time.Minute))

	if n := inboxCount(t, w.db, cid, "D-09"); n != 0 {
		t.Fatalf("D-09 reached the member for an order that was then delivered: %d rows", n)
	}
	if n := inboxCount(t, w.db, cid, "D-06"); n != 1 {
		t.Fatalf("D-06 rows = %d, want 1", n)
	}
	rows := crmScheduleRows(t, w.db, cid, "D-09")
	if len(rows) != 1 || rows[0].Status != "SKIPPED" {
		t.Fatalf("the D-09 schedule must be skipped once the order is live again: %+v", rows)
	}
}

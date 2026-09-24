package consumer

// The 12-noon cut-off (One Voice 1.2, the owner's rule of 21 Sep): a
// delivery day's previews lock at 12:00 IST the day before; a change made
// after noon applies to the day after tomorrow; a plan created after noon
// starts on the first editable day; the exactly-once claims survive; a
// duplicate plan is refused; and a locked order whose day passed with no
// delivery is closed as missed.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run 'Noon|Duplicate|Missed' -v

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// chainBackdateSubscription moves a plan's member-change moment to `at` (in
// the DB and on the struct), modelling a plan that existed before a day's
// cut-off: the sweep's catch-up serves such a plan, never one changed after
// the cut-off.
func chainBackdateSubscription(t *testing.T, w *chainWorld, sub *subscription, at time.Time) {
	t.Helper()
	at = at.UTC()
	if _, err := w.db.Collection(collSubscriptions).UpdateOne(context.Background(),
		bson.D{{Key: "subscription_id", Value: sub.SubscriptionID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "changed_at", Value: at}, {Key: "created_at", Value: at}}}}); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	sub.ChangedAt, sub.CreatedAt = at, at
}

// chainStampChange sets only the change moment: the tests drive the sweep on
// a simulated clock, while pause/resume/patch stamp the real one.
func chainStampChange(t *testing.T, w *chainWorld, subID string, at time.Time) {
	t.Helper()
	if _, err := w.db.Collection(collSubscriptions).UpdateOne(context.Background(),
		bson.D{{Key: "subscription_id", Value: subID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "changed_at", Value: at.UTC()}}}}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
}

func istDayAt(day string, hour, min int) time.Time {
	d, ok := parseDay(day)
	if !ok {
		panic("bad day " + day)
	}
	return d.Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
}

// The pure day arithmetic behind the rule.
func TestNoonLockDayArithmetic(t *testing.T) {
	before := istDayAt("2026-10-06", 11, 59)
	after := istDayAt("2026-10-06", 12, 0)
	if lockedThroughDay(before) != "2026-10-06" || firstEditableDay(before) != "2026-10-07" {
		t.Fatalf("before noon: locked through %s, editable %s", lockedThroughDay(before), firstEditableDay(before))
	}
	if lockedThroughDay(after) != "2026-10-07" || firstEditableDay(after) != "2026-10-08" {
		t.Fatalf("from noon: locked through %s, editable %s", lockedThroughDay(after), firstEditableDay(after))
	}
	if lm := lockMomentFor("2026-10-07"); !lm.Equal(istDayAt("2026-10-06", 12, 0)) {
		t.Fatalf("lock moment for the 7th: %v", lm)
	}
	// UTC never leaks in: 06:31 UTC is 12:01 IST.
	utc := time.Date(2026, 10, 6, 6, 31, 0, 0, time.UTC)
	if lockedThroughDay(utc) != "2026-10-07" {
		t.Fatalf("IST hour must decide the lock, got %s", lockedThroughDay(utc))
	}
	sub := &subscription{Status: "active", Frequency: "daily", StartDate: "2026-10-01", ChangedAt: istDayAt("2026-10-06", 10, 0)}
	if !sub.subChangedBefore(lockMomentFor("2026-10-07")) || sub.subChangedBefore(lockMomentFor("2026-10-06")) {
		t.Fatalf("subChangedBefore")
	}
	none := func(string) *order { return nil }
	if nd := subscriptionNextDelivery(sub, after, none); nd != "2026-10-08" {
		t.Fatalf("a plan seen after noon next delivers the day after tomorrow, got %s", nd)
	}
	if nd := subscriptionNextDelivery(sub, before, none); nd != "2026-10-07" {
		t.Fatalf("a plan seen before noon next delivers tomorrow, got %s", nd)
	}
	sub.OrderedDays = []string{"2026-10-07"}
	lockedTomorrow := func(day string) *order {
		if day == "2026-10-07" {
			return &order{ScheduledFor: day, SubLockedAt: "2026-10-06T06:30:00Z"}
		}
		return nil
	}
	if nd := subscriptionNextDelivery(sub, after, lockedTomorrow); nd != "2026-10-07" {
		t.Fatalf("a locked tomorrow still counts, got %s", nd)
	}
	if nd := subscriptionNextDelivery(sub, after, none); nd != "2026-10-08" {
		t.Fatalf("a claimed tomorrow with no live order (cancelled) does not count, got %s", nd)
	}
	legacy := &subscription{Status: "active", Frequency: "daily", StartDate: "2026-10-01"}
	if !legacy.subChangedBefore(lockMomentFor("2026-10-07")) {
		t.Fatalf("a row without stamps is served by the catch-up")
	}
}

func liveSubOrder(t *testing.T, w *chainWorld, subID, day string) *order {
	t.Helper()
	o, err := w.svc.repo.findLiveSubscriptionOrder(context.Background(), subID, day)
	if err != nil {
		t.Fatalf("findLiveSubscriptionOrder: %v", err)
	}
	return o
}

// One plan through a whole day around the cut-off: preview before noon,
// edits reconcile it, the noon lock creates the task, a pause after noon
// spares tomorrow and takes the day after, a resume after noon comes back
// from the day after tomorrow, and the next morning's catch-up adds nothing.
func TestNoonLockLifecycle(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000010001", 5000)
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{
		ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D,
	})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	chainBackdateSubscription(t, w, sub, istDayAt(D, 8, 0))

	// 09:00 on D: tomorrow is previewed (no task), the day after is not yet.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	prev := liveSubOrder(t, w, sub.SubscriptionID, D1)
	if prev == nil || prev.SubLockedAt != "" || prev.DeliveryDate != D1 {
		t.Fatalf("tomorrow's preview at 09:00: %+v", prev)
	}
	if d, _ := w.svc.repo.findDeliveryByOrder(ctx, prev.OrderID); d != nil {
		t.Fatalf("a preview must have no delivery task")
	}
	if liveSubOrder(t, w, sub.SubscriptionID, D2) != nil {
		t.Fatalf("the day after tomorrow is not previewed before noon")
	}
	// Nothing for D itself: the plan was created after D's own cut-off.
	if liveSubOrder(t, w, sub.SubscriptionID, D) != nil {
		t.Fatalf("a plan created after the cut-off must not deliver the same day")
	}

	// 10:00: a qty edit reconciles the preview; the store sees it as upcoming.
	qty := 3
	if _, err := w.svc.patchSubscription(ctx, cid, sub.SubscriptionID, struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}{Qty: &qty}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	chainStampChange(t, w, sub.SubscriptionID, istDayAt(D, 10, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 10, 0))
	prev = liveSubOrder(t, w, sub.SubscriptionID, D1)
	if prev.Items[0].Qty != 3 || prev.Total != 87 || prev.SubLockedAt != "" {
		t.Fatalf("preview after the edit: %+v total %v", prev.Items, prev.Total)
	}
	rows, err := w.svc.storeUpcomingAt(ctx, w.mgr, w.storeID.Hex(), istDayAt(D, 10, 0))
	if err != nil || len(rows) != 1 || rows[0].OrderID != prev.OrderID || rows[0].DeliveryDate != D1 {
		t.Fatalf("upcoming before noon: %v %+v", err, rows)
	}

	// 12:00: the lock. Tomorrow gets its task; the day after is previewed.
	if placed := w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 0)); placed != 1 {
		t.Fatalf("noon sweep locked %d orders, want 1", placed)
	}
	locked := liveSubOrder(t, w, sub.SubscriptionID, D1)
	if locked.SubLockedAt == "" || locked.Items[0].Qty != 3 {
		t.Fatalf("tomorrow after noon: %+v", locked)
	}
	if d, _ := w.svc.repo.findDeliveryByOrder(ctx, locked.OrderID); d == nil {
		t.Fatalf("the noon lock must create the store task")
	}
	next := liveSubOrder(t, w, sub.SubscriptionID, D2)
	if next == nil || next.SubLockedAt != "" {
		t.Fatalf("the day after tomorrow is previewed from noon: %+v", next)
	}
	rows, _ = w.svc.storeUpcomingAt(ctx, w.mgr, w.storeID.Hex(), istDayAt(D, 12, 30))
	if len(rows) != 1 || rows[0].OrderID != next.OrderID || rows[0].DeliveryDate != D2 {
		t.Fatalf("upcoming after noon shows the next editable day only: %+v", rows)
	}

	// 13:00: a qty edit after noon leaves tomorrow alone, moves the day after.
	qty = 1
	if _, err := w.svc.patchSubscription(ctx, cid, sub.SubscriptionID, struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}{Qty: &qty}); err != nil {
		t.Fatalf("patch 2: %v", err)
	}
	chainStampChange(t, w, sub.SubscriptionID, istDayAt(D, 13, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 13, 0))
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o.Items[0].Qty != 3 {
		t.Fatalf("an edit after noon changed tomorrow's locked order: %+v", o.Items)
	}
	if o := liveSubOrder(t, w, sub.SubscriptionID, D2); o.Items[0].Qty != 1 {
		t.Fatalf("an edit after noon must reach the day after tomorrow: %+v", o.Items)
	}

	// 14:00: pause after noon. Tomorrow still delivers; the day after is
	// cancelled and its day released.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	chainStampChange(t, w, sub.SubscriptionID, istDayAt(D, 14, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 14, 0))
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o == nil || o.Status != "placed" {
		t.Fatalf("a pause after noon must not touch tomorrow: %+v", o)
	}
	if liveSubOrder(t, w, sub.SubscriptionID, D2) != nil {
		t.Fatalf("a pause after noon cancels the day after tomorrow")
	}
	// 15:00: resume after noon. Nothing new for tomorrow (already live);
	// the day after tomorrow comes back, once.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	chainStampChange(t, w, sub.SubscriptionID, istDayAt(D, 15, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 15, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 15, 15))
	for _, day := range []string{D1, D2} {
		n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
			{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: day},
			{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
		})
		if n != 1 {
			t.Fatalf("live orders for %s after pause+resume: %d want 1", day, n)
		}
	}

	// A pause BEFORE noon does take tomorrow: on D1 at 09:00 pause, D2's
	// preview goes and its claim is released; resume at 10:00 brings it back;
	// the D1 morning's own order is untouched throughout.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause 2: %v", err)
	}
	chainStampChange(t, w, sub.SubscriptionID, istDayAt(D1, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 9, 0))
	if liveSubOrder(t, w, sub.SubscriptionID, D2) != nil {
		t.Fatalf("a pause before noon must cancel tomorrow's preview")
	}
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "resume"); err != nil {
		t.Fatalf("resume 2: %v", err)
	}
	chainStampChange(t, w, sub.SubscriptionID, istDayAt(D1, 10, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 10, 0))
	if o := liveSubOrder(t, w, sub.SubscriptionID, D2); o == nil || o.SubLockedAt != "" {
		t.Fatalf("a resume before noon re-previews tomorrow: %+v", o)
	}
	// 00:15 on D1: the catch-up adds nothing (D1 is claimed and live).
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 0, 15))
	n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
		{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: D1},
	})
	if n != 1 {
		t.Fatalf("orders for %s after the midnight tick: %d want 1", D1, n)
	}
	// The wire shape says when the plan next delivers.
	if list, _ := w.svc.listSubscriptionsFor(ctx, cid); len(list) != 1 {
		t.Fatalf("list: %d", len(list))
	} else if nd := w.svc.nextDeliveryFor(ctx, &list[0], istDayAt(D1, 13, 0)); nd != D2 {
		t.Fatalf("next_delivery_date at 13:00 on D1: %s want %s", nd, D2)
	}
}

// A plan created after noon for tomorrow does not reach tomorrow: nothing
// is created for it until the first editable day, even by the next morning's
// catch-up; a plan that predates the cut-off is caught up (locked at once)
// when its preview never ran.
func TestNoonLockNewPlanAfterNoonStartsDayAfterTomorrow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)

	late := w.customer(t, "9000010101", 5000)
	sub, err := w.svc.createSubscription(ctx, late, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	chainBackdateSubscription(t, w, sub, istDayAt(D, 14, 0))
	if placed := w.svc.sweepOneSubscription(ctx, sub, istDayAt(D, 14, 5)); placed != 0 {
		t.Fatalf("a plan created after noon was locked for tomorrow")
	}
	if liveSubOrder(t, w, sub.SubscriptionID, D1) != nil {
		t.Fatalf("a plan created after noon must not deliver tomorrow")
	}
	if o := liveSubOrder(t, w, sub.SubscriptionID, D2); o == nil || o.SubLockedAt != "" {
		t.Fatalf("its first day is the day after tomorrow, as a preview: %+v", o)
	}
	if nd := w.svc.nextDeliveryFor(ctx, sub, istDayAt(D, 14, 5)); nd != D2 {
		t.Fatalf("next_delivery_date: %s want %s", nd, D2)
	}
	// The next morning's catch-up still leaves D1 alone.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 0, 15))
	if liveSubOrder(t, w, sub.SubscriptionID, D1) != nil {
		t.Fatalf("the catch-up served a plan created after the cut-off")
	}

	// A plan from long before, whose preview never ran (server down over
	// the cut-off), is caught up as a locked order with its task.
	early := w.customer(t, "9000010102", 5000)
	sub2, err := w.svc.createSubscription(ctx, early, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	chainBackdateSubscription(t, w, sub2, istDayAt(D, 8, 0))
	if placed := w.svc.sweepOneSubscription(ctx, sub2, istDayAt(D, 14, 5)); placed != 1 {
		t.Fatalf("catch-up placed %d, want 1 (tomorrow, locked)", placed)
	}
	o := liveSubOrder(t, w, sub2.SubscriptionID, D1)
	if o == nil || o.SubLockedAt == "" {
		t.Fatalf("catch-up order: %+v", o)
	}
	if d, _ := w.svc.repo.findDeliveryByOrder(ctx, o.OrderID); d == nil {
		t.Fatalf("the catch-up must create the task")
	}
	// Racing sweeps for the same plan still yield one order per day.
	for i := 0; i < 4; i++ {
		w.svc.sweepOneSubscription(ctx, sub2, istDayAt(D, 14, 10))
	}
	for _, day := range []string{D1, D2} {
		n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
			{Key: "subscription_id", Value: sub2.SubscriptionID}, {Key: "scheduled_for", Value: day}})
		if n != 1 {
			t.Fatalf("orders for %s: %d want 1", day, n)
		}
	}
}

// createSubscription refuses a second live plan on the same product line
// with DUPLICATE_SUBSCRIPTION naming the plan; a cancelled one frees it, a
// different variant is its own line.
func TestDuplicateSubscriptionRefused(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000010201", 500)
	first, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 1, Frequency: "daily"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err = w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: " 500ML ", Qty: 2, Frequency: "weekly"})
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != 409 || ae.Code != "DUPLICATE_SUBSCRIPTION" {
		t.Fatalf("duplicate: %v", err)
	}
	if !strings.Contains(ae.Message, "daily plan for Milk gold-500ml 500ml") {
		t.Fatalf("the message must name the plan: %q", ae.Message)
	}
	// Paused still counts as live.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, first.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err = w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 1, Frequency: "daily"}); !errors.As(err, &ae) || ae.Code != "DUPLICATE_SUBSCRIPTION" {
		t.Fatalf("paused duplicate: %v", err)
	}
	// Another variant of the same product is its own line; another customer too.
	if _, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "1L", Qty: 1, Frequency: "daily"}); err != nil {
		t.Fatalf("other variant: %v", err)
	}
	other := w.customer(t, "9000010202", 500)
	if _, err := w.svc.createSubscription(ctx, other, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 1, Frequency: "daily"}); err != nil {
		t.Fatalf("other customer: %v", err)
	}
	// Cancelled frees the line.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, first.SubscriptionID, "cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 1, Frequency: "daily"}); err != nil {
		t.Fatalf("after cancel: %v", err)
	}
}

// A locked morning order whose delivery day passed with no delivery is
// closed by the sweep: cancelled (the terminal status the app draws), the
// task failed with reason missed, order.failed emitted with reason missed,
// no money moved; a delivered one and a still-current one are untouched.
func TestMissedLockedOrdersAreClosedBySweep(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000010301", 5000)
	addrs, _ := w.svc.repo.listAddresses(ctx, cid)
	const D = "2026-10-06"
	Dm1, Dm2 := addDaysIST(D, -1), addDaysIST(D, -2)
	mk := func(id, day string) *subscription {
		return &subscription{SubscriptionID: id, ConsumerID: cid, ProductID: "taaza-500ml", Name: "Milk taaza-500ml",
			Variant: "500ml", Qty: 1, UnitPrice: 29, Frequency: "daily", Status: "active", StartDate: day}
	}
	// (a) two days old, task never completed; (b) yesterday, no task at all;
	// (c) yesterday, delivered; (d) today's, locked, still current.
	a, err := w.svc.insertSubscriptionOrder(ctx, mk("sub_missed_a", Dm2), &addrs[0], Dm2, true, istDayAt(Dm2, 0, 0))
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := w.svc.insertSubscriptionOrder(ctx, mk("sub_missed_b", Dm1), &addrs[0], Dm1, true, istDayAt(Dm1, 0, 0))
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if _, err := w.db.Collection(collDeliveries).DeleteOne(ctx, bson.D{{Key: "order_id", Value: b.OrderID}}); err != nil {
		t.Fatalf("drop b task: %v", err)
	}
	c, err := w.svc.insertSubscriptionOrder(ctx, mk("sub_missed_c", Dm1), &addrs[0], Dm1, true, istDayAt(Dm1, 0, 0))
	if err != nil {
		t.Fatalf("c: %v", err)
	}
	ctask := chainTaskFor(t, w, c.OrderID)
	chainOutForDelivery(t, w, ctask.ID)
	if _, err := w.svc.deliverDelivery(ctx, w.rider, ctask.ID, deliverInput{ProofPhoto: "p.jpg", Geo: &geoPt{Lat: ctask.Geo.Lat, Lng: ctask.Geo.Lng}, GeofenceOK: true}); err != nil {
		t.Fatalf("deliver c: %v", err)
	}
	d, err := w.svc.insertSubscriptionOrder(ctx, mk("sub_missed_d", D), &addrs[0], D, true, istDayAt(Dm1, 12, 0))
	if err != nil {
		t.Fatalf("d: %v", err)
	}
	cashBefore, _ := w.svc.wallet(ctx, cid)

	// Before noon on D: only orders older than yesterday close.
	if n := w.svc.closeMissedSubscriptionOrders(ctx, istDayAt(D, 9, 0)); n != 1 {
		t.Fatalf("closed before noon: %d want 1", n)
	}
	if o := w.orderByID(t, a.OrderID); o.Status != "cancelled" || o.CancelledBy != orderCancelledByMissed {
		t.Fatalf("a after the sweep: %s by %q", o.Status, o.CancelledBy)
	}
	if o := w.orderByID(t, b.OrderID); o.Status != "placed" {
		t.Fatalf("yesterday's order closed before noon: %s", o.Status)
	}
	// From noon, yesterday's close too; delivered and current stay.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 30))
	if o := w.orderByID(t, b.OrderID); o.Status != "cancelled" || o.CancelledBy != orderCancelledByMissed {
		t.Fatalf("b after the noon sweep: %s by %q", o.Status, o.CancelledBy)
	}
	if o := w.orderByID(t, c.OrderID); o.Status != "delivered" {
		t.Fatalf("a delivered order was touched: %s", o.Status)
	}
	if o := w.orderByID(t, d.OrderID); o.Status != "placed" {
		t.Fatalf("today's order was touched: %s", o.Status)
	}
	if task := chainTaskFor(t, w, a.OrderID); task.Status != "FAILED" || task.FailureReason != "missed" {
		t.Fatalf("task a: %s %q", task.Status, task.FailureReason)
	}
	for _, id := range []string{a.OrderID, b.OrderID} {
		evs := failedEventsFor(t, w, id)
		if len(evs) != 1 {
			t.Fatalf("order.failed for %s: %d want 1", id, len(evs))
		}
		if p, _ := evs[0]["payload"].(bson.M); p["reason"] != "missed" {
			t.Fatalf("reason: %v", p)
		}
	}
	// Idempotent, and no money moved for the missed ones.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 13, 0))
	if evs := failedEventsFor(t, w, a.OrderID); len(evs) != 1 {
		t.Fatalf("re-emitted: %d", len(evs))
	}
	cashAfter, _ := w.svc.wallet(ctx, cid)
	if cashAfter.Available != cashBefore.Available {
		t.Fatalf("closing missed orders moved money: %v -> %v", cashBefore.Available, cashAfter.Available)
	}
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "ref_id", Value: bson.D{{Key: "$in", Value: bson.A{"delivery:" + a.OrderID, "delivery:" + b.OrderID}}}}}); n != 0 {
		t.Fatalf("ledger rows for missed orders: %d", n)
	}
	// The closed day no longer blocks the day index, and the app reads the
	// order as terminal (cancelled).
	if o := liveSubOrder(t, w, "sub_missed_a", Dm2); o != nil {
		t.Fatalf("a missed order still reads as live: %+v", o)
	}
}

// The LOCK step keeps the cut-off even when the tick runs late: the sweep
// ticks every 15 minutes, so the first tick after noon can run at 12:15. A
// pause or a qty edit made at 12:05 is after tomorrow's lock moment and
// must not reach tomorrow (the preview locks as it stands), while a pause
// made at 11:55 still cancels tomorrow on that same 12:15 tick.
func TestNoonLockStepIgnoresEditsAfterTheCutOff(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	type patch = struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}
	plan := func(phone string) (*subscription, primitive.ObjectID) {
		cid := w.customer(t, phone, 5000)
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D})
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		chainBackdateSubscription(t, w, sub, istDayAt(D, 8, 0))
		return sub, cid
	}
	paused, pausedCID := plan("9000010101")
	edited, editedCID := plan("9000010102")
	early, earlyCID := plan("9000010103")

	// 09:00: tomorrow is previewed for all three.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	previews := map[string]*order{}
	for _, s := range []*subscription{paused, edited, early} {
		if previews[s.SubscriptionID] = liveSubOrder(t, w, s.SubscriptionID, D1); previews[s.SubscriptionID] == nil {
			t.Fatalf("no preview for %s", s.SubscriptionID)
		}
	}

	// 11:55: one member pauses before the cut-off.
	if _, err := w.svc.setSubscriptionStatus(ctx, earlyCID, early.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause 11:55: %v", err)
	}
	chainStampChange(t, w, early.SubscriptionID, istDayAt(D, 11, 55))
	// 12:05: one pauses and one doubles the qty, after the cut-off, and no
	// tick has run since 09:00.
	if _, err := w.svc.setSubscriptionStatus(ctx, pausedCID, paused.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause 12:05: %v", err)
	}
	chainStampChange(t, w, paused.SubscriptionID, istDayAt(D, 12, 5))
	two := 2
	if _, err := w.svc.patchSubscription(ctx, editedCID, edited.SubscriptionID, patch{Qty: &two}); err != nil {
		t.Fatalf("patch 12:05: %v", err)
	}
	chainStampChange(t, w, edited.SubscriptionID, istDayAt(D, 12, 5))

	// 12:15: the first tick after noon.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 15))
	for _, s := range []*subscription{paused, edited} {
		o := liveSubOrder(t, w, s.SubscriptionID, D1)
		if o == nil || o.OrderID != previews[s.SubscriptionID].OrderID || o.SubLockedAt == "" {
			t.Fatalf("a change at 12:05 reached tomorrow for %s: %+v", s.SubscriptionID, o)
		}
		if o.Items[0].Qty != 1 || o.Total != previews[s.SubscriptionID].Total {
			t.Fatalf("tomorrow must deliver as previewed for %s: %+v total %v", s.SubscriptionID, o.Items, o.Total)
		}
		if d, _ := w.svc.repo.findDeliveryByOrder(ctx, o.OrderID); d == nil {
			t.Fatalf("the locked order has no store task for %s", s.SubscriptionID)
		}
	}
	if o := liveSubOrder(t, w, early.SubscriptionID, D1); o != nil {
		t.Fatalf("a pause at 11:55 must still cancel tomorrow: %+v", o)
	}
	// The 12:05 changes do reach the day after tomorrow.
	if o := liveSubOrder(t, w, paused.SubscriptionID, addDaysIST(D, 2)); o != nil {
		t.Fatalf("the 12:05 pause must take the day after tomorrow: %+v", o)
	}
	if o := liveSubOrder(t, w, edited.SubscriptionID, addDaysIST(D, 2)); o == nil || o.Items[0].Qty != 2 {
		t.Fatalf("the 12:05 edit must reach the day after tomorrow: %+v", o)
	}
}

// A change made before the cut-off still reaches tomorrow when a second
// change lands after noon but before the first tick: changed_at holds only
// the LAST change, so the tick at 12:10 saw a plan changed at 12:05 and
// locked tomorrow's preview as it was previewed, dropping the 11:50 pause or
// qty edit. A post-noon change now first locks tomorrow against the plan as
// it stood; the change itself still applies from the day after tomorrow.
func TestNoonLockKeepsAPreNoonChangeUnderALaterOne(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	plan := func(phone string) (*subscription, primitive.ObjectID) {
		cid := w.customer(t, phone, 5000)
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D})
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		chainBackdateSubscription(t, w, sub, istDayAt(D, 8, 0))
		return sub, cid
	}
	paused, pausedCID := plan("9000010111")
	edited, editedCID := plan("9000010112")
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))

	// (a) pause at 11:50, resume at 12:05, first tick at 12:10.
	if _, err := w.svc.setSubscriptionStatusAt(ctx, pausedCID, paused.SubscriptionID, "pause", istDayAt(D, 11, 50)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := w.svc.setSubscriptionStatusAt(ctx, pausedCID, paused.SubscriptionID, "resume", istDayAt(D, 12, 5)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// (b) qty 1 -> 3 at 11:50, a slot edit at 12:05.
	three, slot := 3, "early"
	if _, err := w.svc.patchSubscriptionAt(ctx, editedCID, edited.SubscriptionID, subscriptionPatch{Qty: &three}, istDayAt(D, 11, 50)); err != nil {
		t.Fatalf("qty: %v", err)
	}
	if _, err := w.svc.patchSubscriptionAt(ctx, editedCID, edited.SubscriptionID, subscriptionPatch{DeliverySlot: &slot}, istDayAt(D, 12, 5)); err != nil {
		t.Fatalf("slot: %v", err)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 10))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 25))

	if o := liveSubOrder(t, w, paused.SubscriptionID, D1); o != nil {
		t.Fatalf("(a) the 11:50 pause must still take tomorrow: %+v", o)
	}
	if o := liveSubOrder(t, w, paused.SubscriptionID, D2); o == nil {
		t.Fatalf("(a) the 12:05 resume brings the plan back from the day after tomorrow")
	}
	o := liveSubOrder(t, w, edited.SubscriptionID, D1)
	if o == nil || o.SubLockedAt == "" || o.Items[0].Qty != 3 || o.Total != 87 {
		t.Fatalf("(b) tomorrow must lock with the 11:50 qty edit: %+v", o)
	}
	if d, _ := w.svc.repo.findDeliveryByOrder(ctx, o.OrderID); d == nil {
		t.Fatalf("(b) tomorrow's locked order has no store task")
	}
	if o2 := liveSubOrder(t, w, edited.SubscriptionID, D2); o2 == nil || o2.Items[0].Qty != 3 {
		t.Fatalf("(b) the day after tomorrow carries the plan as edited: %+v", o2)
	}
}

// next_delivery_date is what the app's picker and strip read (handoff 9.3),
// so it must name the day the rider will really come: never a day whose
// order is cancelled (the LOCK step cancelled it for a pause before noon, or
// the member cancelled the preview), and a locked tomorrow still counts
// after a pause or cancel made after noon, because that milk is delivered
// and billed.
func TestNextDeliveryDateFollowsTheLiveOrders(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	plan := func(phone string) (*subscription, primitive.ObjectID) {
		cid := w.customer(t, phone, 5000)
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D})
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		chainBackdateSubscription(t, w, sub, istDayAt(D, 8, 0))
		return sub, cid
	}
	next := func(sub *subscription, at time.Time) string {
		t.Helper()
		fresh, err := w.svc.repo.findSubscriptionByID(ctx, sub.SubscriptionID)
		if err != nil || fresh == nil {
			t.Fatalf("reload: %v", err)
		}
		return w.svc.nextDeliveryFor(ctx, fresh, at)
	}
	resumed, resumedCID := plan("9000010121")
	skipped, skippedCID := plan("9000010122")
	late, lateCID := plan("9000010123")
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	if nd := next(late, istDayAt(D, 9, 30)); nd != D1 {
		t.Fatalf("before noon an active plan next delivers tomorrow: %s", nd)
	}

	// (a) pause 11:55; the 12:10 tick cancels tomorrow; resume 12:30.
	if _, err := w.svc.setSubscriptionStatusAt(ctx, resumedCID, resumed.SubscriptionID, "pause", istDayAt(D, 11, 55)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// (b) the member cancels tomorrow's preview directly at 10:00.
	prev := liveSubOrder(t, w, skipped.SubscriptionID, D1)
	if _, err := w.svc.cancelOrder(ctx, skippedCID.Hex(), prev.OrderID); err != nil {
		t.Fatalf("cancel preview: %v", err)
	}
	if nd := next(skipped, istDayAt(D, 10, 20)); nd != D2 {
		t.Fatalf("(b) a cancelled tomorrow is skipped: next_delivery_date %s want %s", nd, D2)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 10))
	if _, err := w.svc.setSubscriptionStatusAt(ctx, resumedCID, resumed.SubscriptionID, "resume", istDayAt(D, 12, 30)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 45))
	if liveSubOrder(t, w, resumed.SubscriptionID, D1) != nil {
		t.Fatalf("(a) the 11:55 pause must take tomorrow")
	}
	if nd := next(resumed, istDayAt(D, 12, 50)); nd != D2 {
		t.Fatalf("(a) tomorrow's order is cancelled: next_delivery_date %s want %s", nd, D2)
	}
	if nd := next(skipped, istDayAt(D, 12, 50)); nd != D2 {
		t.Fatalf("(b) after noon: next_delivery_date %s want %s", nd, D2)
	}

	// (c) tomorrow locked at noon; the plan is paused, then cancelled, at 13:00.
	if o := liveSubOrder(t, w, late.SubscriptionID, D1); o == nil || o.SubLockedAt == "" {
		t.Fatalf("(c) tomorrow should be locked: %+v", o)
	}
	if nd := next(late, istDayAt(D, 12, 50)); nd != D1 {
		t.Fatalf("(c) a locked tomorrow is the next delivery: %s", nd)
	}
	for _, action := range []string{"pause", "cancel"} {
		got, err := w.svc.setSubscriptionStatusAt(ctx, lateCID, late.SubscriptionID, action, istDayAt(D, 13, 0))
		if err != nil {
			t.Fatalf("(c) %s: %v", action, err)
		}
		w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 13, 15))
		if liveSubOrder(t, w, late.SubscriptionID, D1) == nil {
			t.Fatalf("(c) a %s after noon must leave tomorrow's locked order", action)
		}
		if nd := w.svc.nextDeliveryFor(ctx, got, istDayAt(D, 13, 0)); nd != D1 {
			t.Fatalf("(c) %s at 13:00: next_delivery_date %q want %s (the locked milk still comes)", action, nd, D1)
		}
	}
}

// The MISSED step closes yesterday's orders from noon, but not one a rider
// is still out with: it failed the task under the rider, whose late
// "delivered" was then refused. An in-flight order gets until the day after
// to be marked; an order whose task never left the store closes at noon as
// before, and an in-flight one older than yesterday closes on any tick.
func TestMissedSweepSparesAnOrderTheRiderIsCarrying(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000010401", 5000)
	addrs, _ := w.svc.repo.listAddresses(ctx, cid)
	const D = "2026-10-06"
	Dm1, Dm2 := addDaysIST(D, -1), addDaysIST(D, -2)
	mk := func(id, day string) *order {
		s := &subscription{SubscriptionID: id, ConsumerID: cid, ProductID: "taaza-500ml", Name: "Milk taaza-500ml",
			Variant: "500ml", Qty: 1, UnitPrice: 29, Frequency: "daily", Status: "active", StartDate: day}
		o, err := w.svc.insertSubscriptionOrder(ctx, s, &addrs[0], day, true, istDayAt(addDaysIST(day, -1), 12, 0))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return o
	}
	carried := mk("sub_carried", Dm1)
	chainOutForDelivery(t, w, chainTaskFor(t, w, carried.OrderID).ID)
	atStore := mk("sub_at_store", Dm1)
	old := mk("sub_old_carried", Dm2)
	chainOutForDelivery(t, w, chainTaskFor(t, w, old.OrderID).ID)

	if n := w.svc.closeMissedSubscriptionOrders(ctx, istDayAt(D, 12, 30)); n != 2 {
		t.Fatalf("closed at 12:30: %d want 2 (the order still at the store and the two-day-old one)", n)
	}
	if o := w.orderByID(t, atStore.OrderID); o.Status != "cancelled" || o.CancelledBy != orderCancelledByMissed {
		t.Fatalf("an order that never left the store closes at noon: %s by %q", o.Status, o.CancelledBy)
	}
	if o := w.orderByID(t, old.OrderID); o.Status != "cancelled" {
		t.Fatalf("an in-flight order two days old closes: %s", o.Status)
	}
	if o := w.orderByID(t, carried.OrderID); o.Status != "out_for_delivery" {
		t.Fatalf("yesterday's order the rider is carrying was closed: %s", o.Status)
	}
	task := chainTaskFor(t, w, carried.OrderID)
	if task.Status != "OUT_FOR_DELIVERY" {
		t.Fatalf("the rider's task was failed under them: %s %q", task.Status, task.FailureReason)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true}); err != nil {
		t.Fatalf("the rider's late delivered mark must go through: %v", err)
	}
	if o := w.orderByID(t, carried.OrderID); o.Status != "delivered" {
		t.Fatalf("after the late mark: %s", o.Status)
	}
}

// A plan stored with no variant (the Welcome Litre campaign plan is minted
// that way) covers every variant of its product: a second plan on the same
// SKU with a variant used to pass the DUPLICATE_SUBSCRIPTION guard, and the
// worker then minted two morning orders a day. The reverse holds too.
func TestDuplicateSubscriptionMatchesAPlanWithoutAVariant(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	for i, c := range []struct{ stored, requested string }{{"", "500ml"}, {"500ml", ""}} {
		cid := w.customer(t, "900001050"+string(rune('1'+i)), 500)
		if _, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: c.stored, Qty: 1, Frequency: "daily"}); err != nil {
			t.Fatalf("first plan (%q): %v", c.stored, err)
		}
		_, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: c.requested, Qty: 2, Frequency: "daily"})
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != "DUPLICATE_SUBSCRIPTION" {
			t.Fatalf("stored %q, requested %q: want DUPLICATE_SUBSCRIPTION, got %v", c.stored, c.requested, err)
		}
	}
}

package consumer

// R4(iii)/(iv), 24 Sep: when the store manager and the rider see a morning
// order, and what "today" means to each.
//
//   - The store's rider list counts a rider's deliveries "completed today" on
//     the IST day: the morning route runs 05:00-07:30 IST, and a delivery
//     before 05:30 IST is still yesterday in UTC, so the old UTC compare
//     dropped the round's first stops from today's count.
//   - After the noon lock the store sees tomorrow's locked orders as tasks
//     for tomorrow, with the pack size, and the day after's previews in
//     Upcoming.
//   - The rider's today (route header, complete guard, pickup sheet) is
//     today's stops plus overdue open ones, never a later day's.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run 'StoreRiders|StoreSeesTomorrow|RiderToday' -v

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func seedRiderTask(t *testing.T, w *chainWorld, id, status, deliveryDate string, assignedAt, deliveredAt time.Time, product string) {
	t.Helper()
	d := delivery{
		MongoID: primitive.NewObjectID(), ID: id, OrderID: "ord_" + id,
		StoreID: w.storeID.Hex(), RiderPartyID: w.riderID.Hex(), ConsumerID: primitive.NewObjectID().Hex(),
		Status: status, DeliveryDate: deliveryDate, AssignedAt: rfc3339(assignedAt), Lane: "morning",
		Items: []deliveryItem{{ProductID: product, Name: "Milk " + product, Variant: "500ml", Qty: 1}},
		// The consoles carry finished work touched in the last few days (by
		// the wall clock), whatever day the test simulates.
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if !deliveredAt.IsZero() {
		d.DeliveredAt = deliveredAt.UTC().Format(time.RFC3339)
	}
	if _, err := w.db.Collection(collDeliveries).InsertOne(context.Background(), d); err != nil {
		t.Fatalf("seed task %s: %v", id, err)
	}
}

func TestStoreRidersCompletedTodayIsIST(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	// 05:10 and 07:20 IST today (05:10 IST is still the 5th in UTC), and
	// 23:50 IST yesterday, which is not today's work.
	seedRiderTask(t, w, "dlv_early", "DELIVERED", D, istDayAt(D, 4, 30), istDayAt(D, 5, 10), "gold-500ml")
	seedRiderTask(t, w, "dlv_late", "DELIVERED", D, istDayAt(D, 4, 30), istDayAt(D, 7, 20), "gold-500ml")
	seedRiderTask(t, w, "dlv_yesterday", "DELIVERED", addDaysIST(D, -1), istDayAt(addDaysIST(D, -1), 4, 30), istDayAt(addDaysIST(D, -1), 23, 50), "gold-500ml")
	for _, at := range []time.Time{istDayAt(D, 9, 0), istDayAt(D, 0, 30), istDayAt(D, 23, 59)} {
		rs, err := w.svc.storeRidersAt(ctx, w.mgr, w.storeID.Hex(), "", at)
		if err != nil {
			t.Fatalf("storeRiders: %v", err)
		}
		var got *riderSummary
		for i := range rs {
			if rs[i].PartyID == w.riderID.Hex() {
				got = &rs[i]
			}
		}
		if got == nil || got.CompletedToday != 2 {
			t.Fatalf("at %s IST the rider's completed_today = %+v, want 2 (05:10 and 07:20 today)", at.In(istZone).Format("15:04"), got)
		}
	}
}

// The rider's today is every stop due today, whatever its state and
// whenever it was assigned, plus overdue open ones; a later day's stop, even
// assigned today, is not in the route header, the complete guard or the
// pickup sheet. Under the noon lock today's stops are routinely assigned the
// afternoon before, so one already delivered or failed this morning must
// still count (nr-4, 24 Sep: the header read "1 stop, 0 delivered").
func TestRiderTodayKeepsOverdueStopsAndLeavesLaterDays(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	rider := w.riderID.Hex()
	const D = "2026-10-06"
	D1, Dm1 := addDaysIST(D, 1), addDaysIST(D, -1)
	var zero time.Time
	seedRiderTask(t, w, "dlv_overdue", "ASSIGNED", Dm1, istDayAt(addDaysIST(D, -2), 15, 0), zero, "taaza-500ml")
	seedRiderTask(t, w, "dlv_today", "ASSIGNED", D, istDayAt(Dm1, 15, 0), zero, "gold-500ml")
	seedRiderTask(t, w, "dlv_tomorrow", "ASSIGNED", D1, istDayAt(D, 14, 0), zero, "gold-1l")
	// Today's stops assigned yesterday afternoon and already finished.
	seedRiderTask(t, w, "dlv_today_done", "DELIVERED", D, istDayAt(Dm1, 15, 0), istDayAt(D, 6, 0), "chai-500ml")
	seedRiderTask(t, w, "dlv_today_failed", "FAILED", D, istDayAt(Dm1, 15, 0), zero, "shakti-500ml")

	tasks, err := w.svc.repo.riderRouteTasksForDay(ctx, rider, D)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	if !ids["dlv_overdue"] || !ids["dlv_today"] || !ids["dlv_today_done"] || !ids["dlv_today_failed"] || ids["dlv_tomorrow"] {
		t.Fatalf("today's route %v: want the overdue stop and all of today's, not tomorrow's", ids)
	}
	if sum := riderSummariseRoute(tasks); sum.Total != 4 || sum.Pending != 2 || sum.Delivered != 1 || sum.NotDelivered != 1 {
		t.Fatalf("today's header %+v, want total 4, pending 2, delivered 1, not delivered 1", sum)
	}
	// The complete guard refuses while the overdue stop is open.
	if err := w.svc.repo.riderCompleteRouteDay(ctx, rider, D); err == nil {
		t.Fatalf("a day with an overdue open stop was closed")
	}
	lines, err := w.svc.repo.riderInventoryDemandForDay(ctx, rider, D)
	if err != nil {
		t.Fatalf("pickup sheet: %v", err)
	}
	names := []string{}
	for _, l := range lines {
		names = append(names, l.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "taaza-500ml") || !strings.Contains(joined, "gold-500ml") || strings.Contains(joined, "gold-1l") {
		t.Fatalf("today's pickup sheet %q: want the overdue and today's crates, not tomorrow's", joined)
	}
	// The crates of today's finished stops were carried too: the sheet does
	// not shrink as the morning's stops are delivered.
	if !strings.Contains(joined, "chai-500ml") || !strings.Contains(joined, "shakti-500ml") {
		t.Fatalf("today's pickup sheet %q: want the crates of today's delivered and failed stops", joined)
	}
	// The session the sheet opens carries the same lines.
	sess, err := w.svc.repo.riderInventorySessionForDay(ctx, rider, D)
	if err != nil {
		t.Fatalf("pickup session: %v", err)
	}
	for _, l := range sess.Lines {
		if l.Name == "Milk gold-1l" {
			t.Fatalf("today's pickup session carries tomorrow's crate: %+v", l)
		}
	}
}

// Verification (no behaviour change): at the noon lock the store's task
// queue gains tomorrow's funded orders as tomorrow's tasks, with the pack
// size and the dated slot, and Upcoming moves on to the day after.
func TestStoreSeesTomorrowsLockedOrdersAfterTheLock(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	cid := w.customer(t, "9000015101", 5000)
	sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: D1}, istDayAt(D, 9, 0))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w.svc.sweepOneSubscription(ctx, sub, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 0).Add(5*time.Second))
	locked := assertLocked(t, w, sub, D1)

	tasks, err := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeOrders: %v", err)
	}
	var task *delivery
	for i := range tasks {
		if tasks[i].OrderID == locked.OrderID {
			task = &tasks[i]
		}
	}
	if task == nil {
		t.Fatalf("tomorrow's locked order is not in the store's queue: %+v", tasks)
	}
	if task.DeliveryDate != D1 || !strings.Contains(task.Slot, "05:00 - 07:30 AM") || task.Status != "ASSIGNED" || task.RiderPartyID != "" {
		t.Fatalf("tomorrow's task: date %s slot %q status %s rider %q", task.DeliveryDate, task.Slot, task.Status, task.RiderPartyID)
	}
	if len(task.Items) != 1 || task.Items[0].Variant != "500ml" || task.Items[0].Qty != 2 {
		t.Fatalf("tomorrow's task lines: %+v, want 2 x 500ml", task.Items)
	}
	rows := upcomingFor(t, w, istDayAt(D, 12, 1))
	for _, r := range rows {
		if r.DeliveryDate != D2 {
			t.Fatalf("Upcoming after the lock lists %s, want only %s: %+v", r.DeliveryDate, D2, rows)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("Upcoming after the lock: %+v, want the day after tomorrow's preview", rows)
	}
}

package consumer

// R4(v), 24 Sep: the noon lock is idempotent under restarts and two
// instances. Every instance wakes at 12:00:05 and runs a boot tick; a
// restart at 12:14:59 runs one more. Racing sweeps over the same members
// must still leave one task per funded order, one skip (and one
// subscription.day_skipped) per unfunded order, the older of two plans on
// one wallet funded, one preview per plan for the day after, and one B-02
// per member however many CRM ticks run.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run ConcurrentNoonSweeps -v

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func TestConcurrentNoonSweeps(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	long := istDayAt(addDaysIST(D, -3), 9, 0)

	// Rs 70 a morning (2 x gold-500ml at Rs 35).
	plan := func(phone string, fund float64, madeAt time.Time) *subscription {
		t.Helper()
		cid := w.customer(t, phone, fund)
		sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: D1}, chainPlanMadeAt)
		if err != nil {
			t.Fatalf("create %s: %v", phone, err)
		}
		chainBackdateSubscription(t, w, sub, madeAt)
		return sub
	}
	funded := plan("9000015201", 100, long) // D1 locks; 30 left, short for D2 -> one B-02
	short := plan("9000015202", 0, long)    // D1 skipped
	older := plan("9000015203", 100, long)  // one wallet, two plans: the older one is funded
	newerSub, err := w.svc.createSubscriptionAt(ctx, older.ConsumerID, subscriptionInput{ProductID: "taaza-500ml", Qty: 2, Frequency: "daily", StartDate: D1}, chainPlanMadeAt)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	chainBackdateSubscription(t, w, newerSub, long.Add(time.Hour))

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))

	// Two instances wake at 12:00:05 (two goroutines each), and one restarts
	// at 12:14:59 and runs its boot tick while they are still going.
	var wg sync.WaitGroup
	for _, at := range []time.Time{
		istDayAt(D, 12, 0).Add(5 * time.Second), istDayAt(D, 12, 0).Add(5 * time.Second),
		istDayAt(D, 12, 0).Add(5 * time.Second), istDayAt(D, 12, 0).Add(5 * time.Second),
		istDayAt(D, 12, 14).Add(59 * time.Second),
	} {
		wg.Add(1)
		go func(at time.Time) {
			defer wg.Done()
			w.svc.sweepSubscriptionOrders(ctx, at)
		}(at)
	}
	wg.Wait()

	oneTask := func(label string, o *order) {
		t.Helper()
		n, _ := w.db.Collection(collDeliveries).CountDocuments(ctx, bson.D{{Key: "order_id", Value: o.OrderID}})
		if n != 1 {
			t.Fatalf("%s: %d store tasks for %s, want exactly 1", label, n, o.OrderID)
		}
	}
	for _, s := range []*subscription{funded, older} {
		rows := subOrdersFor(t, w, s.SubscriptionID, D1)
		if len(rows) != 1 {
			t.Fatalf("%s on %s: %d orders, want 1", s.SubscriptionID, D1, len(rows))
		}
		oneTask(s.SubscriptionID, assertLocked(t, w, s, D1))
	}
	assertSkipped(t, w, short, D1)
	assertSkipped(t, w, newerSub, D1)
	for _, s := range []*subscription{short, older} {
		if evs := daySkippedEvents(t, w, s.ConsumerID); len(evs) != 1 {
			t.Fatalf("%s: %d subscription.day_skipped events, want 1", s.ConsumerID.Hex(), len(evs))
		}
	}
	if evs := daySkippedEvents(t, w, funded.ConsumerID); len(evs) != 0 {
		t.Fatalf("the funded member got %d day_skipped events", len(evs))
	}
	for _, s := range []*subscription{funded, short, older, newerSub} {
		live := 0
		for _, o := range subOrdersFor(t, w, s.SubscriptionID, D2) {
			if o.Status != "cancelled" {
				live++
			}
		}
		if live != 1 {
			t.Fatalf("%s: %d live previews for %s, want 1", s.SubscriptionID, live, D2)
		}
	}

	// B-02 from two instances' CRM ticks at once, then a later one.
	var cw sync.WaitGroup
	for i := 0; i < 3; i++ {
		cw.Add(1)
		go func() {
			defer cw.Done()
			w.svc.crmProcessSchedules(ctx, istDayAt(D, 12, 20))
		}()
	}
	cw.Wait()
	w.svc.crmProcessSchedules(ctx, istDayAt(D, 13, 5))
	if got := crmDispatchStatuses(t, w.db, funded.ConsumerID, "B-02"); len(got) != 1 {
		t.Fatalf("B-02 for the member short for the day after tomorrow: %v, want exactly one", got)
	}
	if n := inboxCount(t, w.db, funded.ConsumerID, "B-02"); n != 1 {
		t.Fatalf("B-02 inbox rows: %d, want 1", n)
	}
}

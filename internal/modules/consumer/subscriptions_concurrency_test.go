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
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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
	// One wallet, two plans, and enough for both (Rs 70 + Rs 58 = Rs 128):
	// both are locked, however the sweeps interleave.
	bothOld := plan("9000015204", 128, long)
	bothNew, err := w.svc.createSubscriptionAt(ctx, bothOld.ConsumerID, subscriptionInput{ProductID: "taaza-500ml", Qty: 2, Frequency: "daily", StartDate: D1}, chainPlanMadeAt)
	if err != nil {
		t.Fatalf("second plan of the covered member: %v", err)
	}
	chainBackdateSubscription(t, w, bothNew, long.Add(time.Hour))

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
	for _, s := range []*subscription{funded, older, bothOld, bothNew} {
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
	for _, cid := range []primitive.ObjectID{funded.ConsumerID, bothOld.ConsumerID} {
		if evs := daySkippedEvents(t, w, cid); len(evs) != 0 {
			t.Fatalf("the funded member %s got %d day_skipped events", cid.Hex(), len(evs))
		}
	}
	for _, s := range []*subscription{funded, short, older, newerSub, bothOld, bothNew} {
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

// nr-1: a second decider for the same member-day (another instance at
// 12:00:05, or a member's change racing the tick) read the member's previews
// while both were unlocked, then read what the member already owes after the
// first decider had locked the older plan. That order was counted twice (as
// committed, and again as a candidate the loop funds), so the newer plan,
// which the wallet covers, was skipped: no milk and a false D-07. The race is
// made deterministic here by handing the decision the stale preview list.
func TestNoonLockCountsAPlanAnotherDeciderLockedOnce(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -3), 9, 0)
	at := istDayAt(D, 12, 0).Add(5 * time.Second)

	cid := w.customer(t, "9000015301", 64) // Rs 29 + Rs 35: both plans are covered
	older := walletLockPlan(t, w, cid, "taaza-500ml", 1, D1, long)
	newer := walletLockPlan(t, w, cid, "gold-500ml", 1, D1, long.Add(time.Hour))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))

	stale, err := w.svc.repo.listMemberDayPreviews(ctx, cid.Hex(), D1) // this decider's read
	if err != nil || len(stale) != 2 {
		t.Fatalf("previews: %d, %v", len(stale), err)
	}
	var olderPreview *order
	for i := range stale {
		if stale[i].SubscriptionID == older.SubscriptionID {
			olderPreview = &stale[i]
		}
	}
	if olderPreview == nil {
		t.Fatalf("no preview for the older plan")
	}
	// The other decider locks the older plan between this one's two reads.
	if !w.svc.lockSubOrder(ctx, olderPreview, false, at) {
		t.Fatalf("the other decider could not lock the older plan")
	}
	w.svc.decideMemberDay(ctx, cid.Hex(), D1, stale, at)

	assertLocked(t, w, older, D1)
	assertLocked(t, w, newer, D1)
	if evs := daySkippedEvents(t, w, cid); len(evs) != 0 {
		t.Fatalf("a member whose wallet covers both plans got %d day_skipped events", len(evs))
	}
}

// nr-1, as the reviewer's probe found it: two deciders for one member-day,
// the second starting 0-3.6 ms after the first. The older plan is the
// cheaper one and the wallet covers both, so both are locked every time.
func TestRacingNoonLocksStaggeredFundBothPlans(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -3), 9, 0)
	at := istDayAt(D, 12, 0).Add(5 * time.Second)
	lost := 0
	const runs = 30
	for i := 0; i < runs; i++ {
		cid := w.customer(t, fmt.Sprintf("90000154%02d", i), 64) // Rs 29 + Rs 35
		older := walletLockPlan(t, w, cid, "taaza-500ml", 1, D1, long)
		newer := walletLockPlan(t, w, cid, "gold-500ml", 1, D1, long.Add(time.Hour))
		w.svc.sweepOneSubscription(ctx, older, istDayAt(D, 9, 0))
		w.svc.sweepOneSubscription(ctx, newer, istDayAt(D, 9, 0))
		stagger := time.Duration(i%10) * 400 * time.Microsecond
		var wg sync.WaitGroup
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(delay time.Duration) {
				defer wg.Done()
				if delay > 0 {
					time.Sleep(delay)
				}
				w.svc.lockConsumerDay(ctx, cid.Hex(), D1, at)
			}(time.Duration(g) * stagger)
		}
		wg.Wait()
		for _, s := range []*subscription{older, newer} {
			if o := liveSubOrder(t, w, s.SubscriptionID, D1); o == nil || o.SubLockedAt == "" {
				lost++
				t.Logf("run %d (stagger %v): %s (%s) not locked: %+v", i, stagger, s.SubscriptionID, s.ProductID, subOrdersFor(t, w, s.SubscriptionID, D1))
			}
		}
	}
	if lost > 0 {
		t.Fatalf("%d plan-days of %d runs were lost to a racing lock although the wallet covered both plans", lost, runs)
	}
}

// memberDayBudget: what the member owes that day is taken off the noon
// wallet once; an order that is also a candidate is funded by the loop.
func TestMemberDayBudgetCountsACandidateOnce(t *testing.T) {
	older := &order{OrderID: "ord_older", Total: 29, SubscriptionID: "sub_old", SubLockedAt: "2026-10-06T06:30:05Z"}
	newer := &order{OrderID: "ord_newer", Total: 35, SubscriptionID: "sub_new"}
	oneOff := order{OrderID: "ord_oneoff", Total: 20, Lane: "morning"}
	freeTrial := order{OrderID: "ord_trial", Total: 70, SubscriptionID: "sub_trial", SubLockedAt: "x", TrialFree: true}
	cands := []lockCandidate{{o: older}, {o: newer}}
	for _, c := range []struct {
		name      string
		committed []order
		want      float64
	}{
		{"nothing owed", nil, 64},
		{"a candidate another decider locked", []order{*older}, 64},
		{"a one-off order and a locked candidate", []order{oneOff, *older}, 44},
		{"a free trial day owes nothing", []order{freeTrial}, 64},
	} {
		if got := memberDayBudget(64, c.committed, cands); got != c.want {
			t.Errorf("%s: budget %v, want %v", c.name, got, c.want)
		}
	}
}

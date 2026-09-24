package consumer

// The low-wallet rule (owner, 24 Sep, R3): the noon lock decides each
// member's day ONCE, on the wallet as it stood at 12:00:00 IST, whatever
// the tick time. A preview the wallet covers is locked with its store task;
// one it does not cover is closed at once as a skipped day (cancelled by
// "wallet_short", the day claim kept, subscription.day_skipped emitted),
// never retried on every tick. The plan never leaves active, so a recharge
// before 12 noon delivers tomorrow and one after noon the day after, and a
// plan never gets stuck. A member's plans are funded oldest first.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run 'WalletLock|CatchUp' -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// walletLockPlan creates a daily plan starting on start and dates it from
// long before, so every day from start is the plan's.
func walletLockPlan(t *testing.T, w *chainWorld, cid primitive.ObjectID, product string, qty int, start string, createdAt time.Time) *subscription {
	t.Helper()
	sub, err := w.svc.createSubscription(context.Background(), cid, subscriptionInput{
		ProductID: product, Qty: qty, Frequency: "daily", StartDate: start,
	})
	if err != nil {
		t.Fatalf("createSubscription %s: %v", product, err)
	}
	chainBackdateSubscription(t, w, sub, createdAt)
	return sub
}

// subOrdersFor is every order (any status) a plan holds for a day.
func subOrdersFor(t *testing.T, w *chainWorld, subID, day string) []order {
	t.Helper()
	cur, err := w.db.Collection(collOrders).Find(context.Background(),
		bson.D{{Key: "subscription_id", Value: subID}, {Key: "scheduled_for", Value: day}})
	if err != nil {
		t.Fatalf("orders of %s on %s: %v", subID, day, err)
	}
	var out []order
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// assertSkipped: the day holds exactly one order, cancelled as a skipped
// day, with no store task; the day stays claimed.
func assertSkipped(t *testing.T, w *chainWorld, sub *subscription, day string) order {
	t.Helper()
	rows := subOrdersFor(t, w, sub.SubscriptionID, day)
	if len(rows) != 1 {
		t.Fatalf("%s on %s: %d orders, want the one skipped preview: %+v", sub.SubscriptionID, day, len(rows), rows)
	}
	o := rows[0]
	if o.Status != "cancelled" || o.CancelledBy != orderCancelledByWalletShort || o.SubLockedAt != "" {
		t.Fatalf("%s on %s: status %s by %q locked %q, want cancelled by %s, never locked", sub.SubscriptionID, day, o.Status, o.CancelledBy, o.SubLockedAt, orderCancelledByWalletShort)
	}
	if task, _ := w.svc.repo.findDeliveryByOrder(context.Background(), o.OrderID); task != nil {
		t.Fatalf("a skipped day has a store task: %+v", task)
	}
	fresh, err := w.svc.repo.findSubscriptionByID(context.Background(), sub.SubscriptionID)
	if err != nil || fresh == nil {
		t.Fatalf("reload plan: %v", err)
	}
	if !fresh.claimed(day) {
		t.Fatalf("the skipped day %s must stay claimed (never re-previewed)", day)
	}
	if fresh.Status != "active" {
		t.Fatalf("a skipped day leaves the plan %s, want active", fresh.Status)
	}
	return o
}

// assertLocked: the day's live order is locked and has its store task.
func assertLocked(t *testing.T, w *chainWorld, sub *subscription, day string) *order {
	t.Helper()
	o := liveSubOrder(t, w, sub.SubscriptionID, day)
	if o == nil || o.SubLockedAt == "" {
		t.Fatalf("%s on %s must be locked: %+v (all: %+v)", sub.SubscriptionID, day, o, subOrdersFor(t, w, sub.SubscriptionID, day))
	}
	if task, _ := w.svc.repo.findDeliveryByOrder(context.Background(), o.OrderID); task == nil {
		t.Fatalf("%s on %s is locked without a store task", sub.SubscriptionID, day)
	}
	return o
}

// daySkippedEvents is the member's subscription.day_skipped outbox rows.
func daySkippedEvents(t *testing.T, w *chainWorld, cid primitive.ObjectID) []bson.M {
	t.Helper()
	cur, err := w.db.Collection(collCRMEvents).Find(context.Background(),
		bson.D{{Key: "topic", Value: "subscription.day_skipped"}, {Key: "consumer_id", Value: cid}})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []bson.M
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("events decode: %v", err)
	}
	return out
}

func TestWalletLockDecidesOnTheWalletAtNoon(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -2), 9, 0)

	late := w.customer(t, "9000012101", 0)  // tops up at 12:07, after the lock moment
	early := w.customer(t, "9000012102", 0) // tops up at 11:59
	lateSub := walletLockPlan(t, w, late, "taaza-500ml", 1, D1, long)
	earlySub := walletLockPlan(t, w, early, "taaza-500ml", 1, D1, long)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0)) // previews D+1
	chainCreditAt(t, w, late, 100, "order_late_12101", istDayAt(D, 12, 7))
	chainCreditAt(t, w, early, 100, "order_early_12102", istDayAt(D, 11, 59))

	// The first tick after noon runs at 12:15: the late top-up is in the
	// wallet by then, but not in the wallet at 12:00:00.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 15))
	skipped := assertSkipped(t, w, lateSub, D1)
	assertLocked(t, w, earlySub, D1)

	evs := daySkippedEvents(t, w, late)
	if len(evs) != 1 {
		t.Fatalf("subscription.day_skipped rows: %d want 1", len(evs))
	}
	p, _ := evs[0]["payload"].(bson.M)
	if p["subscription_id"] != lateSub.SubscriptionID || p["day"] != D1 || p["reason"] != orderCancelledByWalletShort {
		t.Fatalf("day_skipped payload: %v", p)
	}
	if sf, ok := crmPayloadNumber(p["shortfall"]); !ok || sf != 29 {
		t.Fatalf("shortfall: %v, want 29 (the day's cost, nothing in the wallet at 12:00)", p["shortfall"])
	}
	if len(daySkippedEvents(t, w, early)) != 0 {
		t.Fatalf("a locked day emitted day_skipped")
	}

	// Decided once: later ticks neither retry nor re-preview the day.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 30))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 4, 30))
	if again := assertSkipped(t, w, lateSub, D1); again.OrderID != skipped.OrderID {
		t.Fatalf("the skipped day was re-previewed: %+v", again)
	}
	if n := len(daySkippedEvents(t, w, late)); n != 1 {
		t.Fatalf("day_skipped repeated on later ticks: %d", n)
	}
	// No money moved for the skipped day, and the member's next morning is
	// the day after tomorrow, already previewed.
	if cash := w.cash(t, late); cash != 100 {
		t.Fatalf("a skipped day moved money: cash %v", cash)
	}
	if nd := w.svc.nextDeliveryFor(ctx, lateSub, istDayAt(D, 12, 30)); nd != addDaysIST(D, 2) {
		t.Fatalf("next_delivery_date after the skip: %s", nd)
	}
}

// The same decision at any tick time from 12:00:01 to 23:59, also over a
// month end, for a debit and for a credit that land after 12:00:00; before
// noon nothing is decided.
func TestWalletLockIsTheSameAtAnyTickTime(t *testing.T) {
	cases := []struct {
		name string
		day  string
		tick time.Duration // after 12:00:00 on day
	}{
		{"12:00:01", "2026-10-06", time.Second},
		{"12:14:59", "2026-10-06", 14*time.Minute + 59*time.Second},
		{"23:59", "2026-10-06", 11*time.Hour + 59*time.Minute},
		{"month end 12:00:01", "2026-10-31", time.Second},
		{"month end 12:14:59", "2026-10-31", 14*time.Minute + 59*time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, done := newChainWorld(t)
			defer done()
			ctx := context.Background()
			D := c.day
			D1 := addDaysIST(D, 1)
			noon := istDayAt(D, 12, 0)
			tick := noon.Add(c.tick)
			moved := noon.Add(500 * time.Millisecond) // just after the lock moment, before any tick

			// 2 x Rs 29 = Rs 58 a morning.
			spent := w.customer(t, "9000012201", 60)  // covered at 12:00, spends Rs 30 after it
			topped := w.customer(t, "9000012202", 30) // short at 12:00, tops up Rs 100 after it
			long := istDayAt(addDaysIST(D, -2), 9, 0)
			spentSub := walletLockPlan(t, w, spent, "taaza-500ml", 2, D1, long)
			toppedSub := walletLockPlan(t, w, topped, "taaza-500ml", 2, D1, long)

			w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
			chainDebitAt(t, w, spent, 30, "delivery:ord_instant_12201", moved)
			chainCreditAt(t, w, topped, 100, "order_topped_12202", moved)

			// 11:59: before the lock moment nothing is decided.
			w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 11, 59))
			for _, s := range []*subscription{spentSub, toppedSub} {
				if o := liveSubOrder(t, w, s.SubscriptionID, D1); o == nil || o.SubLockedAt != "" {
					t.Fatalf("11:59 decided %s early: %+v", s.SubscriptionID, o)
				}
			}

			w.svc.sweepSubscriptionOrders(ctx, tick)
			assertLocked(t, w, spentSub, D1)
			assertSkipped(t, w, toppedSub, D1)
			// The next day was previewed by the same tick, for both.
			for _, s := range []*subscription{spentSub, toppedSub} {
				if o := liveSubOrder(t, w, s.SubscriptionID, addDaysIST(D, 2)); o == nil || o.SubLockedAt != "" {
					t.Fatalf("the day after tomorrow must be previewed for %s: %+v", s.SubscriptionID, o)
				}
			}
		})
	}
}

// Recharge by 12 noon tomorrow: tomorrow's delivery. After noon: the day
// after. The plan stays active and a preview always waits on the next
// editable day, so nothing stays stuck.
func TestWalletLockRechargeBeforeNoonDeliversTheNextMorning(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2, D3 := addDaysIST(D, 1), addDaysIST(D, 2), addDaysIST(D, 3)
	long := istDayAt(addDaysIST(D, -2), 9, 0)
	beforeNoon := w.customer(t, "9000012301", 0)
	afterNoon := w.customer(t, "9000012302", 0)
	bSub := walletLockPlan(t, w, beforeNoon, "taaza-500ml", 2, D1, long)
	aSub := walletLockPlan(t, w, afterNoon, "taaza-500ml", 2, D1, long)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	assertSkipped(t, w, bSub, D1)
	assertSkipped(t, w, aSub, D1)

	chainCreditAt(t, w, beforeNoon, 100, "order_before_12301", istDayAt(D1, 11, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 5))
	assertLocked(t, w, bSub, D2) // recharged before noon on D+1: D+2 delivered
	assertSkipped(t, w, aSub, D2)

	chainCreditAt(t, w, afterNoon, 100, "order_after_12302", istDayAt(D1, 12, 30))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 35))
	assertSkipped(t, w, aSub, D2) // D+2 was decided at 12:00, before the top-up
	if o := liveSubOrder(t, w, aSub.SubscriptionID, D3); o == nil || o.SubLockedAt != "" {
		t.Fatalf("the plan must wait on D+3 as a preview: %+v", o)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D2, 12, 5))
	assertLocked(t, w, aSub, D3) // recharged after noon on D+1: D+3 delivered
}

// One wallet, two plans: the older plan is funded first, whichever path
// (the tick, or a member change after the cut-off) reaches the day first.
func TestWalletLockTwoPlansOneWallet(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	older, newer := istDayAt(addDaysIST(D, -3), 9, 0), istDayAt(addDaysIST(D, -2), 9, 0)

	// Rs 57 (1 L, the older plan) + Rs 29 (500 ml, the newer) against Rs 60:
	// funding the newer, cheaper plan first would lock it and skip the older.
	byTick := w.customer(t, "9000012401", 60)
	tickOld := walletLockPlan(t, w, byTick, "taaza-1l", 1, D1, older)
	tickNew := walletLockPlan(t, w, byTick, "taaza-500ml", 1, D1, newer)
	byEdit := w.customer(t, "9000012402", 60)
	editNew := walletLockPlan(t, w, byEdit, "taaza-500ml", 1, D1, newer)
	editOld := walletLockPlan(t, w, byEdit, "taaza-1l", 1, D1, older)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	// 12:03, before any tick after noon: the member edits the OLDER plan.
	two := 2
	if _, err := w.svc.patchSubscriptionAt(ctx, byEdit, editOld.SubscriptionID, subscriptionPatch{Qty: &two}, istDayAt(D, 12, 3)); err != nil {
		t.Fatalf("patch: %v", err)
	}
	assertLocked(t, w, editOld, D1)
	assertSkipped(t, w, editNew, D1)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 15))
	assertLocked(t, w, tickOld, D1)
	assertSkipped(t, w, tickNew, D1)
	if o := liveSubOrder(t, w, editOld.SubscriptionID, D1); o.Items[0].Qty != 1 {
		t.Fatalf("the 12:03 edit reached the locked day: %+v", o.Items)
	}
}

// A one-off morning order the member placed for the same day is already
// owed when the lock funds the plan: the wallet that covers only one of the
// two goes to the order placed first, and the plan's day is skipped instead
// of both reaching the door and one failing there for want of funds.
func TestWalletLockReservesTheMembersOneOffMorningOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -2), 9, 0)
	withOrder := w.customer(t, "9000012701", 100)
	without := w.customer(t, "9000012702", 100)
	oSub := walletLockPlan(t, w, withOrder, "taaza-500ml", 2, D1, long) // Rs 58
	nSub := walletLockPlan(t, w, without, "taaza-500ml", 2, D1, long)

	// 10:00: a one-off morning order for tomorrow, 2 x Rs 35 + Rs 15 = Rs 85.
	one, err := w.svc.createOrderAt(ctx, withOrder.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", DeliveryDate: D1, ConsumerName: "One Off", Phone: "9000012701",
	}, istDayAt(D, 10, 0))
	if err != nil {
		t.Fatalf("one-off order: %v", err)
	}
	if one.Total != 85 || one.DeliveryDate != D1 {
		t.Fatalf("one-off: total %v date %s", one.Total, one.DeliveryDate)
	}

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	assertSkipped(t, w, oSub, D1) // 100 - 85 = 15 < 58
	assertLocked(t, w, nSub, D1)  // 100 >= 58
	if o := w.orderByID(t, one.OrderID); o.Status != "placed" {
		t.Fatalf("the one-off order was touched: %s", o.Status)
	}
}

// setTrialCounts moves a member's 2+2 welcome trial to the given delivered
// counts (2 paid, 0 free = the next delivered day is free).
func setTrialCounts(t *testing.T, w *chainWorld, cid primitive.ObjectID, paid, free int) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.svc.repo.getOrCreateTrial(ctx, cid); err != nil {
		t.Fatalf("trial: %v", err)
	}
	if _, err := w.db.Collection(collConsumerTrials).UpdateOne(ctx, bson.D{{Key: "consumer_id", Value: cid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "delivered_paid", Value: paid}, {Key: "delivered_free", Value: free},
			{Key: "phase", Value: trialPhaseFor(paid, free)}}}}); err != nil {
		t.Fatalf("set trial: %v", err)
	}
}

// Decision 9 (24 Sep): the lock asks the wallet for what the morning will
// really take at the door. A 2+2 free trial day takes Rs 0, so an empty
// wallet never skips it; the phase is read at the lock (the day before's
// delivery may have opened the free window after the preview was made) and
// the order's display flag follows. A paid trial day still needs funds.
func TestWalletLockAFreeTrialDayNeedsNoFunds(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -3), 9, 0)

	freeDay := w.customer(t, "9000012801", 0)
	paidDay := w.customer(t, "9000012802", 0)
	both := w.customer(t, "9000012803", 29) // a free gold day, then Rs 29 of taaza
	freeSub := walletLockPlan(t, w, freeDay, "gold-500ml", 2, D1, long)
	paidSub := walletLockPlan(t, w, paidDay, "gold-500ml", 2, D1, long)
	goldSub := walletLockPlan(t, w, both, "gold-500ml", 2, D1, long)
	taazaSub := walletLockPlan(t, w, both, "taaza-500ml", 1, D1, istDayAt(addDaysIST(D, -2), 9, 0))
	setTrialCounts(t, w, paidDay, 1, 0) // the next delivered day is the 2nd paid one

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0)) // previews made in the paid phase
	if o := liveSubOrder(t, w, freeSub.SubscriptionID, D1); o == nil || o.TrialFree {
		t.Fatalf("setup: the preview is made before the free window opens: %+v", o)
	}
	// D's morning delivery was the 2nd paid day: the free window is open.
	setTrialCounts(t, w, freeDay, 2, 0)
	setTrialCounts(t, w, both, 2, 0)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	if o := assertLocked(t, w, freeSub, D1); !o.TrialFree {
		t.Fatalf("the locked free day must read as free: %+v", o)
	}
	assertSkipped(t, w, paidSub, D1)
	assertLocked(t, w, goldSub, D1)
	assertLocked(t, w, taazaSub, D1) // the free gold day reserved nothing
	if cash := w.cash(t, freeDay); cash != 0 {
		t.Fatalf("locking moved money: %v", cash)
	}
}

// Server down over the lock: the catch-up of a day that was never previewed
// decides on the same noon wallet.
func TestCatchUpUsesTheNoonWallet(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -2), 9, 0)
	afternoon := w.customer(t, "9000012501", 0)
	morning := w.customer(t, "9000012502", 0)
	aSub := walletLockPlan(t, w, afternoon, "taaza-500ml", 2, D1, long)
	mSub := walletLockPlan(t, w, morning, "taaza-500ml", 2, D1, long)
	chainCreditAt(t, w, afternoon, 100, "order_pm_12501", istDayAt(D, 15, 0))
	chainCreditAt(t, w, morning, 100, "order_am_12502", istDayAt(D, 11, 0))

	// The first tick of the day runs at 18:00.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 18, 0))
	assertSkipped(t, w, aSub, D1)
	assertLocked(t, w, mSub, D1)
	if n := len(daySkippedEvents(t, w, afternoon)); n != 1 {
		t.Fatalf("the caught-up skip must emit day_skipped once: %d", n)
	}
}

// A day whose morning route has left is never caught up: a plan from
// before the cut-off whose day was never previewed or locked (the server
// was down from before noon the day before until after the route) gets no
// order and no store task for a round already gone; the next day is served.
func TestCatchUpDoesNotLockADayWhoseRouteLeft(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	cid := w.customer(t, "9000012601", 500)
	sub := walletLockPlan(t, w, cid, "taaza-500ml", 1, D, istDayAt(addDaysIST(D, -3), 9, 0))

	// Down since D-2; the first tick is at 09:00 on D, after D's route.
	if placed := w.svc.sweepOneSubscription(ctx, sub, istDayAt(D, 9, 0)); placed != 0 {
		t.Fatalf("a day whose route left was caught up: placed %d", placed)
	}
	if rows := subOrdersFor(t, w, sub.SubscriptionID, D); len(rows) != 0 {
		t.Fatalf("orders for a gone route: %+v", rows)
	}
	// Before D's route (03:00) the same catch-up still delivers D.
	other := w.customer(t, "9000012602", 500)
	early := walletLockPlan(t, w, other, "taaza-500ml", 1, D, istDayAt(addDaysIST(D, -3), 9, 0))
	if placed := w.svc.sweepOneSubscription(ctx, early, istDayAt(D, 3, 0)); placed != 1 {
		t.Fatalf("catch-up before the route placed %d, want 1", placed)
	}
	assertLocked(t, w, early, D)
	// The first plan goes on: the 09:00 tick previewed D+1, and the lock
	// after noon on D delivers it as usual.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 13, 0))
	assertLocked(t, w, sub, D1)
}

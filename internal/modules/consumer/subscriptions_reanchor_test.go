package consumer

// G4 (owner, 24 Sep): a plan started on a morning that is already locked
// (tomorrow from 12:00 IST, today, anything up to lockedThroughDay) starts on
// the first editable morning instead, and keeps its cadence from there. An
// alternate or weekly plan anchored on a locked tomorrow first delivered 3 or
// 8 days later: a whole cycle lost. A past start date is an anchor, not a
// start request (the shipped app re-sends a plan's original start on
// resume), and is left alone; a patch that re-sends the stored start date is
// not a change and moves nothing.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run Reanchor -v

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestNoonReanchorsALockedStart(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	const M = "2026-10-31"
	sec := func(t time.Time, s int) time.Time { return t.Add(time.Duration(s) * time.Second) }

	for i, c := range []struct {
		name      string
		at        time.Time
		freq      string
		start     string
		wantStart string
		wantNext  string
	}{
		{"14:00 alternate from tomorrow", istDayAt(D, 14, 0), "alternate", D1, D2, D2},
		{"14:00 weekly from tomorrow", istDayAt(D, 14, 0), "weekly", D1, D2, D2},
		{"14:00 daily from tomorrow", istDayAt(D, 14, 0), "daily", D1, D2, D2},
		{"12:00:01 alternate from tomorrow", sec(istDayAt(D, 12, 0), 1), "alternate", D1, D2, D2},
		{"12:14:59 weekly from tomorrow", sec(istDayAt(D, 12, 14), 59), "weekly", D1, D2, D2},
		{"23:59 alternate from tomorrow", istDayAt(D, 23, 59), "alternate", D1, D2, D2},
		{"11:59 alternate from tomorrow (open)", istDayAt(D, 11, 59), "alternate", D1, D1, D1},
		{"11:00 alternate from today", istDayAt(D, 11, 0), "alternate", D, D1, D1},
		{"11:00 weekly, no start", istDayAt(D, 11, 0), "weekly", "", D1, D1},
		{"14:00 weekly from the day after tomorrow (open)", istDayAt(D, 14, 0), "weekly", D2, D2, D2},
		{"14:00 alternate from a past day (an anchor)", istDayAt(D, 14, 0), "alternate", addDaysIST(D, -5), addDaysIST(D, -5), addDaysIST(D, 3)},
		{"month end 12:00:01 alternate from the 1st", sec(istDayAt(M, 12, 0), 1), "alternate", "2026-11-01", "2026-11-02", "2026-11-02"},
		{"month end 23:59 weekly from the 1st", istDayAt(M, 23, 59), "weekly", "2026-11-01", "2026-11-02", "2026-11-02"},
	} {
		cid := w.customer(t, "90000140"+strconv.Itoa(10+i), 1000)
		sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Qty: 1, Frequency: c.freq, StartDate: c.start}, c.at)
		if err != nil {
			t.Fatalf("%s: create: %v", c.name, err)
		}
		if sub.StartDate != c.wantStart || sub.NextDeliveryDate != c.wantNext {
			t.Fatalf("%s: start_date %s next_delivery_date %s, want %s %s", c.name, sub.StartDate, sub.NextDeliveryDate, c.wantStart, c.wantNext)
		}
		stored, _ := w.svc.repo.findSubscriptionByID(ctx, sub.SubscriptionID)
		if stored == nil || stored.StartDate != c.wantStart {
			t.Fatalf("%s: stored start %+v, want %s", c.name, stored, c.wantStart)
		}
		// The create handler's kick previews the first editable morning when
		// the plan is due on it: no cycle is lost.
		w.svc.sweepOneSubscription(ctx, sub, c.at)
		first := firstEditableDay(c.at)
		if c.wantNext == first {
			if o := liveSubOrder(t, w, sub.SubscriptionID, first); o == nil {
				t.Fatalf("%s: no preview for the first editable morning %s", c.name, first)
			}
		}
	}

	type patch = struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}

	// An alternate plan made at 10:00 for tomorrow keeps tomorrow: it is
	// locked at noon. At 13:00 the shipped app re-sends the same start date
	// (its resume mirror, "harmless when unchanged"): nothing moves.
	cid := w.customer(t, "9000014101", 1000)
	alt, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Qty: 1, Frequency: "alternate", StartDate: D1}, istDayAt(D, 10, 0))
	if err != nil || alt.StartDate != D1 {
		t.Fatalf("create before noon: %v %+v", err, alt)
	}
	w.svc.sweepOneSubscription(ctx, alt, istDayAt(D, 10, 0))
	w.svc.sweepSubscriptionOrders(ctx, sec(istDayAt(D, 12, 0), 5))
	assertLocked(t, w, alt, D1)
	same := D1
	got, err := w.svc.patchSubscriptionAt(ctx, cid, alt.SubscriptionID, patch{StartDate: &same}, istDayAt(D, 13, 0))
	if err != nil || got.StartDate != D1 {
		t.Fatalf("re-sending the stored start must not move it: %v %+v", err, got)
	}
	if nd := w.svc.nextDeliveryFor(ctx, got, istDayAt(D, 13, 0)); nd != D1 {
		t.Fatalf("next_delivery_date after the re-send: %s want %s", nd, D1)
	}
	assertLocked(t, w, alt, D1)

	// A new start date on a locked morning (the shipped app's reactivate with
	// "tomorrow" after noon) re-anchors on the first editable one; a past one
	// is an anchor and stays.
	cid2 := w.customer(t, "9000014102", 1000)
	old, err := w.svc.createSubscriptionAt(ctx, cid2, subscriptionInput{ProductID: "gold-500ml", Qty: 1, Frequency: "weekly", StartDate: addDaysIST(D, -10)}, istDayAt(addDaysIST(D, -12), 9, 0))
	if err != nil {
		t.Fatalf("create old: %v", err)
	}
	tomorrow := D1
	got, err = w.svc.patchSubscriptionAt(ctx, cid2, old.SubscriptionID, patch{StartDate: &tomorrow}, istDayAt(D, 13, 10))
	if err != nil || got.StartDate != D2 {
		t.Fatalf("a start moved onto a locked tomorrow after noon: %v %+v, want %s", err, got, D2)
	}
	if nd := w.svc.nextDeliveryFor(ctx, got, istDayAt(D, 13, 10)); nd != D2 {
		t.Fatalf("next_delivery_date after the re-anchor: %s want %s", nd, D2)
	}
	if o := liveSubOrder(t, w, old.SubscriptionID, D2); o == nil {
		t.Fatalf("the re-anchored weekly plan previews its first morning at once")
	}
	past := addDaysIST(D, -3)
	got, err = w.svc.patchSubscriptionAt(ctx, cid2, old.SubscriptionID, patch{StartDate: &past}, istDayAt(D, 13, 20))
	if err != nil || got.StartDate != past {
		t.Fatalf("a past start is an anchor, kept as sent: %v %+v", err, got)
	}
	// Before noon, tomorrow is open: kept as sent.
	cid3 := w.customer(t, "9000014103", 1000)
	early, err := w.svc.createSubscriptionAt(ctx, cid3, subscriptionInput{ProductID: "gold-500ml", Qty: 1, Frequency: "weekly", StartDate: addDaysIST(D, -10)}, istDayAt(addDaysIST(D, -12), 9, 0))
	if err != nil {
		t.Fatalf("create early: %v", err)
	}
	got, err = w.svc.patchSubscriptionAt(ctx, cid3, early.SubscriptionID, patch{StartDate: &tomorrow}, istDayAt(D, 11, 0))
	if err != nil || got.StartDate != D1 {
		t.Fatalf("tomorrow before noon is open: %v %+v", err, got)
	}
}

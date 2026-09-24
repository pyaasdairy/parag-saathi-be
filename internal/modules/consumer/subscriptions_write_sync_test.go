package consumer

// R4(i), 24 Sep: a member's change reaches the store's Upcoming list (and the
// member's own Orders) on the next poll, not after up to 15 minutes. The
// write path of patch, pause, resume and cancel brings the plan's
// still-editable previews into line at once (syncPlanPreviews); no sweep runs
// in between. A day already past its cut-off is never touched by it.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run WritePath -v

import (
	"context"
	"testing"
	"time"
)

// upcomingFor is the store's Upcoming rows at `at`, keyed by order id.
func upcomingFor(t *testing.T, w *chainWorld, at time.Time) map[string]upcomingRow {
	t.Helper()
	rows, err := w.svc.storeUpcomingAt(context.Background(), w.mgr, w.storeID.Hex(), at)
	if err != nil {
		t.Fatalf("storeUpcoming: %v", err)
	}
	out := map[string]upcomingRow{}
	for _, r := range rows {
		out[r.OrderID] = r
	}
	return out
}

func TestWritePathKeepsPreviewsLive(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	type patch = struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}

	// Before noon: a plan made at 09:00 for tomorrow, previewed by the create
	// handler's kick.
	cid := w.customer(t, "9000013001", 5000)
	sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D1}, istDayAt(D, 9, 0))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w.svc.sweepOneSubscription(ctx, sub, istDayAt(D, 9, 0))
	prev := liveSubOrder(t, w, sub.SubscriptionID, D1)
	if prev == nil {
		t.Fatalf("setup: no preview for tomorrow")
	}

	// 10:05 qty 3: the preview and the store's Upcoming row show it at once.
	three := 3
	if _, err := w.svc.patchSubscriptionAt(ctx, cid, sub.SubscriptionID, patch{Qty: &three}, istDayAt(D, 10, 5)); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o == nil || o.Items[0].Qty != 3 || o.Total != 87 {
		t.Fatalf("a qty edit must reach tomorrow's preview with no sweep: %+v", o)
	}
	if row, ok := upcomingFor(t, w, istDayAt(D, 10, 5))[prev.OrderID]; !ok || row.Items[0].Qty != 3 {
		t.Fatalf("Upcoming after the edit: %+v", row)
	}

	// 10:10 pause: the preview is cancelled and its day released.
	if _, err := w.svc.setSubscriptionStatusAt(ctx, cid, sub.SubscriptionID, "pause", istDayAt(D, 10, 10)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o != nil {
		t.Fatalf("a pause before noon must cancel tomorrow's preview at once: %+v", o)
	}
	if fresh, _ := w.svc.repo.findSubscriptionByID(ctx, sub.SubscriptionID); fresh == nil || fresh.claimed(D1) {
		t.Fatalf("the paused day must be released for a resume: %+v", fresh)
	}
	if rows := upcomingFor(t, w, istDayAt(D, 10, 10)); len(rows) != 0 {
		t.Fatalf("Upcoming after the pause: %+v", rows)
	}

	// 10:15 resume: tomorrow is previewed again, with the plan's qty.
	if _, err := w.svc.setSubscriptionStatusAt(ctx, cid, sub.SubscriptionID, "resume", istDayAt(D, 10, 15)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	back := liveSubOrder(t, w, sub.SubscriptionID, D1)
	if back == nil || back.SubLockedAt != "" || back.Items[0].Qty != 3 {
		t.Fatalf("a resume before noon must re-preview tomorrow at once: %+v", back)
	}
	if row, ok := upcomingFor(t, w, istDayAt(D, 10, 15))[back.OrderID]; !ok || row.DeliveryDate != D1 {
		t.Fatalf("Upcoming after the resume: %+v", row)
	}
	if nd := w.svc.nextDeliveryFor(ctx, sub, istDayAt(D, 10, 15)); nd != D1 {
		t.Fatalf("next_delivery_date after the resume: %s want %s", nd, D1)
	}

	// 10:20 cancel: gone.
	if _, err := w.svc.setSubscriptionStatusAt(ctx, cid, sub.SubscriptionID, "cancel", istDayAt(D, 10, 20)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if o := liveSubOrder(t, w, sub.SubscriptionID, D1); o != nil {
		t.Fatalf("a cancelled plan keeps tomorrow's preview: %+v", o)
	}
	if rows := upcomingFor(t, w, istDayAt(D, 10, 20)); len(rows) != 0 {
		t.Fatalf("Upcoming after the cancel: %+v", rows)
	}

	// After noon: tomorrow is locked with its task; a change reaches only the
	// day after tomorrow.
	other := w.customer(t, "9000013002", 5000)
	sub2, err := w.svc.createSubscriptionAt(ctx, other, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D1}, istDayAt(D, 9, 0))
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	w.svc.sweepOneSubscription(ctx, sub2, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 0).Add(5*time.Second))
	lockedD1 := assertLocked(t, w, sub2, D1)
	nextD2 := liveSubOrder(t, w, sub2.SubscriptionID, D2)
	if nextD2 == nil {
		t.Fatalf("setup: no preview for the day after tomorrow")
	}

	// 13:00 pause: D1's task stays, D2's preview goes.
	if _, err := w.svc.setSubscriptionStatusAt(ctx, other, sub2.SubscriptionID, "pause", istDayAt(D, 13, 0)); err != nil {
		t.Fatalf("pause 2: %v", err)
	}
	if o := liveSubOrder(t, w, sub2.SubscriptionID, D1); o == nil || o.OrderID != lockedD1.OrderID || o.SubLockedAt == "" {
		t.Fatalf("a pause after noon must leave tomorrow's locked order: %+v", o)
	}
	if task, _ := w.svc.repo.findDeliveryByOrder(ctx, lockedD1.OrderID); task == nil || task.Status != "ASSIGNED" {
		t.Fatalf("tomorrow's task after a pause after noon: %+v", task)
	}
	if o := liveSubOrder(t, w, sub2.SubscriptionID, D2); o != nil {
		t.Fatalf("a pause after noon must cancel the day after tomorrow at once: %+v", o)
	}
	if rows := upcomingFor(t, w, istDayAt(D, 13, 0)); len(rows) != 0 {
		t.Fatalf("Upcoming after the pause after noon: %+v", rows)
	}

	// 13:30 resume: D2 comes back at once; 13:40 a vacation over D2 takes it.
	if _, err := w.svc.setSubscriptionStatusAt(ctx, other, sub2.SubscriptionID, "resume", istDayAt(D, 13, 30)); err != nil {
		t.Fatalf("resume 2: %v", err)
	}
	d2 := liveSubOrder(t, w, sub2.SubscriptionID, D2)
	if d2 == nil || d2.SubLockedAt != "" {
		t.Fatalf("a resume after noon must re-preview the day after tomorrow at once: %+v", d2)
	}
	if _, ok := upcomingFor(t, w, istDayAt(D, 13, 30))[d2.OrderID]; !ok {
		t.Fatalf("Upcoming after the resume after noon misses %s", d2.OrderID)
	}
	vac := []vacationRange{{Start: D2, End: D2}}
	if _, err := w.svc.patchSubscriptionAt(ctx, other, sub2.SubscriptionID, patch{Vacations: &vac}, istDayAt(D, 13, 40)); err != nil {
		t.Fatalf("vacation: %v", err)
	}
	if o := liveSubOrder(t, w, sub2.SubscriptionID, D2); o != nil {
		t.Fatalf("a vacation over the day after tomorrow must cancel its preview at once: %+v", o)
	}
	if o := liveSubOrder(t, w, sub2.SubscriptionID, D1); o == nil || o.SubLockedAt == "" {
		t.Fatalf("tomorrow's locked order after the vacation edit: %+v", o)
	}
	// Exactly one live order per day throughout.
	for _, day := range []string{D1, D2} {
		live := 0
		for _, o := range subOrdersFor(t, w, sub2.SubscriptionID, day) {
			if o.Status != "cancelled" {
				live++
			}
		}
		want := map[string]int{D1: 1, D2: 0}[day]
		if live != want {
			t.Fatalf("live orders for %s: %d want %d", day, live, want)
		}
	}
}

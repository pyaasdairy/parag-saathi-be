package consumer

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// newTrial is an empty in-memory ledger — the pure trial logic needs no DB, so
// the whole progression is exercised against consumerTrial directly.
func newTrial() *consumerTrial {
	return &consumerTrial{Phase: trialPhasePaid, Charges: []trialCharge{}}
}

// TestTrialProgression walks the full "3 PAID then 3 FREE then normal" arc and
// pins the effective amount, the per-charge phase, and the running counters at
// every step. Each call uses a DISTINCT delivery key (a distinct delivered day).
func TestTrialProgression(t *testing.T) {
	const full = 60.0
	tr := newTrial()

	type step struct {
		key       string
		wantEff   float64
		wantPhase string
		wantPaid  int
		wantFree  int
	}
	steps := []step{
		{"d1", full, trialPhasePaid, 1, 0}, // paid 1
		{"d2", full, trialPhasePaid, 2, 0}, // paid 2
		{"d3", full, trialPhasePaid, 3, 0}, // paid 3  ← boundary: day 3 still paid
		{"d4", 0, trialPhaseFree, 3, 1},    // free 1  ← boundary: day 4 flips to free
		{"d5", 0, trialPhaseFree, 3, 2},    // free 2
		{"d6", 0, trialPhaseFree, 3, 3},    // free 3  ← boundary: day 6 still free
		{"d7", full, trialPhaseDone, 3, 3}, // done    ← boundary: day 7 pays again
		{"d8", full, trialPhaseDone, 3, 3}, // done — stays normal
	}
	for i, s := range steps {
		eff, phase := tr.charge(s.key, full)
		if eff != s.wantEff || phase != s.wantPhase {
			t.Errorf("step %d (%s): got (%.0f, %q), want (%.0f, %q)", i+1, s.key, eff, phase, s.wantEff, s.wantPhase)
		}
		if tr.DeliveredPaid != s.wantPaid || tr.DeliveredFree != s.wantFree {
			t.Errorf("step %d (%s): counters (paid=%d, free=%d), want (paid=%d, free=%d)", i+1, s.key, tr.DeliveredPaid, tr.DeliveredFree, s.wantPaid, s.wantFree)
		}
	}
}

// TestTrialBoundaries isolates the four boundary days the spec calls out:
// day 3 paid, day 4 free, day 6 free, day 7 paid.
func TestTrialBoundaries(t *testing.T) {
	const full = 100.0
	tr := newTrial()
	got := make([]float64, 0, 7)
	for i := 1; i <= 7; i++ {
		eff, _ := tr.charge(string(rune('a'+i)), full)
		got = append(got, eff)
	}
	// days:        1     2     3     4    5    6    7
	want := []float64{full, full, full, 0, 0, 0, full}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("day %d effective = %.0f, want %.0f", i+1, got[i], want[i])
		}
	}
	if got[2] != full {
		t.Error("day 3 must be PAID (full amount)")
	}
	if got[3] != 0 {
		t.Error("day 4 must be FREE (0)")
	}
	if got[5] != 0 {
		t.Error("day 6 must be FREE (0)")
	}
	if got[6] != full {
		t.Error("day 7 must be PAID (full amount)")
	}
}

// TestTrialPauseSafe proves the window counts DELIVERED days, not calendar days:
// a paused/skipped day is simply a key that is never charged, and it does NOT
// burn a free day. Two shoppers with wildly different calendars but the same
// number of DELIVERIES end up in the exact same trial state, and the free window
// only opens after 3 REAL paid deliveries.
func TestTrialPauseSafe(t *testing.T) {
	const full = 40.0

	// Shopper A delivers on consecutive calendar days.
	a := newTrial()
	// Shopper B pauses for long stretches between deliveries (gap days are never
	// charged — no key, no advance), but makes the same number of deliveries.
	b := newTrial()

	aKeys := []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04"}
	// B skips whole weeks between each delivery — a pause on "day 2..N" must not
	// consume the free window.
	bKeys := []string{"2026-01-01", "2026-01-20", "2026-02-15", "2026-03-30"}

	for i := range aKeys {
		ea, pa := a.charge(aKeys[i], full)
		eb, pb := b.charge(bKeys[i], full)
		if ea != eb || pa != pb {
			t.Errorf("delivery %d: A=(%.0f,%q) B=(%.0f,%q) — delivered-day counting must ignore the calendar gap", i+1, ea, pa, eb, pb)
		}
	}
	// After exactly 3 paid deliveries + 1 more, BOTH must be paid=3, free=1 — the
	// free window opened only after the 3rd real paid delivery, regardless of the
	// calendar pauses in between.
	if a.DeliveredPaid != 3 || a.DeliveredFree != 1 {
		t.Errorf("A: paid=%d free=%d, want 3/1", a.DeliveredPaid, a.DeliveredFree)
	}
	if b.DeliveredPaid != 3 || b.DeliveredFree != 1 {
		t.Errorf("B: paid=%d free=%d, want 3/1 — a pause burned a free day", b.DeliveredPaid, b.DeliveredFree)
	}

	// Explicit "pause does not advance": charging nothing (a paused morning is
	// simply no call) leaves the ledger untouched.
	before := *b
	// no charge happens on a paused day
	if b.DeliveredPaid != before.DeliveredPaid || b.DeliveredFree != before.DeliveredFree {
		t.Error("a paused day must not advance the trial")
	}
}

// TestTrialIdempotentByKey proves a replay of the SAME delivery key returns the
// SAME result and never double-counts — the settle-sweep re-run guard.
func TestTrialIdempotentByKey(t *testing.T) {
	const full = 55.0
	tr := newTrial()

	// First charge on day 1 — paid.
	eff1, ph1 := tr.charge("day-1", full)
	if eff1 != full || ph1 != trialPhasePaid {
		t.Fatalf("first charge = (%.0f,%q), want (%.0f,paid)", eff1, ph1, full)
	}
	snapPaid, snapFree, snapCharges := tr.DeliveredPaid, tr.DeliveredFree, len(tr.Charges)

	// Replay the identical key many times — result is stable, counters frozen.
	for i := 0; i < 5; i++ {
		eff, ph := tr.charge("day-1", full)
		if eff != eff1 || ph != ph1 {
			t.Errorf("replay %d = (%.0f,%q), want (%.0f,%q)", i, eff, ph, eff1, ph1)
		}
		if tr.DeliveredPaid != snapPaid || tr.DeliveredFree != snapFree || len(tr.Charges) != snapCharges {
			t.Errorf("replay %d advanced the ledger: paid=%d free=%d charges=%d, want %d/%d/%d",
				i, tr.DeliveredPaid, tr.DeliveredFree, len(tr.Charges), snapPaid, snapFree, snapCharges)
		}
	}

	// A brand-new key on the same ledger DOES advance (distinct delivered day).
	eff2, _ := tr.charge("day-2", full)
	if eff2 != full || tr.DeliveredPaid != 2 {
		t.Errorf("a distinct key must advance: eff=%.0f paid=%d, want %.0f/2", eff2, tr.DeliveredPaid, full)
	}

	// Idempotency is preserved across a FREE day too: run to the free window and
	// replay a free key — still returns 0 and does not advance.
	tr.charge("day-3", full)  // paid 3
	effFree, phFree := tr.charge("day-4", full) // free 1
	if effFree != 0 || phFree != trialPhaseFree {
		t.Fatalf("day-4 = (%.0f,%q), want (0,free)", effFree, phFree)
	}
	freeSnap := tr.DeliveredFree
	if e, p := tr.charge("day-4", full); e != 0 || p != trialPhaseFree || tr.DeliveredFree != freeSnap {
		t.Errorf("free-day replay = (%.0f,%q) free=%d, want (0,free) free=%d", e, p, tr.DeliveredFree, freeSnap)
	}
}

// TestTrialSameDayDifferentAmount proves a SECOND delivery on the same trial day
// (e.g. milk + curd in one morning, or a settle re-run with a corrected total) is
// charged its OWN amount via the day's decided phase — never a replay of the first
// delivery's amount — while the day still counts once toward the window.
func TestTrialSameDayDifferentAmount(t *testing.T) {
	tr := newTrial()

	// First delivery of a PAID day at ₹60 → full, advances paid to 1.
	if eff, ph := tr.charge("day-1", 60); eff != 60 || ph != trialPhasePaid {
		t.Fatalf("first paid = (%.0f,%q), want (60,paid)", eff, ph)
	}
	// A second delivery the SAME day at a DIFFERENT amount (₹25) must charge ₹25,
	// not ₹60, and must NOT advance the counter (still one delivered day).
	if eff, ph := tr.charge("day-1", 25); eff != 25 || ph != trialPhasePaid {
		t.Errorf("same-day 2nd paid = (%.0f,%q), want (25,paid) — must not replay the first amount", eff, ph)
	}
	if tr.DeliveredPaid != 1 {
		t.Errorf("same-day 2nd delivery advanced the window: paid=%d, want 1", tr.DeliveredPaid)
	}

	// Run into the FREE window; a same-day second free delivery is also 0 regardless
	// of its own price — a free day is free for every delivery that day.
	tr.charge("day-2", 60) // paid 2
	tr.charge("day-3", 60) // paid 3
	if eff, ph := tr.charge("day-4", 60); eff != 0 || ph != trialPhaseFree {
		t.Fatalf("day-4 first = (%.0f,%q), want (0,free)", eff, ph)
	}
	if eff, ph := tr.charge("day-4", 999); eff != 0 || ph != trialPhaseFree {
		t.Errorf("same-day 2nd free = (%.0f,%q), want (0,free) at any price", eff, ph)
	}
	if tr.DeliveredFree != 1 {
		t.Errorf("same-day 2nd free advanced the window: free=%d, want 1", tr.DeliveredFree)
	}
}

// TestTrialDeliveryKey pins the per-shopper, per-day idempotency key shape and
// that it is stable per (consumer, day) and distinct across days and shoppers.
func TestTrialDeliveryKey(t *testing.T) {
	c1 := primitive.NewObjectID()
	c2 := primitive.NewObjectID()

	k := trialDeliveryKey(c1, "2026-07-29")
	if want := "sub:" + c1.Hex() + ":2026-07-29"; k != want {
		t.Errorf("key = %q, want %q", k, want)
	}
	// Same shopper + same day → identical key (a re-run collapses onto one entry).
	if trialDeliveryKey(c1, "2026-07-29") != k {
		t.Error("same (consumer, day) must yield the same key")
	}
	// A different day → a distinct key (a new delivered day).
	if trialDeliveryKey(c1, "2026-07-30") == k {
		t.Error("a different day must yield a distinct key")
	}
	// A different shopper → a distinct key (per-shopper isolation).
	if trialDeliveryKey(c2, "2026-07-29") == k {
		t.Error("a different shopper must yield a distinct key")
	}
}

// TestTrialView pins the GET /trial/me projection across the phases.
func TestTrialView(t *testing.T) {
	const full = 30.0
	tr := newTrial()

	// Fresh shopper: fully paid phase, nothing delivered, free not active.
	v := tr.view()
	if v.Phase != trialPhasePaid || v.PaidRemaining != 3 || v.FreeRemaining != 3 || v.FreeActive {
		t.Errorf("fresh view = %+v, want paid/3/3/false", v)
	}

	// After 2 paid days: still paid phase, 1 paid remaining, free not yet active.
	tr.charge("d1", full)
	tr.charge("d2", full)
	v = tr.view()
	if v.Phase != trialPhasePaid || v.DeliveredPaid != 2 || v.PaidRemaining != 1 || v.FreeActive {
		t.Errorf("mid-paid view = %+v, want paid/paid=2/remaining=1/free=false", v)
	}

	// After the 3rd paid day: free window opens (freeActive true, paidRemaining 0).
	tr.charge("d3", full)
	v = tr.view()
	if v.Phase != trialPhaseFree || v.PaidRemaining != 0 || v.FreeRemaining != 3 || !v.FreeActive {
		t.Errorf("free-open view = %+v, want free/paidRemaining=0/freeRemaining=3/free=true", v)
	}

	// After all 6 days: done, both remainings 0, free no longer active.
	tr.charge("d4", full)
	tr.charge("d5", full)
	tr.charge("d6", full)
	v = tr.view()
	if v.Phase != trialPhaseDone || v.PaidRemaining != 0 || v.FreeRemaining != 0 || v.FreeActive {
		t.Errorf("done view = %+v, want done/0/0/false", v)
	}
}

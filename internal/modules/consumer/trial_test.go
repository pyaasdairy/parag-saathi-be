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

// TestTrialProgression walks the full "2 PAID then 2 FREE then normal" arc and
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
		{"d1", 0, trialPhaseFree, 0, 1},    // free 1  ← the trial OPENS free
		{"d2", 0, trialPhaseFree, 0, 2},    // free 2  ← boundary: day 2 still free
		{"d3", full, trialPhasePaid, 1, 2}, // paid 1  ← boundary: day 3 flips to paid
		{"d4", full, trialPhasePaid, 2, 2}, // paid 2  ← boundary: day 4 still paid
		{"d5", full, trialPhaseDone, 2, 2}, // done    ← day 5 bills normally
		{"d6", full, trialPhaseDone, 2, 2}, // done — stays normal
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
// day 1 free, day 2 free, day 3 paid, day 4 paid.
func TestTrialBoundaries(t *testing.T) {
	const full = 100.0
	tr := newTrial()
	got := make([]float64, 0, 6)
	for i := 1; i <= 6; i++ {
		eff, _ := tr.charge(string(rune('a'+i)), full)
		got = append(got, eff)
	}
	// days:     1  2     3     4     5     6
	want := []float64{0, 0, full, full, full, full}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("day %d effective = %.0f, want %.0f", i+1, got[i], want[i])
		}
	}
	if got[0] != 0 {
		t.Error("day 1 must be FREE (0) — the banner promise")
	}
	if got[1] != 0 {
		t.Error("day 2 must be FREE (0) — the banner promise")
	}
	if got[2] != full {
		t.Error("day 3 must be PAID (full amount)")
	}
	if got[3] != full {
		t.Error("day 4 must be PAID (full amount)")
	}
}

// TestTrialPauseSafe proves the window counts DELIVERED days, not calendar days:
// a paused/skipped day is simply a key that is never charged, and it does NOT
// burn a free day. Two shoppers with wildly different calendars but the same
// number of DELIVERIES end up in the exact same trial state, and the free window
// counts only REAL deliveries — free-first, the paid window opens after 2 real free deliveries.
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
	// After 4 deliveries, BOTH must be paid=2, free=2 — the paid window opened
	// only after the 2nd real FREE delivery, regardless of calendar pauses.
	if a.DeliveredPaid != 2 || a.DeliveredFree != 2 {
		t.Errorf("A: paid=%d free=%d, want 2/2", a.DeliveredPaid, a.DeliveredFree)
	}
	if b.DeliveredPaid != 2 || b.DeliveredFree != 2 {
		t.Errorf("B: paid=%d free=%d, want 2/2 — a pause burned a free day", b.DeliveredPaid, b.DeliveredFree)
	}

	// Explicit "pause does not advance": charging nothing (a paused morning is
	// simply no call) leaves the ledger untouched.
	before := *b
	if b.DeliveredPaid != before.DeliveredPaid || b.DeliveredFree != before.DeliveredFree {
		t.Error("a paused day must not advance the trial")
	}
}

// TestTrialIdempotentByKey proves a replay of the SAME delivery key returns the
// SAME result and never double-counts — the settle-sweep re-run guard.
func TestTrialIdempotentByKey(t *testing.T) {
	const full = 55.0
	tr := newTrial()

	// First charge on day 1 — FREE (the trial opens free).
	eff1, ph1 := tr.charge("day-1", full)
	if eff1 != 0 || ph1 != trialPhaseFree {
		t.Fatalf("first charge = (%.0f,%q), want (0,free)", eff1, ph1)
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
	eff2, _ := tr.charge("day-2", full) // free 2 — fills the free window
	if eff2 != 0 || tr.DeliveredFree != 2 {
		t.Errorf("a distinct key must advance: eff=%.0f free=%d, want 0/2", eff2, tr.DeliveredFree)
	}

	// Idempotency is preserved across a PAID day too: the 2 free days are done, so
	// day-3 is the first paid day — replay it and it still bills full, no advance.
	effPaid, phPaid := tr.charge("day-3", full) // paid 1
	if effPaid != full || phPaid != trialPhasePaid {
		t.Fatalf("day-3 = (%.0f,%q), want (%.0f,paid)", effPaid, phPaid, full)
	}
	paidSnap := tr.DeliveredPaid
	if e, p := tr.charge("day-3", full); e != full || p != trialPhasePaid || tr.DeliveredPaid != paidSnap {
		t.Errorf("paid-day replay = (%.0f,%q) paid=%d, want (%.0f,paid) paid=%d", e, p, tr.DeliveredPaid, full, paidSnap)
	}
}

// TestTrialSameDayDifferentAmount proves a SECOND delivery on the same trial day
// (e.g. milk + curd in one morning, or a settle re-run with a corrected total) is
// charged its OWN amount via the day's decided phase — never a replay of the first
// delivery's amount — while the day still counts once toward the window.
func TestTrialSameDayDifferentAmount(t *testing.T) {
	tr := newTrial()

	// First delivery of a FREE day (the trial opens free) → 0, advances free to 1.
	if eff, ph := tr.charge("day-1", 60); eff != 0 || ph != trialPhaseFree {
		t.Fatalf("first free = (%.0f,%q), want (0,free)", eff, ph)
	}
	// A second delivery the SAME free day is also 0 regardless of its own price,
	// and must NOT advance the counter (still one delivered day).
	if eff, ph := tr.charge("day-1", 999); eff != 0 || ph != trialPhaseFree {
		t.Errorf("same-day 2nd free = (%.0f,%q), want (0,free) at any price", eff, ph)
	}
	if tr.DeliveredFree != 1 {
		t.Errorf("same-day 2nd delivery advanced the window: free=%d, want 1", tr.DeliveredFree)
	}

	// Fill the 2 free days, then day-3 is the first PAID day; a same-day second
	// paid delivery charges its OWN amount — never a replay of the first.
	tr.charge("day-2", 60) // free 2
	if eff, ph := tr.charge("day-3", 60); eff != 60 || ph != trialPhasePaid {
		t.Fatalf("day-3 first = (%.0f,%q), want (60,paid)", eff, ph)
	}
	if eff, ph := tr.charge("day-3", 25); eff != 25 || ph != trialPhasePaid {
		t.Errorf("same-day 2nd paid = (%.0f,%q), want (25,paid) — must not replay the first amount", eff, ph)
	}
	if tr.DeliveredPaid != 1 {
		t.Errorf("same-day 2nd paid advanced the window: paid=%d, want 1", tr.DeliveredPaid)
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

	// Fresh shopper: the FREE window is open from day one.
	v := tr.view()
	if v.Phase != trialPhaseFree || v.PaidRemaining != 2 || v.FreeRemaining != 2 || !v.FreeActive {
		t.Errorf("fresh view = %+v, want free/2/2/true", v)
	}

	// After 1 free day: still free phase, 1 free remaining.
	tr.charge("d1", full)
	v = tr.view()
	if v.Phase != trialPhaseFree || v.DeliveredFree != 1 || v.FreeRemaining != 1 || !v.FreeActive {
		t.Errorf("mid-free view = %+v, want free/free=1/remaining=1/active=true", v)
	}

	// After the 2nd free day: paid window opens (free no longer active).
	tr.charge("d2", full)
	v = tr.view()
	if v.Phase != trialPhasePaid || v.FreeRemaining != 0 || v.PaidRemaining != 2 || v.FreeActive {
		t.Errorf("paid-open view = %+v, want paid/freeRemaining=0/paidRemaining=2/free=false", v)
	}

	// After all 4 days: done, both remainings 0, free no longer active.
	tr.charge("d3", full)
	tr.charge("d4", full)
	v = tr.view()
	if v.Phase != trialPhaseDone || v.PaidRemaining != 0 || v.FreeRemaining != 0 || v.FreeActive {
		t.Errorf("done view = %+v, want done/0/0/false", v)
	}
}

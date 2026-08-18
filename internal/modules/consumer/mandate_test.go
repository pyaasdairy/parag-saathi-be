package consumer

import (
	"testing"
	"time"
)

// TestMandateStateMachine pins every edge of the mandate lifecycle: which
// status→status moves are legal and which are refused. cancelled is terminal.
func TestMandateStateMachine(t *testing.T) {
	states := []string{"pending", "active", "paused", "cancelled"}
	// legal[from][to] = true for the allowed edges (everything else must be false).
	legal := map[string]map[string]bool{
		"pending":   {"active": true, "cancelled": true},
		"active":    {"paused": true, "cancelled": true},
		"paused":    {"active": true, "cancelled": true},
		"cancelled": {},
	}
	for _, from := range states {
		for _, to := range states {
			want := legal[from][to]
			if got := mandateCanTransition(from, to); got != want {
				t.Errorf("mandateCanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	// A self-loop is never a transition (idempotent handling lives above the SM).
	for _, s := range states {
		if mandateCanTransition(s, s) {
			t.Errorf("self-transition %q→%q should be illegal", s, s)
		}
	}
	// cancelled is terminal — no outgoing edge at all.
	for _, to := range states {
		if mandateCanTransition("cancelled", to) {
			t.Errorf("cancelled must be terminal, but →%q was allowed", to)
		}
	}
	// An unknown status can never transition anywhere.
	if mandateCanTransition("bogus", "active") {
		t.Error("unknown status must not transition")
	}
}

// TestMandateActionTarget maps the pause/resume/cancel actions to their target
// status, and rejects anything else.
func TestMandateActionTarget(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"pause":   {"paused", true},
		"resume":  {"active", true},
		"cancel":  {"cancelled", true},
		"":        {"", false},
		"delete":  {"", false},
		"suspend": {"", false},
	}
	for action, exp := range cases {
		got, ok := mandateActionTarget(action)
		if got != exp.want || ok != exp.ok {
			t.Errorf("mandateActionTarget(%q) = (%q, %v), want (%q, %v)", action, got, ok, exp.want, exp.ok)
		}
	}
}

// TestActionTargetsAreReachable guards that every action's target is actually a
// legal transition from at least one non-terminal state (the SM and the action
// map can't silently drift apart).
func TestActionTargetsAreReachable(t *testing.T) {
	for _, action := range []string{"pause", "resume", "cancel"} {
		target, _ := mandateActionTarget(action)
		reachable := false
		for _, from := range []string{"pending", "active", "paused"} {
			if mandateCanTransition(from, target) {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("action %q → %q is unreachable in the state machine", action, target)
		}
	}
}

// TestMandateChargeRefIdempotencyKey pins the exactly-once execution key: it is
// STABLE for a given (mandate, IST day) — so two ticks on the same delivery day
// collapse onto one wallet gate row and can't double-charge — and DISTINCT
// across days and across mandates. IST, not UTC: the trial ledger and delivery
// settle key on the consumer's calendar day, and a mandate tick between IST and
// UTC midnight must land on the same day as the delivery it pays for.
func TestMandateChargeRefIdempotencyKey(t *testing.T) {
	// Two instants on the same IST day → same day key → same ref.
	morning := time.Date(2026, 7, 29, 5, 30, 0, 0, time.UTC) // 11:00 IST, 29 Jul
	evening := time.Date(2026, 7, 29, 18, 0, 0, 0, time.UTC) // 23:30 IST, 29 Jul
	if dayKey(morning) != dayKey(evening) {
		t.Fatalf("same-day keys differ: %q vs %q", dayKey(morning), dayKey(evening))
	}
	refA := mandateChargeRef("mnd_abc", dayKey(morning))
	refB := mandateChargeRef("mnd_abc", dayKey(evening))
	if refA != refB {
		t.Errorf("same (mandate, day) must yield the same ref: %q vs %q", refA, refB)
	}
	if refA != "mandate:mnd_abc:2026-07-29" {
		t.Errorf("unexpected ref shape %q", refA)
	}

	// A different day → a different ref (the next charge is a distinct gate row).
	nextDay := morning.Add(24 * time.Hour)
	if r := mandateChargeRef("mnd_abc", dayKey(nextDay)); r == refA {
		t.Errorf("next day must yield a distinct ref, got %q for both", r)
	}
	// A different mandate → a different ref (per-mandate isolation).
	if r := mandateChargeRef("mnd_xyz", dayKey(morning)); r == refA {
		t.Errorf("distinct mandates must not share a ref, got %q for both", r)
	}
	// IST midnight is the boundary, not UTC midnight: 18:29 UTC is 23:59 IST
	// (still 29 Jul), 18:31 UTC is 00:01 IST (already 30 Jul). Same UTC day,
	// different charge days — this is precisely the delivery-day alignment.
	if got := dayKey(time.Date(2026, 7, 29, 18, 29, 0, 0, time.UTC)); got != "2026-07-29" {
		t.Errorf("pre-IST-midnight key = %q, want 2026-07-29", got)
	}
	if got := dayKey(time.Date(2026, 7, 29, 18, 31, 0, 0, time.UTC)); got != "2026-07-30" {
		t.Errorf("post-IST-midnight key = %q, want 2026-07-30", got)
	}
}

// TestNextChargeAfter pins the plan cadence used to advance the schedule.
func TestNextChargeAfter(t *testing.T) {
	from := time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC)
	cases := []struct {
		plan string
		want time.Time
		ok   bool
	}{
		{"daily", from.Add(24 * time.Hour), true},
		{"weekly", from.Add(7 * 24 * time.Hour), true},
		{"monthly", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := nextChargeAfter(c.plan, from)
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("nextChargeAfter(%q) = (%v, %v), want (%v, %v)", c.plan, got, ok, c.want, c.ok)
		}
	}
}

// TestValidateMandate pins the create-time guards: known plan, per-charge amount
// in ₹1..₹5,000, and a max_amount that is at least the charge and at most the
// ₹1,00,000 authorization ceiling.
func TestValidateMandate(t *testing.T) {
	ok := []struct {
		plan              string
		amount, maxAmount float64
	}{
		{"daily", 50, 50},
		{"daily", 50, 1500},
		{"weekly", 350, 5000},
		{"daily", 1, 1},
		{"weekly", 5000, 100000},
	}
	for _, c := range ok {
		if err := validateMandate(c.plan, c.amount, c.maxAmount); err != nil {
			t.Errorf("validateMandate(%q, %v, %v) = %v, want nil", c.plan, c.amount, c.maxAmount, err)
		}
	}
	bad := []struct {
		name              string
		plan              string
		amount, maxAmount float64
	}{
		{"unknown plan", "monthly", 50, 50},
		{"empty plan", "", 50, 50},
		{"zero amount", "daily", 0, 50},
		{"amount over cap", "daily", 5000.01, 6000},
		{"max below amount", "daily", 100, 50},
		{"max over ceiling", "daily", 100, 100000.01},
	}
	for _, c := range bad {
		if err := validateMandate(c.plan, c.amount, c.maxAmount); err == nil {
			t.Errorf("%s: validateMandate(%q, %v, %v) = nil, want error", c.name, c.plan, c.amount, c.maxAmount)
		}
	}
}

package consumer

import (
	"testing"
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

// TestAutopayRefAndAmount pins a Smart Recharge charge's exactly-once key
// (its reason, the mandate and the occasion) and its amount: the member's
// recharge amount, or the shortfall when that is more, never above the
// per-debit cap the bank approved.
func TestAutopayRefAndAmount(t *testing.T) {
	if got := autopayRef(autopayReasonThreshold, "mnd_abc", "2026-10-05-1"); got != "autopay:threshold:mnd_abc:2026-10-05-1" {
		t.Errorf("ref shape %q", got)
	}
	if autopayRef(autopayReasonApp, "mnd_abc", "r1") == autopayRef(autopayReasonSeat, "mnd_abc", "r1") ||
		autopayRef(autopayReasonApp, "mnd_abc", "r1") == autopayRef(autopayReasonApp, "mnd_xyz", "r1") {
		t.Error("refs must be distinct across reasons and mandates")
	}
	m := &mandate{Amount: 500, MaxAmount: 2000}
	for _, c := range []struct{ short, want float64 }{
		{0, 500}, {120, 500}, {740.2, 741}, {1999.5, 2000}, {5000, 2000},
	} {
		if got := autopayAmountFor(m, c.short); got != c.want {
			t.Errorf("autopayAmountFor(short %v) = %v, want %v", c.short, got, c.want)
		}
	}
	if got := (&mandate{}).effectiveThreshold(); got != autopayDefaultThreshold {
		t.Errorf("default threshold %v", got)
	}
	if got := (&mandate{Threshold: 350}).effectiveThreshold(); got != 350 {
		t.Errorf("member threshold %v", got)
	}
	// The horizon must outlast the UPI lead, or a charge started when a day
	// enters it could land after that day's noon lock.
	if autopayFundingHorizon <= autopayChargeLead {
		t.Errorf("horizon %v must exceed the charge lead %v", autopayFundingHorizon, autopayChargeLead)
	}
	if rupeesToPaise(19.99) != 1999 || rupeesToPaise(0.1+0.2) != 30 {
		t.Error("rupeesToPaise rounding")
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

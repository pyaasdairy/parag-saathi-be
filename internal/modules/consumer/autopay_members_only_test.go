package consumer

import (
	"context"
	"testing"
)

// Founder decisions 1 and 2 together: with FOUNDING_PYAAS_MEMBERS_ONLY on, a
// PYAAS milk morning the member's perks do not cover is never delivered (the
// noon lock pauses the plan and funds nothing, pyaasPlanGate), so Smart
// Recharge must not charge the bank to fund it, exactly as the CRM wallet
// sums (B-01, B-02) already leave it out. A Parag plan, a morning the lock
// already decided, and the same plan with the switch off still count.
func TestAutopayNeedLeavesOutAPyaasMorningMembersOnlyRefuses(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedPyaasToned(t, w)
	nm := w.customer(t, "9000013111", 1000)
	for _, p := range []subscriptionInput{
		{ProductID: "pyaas-toned-1l", Qty: 1, Frequency: "daily", StartDate: "2026-10-05"},
		{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, UnitPrice: 35, Frequency: "daily", StartDate: "2026-10-05"},
	} {
		if _, err := w.svc.createSubscriptionAt(ctx, nm, p, chainPlanMadeAt); err != nil {
			t.Fatalf("subscription %s: %v", p.ProductID, err)
		}
	}
	need := func(at string, h int) float64 {
		t.Helper()
		n, err := w.svc.autopayNeed(ctx, nm, istDayAt(at, h, 0))
		if err != nil {
			t.Fatalf("need: %v", err)
		}
		return n
	}

	// 11:00 on 5 Oct, no preview yet: the horizon holds 6 and 7 Oct. PYAAS
	// Rs 85 + Parag 2 x 35 a day.
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = false
	if got := need("2026-10-05", 11); got != 310 {
		t.Fatalf("need with the switch off: %v, want 310 (2 x (85 + 70))", got)
	}
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = true
	if got := need("2026-10-05", 11); got != 140 {
		t.Fatalf("need with the switch on: %v, want 140 (Parag only; the PYAAS mornings are refused)", got)
	}

	// 13:00, after the lock ran with the switch off: 6 Oct is locked (the
	// member is billed it), 7 Oct previewed, 8 Oct entered the horizon.
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = false
	w.svc.sweepSubscriptionOrders(ctx, istDayAt("2026-10-05", 13, 0))
	if got := need("2026-10-05", 13); got != 465 {
		t.Fatalf("need after the lock, switch off: %v, want 465 (3 x (85 + 70))", got)
	}
	// Switched on now: the locked 6 Oct still counts; the PYAAS preview of
	// 7 Oct (the lock will refuse it) and the PYAAS 8 Oct do not.
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = true
	if got := need("2026-10-05", 13); got != 295 {
		t.Fatalf("need after the lock, switch on: %v, want 295 (85 + 3 x 70)", got)
	}
}

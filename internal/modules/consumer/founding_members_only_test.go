package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// Spec 5.1 once the founder switches FOUNDING_PYAAS_MEMBERS_ONLY on at
// launch: PYAAS milk goes only to a member whose perks cover the morning.
// createOrder and createSubscription already refused; a plan made while the
// member's perks ran now stops delivering PYAAS milk once they end (no
// preview, and a preview already made is not locked), and a paused PYAAS
// plan cannot be resumed without them. Parag plans are never touched, and
// with the switch off nothing changes.
func TestMembersOnlyKeepsPyaasMorningsForMembers(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	p := 85.0
	if _, err := w.db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
		SkuID: "pyaas-toned-1l", Kind: catalogKindProduct, Price: &p, Name: "Toned Milk - PYAAS", Category: "milk", Unit: "1 L", Variant: "1L Carton",
	}); err != nil {
		t.Fatalf("seed pyaas: %v", err)
	}
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = true
	defer func() { w.svc.deps.Cfg.FoundingPyaasMembersOnly = false }()
	seedTestFarms(t, w, 1)
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	long := istDayAt(addDaysIST(D, -2), 9, 0)

	m := w.customer(t, "9000019401", 2000)
	if _, err := w.svc.joinFoundingFamily(ctx, m, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	pyaasPlan := walletLockPlan(t, w, m, "pyaas-toned-1l", 1, D1, long)
	paragPlan := walletLockPlan(t, w, m, "gold-500ml", 1, D1, long)
	setPerksUntil := func(day string) {
		t.Helper()
		if _, err := w.db.Collection(collFoundingMembers).UpdateOne(ctx, bson.D{{Key: "consumer_id", Value: m}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: memberStopped}, {Key: "perks_until", Value: day}}}}); err != nil {
			t.Fatalf("perks_until: %v", err)
		}
	}
	// Stopped, the paid month running past D1: tomorrow's PYAAS morning is
	// previewed and locked as usual.
	setPerksUntil(D1)
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	assertLocked(t, w, pyaasPlan, D1)
	assertLocked(t, w, paragPlan, D1)

	// D2 is past the paid month: previewed while it still ran (the month was
	// cut short after the preview), it is not locked; Parag is.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 9, 0))
	if o := liveSubOrder(t, w, pyaasPlan.SubscriptionID, D2); o != nil {
		t.Fatalf("a PYAAS morning past the paid month was previewed: %+v", o)
	}
	setPerksUntil(D2) // a preview for D2 now appears ...
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 9, 30))
	if o := liveSubOrder(t, w, pyaasPlan.SubscriptionID, D2); o == nil {
		t.Fatalf("a PYAAS morning inside the paid month was not previewed")
	}
	setPerksUntil(D1) // ... and the month is cut back before the lock
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 5))
	if o := liveSubOrder(t, w, pyaasPlan.SubscriptionID, D2); o != nil {
		t.Fatalf("a PYAAS morning past the paid month was locked: %+v", o)
	}
	for _, o := range subOrdersFor(t, w, pyaasPlan.SubscriptionID, D2) {
		if task, _ := w.svc.repo.findDeliveryByOrder(ctx, o.OrderID); task != nil || o.Status != "cancelled" {
			t.Fatalf("a refused morning left an order %s (%s) or a store task", o.OrderID, o.Status)
		}
	}
	assertLocked(t, w, paragPlan, D2)

	// A paused PYAAS plan cannot be resumed without the perks; Parag can.
	at := istDayAt(D1, 13, 0)
	if _, err := w.svc.setSubscriptionStatusAt(ctx, m, pyaasPlan.SubscriptionID, "pause", at); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, err := w.svc.setSubscriptionStatusAt(ctx, m, pyaasPlan.SubscriptionID, "resume", at.Add(time.Minute))
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "FOUNDING_REQUIRED" {
		t.Fatalf("resume without the perks: %v", err)
	}
	if _, err := w.svc.setSubscriptionStatusAt(ctx, m, paragPlan.SubscriptionID, "pause", at); err != nil {
		t.Fatalf("pause parag: %v", err)
	}
	if _, err := w.svc.setSubscriptionStatusAt(ctx, m, paragPlan.SubscriptionID, "resume", at.Add(time.Minute)); err != nil {
		t.Fatalf("resume parag: %v", err)
	}

	// With the switch off, the same plan delivers as it always did.
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = false
	if _, err := w.svc.setSubscriptionStatusAt(ctx, m, pyaasPlan.SubscriptionID, "resume", at.Add(2*time.Minute)); err != nil {
		t.Fatalf("resume with the switch off: %v", err)
	}
}

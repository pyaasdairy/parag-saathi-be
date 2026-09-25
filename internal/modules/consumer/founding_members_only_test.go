package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// seedPyaasToned puts the reference PYAAS 1 L line on the test catalog.
func seedPyaasToned(t *testing.T, w *chainWorld) {
	t.Helper()
	p := 85.0
	if _, err := w.db.Collection(collCatalog).InsertOne(context.Background(), catalogDoc{
		SkuID: "pyaas-toned-1l", Kind: catalogKindProduct, Price: &p, Name: "Toned Milk - PYAAS", Category: "milk", Unit: "1 L", Variant: "1L Carton",
	}); err != nil {
		t.Fatalf("seed pyaas: %v", err)
	}
}

// planPausedEvents is every founding.pyaas_plan_paused a member was sent.
func planPausedEvents(t *testing.T, w *chainWorld, cid primitive.ObjectID) []crmEvent {
	t.Helper()
	cur, err := w.db.Collection(collCRMEvents).Find(context.Background(),
		bson.D{{Key: "topic", Value: "founding.pyaas_plan_paused"}, {Key: "consumer_id", Value: cid}})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []crmEvent
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("events decode: %v", err)
	}
	return out
}

func mustSub(t *testing.T, w *chainWorld, subID string) *subscription {
	t.Helper()
	s, err := w.svc.repo.findSubscriptionByID(context.Background(), subID)
	if err != nil || s == nil {
		t.Fatalf("subscription %s: %v", subID, err)
	}
	return s
}

// Spec 5.1 once the founder switches FOUNDING_PYAAS_MEMBERS_ONLY on at
// launch: PYAAS milk goes only to a member whose perks cover the morning.
// createOrder and createSubscription already refused; a plan made while the
// member's perks ran stops delivering PYAAS milk once they end, and it stops
// visibly: the first morning refused (no preview, or a preview the lock will
// not fund) pauses the plan with pause_reason founding_required and records
// founding.pyaas_plan_paused once. A morning the perks still cover goes out.
// A paused PYAAS plan cannot be resumed without the perks, and can be once
// they cover the next morning. Parag plans are never touched, and with the
// switch off nothing changes.
func TestMembersOnlyKeepsPyaasMorningsForMembers(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedPyaasToned(t, w)
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
	// Stopped, the paid month running through D1: D1's PYAAS morning is
	// previewed and locked as usual.
	setPerksUntil(D1)
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	assertLocked(t, w, pyaasPlan, D1)
	assertLocked(t, w, paragPlan, D1)

	// D2 is past the paid month. At the same noon the next open morning is
	// D2: the PYAAS plan is paused (founding_required) instead of silently
	// skipping, the member is told once, and D1, which the month covers,
	// still goes out.
	if s := mustSub(t, w, pyaasPlan.SubscriptionID); s.Status != "paused" || s.PauseReason != pauseReasonFoundingRequired {
		t.Fatalf("the plan past the paid month: status %s reason %q, want paused founding_required", s.Status, s.PauseReason)
	}
	if o := liveSubOrder(t, w, pyaasPlan.SubscriptionID, D2); o != nil {
		t.Fatalf("a PYAAS morning past the paid month was previewed: %+v", o)
	}
	assertLocked(t, w, pyaasPlan, D1)
	evs := planPausedEvents(t, w, m)
	if len(evs) != 1 || evs[0].Payload["day"] != D2 || evs[0].Payload["subscription_id"] != pyaasPlan.SubscriptionID ||
		evs[0].Payload["reason"] != pauseReasonFoundingRequired || evs[0].Payload["member_status"] != memberStopped {
		t.Fatalf("founding.pyaas_plan_paused: %+v", evs)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 9, 0))
	if n := len(planPausedEvents(t, w, m)); n != 1 {
		t.Fatalf("a later tick told the member again: %d events", n)
	}
	if s := mustSub(t, w, paragPlan.SubscriptionID); s.Status != "active" || s.PauseReason != "" {
		t.Fatalf("the Parag plan was touched: %s %q", s.Status, s.PauseReason)
	}

	// The perks come back before D2's noon (the month runs through D2
	// again): the member resumes and D2 is previewed at once. The month is
	// then cut back before the lock: the lock refuses D2 (cancelled, no
	// store task) and pauses the plan again, telling the member again.
	setPerksUntil(D2)
	if _, err := w.svc.setSubscriptionStatusAt(ctx, m, pyaasPlan.SubscriptionID, "resume", istDayAt(D1, 9, 30)); err != nil {
		t.Fatalf("resume with the perks back: %v", err)
	}
	if o := liveSubOrder(t, w, pyaasPlan.SubscriptionID, D2); o == nil {
		t.Fatalf("a PYAAS morning inside the paid month was not previewed on resume")
	}
	setPerksUntil(D1)
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D1, 12, 5))
	if o := liveSubOrder(t, w, pyaasPlan.SubscriptionID, D2); o != nil {
		t.Fatalf("a PYAAS morning past the paid month was locked: %+v", o)
	}
	for _, o := range subOrdersFor(t, w, pyaasPlan.SubscriptionID, D2) {
		if task, _ := w.svc.repo.findDeliveryByOrder(ctx, o.OrderID); task != nil || o.Status != "cancelled" {
			t.Fatalf("a refused morning left an order %s (%s) or a store task", o.OrderID, o.Status)
		}
	}
	if s := mustSub(t, w, pyaasPlan.SubscriptionID); s.Status != "paused" || s.PauseReason != pauseReasonFoundingRequired {
		t.Fatalf("the lock's refusal: status %s reason %q", s.Status, s.PauseReason)
	}
	if evs := planPausedEvents(t, w, m); len(evs) != 2 || evs[1].Payload["day"] != D2 {
		t.Fatalf("the second pause: %+v", evs)
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
	s, err := w.svc.setSubscriptionStatusAt(ctx, m, pyaasPlan.SubscriptionID, "resume", at.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("resume with the switch off: %v", err)
	}
	if s.Status != "active" || s.PauseReason != "" {
		t.Fatalf("resumed plan: %s %q", s.Status, s.PauseReason)
	}
}

// Launch day (RV-PR-01): the founder switches FOUNDING_PYAAS_MEMBERS_ONLY on
// over a non-member's PYAAS plan made while it was off. The plan no longer
// stays "active" with a next delivery date for milk that never comes: it is
// paused (founding_required), shows no next delivery, and the member is told
// once. Their Parag plan delivers as before.
func TestMembersOnlySwitchedOnPausesANonMemberPlanAndSaysSo(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedPyaasToned(t, w)
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -2), 9, 0)
	nm := w.customer(t, "9000019411", 2000)
	plan := walletLockPlan(t, w, nm, "pyaas-toned-1l", 1, D1, long) // made while the switch is off
	parag := walletLockPlan(t, w, nm, "gold-500ml", 1, D1, long)

	w.svc.deps.Cfg.FoundingPyaasMembersOnly = true
	defer func() { w.svc.deps.Cfg.FoundingPyaasMembersOnly = false }()
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))

	fresh := mustSub(t, w, plan.SubscriptionID)
	if fresh.Status != "paused" || fresh.PauseReason != pauseReasonFoundingRequired {
		t.Fatalf("launch day: status %s reason %q, want paused founding_required", fresh.Status, fresh.PauseReason)
	}
	if next := w.svc.nextDeliveryFor(ctx, fresh, istDayAt(D, 12, 5)); next != "" {
		t.Fatalf("a paused plan still shows next_delivery_date %s", next)
	}
	if o := liveSubOrder(t, w, plan.SubscriptionID, D1); o != nil {
		t.Fatalf("a refused PYAAS morning is live: %+v", o)
	}
	evs := planPausedEvents(t, w, nm)
	if len(evs) != 1 || evs[0].Payload["day"] != D1 || evs[0].Payload["product_id"] != "pyaas-toned-1l" || evs[0].Payload["member_status"] != "" {
		t.Fatalf("founding.pyaas_plan_paused: %+v", evs)
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 20))
	if n := len(planPausedEvents(t, w, nm)); n != 1 {
		t.Fatalf("told %d times, want once", n)
	}
	assertLocked(t, w, parag, D1)
	if s := mustSub(t, w, parag.SubscriptionID); s.Status != "active" {
		t.Fatalf("the Parag plan: %s", s.Status)
	}
	// The member's resume is refused until the perks cover the next morning.
	_, err := w.svc.setSubscriptionStatusAt(ctx, nm, plan.SubscriptionID, "resume", istDayAt(D, 13, 0))
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "FOUNDING_REQUIRED" {
		t.Fatalf("resume as a non-member: %v", err)
	}
}

// The CRM wallet sums (RV-PR-01) leave out a PYAAS plan the switch refuses:
// no B-01 "top up" and no B-02 cost for milk the lock will not fund. With the
// switch off the same plan counts as before.
func TestMembersOnlyGatedPlanIsLeftOutOfTheCRMWalletSums(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedPyaasToned(t, w)
	nm := w.customer(t, "9000019421", 100) // Rs 100 against Rs 85 a day: 1.2 days of cover
	today := istToday(time.Now())
	plan := walletLockPlan(t, w, nm, "pyaas-toned-1l", 1, addDaysIST(today, -3), istDayAt(addDaysIST(today, -5), 9, 0))
	day := addDaysIST(today, 2)

	w.svc.deps.Cfg.FoundingPyaasMembersOnly = true
	if c := w.svc.crmPlanCostOn(ctx, plan, day); c != 0 {
		t.Fatalf("B-02 cost of a refused PYAAS morning: %v, want 0", c)
	}
	at := istDayAt(today, 10, 40)
	w.svc.crmSweepWalletCover(ctx, at)
	if n := inboxCount(t, w.db, nm, "B-01"); n != 0 {
		t.Fatalf("B-01 asked a non-member to top up for PYAAS milk the switch stops: %d", n)
	}

	w.svc.deps.Cfg.FoundingPyaasMembersOnly = false
	if c := w.svc.crmPlanCostOn(ctx, plan, day); c != 85 {
		t.Fatalf("B-02 cost with the switch off: %v, want 85", c)
	}
	w.svc.crmSweepWalletCover(ctx, at)
	if n := inboxCount(t, w.db, nm, "B-01"); n != 1 {
		t.Fatalf("B-01 with the switch off: %d, want 1", n)
	}
}

// The owner's pre-launch list (RV-PR-01): before FOUNDING_PYAAS_MEMBERS_ONLY
// goes on, GET /consumer/admin/founding/members-only-impact names every
// active PYAAS milk plan it will stop - a non-member's, and a stopped
// member's once the paid month has run out - and never a Parag plan or an
// active member's PYAAS plan. Admin key only.
func TestMembersOnlyImpactListsThePlansTheSwitchWillStop(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedPyaasToned(t, w)
	seedTestFarms(t, w, 1)
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -2), 9, 0)

	nm := w.customer(t, "9000019431", 2000)
	nmPlan := walletLockPlan(t, w, nm, "pyaas-toned-1l", 1, D1, long)
	walletLockPlan(t, w, nm, "gold-500ml", 1, D1, long)
	am := w.customer(t, "9000019432", 2000)
	if _, err := w.svc.joinFoundingFamily(ctx, am, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	walletLockPlan(t, w, am, "pyaas-toned-1l", 1, D1, long)
	sm := w.customer(t, "9000019433", 2000)
	// gonard-dairy unlocked with am's join (unlocksAt 1), so sm joins the farm
	// still filling; the stop below is what the list judges.
	if _, err := w.svc.joinFoundingFamily(ctx, sm, "mishra-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	smPlan := walletLockPlan(t, w, sm, "pyaas-toned-1l", 1, D1, long)
	if _, err := w.db.Collection(collFoundingMembers).UpdateOne(ctx, bson.D{{Key: "consumer_id", Value: sm}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: memberStopped}, {Key: "perks_until", Value: D}}}}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	rows, err := w.svc.pyaasPlansMembersOnlyStops(ctx, D1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.sub.SubscriptionID] = r.memberStatus
	}
	if len(got) != 2 || got[nmPlan.SubscriptionID] != "" || got[smPlan.SubscriptionID] != memberStopped {
		t.Fatalf("plans the switch stops on %s: %v", D1, got)
	}
	if _, listed := got[nmPlan.SubscriptionID]; !listed {
		t.Fatalf("the non-member's PYAAS plan is missing: %v", got)
	}

	t.Setenv("ADMIN_API_KEY", strings.Repeat("k", adminKeyMinLen))
	r := chi.NewRouter()
	registerAdminCRM(r, &handler{svc: w.svc}, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/founding/members-only-impact?day="+D1, nil)
	req.Header.Set("X-Admin-Key", strings.Repeat("k", adminKeyMinLen))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET members-only-impact: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Day      string           `json:"day"`
			SwitchOn bool             `json:"switchOn"`
			Total    int              `json:"total"`
			Items    []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Data.Day != D1 || out.Data.SwitchOn || out.Data.Total != 2 || len(out.Data.Items) != 2 {
		t.Fatalf("impact view: %s", rec.Body.String())
	}
	for _, it := range out.Data.Items {
		if it["productId"] != "pyaas-toned-1l" || it["customerPhone"] == nil || it["status"] != "active" {
			t.Fatalf("impact row: %v", it)
		}
	}
	bad := httptest.NewRequest(http.MethodGet, "/admin/founding/members-only-impact?day=tomorrow", nil)
	bad.Header.Set("X-Admin-Key", strings.Repeat("k", adminKeyMinLen))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a malformed day: %d", rec.Code)
	}
	noKey := httptest.NewRequest(http.MethodGet, "/admin/founding/members-only-impact", nil)
	noKey.Header.Set("X-Admin-Key", "wrong")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, noKey)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without the admin key: %d", rec.Code)
	}
}

package consumer

// The config's conditions match what the emitters send: every condition line
// of a live event trigger parses on a known fact, E-04/E-05 read the app's
// complaint enum, A-05 and E-07 evaluate at fire time, and W-01 routes on
// the topic the enrolment really emits.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMCond -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// crmKnownFactKeys are the condition keys crmEventCtx.fact resolves. Adding
// a key there means adding it here, so a config line on an unknown key is
// caught in CI rather than failing closed in production.
var crmKnownFactKeys = map[string]bool{
	"order.is_promotional_only": true, "product_line": true, "orders_count": true, "complaint.type": true,
	"new_eta_known": true, "rating": true, "offer.id": true, "offer.entitled_free_deliveries_remaining": true,
	"wallet.topup_balance": true, "wallet.covers_first_cycle": true, "change": true, "complaint_open": true,
	"before_delivery": true, "serviceability.in_zone": true, "credit.account": true, "order.contains_promotional_line": true,
	"member.started": true, "complaint.refundable_amount": true, "complaint.has_order": true,
	"tomorrow.delivery_blocked": true, "offer.pack1_tomorrow": true,
	"serviceability.has_orders": true, "serviceability.has_serviceable_address": true,
}

func TestCRMCondConfigLinesParseOnKnownFacts(t *testing.T) {
	cfg := crmConfigLoad()
	for id, tr := range cfg.Triggers {
		if _, waiting := cfg.AwaitingEvent[id]; waiting {
			if tr.Kind == "alias" {
				t.Errorf("%s: an alias is not awaiting an event", id)
			}
			continue
		}
		if tr.Kind != "event" || tr.Category == "internal" {
			continue // scheduled W/B triggers are hand-coded; internal ones bypass conditions
		}
		if tr.Event == "wallet.recharge_settled" {
			continue // W-04: crmOnRechargeSettled owns it, the generic router never sees the topic
		}
		lines := append([]string{}, tr.Conditions...)
		if tr.Template.If != "" {
			lines = append(lines, tr.Template.If)
		}
		for _, line := range lines {
			c, err := crmParseCondition(line)
			if err != nil {
				t.Errorf("%s: condition %q does not parse: %v", id, line, err)
				continue
			}
			if c.op != "literal" && !crmKnownFactKeys[c.key] {
				t.Errorf("%s: condition %q reads unknown fact %q", id, line, c.key)
			}
		}
	}
	for id := range cfg.AwaitingEvent {
		if _, ok := cfg.Triggers[id]; !ok {
			t.Errorf("meta.awaiting_event names %s, which is not a trigger", id)
		}
	}
	if cfg.Triggers["W-01"].Event != "offer.finalized" {
		t.Fatalf("W-01 must route on the topic enrolment emits, got %q", cfg.Triggers["W-01"].Event)
	}
	if got := cfg.Triggers["E-04"].Conditions; len(got) != 2 || got[0] != "complaint.type in ['missing']" || got[1] != "complaint.has_order == true" {
		t.Fatalf("E-04 conditions: %v", got)
	}
	if got := cfg.Triggers["E-05"].Conditions; len(got) != 1 || got[0] != "complaint.type == 'quality'" {
		t.Fatalf("E-05 conditions: %v", got)
	}
}

func TestCRMCondComplaintTypesMatchTheApp(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007501", 500)
	o := instantOrderDelivered(t, w, cid)

	file := func(ref, category string) {
		t.Helper()
		if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: ref, Category: category, OrderID: o.OrderID, Detail: "x"}); err != nil {
			t.Fatalf("fileComplaint %s: %v", category, err)
		}
	}
	file("PYS-M1", "missing")
	file("PYS-L1", "late")
	w.svc.crmProcessEvents(ctx)
	e02 := crmDispatchScopes(t, w.db, cid, "E-02")
	if len(e02) != 2 {
		t.Fatalf("E-02 acknowledges every complaint (scoped by complaint id): %v", e02)
	}
	e04 := crmDispatchScopes(t, w.db, cid, "E-04")
	if len(e04) != 1 {
		t.Fatalf("E-04 fires for 'missing' only: %v", e04)
	}
	var missing complaint
	if err := w.db.Collection(collComplaints).FindOne(ctx, bson.D{{Key: "ref", Value: "PYS-M1"}}).Decode(&missing); err != nil {
		t.Fatalf("missing complaint: %v", err)
	}
	if e04[missing.ID] != "SENT" {
		t.Fatalf("E-04 scope must be the missing complaint's id: %v", e04)
	}
	// An order-less 'missing' complaint has nothing to redeliver: E-04's
	// complaint.has_order condition keeps it silent (E2E-03).
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-M2", Category: "missing", Detail: "no order"}); err != nil {
		t.Fatalf("fileComplaint order-less: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	if e04 = crmDispatchScopes(t, w.db, cid, "E-04"); len(e04) != 1 {
		t.Fatalf("an order-less missing complaint must not send E-04: %v", e04)
	}
	if e02 = crmDispatchScopes(t, w.db, cid, "E-02"); len(e02) != 3 {
		t.Fatalf("E-02 still acknowledges it: %v", e02)
	}
}

func TestCRMCondUnpaidSubscriptionRecheckedAtFire(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	nowIST := time.Now().In(istZone)
	t0 := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 15, 0, 0, 0, istZone)

	subscribe := func(cid primitive.ObjectID, product string) {
		t.Helper()
		if _, err := w.svc.createSubscription(ctx, cid, subscriptionInput{
			ProductID: product, Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: addDaysIST(istToday(time.Now()), 1),
		}); err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
	}
	stillUnpaid := w.customer(t, "9000007502", 0)
	paysLater := w.customer(t, "9000007503", 0)
	subscribe(stillUnpaid, "gold-500ml")
	subscribe(paysLater, "gold-500ml")
	// A Welcome Litre household with a free delivery still owed adds a plan
	// through the app with an empty wallet: CH-02 says no nudge.
	res, err := w.svc.crmEnrol(ctx, "cond-test-operator", crmEnrolInput{
		Phone: "9000007504", Name: "Campaign Household", Line1: "Flat 4, Cond Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	campaign, _ := primitive.ObjectIDFromHex(res.ConsumerID)
	// A second milk: the campaign plan already holds its own line, and the
	// DUPLICATE_SUBSCRIPTION guard refuses a second plan on it.
	subscribe(campaign, "taaza-500ml")

	w.svc.crmProcessEventsAt(ctx, t0)
	for _, cid := range []primitive.ObjectID{stillUnpaid, paysLater} {
		if rows := crmScheduleRows(t, w.db, cid, "A-05"); len(rows) != 1 {
			t.Fatalf("A-05 must be queued (PT2H): %+v", rows)
		}
	}
	// The campaign household is refused at enqueue (a free delivery is owed):
	// no schedule row, nothing to re-check later.
	if rows := crmScheduleRows(t, w.db, campaign, "A-05"); len(rows) != 0 {
		t.Fatalf("A-05 must not even be queued while a free delivery is owed: %+v", rows)
	}
	if _, err := w.svc.creditTopup(ctx, paysLater, 500, "razorpay", "cond-topup"); err != nil {
		t.Fatalf("creditTopup: %v", err)
	}
	w.svc.crmProcessSchedules(ctx, t0.Add(2*time.Hour))
	if got := crmDispatchStatuses(t, w.db, stillUnpaid, "A-05"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("A-05 for the still-unpaid plan: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, paysLater, "A-05"); len(got) != 0 {
		t.Fatalf("A-05 must not nag a member who topped up inside the delay: %v", got)
	}
	if rows := crmScheduleRows(t, w.db, paysLater, "A-05"); rows[0].Status != "SKIPPED" {
		t.Fatalf("paid-in-time row: %+v", rows[0])
	}
	if got := crmDispatchStatuses(t, w.db, campaign, "A-05"); len(got) != 0 {
		t.Fatalf("A-05 must not fire while a free delivery is owed (CH-02): %v", got)
	}
	// The campaign plan itself is announced by W-01, never A-03.
	if got := crmDispatchStatuses(t, w.db, campaign, "W-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("W-01 through the generic router on offer.finalized: %v", got)
	}
	if n := len(crmEventsOf(t, w.db, campaign, "subscription.activated")); n != 1 {
		t.Fatalf("only the app-created plan emits subscription.activated: %d", n)
	}
}

func TestCRMCondReferralAskAtFire(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	nowIST := time.Now().In(istZone)
	t0 := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 14, 0, 0, 0, istZone)
	yes := true
	consent := func(cid primitive.ObjectID) {
		t.Helper()
		if aerr := w.svc.applyConsents(ctx, cid, []consentInput{{Type: "marketing_whatsapp", Granted: &yes, OccurredAt: time.Now().UTC()}}); aerr != nil {
			t.Fatalf("applyConsents: %v", aerr)
		}
	}
	rate := func(cid primitive.ObjectID) *order {
		t.Helper()
		o := instantOrderDelivered(t, w, cid)
		if _, err := w.svc.reviewOrder(ctx, cid.Hex(), o.OrderID, 5, "lovely"); err != nil {
			t.Fatalf("reviewOrder: %v", err)
		}
		return o
	}

	happy := w.customer(t, "9000007505", 500)
	consent(happy)
	rate(happy)
	complaining := w.customer(t, "9000007506", 500)
	consent(complaining)
	co := rate(complaining)
	cancelled := w.customer(t, "9000007507", 500)
	consent(cancelled)
	xo := rate(cancelled)
	noConsent := w.customer(t, "9000007508", 500)
	rate(noConsent)

	w.svc.crmProcessEventsAt(ctx, t0)
	for _, cid := range []primitive.ObjectID{happy, complaining, cancelled, noConsent} {
		if got := crmDispatchStatuses(t, w.db, cid, "E-07"); len(got) != 0 {
			t.Fatalf("E-07 waits PT4H: %v", got)
		}
		if rows := crmScheduleRows(t, w.db, cid, "E-07"); len(rows) != 1 {
			t.Fatalf("E-07 queued: %+v", rows)
		}
	}
	// Meanwhile: one member complains, one member's order is cancelled.
	if _, err := w.svc.fileComplaint(ctx, complaining, complaintInput{Ref: "PYS-E7", Category: "late", OrderID: co.OrderID, Detail: "late"}); err != nil {
		t.Fatalf("fileComplaint: %v", err)
	}
	if _, err := w.db.Collection(collOrders).UpdateOne(ctx, bson.D{{Key: "order_id", Value: xo.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}}}}); err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	w.svc.crmProcessSchedules(ctx, t0.Add(4*time.Hour)) // 18:00 IST, inside promotional hours
	if got := crmDispatchStatuses(t, w.db, happy, "E-07"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("E-07 for a consenting five-star member: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, complaining, "E-07"); len(got) != 0 {
		t.Fatalf("E-07 must hold while a complaint is open: %v", got)
	}
	if rows := crmScheduleRows(t, w.db, complaining, "E-07"); rows[0].Status != "SKIPPED" || rows[0].Reason != "conditions no longer hold at fire" {
		t.Fatalf("complaint_open re-check: %+v", rows[0])
	}
	if rows := crmScheduleRows(t, w.db, cancelled, "E-07"); rows[0].Status != "SKIPPED" || rows[0].Reason != "order "+xo.OrderID+" cancelled before the delay elapsed" {
		t.Fatalf("stale order re-check: %+v", rows[0])
	}
	// Without consent the guard chain suppresses it, on record.
	if got := crmDispatchStatuses(t, w.db, noConsent, "E-07"); len(got) != 1 || got[0] != "SUPPRESSED" {
		t.Fatalf("E-07 without consent: %v", got)
	}
	var row crmDispatchRow
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{{Key: "consumer_id", Value: noConsent}, {Key: "trigger_id", Value: "E-07"}}).Decode(&row); err != nil || row.Guard != "G2_consent" {
		t.Fatalf("E-07 suppression guard: %+v %v", row, err)
	}
}

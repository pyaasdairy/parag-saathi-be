package consumer

// THE CRM MATRIX: every trigger in crm_triggers.json that is not listed in
// meta.awaiting_event fires end to end. An event trigger is driven by
// emitting ITS OWN topic (the trigger's "event") with the payload its real
// emitter writes, then draining the real worker; a scheduled or explicit
// Welcome Litre trigger is driven through the product flow that fires it.
// Each one must leave what the member (or the operator) sees: an inbox row
// with no unresolved token, a call-back row (human_call), or the operator's
// record (internal triggers).
//
// The coverage check is the guard for the future: a trigger added to the
// config with no scenario here and no meta.awaiting_event entry fails this
// test, and so does an event trigger whose topic nothing in the backend
// emits. Adding a message is therefore: config, then (only for a new topic)
// one emitter, then one line here (docs/CRM-MESSAGES.md "How to add a
// message").
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMMatrix -v

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// crmEmitCallRe matches an emitCRMEvent call site and captures its literal
// topic. Every emitter in the package passes the topic as a string literal.
var crmEmitCallRe = regexp.MustCompile(`emitCRMEvent\(\s*[^,]+,\s*"([^"]+)"`)

// crmEmittedTopics is every outbox topic some backend code path really
// emits: read from the emitCRMEvent call sites in this package's non-test
// sources, never from a hand-kept list (a topic added to crmLifecycleTopics
// without its emitter must fail the matrix). An event trigger on any other
// topic has no emitter.
func crmEmittedTopics() map[string]bool {
	out := map[string]bool{}
	ents, err := os.ReadDir(".")
	if err != nil {
		return out // nothing readable: every event trigger reports "no emitter"
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		for _, m := range crmEmitCallRe.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// crmMatrixUncovered lists every problem the coverage rule finds in a config:
// a live trigger (not an alias, not awaiting an event) with no scenario, an
// event trigger on a topic nothing emits, and an awaiting_event entry that
// names no trigger.
func crmMatrixUncovered(cfg *crmConfig, covered map[string]bool) []string {
	emitted := crmEmittedTopics()
	var out []string
	for id, t := range cfg.Triggers {
		if t.Kind == "alias" {
			continue
		}
		if _, waiting := cfg.AwaitingEvent[id]; waiting {
			continue
		}
		if !covered[id] {
			out = append(out, id+": no matrix scenario and no meta.awaiting_event entry")
		}
		if t.Kind == "event" && !emitted[t.Event] {
			out = append(out, id+": topic "+t.Event+" has no emitter")
		}
	}
	for id := range cfg.AwaitingEvent {
		if _, ok := cfg.Triggers[id]; !ok {
			out = append(out, id+": meta.awaiting_event names a trigger that does not exist")
		}
	}
	sort.Strings(out)
	return out
}

// What a fired trigger leaves behind.
const (
	crmMatrixInbox    = "inbox"    // an inbox row for the member
	crmMatrixCallback = "callback" // a crm_callbacks row (human_call), no inbox row
	crmMatrixAdmin    = "admin"    // the operator's notifications bell
	crmMatrixDispatch = "dispatch" // a SENT dispatch row under the zero consumer (W-10)
)

type crmMatrixEvent struct {
	// setup returns the member and the payload its real emitter writes; it
	// may stage the rows the trigger's conditions read (an order, a consent).
	setup  func(t *testing.T) (primitive.ObjectID, map[string]any)
	expect string
}

func TestCRMMatrixEveryLiveTriggerFires(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cfg := crmConfigLoad()

	nowIST := time.Now().In(istZone)
	at := func(h, m int) time.Time {
		return time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), h, m, 0, 0, istZone)
	}
	t0 := at(11, 0) // inside the promotional window, so E-07 is judged on consent alone
	label := "Full Cream Milk - Parag Gold 500ml" + crmLabelledSuffix
	phone := 9000009000
	member := func(fund float64) primitive.ObjectID {
		phone++
		return w.customer(t, strconv.Itoa(phone), fund)
	}
	// stageOrder stages an order row the conditions read (lane, delivered count,
	// the complaint's [AMOUNT]) without emitting any event of its own.
	stageOrder := func(cid primitive.ObjectID, lane, status string) string {
		o := &order{
			MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: cid.Hex(), Status: status, Lane: lane,
			Items:    []orderItem{{ID: newItemID(), ProductID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Qty: 2, Price: 35}},
			Subtotal: 70, Total: 70, PaymentMethod: "wallet", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PlacedAt: time.Now().UTC(),
		}
		if err := w.svc.repo.insertOrder(ctx, o); err != nil {
			t.Fatalf("stage order: %v", err)
		}
		return o.OrderID
	}
	// stagePlan stages the plan a delayed plan message re-reads when it fires
	// (crmPlanChangeStale): A-05 needs it still active, C-03 still paused.
	stagePlan := func(cid primitive.ObjectID, subID, status string) {
		if _, err := w.db.Collection(collSubscriptions).InsertOne(ctx, &subscription{
			MongoID: primitive.NewObjectID(), SubscriptionID: subID, ConsumerID: cid, ProductID: "gold-500ml",
			Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Qty: 2, UnitPrice: 35, Frequency: "daily", Status: status,
			StartDate: istDay(time.Now().Add(24 * time.Hour)), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("stage plan: %v", err)
		}
	}
	grantMarketing := func(cid primitive.ObjectID) {
		yes := true
		if aerr := w.svc.applyConsents(ctx, cid, []consentInput{{Type: "marketing_whatsapp", Granted: &yes, Version: "2026-07"}}); aerr != nil {
			t.Fatalf("consent: %v", aerr)
		}
	}

	events := map[string]crmMatrixEvent{
		"A-01": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"source": "otp"}
		}, crmMatrixInbox},
		"A-02": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			id := stageOrder(cid, "instant", "delivered") // the member's first delivered order is a Quick Pyaas one
			return cid, map[string]any{"order_id": id, "offer_pack": int32(0), "promotional_only": false, "labelled_product": label}
		}, crmMatrixInbox},
		"A-03": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"subscription_id": "sub_mx_a03", "product_id": "gold-500ml", "qty": int32(2),
				"frequency": "daily", "start_date": istDay(time.Now().Add(24 * time.Hour)), "start_label": "tomorrow"}
		}, crmMatrixInbox},
		"A-05": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			stagePlan(cid, "sub_mx_a05", "active")
			return cid, map[string]any{"subscription_id": "sub_mx_a05", "first_cycle_amount": float64(70), "scope_key": "sub_mx_a05",
				"start_label": "tomorrow"}
		}, crmMatrixInbox},
		"B-03": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"payment_order_id": "order_mx_b03", "payment_id": "pay_mx_b03", "amount": float64(500),
				"reason": "Payment was declined by the bank", "source": "razorpay", "scope_key": "pay_mx_b03"}
		}, crmMatrixInbox},
		"B-06": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"amount": float64(500), "account": "topup", "reason": "recharge", "ref": "order_mx_b06", "scope_key": "order_mx_b06"}
		}, crmMatrixInbox},
		"C-03": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			stagePlan(cid, "sub_mx_c03", "paused")
			return cid, map[string]any{"subscription_id": "sub_mx_c03", "change": "paused", "scope_key": "sub_mx_c03:pause"}
		}, crmMatrixInbox},
		"D-01": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			return cid, map[string]any{"order_id": stageOrder(cid, "morning", "placed"), "labelled_product": label, "promotional_only": false, "eta": "tomorrow by 7 am"}
		}, crmMatrixInbox},
		"D-02": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			return cid, map[string]any{"order_id": stageOrder(cid, "instant", "out_for_delivery"), "labelled_product": label,
				"promotional_only": false, "partner": "Ravi", "eta_min": int32(12)}
		}, crmMatrixInbox},
		"D-03": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			return cid, map[string]any{"order_id": stageOrder(cid, "instant", "out_for_delivery"), "labelled_product": label,
				"new_eta_known": true, "eta": "about 7:52 am", "window_end": time.Now().UTC().Format(time.RFC3339)}
		}, crmMatrixInbox},
		"D-05": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			id := stageOrder(cid, "morning", "placed")
			return cid, map[string]any{"order_id": id, "line_id": "item_mx", "labelled_product": label, "amount": float64(35),
				"before_delivery": true, "scope_key": id + ":item_mx"}
		}, crmMatrixInbox},
		"D-06": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			return cid, map[string]any{"order_id": stageOrder(cid, "morning", "delivered"), "offer_pack": int32(0), "promotional_only": false, "labelled_product": label}
		}, crmMatrixInbox},
		// The noon lock skipped tomorrow for want of funds (skipSubPreview).
		"D-07": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			day := istDay(time.Now().Add(24 * time.Hour))
			return member(0), map[string]any{"subscription_id": "sub_mx_d07", "day": day, "reason": orderCancelledByWalletShort,
				"shortfall": float64(70), "resume_label": crmDayLabel(addDaysIST(day, 1), time.Now()), "tomorrow_blocked": true,
				"scope_key": "day_skipped:" + day}
		}, crmMatrixInbox},
		"D-09": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			return cid, map[string]any{"order_id": stageOrder(cid, "morning", "cancelled"), "labelled_product": label, "reason": "customer not at home"}
		}, crmMatrixInbox},
		"E-01": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			return cid, map[string]any{"order_id": stageOrder(cid, "morning", "delivered"), "rating": int32(2)}
		}, crmMatrixInbox},
		"E-02": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"complaint_id": "cmp_mx_e02", "ref": "PYS-MX-E02", "category": "late", "order_id": ""}
		}, crmMatrixInbox},
		"E-04": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			return cid, map[string]any{"complaint_id": "cmp_mx_e04", "ref": "PYS-MX-E04", "category": "missing", "order_id": stageOrder(cid, "morning", "delivered")}
		}, crmMatrixInbox},
		"E-05": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"complaint_id": "cmp_mx_e05", "ref": "PYS-MX-E05", "category": "quality", "order_id": ""}
		}, crmMatrixCallback},
		"E-06": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"complaint_id": "cmp_mx_e06", "ref": "PYS-MX-E06", "resolution": "One pack refunded to your wallet"}
		}, crmMatrixInbox},
		"E-07": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			cid := member(0)
			grantMarketing(cid) // promotional: G2 needs a live marketing grant
			return cid, map[string]any{"order_id": stageOrder(cid, "morning", "delivered"), "rating": int32(5)}
		}, crmMatrixInbox},
		"W-08": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"in_zone": false, "pincode": "999999", "source": "enrol",
				"has_orders": false, "has_serviceable_address": false}
		}, crmMatrixInbox},
		// Founding Family (founding.go): the unlock pair and the seat held,
		// with the payloads unlockFoundingFarm and joinFoundingFamily write.
		"FF-01": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"farm_id": "mx-farm", "farm": "Matrix Farm", "farmer": "Harsh Singh",
				"line": int32(4), "togo": int32(0), "unlocked_packs": "", "first_delivery_label": "tomorrow"}
		}, crmMatrixInbox},
		"FF-02": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"farm_id": "mx-farm", "farm": "Matrix Farm", "farmer": "Harsh Singh",
				"line": int32(4), "togo": int32(0), "unlocked_packs": "", "first_delivery_label": "tomorrow"}
		}, crmMatrixInbox},
		"FF-03": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"farm_id": "mx-farm", "farm": "Matrix Farm", "farmer": "Harsh Singh",
				"line": int32(12), "togo": int32(53)}
		}, crmMatrixInbox},
		// The payload founding_line.go writes when a friend's Rs 99 moves
		// the referrer up the line.
		"FF-04": {func(t *testing.T) (primitive.ObjectID, map[string]any) {
			return member(0), map[string]any{"referral_id": "mx-ref", "line": int32(11), "from": int32(12),
				"referrer_farm_id": "mx-farm", "friend": "Neha", "farm_id": "mx-farm-2", "farm": "Matrix Farm Two",
				"farmer": "Harsh Singh", "togo": int32(7), "friend_farm_unlocked": false, "scope_key": "founding:line:mx-ref"}
		}, crmMatrixInbox},
	}

	// Flows for the triggers a clock or the Welcome Litre state machine
	// fires. Each returns the member it drove; the expectations are below.
	flows := map[string]string{
		"B-01": crmMatrixInbox, "B-02": crmMatrixInbox,
		"W-01": crmMatrixInbox, "W-02": crmMatrixInbox, "W-03a": crmMatrixInbox, "W-03b": crmMatrixInbox,
		"W-04": crmMatrixInbox, "W-05": crmMatrixInbox, "W-06": crmMatrixInbox, "W-07": crmMatrixInbox,
		"W-09": crmMatrixAdmin, "W-10": crmMatrixDispatch,
	}

	// ── Coverage: the rule itself, on the shipped config ──
	covered := map[string]bool{}
	for id := range events {
		covered[id] = true
	}
	for id := range flows {
		covered[id] = true
	}
	for id := range events {
		if tr, ok := cfg.Triggers[id]; !ok || tr.Kind != "event" {
			t.Fatalf("matrix event scenario %s is not an event trigger in the config", id)
		}
	}
	if problems := crmMatrixUncovered(cfg, covered); len(problems) > 0 {
		t.Fatalf("CRM matrix coverage:\n  %s", strings.Join(problems, "\n  "))
	}

	// ── 1) Event triggers: emit the trigger's own topic, drain, wait out its delay ──
	cids := map[string]primitive.ObjectID{}
	for id, sc := range events {
		cid, payload := sc.setup(t)
		cids[id] = cid
		w.svc.emitCRMEvent(ctx, cfg.Triggers[id].Event, cid, payload)
	}
	w.svc.crmProcessEventsAt(ctx, t0)
	w.svc.crmFireDueSchedules(ctx, t0.Add(5*time.Hour)) // longest configured delay is PT4H (E-07)

	// ── 2) The wallet sweeps: a member on a daily plan with an empty wallet ──
	// (staged before any scheduler tick, because each sweep claims its IST day once).
	low := member(0)
	lowSub := &subscription{
		MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(), ConsumerID: low,
		ProductID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Qty: 2, UnitPrice: 35,
		Frequency: "daily", Status: "active", StartDate: istDay(time.Now().Add(-48 * time.Hour)),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, err := w.db.Collection(collSubscriptions).InsertOne(ctx, lowSub); err != nil {
		t.Fatalf("stage plan: %v", err)
	}
	cids["B-01"], cids["B-02"] = low, low

	// ── 3) Welcome Litre, day 0: W-01, W-02, W-03a, W-03b, then W-04, W-05 ──
	enrol := func(line string) (primitive.ObjectID, string) {
		phone++
		res, err := w.svc.crmEnrol(ctx, "matrix-operator", crmEnrolInput{
			Phone: strconv.Itoa(phone), Name: "Matrix Household", Line1: line, Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
		})
		if err != nil {
			t.Fatalf("crmEnrol: %v", err)
		}
		cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
		// Pack 2 rides only a morning the plan really delivers (the noon
		// rule): the plan dates from before tomorrow's cut-off, so W-04
		// fires whatever the wall clock says.
		planFromBeforeNoon(t, ctx, w.svc.repo, res.SubscriptionID)
		return cid, res.Pack1OrderID
	}
	wl, pack1 := enrol("Flat 1, Matrix Tower")
	grantMarketing(wl)                                   // W-03b is promotional
	w.svc.crmProcessEvents(ctx)                          // offer.finalized -> W-01
	deliverOrder(t, ctx, w.svc, w.svc.repo, w.db, pack1) // pack 1 at the door
	w.svc.crmProcessEvents(ctx)                          // order.delivered pack 1 -> W-02
	forceFirstDelivery(t, w.db, wl, at(6, 30))           // day 0 is today
	w.svc.crmProcessSchedules(ctx, at(10, 40))           // W-03a (no top-up yet), W-03b; B-01 sweep
	if _, err := w.svc.creditTopup(ctx, wl, 500, "razorpay", "order_mx_w04"); err != nil {
		t.Fatalf("recharge: %v", err)
	}
	w.svc.crmProcessEvents(ctx) // wallet.recharge_settled -> pack 2 attached -> W-04
	off := mustOffer(t, ctx, w.svc, wl)
	if off.Pack2OrderID == "" {
		t.Fatalf("pack 2 not attached after the recharge: %+v", off)
	}
	deliverOrder(t, ctx, w.svc, w.svc.repo, w.db, off.Pack2OrderID)
	w.svc.crmProcessEvents(ctx) // order.delivered pack 2 -> W-05
	for _, id := range []string{"W-01", "W-02", "W-03a", "W-03b", "W-04", "W-05"} {
		cids[id] = wl
	}

	// ── 4) Welcome Litre, day 3 (W-06) and day 8 (W-07) ──
	for id, daysAgo := range map[string]int{"W-06": 3, "W-07": 8} {
		cid, p1 := enrol("Flat " + id + ", Matrix Tower")
		deliverOrder(t, ctx, w.svc, w.svc.repo, w.db, p1)
		w.svc.crmProcessEvents(ctx)
		forceFirstDelivery(t, w.db, cid, at(6, 30).AddDate(0, 0, -daysAgo))
		cids[id] = cid
	}
	w.svc.crmProcessSchedules(ctx, at(10, 45))

	// ── 5) B-02 once the noon lock has run (recorded, not driven: nr-3), W-10 at 18:00 ──
	noonLockRanAt(t, w.svc, at(12, 0).Add(5*time.Second))
	w.svc.crmProcessSchedules(ctx, at(12, 5))
	w.svc.crmProcessSchedules(ctx, at(18, 5))

	// ── 6) W-09: a second enrolment at the same door raises the abuse flag ──
	adminID := primitive.NewObjectID()
	if _, err := w.db.Collection("parties").InsertOne(ctx, bson.D{{Key: "_id", Value: adminID}, {Key: "full_name", Value: "Ops Admin"}, {Key: "phone", Value: "+919000000099"}}); err != nil {
		t.Fatalf("admin party: %v", err)
	}
	if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: adminID}, {Key: "role_code", Value: "SUPER_ADMIN"}, {Key: "status", Value: "ACTIVE"},
	}); err != nil {
		t.Fatalf("admin role: %v", err)
	}
	flagged, _ := enrol("Flat 1, Matrix Tower")
	w.svc.crmProcessEvents(ctx)
	cids["W-09"] = flagged

	// ── The matrix: what each live trigger left behind ──
	ids := make([]string, 0, len(covered))
	for id := range covered {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	expect := func(id string) string {
		if sc, ok := events[id]; ok {
			return sc.expect
		}
		return flows[id]
	}
	for _, id := range ids {
		cid := cids[id]
		switch expect(id) {
		case crmMatrixInbox:
			cur, err := w.db.Collection(collConsumerInbox).Find(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: id}})
			if err != nil {
				t.Fatalf("%s inbox read: %v", id, err)
			}
			var rows []struct {
				EN string `bson:"body_en"`
				HI string `bson:"body_hi"`
			}
			if err := cur.All(ctx, &rows); err != nil {
				t.Fatalf("%s inbox decode: %v", id, err)
			}
			if len(rows) == 0 {
				t.Errorf("%s: no inbox row (dispatch rows: %v)", id, crmDispatchStatuses(t, w.db, cid, id))
				continue
			}
			for _, r := range rows {
				for _, body := range []string{r.EN, r.HI} {
					if tok := crmTokenRe.FindString(body); tok != "" || strings.TrimSpace(body) == "" {
						t.Errorf("%s: unresolved or empty body %q", id, body)
					}
				}
			}
		case crmMatrixCallback:
			if n, _ := w.db.Collection(collCRMCallbacks).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: id}}); n != 1 {
				t.Errorf("%s: %d call-back rows, want 1", id, n)
			}
			if n := inboxCount(t, w.db, cid, id); n != 0 {
				t.Errorf("%s: a human_call trigger wrote %d inbox rows", id, n)
			}
		case crmMatrixAdmin:
			if n, _ := w.db.Collection("notifications").CountDocuments(ctx, bson.D{{Key: "party_id", Value: adminID}, {Key: "template_key", Value: "CRM_ABUSE_FLAG"}}); n != 1 {
				t.Errorf("%s: operator bell rows = %d, want 1", id, n)
			}
		case crmMatrixDispatch:
			if got := crmDispatchStatuses(t, w.db, primitive.NilObjectID, id); len(got) != 1 || got[0] != "SENT" {
				t.Errorf("%s: dispatch rows %v, want one SENT", id, got)
			}
		}
	}
}

// The coverage rule catches a trigger added with no emitter and no
// awaiting_event mark, and is satisfied again once it is marked.
func TestCRMMatrixCoverageCatchesAnUnwiredTrigger(t *testing.T) {
	real := crmConfigLoad()
	cfg := &crmConfig{Triggers: map[string]crmTrigger{}, Templates: real.Templates, AwaitingEvent: map[string]string{}}
	for id, tr := range real.Triggers {
		cfg.Triggers[id] = tr
	}
	for id, why := range real.AwaitingEvent {
		cfg.AwaitingEvent[id] = why
	}
	covered := map[string]bool{}
	for id, tr := range cfg.Triggers {
		if _, waiting := cfg.AwaitingEvent[id]; !waiting && tr.Kind != "alias" {
			covered[id] = true
		}
	}
	if got := crmMatrixUncovered(cfg, covered); len(got) != 0 {
		t.Fatalf("baseline must be clean: %v", got)
	}

	// Someone adds a message on a topic nothing emits, with no scenario.
	cfg.Triggers["Z-99"] = crmTrigger{ID: "Z-99", Kind: "event", Event: "thing.happened", Category: "service_implicit", Template: crmTemplateRef{Ref: "T-D06"}}
	got := strings.Join(crmMatrixUncovered(cfg, covered), "\n")
	if !strings.Contains(got, "Z-99: no matrix scenario") || !strings.Contains(got, "Z-99: topic thing.happened has no emitter") {
		t.Fatalf("an unwired trigger must fail the matrix, got:\n%s", got)
	}
	// Marking it awaiting_event (with its reason) is the documented way to
	// keep copy in the config before its product event exists.
	cfg.AwaitingEvent["Z-99"] = "no product event yet"
	if got := crmMatrixUncovered(cfg, covered); len(got) != 0 {
		t.Fatalf("a trigger marked awaiting_event must pass: %v", got)
	}
	// A stale mark on a removed trigger is caught too.
	delete(cfg.Triggers, "Z-99")
	if got := strings.Join(crmMatrixUncovered(cfg, covered), "\n"); !strings.Contains(got, "Z-99: meta.awaiting_event names a trigger that does not exist") {
		t.Fatalf("a stale awaiting_event entry must fail: %s", got)
	}
}

// R1-14: following "How to add a message" step 2 half-way (the topic added to
// crmLifecycleTopics, the emitCRMEvent call forgotten) used to leave the
// matrix green, because the emitted-topic set was that same hand-kept list.
// The set is now read from the emitCRMEvent call sites in the package's
// non-test sources.
func TestCRMMatrixCoverageCatchesATopicWithNoEmitter(t *testing.T) {
	real := crmConfigLoad()
	cfg := &crmConfig{Triggers: map[string]crmTrigger{}, Templates: real.Templates, AwaitingEvent: real.AwaitingEvent}
	covered := map[string]bool{}
	for id, tr := range real.Triggers {
		cfg.Triggers[id] = tr
		if _, waiting := real.AwaitingEvent[id]; !waiting && tr.Kind != "alias" {
			covered[id] = true
		}
	}
	saved := crmLifecycleTopics
	defer func() { crmLifecycleTopics = saved }()
	crmLifecycleTopics = append(append([]string{}, saved...), "subscription.renewed")
	cfg.Triggers["Z-98"] = crmTrigger{ID: "Z-98", Kind: "event", Event: "subscription.renewed", Category: "service_implicit", Template: crmTemplateRef{Ref: "T-A03"}}
	covered["Z-98"] = true
	got := strings.Join(crmMatrixUncovered(cfg, covered), "\n")
	if !strings.Contains(got, "Z-98: topic subscription.renewed has no emitter") {
		t.Fatalf("a listed topic nothing emits must fail the matrix, got:\n%s", got)
	}
	// Every topic the router serves really has an emitter today.
	emitted := crmEmittedTopics()
	for _, tp := range saved {
		if !emitted[tp] {
			t.Errorf("crmLifecycleTopics lists %s but no emitCRMEvent call emits it", tp)
		}
	}
}

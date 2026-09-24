package consumer

// A trigger's "delay" is honoured through crm_schedules: the draining tick
// queues the dispatch for event-time + delay, the scheduler tick fires it
// once the clock passes that instant, and the conditions are checked again
// at fire time. Driven with an injected clock end to end.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMDelay -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCRMParseISODuration(t *testing.T) {
	cases := map[string]time.Duration{
		"PT0S": 0, "PT2H": 2 * time.Hour, "PT30M": 30 * time.Minute, "PT1H30M": 90 * time.Minute,
		"P1D": 24 * time.Hour, "P1DT2H": 26 * time.Hour, "pt4h": 4 * time.Hour,
	}
	for in, want := range cases {
		got, ok := crmParseISODuration(in)
		if !ok || got != want {
			t.Errorf("%q: got %v ok=%v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "P", "PT", "2H", "PT2", "P2H", "PT1D", "next_morning", "PTH"} {
		if _, ok := crmParseISODuration(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
	cfg := crmConfigLoad()
	if d := crmTriggerDelay(cfg.Triggers["A-01"]); d != 2*time.Hour {
		t.Fatalf("A-01 delay: %v", d)
	}
	if d := crmTriggerDelay(cfg.Triggers["D-06"]); d != 0 {
		t.Fatalf("D-06 delay: %v", d)
	}
	// Every configured delay is readable, so none is silently treated as zero.
	for id, tr := range cfg.Triggers {
		if tr.Delay == "" {
			continue
		}
		if _, ok := crmParseISODuration(tr.Delay); !ok {
			t.Errorf("%s: delay %q does not parse", id, tr.Delay)
		}
	}
}

// crmScheduleRows lists a consumer's schedule rows for one trigger.
func crmScheduleRows(t *testing.T, db *mongo.Database, cid primitive.ObjectID, trigger string) []crmSchedule {
	t.Helper()
	cur, err := db.Collection(collCRMSchedules).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		t.Fatalf("schedule rows: %v", err)
	}
	var rows []crmSchedule
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("schedule rows decode: %v", err)
	}
	return rows
}

func TestCRMDelayHonouredWithFakeClock(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	// A-01 (user.registered, PT2H, orders_count == 0) for two fresh members.
	waits := w.customer(t, "9000007301", 0)
	shops := w.customer(t, "9000007302", 0) // recharges during the delay (below)
	nowIST := time.Now().In(istZone)
	t0 := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 9, 0, 0, 0, istZone)
	for _, cid := range []primitive.ObjectID{waits, shops} {
		w.svc.emitCRMEvent(ctx, "user.registered", cid, map[string]any{})
	}

	// The draining tick queues, it does not send.
	w.svc.crmProcessEventsAt(ctx, t0)
	for _, cid := range []primitive.ObjectID{waits, shops} {
		if got := crmDispatchStatuses(t, w.db, cid, "A-01"); len(got) != 0 {
			t.Fatalf("A-01 must not dispatch on the draining tick: %v", got)
		}
		rows := crmScheduleRows(t, w.db, cid, "A-01")
		if len(rows) != 1 || rows[0].Status != "NEW" || !rows[0].DueAt.Equal(t0.Add(2*time.Hour).UTC()) {
			t.Fatalf("A-01 schedule row: %+v", rows)
		}
	}
	// A replayed event (the outbox is at-least-once) queues nothing new.
	evs := crmEventsOf(t, w.db, waits, "user.registered")
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	w.svc.crmRouteEventAt(ctx, evs[0], t0)
	if rows := crmScheduleRows(t, w.db, waits, "A-01"); len(rows) != 1 {
		t.Fatalf("a replayed event must not queue twice: %d rows", len(rows))
	}

	// Before the delay elapses: nothing, and the schedule is still NEW.
	w.svc.crmProcessSchedules(ctx, t0.Add(119*time.Minute))
	if got := crmDispatchStatuses(t, w.db, waits, "A-01"); len(got) != 0 {
		t.Fatalf("A-01 fired early: %v", got)
	}
	if rows := crmScheduleRows(t, w.db, waits, "A-01"); rows[0].Status != "NEW" {
		t.Fatalf("early tick must leave the row NEW: %+v", rows[0])
	}

	// Meanwhile the second member recharges and their first order is
	// delivered: at fire time orders_count is 1, so their A-01 is refused by
	// the re-check.
	if _, err := w.svc.creditTopup(ctx, shops, 500, "razorpay", "fund-9000007302"); err != nil {
		t.Fatalf("recharge: %v", err)
	}
	instantOrderDelivered(t, w, shops)

	w.svc.crmProcessSchedules(ctx, t0.Add(2*time.Hour))
	if got := crmDispatchStatuses(t, w.db, waits, "A-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("A-01 at t0+2h: %v", got)
	}
	if n := inboxCount(t, w.db, waits, "A-01"); n != 1 {
		t.Fatalf("A-01 inbox rows = %d", n)
	}
	if rows := crmScheduleRows(t, w.db, waits, "A-01"); rows[0].Status != "DONE" {
		t.Fatalf("fired row must be DONE: %+v", rows[0])
	}
	if got := crmDispatchStatuses(t, w.db, shops, "A-01"); len(got) != 0 {
		t.Fatalf("A-01 must be refused once the member has ordered: %v", got)
	}
	if rows := crmScheduleRows(t, w.db, shops, "A-01"); rows[0].Status != "SKIPPED" || rows[0].Reason != "conditions no longer hold at fire" {
		t.Fatalf("re-check must skip with a reason: %+v", rows[0])
	}

	// A later tick fires nothing twice.
	w.svc.crmProcessSchedules(ctx, t0.Add(3*time.Hour))
	if got := crmDispatchStatuses(t, w.db, waits, "A-01"); len(got) != 1 {
		t.Fatalf("A-01 must fire once: %v", got)
	}

	// The lease of a dead worker is handed back after ten minutes.
	late := w.customer(t, "9000007303", 0)
	w.svc.emitCRMEvent(ctx, "user.registered", late, map[string]any{})
	w.svc.crmProcessEventsAt(ctx, t0)
	if _, err := w.db.Collection(collCRMSchedules).UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: late}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "PROCESSING"}, {Key: "claimed_at", Value: t0.Add(2 * time.Hour).UTC()}}}}); err != nil {
		t.Fatalf("stage dead lease: %v", err)
	}
	w.svc.crmProcessSchedules(ctx, t0.Add(2*time.Hour+5*time.Minute))
	if got := crmDispatchStatuses(t, w.db, late, "A-01"); len(got) != 0 {
		t.Fatalf("a live lease must not be fired by another tick: %v", got)
	}
	w.svc.crmProcessSchedules(ctx, t0.Add(2*time.Hour+11*time.Minute))
	if got := crmDispatchStatuses(t, w.db, late, "A-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("a dead lease must be recovered and fired: %v", got)
	}
}

func TestCRMEventStaleOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007304", 500)
	o := instantOrderDelivered(t, w, cid)

	mk := func(topic, orderID string) *crmEventCtx {
		return &crmEventCtx{s: w.svc, ctx: ctx, ev: crmEvent{Topic: topic, ConsumerID: cid, Payload: map[string]any{"order_id": orderID}}}
	}
	if stale, _ := crmEventStale(mk("rating.submitted", o.OrderID)); stale {
		t.Fatal("a delivered order is not stale")
	}
	if stale, _ := crmEventStale(mk("rating.submitted", "")); stale {
		t.Fatal("an event without an order is never stale")
	}
	if stale, _ := crmEventStale(mk("rating.submitted", "ord_missing")); !stale {
		t.Fatal("an order that no longer exists is stale")
	}
	if _, err := w.db.Collection(collOrders).UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: o.OrderID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}}}}); err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	if stale, why := crmEventStale(mk("rating.submitted", o.OrderID)); !stale || why == "" {
		t.Fatalf("a cancelled order is stale: %v %q", stale, why)
	}
	for _, topic := range []string{"order.failed", "order.line_cancelled"} {
		if stale, _ := crmEventStale(mk(topic, o.OrderID)); stale {
			t.Fatalf("%s announces the cancellation and is never stale", topic)
		}
	}
}

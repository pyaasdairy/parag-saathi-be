package consumer

// Founder decision 6 (25 Sep 2026): quiet hours 22:00-07:00 IST. Order,
// delivery and money-added messages always send; offers (and the app's
// taglines) wait for morning. The rule is data: guards.G5_quiet_hours
// (windows.service_transactional.avoid for the hours, quiet_hours.always_send
// for what may still go out). A message held by the quiet hours is deferred,
// never dropped: an event trigger waits in crm_schedules and is re-checked
// when the hours end; a scheduled sweep caught inside them sends nothing
// that night and its next run decides again.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 \
//	  go test ./internal/modules/consumer/ -run CRMQuiet -v

import (
	"context"
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Every live trigger's quiet-hours class, as the founder's rule reads on the
// shipped config. A config edit that moves a trigger between classes fails
// here, so docs/CRM-MESSAGES.md ("Quiet hours") is updated with it.
func TestCRMQuietHoursClassOfEveryLiveTrigger(t *testing.T) {
	cfg := crmConfigLoad()
	if cfg.Guards.QuietStart != "22:00" || cfg.Guards.QuietEnd != "07:00" {
		t.Fatalf("quiet hours from the config: %s-%s, want 22:00-07:00", cfg.Guards.QuietStart, cfg.Guards.QuietEnd)
	}
	want := map[string]string{}
	for _, id := range []string{
		"A-02", "A-03", "B-02", "B-03", "B-06",
		"D-01", "D-02", "D-03", "D-05", "D-06", "D-07", "D-09",
		"E-01", "E-02", "E-04", "E-05", "E-06",
		"W-01", "W-02", "W-03a", "W-04", "W-05", "W-08", "W-09", "W-10",
		"FF-01", "FF-02", "FF-03",
	} {
		want[id] = "always"
	}
	for _, id := range []string{"A-01", "A-05", "B-01", "C-03", "W-06", "W-07"} {
		want[id] = "deferred"
	}
	for _, id := range []string{"E-07", "W-03b"} {
		want[id] = "promotional"
	}
	var live []string
	for id, tr := range cfg.Triggers {
		if _, waiting := cfg.AwaitingEvent[id]; waiting || tr.Kind == "alias" {
			continue
		}
		live = append(live, id)
	}
	sort.Strings(live)
	if len(live) != len(want) {
		t.Fatalf("live triggers %d, classified %d: %v", len(live), len(want), live)
	}
	for _, id := range live {
		tr := cfg.Triggers[id]
		got := "deferred"
		switch {
		case tr.Category == "promotional":
			got = "promotional"
			if crmAlwaysSends(tr) {
				t.Errorf("%s: a promotional trigger must never be always_send", id)
			}
		case crmAlwaysSends(tr):
			got = "always"
		}
		if got != want[id] {
			t.Errorf("%s (%s, section %s, critical %v): %s, want %s", id, tr.Category, tr.Section, tr.Critical, got, want[id])
		}
	}
	// A trigger added later is deferred unless the rule names it.
	if crmAlwaysSends(crmTrigger{ID: "Z-01", Category: "service_implicit", Section: "C"}) {
		t.Error("an unlisted service trigger must be deferred by default")
	}
}

func TestCRMQuietHoursSendWindow(t *testing.T) {
	cfg := crmConfigLoad()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	a01, d06, e07 := cfg.Triggers["A-01"], cfg.Triggers["D-06"], cfg.Triggers["E-07"]
	for _, c := range []struct {
		name string
		t    crmTrigger
		at   time.Time
		open bool
		next time.Time
	}{
		{"deferred, 21:59", a01, istDayAt(D, 21, 59), true, istDayAt(D, 21, 59)},
		{"deferred, 22:00", a01, istDayAt(D, 22, 0), false, istDayAt(D1, 7, 0)},
		{"deferred, 23:30", a01, istDayAt(D, 23, 30), false, istDayAt(D1, 7, 0)},
		{"deferred, 00:00", a01, istDayAt(D1, 0, 0), false, istDayAt(D1, 7, 0)},
		{"deferred, 06:59", a01, istDayAt(D1, 6, 59), false, istDayAt(D1, 7, 0)},
		{"deferred, 07:00", a01, istDayAt(D1, 7, 0), true, istDayAt(D1, 7, 0)},
		{"delivery, 05:30", d06, istDayAt(D1, 5, 30), true, istDayAt(D1, 5, 30)},
		{"delivery, 23:45", d06, istDayAt(D, 23, 45), true, istDayAt(D, 23, 45)},
		{"promotional, 08:00", e07, istDayAt(D, 8, 0), false, istDayAt(D, 10, 0)},
		{"promotional, 12:00", e07, istDayAt(D, 12, 0), true, istDayAt(D, 12, 0)},
		{"promotional, 21:30", e07, istDayAt(D, 21, 30), false, istDayAt(D1, 10, 0)},
		{"promotional, 23:00", e07, istDayAt(D, 23, 0), false, istDayAt(D1, 10, 0)},
	} {
		next, open := crmSendWindow(c.t, c.at)
		if open != c.open || !next.Equal(c.next) {
			t.Errorf("%s: (%s, %v), want (%s, %v)", c.name, next.In(istZone).Format("2 Jan 15:04"), open,
				c.next.In(istZone).Format("2 Jan 15:04"), c.open)
		}
	}
}

// The router's decision for an event trigger whose conditions hold: now, or
// the moment its crm_schedules row comes due.
func TestCRMQuietHoursDueAt(t *testing.T) {
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	nudge := crmTrigger{ID: "Z-01", Kind: "event", Category: "service_implicit", Section: "C"}
	order := crmTrigger{ID: "Z-02", Kind: "event", Category: "service_implicit", Section: "D"}
	promo := crmTrigger{ID: "Z-03", Kind: "event", Category: "promotional", Section: "E"}
	delayed := crmTrigger{ID: "Z-04", Kind: "event", Category: "service_implicit", Section: "A", Delay: "PT2H"}
	ev := func(at time.Time) crmEvent { return crmEvent{CreatedAt: at.UTC()} }
	for _, c := range []struct {
		name    string
		t       crmTrigger
		at      time.Time
		fireNow bool
		due     time.Time
	}{
		{"a nudge by day goes now", nudge, istDayAt(D, 12, 0), true, istDayAt(D, 12, 0)},
		{"a nudge at 23:00 waits for 07:00", nudge, istDayAt(D, 23, 0), false, istDayAt(D1, 7, 0)},
		{"an order message at 02:00 goes now", order, istDayAt(D1, 2, 0), true, istDayAt(D1, 2, 0)},
		{"an offer at 08:00 waits for 10:00", promo, istDayAt(D, 8, 0), false, istDayAt(D, 10, 0)},
		{"a delay counts from the event; the fire re-checks the hours", delayed, istDayAt(D, 21, 0), false, istDayAt(D, 23, 0)},
	} {
		due, now := crmDueAt(c.t, ev(c.at), c.at)
		if now != c.fireNow || !due.Equal(c.due) {
			t.Errorf("%s: (%s, %v), want (%s, %v)", c.name, due.In(istZone).Format("2 Jan 15:04"), now,
				c.due.In(istZone).Format("2 Jan 15:04"), c.fireNow)
		}
	}
}

// A-01 comes due at 22:30 (a 20:30 sign-up + PT2H): it waits until 07:00,
// is re-checked, and sent then. Nothing is dispatched, and nothing is lost,
// in between.
func TestCRMQuietHoursDelayedNudgeWaitsForMorning(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)

	cid := w.customer(t, "9000014201", 0)
	w.svc.emitCRMEvent(ctx, "user.registered", cid, map[string]any{"source": "otp"})
	crmStampEvents(t, w, cid, istDayAt(D, 20, 30))
	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 20, 31))
	if rows := crmScheduleRows(t, w.db, cid, "A-01"); len(rows) != 1 || !rows[0].DueAt.Equal(istDayAt(D, 22, 30).UTC()) {
		t.Fatalf("A-01 queued for 22:30: %+v", rows)
	}

	w.svc.crmFireDueSchedules(ctx, istDayAt(D, 22, 31))
	if got := crmDispatchStatuses(t, w.db, cid, "A-01"); len(got) != 0 {
		t.Fatalf("A-01 inside the quiet hours must not be dispatched (nor suppressed): %v", got)
	}
	rows := crmScheduleRows(t, w.db, cid, "A-01")
	if len(rows) != 1 || rows[0].Status != "NEW" || !rows[0].DueAt.Equal(istDayAt(D1, 7, 0).UTC()) ||
		rows[0].Reason != "waiting for the quiet hours to end" {
		t.Fatalf("A-01 must wait for 07:00: %+v", rows)
	}
	w.svc.crmFireDueSchedules(ctx, istDayAt(D1, 6, 59))
	if got := crmDispatchStatuses(t, w.db, cid, "A-01"); len(got) != 0 {
		t.Fatalf("A-01 before 07:00: %v", got)
	}

	w.svc.crmFireDueSchedules(ctx, istDayAt(D1, 7, 1))
	if got := crmDispatchStatuses(t, w.db, cid, "A-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("A-01 at 07:01: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "A-01"); n != 1 {
		t.Fatalf("A-01 inbox rows = %d", n)
	}
}

// Order, delivery and money-added messages go at once, day or night: a
// morning delivery at 05:30 and a top-up at 02:00.
func TestCRMQuietHoursOrderDeliveryMoneyAlwaysSend(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"

	paid := w.customer(t, "9000014202", 0)
	w.svc.emitCRMEvent(ctx, "wallet.credited", paid, map[string]any{
		"amount": float64(500), "account": "topup", "reason": "recharge", "ref": "order_qh_b06", "scope_key": "order_qh_b06",
	})
	crmStampEvents(t, w, paid, istDayAt(D, 2, 0))

	fed := w.customer(t, "9000014203", 0)
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: fed.Hex(), Status: "delivered", Lane: "morning",
		Items:    []orderItem{{ID: newItemID(), ProductID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Qty: 2, Price: 35}},
		Subtotal: 70, Total: 70, PaymentMethod: "wallet", DeliveredAt: istDayAt(D, 5, 30).UTC().Format(time.RFC3339),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PlacedAt: time.Now().UTC(),
	}
	if err := w.svc.repo.insertOrder(ctx, o); err != nil {
		t.Fatalf("stage order: %v", err)
	}
	w.svc.emitCRMEvent(ctx, "order.delivered", fed, map[string]any{
		"order_id": o.OrderID, "offer_pack": int32(0), "promotional_only": false,
		"labelled_product": crmLabelledProductOf(o), "orders_count": int32(1),
	})
	crmStampEvents(t, w, fed, istDayAt(D, 5, 30))

	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 5, 31))
	for cid, id := range map[primitive.ObjectID]string{paid: "B-06", fed: "D-06"} {
		if got := crmDispatchStatuses(t, w.db, cid, id); len(got) != 1 || got[0] != "SENT" {
			t.Errorf("%s inside the quiet hours must go at once: %v", id, got)
		}
		if rows := crmScheduleRows(t, w.db, cid, id); len(rows) != 0 {
			t.Errorf("%s must not be queued: %+v", id, rows)
		}
	}
}

// A scheduled sweep the worker only reaches after 22:00 holds what is not
// always_send and does not spend the day's claim on it: B-01 comes with the
// next morning's sweep, W-07 (and the pack's expiry) with the next 10:30 run.
func TestCRMQuietHoursScheduledSweepCatchUp(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)

	low := w.customer(t, "9000014204", 0)
	if _, err := w.db.Collection(collSubscriptions).InsertOne(ctx, &subscription{
		MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(), ConsumerID: low,
		ProductID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Qty: 2, UnitPrice: 35,
		Frequency: "daily", Status: "active", StartDate: addDaysIST(D, -2),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("stage plan: %v", err)
	}
	res, err := w.svc.crmEnrol(ctx, "quiet-hours-operator", crmEnrolInput{
		Phone: "9000014205", Name: "Quiet Household", Line1: "Flat 7, Quiet Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	wl, _ := primitive.ObjectIDFromHex(res.ConsumerID)
	forceFirstDelivery(t, w.db, wl, istDayAt(addDaysIST(D, -9), 6, 30)) // day 9: the pack's window is over

	w.svc.crmProcessSchedules(ctx, istDayAt(D, 22, 40)) // the worker's first run that day
	if got := crmDispatchStatuses(t, w.db, low, "B-01"); len(got) != 0 {
		t.Fatalf("B-01 at 22:40: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, primitive.NilObjectID, "B-01-SWEEP"); len(got) != 0 {
		t.Fatalf("a held sweep must not spend the day's claim: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, wl, "W-07"); len(got) != 0 {
		t.Fatalf("W-07 at 22:40: %v", got)
	}
	if o := mustOffer(t, ctx, w.svc, wl); o.Pack2State != pack2Locked {
		t.Fatalf("the pack expires only once W-07 is sent: %s", o.Pack2State)
	}

	w.svc.crmProcessSchedules(ctx, istDayAt(D1, 10, 31))
	if got := crmDispatchStatuses(t, w.db, low, "B-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("B-01 the next morning: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, wl, "W-07"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("W-07 the next morning: %v", got)
	}
	if o := mustOffer(t, ctx, w.svc, wl); o.Pack2State != pack2Expired {
		t.Fatalf("pack 2 after W-07: %s", o.Pack2State)
	}
	var n int64
	if n, err = w.db.Collection(collCRMSchedules).CountDocuments(ctx, bson.D{}); err != nil || n != 0 {
		t.Fatalf("a scheduled sweep queues nothing: %d rows, %v", n, err)
	}
}

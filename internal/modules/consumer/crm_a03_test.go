package consumer

// R1-10: A-03 ("Your morning milk starts [DATE]") is per_subscription, but
// subscription.activated carried no scope_key, so the claim fell back to one
// per member per IST day: a member who subscribed to two products on one day
// heard about the first plan only.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMA03 -v

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestCRMA03OncePerPlanNotOncePerDay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007651", 1000)
	var subs []string
	for _, p := range []string{"gold-500ml", "taaza-1l"} {
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: p, Qty: 1, Frequency: "daily", StartDate: addDaysIST(istToday(time.Now()), 1)})
		if err != nil {
			t.Fatalf("createSubscription %s: %v", p, err)
		}
		subs = append(subs, sub.SubscriptionID)
	}
	w.svc.crmProcessEvents(ctx)
	w.svc.crmProcessEvents(ctx) // a replay changes nothing
	got := crmDispatchScopes(t, w.db, cid, "A-03")
	if len(got) != 2 || got[subs[0]] != "SENT" || got[subs[1]] != "SENT" {
		t.Fatalf("A-03 once per plan: %v (plans %v)", got, subs)
	}
	if n := inboxCount(t, w.db, cid, "A-03"); n != 2 {
		t.Fatalf("A-03 inbox rows = %d, want 2", n)
	}
}

// R2F-16: A-03's [DATE] named the first cadence day on or after start_date,
// which the noon lock does not honour: a plan created after noon for
// tomorrow was announced for "tomorrow" (it first delivers the day after), a
// weekly one for tomorrow likewise (it now starts on the first editable
// morning, G4, so it delivers the day after tomorrow rather than a week
// later), and a plan created in the morning with start_date today was
// announced for "today" (today is past its cut-off). [DATE] is now the
// plan's next_delivery_date, worded against the moment of creation.
func TestCRMA03NamesTheFirstMorningTheNoonLockDelivers(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	for i, c := range []struct {
		name      string
		at        time.Time
		start     string
		freq      string
		wantDay   string
		wantLabel string
	}{
		{"created 10:00 for tomorrow", istDayAt(D, 10, 0), D1, "daily", D1, "tomorrow"},
		{"created 14:00 for tomorrow", istDayAt(D, 14, 0), D1, "daily", D2, "8 Oct"},
		{"created 14:00, weekly from tomorrow (re-anchored, G4)", istDayAt(D, 14, 0), D1, "weekly", D2, "8 Oct"},
		{"created 10:00 for today", istDayAt(D, 10, 0), D, "daily", D1, "tomorrow"},
	} {
		cid := w.customer(t, "90000076"+strconv.Itoa(60+i), 1000)
		sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Qty: 1, Frequency: c.freq, StartDate: c.start}, c.at)
		if err != nil {
			t.Fatalf("%s: createSubscription: %v", c.name, err)
		}
		if sub.NextDeliveryDate != c.wantDay {
			t.Fatalf("%s: next_delivery_date %q, want %q", c.name, sub.NextDeliveryDate, c.wantDay)
		}
		evs := crmEventsOf(t, w.db, cid, "subscription.activated")
		if len(evs) != 1 {
			t.Fatalf("%s: subscription.activated events = %d", c.name, len(evs))
		}
		if got := evs[0].Payload["start_label"]; got != c.wantLabel {
			t.Errorf("%s: A-03 [DATE] = %v, want %q (the plan first delivers %s)", c.name, got, c.wantLabel, c.wantDay)
		}
	}
}

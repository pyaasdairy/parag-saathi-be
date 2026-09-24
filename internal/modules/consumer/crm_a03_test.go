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

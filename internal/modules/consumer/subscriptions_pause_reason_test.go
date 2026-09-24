package consumer

// A pause is always the member's (owner, 24 Sep, R3(b) Option A): the server
// never pauses a plan for a low wallet (the noon lock skips the day instead)
// and never resumes a pause on its own, so the only pause it records is the
// member's POST /pause, now stamped pause_reason "member" (additive: stored
// and on the wire while paused, gone once resumed).
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run PauseReason -v

import (
	"context"
	"encoding/json"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestSubscriptionPauseReasonIsTheMember(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	cid := w.customer(t, "9000014101", 0)
	sub := walletLockPlan(t, w, cid, "taaza-500ml", 1, addDaysIST(D, 1), istDayAt(addDaysIST(D, -2), 9, 0))

	wire := func(s *subscription) map[string]any {
		t.Helper()
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m
	}
	stored := func() bson.M {
		t.Helper()
		var m bson.M
		if err := w.db.Collection(collSubscriptions).FindOne(ctx, bson.D{{Key: "subscription_id", Value: sub.SubscriptionID}}).Decode(&m); err != nil {
			t.Fatalf("reload: %v", err)
		}
		return m
	}

	paused, err := w.svc.setSubscriptionStatusAt(ctx, cid, sub.SubscriptionID, "pause", istDayAt(D, 10, 0))
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := wire(paused)["pause_reason"]; got != "member" {
		t.Fatalf("POST /pause answers pause_reason %v, want member", got)
	}
	if got := stored()["pause_reason"]; got != "member" {
		t.Fatalf("stored pause_reason %v, want member", got)
	}

	// A credit and a day of ticks never resume it.
	chainCreditAt(t, w, cid, 500, "order_paused_14101", istDayAt(D, 11, 0))
	for _, at := range []int{11, 12, 13, 18} {
		w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, at, 5))
	}
	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 18, 10))
	if got := stored()["status"]; got != "paused" {
		t.Fatalf("a credit resumed the member's pause: status %v", got)
	}
	for _, day := range []string{addDaysIST(D, 1), addDaysIST(D, 2)} {
		if o := liveSubOrder(t, w, sub.SubscriptionID, day); o != nil {
			t.Fatalf("a paused plan got an order for %s: %+v", day, o)
		}
	}

	resumed, err := w.svc.setSubscriptionStatusAt(ctx, cid, sub.SubscriptionID, "resume", istDayAt(D, 19, 0))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, present := wire(resumed)["pause_reason"]; present {
		t.Fatalf("an active plan still carries pause_reason on the wire: %v", wire(resumed))
	}
	if got := stored()["pause_reason"]; got != nil && got != "" {
		t.Fatalf("stored pause_reason after resume: %v", got)
	}
}

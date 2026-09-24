package consumer

// R1-06: DPDP erasure took the CRM offers, inbox, dispatch log, events,
// call-backs, consents, complaints and push devices, but not crm_schedules.
// A member who erased their account within the delay of a pending message
// (A-01 2 h, C-03 1 h, A-05 2 h, E-07 4 h) kept a row holding their id and the
// event payload, and when it came due it wrote a fresh inbox row and a
// dispatch row under the erased id.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMErasure -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMErasureTakesPendingSchedulesWithTheAccount(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	t0 := time.Now()

	cid := w.customer(t, "9000007901", 500)
	sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: "daily", StartDate: addDaysIST(istToday(time.Now()), 1)})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	w.svc.emitCRMEvent(ctx, "user.registered", cid, map[string]any{"source": "otp"})
	w.svc.crmProcessEventsAt(ctx, t0)
	if n, _ := w.db.Collection(collCRMSchedules).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); n == 0 {
		t.Fatal("setup: no pending schedule to erase")
	}

	if err := w.svc.erase(ctx, cid); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if n, _ := w.db.Collection(collCRMSchedules).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); n != 0 {
		t.Fatalf("erasure left %d crm_schedules rows holding the member's id and payload", n)
	}
	w.svc.crmFireDueSchedules(ctx, t0.Add(2*time.Hour+time.Minute))
	for _, coll := range []string{collConsumerInbox, collCRMDispatch} {
		if n, _ := w.db.Collection(coll).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); n != 0 {
			t.Fatalf("%s holds %d rows for an erased member", coll, n)
		}
	}
}

// Belt and braces: a schedule row that outlives its account (queued by a tick
// racing the erasure) is skipped when it comes due, never sent.
func TestCRMScheduleForAnErasedAccountIsSkipped(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	gone := primitive.NewObjectID() // no account row
	if _, err := w.db.Collection(collCRMSchedules).InsertOne(ctx, crmSchedule{
		TriggerID: "A-01", ConsumerID: gone, EventID: primitive.NewObjectID(), Topic: "user.registered",
		Payload: map[string]any{"source": "otp"}, DueAt: time.Now().UTC().Add(-time.Minute), Status: "NEW", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("stage schedule: %v", err)
	}
	w.svc.crmFireDueSchedules(ctx, time.Now())
	rows := crmScheduleRows(t, w.db, gone, "A-01")
	if len(rows) != 1 || rows[0].Status != "SKIPPED" || rows[0].Reason != "account erased" {
		t.Fatalf("schedule for an erased account: %+v", rows)
	}
	for _, coll := range []string{collConsumerInbox, collCRMDispatch} {
		if n, _ := w.db.Collection(coll).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: gone}}); n != 0 {
			t.Fatalf("%s holds %d rows for an erased member", coll, n)
		}
	}
}

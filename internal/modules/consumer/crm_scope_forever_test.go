package consumer

// R1-13: the dispatch claim is (trigger, member, IST day, scope). For a
// trigger whose cap is per event (per_order, per_complaint, per_credit,
// per_failure, per_subscription ...) the same product event processed on a
// LATER IST day (an outbox lease handed back across midnight, an emitter
// retry) claimed a new day and sent D-06 again for the same order. Such a
// trigger now claims its scope once, whatever the day; a trigger without a
// per-event cap (C-03, keyed per plan and action) keeps its per-day claim.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMScopeForever -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMScopeForeverPerEventCapsAcrossDays(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007861", 1000)
	o := instantOrderDelivered(t, w, cid)
	w.svc.crmProcessEvents(ctx)
	if n := inboxCount(t, w.db, cid, "D-06"); n != 1 {
		t.Fatalf("D-06 rows after the delivery = %d", n)
	}
	// The same order.delivered event again, drained on the next IST day.
	var ev crmEvent
	if err := w.db.Collection(collCRMEvents).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "topic", Value: "order.delivered"}}).Decode(&ev); err != nil {
		t.Fatalf("event: %v", err)
	}
	ev.ID, ev.Status = primitive.NilObjectID, "NEW"
	if _, err := w.db.Collection(collCRMEvents).InsertOne(ctx, ev); err != nil {
		t.Fatalf("replay: %v", err)
	}
	w.svc.crmProcessEventsAt(ctx, time.Now().Add(24*time.Hour))
	if n := inboxCount(t, w.db, cid, "D-06"); n != 1 {
		t.Fatalf("D-06 for order %s after a next-day replay: %d rows, want 1", o.OrderID, n)
	}

	// C-03 has no per-event cap: a second pause of the same plan on a later
	// day is a new signal and still speaks.
	day1 := time.Now()
	for i, at := range []time.Time{day1, day1.Add(48 * time.Hour)} {
		w.svc.crmDispatchWith(ctx, "C-03", cid, map[string]string{}, at, crmDispatchOpts{Scope: "sub_forever:pause"})
		if n := inboxCount(t, w.db, cid, "C-03"); n != i+1 {
			t.Fatalf("C-03 pause #%d on its own day: %d rows, want %d", i+1, n, i+1)
		}
	}
}

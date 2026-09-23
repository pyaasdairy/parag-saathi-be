package consumer

// W-01 reaches SENT through the worker, not the enrol request: enrolment
// only writes an offer.finalized row to the outbox.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMW01 -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMW01DispatchedByWorkerNotHandler(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	res, err := w.svc.crmEnrol(ctx, "w01-test-operator", crmEnrolInput{
		Phone: "9000007105", Name: "Welcome Household", Line1: "Flat 1, Worker Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)

	// The request wrote the event and nothing else.
	if got := crmDispatchStatuses(t, w.db, cid, "W-01"); len(got) != 0 {
		t.Fatalf("enrol must not dispatch W-01 inline: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "W-01"); n != 0 {
		t.Fatalf("W-01 inbox rows before the worker = %d", n)
	}
	evs := crmEventsOf(t, w.db, cid, "offer.finalized")
	if len(evs) != 1 || evs[0].Status != "NEW" || evs[0].Payload["offer_id"] != offerWelcomeLitre || evs[0].Payload["source"] != "promoter" {
		t.Fatalf("offer.finalized outbox row: %+v", evs)
	}

	// The worker sends it, exactly once, and drains the event.
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchStatuses(t, w.db, cid, "W-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("W-01 after the worker: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "W-01"); n != 1 {
		t.Fatalf("W-01 inbox rows after the worker = %d", n)
	}
	if evs = crmEventsOf(t, w.db, cid, "offer.finalized"); len(evs) != 1 || evs[0].Status != "DONE" {
		t.Fatalf("offer.finalized must be DONE: %+v", evs)
	}
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchStatuses(t, w.db, cid, "W-01"); len(got) != 1 {
		t.Fatalf("a second worker turn must not re-send W-01: %v", got)
	}
}

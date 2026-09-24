package consumer

// W-08 ("We don't deliver to your area yet ... that's the only message
// you'll get from us") is for a member we cannot serve: no orders and no
// saved address inside a zone. It used to fire on any out-of-zone check, so
// a member with a serviceable home address and orders who checked a far pin
// (the waitlist form, with a session) was told we do not deliver to them.
// The emitters now write both facts into serviceability.checked, W-08's
// conditions read them, and an event without them fails closed.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMW08 -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMW08OnlyForAMemberWeCannotServe(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	w.svc.deps.Cfg.AccessTokenTTL = time.Hour // the chain world sets no token lifetime
	w.svc.appKey = "test-app-key"
	h := &handler{svc: w.svc}

	moveFar := func(cid primitive.ObjectID) {
		t.Helper()
		if _, err := w.db.Collection(collAddresses).UpdateMany(ctx, bson.D{{Key: "consumer_id", Value: cid}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "lat", Value: 28.6139}, {Key: "lng", Value: 77.2090}, {Key: "pincode", Value: "110001"}}}}); err != nil {
			t.Fatalf("move address: %v", err)
		}
	}
	// waitlist posts the app's waitlist form for a far pin with the member's
	// session, as the app does from the "not in your area yet" screen.
	waitlist := func(cid primitive.ObjectID, phone string) {
		t.Helper()
		acct, _ := w.svc.repo.findAccountByID(ctx, cid)
		tok, err := w.svc.issueTokens(ctx, acct, time.Now().UTC())
		if err != nil {
			t.Fatalf("issueTokens: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/consumer/waitlist",
			strings.NewReader(`{"phone":"`+phone+`","pincode":"110001","name":"Far Pin","lat":28.6139,"lng":77.2090}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Parag-App-Key", "test-app-key")
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		rec := httptest.NewRecorder()
		h.joinWaitlist(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("waitlist: %d %s", rec.Code, rec.Body.String())
		}
	}
	facts := func(cid primitive.ObjectID) map[string]any {
		t.Helper()
		evs := crmEventsOf(t, w.db, cid, "serviceability.checked")
		if len(evs) != 1 {
			t.Fatalf("serviceability.checked events: %+v", evs)
		}
		return evs[0].Payload
	}

	// A: a serviceable home address and a delivered order, checks a far pin.
	served := w.customer(t, "9000007881", 1000)
	instantOrderDelivered(t, w, served)
	waitlist(served, "9000007881")
	if p := facts(served); p["in_zone"] != false || p["has_orders"] != true || p["has_serviceable_address"] != true {
		t.Fatalf("served member's facts: %+v", p)
	}
	// B: a serviceable home address, no order yet, checks a far pin.
	homeInZone := w.customer(t, "9000007882", 0)
	waitlist(homeInZone, "9000007882")
	if p := facts(homeInZone); p["has_orders"] != false || p["has_serviceable_address"] != true {
		t.Fatalf("in-zone member's facts: %+v", p)
	}
	// C: ordered before, every saved address now outside the zones.
	orderedFar := w.customer(t, "9000007883", 1000)
	instantOrderDelivered(t, w, orderedFar)
	moveFar(orderedFar)
	waitlist(orderedFar, "9000007883")
	if p := facts(orderedFar); p["has_orders"] != true || p["has_serviceable_address"] != false {
		t.Fatalf("ordered-far member's facts: %+v", p)
	}
	// D: no orders and no serviceable address: the member W-08 is for.
	outOfArea := w.customer(t, "9000007884", 0)
	moveFar(outOfArea)
	waitlist(outOfArea, "9000007884")
	if p := facts(outOfArea); p["has_orders"] != false || p["has_serviceable_address"] != false {
		t.Fatalf("out-of-area member's facts: %+v", p)
	}
	// E: an event without the facts (written before this change): fails closed.
	legacy := w.customer(t, "9000007885", 0)
	moveFar(legacy)
	w.svc.emitCRMEvent(ctx, "serviceability.checked", legacy, map[string]any{"in_zone": false, "pincode": "110001", "source": "waitlist"})

	w.svc.crmProcessEvents(ctx)
	for _, c := range []struct {
		name string
		cid  primitive.ObjectID
		want int
	}{
		{"served member with orders and a serviceable address", served, 0},
		{"member with a serviceable address", homeInZone, 0},
		{"member with orders", orderedFar, 0},
		{"member with no orders and no serviceable address", outOfArea, 1},
		{"event without the facts", legacy, 0},
	} {
		if got := crmDispatchStatuses(t, w.db, c.cid, "W-08"); len(got) != c.want || (c.want == 1 && got[0] != "SENT") {
			t.Fatalf("W-08 for the %s: %v, want %d SENT", c.name, got, c.want)
		}
	}
	if n := inboxCount(t, w.db, outOfArea, "W-08"); n != 1 {
		t.Fatalf("W-08 inbox rows for the out-of-area member: %d", n)
	}
	if n := inboxCount(t, w.db, served, "W-08"); n != 0 {
		t.Fatalf("W-08 reached a member we serve: %d inbox rows", n)
	}
}

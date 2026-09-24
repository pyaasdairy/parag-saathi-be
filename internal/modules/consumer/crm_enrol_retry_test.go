package consumer

// A Welcome Litre enrolment retried by the member who already has it (the
// app timed out and the member tapped again, or a double tap) must answer
// 409 ALREADY_ENROLLED, which app/welcome-offer.tsx routes to the tabs. The
// enrolment itself exhausts the 2+2 trial and registers the household claim,
// so the eligibility reads used to refuse the member's own finished offer as
// 422 NOT_ELIGIBLE and the app showed "Could not start the offer". Nobody
// else's eligibility changes: a member with trial activity and no offer is
// still NOT_ELIGIBLE.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMEnrolRetry -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestCRMEnrolRetryAnswersAlreadyEnrolled(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007871", 0)

	res, err := w.svc.crmSelfEnrol(ctx, cid, crmEnrolInput{})
	if err != nil {
		t.Fatalf("self enrol: %v", err)
	}
	if res.Pack1OrderID == "" || res.SubscriptionID == "" {
		t.Fatalf("self enrol result: %+v", res)
	}
	counts := func() (offers, subs, packs, enrolled int64) {
		t.Helper()
		var err error
		if offers, err = w.svc.repo.offers().CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
			t.Fatalf("offers: %v", err)
		}
		if subs, err = w.svc.repo.subscriptions.CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
			t.Fatalf("subscriptions: %v", err)
		}
		if packs, err = w.svc.repo.orders.CountDocuments(ctx, bson.D{
			{Key: "user_id", Value: cid.Hex()}, {Key: "offer_pack", Value: 1},
			{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
		}); err != nil {
			t.Fatalf("pack orders: %v", err)
		}
		return offers, subs, packs, crmEventCount(t, w, "offer_enrolled", cid)
	}
	o1, s1, p1, e1 := counts()
	if o1 != 1 || s1 != 1 || p1 != 1 || e1 != 1 {
		t.Fatalf("after the enrolment: offers %d, plans %d, pack-1 orders %d, offer_enrolled %d", o1, s1, p1, e1)
	}

	// The retry through the app's route: 409 ALREADY_ENROLLED.
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodPost, "/crm/enrol/self", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
	rec := httptest.NewRecorder()
	h.crmSelfEnrolHandler(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body["code"] != "ALREADY_ENROLLED" {
		t.Fatalf("self enrol retry: %d %v, want 409 ALREADY_ENROLLED", rec.Code, body)
	}
	// The operator console's enrol of the same household answers the same.
	acct, _ := w.svc.repo.findAccountByID(ctx, cid)
	if _, err := w.svc.crmEnrol(ctx, "swf-operator", crmEnrolInput{
		Phone: acct.Phone, Name: "Retry Household", Line1: "Shop St 1", Pincode: "226030", Lat: 26.7712, Lng: 81.0123,
	}); crmErrCode(err) != "ALREADY_ENROLLED" {
		t.Fatalf("promoter re-enrol: %v, want ALREADY_ENROLLED", err)
	}
	// Nothing was minted twice.
	if o2, s2, p2, e2 := counts(); o2 != o1 || s2 != s1 || p2 != p1 || e2 != e1 {
		t.Fatalf("a retry minted: offers %d, plans %d, pack-1 orders %d, offer_enrolled %d", o2, s2, p2, e2)
	}

	// Eligibility is not weakened for anyone else: a member with 2+2 trial
	// activity and no offer is still refused as NOT_ELIGIBLE.
	other := w.customer(t, "9000007872", 0)
	if _, err := w.svc.repo.getOrCreateTrial(ctx, other); err != nil {
		t.Fatalf("trial: %v", err)
	}
	if _, err := w.svc.repo.trials.UpdateOne(ctx, bson.D{{Key: "consumer_id", Value: other}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "delivered_paid", Value: 1}}}}); err != nil {
		t.Fatalf("trial activity: %v", err)
	}
	if _, err := w.svc.crmSelfEnrol(ctx, other, crmEnrolInput{}); crmErrCode(err) != "NOT_ELIGIBLE" {
		t.Fatalf("a member with trial activity: %v, want NOT_ELIGIBLE", err)
	}
}

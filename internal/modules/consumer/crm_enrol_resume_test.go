package consumer

// A Welcome Litre enrolment that stopped half way (the offer row inserted and
// the 2+2 trial marked used at step 5, then the plan or the pack-1 mint failed
// at step 6) left an UNFINISHED offer. GET /crm/eligibility already reports
// that member as already_enrolled, and the code promises "a retry resumes
// right here", but the retry met the trial check first and was refused 422
// NOT_ELIGIBLE forever: the member's own half-made enrolment blocked them.
// A retry now resumes the member's own unfinished offer, a finished one still
// answers 409 ALREADY_ENROLLED, and a double tap never refuses the member.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMEnrolResume -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type crmEnrolTally struct {
	offers, plans, packs, enrolled, finalized, claims int64
}

func crmEnrolCounts(t *testing.T, w *chainWorld, cid primitive.ObjectID) crmEnrolTally {
	t.Helper()
	ctx := context.Background()
	var out crmEnrolTally
	var err error
	if out.offers, err = w.svc.repo.offers().CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
		t.Fatalf("offers: %v", err)
	}
	if out.plans, err = w.svc.repo.subscriptions.CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
		t.Fatalf("plans: %v", err)
	}
	if out.packs, err = w.svc.repo.orders.CountDocuments(ctx, bson.D{
		{Key: "user_id", Value: cid.Hex()}, {Key: "offer_pack", Value: 1},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
	}); err != nil {
		t.Fatalf("pack orders: %v", err)
	}
	if out.claims, err = w.db.Collection("free_pack_claims").CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}}); err != nil {
		t.Fatalf("claims: %v", err)
	}
	out.enrolled = crmEventCount(t, w, "offer_enrolled", cid)
	out.finalized = crmEventCount(t, w, "offer.finalized", cid)
	return out
}

// crmSelfEnrolCall drives POST /crm/enrol/self for the member.
func crmSelfEnrolCall(t *testing.T, w *chainWorld, cid primitive.ObjectID) (int, map[string]any) {
	t.Helper()
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodPost, "/crm/enrol/self", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
	rec := httptest.NewRecorder()
	h.crmSelfEnrolHandler(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// assertUnfinished checks the state a failed step 6 leaves: one offer row with
// no pack-1 order, the 2+2 trial used, no plan and no pack left behind, and
// the eligibility read already answering already_enrolled.
func assertUnfinished(t *testing.T, w *chainWorld, cid primitive.ObjectID, label string) {
	t.Helper()
	ctx := context.Background()
	o, err := w.svc.repo.findOffer(ctx, cid)
	if err != nil || o == nil || o.Pack1OrderID != "" {
		t.Fatalf("%s: want an unfinished offer, got %+v (%v)", label, o, err)
	}
	tr, err := w.svc.repo.getOrCreateTrial(ctx, cid)
	if err != nil || tr.DeliveredPaid == 0 {
		t.Fatalf("%s: step 5 must have used the 2+2 trial: %+v (%v)", label, tr, err)
	}
	if got := crmEnrolCounts(t, w, cid); got.offers != 1 || got.plans != 0 || got.packs != 0 || got.enrolled != 0 {
		t.Fatalf("%s: after the failed step 6: %+v", label, got)
	}
	if st, _ := w.svc.crmEligibility(ctx, cid); st != "already_enrolled" {
		t.Fatalf("%s: eligibility %q, want already_enrolled", label, st)
	}
}

// assertResumed retries through the app's route and checks the enrolment
// finished exactly once, then that one more retry is 409 ALREADY_ENROLLED.
func assertResumed(t *testing.T, w *chainWorld, cid primitive.ObjectID, label string) {
	t.Helper()
	ctx := context.Background()
	code, body := crmSelfEnrolCall(t, w, cid)
	if code != http.StatusOK || body["pack1_order_id"] == "" || body["pack1_order_id"] == nil || body["subscription_id"] == "" {
		t.Fatalf("%s: the retry must resume the member's own offer: %d %v", label, code, body)
	}
	o, err := w.svc.repo.findOffer(ctx, cid)
	if err != nil || o == nil || o.Pack1OrderID != body["pack1_order_id"] || o.SubscriptionID != body["subscription_id"] {
		t.Fatalf("%s: the offer must be finalized with the reply's ids: %+v %v", label, o, body)
	}
	want := crmEnrolTally{offers: 1, plans: 1, packs: 1, enrolled: 1, finalized: 1, claims: 1}
	if got := crmEnrolCounts(t, w, cid); got != want {
		t.Fatalf("%s: after the resume: %+v, want %+v", label, got, want)
	}
	if code, body := crmSelfEnrolCall(t, w, cid); code != http.StatusConflict || body["code"] != "ALREADY_ENROLLED" {
		t.Fatalf("%s: a retry after the resume: %d %v, want 409 ALREADY_ENROLLED", label, code, body)
	}
	if got := crmEnrolCounts(t, w, cid); got != want {
		t.Fatalf("%s: the 409 minted: %+v", label, got)
	}
}

func TestCRMEnrolResumeAfterThePlanMintFailed(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007881", 0)

	// Inject the failure: the offer's plan SKU is not sellable for a moment,
	// so step 6's plan mint refuses after step 5 has used the trial.
	cfg := crmOfferConfig()
	var sku catalogDoc
	if err := w.db.Collection(collCatalog).FindOneAndDelete(ctx, bson.D{{Key: "sku_id", Value: cfg.SeedSKU}}).Decode(&sku); err != nil {
		t.Fatalf("take the plan SKU away: %v", err)
	}
	if _, err := w.svc.crmSelfEnrol(ctx, cid, crmEnrolInput{}); crmErrCode(err) != "SKU_UNAVAILABLE" {
		t.Fatalf("enrol with the plan SKU gone: %v, want SKU_UNAVAILABLE", err)
	}
	assertUnfinished(t, w, cid, "plan mint failed")

	if _, err := w.db.Collection(collCatalog).InsertOne(ctx, sku); err != nil {
		t.Fatalf("restore the plan SKU: %v", err)
	}
	assertResumed(t, w, cid, "plan mint failed")
}

func TestCRMEnrolResumeAfterThePackMintFailed(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007882", 0)

	// Inject the failure: the orders collection refuses any promotional pack
	// order, so the pack-1 mint fails after the plan was minted (and the
	// enrolment retracts that plan).
	setValidator := func(v bson.D) {
		t.Helper()
		if err := w.db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: collOrders}, {Key: "validator", Value: v},
		}).Err(); err != nil {
			t.Fatalf("orders validator: %v", err)
		}
	}
	setValidator(bson.D{{Key: "offer_pack", Value: bson.D{{Key: "$exists", Value: false}}}})
	if _, err := w.svc.crmSelfEnrol(ctx, cid, crmEnrolInput{}); err == nil {
		t.Fatalf("enrol with pack orders refused: want an error")
	}
	assertUnfinished(t, w, cid, "pack mint failed")

	setValidator(bson.D{})
	assertResumed(t, w, cid, "pack mint failed")

	// The operator console resuming someone else's half-made enrolment works
	// the same way (one state machine for both entries).
	other := w.customer(t, "9000007883", 0)
	setValidator(bson.D{{Key: "offer_pack", Value: bson.D{{Key: "$exists", Value: false}}}})
	acct, _ := w.svc.repo.findAccountByID(ctx, other)
	in := crmEnrolInput{Phone: acct.Phone, Name: "Resume House", Line1: "Shop St 1", Pincode: "226030", Lat: 26.7712, Lng: 81.0123}
	if _, err := w.svc.crmEnrol(ctx, "swf-operator", in); err == nil {
		t.Fatalf("operator enrol with pack orders refused: want an error")
	}
	assertUnfinished(t, w, other, "operator, pack mint failed")
	setValidator(bson.D{})
	res, err := w.svc.crmEnrol(ctx, "swf-operator", in)
	if err != nil || res.Pack1OrderID == "" {
		t.Fatalf("operator retry must resume: %v %+v", err, res)
	}
	if _, err := w.svc.crmEnrol(ctx, "swf-operator", in); crmErrCode(err) != "ALREADY_ENROLLED" {
		t.Fatalf("operator retry after the resume: %v, want ALREADY_ENROLLED", err)
	}
}

// The race window a double tap opens: this request read no offer, then the
// other tap inserted it and used the trial before this request's trial (or
// household) read, which refuses. The refusal is decided by re-reading the
// member's own offer: finished answers 409, unfinished is resumed, and only
// with no offer of their own does the NOT_ELIGIBLE stand.
func TestCRMEnrolResumeARefusalRereadsTheMembersOffer(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007884", 0)
	refusal := errUnprocessable("NOT_ELIGIBLE", "this customer already has welcome-trial activity")

	// No offer of their own: the refusal stands.
	if own, err := w.svc.crmOwnOfferAfterRefusal(ctx, cid, refusal); own != nil || crmErrCode(err) != "NOT_ELIGIBLE" {
		t.Fatalf("no offer: %+v %v, want the NOT_ELIGIBLE refusal", own, err)
	}
	// The other tap's offer, inserted and not yet finished: resume it.
	now := chainLedgerEpoch
	if err := w.svc.repo.insertOffer(ctx, &consumerOffer{
		ConsumerID: cid, OfferID: offerWelcomeLitre, EnrolledAt: now,
		Pack1State: pack1Pending, Pack2State: pack2Locked, Source: "self",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert offer: %v", err)
	}
	own, err := w.svc.crmOwnOfferAfterRefusal(ctx, cid, refusal)
	if err != nil || own == nil || own.ConsumerID != cid || own.Pack1OrderID != "" {
		t.Fatalf("unfinished offer: %+v %v, want it returned to resume", own, err)
	}
	// ...and once the other tap finished it: 409 ALREADY_ENROLLED.
	if _, err := w.svc.repo.offers().UpdateOne(ctx, bson.D{{Key: "consumer_id", Value: cid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "pack1_order_id", Value: "ord_other_tap"}}}}); err != nil {
		t.Fatalf("finish offer: %v", err)
	}
	if own, err := w.svc.crmOwnOfferAfterRefusal(ctx, cid, refusal); own != nil || crmErrCode(err) != "ALREADY_ENROLLED" {
		t.Fatalf("finished offer: %+v %v, want ALREADY_ENROLLED", own, err)
	}
}

// Two taps at once from a fresh member: whichever interleaving the two
// requests take, neither is ever refused as NOT_ELIGIBLE by the other's
// enrolment. Each tap ends in the enrolment or 409 ALREADY_ENROLLED, and the
// member ends with one finished offer, one plan (the offer's own), one pack-1
// order and one offer_enrolled.
func TestCRMEnrolResumeDoubleTapNeverRefusesTheMember(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		cid := w.customer(t, "90000079"+string(rune('0'+i/10))+string(rune('0'+i%10)), 0)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j := range errs {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				_, errs[j] = w.svc.crmSelfEnrol(ctx, cid, crmEnrolInput{})
			}(j)
		}
		wg.Wait()
		for j, err := range errs {
			if err != nil && crmErrCode(err) != "ALREADY_ENROLLED" {
				t.Fatalf("round %d tap %d: %v (%s), want the enrolment or ALREADY_ENROLLED", i, j, err, crmErrCode(err))
			}
		}
		o, err := w.svc.repo.findOffer(ctx, cid)
		if err != nil || o == nil || o.Pack1OrderID == "" {
			t.Fatalf("round %d: offer not finished: %+v (%v)", i, o, err)
		}
		var sub subscription
		if err := w.svc.repo.subscriptions.FindOne(ctx, bson.D{{Key: "subscription_id", Value: o.SubscriptionID}}).Decode(&sub); err != nil {
			t.Fatalf("round %d: the offer's plan %s is gone: %v", i, o.SubscriptionID, err)
		}
		want := crmEnrolTally{offers: 1, plans: 1, packs: 1, enrolled: 1, finalized: 1, claims: 1}
		if got := crmEnrolCounts(t, w, cid); got != want {
			t.Fatalf("round %d: %+v, want %+v", i, got, want)
		}
	}
}

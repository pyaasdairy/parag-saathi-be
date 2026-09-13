package consumer

// Regression tests for the two money/promise defects found in the 13 Sep 2026
// edge sweep. Both are Mongo-backed because both are about real stored state.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 CRM_ENABLED=true \
//	  go test ./internal/modules/consumer/ -run CRMP0 -v

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func crmP0Service(t *testing.T) (*service, *mongo.Database, func()) {
	t.Helper()
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the CRM P0 regression tests")
	}
	t.Setenv("CRM_ENABLED", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		cancel()
		t.Fatalf("mongo connect: %v", err)
	}
	db := client.Database("consumer_crm_p0_test")
	_ = db.Drop(ctx)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &deps.Deps{
		Cfg: &config.Config{JWTSecret: "crm-p0-test"},
		Log: log, DB: db, Flags: flags.NewService(db), Bus: eventbus.New(log),
	}
	repo := newRepository(db)
	svc := newService(d, repo, log)
	svc.ensureCRMIndexes(ctx)

	// A serving store + priced SKUs, so enrolment can resolve serviceability
	// and a plan price.
	if _, err := db.Collection("org_units").InsertOne(ctx, bson.D{
		{Key: "type", Value: "STORE"}, {Key: "active", Value: true},
		{Key: "name", Value: "PYAAS P0 Store"},
		{Key: "geo_lat", Value: 26.7700}, {Key: "geo_lng", Value: 81.0100},
	}); err != nil {
		cancel()
		t.Fatalf("seed store: %v", err)
	}
	for sku, pr := range map[string]float64{
		"gold-500ml": 35, "gold-1l": 69, "taaza-500ml": 29, "taaza-1l": 57,
	} {
		p := pr
		if _, err := db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
			SkuID: sku, Kind: catalogKindProduct, Price: &p,
			Name: "Milk " + sku, Category: "milk",
		}); err != nil {
			cancel()
			t.Fatalf("seed catalog: %v", err)
		}
	}
	return svc, db, func() { _ = db.Drop(ctx); _ = client.Disconnect(ctx); cancel() }
}

// P0-1: a household that ALREADY subscribes must never come out of enrolment
// with a second plan — that billed them twice every morning.
func TestCRMP0NoDoubleSubscriptionOnEnrol(t *testing.T) {
	svc, _, done := crmP0Service(t)
	defer done()
	ctx := context.Background()

	// The household subscribes normally first (no order yet, unfunded wallet —
	// exactly the state that still reads as `eligible`).
	res, err := svc.crmEnrol(ctx, "p0-test", crmEnrolInput{
		Phone: "9000000900", Name: "Existing Subscriber", Line1: "Flat 1, P0 Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("first enrol: %v", err)
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)

	subs, err := svc.repo.listSubscriptions(ctx, cid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("precondition: enrolment should leave exactly 1 plan, got %d", len(subs))
	}
	first := subs[0].SubscriptionID

	// Now the adoption path directly: a second call must reuse that plan, not
	// mint another. (crmEnsureSubscription is what enrolment calls.)
	sub, minted, err := svc.crmEnsureSubscription(ctx, cid, "gold-500ml", 2, "daily")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if minted {
		t.Fatal("a household with a live plan must ADOPT it, never mint a second")
	}
	if sub.SubscriptionID != first {
		t.Fatalf("adopted the wrong plan: %s want %s", sub.SubscriptionID, first)
	}
	after, _ := svc.repo.listSubscriptions(ctx, cid)
	if len(after) != 1 {
		t.Fatalf("DOUBLE SUBSCRIPTION: %d active plans — the customer would be billed twice daily", len(after))
	}
}

// A PAUSED plan still belongs to the household — adopting it must not mint a
// second plan either.
func TestCRMP0AdoptsPausedPlan(t *testing.T) {
	svc, _, done := crmP0Service(t)
	defer done()
	ctx := context.Background()
	cid := primitive.NewObjectID()

	created, err := svc.crmCreateSubscription(ctx, cid, "gold-500ml", 2, "daily")
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := svc.repo.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: created.SubscriptionID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "paused"}}}}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	sub, minted, err := svc.crmEnsureSubscription(ctx, cid, "gold-500ml", 2, "daily")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if minted || sub.SubscriptionID != created.SubscriptionID {
		t.Fatal("a paused plan must be adopted, not duplicated")
	}
}

// A household with NO plan still gets one — the fix must not starve the
// ordinary case.
func TestCRMP0MintsWhenNoPlanExists(t *testing.T) {
	svc, _, done := crmP0Service(t)
	defer done()
	sub, minted, err := svc.crmEnsureSubscription(context.Background(), primitive.NewObjectID(), "taaza-1l", 1, "daily")
	if err != nil || !minted || sub == nil {
		t.Fatalf("a fresh household must get a plan: minted=%v err=%v", minted, err)
	}
	if sub.ProductID != "taaza-1l" || sub.Qty != 1 {
		t.Fatalf("minted the wrong plan: %+v", sub)
	}
}

// P0-2: once the 7-day window has closed, the offer view must stop advertising
// a pack that a recharge can no longer earn — even before the 10:30 sweep flips
// the stored state.
func TestCRMP0ClosedWindowStopsPromisingPack2(t *testing.T) {
	svc, _, done := crmP0Service(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: svc}

	call := func(cid primitive.ObjectID) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/crm/offer", nil)
		req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey,
			consumerActor{ID: cid.Hex(), Phone: "+919000000901"}))
		rec := httptest.NewRecorder()
		h.crmMyOffer(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("offer view: %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Offer map[string]any `json:"offer"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Offer
	}

	// (a) INSIDE the window (delivered 2 days ago): still offered, with a real
	// deadline and a day count.
	open := primitive.NewObjectID()
	twoDaysAgo := time.Now().Add(-2 * 24 * time.Hour)
	if _, err := svc.repo.offers().InsertOne(ctx, consumerOffer{
		ConsumerID: open, OfferID: offerWelcomeLitre, EnrolledAt: twoDaysAgo,
		Pack1State: pack1Delivered, Pack2State: pack2Locked, FirstDeliveryAt: &twoDaysAgo,
	}); err != nil {
		t.Fatalf("seed open offer: %v", err)
	}
	v := call(open)
	if v["pack2_state"] != pack2Locked {
		t.Fatalf("inside the window pack 2 must still be on offer: %v", v["pack2_state"])
	}
	if v["pack2_recharge_by"] == nil || v["pack2_days_left"] == nil {
		t.Fatalf("inside the window the real deadline must be stated: %+v", v)
	}

	// (b) PAST the window (delivered 9 days ago) but before the sweep has run:
	// stored state is still locked, yet the view must report it closed.
	closed := primitive.NewObjectID()
	nineDaysAgo := time.Now().Add(-9 * 24 * time.Hour)
	if _, err := svc.repo.offers().InsertOne(ctx, consumerOffer{
		ConsumerID: closed, OfferID: offerWelcomeLitre, EnrolledAt: nineDaysAgo,
		Pack1State: pack1Delivered, Pack2State: pack2Locked, FirstDeliveryAt: &nineDaysAgo,
	}); err != nil {
		t.Fatalf("seed closed offer: %v", err)
	}
	v = call(closed)
	if v["pack2_state"] != pack2Expired {
		t.Fatalf("the closed window must report expired, got %v — the app would keep asking for ₹500", v["pack2_state"])
	}
	if v["pack2_recharge_by"] != nil || v["pack2_days_left"] != nil {
		t.Fatalf("a closed window must state no deadline: %+v", v)
	}

	// The STORED state is untouched — the sweep remains the authority, and
	// entitlement still derives from the real row.
	var stored consumerOffer
	if err := svc.repo.offers().FindOne(ctx, bson.D{{Key: "consumer_id", Value: closed}}).Decode(&stored); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Pack2State != pack2Locked {
		t.Fatalf("the view must not mutate stored state: %s", stored.Pack2State)
	}
	if got := entitledFreeDeliveries(&stored); got != 0 {
		t.Fatalf("entitlement must stay 0 for a closed window, got %d", got)
	}
}

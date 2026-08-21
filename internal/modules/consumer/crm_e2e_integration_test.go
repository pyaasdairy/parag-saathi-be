package consumer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
)

// TestCRMWelcomeLitreE2E walks the WHOLE Welcome Litre journey against a real
// Mongo through the SAME service functions production runs — no shortcuts into
// repo internals except to stage the world (store, catalog, rider) and to
// read back what the customer would see.
//
//	journey: enrol → pack-1 ₹0 order + delivery task → rider delivers (real
//	settle: ₹0 gate row consumes the ref) → worker → pack1=delivered + W-02 →
//	recharge ₹500 (settled) → worker → pack2 pending + pack-2 order minted +
//	W-04 → deliver pack 2 → W-05 → schedules: W-03a (day-0 unfunded), W-06
//	(day-3 nudge), W-07 day-7 expiry with the mandatory no-charge line →
//	has_paid_order stays false throughout (promo-only customer).
//
// Gated on CONSUMER_MONGO_TEST_URI like the other integration tests:
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 CRM_ENABLED=true \
//	  go test ./internal/modules/consumer/ -run CRMWelcomeLitreE2E -v
func TestCRMWelcomeLitreE2E(t *testing.T) {
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the CRM E2E")
	}
	t.Setenv("CRM_ENABLED", "true")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(ctx)
	db := client.Database("consumer_crm_e2e_test")
	_ = db.Drop(ctx)
	defer db.Drop(ctx)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &deps.Deps{
		Cfg: &config.Config{JWTSecret: "crm-e2e-secret"},
		Log: log, DB: db,
		Flags: flags.NewService(db),
		Bus:   eventbus.New(log),
	}
	repo := newRepository(db)
	svc := newService(d, repo, log)
	svc.ensureCRMIndexes(ctx)

	// ── Stage the world: one serving store + the campaign SKU priced ──
	if _, err := db.Collection("org_units").InsertOne(ctx, bson.D{
		{Key: "type", Value: "STORE"}, {Key: "active", Value: true},
		{Key: "name", Value: "PYAAS E2E Store"},
		{Key: "geo_lat", Value: 26.7700}, {Key: "geo_lng", Value: 81.0100},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	price := 35.0
	if _, err := db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
		SkuID: "gold-500ml", Kind: catalogKindProduct, Price: &price,
		Name: "Full Cream Milk - Parag Gold", Category: "milk",
	}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	const phone = "9000000042"

	// ── 1) ENROL (the promoter/ops route's service call) ──
	res, err := svc.crmEnrol(ctx, "e2e-operator", crmEnrolInput{
		Phone: phone, Name: "E2E Household", Line1: "Flat 101, E2E Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
		SocietyID: "SOC-E2E", PromoterID: "PRM-01", AssetType: "poster",
	})
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if res.AbuseFlagged {
		t.Fatal("first enrolment at a fresh address must not be abuse-flagged")
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)

	// Idempotency: a second enrol on the same phone must refuse, not double-mint.
	if _, err := svc.crmEnrol(ctx, "e2e-operator", crmEnrolInput{
		Phone: phone, Name: "E2E Household", Line1: "Flat 101, E2E Tower", Pincode: "226030",
	}); err == nil {
		t.Fatal("re-enrol must be refused (ALREADY_ENROLLED)")
	}

	// The pack-1 order: standalone, ₹0, promotional line, NO subscription id.
	p1, err := repo.findOrderAnyUser(ctx, res.Pack1OrderID)
	if err != nil || p1 == nil {
		t.Fatalf("pack1 order lookup: %v", err)
	}
	if p1.Total != 0 || p1.SubscriptionID != "" || p1.OfferPack != 1 {
		t.Fatalf("pack1 order shape wrong: total=%v sub=%q pack=%d", p1.Total, p1.SubscriptionID, p1.OfferPack)
	}
	if len(p1.Items) != 1 || !p1.Items[0].IsPromotional || p1.Items[0].Price != 0 || p1.Items[0].PromotionalValue != 35 {
		t.Fatalf("pack1 line wrong: %+v", p1.Items[0])
	}
	// The campaign subscription: a NORMAL plan the sweep can govern.
	var sub subscription
	if err := repo.subscriptions.FindOne(ctx, bson.D{{Key: "subscription_id", Value: res.SubscriptionID}}).Decode(&sub); err != nil {
		t.Fatalf("campaign subscription missing: %v", err)
	}
	if sub.ProductID != "gold-500ml" || sub.Qty != 2 || sub.UnitPrice != 35 {
		t.Fatalf("subscription shape wrong: %+v", sub)
	}
	// The 2+2 trial must be EXHAUSTED for the enrollee (exclusivity, Option B).
	tr, err := svc.trialFor(ctx, cid)
	if err != nil || tr.Phase != trialPhaseDone {
		t.Fatalf("trial not exhausted at enrol: phase=%v err=%v", tr.Phase, err)
	}
	// W-01 landed in the in-app inbox.
	if n := inboxCount(t, db, cid, "W-01"); n != 1 {
		t.Fatalf("W-01 inbox rows = %d, want 1", n)
	}

	// ── 2) DELIVER PACK 1 through the REAL rider settle ──
	deliverOrder(t, ctx, svc, repo, db, p1.OrderID)
	// The ₹0 settle must consume the exactly-once ref with a gate row…
	var gate walletTxn
	if err := repo.walletTxns.FindOne(ctx, bson.D{{Key: "ref_id", Value: "delivery:" + p1.OrderID}}).Decode(&gate); err != nil {
		t.Fatalf("₹0 gate row missing — the settle sweep would back-charge: %v", err)
	}
	if gate.Amount != 0 || gate.Status != "SUCCESS" {
		t.Fatalf("gate row wrong: %+v", gate)
	}
	// …and must NOT mark the customer paid (CH-19: promo-only settle).
	if hasPaid(t, db, cid) {
		t.Fatal("₹0 promo settle must not set has_paid_order")
	}

	// Worker turn: order.delivered → pack1 delivered + W-02.
	svc.crmProcessEvents(ctx)
	off := mustOffer(t, ctx, svc, cid)
	if off.Pack1State != pack1Delivered || off.FirstDeliveryAt == nil {
		t.Fatalf("pack1 state after delivery: %+v", off)
	}
	if n := inboxCount(t, db, cid, "W-02"); n != 1 {
		t.Fatalf("W-02 inbox rows = %d, want 1", n)
	}
	// Entitlement (CH-01, derived): pack 1 spent, pack 2 still locked → 0.
	if got := entitledFreeDeliveries(off); got != 0 {
		t.Fatalf("entitlement after pack1, pre-recharge: %d", got)
	}

	// ── 3) SCHEDULES, day 0, unfunded: W-03a fires once and only once ──
	sched := time.Date(2026, 8, 21, 11, 0, 0, 0, istZone)
	// pin "today" to the delivery day so day-diff = 0
	forceFirstDelivery(t, db, cid, sched.Add(-6*time.Hour))
	svc.crmProcessSchedules(ctx, sched)
	svc.crmProcessSchedules(ctx, sched.Add(5*time.Minute)) // same day again — dedup
	if n := inboxCount(t, db, cid, "W-03a"); n != 1 {
		t.Fatalf("W-03a rows = %d, want exactly 1 (dispatch-log dedup)", n)
	}

	// ── 4) RECHARGE ₹500 (settled) → pack 2 unlocks + order minted + W-04 ──
	if _, err := svc.creditTopup(ctx, cid, 500, "razorpay", "e2e-rzp-1"); err != nil {
		t.Fatalf("creditTopup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	off = mustOffer(t, ctx, svc, cid)
	if off.Pack2State != pack2Pending || off.Pack2OrderID == "" {
		t.Fatalf("pack2 not unlocked after settled recharge: %+v", off)
	}
	if n := inboxCount(t, db, cid, "W-04"); n != 1 {
		t.Fatalf("W-04 rows = %d, want 1", n)
	}
	if got := entitledFreeDeliveries(off); got != 1 {
		t.Fatalf("entitlement with pack2 pending: %d, want 1", got)
	}
	// Replayed webhook (same ref) must NOT mint a second pack-2.
	if _, err := svc.creditTopup(ctx, cid, 500, "razorpay", "e2e-rzp-1"); err != nil {
		t.Fatalf("dup creditTopup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	if n, _ := repo.orders.CountDocuments(ctx, bson.D{{Key: "offer_pack", Value: 2}, {Key: "user_id", Value: cid.Hex()}}); n != 1 {
		t.Fatalf("pack2 orders = %d, want 1 (webhook replay must dedupe)", n)
	}

	// ── 5) DELIVER PACK 2 → delivered + W-05; journey complete ──
	deliverOrder(t, ctx, svc, repo, db, off.Pack2OrderID)
	svc.crmProcessEvents(ctx)
	off = mustOffer(t, ctx, svc, cid)
	if off.Pack2State != pack2Delivered {
		t.Fatalf("pack2 state after delivery: %s", off.Pack2State)
	}
	if n := inboxCount(t, db, cid, "W-05"); n != 1 {
		t.Fatalf("W-05 rows = %d, want 1", n)
	}
	if hasPaid(t, db, cid) {
		t.Fatal("customer still promo-only — has_paid_order must remain false")
	}

	// ── 6) THE EXPIRY BRANCH on a second, unfunded household ──
	res2, err := svc.crmEnrol(ctx, "e2e-operator", crmEnrolInput{
		Phone: "9000000043", Name: "Expiry Household", Line1: "Flat 501, E2E Tower",
		Pincode: "226030", Lat: 26.7726, Lng: 81.0157,
	})
	if err != nil {
		t.Fatalf("enrol #2: %v", err)
	}
	cid2, _ := primitive.ObjectIDFromHex(res2.ConsumerID)
	deliverOrder(t, ctx, svc, repo, db, res2.Pack1OrderID)
	svc.crmProcessEvents(ctx)
	// Pretend pack 1 landed 8 days ago; day-3 nudge then day-7 expiry.
	forceFirstDelivery(t, db, cid2, time.Now().In(istZone).AddDate(0, 0, -8))
	svc.crmProcessSchedules(ctx, time.Date(2026, 8, 29, 11, 0, 0, 0, istZone))
	off2 := mustOffer(t, ctx, svc, cid2)
	if off2.Pack2State != pack2Expired {
		t.Fatalf("pack2 must expire past the grace window: %s", off2.Pack2State)
	}
	if n := inboxCount(t, db, cid2, "W-07"); n != 1 {
		t.Fatalf("W-07 rows = %d, want 1", n)
	}
	// The mandatory no-charge line must be IN the message the customer sees.
	var w7 bson.M
	if err := db.Collection(collConsumerInbox).FindOne(ctx, bson.D{
		{Key: "consumer_id", Value: cid2}, {Key: "trigger_id", Value: "W-07"},
	}).Decode(&w7); err != nil {
		t.Fatalf("read W-07: %v", err)
	}
	body, _ := w7["body_en"].(string)
	if body == "" || !strings.Contains(body, "Nothing has been charged") {
		t.Fatalf("W-07 body lost the mandatory no-charge line: %q", body)
	}
	// An expired offer entitles nothing.
	if got := entitledFreeDeliveries(off2); got != 0 {
		t.Fatalf("expired offer entitlement: %d", got)
	}
	// A recharge AFTER expiry must not resurrect pack 2 (CAS from locked only).
	if _, err := svc.creditTopup(ctx, cid2, 500, "razorpay", "e2e-rzp-2"); err != nil {
		t.Fatalf("post-expiry topup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	off2 = mustOffer(t, ctx, svc, cid2)
	if off2.Pack2State != pack2Expired {
		t.Fatalf("post-expiry recharge resurrected pack2: %s", off2.Pack2State)
	}

	// ── 7) ABUSE FLAG: third enrolment at household #1's address ──
	res3, err := svc.crmEnrol(ctx, "e2e-operator", crmEnrolInput{
		Phone: "9000000044", Name: "Same Address", Line1: "Flat 101, E2E Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("enrol #3 (same address) must flag, not refuse: %v", err)
	}
	if !res3.AbuseFlagged {
		t.Fatal("duplicate-address enrolment must carry the abuse flag")
	}

	// ── 7b) RECHARGE BEFORE DELIVERY (the excited signup): household #3 has
	// pack 1 still pending — a settled ₹500 must unlock pack 2 anyway. The
	// grace window can't have started before the first delivery exists.
	cid3, _ := primitive.ObjectIDFromHex(res3.ConsumerID)
	if _, err := svc.creditTopup(ctx, cid3, 500, "razorpay", "e2e-rzp-3"); err != nil {
		t.Fatalf("pre-delivery topup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	off3 := mustOffer(t, ctx, svc, cid3)
	if off3.Pack2State != pack2Pending || off3.Pack2OrderID == "" {
		t.Fatalf("recharge BEFORE pack-1 delivery must still unlock pack 2: %+v", off3)
	}

	// ── 7c) DAY-7 BOUNDARY: the W-06 nudge advertises day 7's DATE, so a
	// day-7 recharge must unlock and the sweep must NOT expire until day 8.
	res5, err := svc.crmEnrol(ctx, "e2e-operator", crmEnrolInput{
		Phone: "9000000045", Name: "Boundary Household", Line1: "Flat 901, E2E Tower",
		Pincode: "226030", Lat: 26.7729, Lng: 81.0161,
	})
	if err != nil {
		t.Fatalf("enrol #5: %v", err)
	}
	cid5, _ := primitive.ObjectIDFromHex(res5.ConsumerID)
	deliverOrder(t, ctx, svc, repo, db, res5.Pack1OrderID)
	svc.crmProcessEvents(ctx)
	forceFirstDelivery(t, db, cid5, time.Now().In(istZone).AddDate(0, 0, -7)) // today = day 7
	svc.crmProcessSchedules(ctx, time.Now().In(istZone))                      // must NOT expire on day 7
	if o5 := mustOffer(t, ctx, svc, cid5); o5.Pack2State != pack2Locked {
		t.Fatalf("sweep expired pack 2 ON day 7 — the advertised recharge day: %s", o5.Pack2State)
	}
	if _, err := svc.creditTopup(ctx, cid5, 500, "razorpay", "e2e-rzp-5"); err != nil {
		t.Fatalf("day-7 topup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	if o5 := mustOffer(t, ctx, svc, cid5); o5.Pack2State != pack2Pending || o5.Pack2OrderID == "" {
		t.Fatalf("day-7 recharge (the advertised deadline) must unlock pack 2: %+v", o5)
	}

	// ── 7d) WALLET-HEALTH NUDGES for the GENERAL base (B-01 / B-02): a
	// normal subscriber (never CRM-enrolled) with a low wallet must be told.
	nAcct := &account{ID: primitive.NewObjectID(), Phone: "+919000000077", Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.insertAccount(ctx, nAcct); err != nil {
		t.Fatalf("normal subscriber account: %v", err)
	}
	nSub := &subscription{MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(),
		ConsumerID: nAcct.ID, ProductID: "gold-500ml", Name: "Full Cream Milk - Parag Gold",
		Qty: 2, UnitPrice: 35, Frequency: "daily", Status: "active",
		StartDate: istDay(time.Now()), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if _, err := repo.subscriptions.InsertOne(ctx, nSub); err != nil {
		t.Fatalf("normal subscription: %v", err)
	}
	if _, err := svc.creditTopup(ctx, nAcct.ID, 50, "razorpay", "e2e-rzp-n1"); err != nil {
		t.Fatalf("normal topup: %v", err)
	}
	svc.crmProcessEvents(ctx) // consume the recharge event (no offer → no-op)
	// ₹50 wallet vs ₹70/day burn: cover 0.7 days < 4 → B-01 at the 09:00 sweep.
	bday := time.Date(2026, 8, 25, 9, 5, 0, 0, istZone)
	svc.crmProcessSchedules(ctx, bday)
	if n := inboxCount(t, db, nAcct.ID, "B-01"); n != 1 {
		t.Fatalf("B-01 low-balance nudge = %d rows, want 1", n)
	}
	svc.crmProcessSchedules(ctx, bday.Add(10*time.Minute)) // same day again — sweep claim dedupes
	if n := inboxCount(t, db, nAcct.ID, "B-01"); n != 1 {
		t.Fatalf("B-01 must not repeat within the day: %d", n)
	}
	// 17:00: ₹50 cannot cover tomorrow's ₹70 → the critical B-02 cut-off alert.
	svc.crmProcessSchedules(ctx, time.Date(2026, 8, 25, 17, 5, 0, 0, istZone))
	if n := inboxCount(t, db, nAcct.ID, "B-02"); n != 1 {
		t.Fatalf("B-02 shortfall alert = %d rows, want 1", n)
	}
	// Next day, still short: B-01 stays quiet (once per 7 days) but the
	// critical B-02 fires again — the spec's critical_exempt_from_daily_cap.
	nday := time.Date(2026, 8, 26, 17, 5, 0, 0, istZone)
	svc.crmProcessSchedules(ctx, time.Date(2026, 8, 26, 9, 5, 0, 0, istZone))
	svc.crmProcessSchedules(ctx, nday)
	if n := inboxCount(t, db, nAcct.ID, "B-01"); n != 1 {
		t.Fatalf("B-01 repeated inside its 7-day cycle: %d", n)
	}
	if n := inboxCount(t, db, nAcct.ID, "B-02"); n != 2 {
		t.Fatalf("critical B-02 must re-fire daily while short: %d rows, want 2", n)
	}
	// A LIVE Welcome Litre household is excluded — the campaign owns its nudges.
	if !svc.crmInLiveWelcomeJourney(ctx, cid3) {
		t.Fatal("household #3 (pack2 pending) must count as a live welcome journey")
	}
	if svc.crmInLiveWelcomeJourney(ctx, nAcct.ID) {
		t.Fatal("a never-enrolled subscriber must not count as a welcome journey")
	}

	// ── 8) OFF SWITCH: with CRM disabled, dispatch + schedules are inert ──
	t.Setenv("CRM_ENABLED", "")
	before := inboxTotal(t, db)
	svc.crmDispatch(ctx, "W-06", cid2, nil)
	svc.crmProcessSchedules(ctx, time.Now().In(istZone))
	if after := inboxTotal(t, db); after != before {
		t.Fatalf("disabled CRM still wrote %d inbox rows", after-before)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// deliverOrder drives an order's delivery task through the REAL rider path:
// assign → out-for-delivery → deliverDelivery (proof + geofence + settle).
func deliverOrder(t *testing.T, ctx context.Context, svc *service, repo *repository, db *mongo.Database, orderID string) {
	t.Helper()
	var d delivery
	if err := db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: orderID}}).Decode(&d); err != nil {
		t.Fatalf("delivery task for %s missing: %v", orderID, err)
	}
	if d.PaymentMode != "PREPAID" {
		t.Fatalf("promo delivery must be PREPAID (exactly-once ref), got %s", d.PaymentMode)
	}
	const rider = "e2e-rider-party"
	if _, err := db.Collection(collDeliveries).UpdateOne(ctx,
		bson.D{{Key: "delivery_id", Value: d.ID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "rider_party_id", Value: rider}, {Key: "status", Value: "OUT_FOR_DELIVERY"},
		}}}); err != nil {
		t.Fatalf("stage rider: %v", err)
	}
	_, err := svc.deliverDelivery(ctx, e2eActor(rider), d.ID, deliverInput{
		ProofPhoto: "https://s3.example/proof.jpg",
		Geo:        &geoPt{Lat: d.Geo.Lat, Lng: d.Geo.Lng},
		GeofenceOK: true,
	})
	if err != nil {
		t.Fatalf("deliverDelivery(%s): %v", orderID, err)
	}
}

func mustOffer(t *testing.T, ctx context.Context, svc *service, cid primitive.ObjectID) *consumerOffer {
	t.Helper()
	o, err := svc.repo.findOffer(ctx, cid)
	if err != nil || o == nil {
		t.Fatalf("offer lookup: %v", err)
	}
	return o
}

func inboxCount(t *testing.T, db *mongo.Database, cid primitive.ObjectID, trigger string) int {
	t.Helper()
	n, err := db.Collection(collConsumerInbox).CountDocuments(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}})
	if err != nil {
		t.Fatalf("inbox count: %v", err)
	}
	return int(n)
}

func inboxTotal(t *testing.T, db *mongo.Database) int {
	t.Helper()
	n, _ := db.Collection(collConsumerInbox).CountDocuments(context.Background(), bson.D{})
	return int(n)
}

func hasPaid(t *testing.T, db *mongo.Database, cid primitive.ObjectID) bool {
	t.Helper()
	var doc struct {
		HasPaidOrder bool `bson:"has_paid_order"`
	}
	_ = db.Collection("consumer_accounts").FindOne(context.Background(), bson.D{{Key: "_id", Value: cid}}).Decode(&doc)
	return doc.HasPaidOrder
}

func forceFirstDelivery(t *testing.T, db *mongo.Database, cid primitive.ObjectID, at time.Time) {
	t.Helper()
	if _, err := db.Collection(collConsumerOffers).UpdateOne(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "offer_id", Value: offerWelcomeLitre}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "first_delivery_at", Value: at.UTC()}}}}); err != nil {
		t.Fatalf("force first_delivery_at: %v", err)
	}
}

func e2eActor(partyID string) auth.Actor {
	return auth.Actor{PartyID: partyID, Kind: "role", RoleCode: "DELIVERY_RIDER"}
}

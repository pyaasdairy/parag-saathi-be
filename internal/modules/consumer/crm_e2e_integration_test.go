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
	for sku, pr := range map[string]float64{
		"gold-500ml": 35, "gold-1l": 69, "taaza-500ml": 29, "taaza-1l": 57,
	} {
		p := pr
		if _, err := db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
			SkuID: sku, Kind: catalogKindProduct, Price: &p,
			Name: "Milk " + sku, Category: "milk",
		}); err != nil {
			t.Fatalf("seed catalog %s: %v", sku, err)
		}
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
	planFromBeforeNoon(t, ctx, repo, res.SubscriptionID)

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
	// W-01 lands in the in-app inbox from the WORKER, not the enrol request.
	if n := inboxCount(t, db, cid, "W-01"); n != 0 {
		t.Fatalf("W-01 inbox rows before the worker = %d, want 0", n)
	}
	svc.crmProcessEvents(ctx)
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
	// Anchor on TODAY (not a frozen date): step 4's recharge runs on the real
	// clock, and the IST-day grace predicate must see day 0 — a hardcoded
	// date made this test rot after seven days.
	nowIST := time.Now().In(istZone)
	sched := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 11, 0, 0, 0, istZone)
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
	svc.crmProcessSchedules(ctx, sched.AddDate(0, 0, 8))
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
	planFromBeforeNoon(t, ctx, repo, res3.SubscriptionID)
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
	planFromBeforeNoon(t, ctx, repo, res5.SubscriptionID)
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
	bday := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 9, 5, 0, 0, istZone).AddDate(0, 0, 4)
	svc.crmProcessSchedules(ctx, bday)
	if n := inboxCount(t, db, nAcct.ID, "B-01"); n != 1 {
		t.Fatalf("B-01 low-balance nudge = %d rows, want 1", n)
	}
	svc.crmProcessSchedules(ctx, bday.Add(10*time.Minute)) // same day again — sweep claim dedupes
	if n := inboxCount(t, db, nAcct.ID, "B-01"); n != 1 {
		t.Fatalf("B-01 must not repeat within the day: %d", n)
	}
	// 17:00: ₹50 cannot cover tomorrow's ₹70 → the critical B-02 cut-off alert.
	svc.crmProcessSchedules(ctx, time.Date(bday.Year(), bday.Month(), bday.Day(), 17, 5, 0, 0, istZone))
	if n := inboxCount(t, db, nAcct.ID, "B-02"); n != 1 {
		t.Fatalf("B-02 shortfall alert = %d rows, want 1", n)
	}
	// Next day, still short: B-01 stays quiet (once per 7 days) but the
	// critical B-02 fires again — the spec's critical_exempt_from_daily_cap.
	nday := time.Date(bday.Year(), bday.Month(), bday.Day(), 17, 5, 0, 0, istZone).AddDate(0, 0, 1)
	svc.crmProcessSchedules(ctx, time.Date(nday.Year(), nday.Month(), nday.Day(), 9, 5, 0, 0, istZone))
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

	// ── 7e) SELF-SERVE ENROLMENT (terms §3.1 — the app's own funnel) ──
	selfAcct := &account{ID: primitive.NewObjectID(), Phone: "+919000000050", Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.insertAccount(ctx, selfAcct); err != nil {
		t.Fatalf("self account: %v", err)
	}
	// Eligibility BEFORE an address: the funnel must route to address capture.
	if st, err := svc.crmEligibility(ctx, selfAcct.ID); err != nil || st != "address_required" {
		t.Fatalf("eligibility without address = %q (%v), want address_required", st, err)
	}
	sLat, sLng := 26.7726, 81.0152
	if _, err := repo.addresses.InsertOne(ctx, &address{
		ID: primitive.NewObjectID(), ConsumerID: selfAcct.ID, Label: "Home",
		Line1: "Flat 202, Self Tower", Pincode: "226030", City: "Lucknow",
		IsDefault: true, Lat: &sLat, Lng: &sLng, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("self address: %v", err)
	}
	if st, _ := svc.crmEligibility(ctx, selfAcct.ID); st != "eligible" {
		t.Fatalf("eligibility with address = %q, want eligible", st)
	}
	// Plan choice honoured (§6.3): Toned 1 L, alternate mornings.
	selfRes, err := svc.crmSelfEnrol(ctx, selfAcct.ID, crmEnrolInput{
		PlanProductID: "taaza-1l", PlanQty: 1, PlanFrequency: "alternate",
	})
	if err != nil {
		t.Fatalf("self-enrol: %v", err)
	}
	var selfSub subscription
	if err := repo.subscriptions.FindOne(ctx, bson.D{{Key: "subscription_id", Value: selfRes.SubscriptionID}}).Decode(&selfSub); err != nil {
		t.Fatalf("self subscription: %v", err)
	}
	if selfSub.ProductID != "taaza-1l" || selfSub.Qty != 1 || selfSub.Frequency != "alternate" {
		t.Fatalf("plan not honoured: %+v", selfSub)
	}
	planFromBeforeNoon(t, ctx, repo, selfRes.SubscriptionID)
	selfOffer := mustOffer(t, ctx, svc, selfAcct.ID)
	if selfOffer.Source != "self" || selfOffer.Pack1OrderID == "" {
		t.Fatalf("self offer shape: source=%q pack1=%q", selfOffer.Source, selfOffer.Pack1OrderID)
	}
	// The FREE pack stays the seed SKU regardless of the chosen plan.
	sp1, _ := repo.findOrderAnyUser(ctx, selfRes.Pack1OrderID)
	if sp1 == nil || len(sp1.Items) != 1 || sp1.Items[0].ProductID != "gold-500ml" {
		t.Fatalf("free pack must be the seed SKU: %+v", sp1)
	}
	if st, _ := svc.crmEligibility(ctx, selfAcct.ID); st != "already_enrolled" {
		t.Fatal("post-enrol eligibility must read already_enrolled")
	}
	// A plan below the 1 L/day milk floor is refused.
	if _, err := svc.crmSelfEnrol(ctx, selfAcct.ID, crmEnrolInput{PlanProductID: "taaza-500ml", PlanQty: 1}); err == nil {
		t.Fatal("below-floor plan must be refused (and re-enrol conflicts anyway)")
	}
	// A PAYING customer's eligibility is not_eligible — the funnel never shows.
	if st, _ := svc.crmEligibility(ctx, cid); st == "eligible" {
		t.Fatal("household #1 (recharged, enrolled) must never read eligible")
	}
	// Out-of-zone address → not_serviceable (funnel routes to the waitlist).
	farAcct := &account{ID: primitive.NewObjectID(), Phone: "+919000000051", Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_ = repo.insertAccount(ctx, farAcct)
	fLat, fLng := 27.5000, 81.0000 // ~80 km north of the store fence
	_, _ = repo.addresses.InsertOne(ctx, &address{
		ID: primitive.NewObjectID(), ConsumerID: farAcct.ID, Label: "Home",
		Line1: "Far Away House", Pincode: "261001", IsDefault: true,
		Lat: &fLat, Lng: &fLng, CreatedAt: time.Now().UTC(),
	})
	if st, _ := svc.crmEligibility(ctx, farAcct.ID); st != "not_serviceable" {
		t.Fatalf("far address eligibility = %q, want not_serviceable", st)
	}
	if _, err := svc.crmSelfEnrol(ctx, farAcct.ID, crmEnrolInput{}); err == nil {
		t.Fatal("out-of-zone self-enrol must be refused")
	}

	// ── 7f) PACK-2 RIDES THE NEXT DELIVERY (terms §4.4–4.5) ──
	// Deliver the self household's pack 1, PAUSE their plan, then recharge:
	// the pack must unlock but NOT mint a lone-drop order until a real
	// delivery morning exists again.
	deliverOrder(t, ctx, svc, repo, db, selfRes.Pack1OrderID)
	svc.crmProcessEvents(ctx)
	if _, err := repo.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: selfRes.SubscriptionID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "paused"}}}}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := svc.creditTopup(ctx, selfAcct.ID, 500, "razorpay", "e2e-rzp-self"); err != nil {
		t.Fatalf("self topup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	selfOffer = mustOffer(t, ctx, svc, selfAcct.ID)
	if selfOffer.Pack2State != pack2Pending || selfOffer.Pack2OrderID != "" {
		t.Fatalf("paused plan: pack2 must be pending WITHOUT an order (no lone drop): %+v", selfOffer)
	}
	if selfOffer.Pack2UnlockedAt == nil {
		t.Fatal("pack2_unlocked_at must anchor the 14-day window")
	}
	// Sweep ticks while paused: still no order.
	svc.crmProcessSchedules(ctx, time.Now().In(istZone))
	if o := mustOffer(t, ctx, svc, selfAcct.ID); o.Pack2OrderID != "" {
		t.Fatal("sweep must not attach while the plan is paused")
	}
	// RESUME → the next tick attaches the pack to tomorrow's delivery + W-04.
	if _, err := repo.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: selfRes.SubscriptionID}},
		bson.D{{Key: "$set", Value: bson.D{
			// Re-anchor on TOMORROW — the app's reactivate does exactly this, and
			// an alternate-day plan anchored today would (correctly) not be due
			// tomorrow, so the attach would keep waiting for its next due morning.
			{Key: "status", Value: "active"}, {Key: "start_date", Value: istDay(time.Now().Add(24 * time.Hour))},
		}}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	svc.crmProcessSchedules(ctx, time.Now().In(istZone))
	selfOffer = mustOffer(t, ctx, svc, selfAcct.ID)
	if selfOffer.Pack2OrderID == "" {
		t.Fatal("resumed + funded: the sweep must attach pack 2 to tomorrow")
	}
	if n := inboxCount(t, db, selfAcct.ID, "W-04"); n != 1 {
		t.Fatalf("W-04 fires at ATTACH time, once: %d", n)
	}

	// ── 7g) THE 14-DAY LAPSE: unlocked but never delivered within the window ──
	lapseRes, err := svc.crmEnrol(ctx, "e2e-operator", crmEnrolInput{
		Phone: "9000000052", Name: "Lapse Household", Line1: "Flat 303, Self Tower",
		Pincode: "226030", Lat: 26.7727, Lng: 81.0159,
	})
	if err != nil {
		t.Fatalf("enrol lapse household: %v", err)
	}
	lapseCid, _ := primitive.ObjectIDFromHex(lapseRes.ConsumerID)
	deliverOrder(t, ctx, svc, repo, db, lapseRes.Pack1OrderID)
	svc.crmProcessEvents(ctx)
	_, _ = repo.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: lapseRes.SubscriptionID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "paused"}}}})
	if _, err := svc.creditTopup(ctx, lapseCid, 500, "razorpay", "e2e-rzp-lapse"); err != nil {
		t.Fatalf("lapse topup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	// Pretend the recharge settled 15 days ago and the plan stayed paused.
	old := time.Now().UTC().AddDate(0, 0, -15)
	_, _ = repo.offers().UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: lapseCid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "pack2_unlocked_at", Value: old}}}})
	svc.crmProcessSchedules(ctx, time.Now().In(istZone))
	if o := mustOffer(t, ctx, svc, lapseCid); o.Pack2State != pack2Expired || o.Pack2OrderID != "" {
		t.Fatalf("14-day lapse must expire the pending pack: %+v", o)
	}

	// ── 7h) ERASE → RE-SIGNUP CANNOT RE-ARM (the household claim registry) ──
	if err := svc.erase(ctx, selfAcct.ID); err != nil {
		t.Fatalf("erase: %v", err)
	}
	rebornAcct := &account{ID: primitive.NewObjectID(), Phone: "+919000000050", Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.insertAccount(ctx, rebornAcct); err != nil {
		t.Fatalf("reborn account: %v", err)
	}
	rLat, rLng := 26.7726, 81.0152
	_, _ = repo.addresses.InsertOne(ctx, &address{
		ID: primitive.NewObjectID(), ConsumerID: rebornAcct.ID, Label: "Home",
		Line1: "Flat 202, Self Tower", Pincode: "226030", IsDefault: true,
		Lat: &rLat, Lng: &rLng, CreatedAt: time.Now().UTC(),
	})
	if st, _ := svc.crmEligibility(ctx, rebornAcct.ID); st != "not_eligible" {
		t.Fatalf("erase-and-resignup eligibility = %q, want not_eligible (claim survives)", st)
	}
	if _, err := svc.crmSelfEnrol(ctx, rebornAcct.ID, crmEnrolInput{}); err == nil {
		t.Fatal("erase-and-resignup self-enrol must be refused — one welcome per household, forever")
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

// TestCommerceFunnelE2E walks the COMMERCE lifecycle — product buy → rider
// claim → pickup → delivered → wallet settle — for a NEW user and an EXISTING
// user, asserting the SALES-FUNNEL state (GET /crm/eligibility's answer) at
// every checkpoint. Proves the Welcome Litre engine and the ordinary shop
// coexist without touching each other's money or state.
func TestCommerceFunnelE2E(t *testing.T) {
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the commerce E2E")
	}
	t.Setenv("CRM_ENABLED", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(ctx)
	db := client.Database("consumer_commerce_e2e_test")
	_ = db.Drop(ctx)
	defer db.Drop(ctx)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &deps.Deps{Cfg: &config.Config{JWTSecret: "commerce-e2e"}, Log: log, DB: db,
		Flags: flags.NewService(db), Bus: eventbus.New(log)}
	repo := newRepository(db)
	svc := newService(d, repo, log)
	svc.ensureCRMIndexes(ctx)
	if _, err := db.Collection("org_units").InsertOne(ctx, bson.D{
		{Key: "type", Value: "STORE"}, {Key: "active", Value: true},
		{Key: "name", Value: "PYAAS Commerce Store"},
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

	// buyAndDeliver drives the REAL instant-lane lifecycle: order → OFFERED
	// task → first-accept-wins claim → pickup → deliver (proof + settle).
	buyAndDeliver := func(t *testing.T, cid primitive.ObjectID, rider string) *order {
		t.Helper()
		o, err := svc.createOrder(ctx, cid.Hex(), orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "x", Price: 1, Qty: 2}},
			PaymentMethod: "wallet", Lane: "instant", Priority: "normal",
			Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
		})
		if err != nil {
			t.Fatalf("createOrder: %v", err)
		}
		if o.Total != 85 { // server repricing: 2 × ₹35 + ₹15 instant fee; client price ignored
			t.Fatalf("server reprice: total=%v want 85", o.Total)
		}
		var task delivery
		if err := db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&task); err != nil {
			t.Fatalf("delivery task missing: %v", err)
		}
		if task.Status != "OFFERED" {
			t.Fatalf("instant task must broadcast OFFERED: %s", task.Status)
		}
		ra := e2eActor(rider)
		if _, err := svc.claimOfferedDelivery(ctx, ra, task.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		// Second rider loses the first-accept-wins race.
		if _, err := svc.claimOfferedDelivery(ctx, e2eActor("late-rider"), task.ID); err == nil {
			t.Fatal("second claim must be refused")
		}
		if _, err := svc.pickupDelivery(ctx, ra, task.ID); err != nil {
			t.Fatalf("pickup: %v", err)
		}
		if _, err := svc.deliverDelivery(ctx, ra, task.ID, deliverInput{
			ProofPhoto: "https://s3.example/p.jpg",
			Geo:        &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
		}); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		return o
	}

	// ── NEW USER: funded wallet, buys BEFORE ever touching the offer ──
	newAcct := &account{ID: primitive.NewObjectID(), Phone: "+919000000060", Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.insertAccount(ctx, newAcct); err != nil {
		t.Fatalf("account: %v", err)
	}
	nLat, nLng := 26.7712, 81.0123
	_, _ = repo.addresses.InsertOne(ctx, &address{ID: primitive.NewObjectID(), ConsumerID: newAcct.ID,
		Label: "Home", Line1: "Shop St 1", Pincode: "226030", IsDefault: true,
		Lat: &nLat, Lng: &nLng, CreatedAt: time.Now().UTC()})
	if _, err := svc.creditTopup(ctx, newAcct.ID, 500, "razorpay", "e2e-com-1"); err != nil {
		t.Fatalf("topup: %v", err)
	}
	svc.crmProcessEvents(ctx) // no offer → the recharge event is a clean no-op
	// FUNNEL before buying: a fresh, in-zone household reads eligible.
	if st, _ := svc.crmEligibility(ctx, newAcct.ID); st != "eligible" {
		t.Fatalf("fresh funded user funnel = %q, want eligible", st)
	}
	o1 := buyAndDeliver(t, newAcct.ID, "rider-A")
	// Wallet debited at the door, exactly once.
	wv, _ := svc.wallet(ctx, newAcct.ID)
	if wv.Available != 415 { // 500 - (70 + ₹15 instant fee)
		t.Fatalf("wallet after paid delivery: %v want 415", wv.Available)
	}
	if !hasPaid(t, db, newAcct.ID) {
		t.Fatal("paid settle must set has_paid_order")
	}
	// FUNNEL after buying: a PAYING household is no longer pitched the offer.
	if st, _ := svc.crmEligibility(ctx, newAcct.ID); st != "not_eligible" {
		t.Fatalf("paying user funnel = %q, want not_eligible", st)
	}
	// And self-enrol is refused server-side even if the app somehow asked.
	if _, err := svc.crmSelfEnrol(ctx, newAcct.ID, crmEnrolInput{}); err == nil {
		t.Fatal("paying customer self-enrol must be refused")
	}
	_ = o1

	// ── EXISTING USER: same commerce path, funnel stays hidden throughout ──
	oldAcct := &account{ID: primitive.NewObjectID(), Phone: "+919000000061", Status: "ACTIVE",
		HasPaidOrder: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.insertAccount(ctx, oldAcct); err != nil {
		t.Fatalf("existing account: %v", err)
	}
	_, _ = repo.addresses.InsertOne(ctx, &address{ID: primitive.NewObjectID(), ConsumerID: oldAcct.ID,
		Label: "Home", Line1: "Shop St 2", Pincode: "226030", IsDefault: true,
		Lat: &nLat, Lng: &nLng, CreatedAt: time.Now().UTC()})
	if _, err := svc.creditTopup(ctx, oldAcct.ID, 200, "razorpay", "e2e-com-2"); err != nil {
		t.Fatalf("existing topup: %v", err)
	}
	svc.crmProcessEvents(ctx)
	if st, _ := svc.crmEligibility(ctx, oldAcct.ID); st != "not_eligible" {
		t.Fatalf("existing user funnel = %q, want not_eligible", st)
	}
	buyAndDeliver(t, oldAcct.ID, "rider-B")
	wv2, _ := svc.wallet(ctx, oldAcct.ID)
	if wv2.Available != 115 { // 200 - 85
		t.Fatalf("existing wallet after delivery: %v want 115", wv2.Available)
	}
	if st, _ := svc.crmEligibility(ctx, oldAcct.ID); st != "not_eligible" {
		t.Fatal("existing user funnel must stay hidden after another purchase")
	}
	// The recharge events for non-enrolled users must never create offer state.
	if o, _ := repo.findOffer(ctx, oldAcct.ID); o != nil {
		t.Fatal("a plain shopper must have NO offer document")
	}
}

// planFromBeforeNoon dates a campaign plan to 09:00 IST today, before
// tomorrow's 12:00 cut-off. Pack 2 attaches only to a morning the plan
// really delivers (the noon rule: a plan created after the cut-off starts
// the day after tomorrow), so without this the journey's attach steps
// passed in the morning and failed every afternoon.
func planFromBeforeNoon(t *testing.T, ctx context.Context, repo *repository, subID string) {
	t.Helper()
	ist := time.Now().In(istZone)
	at := time.Date(ist.Year(), ist.Month(), ist.Day(), 9, 0, 0, 0, istZone).UTC()
	if _, err := repo.subscriptions.UpdateOne(ctx, bson.D{{Key: "subscription_id", Value: subID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "created_at", Value: at}, {Key: "changed_at", Value: at}}}}); err != nil {
		t.Fatalf("date the plan: %v", err)
	}
}

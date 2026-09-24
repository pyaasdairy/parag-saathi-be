package consumer

// THE FULL CHAIN, end to end, against a real Mongo: a customer places an order,
// the PYAAS store manager sees it in the console and assigns a rider, the rider
// accepts, picks up and delivers, and the customer's order reads "delivered"
// with the wallet debited exactly once.
//
// WHY THIS EXISTS ALONGSIDE THE CRM E2Es. TestCRMWelcomeLitreE2E and
// TestCommerceFunnelE2E prove the promo engine and the INSTANT lane (where a
// rider claims a broadcast OFFERED task). Neither one ever touches:
//   - the STORE MANAGER — storeOrders / assignRider were never called, because
//     both tests either use the instant claim or write rider_party_id straight
//     into Mongo, so the roster and the role checks were untested;
//   - the MORNING lane — sweepSubscriptionOrders had no Mongo test at all, so
//     the path an actual daily-milk customer takes every single morning was
//     never exercised;
//   - the customer-visible order state machine (placed → assigned →
//     out_for_delivery → delivered) asserted from the CONSUMER's side;
//   - repo.ensureIndexes, which is where the exactly-once money gate lives —
//     without it a "debited only once" assertion passes for the wrong reason.
//
// Run:
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 CRM_ENABLED=true \
//	  go test ./internal/modules/consumer/ -run FullChain -v

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type chainWorld struct {
	svc     *service
	db      *mongo.Database
	storeID primitive.ObjectID
	mgr     auth.Actor
	rider   auth.Actor
	riderID primitive.ObjectID
}

// newChainWorld stages a production-shaped world: indexes (including the money
// gate), one serving store, priced SKUs, and a store manager + rider who are
// REAL role holders on that store's roster.
func newChainWorld(t *testing.T) (*chainWorld, func()) {
	t.Helper()
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the full-chain E2E")
	}
	t.Setenv("CRM_ENABLED", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		cancel()
		t.Fatalf("mongo connect: %v", err)
	}
	db := client.Database("consumer_fullchain_e2e")
	_ = db.Drop(ctx)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &deps.Deps{
		Cfg: &config.Config{JWTSecret: "fullchain-e2e", OTPDevMode: true},
		Log: log, DB: db, Flags: flags.NewService(db), Bus: eventbus.New(log),
	}
	repo := newRepository(db)
	// The REAL indexes — this is what makes "debited exactly once" meaningful.
	if err := repo.ensureIndexes(ctx); err != nil {
		cancel()
		t.Fatalf("ensureIndexes: %v", err)
	}
	if _, err := repo.ensureSubscriptionDayIndex(ctx); err != nil {
		cancel()
		t.Fatalf("ensureSubscriptionDayIndex: %v", err)
	}
	svc := newService(d, repo, log)
	svc.ensureCRMIndexes(ctx)
	// Phase-2 feature indexes are NON-fatal in production, so ensureIndexes no
	// longer builds them — build them here so the race guards under test are
	// the ones production has when the build succeeds.
	if err := repo.ensureComplaintIndexes(ctx); err != nil {
		cancel()
		t.Fatalf("complaint indexes: %v", err)
	}
	if err := repo.ensurePushIndexes(ctx); err != nil {
		cancel()
		t.Fatalf("push indexes: %v", err)
	}
	// The growth programmes' uniqueness guards: one member row per consumer
	// (a concurrent join cannot take two seats or two Rs 99) and one referral
	// per referee (a concurrent apply cannot pay two rewards).
	if err := repo.ensureFoundingIndexes(ctx); err != nil {
		cancel()
		t.Fatalf("founding indexes: %v", err)
	}
	if err := repo.ensureReferralIndexes(ctx); err != nil {
		cancel()
		t.Fatalf("referral indexes: %v", err)
	}
	// The noon lock's as-of wallet replay reads by this index.
	if err := repo.ensureWalletAsOfIndex(ctx); err != nil {
		cancel()
		t.Fatalf("wallet as-of index: %v", err)
	}

	storeID := primitive.NewObjectID()
	if _, err := db.Collection("org_units").InsertOne(ctx, bson.D{
		{Key: "_id", Value: storeID},
		{Key: "type", Value: "STORE"}, {Key: "active", Value: true},
		{Key: "name", Value: "PYAAS Full-Chain Store"},
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

	// A real manager and a real rider on THIS store's roster. Party ids must be
	// valid ObjectIDs — storeForActor/ridersForStore parse them as such.
	mgrID, riderID := primitive.NewObjectID(), primitive.NewObjectID()
	for _, p := range []struct {
		id   primitive.ObjectID
		name string
		role string
	}{{mgrID, "Store Manager", "STORE_MANAGER"}, {riderID, "Ravi Rider", "DELIVERY_RIDER"}} {
		if _, err := db.Collection("parties").InsertOne(ctx, bson.D{
			{Key: "_id", Value: p.id}, {Key: "full_name", Value: p.name},
			{Key: "phone", Value: "+9199000000" + p.role[:2]},
		}); err != nil {
			cancel()
			t.Fatalf("seed party: %v", err)
		}
		if _, err := db.Collection("role_assignments").InsertOne(ctx, bson.D{
			{Key: "party_id", Value: p.id}, {Key: "role_code", Value: p.role},
			{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: storeID},
		}); err != nil {
			cancel()
			t.Fatalf("seed role: %v", err)
		}
	}

	w := &chainWorld{
		svc: svc, db: db, storeID: storeID, riderID: riderID,
		mgr:   auth.Actor{PartyID: mgrID.Hex(), Kind: "role", RoleCode: "STORE_MANAGER"},
		rider: auth.Actor{PartyID: riderID.Hex(), Kind: "role", RoleCode: "DELIVERY_RIDER"},
	}
	return w, func() { _ = db.Drop(ctx); _ = client.Disconnect(ctx); cancel() }
}

// customer creates an ACTIVE account with a default in-zone address, funded to
// `fund` rupees through the settled-recharge path. The top-up's ledger row is
// stamped at chainLedgerEpoch, long before any simulated lock moment: the
// noon lock reads the wallet as it stood at 12:00 (walletAsOf), and a seed
// row stamped with the real clock would otherwise fall inside a simulated
// day's window whenever the wall clock happened to be in it.
func (w *chainWorld) customer(t *testing.T, phone string, fund float64) primitive.ObjectID {
	t.Helper()
	ctx := context.Background()
	acct := &account{
		ID: primitive.NewObjectID(), Phone: "+91" + phone, Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := w.svc.repo.insertAccount(ctx, acct); err != nil {
		t.Fatalf("account: %v", err)
	}
	lat, lng := 26.7712, 81.0123
	if _, err := w.db.Collection(collAddresses).InsertOne(ctx, &address{
		ID: primitive.NewObjectID(), ConsumerID: acct.ID, Label: "Home",
		Line1: "Shop St 1", Pincode: "226030", City: "Lucknow",
		IsDefault: true, Lat: &lat, Lng: &lng, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("address: %v", err)
	}
	if fund > 0 {
		if _, err := w.svc.creditTopup(ctx, acct.ID, fund, "razorpay", "fund-"+phone); err != nil {
			t.Fatalf("fund wallet: %v", err)
		}
		chainStampLedger(t, w, acct.ID, "fund-"+phone, "TOPUP", chainLedgerEpoch)
	}
	return acct.ID
}

func (w *chainWorld) cash(t *testing.T, cid primitive.ObjectID) float64 {
	t.Helper()
	wl, err := w.svc.wallet(context.Background(), cid)
	if err != nil {
		t.Fatalf("wallet: %v", err)
	}
	return wl.Cash
}

func (w *chainWorld) orderByID(t *testing.T, orderID string) *order {
	t.Helper()
	var o order
	if err := w.db.Collection(collOrders).FindOne(context.Background(),
		bson.D{{Key: "order_id", Value: orderID}}).Decode(&o); err != nil {
		t.Fatalf("reload order %s: %v", orderID, err)
	}
	return &o
}

// ── 1) THE WHOLE CHAIN for a brand-new customer, morning lane ───────────────

func TestFullChainNewCustomerOrderToDelivered(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000001001", 500)
	if got := w.cash(t, cid); got != 500 {
		t.Fatalf("funded wallet: %v want 500", got)
	}

	// (a) the customer's cart becomes an order. The server reprices: 2×₹35 = ₹70
	// subtotal, under ₹199 so the ₹15 delivery fee applies → ₹85.
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Chain Tester", Phone: "9000001001",
		Total: 1, // a lying client total — the server must ignore it
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	if ord.Total != 85 {
		t.Fatalf("server repricing: total %v want 85 (2x35 + 15 fee)", ord.Total)
	}
	if ord.Status != "placed" {
		t.Fatalf("new order status %q want placed", ord.Status)
	}
	if got := w.cash(t, cid); got != 500 {
		t.Fatalf("placing an order must not move money yet: cash %v", got)
	}

	// (b) THE STORE MANAGER's console: the order is waiting in the queue.
	queue, err := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeOrders (manager console): %v", err)
	}
	var task *delivery
	for i := range queue {
		if queue[i].OrderID == ord.OrderID {
			task = &queue[i]
		}
	}
	if task == nil {
		t.Fatalf("the manager cannot see the order: %d tasks in queue", len(queue))
	}
	if task.Status != "ASSIGNED" {
		t.Fatalf("morning-lane task should start ASSIGNED (manager assigns), got %q", task.Status)
	}

	// (c) the manager assigns the rider off the store's roster.
	assigned, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex())
	if err != nil {
		t.Fatalf("assignRider: %v", err)
	}
	if assigned.RiderPartyID != w.riderID.Hex() {
		t.Fatalf("rider not attached: %+v", assigned.RiderPartyID)
	}

	// (d) the rider accepts, picks up, delivers — with proof + geofence.
	if _, err := w.svc.acceptDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("acceptDelivery: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickupDelivery: %v", err)
	}
	// the customer sees it moving BEFORE it lands
	if o := w.orderByID(t, ord.OrderID); o.Status != "out_for_delivery" {
		t.Fatalf("after pickup the customer's order should read out_for_delivery, got %q", o.Status)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{
		ProofPhoto: "https://example.test/proof.jpg", ProofNote: "handed over",
		Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliverDelivery: %v", err)
	}

	// (e) the customer's side: delivered, and the wallet debited ONCE.
	final := w.orderByID(t, ord.OrderID)
	if final.Status != "delivered" {
		t.Fatalf("final order status %q want delivered", final.Status)
	}
	if got := w.cash(t, cid); got != 415 {
		t.Fatalf("wallet after delivery: %v want 415 (500 - 85)", got)
	}
	n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: cid},
		{Key: "ref_id", Value: "delivery:" + ord.OrderID},
	})
	if n != 1 {
		t.Fatalf("expected exactly ONE debit row for this order, got %d", n)
	}

	// (f) a paid delivery makes the household ineligible for the free-litre
	// campaign — the funnel and the shop agreeing.
	if st, _ := w.svc.crmEligibility(ctx, cid); st != "not_eligible" {
		t.Fatalf("after paying, funnel should read not_eligible, got %q", st)
	}
}

// ── 2) THE MORNING SUBSCRIPTION LANE — what a daily-milk customer lives ─────

func TestFullChainSubscriptionMorningDelivery(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000001002", 500)
	// taaza (not gold) so the 2+2 trial pricing never masks the real charge.
	const D = "2026-10-06"
	sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{
		ProductID: "taaza-500ml", Name: "Milk taaza-500ml", Qty: 2,
		Frequency: "daily", StartDate: D,
	}, chainPlanMadeAt)
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	if sub.UnitPrice != 29 {
		t.Fatalf("server price authority: unit %v want 29", sub.UnitPrice)
	}

	// The sweep at 03:00 on D materialises D's delivery: the catch-up branch,
	// for a plan that predates D's noon cut-off (a plan created now would
	// start on the first editable day under the noon rule), before D's route
	// leaves (a day whose route has left is not caught up). A fixed day, never
	// the wall clock: the catch-up of "today" depends on the hour.
	chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -2), 9, 0))
	placed := w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 3, 0))
	if placed < 1 {
		t.Fatalf("sweep placed %d orders — the morning lane produced nothing", placed)
	}

	queue, err := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeOrders: %v", err)
	}
	var task *delivery
	for i := range queue {
		if queue[i].Lane == "morning" {
			task = &queue[i]
			break
		}
	}
	if task == nil {
		t.Fatalf("the subscription delivery never reached the store queue (%d tasks)", len(queue))
	}

	if _, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex()); err != nil {
		t.Fatalf("assignRider: %v", err)
	}
	if _, err := w.svc.acceptDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{
		ProofPhoto: "https://example.test/p.jpg",
		Geo:        &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	final := w.orderByID(t, task.OrderID)
	if final.Status != "delivered" {
		t.Fatalf("morning order final status %q want delivered", final.Status)
	}
	// 2 × ₹29 = ₹58, no delivery fee on the subscription lane.
	if got := w.cash(t, cid); got != 442 {
		t.Fatalf("wallet after morning delivery: %v want 442 (500 - 58)", got)
	}
}

// ── 3) THE MANAGER'S GUARD RAILS ───────────────────────────────────────────

func TestFullChainManagerAuthorityIsEnforced(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000001003", 500)
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1",
		Lane: "morning", ConsumerName: "Guard Test", Phone: "9000001003",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	queue, _ := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	var task *delivery
	for i := range queue {
		if queue[i].OrderID == ord.OrderID {
			task = &queue[i]
		}
	}
	if task == nil {
		t.Fatal("order not in queue")
	}

	// A RIDER must not be able to drive the manager's console.
	if _, err := w.svc.storeOrders(ctx, w.rider, w.storeID.Hex()); err == nil {
		t.Fatal("a rider must not be able to read the store's order queue")
	}
	if _, err := w.svc.assignRider(ctx, w.rider, w.storeID.Hex(), task.ID, w.riderID.Hex()); err == nil {
		t.Fatal("a rider must not be able to assign deliveries")
	}
	// A stranger with no role on this store must be refused.
	stranger := auth.Actor{PartyID: primitive.NewObjectID().Hex(), Kind: "role", RoleCode: "STORE_MANAGER"}
	if _, err := w.svc.storeOrders(ctx, stranger, w.storeID.Hex()); err == nil {
		t.Fatal("a manager with no assignment on this store must be refused")
	}
	// Someone who is not the assigned rider must not deliver it.
	if _, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex()); err != nil {
		t.Fatalf("assign: %v", err)
	}
	other := auth.Actor{PartyID: primitive.NewObjectID().Hex(), Kind: "role", RoleCode: "DELIVERY_RIDER"}
	if _, err := w.svc.acceptDelivery(ctx, other, task.ID); err == nil {
		t.Fatal("a rider the task was not assigned to must not accept it")
	}
}

// ── 4) MONEY SAFETY on the delivery leg ────────────────────────────────────

func TestFullChainDeliveryDebitsExactlyOnce(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000001004", 500)
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1",
		Lane: "morning", ConsumerName: "Money Test", Phone: "9000001004",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	queue, _ := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	var task *delivery
	for i := range queue {
		if queue[i].OrderID == ord.OrderID {
			task = &queue[i]
		}
	}
	if _, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex()); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := w.svc.acceptDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	in := deliverInput{ProofPhoto: "https://example.test/p.jpg",
		Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, in); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	after := w.cash(t, cid)

	// A retried delivery event (flaky network, double tap, replayed sync) must
	// never charge the customer twice.
	for i := 0; i < 3; i++ {
		_, _ = w.svc.deliverDelivery(ctx, w.rider, task.ID, in)
	}
	if got := w.cash(t, cid); got != after {
		t.Fatalf("REPLAYED DELIVERY CHARGED AGAIN: %v then %v", after, got)
	}
	n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: cid}, {Key: "ref_id", Value: "delivery:" + ord.OrderID},
	})
	if n != 1 {
		t.Fatalf("debit rows for this order: %d want 1", n)
	}
}

// ── 5) THE FREE-PACK HOUSEHOLD through the same manager+rider chain ─────────

func TestFullChainWelcomeLitrePackThroughStore(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	res, err := w.svc.crmEnrol(ctx, "fullchain", crmEnrolInput{
		Phone: "9000001005", Name: "Free Pack Household", Line1: "Flat 9, Chain Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)

	// The ₹0 promo pack is a real order that a real rider must deliver.
	queue, err := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeOrders: %v", err)
	}
	var task *delivery
	for i := range queue {
		if queue[i].OrderID == res.Pack1OrderID {
			task = &queue[i]
		}
	}
	if task == nil {
		t.Fatalf("the free pack never reached the store queue (%d tasks)", len(queue))
	}
	if task.Amount != 0 {
		t.Fatalf("the promo task must carry ₹0, got %v", task.Amount)
	}

	if _, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex()); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := w.svc.acceptDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{
		ProofPhoto: "https://example.test/p.jpg",
		Geo:        &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Free milk must not make them a "paying" customer, and must not charge.
	if got := w.cash(t, cid); got != 0 {
		t.Fatalf("a free pack must never move money: cash %v", got)
	}
	w.svc.crmProcessEvents(ctx)
	var off consumerOffer
	if err := w.svc.repo.offers().FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}}).Decode(&off); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if off.Pack1State != pack1Delivered {
		t.Fatalf("pack 1 should be delivered, got %q", off.Pack1State)
	}
	var acct account
	if err := w.svc.repo.accounts.FindOne(ctx, bson.D{{Key: "_id", Value: cid}}).Decode(&acct); err != nil {
		t.Fatalf("account: %v", err)
	}
	if acct.HasPaidOrder {
		t.Fatal("a ₹0 promotional delivery must NEVER mark the household as having paid")
	}
}

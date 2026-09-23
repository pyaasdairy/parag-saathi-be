package consumer

// The exactly-once claim is scoped to the product event: two instant orders
// on one day each get their own D-01 and D-06, while a scheduled trigger
// (W-07) still claims once per day. The index migration stamps legacy rows
// with scope_key "" and keeps a per-day claim taken under the old index
// taken.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMClaim -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// instantOrderDelivered drives one instant order through the real rider
// lane: create -> OFFERED -> first-accept claim -> pickup -> deliver.
func instantOrderDelivered(t *testing.T, w *chainWorld, cid primitive.ObjectID) *order {
	t.Helper()
	ctx := context.Background()
	o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
		ConsumerName: "Scope Tester", Phone: "9000007201",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	var task delivery
	if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&task); err != nil {
		t.Fatalf("delivery task for %s missing: %v", o.OrderID, err)
	}
	if task.Status != "OFFERED" {
		t.Fatalf("instant task must broadcast OFFERED, got %s", task.Status)
	}
	if _, err := w.svc.claimOfferedDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("claimOfferedDelivery: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickupDelivery: %v", err)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{
		ProofPhoto: "https://example.test/proof.jpg",
		Geo:        &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliverDelivery: %v", err)
	}
	return o
}

// crmDispatchScopes lists a consumer's dispatch rows for one trigger as
// scope_key -> status.
func crmDispatchScopes(t *testing.T, db *mongo.Database, cid primitive.ObjectID, trigger string) map[string]string {
	t.Helper()
	cur, err := db.Collection(collCRMDispatch).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		t.Fatalf("dispatch rows: %v", err)
	}
	var rows []crmDispatchRow
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("dispatch rows decode: %v", err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.ScopeKey] = r.Status
	}
	return out
}

func TestCRMClaimScopePerOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000007201", 1000)
	o1 := instantOrderDelivered(t, w, cid)
	o2 := instantOrderDelivered(t, w, cid)
	w.svc.crmProcessEvents(ctx)

	for _, trigger := range []string{"D-01", "D-06"} {
		got := crmDispatchScopes(t, w.db, cid, trigger)
		if len(got) != 2 || got[o1.OrderID] != "SENT" || got[o2.OrderID] != "SENT" {
			t.Fatalf("%s must be SENT once per order (scoped claim), got %v", trigger, got)
		}
		if n := inboxCount(t, w.db, cid, trigger); n != 2 {
			t.Fatalf("%s inbox rows = %d, want 2", trigger, n)
		}
	}
	// A replay of the same event is still exactly-once: the scope is taken.
	if st, _ := w.svc.crmDispatchWith(ctx, "D-06", cid, map[string]string{"LABELLED_PRODUCT": "x"}, time.Now().UTC(),
		crmDispatchOpts{Scope: o1.OrderID}); st != "" {
		t.Fatalf("a second claim on the same order scope must report nothing, got %q", st)
	}
	if got := crmDispatchScopes(t, w.db, cid, "D-06"); len(got) != 2 {
		t.Fatalf("a replayed scope must not add a row: %v", got)
	}

	// W-07 keeps the plain per-day claim: two sweeps on one day, one row.
	res, err := w.svc.crmEnrol(ctx, "scope-test-operator", crmEnrolInput{
		Phone: "9000007202", Name: "Expiry Household", Line1: "Flat 2, Scope Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	pcid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
	nowIST := time.Now().In(istZone)
	sched := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 11, 0, 0, 0, istZone)
	forceFirstDelivery(t, w.db, pcid, sched.AddDate(0, 0, -9))
	w.svc.crmProcessSchedules(ctx, sched)
	w.svc.crmProcessSchedules(ctx, sched.Add(time.Minute))
	if got := crmDispatchScopes(t, w.db, pcid, "W-07"); len(got) != 1 || got[""] != "SENT" {
		t.Fatalf("W-07 must claim once per day with an empty scope, got %v", got)
	}
}

// crmIndexNames lists a collection's index names.
func crmIndexNames(t *testing.T, coll *mongo.Collection) map[string]bool {
	t.Helper()
	cur, err := coll.Indexes().List(context.Background())
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var specs []bson.M
	if err := cur.All(context.Background(), &specs); err != nil {
		t.Fatalf("decode indexes: %v", err)
	}
	names := map[string]bool{}
	for _, sp := range specs {
		if n, _ := sp["name"].(string); n != "" {
			names[n] = true
		}
	}
	return names
}

func TestCRMClaimIndexMigration(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	// The production dispatch log after ensureCRMIndexes: scoped index in
	// place, no legacy index.
	live := crmIndexNames(t, w.svc.repo.crmDispatchCol())
	if !live[crmClaimIndexScoped] || live[crmClaimIndexLegacy] {
		t.Fatalf("live dispatch log indexes: %v", live)
	}

	// A pre-migration log: the auto-named legacy index and a row with no
	// scope_key, then the migration.
	legacy := w.db.Collection("crm_dispatch_log_legacy_probe")
	if _, err := legacy.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "trigger_id", Value: 1}, {Key: "consumer_id", Value: 1}, {Key: "ist_day", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		t.Fatalf("legacy index: %v", err)
	}
	cid := primitive.NewObjectID()
	if _, err := legacy.InsertOne(ctx, bson.D{
		{Key: "trigger_id", Value: "W-07"}, {Key: "consumer_id", Value: cid}, {Key: "ist_day", Value: "2026-09-24"},
		{Key: "category", Value: "service_implicit"}, {Key: "status", Value: "SENT"}, {Key: "created_at", Value: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	if !w.svc.ensureCRMClaimIndex(ctx, legacy) {
		t.Fatal("migration must succeed on a clean legacy log")
	}
	names := crmIndexNames(t, legacy)
	if !names[crmClaimIndexScoped] || names[crmClaimIndexLegacy] {
		t.Fatalf("after migration: %v", names)
	}
	var row struct {
		ScopeKey *string `bson:"scope_key"`
	}
	if err := legacy.FindOne(ctx, bson.D{{Key: "trigger_id", Value: "W-07"}}).Decode(&row); err != nil || row.ScopeKey == nil || *row.ScopeKey != "" {
		t.Fatalf("legacy row must be stamped with an empty scope_key: %+v (%v)", row, err)
	}
	// The per-day claim taken under the old index stays taken.
	if _, err := legacy.InsertOne(ctx, bson.D{
		{Key: "trigger_id", Value: "W-07"}, {Key: "consumer_id", Value: cid}, {Key: "ist_day", Value: "2026-09-24"},
		{Key: "scope_key", Value: ""}, {Key: "status", Value: "CLAIMED"}, {Key: "created_at", Value: time.Now().UTC()},
	}); err == nil || !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("a second per-day claim must be refused by the scoped index, got %v", err)
	}
	// The same day, a different scope, is a different claim.
	if _, err := legacy.InsertOne(ctx, bson.D{
		{Key: "trigger_id", Value: "W-07"}, {Key: "consumer_id", Value: cid}, {Key: "ist_day", Value: "2026-09-24"},
		{Key: "scope_key", Value: "ord_1"}, {Key: "status", Value: "CLAIMED"}, {Key: "created_at", Value: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("a scoped claim on the same day must be admitted: %v", err)
	}
	// Idempotent: the second boot finds no legacy index to drop and succeeds.
	if !w.svc.ensureCRMClaimIndex(ctx, legacy) {
		t.Fatal("a second migration run must succeed")
	}

	// A log whose legacy rows collide on the scoped key: the build is refused,
	// the guard reports it, and nothing is dropped.
	dirty := w.db.Collection("crm_dispatch_log_dirty_probe")
	if _, err := dirty.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "trigger_id", Value: 1}, {Key: "consumer_id", Value: 1}, {Key: "ist_day", Value: 1}},
		Options: options.Index().SetName(crmClaimIndexLegacy),
	}); err != nil {
		t.Fatalf("dirty legacy index: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := dirty.InsertOne(ctx, bson.D{
			{Key: "trigger_id", Value: "B-01"}, {Key: "consumer_id", Value: cid}, {Key: "ist_day", Value: "2026-09-24"},
			{Key: "status", Value: "SENT"}, {Key: "created_at", Value: time.Now().UTC()},
		}); err != nil {
			t.Fatalf("dirty row: %v", err)
		}
	}
	if w.svc.ensureCRMClaimIndex(ctx, dirty) {
		t.Fatal("a refused build must report false")
	}
	if n := w.svc.crmCountClaimDuplicates(ctx, dirty); n != 1 {
		t.Fatalf("duplicate keys = %d, want 1", n)
	}
	names = crmIndexNames(t, dirty)
	if names[crmClaimIndexScoped] || !names[crmClaimIndexLegacy] {
		t.Fatalf("refused build must leave the legacy index in force: %v", names)
	}
}

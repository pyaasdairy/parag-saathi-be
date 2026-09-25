package consumer

// OPERATOR PUSH (founder decision 8): the store alerts that matter ring the
// Saathi phones they concern, over FCM, through the operator device registry
// (POST /api/v1/push/register). Recorded here with a stand-in sender over the
// REAL registry and role assignments:
//
//   - a new instant order (OFFERED) rings the store's managers and its riders,
//     never another store's, a revoked rider or a party with no store role;
//     it rings in quiet hours (an order alert) and a dead token is pruned;
//   - the closing alert rings the managers once, in quiet hours too (a
//     delivery alert); the "now closed" notice waits out quiet hours (its
//     inbox row still lands) and rings when the close is in the day;
//   - low stock rings the admins only when the low set changes.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 \
//	  go test ./internal/modules/consumer/ -run OperatorPush -v

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/push"
)

// opPushRecorder stands in for FCM: it records every message and answers
// UNREGISTERED for the tokens marked dead.
type opPushRecorder struct {
	mu   sync.Mutex
	sent []push.FCMMessage
	dead map[string]bool
}

func (r *opPushRecorder) Enabled() bool { return true }

func (r *opPushRecorder) Send(_ context.Context, m push.FCMMessage) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dead[m.Token] {
		return "", push.ErrUnregistered
	}
	r.sent = append(r.sent, m)
	return "projects/p/messages/1", nil
}

// take returns and clears what was sent so far.
func (r *opPushRecorder) take() []push.FCMMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.sent
	r.sent = nil
	return out
}

// opPushWire plugs the recorder into the chain world's service over the real
// registry and registers one device per party given.
func opPushWire(t *testing.T, w *chainWorld, devices map[string]string) *opPushRecorder {
	t.Helper()
	reg := push.NewOperatorRegistry(w.db)
	if err := reg.EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("registry indexes: %v", err)
	}
	rec := &opPushRecorder{dead: map[string]bool{}}
	w.svc.opPush = push.NewOperatorWith(reg, rec)
	w.svc.opPushSync = true
	for party, token := range devices {
		pid, err := primitive.ObjectIDFromHex(party)
		if err != nil {
			t.Fatalf("party %q", party)
		}
		if err := reg.Register(context.Background(), pid, "", push.RegisterInput{Token: token, Platform: "android"}, time.Now()); err != nil {
			t.Fatalf("register %s: %v", token, err)
		}
	}
	return rec
}

// opPushParty seeds a party with one role assignment (status as given).
func opPushParty(t *testing.T, w *chainWorld, role, status string, store primitive.ObjectID) primitive.ObjectID {
	t.Helper()
	ctx := context.Background()
	id := primitive.NewObjectID()
	if _, err := w.db.Collection("parties").InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "phone", Value: "+9199" + id.Hex()[16:]}}); err != nil {
		t.Fatalf("party: %v", err)
	}
	if role != "" {
		if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
			{Key: "party_id", Value: id}, {Key: "role_code", Value: role},
			{Key: "status", Value: status}, {Key: "org_unit_id", Value: store},
		}); err != nil {
			t.Fatalf("role: %v", err)
		}
	}
	return id
}

func opPushTokens(msgs []push.FCMMessage) map[string]push.FCMMessage {
	out := map[string]push.FCMMessage{}
	for _, m := range msgs {
		out[m.Token] = m
	}
	return out
}

func opPushOrder(cid primitive.ObjectID, id, lane string, at time.Time) *order {
	return &order{
		OrderID: id, UserID: cid.Hex(), Status: "PLACED", Lane: lane, PlacedAt: at,
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		Subtotal:      70, Total: 70, PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123}, ConsumerName: "Push Tester", Phone: "9000007301",
	}
}

func TestOperatorPushNewInstantOrderRingsTheStoresManagersAndRiders(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	storeB, mgrB := ihSecondStore(t, w)
	revoked := opPushParty(t, w, "DELIVERY_RIDER", "REVOKED", w.storeID)
	riderB := opPushParty(t, w, "DELIVERY_RIDER", "ACTIVE", storeB)
	farmer := opPushParty(t, w, "", "", w.storeID)
	rec := opPushWire(t, w, map[string]string{
		w.mgr.PartyID: "tok-mgr", w.rider.PartyID: "tok-rider",
		mgrB.PartyID: "tok-mgr-b", riderB.Hex(): "tok-rider-b",
		revoked.Hex(): "tok-revoked", farmer.Hex(): "tok-farmer",
	})
	cid := w.customer(t, "9000007301", 0)

	// 23:10 IST, inside quiet hours: an order alert still rings.
	at := ihAt(23, 10)
	w.svc.createDeliveryForOrderAt(ctx, opPushOrder(cid, "ord_push_1", "instant", at), at)
	var task delivery
	if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: "ord_push_1"}}).Decode(&task); err != nil || task.Status != "OFFERED" {
		t.Fatalf("instant task: %v %+v", err, task.Status)
	}
	got := opPushTokens(rec.take())
	if len(got) != 2 {
		t.Fatalf("rang %d devices %v, want the store's manager and rider only", len(got), got)
	}
	for _, tok := range []string{"tok-mgr", "tok-rider"} {
		m, ok := got[tok]
		if !ok {
			t.Fatalf("%s did not ring: %v", tok, got)
		}
		if m.Title != "New instant order" || !m.Urgent || m.CollapseKey != "order-"+task.ID ||
			m.Body != "Order "+task.OrderCode+" - 2 items - Rs 70 - "+strconv.FormatFloat(task.DistanceKm, 'f', 1, 64)+" km from the store. The first rider to accept takes it." {
			t.Fatalf("%s message: %+v", tok, m)
		}
		if m.Data["type"] != templateStoreInstantOrder || m.Data["delivery_id"] != task.ID || m.Data["order_id"] != "ord_push_1" ||
			m.Data["store_id"] != w.storeID.Hex() || m.Data["lane"] != "instant" {
			t.Fatalf("%s data: %v", tok, m.Data)
		}
	}

	// A morning-lane task goes to the manager's batch assign, not a broadcast:
	// nothing rings.
	w.svc.createDeliveryForOrderAt(ctx, opPushOrder(cid, "ord_push_2", "morning", ihAt(10, 0)), ihAt(10, 0))
	if n := len(rec.take()); n != 0 {
		t.Fatalf("a morning task rang %d devices", n)
	}

	// The rider's phone was wiped: FCM answers UNREGISTERED, the token is
	// pruned and the manager still hears the next order.
	rec.dead["tok-rider"] = true
	w.svc.createDeliveryForOrderAt(ctx, opPushOrder(cid, "ord_push_3", "instant", ihAt(10, 5)), ihAt(10, 5))
	if got := opPushTokens(rec.take()); len(got) != 1 || got["tok-mgr"].Token == "" {
		t.Fatalf("after the rider's token died: %v", got)
	}
	if n, _ := w.db.Collection(push.CollOperatorPushDevices).CountDocuments(ctx, bson.D{{Key: "token", Value: "tok-rider"}}); n != 0 {
		t.Fatal("an UNREGISTERED token must be pruned from the registry")
	}

	// Inert without FCM: nothing is sent and nothing breaks.
	w.svc.opPush = nil
	w.svc.createDeliveryForOrderAt(ctx, opPushOrder(cid, "ord_push_4", "instant", ihAt(10, 10)), ihAt(10, 10))
	if n, _ := w.db.Collection(collDeliveries).CountDocuments(ctx, bson.D{{Key: "order_id", Value: "ord_push_4"}}); n != 1 {
		t.Fatal("the task must be created whether or not push is configured")
	}
}

func TestOperatorPushClosingRingsInQuietHoursClosedWaitsForTheDay(t *testing.T) {
	w, done, _, mgrB := ihAlertWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ctx := context.Background()
	rec := opPushWire(t, w, map[string]string{w.mgr.PartyID: "tok-mgr", w.rider.PartyID: "tok-rider", mgrB.PartyID: "tok-mgr-b"})

	// Store A keeps the shown 07:00-22:00: 21:45 rings the manager only.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	w.svc.instantAlertsTick(ctx, ihAt(21, 45))
	first := rec.take()
	if len(first) != 1 || first[0].Token != "tok-mgr" {
		t.Fatalf("21:45 closing alert rang %v, want the manager's phone only", first)
	}
	m := first[0]
	if m.Title != "Instant delivery closes at 10:00 PM" || m.Body != ihClosingMessage || !m.Urgent ||
		m.Data["type"] != templateStoreInstantClosing || m.Data["store_id"] != w.storeID.Hex() || m.CollapseKey != "instant-"+w.storeID.Hex() {
		t.Fatalf("closing push: %+v", m)
	}
	for _, min := range []int{46, 50, 59} {
		w.svc.instantAlertsTick(ctx, ihAt(21, min))
	}
	if n := len(rec.take()); n != 0 {
		t.Fatalf("the closing alert rang again (%d): once per close", n)
	}

	// The manager extends by an hour: the closing alert for the extended
	// close lands at 22:45, in quiet hours, and still rings (the lane is
	// running and the manager must act before it closes).
	ihClock(w, ihAt(21, 50))
	if code, v := ihOp(t, w, w.mgr, w.storeID.Hex(), "extend", 60); code != 200 {
		t.Fatalf("extend: %d %v", code, v)
	}
	for _, min := range []int{0, 30, 44} {
		w.svc.instantAlertsTick(ctx, ihAt(22, min))
	}
	if n := len(rec.take()); n != 0 {
		t.Fatalf("nothing is due while extended before 22:45, rang %d", n)
	}
	w.svc.instantAlertsTick(ctx, ihAt(22, 45))
	late := rec.take()
	if len(late) != 1 || late[0].Title != "Instant delivery closes at 11:00 PM" || !late[0].Urgent {
		t.Fatalf("22:45 closing alert for the extended close: %+v", late)
	}

	// 23:00: the lane closes. The inbox row lands; the ring waits out quiet
	// hours (informational).
	w.svc.instantAlertsTick(ctx, ihAt(23, 0))
	if n := ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosed); n != 1 {
		t.Fatalf("the closed notice must still reach the inbox, got %d", n)
	}
	if n := len(rec.take()); n != 0 {
		t.Fatalf("the closed notice rang %d phones at 23:00", n)
	}

	// Next day the store closes at 20:00: "now closed" rings at once.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 7 * 60, InstantCloseMin: 20 * 60})
	w.svc.instantAlertsTick(ctx, ihAt(24+19, 45))
	w.svc.instantAlertsTick(ctx, ihAt(24+20, 0))
	got := rec.take()
	if len(got) != 2 || got[0].Data["type"] != templateStoreInstantClosing || got[1].Data["type"] != templateStoreInstantClosed {
		t.Fatalf("day two: %+v", got)
	}
	if c := got[1]; c.Title != "Instant delivery is now closed" || c.Body != "It reopens at 7:00 AM." || c.Urgent ||
		c.CollapseKey != "instant-"+w.storeID.Hex() || c.Data["window_reopen"] == "" || c.Token != "tok-mgr" {
		t.Fatalf("closed push: %+v", c)
	}
}

func TestOperatorPushLowStockRingsTheAdminsOnlyWhenTheSetChanges(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	admin := opPushParty(t, w, "SUPER_ADMIN", "ACTIVE", primitive.NewObjectID())
	rec := opPushWire(t, w, map[string]string{admin.Hex(): "tok-admin", w.mgr.PartyID: "tok-mgr"})
	store := w.storeID.Hex()
	set := lowStockRequest{StoreName: "PYAAS Full-Chain Store", Summary: "Toned Milk 500ml (4)", ItemCount: 1}

	ihClock(w, ihAt(10, 0))
	if err := w.svc.raiseLowStockAlert(ctx, store, set); err != nil {
		t.Fatalf("raise: %v", err)
	}
	got := rec.take()
	if len(got) != 1 || got[0].Token != "tok-admin" || got[0].Title != "Low stock at PYAAS Full-Chain Store" ||
		got[0].Body != "Toned Milk 500ml (4)" || got[0].Urgent || got[0].Data["type"] != templateStoreLowStock ||
		got[0].Data["store_id"] != store || got[0].Data["item_count"] != "1" {
		t.Fatalf("first raise: %+v", got)
	}

	// Saathi posts the same set on every launch: silence.
	for i := 0; i < 3; i++ {
		if err := w.svc.raiseLowStockAlert(ctx, store, set); err != nil {
			t.Fatalf("repeat: %v", err)
		}
	}
	if n := len(rec.take()); n != 0 {
		t.Fatalf("the same set rang %d times", n)
	}

	changed := lowStockRequest{StoreName: set.StoreName, Summary: "Toned Milk 500ml (2), Chai Special (1)", ItemCount: 2}
	if err := w.svc.raiseLowStockAlert(ctx, store, changed); err != nil {
		t.Fatalf("changed: %v", err)
	}
	if got := rec.take(); len(got) != 1 || got[0].Body != changed.Summary {
		t.Fatalf("a changed set must ring: %+v", got)
	}

	// At 23:00 a change re-arms the inbox row but waits out quiet hours.
	ihClock(w, ihAt(23, 0))
	night := lowStockRequest{StoreName: set.StoreName, Summary: "Chai Special (1)", ItemCount: 1}
	if err := w.svc.raiseLowStockAlert(ctx, store, night); err != nil {
		t.Fatalf("night: %v", err)
	}
	if n := len(rec.take()); n != 0 {
		t.Fatalf("low stock rang %d phones at 23:00", n)
	}
	var row struct {
		Params map[string]string `bson:"params"`
		ReadAt *time.Time        `bson:"read_at"`
	}
	if err := w.db.Collection("notifications").FindOne(ctx, bson.D{{Key: "party_id", Value: admin}, {Key: "template_key", Value: templateStoreLowStock}}).Decode(&row); err != nil ||
		row.Params["summary"] != night.Summary || row.ReadAt != nil {
		t.Fatalf("the night change must still re-arm the inbox row: %v %+v", err, row)
	}

	// Stock recovered: the row clears and nothing rings.
	ihClock(w, ihAt(24+9, 0))
	if err := w.svc.raiseLowStockAlert(ctx, store, lowStockRequest{StoreName: set.StoreName}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n := len(rec.take()); n != 0 {
		t.Fatalf("a cleared alert rang %d phones", n)
	}
}

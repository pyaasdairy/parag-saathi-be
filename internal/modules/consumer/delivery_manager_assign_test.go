package consumer

// Founder decision 9 (25 Sep): a store manager may hand-assign an UNCLAIMED
// instant order to one of the store's own riders, with a record of who
// assigned it. Until now the store's assign guarded on ASSIGNED/FAILED only,
// so a broadcast (OFFERED) order nobody claimed could not be given to anyone
// from the store console: it waited for a rider to notice it, or for the
// admin website.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27021 \
//	  go test ./internal/modules/consumer/ -run 'ManagerAssign|ManagerHandAssign|AdminAssignStamps|RiderDeclineClears' -v

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// chainAddRider rosters another DELIVERY_RIDER at a store.
func chainAddRider(t *testing.T, w *chainWorld, storeID primitive.ObjectID, name string) auth.Actor {
	t.Helper()
	ctx := context.Background()
	id := primitive.NewObjectID()
	if _, err := w.db.Collection("parties").InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "full_name", Value: name}}); err != nil {
		t.Fatalf("seed rider party: %v", err)
	}
	if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: id}, {Key: "role_code", Value: "DELIVERY_RIDER"},
		{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: storeID},
	}); err != nil {
		t.Fatalf("seed rider role: %v", err)
	}
	return auth.Actor{PartyID: id.Hex(), Kind: "role", RoleCode: "DELIVERY_RIDER"}
}

// chainOtherStore makes a second store with its own manager.
func chainOtherStore(t *testing.T, w *chainWorld) (primitive.ObjectID, auth.Actor) {
	t.Helper()
	ctx := context.Background()
	storeID, mgrID := primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := w.db.Collection("org_units").InsertOne(ctx, bson.D{
		{Key: "_id", Value: storeID}, {Key: "type", Value: "STORE"}, {Key: "active", Value: true},
		{Key: "name", Value: "Other Store"}, {Key: "geo_lat", Value: 28.61}, {Key: "geo_lng", Value: 77.20},
	}); err != nil {
		t.Fatalf("seed other store: %v", err)
	}
	if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: mgrID}, {Key: "role_code", Value: "STORE_MANAGER"},
		{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: storeID},
	}); err != nil {
		t.Fatalf("seed other manager: %v", err)
	}
	return storeID, auth.Actor{PartyID: mgrID.Hex(), Kind: "role", RoleCode: "STORE_MANAGER"}
}

// chainInstantOffer places one instant order; its task broadcasts OFFERED.
func chainInstantOffer(t *testing.T, w *chainWorld, phone string) (*order, *delivery) {
	t.Helper()
	cid := w.customer(t, phone, 500)
	o, err := w.svc.createOrder(context.Background(), cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
		ConsumerName: "Instant Tester", Phone: phone,
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	task := chainTaskFor(t, w, o.OrderID)
	if task.Status != "OFFERED" || task.RiderPartyID != "" {
		t.Fatalf("instant task must broadcast OFFERED with no rider, got %s/%q", task.Status, task.RiderPartyID)
	}
	return o, task
}

func offerIDs(ds []delivery) map[string]bool {
	out := map[string]bool{}
	for _, d := range ds {
		out[d.ID] = true
	}
	return out
}

// waitAuditRows polls the audit log (the recorder writes in the background).
func waitAuditRows(t *testing.T, w *chainWorld, action, targetID string, want int) []audit.Entry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		cur, err := w.db.Collection("audit_logs").Find(context.Background(),
			bson.D{{Key: "action", Value: action}, {Key: "target_id", Value: targetID}})
		if err != nil {
			t.Fatalf("audit find: %v", err)
		}
		var rows []audit.Entry
		if err := cur.All(context.Background(), &rows); err != nil {
			t.Fatalf("audit decode: %v", err)
		}
		if len(rows) >= want || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func withAuditRecorder(w *chainWorld) {
	w.svc.deps.Audit = audit.NewRecorder(w.db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The hand-assign itself: the unclaimed offer goes to the chosen rider as
// ASSIGNED, leaves every other rider's pool, the member sees "assigned" as on
// a claim, the task says who assigned it and why, and an audit row records it.
// The rider then works it exactly like any assigned task, and it is charged once.
func TestManagerHandAssignsAnUnclaimedInstantOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	withAuditRecorder(w)
	ctx := context.Background()
	other := chainAddRider(t, w, w.storeID, "Suresh Second")

	ord, task := chainInstantOffer(t, w, "9000006101")
	// Both riders see the broadcast before the manager acts.
	for _, rd := range []auth.Actor{w.rider, other} {
		pool, err := w.svc.offeredForRider(ctx, rd, nil)
		if err != nil || !offerIDs(pool)[task.ID] {
			t.Fatalf("before the assign %s must see the offer: %v %v", rd.PartyID, pool, err)
		}
	}

	at := time.Date(2026, 10, 6, 9, 15, 0, 0, time.UTC)
	got, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex(), "  Ravi is at the gate \n already ", at)
	if err != nil {
		t.Fatalf("hand-assign of an unclaimed instant order: %v", err)
	}
	if got.Status != "ASSIGNED" || got.RiderPartyID != w.riderID.Hex() {
		t.Fatalf("task after the hand-assign: %s rider %q, want ASSIGNED to Ravi", got.Status, got.RiderPartyID)
	}
	if got.AssignSource != assignSourceManager || got.AssignReason != "Ravi is at the gate already" || got.AssignedAt != "2026-10-06T09:15:00Z" {
		t.Fatalf("assignment record: source %q reason %q at %q", got.AssignSource, got.AssignReason, got.AssignedAt)
	}
	if got.AssignedBy == nil || got.AssignedBy.PartyID != w.mgr.PartyID || got.AssignedBy.Role != "STORE_MANAGER" || got.AssignedBy.Name != "Store Manager" {
		t.Fatalf("assigned_by: %+v", got.AssignedBy)
	}
	// Stored, not just returned.
	if stored := chainTaskFor(t, w, ord.OrderID); stored.AssignedBy == nil || stored.AssignSource != assignSourceManager {
		t.Fatalf("assignment not stored: %+v", stored)
	}

	// Gone from every pool, including the chosen rider's own.
	for _, rd := range []auth.Actor{w.rider, other} {
		pool, err := w.svc.offeredForRider(ctx, rd, nil)
		if err != nil || offerIDs(pool)[task.ID] {
			t.Fatalf("after the assign the offer must leave %s's pool: %v %v", rd.PartyID, pool, err)
		}
	}
	// In the chosen rider's queue at once.
	mine, err := w.svc.riderDeliveries(ctx, w.rider)
	if err != nil || !offerIDs(mine)[task.ID] {
		t.Fatalf("the assigned rider's queue lacks the task: %v %v", mine, err)
	}
	// The member's order moved as it does on a claim.
	if o := w.orderByID(t, ord.OrderID); o.Status != "assigned" || o.RiderID == nil || *o.RiderID != w.riderID.Hex() {
		t.Fatalf("order after the hand-assign: %q rider %v, want assigned to Ravi", o.Status, o.RiderID)
	}
	// Nobody else can claim it now.
	if _, err := w.svc.claimOfferedDelivery(ctx, other, task.ID); geofenceCode(err) != "CLAIMED_BY_OTHER" {
		t.Fatalf("a claim of a hand-assigned task: %v, want CLAIMED_BY_OTHER", err)
	}

	rows := waitAuditRows(t, w, auditActionManagerAssign, task.ID, 1)
	if len(rows) != 1 {
		t.Fatalf("audit rows for the hand-assign: %d want 1", len(rows))
	}
	r := rows[0]
	if r.ActorPartyID != w.mgr.PartyID || r.ActorRole != "STORE_MANAGER" || r.TargetType != auditTargetDelivery ||
		r.Meta["previous_status"] != "OFFERED" || r.Meta["rider_party_id"] != w.riderID.Hex() ||
		r.Meta["store_id"] != w.storeID.Hex() || r.Meta["lane"] != "instant" ||
		r.Meta["assign_source"] != assignSourceManager || r.Meta["reason"] != "Ravi is at the gate already" ||
		r.Meta["assigned_by_name"] != "Store Manager" {
		t.Fatalf("audit row: %+v", r)
	}

	// The rider works it like any assigned task; money moves exactly once.
	if _, err := w.svc.acceptDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{
		ProofPhoto: "https://example.test/p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n := deliveryDebitRows(t, w, ord.OrderID); n != 1 {
		t.Fatalf("debit rows after one delivery: %d want 1", n)
	}
	// Accept, pickup and deliver keep the record of who assigned it.
	if d := chainTaskFor(t, w, ord.OrderID); d.AssignSource != assignSourceManager || d.AssignedBy == nil {
		t.Fatalf("the assignment record did not survive the run: %+v", d)
	}
}

// First-accept-wins still holds: once a rider has claimed the order, the
// manager's assign is refused with the claim's own code and changes nothing.
func TestManagerAssignOfAClaimedInstantOrderIsRefused(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	withAuditRecorder(w)
	ctx := context.Background()
	other := chainAddRider(t, w, w.storeID, "Suresh Second")

	_, task := chainInstantOffer(t, w, "9000006102")
	if _, err := w.svc.claimOfferedDelivery(ctx, other, task.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex(), "", time.Now())
	if geofenceCode(err) != "CLAIMED_BY_OTHER" {
		t.Fatalf("assign of a claimed order: %v, want CLAIMED_BY_OTHER", err)
	}
	d := chainTaskFor(t, w, task.OrderID)
	if d.Status != "ACCEPTED" || d.RiderPartyID != other.PartyID || d.AssignSource != assignSourceClaim || d.AssignedBy != nil {
		t.Fatalf("a refused assign changed the claimed task: %+v", d)
	}
	if rows := waitAuditRows(t, w, auditActionManagerAssign, task.ID, 1); len(rows) != 0 {
		t.Fatalf("a refused assign was audited: %+v", rows)
	}
}

// The store scoping is unchanged: another store's manager cannot touch the
// order, and only the store's own riders can be picked.
func TestManagerAssignKeepsStoreScoping(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	otherStore, otherMgr := chainOtherStore(t, w)
	stranger := chainAddRider(t, w, otherStore, "Other Store Rider")

	_, task := chainInstantOffer(t, w, "9000006103")
	if _, err := w.svc.assignRiderAt(ctx, otherMgr, w.storeID.Hex(), task.ID, w.riderID.Hex(), "", time.Now()); geofenceCode(err) != "FORBIDDEN" {
		t.Fatalf("another store's manager: %v, want FORBIDDEN", err)
	}
	if _, err := w.svc.assignRiderAt(ctx, otherMgr, otherStore.Hex(), task.ID, stranger.PartyID, "", time.Now()); geofenceCode(err) != "FORBIDDEN" {
		t.Fatalf("assign through another store's path: %v, want FORBIDDEN", err)
	}
	if _, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, stranger.PartyID, "", time.Now()); geofenceCode(err) != "BAD_REQUEST" {
		t.Fatalf("a rider from another store: %v, want BAD_REQUEST", err)
	}
	if d := chainTaskFor(t, w, task.OrderID); d.Status != "OFFERED" || d.RiderPartyID != "" || d.AssignedBy != nil {
		t.Fatalf("a refused assign changed the offer: %+v", d)
	}
}

// A rider who declines a hand-assigned order sends it back to the pool with no
// assignment record, and the next rider's claim is credited to the claim.
func TestRiderDeclineClearsTheManagerAssignment(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	other := chainAddRider(t, w, w.storeID, "Suresh Second")

	ord, task := chainInstantOffer(t, w, "9000006104")
	if _, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex(), "nearest", time.Now()); err != nil {
		t.Fatalf("assign: %v", err)
	}
	back, err := w.svc.rejectOfferedDelivery(ctx, w.rider, task.ID)
	if err != nil {
		t.Fatalf("decline: %v", err)
	}
	if back.Status != "OFFERED" || back.RiderPartyID != "" || back.AssignedBy != nil || back.AssignSource != "" || back.AssignReason != "" {
		t.Fatalf("a declined task kept the manager's assignment: %+v", back)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "placed" {
		t.Fatalf("order after the decline: %q want placed", o.Status)
	}
	claimed, err := w.svc.claimOfferedDelivery(ctx, other, task.ID)
	if err != nil {
		t.Fatalf("claim after the decline: %v", err)
	}
	if claimed.AssignSource != assignSourceClaim || claimed.AssignedBy != nil {
		t.Fatalf("the claim was credited to the manager: %+v", claimed)
	}
}

// The morning assign (unchanged behaviour) now also records who assigned it,
// and a reassign replaces the previous reason rather than inheriting it.
func TestManagerAssignStampsMorningTasksToo(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	other := chainAddRider(t, w, w.storeID, "Suresh Second")

	ord := chainMorningOrder(t, w, "9000006105", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	if _, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex(), "first round", time.Now()); err != nil {
		t.Fatalf("assign: %v", err)
	}
	d, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, other.PartyID, "", time.Now())
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if d.RiderPartyID != other.PartyID || d.AssignSource != assignSourceManager || d.AssignReason != "" ||
		d.AssignedBy == nil || d.AssignedBy.PartyID != w.mgr.PartyID {
		t.Fatalf("morning reassign record: %+v", d)
	}
}

// The admin website's assign stamps the admin, so a task a manager assigned
// and the admin then moved never credits the manager.
func TestAdminAssignStampsTheAdmin(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	other := chainAddRider(t, w, w.storeID, "Suresh Second")

	_, task := chainInstantOffer(t, w, "9000006106")
	if _, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex(), "nearest", time.Now()); err != nil {
		t.Fatalf("manager assign: %v", err)
	}
	h := &handler{svc: w.svc}
	r := chi.NewRouter()
	r.Post("/admin/crm/deliveries/{deliveryId}/assign", h.crmAssign)
	req := httptest.NewRequest(http.MethodPost, "/admin/crm/deliveries/"+task.ID+"/assign",
		strings.NewReader(`{"rider_party_id":"`+other.PartyID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithActor(req.Context(), auth.Actor{PartyID: adminKeyActorID, Kind: "service", RoleCode: "SUPER_ADMIN"}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin assign: %d %s", rec.Code, rec.Body.String())
	}
	d := chainTaskFor(t, w, task.OrderID)
	if d.RiderPartyID != other.PartyID || d.AssignSource != assignSourceAdmin || d.AssignReason != "" ||
		d.AssignedBy == nil || d.AssignedBy.PartyID != adminKeyActorID || d.AssignedBy.Role != "SUPER_ADMIN" {
		t.Fatalf("admin assign record: %+v %+v", d, d.AssignedBy)
	}
}

// The wire: the new fields are additive camelCase keys on the task JSON.
func TestAssignmentFieldsOnTheTaskWire(t *testing.T) {
	d := delivery{ID: "del_x", Status: "ASSIGNED", AssignSource: assignSourceManager, AssignReason: "r",
		AssignedBy: &assignedByDoc{PartyID: "p1", Role: "STORE_MANAGER", Name: "Asha"}}
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, d)
	body := rec.Body.String()
	for _, want := range []string{`"assignSource":"manager_assign"`, `"assignReason":"r"`, `"assignedBy":{"partyId":"p1","role":"STORE_MANAGER","name":"Asha"}`, `"status":"ASSIGNED"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("task JSON lacks %s: %s", want, body)
		}
	}
	// Untouched tasks carry none of the three keys.
	rec = httptest.NewRecorder()
	writeJSON(rec, 200, delivery{ID: "del_y", Status: "OFFERED"})
	for _, absent := range []string{"assignSource", "assignReason", "assignedBy"} {
		if strings.Contains(rec.Body.String(), absent) {
			t.Fatalf("an unassigned task carries %s: %s", absent, rec.Body.String())
		}
	}
}

// The reason is one bounded line.
func TestCleanAssignReason(t *testing.T) {
	if got := cleanAssignReason("  a \n\t b  "); got != "a b" {
		t.Fatalf("clean: %q", got)
	}
	long := strings.Repeat("x", assignReasonMaxLen+50)
	if got := cleanAssignReason(long); len([]rune(got)) != assignReasonMaxLen {
		t.Fatalf("clip: %d", len([]rune(got)))
	}
}

// A hand-assigned instant order the rider has not accepted can be moved to
// another rider ("Ravi is not picking up, give it to Suresh"). The member's
// order must then name the new rider, as it does after a claim, not the one
// who never came.
func TestManagerReassignOfAnInstantOrderNamesTheNewRider(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	at := time.Date(2026, 10, 6, 9, 15, 0, 0, time.UTC)
	w.svc.clock = func() time.Time { return at }
	suresh := chainAddRider(t, w, w.storeID, "Suresh Second")

	ord, task := chainInstantOffer(t, w, "9000006107")
	if _, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex(), "", at); err != nil {
		t.Fatalf("hand-assign to Ravi: %v", err)
	}
	got, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, suresh.PartyID, "Ravi not answering", at.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("reassign to Suresh: %v", err)
	}
	if got.Status != "ASSIGNED" || got.RiderPartyID != suresh.PartyID || got.AssignReason != "Ravi not answering" {
		t.Fatalf("task after the reassign: %+v", got)
	}
	o := w.orderByID(t, ord.OrderID)
	if o.Status != "assigned" || o.RiderID == nil || *o.RiderID != suresh.PartyID {
		rid := ""
		if o.RiderID != nil {
			rid = *o.RiderID
		}
		t.Fatalf("order after the reassign: %q rider %q, want assigned to Suresh %q", o.Status, rid, suresh.PartyID)
	}
	if o.Rider == nil || o.Rider.FullName != "Suresh Second" {
		t.Fatalf("order rider card after the reassign: %+v, want Suresh Second", o.Rider)
	}
	// Ravi no longer has it; Suresh does and can accept it.
	if mine, _ := w.svc.riderDeliveries(ctx, w.rider); offerIDs(mine)[task.ID] {
		t.Fatal("the first rider still holds the reassigned order")
	}
	if _, err := w.svc.acceptDelivery(ctx, suresh, task.ID); err != nil {
		t.Fatalf("accept by Suresh: %v", err)
	}
}

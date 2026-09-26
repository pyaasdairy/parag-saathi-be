package consumer

// Founder decision 10 (25 Sep): attendance gates the rider queue, with a
// manager override, and a missed check-in never blocks a delivery.
//
//   - the unclaimed instant OFFER POOL shows only to riders on duty today, and
//     only they may claim from it;
//   - tasks already assigned to a rider always show and can always be worked;
//   - the manager may assign any of the store's riders, on duty or not, and
//     may mark a rider on (or off) duty for the day;
//   - if NO rider of the store is on duty, the pool falls back to every store
//     rider, and that is logged.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27021 \
//	  go test ./internal/modules/consumer/ -run 'OfferPool|DutyGate|ManagerMarks|ThrottledLog' -v

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

const dutyDay = "2026-10-06"

func dutyAt(day string, h, m int) time.Time { return istDayAt(day, h, m) }

// chainDutyWorld is the chain world with the rider-console indexes built (the
// production boot builds them) and a second rider on the store's roster.
func chainDutyWorld(t *testing.T) (*chainWorld, auth.Actor, func()) {
	t.Helper()
	w, done := newChainWorld(t)
	if err := w.svc.repo.ensureRiderOpsIndexes(context.Background()); err != nil {
		done()
		t.Fatalf("rider-ops indexes: %v", err)
	}
	return w, chainAddRider(t, w, w.storeID, "Suresh Second"), done
}

// chainCheckIn writes the rider's own attendance for an IST day, the row the
// check-in handler writes (selfie evidence aside).
func chainCheckIn(t *testing.T, w *chainWorld, riderID, day string) {
	t.Helper()
	at := dutyAt(day, 5, 0).UTC()
	if _, dup, err := w.svc.repo.riderInsertAttendance(context.Background(), riderAttendanceDoc{
		RiderPartyID: riderID, Day: day, State: riderDutyOnDuty, CheckInAt: &at,
		PhotoRef: "https://example.test/selfie.jpg", CreatedAt: at, UpdatedAt: at,
	}); err != nil || dup {
		t.Fatalf("check-in %s on %s: dup=%v err=%v", riderID, day, dup, err)
	}
}

func chainCheckOut(t *testing.T, w *chainWorld, riderID, day string) {
	t.Helper()
	if closed, err := w.svc.repo.riderCloseAttendance(context.Background(), riderID, day, nil); err != nil || !closed {
		t.Fatalf("check-out %s on %s: closed=%v err=%v", riderID, day, closed, err)
	}
}

func poolHas(t *testing.T, w *chainWorld, rd auth.Actor, taskID string, now time.Time) bool {
	t.Helper()
	pool, err := w.svc.offeredForRiderAt(context.Background(), rd, nil, now)
	if err != nil {
		t.Fatalf("offer pool for %s: %v", rd.PartyID, err)
	}
	return offerIDs(pool)[taskID]
}

// logText reads a syncBuffer (crm_channel_errors_test.go) as plain text.
func logText(b *syncBuffer) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func captureLog(w *chainWorld) *syncBuffer {
	buf := &syncBuffer{}
	w.svc.log = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

// A rider on duty sees the offer and can claim it; a rider who has not
// checked in, at a store where someone has, neither sees nor claims it.
func TestOfferPoolShowsOnlyToOnDutyRiders(t *testing.T) {
	w, suresh, done := chainDutyWorld(t)
	defer done()
	ctx := context.Background()
	now := dutyAt(dutyDay, 10, 0)
	chainCheckIn(t, w, w.riderID.Hex(), dutyDay)

	_, task := chainInstantOffer(t, w, "9000006201")
	if !poolHas(t, w, w.rider, task.ID, now) {
		t.Fatal("the on-duty rider must see the offer")
	}
	if poolHas(t, w, suresh, task.ID, now) {
		t.Fatal("a rider who has not checked in saw the offer while another rider is on duty")
	}
	_, err := w.svc.claimOfferedDeliveryAt(ctx, suresh, task.ID, now)
	if code := geofenceCode(err); code != "NOT_ON_DUTY" {
		t.Fatalf("claim by an off-duty rider: %v, want NOT_ON_DUTY", err)
	}
	var ae *apiError
	if e, ok := err.(*apiError); ok {
		ae = e
	}
	if ae == nil || ae.status != http.StatusForbidden {
		t.Fatalf("NOT_ON_DUTY must be a 403 (the rider app reads 409 as taken): %+v", ae)
	}
	if d := chainTaskFor(t, w, task.OrderID); d.Status != "OFFERED" || d.RiderPartyID != "" {
		t.Fatalf("a refused claim changed the offer: %+v", d)
	}
	if d, err := w.svc.claimOfferedDeliveryAt(ctx, w.rider, task.ID, now); err != nil || d.RiderPartyID != w.riderID.Hex() {
		t.Fatalf("claim by the on-duty rider: %v %v", d, err)
	}
}

// Nobody on duty: the pool falls back to every store rider (a missed
// check-in never strands an order), and the fallback is logged, once per
// store per window however often the riders poll. Yesterday's check-in and a
// check-out both mean off duty today.
func TestOfferPoolFallsBackWhenNobodyIsOnDuty(t *testing.T) {
	w, suresh, done := chainDutyWorld(t)
	defer done()
	ctx := context.Background()
	logs := captureLog(w)
	now := dutyAt(dutyDay, 10, 0)
	yesterday := addDaysIST(dutyDay, -1)
	chainCheckIn(t, w, w.riderID.Hex(), yesterday) // forgot to check out yesterday
	chainCheckIn(t, w, suresh.PartyID, dutyDay)
	chainCheckOut(t, w, suresh.PartyID, dutyDay) // on and off again today

	_, task := chainInstantOffer(t, w, "9000006202")
	for i := 0; i < 3; i++ {
		for _, rd := range []auth.Actor{w.rider, suresh} {
			if !poolHas(t, w, rd, task.ID, now.Add(time.Duration(i)*time.Minute)) {
				t.Fatalf("with nobody on duty %s must still see the offer (poll %d)", rd.PartyID, i)
			}
		}
	}
	if n := strings.Count(logText(logs), "falls back to every store rider"); n != 1 {
		t.Fatalf("fallback log lines in one window: %d want 1\n%s", n, logText(logs))
	}
	if !strings.Contains(logText(logs), "store="+w.storeID.Hex()) {
		t.Fatalf("the fallback line must name the store: %s", logText(logs))
	}
	// Past the window it is logged again.
	if !poolHas(t, w, suresh, task.ID, now.Add(fallbackLogEvery+time.Minute)) {
		t.Fatal("fallback pool after the window")
	}
	if n := strings.Count(logText(logs), "falls back to every store rider"); n != 2 {
		t.Fatalf("fallback log lines after the window: %d want 2", n)
	}
	if _, err := w.svc.claimOfferedDeliveryAt(ctx, suresh, task.ID, now); err != nil {
		t.Fatalf("a fallback claim must succeed: %v", err)
	}
}

// Assigned work is never gated: the manager may give an instant order to a
// rider who has not checked in (the override, recorded in the audit row), and
// that rider sees and completes it; a morning task assigned to them shows too.
func TestDutyGateNeverBlocksAssignedWork(t *testing.T) {
	w, suresh, done := chainDutyWorld(t)
	defer done()
	withAuditRecorder(w)
	ctx := context.Background()
	now := dutyAt(dutyDay, 10, 0)
	chainCheckIn(t, w, w.riderID.Hex(), dutyDay) // Ravi on duty, Suresh not

	ord, task := chainInstantOffer(t, w, "9000006203")
	if _, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), task.ID, suresh.PartyID, "knows the society", now); err != nil {
		t.Fatalf("the manager's assign to an off-duty rider: %v", err)
	}
	morning := chainMorningOrder(t, w, "9000006204", nil)
	mtask := chainTaskFor(t, w, morning.OrderID)
	if _, err := w.svc.assignRiderAt(ctx, w.mgr, w.storeID.Hex(), mtask.ID, suresh.PartyID, "", now); err != nil {
		t.Fatalf("morning assign to an off-duty rider: %v", err)
	}
	mine, err := w.svc.riderDeliveries(ctx, suresh)
	if err != nil || !offerIDs(mine)[task.ID] || !offerIDs(mine)[mtask.ID] {
		t.Fatalf("an off-duty rider's queue must carry their assigned tasks: %v %v", mine, err)
	}
	if _, err := w.svc.acceptDelivery(ctx, suresh, task.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, suresh, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if _, err := w.svc.deliverDelivery(ctx, suresh, task.ID, deliverInput{
		ProofPhoto: "https://example.test/p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliver by an off-duty rider: %v", err)
	}
	if n := deliveryDebitRows(t, w, ord.OrderID); n != 1 {
		t.Fatalf("debit rows: %d want 1", n)
	}
	rows := waitAuditRows(t, w, auditActionManagerAssign, task.ID, 1)
	if len(rows) != 1 || rows[0].Meta["rider_on_duty"] != false || rows[0].Meta["rider_duty_source"] != dutySourceNone {
		t.Fatalf("the audit row must show the assign overrode duty: %+v", rows)
	}
}

// The manager's duty mark: on puts a rider in the pool without a check-in,
// off takes one out, the roster shows both with their source, and the mark
// lasts for the IST day only. Scoped to the manager's own store and riders,
// and audited.
func TestManagerMarksARiderOnAndOffDuty(t *testing.T) {
	w, suresh, done := chainDutyWorld(t)
	defer done()
	withAuditRecorder(w)
	ctx := context.Background()
	now := dutyAt(dutyDay, 10, 0)
	chainCheckIn(t, w, w.riderID.Hex(), dutyDay)
	_, task := chainInstantOffer(t, w, "9000006205")
	if poolHas(t, w, suresh, task.ID, now) {
		t.Fatal("before the mark Suresh is off duty")
	}

	v, err := w.svc.setRiderDutyAt(ctx, w.mgr, w.storeID.Hex(), suresh.PartyID, true, " phone camera broken ", now)
	if err != nil {
		t.Fatalf("mark on duty: %v", err)
	}
	if !v.OnDuty || v.DutySource != dutySourceManager || v.Day != dutyDay || v.PartyID != suresh.PartyID {
		t.Fatalf("duty view: %+v", v)
	}
	if !poolHas(t, w, suresh, task.ID, now) {
		t.Fatal("a rider the manager marked on duty must see the pool")
	}
	roster := func(at time.Time) map[string]riderSummary {
		rs, err := w.svc.storeRidersAt(ctx, w.mgr, w.storeID.Hex(), "", at)
		if err != nil {
			t.Fatalf("roster: %v", err)
		}
		out := map[string]riderSummary{}
		for _, r := range rs {
			out[r.PartyID] = r
		}
		return out
	}
	rs := roster(now)
	if r := rs[suresh.PartyID]; !r.OnDuty || r.DutySource != dutySourceManager {
		t.Fatalf("roster Suresh: %+v", r)
	}
	if r := rs[w.riderID.Hex()]; !r.OnDuty || r.DutySource != dutySourceAttendance {
		t.Fatalf("roster Ravi: %+v", r)
	}

	// Off beats Ravi's own check-in (he went home without checking out).
	if _, err := w.svc.setRiderDutyAt(ctx, w.mgr, w.storeID.Hex(), w.riderID.Hex(), false, "went home", now); err != nil {
		t.Fatalf("mark off duty: %v", err)
	}
	if poolHas(t, w, w.rider, task.ID, now) {
		t.Fatal("a rider the manager marked off duty still sees the pool while Suresh is on duty")
	}
	if r := roster(now)[w.riderID.Hex()]; r.OnDuty || r.DutySource != dutySourceManager {
		t.Fatalf("roster Ravi after the off mark: %+v", r)
	}
	// Setting it again is idempotent (one row per rider per day).
	if _, err := w.svc.setRiderDutyAt(ctx, w.mgr, w.storeID.Hex(), w.riderID.Hex(), false, "went home", now); err != nil {
		t.Fatalf("mark off again: %v", err)
	}
	if n, _ := w.db.Collection(collRiderDutyOverrides).CountDocuments(ctx, bson.D{{Key: "rider_party_id", Value: w.riderID.Hex()}}); n != 1 {
		t.Fatalf("override rows for one rider-day: %d want 1", n)
	}

	// Tomorrow the marks and today's attendance have lapsed: nobody is on
	// duty, so both riders see the pool again (fallback).
	tomorrow := dutyAt(addDaysIST(dutyDay, 1), 10, 0)
	rs = roster(tomorrow)
	for _, id := range []string{suresh.PartyID, w.riderID.Hex()} {
		if r := rs[id]; r.OnDuty || r.DutySource != dutySourceNone {
			t.Fatalf("roster tomorrow for %s: %+v", id, r)
		}
	}
	if !poolHas(t, w, w.rider, task.ID, tomorrow) || !poolHas(t, w, suresh, task.ID, tomorrow) {
		t.Fatal("tomorrow with nobody on duty the pool must fall back to both riders")
	}

	// Scoping, as for the assign.
	otherStore, otherMgr := chainOtherStore(t, w)
	stranger := chainAddRider(t, w, otherStore, "Other Store Rider")
	if _, err := w.svc.setRiderDutyAt(ctx, otherMgr, w.storeID.Hex(), suresh.PartyID, true, "", now); geofenceCode(err) != "FORBIDDEN" {
		t.Fatalf("another store's manager: %v, want FORBIDDEN", err)
	}
	if _, err := w.svc.setRiderDutyAt(ctx, w.mgr, w.storeID.Hex(), stranger.PartyID, true, "", now); geofenceCode(err) != "BAD_REQUEST" {
		t.Fatalf("a rider from another store: %v, want BAD_REQUEST", err)
	}

	rows := waitAuditRows(t, w, auditActionDutyOverride, suresh.PartyID, 1)
	if len(rows) != 1 || rows[0].ActorPartyID != w.mgr.PartyID || rows[0].Meta["on_duty"] != true ||
		rows[0].Meta["previous_on_duty"] != false || rows[0].Meta["reason"] != "phone camera broken" ||
		rows[0].Meta["store_id"] != w.storeID.Hex() || rows[0].Meta["day"] != dutyDay {
		t.Fatalf("duty audit row for Suresh: %+v", rows)
	}
	rows = waitAuditRows(t, w, auditActionDutyOverride, w.riderID.Hex(), 2)
	sources := map[any]bool{}
	for _, r := range rows {
		if r.Meta["on_duty"] != false {
			t.Fatalf("duty audit row for Ravi: %+v", r)
		}
		sources[r.Meta["previous_duty_source"]] = true
	}
	if len(rows) != 2 || !sources[dutySourceAttendance] || !sources[dutySourceManager] {
		t.Fatalf("duty audit rows for Ravi (first overrode his check-in, second repeated the mark): %+v", rows)
	}
}

// The endpoint: POST /stores/{storeId}/riders/{riderPartyId}/duty, operator
// envelope, on_duty required.
func TestDutyGateEndpointWire(t *testing.T) {
	w, suresh, done := chainDutyWorld(t)
	defer done()
	fixed := dutyAt(dutyDay, 9, 30)
	w.svc.clock = func() time.Time { return fixed }
	h := &handler{svc: w.svc}
	r := chi.NewRouter()
	r.Post("/stores/{storeId}/riders/{riderPartyId}/duty", h.setRiderDuty)
	post := func(body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/stores/"+w.storeID.Hex()+"/riders/"+suresh.PartyID+"/duty", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(auth.WithActor(req.Context(), w.mgr))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	code, body := post(`{"on_duty":true,"reason":"no selfie camera"}`)
	if code != http.StatusOK {
		t.Fatalf("duty on: %d %s", code, body)
	}
	var env struct {
		Data riderDutyView `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if env.Data != (riderDutyView{PartyID: suresh.PartyID, Day: dutyDay, OnDuty: true, DutySource: dutySourceManager}) {
		t.Fatalf("duty wire: %s", body)
	}
	if code, body = post(`{"reason":"missing flag"}`); code != http.StatusBadRequest {
		t.Fatalf("missing on_duty: %d %s", code, body)
	}
	// The roster wire carries the additive keys.
	rs, err := w.svc.storeRidersAt(context.Background(), w.mgr, w.storeID.Hex(), "", fixed)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	raw, _ := json.Marshal(rs)
	if !strings.Contains(string(raw), `"onDuty":true,"dutySource":"manager"`) || !strings.Contains(string(raw), `"onDuty":false,"dutySource":"none"`) {
		t.Fatalf("roster wire: %s", raw)
	}
}

func TestThrottledLog(t *testing.T) {
	var tl throttledLog
	t0 := time.Date(2026, 10, 6, 4, 30, 0, 0, time.UTC)
	if !tl.allow("s1", t0, time.Minute) || tl.allow("s1", t0.Add(30*time.Second), time.Minute) {
		t.Fatal("inside the window a key logs once")
	}
	if !tl.allow("s2", t0, time.Minute) {
		t.Fatal("keys are independent")
	}
	if !tl.allow("s1", t0.Add(61*time.Second), time.Minute) {
		t.Fatal("after the window the key logs again")
	}
	// A clock that steps backwards never silences a key for good.
	if !tl.allow("s1", t0.Add(-time.Hour), time.Minute) {
		t.Fatal("a backwards clock must not suppress the line")
	}
}

// chainShareRider rosters an existing rider at a second store too.
func chainShareRider(t *testing.T, w *chainWorld, riderID, storeID primitive.ObjectID) {
	t.Helper()
	if _, err := w.db.Collection("role_assignments").InsertOne(context.Background(), bson.D{
		{Key: "party_id", Value: riderID}, {Key: "role_code", Value: "DELIVERY_RIDER"},
		{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: storeID},
	}); err != nil {
		t.Fatalf("share rider with store %s: %v", storeID.Hex(), err)
	}
}

// chainOfferAt moves an instant offer to another store, with a drop near that
// store, so it sits in that store's pool.
func chainOfferAt(t *testing.T, w *chainWorld, taskID string, storeID primitive.ObjectID) {
	t.Helper()
	if _, err := w.db.Collection(collDeliveries).UpdateOne(context.Background(),
		bson.D{{Key: "delivery_id", Value: taskID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "store_id", Value: storeID.Hex()}, {Key: "geo", Value: geoPt{Lat: 28.611, Lng: 77.201}}}}}); err != nil {
		t.Fatalf("move offer to store %s: %v", storeID.Hex(), err)
	}
}

// A manager's duty mark counts at that manager's own store only. A rider who
// rides for two stores is on or off duty at the other store by their own
// check-in (or that store's manager's mark), so the manager of store A can
// neither pull a shared rider out of store B's pool nor push one into it.
func TestManagerDutyMarkCountsOnlyAtTheirOwnStore(t *testing.T) {
	w, _, done := chainDutyWorld(t)
	defer done()
	ctx := context.Background()
	now := dutyAt(dutyDay, 10, 0)
	storeB, mgrB := chainOtherStore(t, w)
	bRider := chainAddRider(t, w, storeB, "Store B Rider")
	chainShareRider(t, w, w.riderID, storeB) // Ravi rides for both stores
	chainCheckIn(t, w, bRider.PartyID, dutyDay)

	_, atA := chainInstantOffer(t, w, "9000006211")
	_, atB := chainInstantOffer(t, w, "9000006212")
	chainOfferAt(t, w, atB.ID, storeB)

	// Ravi has not checked in; store B has an on-duty rider, store A has none.
	if poolHas(t, w, w.rider, atB.ID, now) {
		t.Fatal("setup: Ravi is off duty and store B has a rider on duty")
	}
	// Store A's manager marks Ravi on duty: at store A, not at store B.
	if _, err := w.svc.setRiderDutyAt(ctx, w.mgr, w.storeID.Hex(), w.riderID.Hex(), true, "", now); err != nil {
		t.Fatalf("mark on at A: %v", err)
	}
	if !poolHas(t, w, w.rider, atA.ID, now) {
		t.Fatal("marked on duty at A, Ravi must see A's offer")
	}
	if poolHas(t, w, w.rider, atB.ID, now) {
		t.Fatal("store A's manager put Ravi into store B's pool")
	}
	if _, err := w.svc.claimOfferedDeliveryAt(ctx, w.rider, atB.ID, now); geofenceCode(err) != "NOT_ON_DUTY" {
		t.Fatalf("Ravi's claim at store B: %v, want NOT_ON_DUTY", err)
	}
	if r := rosterOf(t, w, mgrB, storeB, now)[w.riderID.Hex()]; r.OnDuty || r.DutySource != dutySourceNone {
		t.Fatalf("store B's roster shows store A's mark: %+v", r)
	}

	// Ravi checks in; store A's manager marks him off: off at A, on at B.
	chainCheckIn(t, w, w.riderID.Hex(), dutyDay)
	if _, err := w.svc.setRiderDutyAt(ctx, w.mgr, w.storeID.Hex(), w.riderID.Hex(), false, "", now); err != nil {
		t.Fatalf("mark off at A: %v", err)
	}
	if !poolHas(t, w, w.rider, atB.ID, now) {
		t.Fatal("store A's manager pulled a checked-in Ravi out of store B's pool")
	}
	if r := rosterOf(t, w, mgrB, storeB, now)[w.riderID.Hex()]; !r.OnDuty || r.DutySource != dutySourceAttendance {
		t.Fatalf("store B's roster for Ravi: %+v", r)
	}
	if r := rosterOf(t, w, w.mgr, w.storeID, now)[w.riderID.Hex()]; r.OnDuty || r.DutySource != dutySourceManager {
		t.Fatalf("store A's roster for Ravi: %+v", r)
	}
	// Both stores' managers may mark the same rider the same day, each for
	// their own store.
	if _, err := w.svc.setRiderDutyAt(ctx, mgrB, storeB.Hex(), w.riderID.Hex(), true, "", now); err != nil {
		t.Fatalf("store B's own mark on the shared rider: %v", err)
	}
	if n, _ := w.db.Collection(collRiderDutyOverrides).CountDocuments(ctx, bson.D{{Key: "rider_party_id", Value: w.riderID.Hex()}}); n != 2 {
		t.Fatalf("override rows for one shared rider-day: %d want 2 (one per store)", n)
	}
}

func rosterOf(t *testing.T, w *chainWorld, mgr auth.Actor, storeID primitive.ObjectID, at time.Time) map[string]riderSummary {
	t.Helper()
	rs, err := w.svc.storeRidersAt(context.Background(), mgr, storeID.Hex(), "", at)
	if err != nil {
		t.Fatalf("roster of %s: %v", storeID.Hex(), err)
	}
	out := map[string]riderSummary{}
	for _, r := range rs {
		out[r.PartyID] = r
	}
	return out
}

// A rider may claim an offer only from a store they ride for: the pool never
// shows another store's offers, and the claim by id is refused the same way,
// on duty or not, fallback or not.
func TestRiderCannotClaimAnotherStoresOffer(t *testing.T) {
	w, _, done := chainDutyWorld(t)
	defer done()
	ctx := context.Background()
	now := dutyAt(dutyDay, 10, 0)
	storeB, _ := chainOtherStore(t, w)
	stranger := chainAddRider(t, w, storeB, "Store B Rider")

	ord, task := chainInstantOffer(t, w, "9000006213")
	// Nobody at store A is on duty (its pool is in fallback) and the stranger
	// is off duty: refused.
	if _, err := w.svc.claimOfferedDeliveryAt(ctx, stranger, task.ID, now); geofenceCode(err) != "FORBIDDEN" {
		t.Fatalf("another store's rider (off duty, fallback): %v, want FORBIDDEN", err)
	}
	// On duty at their own store: still refused.
	chainCheckIn(t, w, stranger.PartyID, dutyDay)
	if poolHas(t, w, stranger, task.ID, now) {
		t.Fatal("another store's offer in the stranger's pool")
	}
	if _, err := w.svc.claimOfferedDeliveryAt(ctx, stranger, task.ID, now); geofenceCode(err) != "FORBIDDEN" {
		t.Fatalf("another store's rider (on duty): %v, want FORBIDDEN", err)
	}
	if d := chainTaskFor(t, w, ord.OrderID); d.Status != "OFFERED" || d.RiderPartyID != "" {
		t.Fatalf("a refused claim changed the offer: %+v", d)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "placed" {
		t.Fatalf("a refused claim moved the order: %q", o.Status)
	}
	// The store's own rider still claims it.
	if d, err := w.svc.claimOfferedDeliveryAt(ctx, w.rider, task.ID, now); err != nil || d.RiderPartyID != w.riderID.Hex() {
		t.Fatalf("the store's own rider: %v %v", d, err)
	}
}

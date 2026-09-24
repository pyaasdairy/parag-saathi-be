package consumer

// TONIGHT-ONLY INSTANT OVERRIDES (founder, 24 Sep): at closing time the store
// manager either keeps instant open a little longer or closes it now.
//
//	POST /consumer/stores/{storeId}/zone/instant/extend    {minutes: 30|60|120}
//	POST /consumer/stores/{storeId}/zone/instant/close-now
//
// Both answer the zone in the GET /zone shape. An extension runs from the
// later of now, tonight's closing time and the current extension, never past
// 02:00 IST; close-now shuts instant until the next opening time without the
// persistent pause, so it reopens by itself. Fixed IST clock throughout.

import (
	"context"
	"encoding/json"
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

// ihOp calls extend ("extend", body {minutes}) or close-now ("close-now") as
// the given operator, on the service clock, and returns the status plus the
// {data} or {error} object.
func ihOp(t *testing.T, w *chainWorld, actor auth.Actor, storeID, op string, minutes int) (int, map[string]any) {
	t.Helper()
	h := &handler{svc: w.svc}
	body := "{}"
	if op == "extend" {
		b, _ := json.Marshal(map[string]any{"minutes": minutes})
		body = string(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/stores/"+storeID+"/zone/instant/"+op, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("storeId", storeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req = req.WithContext(auth.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	switch op {
	case "extend":
		h.extendInstant(rec, req)
	case "close-now":
		h.closeInstantNow(rec, req)
	default:
		t.Fatalf("unknown op %q", op)
	}
	var env struct {
		Data  map[string]any `json:"data"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s body: %v %s", op, err, rec.Body.String())
	}
	if env.Error != nil {
		return rec.Code, env.Error
	}
	return rec.Code, env.Data
}

// ihUTC is the wire form of an IST moment (RFC3339, UTC), as the zone view
// and /serviceability write it.
func ihUTC(at time.Time) string { return at.UTC().Format(time.RFC3339) }

func ihStoredZone(t *testing.T, w *chainWorld, storeID string) *zone {
	t.Helper()
	z, err := w.svc.repo.getZone(context.Background(), storeID)
	if err != nil || z == nil {
		t.Fatalf("reload zone %s: %v %v", storeID, z, err)
	}
	return z
}

// ihInstantAt is the instant verdict /serviceability gives 1 km from the store.
func ihInstantAt(t *testing.T, w *chainWorld, at time.Time) *serviceabilityResult {
	t.Helper()
	p1 := pointAtBearing(guardCenter, 1000, 90)
	sv, err := w.svc.serviceabilityAt(context.Background(), p1.Lat, p1.Lng, "", at)
	if err != nil || !sv.Serviceable {
		t.Fatalf("serviceability at %s: %+v %v", at.In(istZone).Format("15:04"), sv, err)
	}
	return sv
}

// ── extend 60 at 21:50: open until 23:00 ───────────────────────────────────

func TestInstantExtendKeepsInstantOpenUntilTheNewTime(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000}) // 07:00-22:00 as shown
	store := w.storeID.Hex()
	p1 := pointAtBearing(guardCenter, 1000, 90)

	ihClock(w, ihAt(21, 50))
	code, v := ihOp(t, w, w.mgr, store, "extend", 60)
	if code != http.StatusOK {
		t.Fatalf("extend: %d %v", code, v)
	}
	want := ihUTC(ihAt(23, 0))
	if v["instantExtendedUntil"] != want || v["instant_extended_until"] != want {
		t.Fatalf("extended until %v / %v, want %s", v["instantExtendedUntil"], v["instant_extended_until"], want)
	}
	if v["instantOpenNow"] != true || v["instant_open_now"] != true || v["instantClosesAt"] != want || v["instant_closes_at"] != want {
		t.Fatalf("open now / closes at: %v %v %v %v", v["instantOpenNow"], v["instant_open_now"], v["instantClosesAt"], v["instant_closes_at"])
	}
	// The same shape GET /zone returns: every existing key is still there.
	for _, k := range []string{"storeId", "active", "configured", "center", "standardRadiusM", "instantRadiusM",
		"standard_radius_km", "instant_radius_km", "instantOpenMin", "instantCloseMin", "instantPaused",
		"instant_open_min", "instant_close_min", "instant_paused", "monsoonEnabled", "monsoon_rupees", "updatedAt"} {
		if _, ok := v[k]; !ok {
			t.Fatalf("zone view lost key %q: %v", k, v)
		}
	}
	if v["instantOpenMin"] != float64(420) || v["instantCloseMin"] != float64(1320) || v["instantPaused"] != false {
		t.Fatalf("saved hours / pause must not move: %v %v %v", v["instantOpenMin"], v["instantCloseMin"], v["instantPaused"])
	}
	if z := ihStoredZone(t, w, store); z.InstantPaused || z.InstantExtendedUntil == nil || !z.InstantExtendedUntil.Equal(ihAt(23, 0)) {
		t.Fatalf("stored: paused %v extended %v", z.InstantPaused, z.InstantExtendedUntil)
	}

	cid := w.customer(t, "9000008201", 0)
	ihClock(w, ihAt(22, 30))
	if sv := ihInstantAt(t, w, ihAt(22, 30)); !sv.Instant || sv.InstantClosed {
		t.Fatalf("22:30 inside the extension: %+v", sv)
	}
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusCreated {
		t.Fatalf("instant order at 22:30: %d %v, want 201", code, out)
	}
	ihClock(w, ihAt(23, 1))
	sv := ihInstantAt(t, w, ihAt(23, 1))
	if sv.Instant || !sv.InstantClosed || sv.InstantResumesLabel != "tomorrow at 7:00 AM" {
		t.Fatalf("23:01 after the extension: %+v", sv)
	}
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusUnprocessableEntity || out["code"] != "INSTANT_CLOSED" {
		t.Fatalf("instant order at 23:01: %d %v, want 422 INSTANT_CLOSED", code, out)
	}
	// It simply expires: the next morning is the ordinary hours again.
	if sv := ihInstantAt(t, w, ihAt(24+7, 0)); !sv.Instant {
		t.Fatalf("07:00 next day: %+v", sv)
	}

	// A second extension runs from the current one, not from 22:00.
	ihClock(w, ihAt(22, 40))
	if code, v = ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(23, 30)) {
		t.Fatalf("extend 30 on top of 23:00: %d %v", code, v["instantExtendedUntil"])
	}
	// Asked during the day, it extends tonight's close.
	ihClock(w, ihAt(24+15, 0))
	if code, v = ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(24+22, 30)) {
		t.Fatalf("extend 30 at 15:00: %d %v", code, v["instantExtendedUntil"])
	}
	// After closing (no extension left), it reopens instant from now.
	ihClock(w, ihAt(48+22, 40))
	if code, v = ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(48+23, 10)) || v["instantOpenNow"] != true {
		t.Fatalf("extend 30 at 22:40 after close: %d %v open %v", code, v["instantExtendedUntil"], v["instantOpenNow"])
	}
}

// ── refusals: paused, no instant radius, no zone, a bad length ─────────────

func TestInstantExtendRefusals(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "") // the flag widens instant to a zone with no instant radius
	store := w.storeID.Hex()
	ihClock(w, ihAt(21, 50))

	// No zone drawn yet: there is no instant lane to extend.
	if code, e := ihOp(t, w, w.mgr, store, "extend", 60); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_NOT_CONFIGURED" {
		t.Fatalf("no zone: %d %v", code, e)
	}
	// A zone with no instant radius.
	ihZone(t, w, zone{StandardRadiusM: 8000})
	if code, e := ihOp(t, w, w.mgr, store, "extend", 60); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_NOT_CONFIGURED" {
		t.Fatalf("no instant radius: %d %v", code, e)
	}
	// Paused: the manager's switch means "until I turn it back on".
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantPaused: true})
	code, e := ihOp(t, w, w.mgr, store, "extend", 60)
	if code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_PAUSED" {
		t.Fatalf("paused: %d %v", code, e)
	}
	if z := ihStoredZone(t, w, store); !z.InstantPaused || z.InstantExtendedUntil != nil {
		t.Fatalf("a refused extension changed the zone: paused %v extended %v", z.InstantPaused, z.InstantExtendedUntil)
	}
	// Only 30, 60 or 120 minutes.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	for _, m := range []int{0, 15, 45, 90, 180, -30} {
		if code, e := ihOp(t, w, w.mgr, store, "extend", m); code != http.StatusUnprocessableEntity || e["code"] != "INVALID_EXTENSION" {
			t.Fatalf("extend %d: %d %v", m, code, e)
		}
	}
}

// ── the cap: never past 02:00 IST ──────────────────────────────────────────

func TestInstantExtendNeverPastTwoAM(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	store := w.storeID.Hex()

	ihClock(w, ihAt(21, 50))
	steps := []struct {
		minutes int
		until   time.Time
	}{{120, ihAt(24, 0)}, {60, ihAt(24+1, 0)}, {120, ihAt(24+2, 0)}} // the last one is cut at 02:00
	for _, s := range steps {
		code, v := ihOp(t, w, w.mgr, store, "extend", s.minutes)
		if code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(s.until) {
			t.Fatalf("extend %d: %d %v, want %s", s.minutes, code, v["instantExtendedUntil"], ihUTC(s.until))
		}
	}
	code, e := ihOp(t, w, w.mgr, store, "extend", 30)
	if code != http.StatusUnprocessableEntity || e["code"] != "EXTEND_TOO_LATE" {
		t.Fatalf("past 02:00: %d %v", code, e)
	}
	if z := ihStoredZone(t, w, store); !z.InstantExtendedUntil.Equal(ihAt(24+2, 0)) {
		t.Fatalf("a refused extension moved the stored one: %v", z.InstantExtendedUntil)
	}
	if sv := ihInstantAt(t, w, ihAt(24+1, 59)); !sv.Instant {
		t.Fatalf("01:59 inside the capped extension: %+v", sv)
	}
	if sv := ihInstantAt(t, w, ihAt(24+2, 0)); sv.Instant || sv.InstantResumesLabel != "today at 7:00 AM" {
		t.Fatalf("02:00: %+v", sv)
	}
	// Small hours before opening belong to last night: too late to extend.
	ihClock(w, ihAt(24+5, 0))
	if code, e := ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusUnprocessableEntity || e["code"] != "EXTEND_TOO_LATE" {
		t.Fatalf("05:00: %d %v", code, e)
	}
}

// ── close-now at 21:00: shut now, open again at 07:00, pause untouched ─────

func TestInstantCloseNowReopensAtTheNextOpeningTime(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	store := w.storeID.Hex()
	p1 := pointAtBearing(guardCenter, 1000, 90)

	// An extension is running; close-now clears it.
	ihClock(w, ihAt(20, 0))
	if code, v := ihOp(t, w, w.mgr, store, "extend", 60); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(23, 0)) {
		t.Fatalf("extend: %d %v", code, v)
	}
	ihClock(w, ihAt(21, 0))
	code, v := ihOp(t, w, w.mgr, store, "close-now", 0)
	if code != http.StatusOK {
		t.Fatalf("close-now: %d %v", code, v)
	}
	next := ihUTC(ihAt(24+7, 0))
	if v["instantClosedUntil"] != next || v["instant_closed_until"] != next || v["instantOpenNow"] != false || v["instantPaused"] != false {
		t.Fatalf("close-now view: closed until %v / %v open %v paused %v", v["instantClosedUntil"], v["instant_closed_until"], v["instantOpenNow"], v["instantPaused"])
	}
	if v["instantExtendedUntil"] != nil || v["instant_extended_until"] != nil || v["instantClosesAt"] != nil {
		t.Fatalf("close-now keeps no extension: %v %v", v["instantExtendedUntil"], v["instantClosesAt"])
	}
	z := ihStoredZone(t, w, store)
	if z.InstantPaused || z.InstantExtendedUntil != nil || z.InstantClosedUntil == nil || !z.InstantClosedUntil.Equal(ihAt(24+7, 0)) {
		t.Fatalf("stored after close-now: paused %v extended %v closed until %v", z.InstantPaused, z.InstantExtendedUntil, z.InstantClosedUntil)
	}

	sv := ihInstantAt(t, w, ihAt(21, 0))
	if sv.Instant || !sv.InstantClosed || sv.InstantResumesLabel != "tomorrow at 7:00 AM" || sv.InstantResumesAt != next {
		t.Fatalf("21:00 after close-now: %+v", sv)
	}
	cid := w.customer(t, "9000008301", 0)
	ihClock(w, ihAt(21, 30))
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusUnprocessableEntity || out["code"] != "INSTANT_CLOSED" {
		t.Fatalf("instant order at 21:30 after close-now: %d %v", code, out)
	}
	if code, out := ihPost(t, w, cid, "morning", p1); code != http.StatusCreated {
		t.Fatalf("morning order at 21:30 after close-now: %d %v", code, out)
	}
	if sv := ihInstantAt(t, w, ihAt(24+6, 59)); sv.Instant {
		t.Fatalf("06:59 next day is still closed: %+v", sv)
	}
	// It reopens by itself: no Save, no pause switch.
	if sv := ihInstantAt(t, w, ihAt(24+7, 0)); !sv.Instant || sv.InstantClosed {
		t.Fatalf("07:00 next day: %+v", sv)
	}
	ihClock(w, ihAt(24+7, 5))
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusCreated {
		t.Fatalf("instant order at 07:05 next day: %d %v", code, out)
	}

	// Before the morning opening, close-now shuts it until today's opening.
	ihClock(w, ihAt(48+6, 0))
	if code, v := ihOp(t, w, w.mgr, store, "close-now", 0); code != http.StatusOK || v["instantClosedUntil"] != ihUTC(ihAt(48+7, 0)) {
		t.Fatalf("close-now at 06:00: %d %v", code, v["instantClosedUntil"])
	}
	// Extend after close-now reopens: the manager's latest word wins.
	ihClock(w, ihAt(72+21, 0))
	ihOp(t, w, w.mgr, store, "close-now", 0)
	ihClock(w, ihAt(72+21, 10))
	if code, v := ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusOK || v["instantClosedUntil"] != nil || v["instantOpenNow"] != true ||
		v["instantExtendedUntil"] != ihUTC(ihAt(72+22, 30)) {
		t.Fatalf("extend after close-now: %d %v", code, v)
	}

	// A Save of the zone (the console's PUT) keeps tonight's override.
	in := zoneInput{Center: &geoPt{Lat: guardCenter.Lat, Lng: guardCenter.Lng}, StandardRadiusM: 8000, InstantRadiusM: 2500}
	if _, err := w.svc.upsertZone(context.Background(), w.mgr, store, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	if z := ihStoredZone(t, w, store); z.InstantExtendedUntil == nil || !z.InstantExtendedUntil.Equal(ihAt(72+22, 30)) {
		t.Fatalf("a Save dropped tonight's extension: %v", z.InstantExtendedUntil)
	}
}

// ── a manager touches only their own store ─────────────────────────────────

// ihSecondStore adds store B (with coordinates) and its own STORE_MANAGER.
func ihSecondStore(t *testing.T, w *chainWorld) (primitive.ObjectID, auth.Actor) {
	t.Helper()
	ctx := context.Background()
	storeB, mgrB := primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := w.db.Collection("org_units").InsertOne(ctx, bson.D{
		{Key: "_id", Value: storeB}, {Key: "type", Value: "STORE"}, {Key: "active", Value: true},
		{Key: "name", Value: "PYAAS Second Store"}, {Key: "geo_lat", Value: 26.8500}, {Key: "geo_lng", Value: 81.0000},
	}); err != nil {
		t.Fatalf("store B: %v", err)
	}
	if _, err := w.db.Collection("parties").InsertOne(ctx, bson.D{
		{Key: "_id", Value: mgrB}, {Key: "full_name", Value: "Manager B"}, {Key: "phone", Value: "+919900000077"},
	}); err != nil {
		t.Fatalf("manager B: %v", err)
	}
	if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: mgrB}, {Key: "role_code", Value: "STORE_MANAGER"},
		{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: storeB},
	}); err != nil {
		t.Fatalf("manager B role: %v", err)
	}
	return storeB, auth.Actor{PartyID: mgrB.Hex(), Kind: "role", RoleCode: "STORE_MANAGER"}
}

func TestInstantOverridesOnlyOnTheManagersOwnStore(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	storeB, mgrB := ihSecondStore(t, w)
	zb := zone{StoreID: storeB.Hex(), Active: true, Center: newGeoPoint(26.85, 81.0), InstantRadiusM: 2500, StandardRadiusM: 8000}
	if _, err := w.svc.repo.upsertZone(context.Background(), &zb); err != nil {
		t.Fatalf("zone B: %v", err)
	}
	ihClock(w, ihAt(21, 50))

	// Manager A on store B: refused exactly as putZone refuses (403).
	for _, op := range []string{"extend", "close-now"} {
		code, e := ihOp(t, w, w.mgr, storeB.Hex(), op, 60)
		if code != http.StatusForbidden || e["code"] != "FORBIDDEN" {
			t.Fatalf("manager A %s on store B: %d %v", op, code, e)
		}
	}
	if _, err := w.svc.upsertZone(context.Background(), w.mgr, storeB.Hex(), zoneInput{
		Center: &geoPt{Lat: 26.85, Lng: 81.0}, StandardRadiusM: 8000,
	}); err == nil || err.(*apiError).status != http.StatusForbidden {
		t.Fatalf("precondition: putZone refuses manager A on store B with 403, got %v", err)
	}
	if z := ihStoredZone(t, w, storeB.Hex()); z.InstantExtendedUntil != nil || z.InstantClosedUntil != nil {
		t.Fatalf("store B changed by manager A: %v %v", z.InstantExtendedUntil, z.InstantClosedUntil)
	}
	// Store B's own manager can.
	if code, v := ihOp(t, w, mgrB, storeB.Hex(), "extend", 60); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(23, 0)) {
		t.Fatalf("manager B extends B: %d %v", code, v)
	}
}

// ── the zone view reports a live instant state only where there is a lane ──
//
// Before: GET /zone answered instantOpenNow true and instantClosesAt 10 PM by
// day for a zone with no instant radius and for an inactive zone, although
// the app offers no instant there and extend refuses it, so the planned
// "closes at 10 PM / Extend" UI would show for a lane that does not exist.

func TestInstantZoneViewReportsNoLiveStateWithoutALane(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	store := w.storeID.Hex()
	at := ihAt(10, 0)
	closes := ihUTC(ihAt(22, 0))
	check := func(what string, z *zone, open bool, closesAt any) {
		t.Helper()
		v := zoneViewAt(z, store, at)
		if v["instantOpenNow"] != open || v["instant_open_now"] != open || v["instantClosesAt"] != closesAt || v["instant_closes_at"] != closesAt {
			t.Fatalf("%s at 10:00: open %v/%v closes %v/%v, want %v %v", what,
				v["instantOpenNow"], v["instant_open_now"], v["instantClosesAt"], v["instant_closes_at"], open, closesAt)
		}
	}

	t.Setenv("INSTANT_TEST_OPEN", "")
	std := ihZone(t, w, zone{StandardRadiusM: 8000})
	check("no instant radius", std, false, nil)
	// The flag widens instant to it: then it is a lane, hours and all.
	t.Setenv("INSTANT_TEST_OPEN", "true")
	check("no instant radius, INSTANT_TEST_OPEN on", std, true, closes)
	t.Setenv("INSTANT_TEST_OPEN", "")

	inst := ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	check("instant radius", inst, true, closes) // unchanged
	off := *inst
	off.Active = false
	check("inactive zone", &off, false, nil)
	// Every other key is still there, and the nil (no zone) view is unchanged.
	if v := zoneViewAt(std, store, at); v["instantOpenMin"] != 420 || v["instantCloseMin"] != 1320 || v["configured"] != true {
		t.Fatalf("no-radius view hours: %v", v)
	}
	if v := zoneViewAt(nil, store, at); v["instantOpenNow"] != false || v["instantClosesAt"] != nil {
		t.Fatalf("no-zone view: %v", v)
	}
}

// ── the window arithmetic behind both, pure ─────────────────────────────────

func TestInstantHoursWindowArithmetic(t *testing.T) {
	day := zone{InstantOpenMin: 420, InstantCloseMin: 1320} // 07:00-22:00
	if o, c, in := hoursWindowAt(&day, ihAt(21, 50)); !in || !o.Equal(ihAt(7, 0)) || !c.Equal(ihAt(22, 0)) {
		t.Fatalf("21:50 in today's window: %v %v %v", o, c, in)
	}
	if o, c, in := hoursWindowAt(&day, ihAt(24+1, 0)); in || !o.Equal(ihAt(7, 0)) || !c.Equal(ihAt(22, 0)) {
		t.Fatalf("01:00 belongs to last night's window: %v %v %v", o, c, in)
	}
	if n := nextInstantOpening(&day, ihAt(22, 0)); !n.Equal(ihAt(24+7, 0)) {
		t.Fatalf("next opening after 22:00: %v", n)
	}
	if n := nextInstantOpening(&day, ihAt(6, 0)); !n.Equal(ihAt(7, 0)) {
		t.Fatalf("next opening at 06:00: %v", n)
	}
	if at, ok := instantClosesAt(&day, ihAt(21, 45)); !ok || !at.Equal(ihAt(22, 0)) {
		t.Fatalf("closes at: %v %v", at, ok)
	}
	if _, ok := instantClosesAt(&day, ihAt(22, 30)); ok {
		t.Fatalf("a shut lane has no closing time")
	}
	// Overnight 22:00-06:00: 02:00 is inside the window that opened last night.
	night := zone{InstantOpenMin: 1320, InstantCloseMin: 360}
	if o, c, in := hoursWindowAt(&night, ihAt(24+2, 0)); !in || !o.Equal(ihAt(22, 0)) || !c.Equal(ihAt(24+6, 0)) {
		t.Fatalf("overnight 02:00: %v %v %v", o, c, in)
	}
	// Round the clock never closes, so it never has a closing time.
	allDay := zone{InstantOpenMin: 0, InstantCloseMin: 1440}
	if _, ok := instantClosesAt(&allDay, ihAt(23, 50)); ok {
		t.Fatalf("00:00-24:00 has no closing time")
	}
	// An extension moves the closing time; a stale one does not.
	ext := ihAt(23, 0)
	day.InstantExtendedUntil = &ext
	if at, ok := instantClosesAt(&day, ihAt(21, 45)); !ok || !at.Equal(ihAt(23, 0)) {
		t.Fatalf("extended closes at: %v %v", at, ok)
	}
	if at, ok := instantClosesAt(&day, ihAt(24+10, 0)); !ok || !at.Equal(ihAt(24+22, 0)) {
		t.Fatalf("yesterday's extension ignored: %v %v", at, ok)
	}
}

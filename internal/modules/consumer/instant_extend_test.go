package consumer

// TONIGHT-ONLY INSTANT OVERRIDES (founder, 24 Sep): at closing time the store
// manager either keeps instant open a little longer or closes it now.
//
//	POST /consumer/stores/{storeId}/zone/instant/extend    {minutes: 30|60|120}
//	POST /consumer/stores/{storeId}/zone/instant/close-now
//	POST /consumer/stores/{storeId}/zone/instant/reopen    (undoes a close-now)
//
// All answer the zone in the GET /zone shape. An extension runs from the
// later of now, tonight's closing time and the current extension, never past
// 02:00 IST; close-now shuts instant until the next opening time without
// touching the pause switch, so it reopens by itself. Fixed IST clock throughout.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// ihOp calls extend ("extend", body {minutes}), close-now ("close-now") or
// reopen ("reopen") as
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
	case "reopen":
		h.reopenInstant(rec, req)
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
	// Paused (a pause with no end: it holds until switched off; one with an end
	// is refused the same way while it holds, instant_pause_test.go).
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

// ── reopen undoes a close-now ──────────────────────────────────────────────
//
// Before: close-now had no undo. After a daytime close-now (10:00) instant
// stayed shut until 07:00 the next day: switching the Zone tab's pause off
// did nothing (close-now never sets it), and extend reopened it only by also
// moving tonight's close. Reopen clears the close-now alone.

func TestInstantReopenUndoesACloseNow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000}) // 07:00-22:00
	store := w.storeID.Hex()
	p1 := pointAtBearing(guardCenter, 1000, 90)
	cid := w.customer(t, "9000008401", 0)

	ihClock(w, ihAt(10, 0))
	if code, v := ihOp(t, w, w.mgr, store, "close-now", 0); code != http.StatusOK || v["instantClosedUntil"] != ihUTC(ihAt(24+7, 0)) {
		t.Fatalf("close-now at 10:00: %d %v", code, v)
	}
	if sv := ihInstantAt(t, w, ihAt(10, 30)); sv.Instant {
		t.Fatalf("10:30 after close-now: %+v", sv)
	}
	ihClock(w, ihAt(11, 0))
	code, v := ihOp(t, w, w.mgr, store, "reopen", 0)
	if code != http.StatusOK || v["instantOpenNow"] != true || v["instant_open_now"] != true || v["instantClosedUntil"] != nil ||
		v["instant_closed_until"] != nil || v["instantExtendedUntil"] != nil || v["instantPaused"] != false {
		t.Fatalf("reopen at 11:00: %d %v", code, v)
	}
	// Tonight's close is not moved.
	if v["instantClosesAt"] != ihUTC(ihAt(22, 0)) {
		t.Fatalf("reopen moved tonight's close: %v", v["instantClosesAt"])
	}
	if z := ihStoredZone(t, w, store); z.InstantClosedUntil != nil || z.InstantExtendedUntil != nil || z.InstantPaused {
		t.Fatalf("stored after reopen: closed %v extended %v paused %v", z.InstantClosedUntil, z.InstantExtendedUntil, z.InstantPaused)
	}
	if sv := ihInstantAt(t, w, ihAt(11, 0)); !sv.Instant || sv.InstantClosed {
		t.Fatalf("11:00 after reopen: %+v", sv)
	}
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusCreated {
		t.Fatalf("instant order at 11:00 after reopen: %d %v", code, out)
	}
	if sv := ihInstantAt(t, w, ihAt(22, 1)); sv.Instant {
		t.Fatalf("22:01: the hours still close it: %+v", sv)
	}

	// After the hours have closed, reopen only cancels the close-now: the
	// hours still hold (extend is what opens past them).
	ihClock(w, ihAt(24+21, 0))
	ihOp(t, w, w.mgr, store, "close-now", 0)
	ihClock(w, ihAt(24+23, 0))
	if code, v := ihOp(t, w, w.mgr, store, "reopen", 0); code != http.StatusOK || v["instantOpenNow"] != false || v["instantClosedUntil"] != nil {
		t.Fatalf("reopen at 23:00: %d %v", code, v)
	}
	if sv := ihInstantAt(t, w, ihAt(24+23, 0)); sv.Instant || sv.InstantResumesLabel != "tomorrow at 7:00 AM" {
		t.Fatalf("23:00 after reopen: %+v", sv)
	}

	// Nothing to undo: 200 and the zone as it is. An extension survives it.
	ihClock(w, ihAt(48+21, 50))
	if code, v := ihOp(t, w, w.mgr, store, "extend", 60); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(48+23, 0)) {
		t.Fatalf("extend: %d %v", code, v)
	}
	if code, v := ihOp(t, w, w.mgr, store, "reopen", 0); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(48+23, 0)) || v["instantOpenNow"] != true {
		t.Fatalf("reopen with nothing to undo: %d %v", code, v)
	}

	// Refusals: another store's manager, the pause while it holds, a store
	// with no instant lane.
	_, mgrB := ihSecondStore(t, w)
	if code, e := ihOp(t, w, mgrB, store, "reopen", 0); code != http.StatusForbidden || e["code"] != "FORBIDDEN" {
		t.Fatalf("manager B reopens A: %d %v", code, e)
	}
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantPaused: true})
	if code, e := ihOp(t, w, w.mgr, store, "reopen", 0); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_PAUSED" {
		t.Fatalf("reopen while paused: %d %v", code, e)
	}
	ihZone(t, w, zone{StandardRadiusM: 8000})
	if code, e := ihOp(t, w, w.mgr, store, "reopen", 0); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_NOT_CONFIGURED" {
		t.Fatalf("reopen with no instant lane: %d %v", code, e)
	}

	// Mounted beside extend and close-now, in the STORE_MANAGER group.
	src, err := os.ReadFile("module.go")
	if err != nil {
		t.Fatalf("read module.go: %v", err)
	}
	s := string(src)
	i := strings.Index(s, `sm.Post("/stores/{storeId}/zone/instant/close-now", h.closeInstantNow)`)
	j := strings.Index(s, `sm.Post("/stores/{storeId}/zone/instant/reopen", h.reopenInstant)`)
	if i < 0 || j < 0 || j < i || strings.Contains(s[i:j], "})") {
		t.Fatalf("the reopen route is not mounted beside close-now in the STORE_MANAGER group")
	}
}

// ihOpRaw posts a raw body to an instant route as the given operator, on the
// service clock; safe off the test goroutine (it never calls t).
func ihOpRaw(w *chainWorld, actor auth.Actor, storeID, op, body string) (int, map[string]any) {
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodPost, "/stores/"+storeID+"/zone/instant/"+op, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("storeId", storeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req = req.WithContext(auth.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.extendInstant(rec, req)
	var env struct {
		Data  map[string]any `json:"data"`
		Error map[string]any `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != nil {
		return rec.Code, env.Error
	}
	return rec.Code, env.Data
}

// ── two extends at once both count; a retried request counts once ─────────
//
// Before: extend read the zone, worked out the new close and wrote it
// unchecked. Two managers tapping +60 together at 21:50 ended at 23:00, one
// tap lost; and a retry of the same request (a timeout, then the same POST
// again) added its minutes a second time.

func TestInstantExtendConcurrentAndRetried(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ctx := context.Background()
	store := w.storeID.Hex()
	mgr2 := primitive.NewObjectID()
	if _, err := w.db.Collection("parties").InsertOne(ctx, bson.D{{Key: "_id", Value: mgr2}, {Key: "phone", Value: "+919900000079"}}); err != nil {
		t.Fatalf("manager 2: %v", err)
	}
	if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: mgr2}, {Key: "role_code", Value: "STORE_MANAGER"},
		{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: w.storeID},
	}); err != nil {
		t.Fatalf("manager 2 role: %v", err)
	}
	second := auth.Actor{PartyID: mgr2.Hex(), Kind: "role", RoleCode: "STORE_MANAGER"}
	reset := func() {
		t.Helper()
		ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
		if _, err := w.db.Collection(collStoreZones).UpdateOne(ctx, bson.D{{Key: "store_id", Value: store}}, bson.D{{Key: "$unset", Value: bson.D{
			{Key: "instant_extended_until", Value: ""}, {Key: "instant_closed_until", Value: ""}, {Key: "instant_extend_requests", Value: ""},
		}}}); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	// together runs each (actor, body) at the same moment and answers the codes.
	together := func(calls []struct {
		actor auth.Actor
		body  string
	}) []int {
		codes := make([]int, len(calls))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, c := range calls {
			wg.Add(1)
			go func(i int, actor auth.Actor, body string) {
				defer wg.Done()
				<-start
				codes[i], _ = ihOpRaw(w, actor, store, "extend", body)
			}(i, c.actor, c.body)
		}
		close(start)
		wg.Wait()
		return codes
	}
	storedUntil := func() time.Time {
		t.Helper()
		z := ihStoredZone(t, w, store)
		if z.InstantExtendedUntil == nil {
			return time.Time{}
		}
		return *z.InstantExtendedUntil
	}
	ihClock(w, ihAt(21, 50))

	// Two managers, +60 each, at the same moment: both count (22:00 + 2 h).
	lost := 0
	for run := 0; run < 10; run++ {
		reset()
		codes := together([]struct {
			actor auth.Actor
			body  string
		}{{w.mgr, `{"minutes":60}`}, {second, `{"minutes":60}`}})
		if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
			t.Fatalf("run %d: codes %v", run, codes)
		}
		if !storedUntil().Equal(ihAt(24, 0)) {
			lost++
		}
	}
	if lost != 0 {
		t.Fatalf("two simultaneous +60 extends: %d of 10 runs lost one (stored %v, want midnight)", lost, storedUntil().In(istZone))
	}

	// A retried request (same request_id) counts once; a new one stacks.
	reset()
	if code, v := ihOpRaw(w, w.mgr, store, "extend", `{"minutes":60,"request_id":"tap-1"}`); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(23, 0)) {
		t.Fatalf("tap-1: %d %v", code, v)
	}
	if code, v := ihOpRaw(w, w.mgr, store, "extend", `{"minutes":60,"request_id":"tap-1"}`); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(23, 0)) {
		t.Fatalf("tap-1 retried: %d %v, want 23:00 unchanged", code, v)
	}
	// The same retry racing itself still counts once.
	together([]struct {
		actor auth.Actor
		body  string
	}{{w.mgr, `{"minutes":60,"requestId":"tap-2"}`}, {w.mgr, `{"minutes":60,"requestId":"tap-2"}`}})
	if u := storedUntil(); !u.Equal(ihAt(24, 0)) {
		t.Fatalf("tap-2 twice at once: stored %v, want midnight", u.In(istZone))
	}
	// A new request, and one with no request_id, stack as before.
	if code, v := ihOpRaw(w, w.mgr, store, "extend", `{"minutes":60,"request_id":"tap-3"}`); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(24+1, 0)) {
		t.Fatalf("tap-3: %d %v", code, v)
	}
	if code, v := ihOpRaw(w, w.mgr, store, "extend", `{"minutes":30}`); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(24+1, 30)) {
		t.Fatalf("no request_id: %d %v", code, v)
	}
	// An over-long request_id is refused before anything is written.
	long := strings.Repeat("x", 101)
	if code, e := ihOpRaw(w, w.mgr, store, "extend", `{"minutes":30,"request_id":"`+long+`"}`); code != http.StatusUnprocessableEntity || e["code"] != "INVALID_REQUEST_ID" {
		t.Fatalf("long request_id: %d %v", code, e)
	}
	if u := storedUntil(); !u.Equal(ihAt(24+1, 30)) {
		t.Fatalf("a refused request moved the extension: %v", u.In(istZone))
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

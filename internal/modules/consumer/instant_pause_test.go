package consumer

// THE "CLOSE INSTANT NOW" SWITCH ENDS AT THE NEXT OPENING TIME (ICR-11).
//
// Kushagra's Zone tab tells the manager, under the switch, "Instant is shut
// until you turn this off or the next opening time". The backend pause
// (instant_paused) used to hold until switched off, and /serviceability told
// the shopper "Instant resumes when the store turns it back on". The backend
// now does what the screen says:
//
//   - switching the pause ON (off -> on through the console's PUT) stores
//     instant_paused_until = the next opening time, worked out as the resume
//     label works it out (saved hours, else the 07:00 default);
//   - from that moment the pause no longer applies: instant follows its hours,
//     and the zone view reports instantPaused false, so the switch shows off;
//   - the console re-sends the switch on every Save: a Save while paused never
//     moves the end; switching it off clears it at once;
//   - a pause stored before this change (no instant_paused_until) keeps its old
//     meaning and holds until switched off.
//
// /serviceability names the real resume moment again, and the order guard
// agrees with it. Fixed IST clock throughout.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// ipConsoleBody is the Zone tab's Save body (StoreZone.toWire): snake_case,
// km radii, the hours as shown and the switch, sent whole on every Save.
func ipConsoleBody(open, close int, paused bool) map[string]any {
	return map[string]any{
		"center":             map[string]any{"lat": guardCenter.Lat, "lng": guardCenter.Lng},
		"standard_radius_km": 8.0, "instant_radius_km": 2.5,
		"include_pincodes": []string{}, "exclude_pincodes": []string{},
		"active": true, "monsoon_enabled": false, "monsoon_rupees": 15,
		"instant_open_min": open, "instant_close_min": close, "instant_paused": paused,
	}
}

// ipZoneCall runs GET (body nil) or PUT /consumer/stores/{storeId}/zone as the
// store manager through the real handler, on the service clock pinned to at,
// and returns the status plus the {data} or {error} object.
func ipZoneCall(t *testing.T, w *chainWorld, at time.Time, body map[string]any) (int, map[string]any) {
	t.Helper()
	ihClock(w, at)
	store := w.storeID.Hex()
	h := &handler{svc: w.svc}
	method, payload := http.MethodGet, ""
	if body != nil {
		b, _ := json.Marshal(body)
		method, payload = http.MethodPut, string(b)
	}
	req := httptest.NewRequest(method, "/stores/"+store+"/zone", strings.NewReader(payload))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("storeId", store)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req = req.WithContext(auth.WithActor(req.Context(), w.mgr))
	rec := httptest.NewRecorder()
	if body != nil {
		h.putZone(rec, req)
	} else {
		h.getZone(rec, req)
	}
	var env struct {
		Data  map[string]any `json:"data"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("zone %s body: %v %s", method, err, rec.Body.String())
	}
	if env.Error != nil {
		return rec.Code, env.Error
	}
	return rec.Code, env.Data
}

// ipSave is the console's Save at `at`; it must answer 200.
func ipSave(t *testing.T, w *chainWorld, at time.Time, open, close int, paused bool) map[string]any {
	t.Helper()
	code, v := ipZoneCall(t, w, at, ipConsoleBody(open, close, paused))
	if code != http.StatusOK {
		t.Fatalf("save at %s (paused %v): %d %v", at.In(istZone).Format("01-02 15:04"), paused, code, v)
	}
	return v
}

// ipSwitch asserts the switch the view shows and the pause's end (nil: none).
func ipSwitch(t *testing.T, what string, v map[string]any, paused bool, until any) {
	t.Helper()
	if v["instantPaused"] != paused || v["instant_paused"] != paused {
		t.Fatalf("%s: switch shows %v / %v, want %v", what, v["instantPaused"], v["instant_paused"], paused)
	}
	if v["instantPausedUntil"] != until || v["instant_paused_until"] != until {
		t.Fatalf("%s: paused until %v / %v, want %v", what, v["instantPausedUntil"], v["instant_paused_until"], until)
	}
}

// ipStoredUntil is the stored pause end (zero when none).
func ipStoredUntil(t *testing.T, w *chainWorld) (bool, time.Time) {
	t.Helper()
	z := ihStoredZone(t, w, w.storeID.Hex())
	if z.InstantPausedUntil == nil {
		return z.InstantPaused, time.Time{}
	}
	return z.InstantPaused, *z.InstantPausedUntil
}

// ── switched on at 21:00: shut until 07:00, then back by itself ────────────

func TestInstantPauseEndsAtTheNextOpeningTime(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	if err := w.svc.repo.ensureInstantAlertIndexes(context.Background()); err != nil {
		t.Fatalf("alert index: %v", err)
	}
	ctx := context.Background()
	store := w.storeID.Hex()
	p1 := pointAtBearing(guardCenter, 1000, 90)
	seven := ihUTC(ihAt(24+7, 0))

	ipSwitch(t, "saved with the switch off", ipSave(t, w, ihAt(9, 0), 420, 1320, false), false, nil)

	// 21:00: the manager turns "Close instant now" on.
	v := ipSave(t, w, ihAt(21, 0), 420, 1320, true)
	ipSwitch(t, "switched on at 21:00", v, true, seven)
	if v["instantOpenNow"] != false {
		t.Fatalf("switched on: instant open now %v", v["instantOpenNow"])
	}
	if paused, until := ipStoredUntil(t, w); !paused || !until.Equal(ihAt(24+7, 0)) {
		t.Fatalf("stored: paused %v until %v, want until 07:00 tomorrow", paused, until.In(istZone))
	}

	// The shopper is told when instant really comes back, and the order guard agrees.
	sv := ihInstantAt(t, w, ihAt(21, 30))
	if sv.Instant || !sv.InstantClosed || sv.InstantResumesLabel != "tomorrow at 7:00 AM" || sv.InstantResumesAt != seven {
		t.Fatalf("21:30 while paused: %+v", sv)
	}
	cid := w.customer(t, "9000008501", 0)
	ihClock(w, ihAt(21, 30))
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusUnprocessableEntity || out["code"] != "INSTANT_CLOSED" {
		t.Fatalf("instant order at 21:30 while paused: %d %v, want 422 INSTANT_CLOSED", code, out)
	}
	if code, out := ihPost(t, w, cid, "morning", p1); code != http.StatusCreated {
		t.Fatalf("morning order at 21:30 while paused: %d %v, want 201", code, out)
	}
	// Still a pause: extend and reopen refuse it, and no alert goes out.
	ihClock(w, ihAt(21, 40))
	if code, e := ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_PAUSED" {
		t.Fatalf("extend while paused: %d %v", code, e)
	}
	if code, e := ihOp(t, w, w.mgr, store, "reopen", 0); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_PAUSED" {
		t.Fatalf("reopen while paused: %d %v", code, e)
	}
	for _, at := range []time.Time{ihAt(21, 45), ihAt(22, 0), ihAt(22, 5)} {
		w.svc.instantAlertsTick(ctx, at)
	}
	if n := len(ihNotes(t, w, w.mgr.PartyID)); n != 0 {
		t.Fatalf("alerts while paused: %d", n)
	}

	// The console re-sends the switch on every Save: the end does not move.
	ipSwitch(t, "re-saved at 23:00", ipSave(t, w, ihAt(23, 0), 420, 1320, true), true, seven)
	if _, until := ipStoredUntil(t, w); !until.Equal(ihAt(24+7, 0)) {
		t.Fatalf("a re-save moved the end to %v", until.In(istZone))
	}
	if sv := ihInstantAt(t, w, ihAt(24+6, 59)); sv.Instant || sv.InstantResumesLabel != "today at 7:00 AM" || sv.InstantResumesAt != seven {
		t.Fatalf("06:59 while paused: %+v", sv)
	}

	// 07:00: the pause is over by itself. Instant is back and the switch shows off.
	if sv := ihInstantAt(t, w, ihAt(24+7, 0)); !sv.Instant || sv.InstantClosed {
		t.Fatalf("07:00, the pause has ended: %+v", sv)
	}
	ihClock(w, ihAt(24+7, 5))
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusCreated {
		t.Fatalf("instant order at 07:05 after the pause: %d %v, want 201", code, out)
	}
	code, v := ipZoneCall(t, w, ihAt(24+7, 5), nil)
	if code != http.StatusOK {
		t.Fatalf("GET zone at 07:05: %d %v", code, v)
	}
	ipSwitch(t, "GET at 07:05", v, false, nil)
	if v["instantOpenNow"] != true || v["instantClosesAt"] != ihUTC(ihAt(24+22, 0)) {
		t.Fatalf("GET at 07:05: open %v closes %v", v["instantOpenNow"], v["instantClosesAt"])
	}
	// The lane is the manager's again: tonight's alert goes out, reopen is no longer refused.
	w.svc.instantAlertsTick(ctx, ihAt(24+21, 45))
	if n := ihCount(t, w, w.mgr.PartyID, templateStoreInstantClosing); n != 1 {
		t.Fatalf("closing alerts the evening after the pause: %d, want 1", n)
	}
	ihClock(w, ihAt(24+21, 50))
	if code, v := ihOp(t, w, w.mgr, store, "reopen", 0); code != http.StatusOK || v["instantOpenNow"] != true {
		t.Fatalf("reopen after the pause ended: %d %v", code, v)
	}
	if code, v := ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusOK || v["instantExtendedUntil"] != ihUTC(ihAt(24+22, 30)) {
		t.Fatalf("extend after the pause ended: %d %v", code, v)
	}

	// Saving the form as it now shows (switch off) clears the stored pause.
	ipSwitch(t, "saved off", ipSave(t, w, ihAt(24+22, 0), 420, 1320, false), false, nil)
	if paused, until := ipStoredUntil(t, w); paused || !until.IsZero() {
		t.Fatalf("stored after switching off: paused %v until %v", paused, until)
	}
}

// ── when it is switched on decides the end; off ends it at once ────────────

func TestInstantPauseSwitchOnAndOff(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")

	// Before today's opening: until today's opening.
	ipSave(t, w, ihAt(4, 0), 420, 1320, false)
	ipSwitch(t, "switched on at 05:00", ipSave(t, w, ihAt(5, 0), 420, 1320, true), true, ihUTC(ihAt(7, 0)))
	if sv := ihInstantAt(t, w, ihAt(6, 0)); sv.InstantResumesLabel != "today at 7:00 AM" || sv.InstantResumesAt != ihUTC(ihAt(7, 0)) {
		t.Fatalf("06:00, paused at 05:00: %+v", sv)
	}

	// Off: instant is back at once (inside the hours), nothing stored.
	v := ipSave(t, w, ihAt(10, 0), 420, 1320, false)
	ipSwitch(t, "switched off at 10:00", v, false, nil)
	if v["instantOpenNow"] != true {
		t.Fatalf("switched off at 10:00: open now %v", v["instantOpenNow"])
	}
	if paused, until := ipStoredUntil(t, w); paused || !until.IsZero() {
		t.Fatalf("stored after switching off: paused %v until %v", paused, until)
	}
	if sv := ihInstantAt(t, w, ihAt(10, 0)); !sv.Instant || sv.InstantClosed {
		t.Fatalf("10:00 after switching off: %+v", sv)
	}

	// During the day: until tomorrow's opening, not until tonight's close.
	ipSwitch(t, "switched on at 10:30", ipSave(t, w, ihAt(10, 30), 420, 1320, true), true, ihUTC(ihAt(24+7, 0)))
	if sv := ihInstantAt(t, w, ihAt(12, 0)); sv.Instant || sv.InstantResumesLabel != "tomorrow at 7:00 AM" {
		t.Fatalf("noon, paused at 10:30: %+v", sv)
	}

	// The hours saved in the same Save decide the opening: 08:00-20:00.
	ipSave(t, w, ihAt(24+9, 0), 480, 1200, false)
	ipSwitch(t, "switched on at 21:00, opens 08:00", ipSave(t, w, ihAt(24+21, 0), 480, 1200, true), true, ihUTC(ihAt(48+8, 0)))
	if sv := ihInstantAt(t, w, ihAt(24+23, 0)); sv.InstantResumesLabel != "tomorrow at 8:00 AM" {
		t.Fatalf("23:00, opens 08:00: %+v", sv)
	}
	// New hours saved while paused do not move the end; the hours still gate
	// the lane after it, and the label names when instant really comes back.
	ipSwitch(t, "re-saved with 09:00 opening", ipSave(t, w, ihAt(24+23, 30), 540, 1200, true), true, ihUTC(ihAt(48+8, 0)))
	if sv := ihInstantAt(t, w, ihAt(24+23, 45)); sv.InstantResumesLabel != "tomorrow at 9:00 AM" || sv.InstantResumesAt != ihUTC(ihAt(48+9, 0)) {
		t.Fatalf("23:45, paused until 08:00, opens 09:00: %+v", sv)
	}
	if sv := ihInstantAt(t, w, ihAt(48+8, 30)); sv.Instant || sv.InstantResumesLabel != "today at 9:00 AM" {
		t.Fatalf("08:30, the pause has ended, the hours still shut it: %+v", sv)
	}
	if sv := ihInstantAt(t, w, ihAt(48+9, 0)); !sv.Instant {
		t.Fatalf("09:00: %+v", sv)
	}

	// After it has ended, the stored switch is still on but shows off; a Save
	// that turns it on again is a new switch-on, with a new end.
	if code, v := ipZoneCall(t, w, ihAt(48+10, 0), nil); code != http.StatusOK || v["instantPaused"] != false {
		t.Fatalf("GET after the pause ended: %d %v", code, v["instantPaused"])
	}
	ipSwitch(t, "switched on again at 10:00", ipSave(t, w, ihAt(48+10, 0), 540, 1200, true), true, ihUTC(ihAt(72+9, 0)))
}

// ── a pause stored before this change holds until switched off ─────────────

func TestInstantPauseWithNoEndKeepsItsOldMeaning(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	store := w.storeID.Hex()
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 420, InstantCloseMin: 1320, InstantPaused: true})

	for _, at := range []time.Time{ihAt(24+7, 0), ihAt(48+12, 0)} {
		if sv := ihInstantAt(t, w, at); sv.Instant || sv.InstantResumesLabel != pausedResumesLabel || sv.InstantResumesAt != "" {
			t.Fatalf("a pause with no end at %s: %+v", at.In(istZone).Format("01-02 15:04"), sv)
		}
	}
	code, v := ipZoneCall(t, w, ihAt(48+12, 0), nil)
	if code != http.StatusOK {
		t.Fatalf("GET: %d %v", code, v)
	}
	ipSwitch(t, "GET, a pause with no end", v, true, nil)
	ihClock(w, ihAt(48+12, 0))
	if code, e := ihOp(t, w, w.mgr, store, "extend", 30); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_PAUSED" {
		t.Fatalf("extend, a pause with no end: %d %v", code, e)
	}

	// The console's re-Save keeps it as it is: no end is invented for it.
	ipSwitch(t, "re-saved", ipSave(t, w, ihAt(48+12, 30), 420, 1320, true), true, nil)
	if paused, until := ipStoredUntil(t, w); !paused || !until.IsZero() {
		t.Fatalf("stored after a re-save: paused %v until %v", paused, until)
	}
	if sv := ihInstantAt(t, w, ihAt(72+8, 0)); sv.Instant || sv.InstantResumesLabel != pausedResumesLabel {
		t.Fatalf("the morning after the re-save: %+v", sv)
	}
	// Switching it off ends it.
	ipSave(t, w, ihAt(72+9, 0), 420, 1320, false)
	if sv := ihInstantAt(t, w, ihAt(72+9, 0)); !sv.Instant {
		t.Fatalf("switched off: %+v", sv)
	}
}

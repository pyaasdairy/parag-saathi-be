package consumer

// INSTANT HOURS AS THE CONSOLE SHOWS THEM (founder, 24 Sep): a zone whose
// instant hours were never saved (instant_close_min 0) used to be open round
// the clock, while the Saathi Zone tab showed it closing at 10 PM, so the app
// offered "20 min" at 11 PM. Unsaved hours now mean the 07:00-22:00 IST the
// console shows, everywhere instant openness is decided: GET /serviceability,
// the order guard in createOrder, and the store console.
//
// Every test here runs on a fixed IST clock (ihAt), injected into the service,
// so no verdict depends on when the suite runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ihDay is the IST day every instant-hours test runs on.
const ihDay = "2026-09-24"

// ihAt is hh:mm IST on ihDay (day offsets: ihAt(24+2, 0) is 02:00 the next day).
func ihAt(hour, min int) time.Time { return istDayAt(ihDay, hour, min) }

// ihClock pins the service clock (the handlers read it through s.now()).
func ihClock(w *chainWorld, at time.Time) { w.svc.clock = func() time.Time { return at } }

// ihZone stores a zone for the chain world's store exactly as given: unlike
// guardZone it never fills in hours, so InstantCloseMin 0 is a zone whose
// hours were never saved.
func ihZone(t *testing.T, w *chainWorld, z zone) *zone {
	t.Helper()
	z.StoreID, z.Active = w.storeID.Hex(), true
	z.Center = newGeoPoint(guardCenter.Lat, guardCenter.Lng)
	out, err := w.svc.repo.upsertZone(context.Background(), &z)
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	return out
}

// ihPost places a one-item order through the real POST /orders handler, on the
// service clock.
func ihPost(t *testing.T, w *chainWorld, cid primitive.ObjectID, lane string, pt geoPt) (int, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"payment_method":"cod","lane":%q,"delivery_date":null,"order_type":%q,`+
		`"order_items":[{"id":"i1","product_id":"gold-500ml","name":"Milk","variant":"500ml","price":35,"qty":1}],`+
		`"consumer_name":"Hours Probe","phone":"9000008000","geo":{"lat":%v,"lng":%v},"address_label":"Probe","address_text":"Probe"}`,
		lane, lane, pt.Lat, pt.Lng)
	req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
	rec := httptest.NewRecorder()
	(&handler{svc: w.svc}).createOrder(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ── 1) unsaved hours close at 22:00, on every surface ──────────────────────

func TestInstantHoursUnsavedZoneClosesAtTenPM(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	z := ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000}) // hours never saved
	if z.InstantCloseMin != 0 {
		t.Fatalf("precondition: the stored zone has no saved hours, got close %d", z.InstantCloseMin)
	}
	ctx := context.Background()
	p1 := pointAtBearing(guardCenter, 1000, 90)

	// /serviceability
	sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", ihAt(22, 1))
	if err != nil || sv.Instant || !sv.InstantClosed || sv.InstantResumesLabel != "tomorrow at 7:00 AM" {
		t.Fatalf("22:01 unsaved hours: want instant closed, resumes tomorrow 7AM; got %+v %v", sv, err)
	}
	if sv.InstantResumesAt != istDayAt("2026-09-25", 7, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("resumesAt %q", sv.InstantResumesAt)
	}
	sv, err = w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", ihAt(21, 59))
	if err != nil || !sv.Instant || sv.InstantClosed {
		t.Fatalf("21:59 unsaved hours: want instant open; got %+v %v", sv, err)
	}

	// POST /orders, through the handler, on the service clock.
	cid := w.customer(t, "9000008101", 0)
	ihClock(w, ihAt(22, 1))
	code, out := ihPost(t, w, cid, "instant", p1)
	if code != http.StatusUnprocessableEntity || out["code"] != "INSTANT_CLOSED" {
		t.Fatalf("instant order at 22:01: %d %v, want 422 INSTANT_CLOSED", code, out)
	}
	// The morning lane is never touched by the instant hours.
	if code, out = ihPost(t, w, cid, "morning", p1); code != http.StatusCreated {
		t.Fatalf("morning order at 22:01: %d %v, want 201", code, out)
	}
	ihClock(w, ihAt(21, 59))
	if code, out = ihPost(t, w, cid, "instant", p1); code != http.StatusCreated {
		t.Fatalf("instant order at 21:59: %d %v, want 201", code, out)
	}

	// The console shows exactly the hours that are enforced.
	v := zoneView(z, w.storeID.Hex())
	if v["instantOpenMin"] != 420 || v["instantCloseMin"] != 1320 || v["instant_open_min"] != 420 || v["instant_close_min"] != 1320 {
		t.Fatalf("console hours: %v-%v / %v-%v", v["instantOpenMin"], v["instantCloseMin"], v["instant_open_min"], v["instant_close_min"])
	}
}

// ── 2) saved hours are enforced exactly as before ──────────────────────────

func TestInstantHoursSavedHoursUnchanged(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ctx := context.Background()
	p1 := pointAtBearing(guardCenter, 1000, 180)

	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 480, InstantCloseMin: 1200})
	for _, c := range []struct {
		at      time.Time
		instant bool
	}{{ihAt(7, 59), false}, {ihAt(8, 0), true}, {ihAt(19, 59), true}, {ihAt(20, 1), false}, {ihAt(22, 30), false}} {
		sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", c.at)
		if err != nil || sv.Instant != c.instant || sv.InstantClosed == c.instant {
			t.Fatalf("saved 08:00-20:00 at %s: %+v %v, want instant=%v", c.at.In(istZone).Format("15:04"), sv, err, c.instant)
		}
	}
	// Round the clock is a saved 00:00-24:00.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 0, InstantCloseMin: 1440})
	for _, at := range []time.Time{ihAt(3, 0), ihAt(22, 1), ihAt(23, 59)} {
		if sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", at); err != nil || !sv.Instant || sv.InstantClosed {
			t.Fatalf("saved 00:00-24:00 at %s: %+v %v", at.In(istZone).Format("15:04"), sv, err)
		}
	}
}

// ── 3) INSTANT_TEST_OPEN keeps exactly the interaction it had ──────────────
//
// With a zone drawn, the flag widens instant to wherever the point is
// serviceable and the store's hours still gate that widened lane. (With no
// zone drawn it now follows the console's 07:00-22:00 as well: test 6.)

func TestInstantHoursKeepTheInstantTestOpenInteraction(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	p5 := pointAtBearing(guardCenter, 5000, 0) // standard circle only
	t.Setenv("INSTANT_TEST_OPEN", "true")

	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000}) // hours never saved
	sv, err := w.svc.serviceabilityAt(ctx, p5.Lat, p5.Lng, "", ihAt(21, 59))
	if err != nil || !sv.Instant || sv.InstantClosed {
		t.Fatalf("flag on, 5 km, 21:59: the flag widens instant; got %+v %v", sv, err)
	}
	sv, err = w.svc.serviceabilityAt(ctx, p5.Lat, p5.Lng, "", ihAt(22, 1))
	if err != nil || sv.Instant || !sv.InstantClosed {
		t.Fatalf("flag on, 5 km, 22:01: the hours gate the widened lane; got %+v %v", sv, err)
	}
	// Flag off: 5 km is standard-only whatever the hour, never "closed".
	t.Setenv("INSTANT_TEST_OPEN", "")
	sv, err = w.svc.serviceabilityAt(ctx, p5.Lat, p5.Lng, "", ihAt(22, 1))
	if err != nil || sv.Instant || sv.InstantClosed {
		t.Fatalf("flag off, 5 km, 22:01: %+v %v", sv, err)
	}
}

// ── 5) the console's "Closes 12:00 AM" is midnight, not "never saved" ──────
//
// The Zone tab's time picker only produces 0..1439, so a midnight close is
// sent as instant_close_min 0. That was stored as 0 and read as "hours never
// saved": the manager's 09:00-midnight became 07:00-22:00 and the form
// reloaded showing 7:00 AM-10:00 PM; all day (00:00-00:00) could not be
// saved from the console at all.

func TestInstantHoursMidnightCloseFromTheConsole(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ctx := context.Background()
	store := w.storeID.Hex()
	p1 := pointAtBearing(guardCenter, 1000, 45)
	// save sends the console's PUT body (snake_case, km radii, both hours) and
	// answers the view the form re-hydrates from.
	save := func(open, close int) map[string]any {
		t.Helper()
		in := zoneInput{Center: &geoPt{Lat: guardCenter.Lat, Lng: guardCenter.Lng}, StandardRadiusKm: 8, InstantRadiusKm: 2.5,
			InstantOpenMinS: &open, InstantCloseMinS: &close}
		z, err := w.svc.upsertZone(ctx, w.mgr, store, in)
		if err != nil {
			t.Fatalf("save %d-%d: %v", open, close, err)
		}
		return zoneViewAt(z, store, ihAt(23, 50))
	}
	stored := func(open, close int) {
		t.Helper()
		if z := ihStoredZone(t, w, store); z.InstantOpenMin != open || z.InstantCloseMin != close {
			t.Fatalf("stored %d-%d, want %d-%d", z.InstantOpenMin, z.InstantCloseMin, open, close)
		}
	}
	echo := func(v map[string]any, open, close int) {
		t.Helper()
		if v["instantOpenMin"] != open || v["instantCloseMin"] != close || v["instant_open_min"] != open || v["instant_close_min"] != close {
			t.Fatalf("console echo %v-%v / %v-%v, want %d-%d", v["instantOpenMin"], v["instantCloseMin"], v["instant_open_min"], v["instant_close_min"], open, close)
		}
	}
	instant := func(at time.Time, want bool) {
		t.Helper()
		sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", at)
		if err != nil || sv.Instant != want || sv.InstantClosed == want {
			t.Fatalf("at %s: %+v %v, want instant=%v", at.In(istZone).Format("01-02 15:04"), sv, err, want)
		}
	}

	// Opens 9:00 AM, closes 12:00 AM.
	v := save(540, 0)
	stored(540, 1440)
	echo(v, 540, 0) // the form shows 9:00 AM - 12:00 AM and sends 0 back
	if v["instantClosesAt"] != ihUTC(ihAt(24, 0)) {
		t.Fatalf("closes at %v, want midnight", v["instantClosesAt"])
	}
	for _, c := range []struct {
		at   time.Time
		want bool
	}{{ihAt(8, 59), false}, {ihAt(9, 0), true}, {ihAt(23, 0), true}, {ihAt(23, 59), true}, {ihAt(24, 0), false}, {ihAt(24+8, 0), false}} {
		instant(c.at, c.want)
	}
	// Saving the form again as it shows is stable.
	echo(save(v["instantOpenMin"].(int), v["instantCloseMin"].(int)), 540, 0)
	stored(540, 1440)

	// Opens 12:00 AM, closes 12:00 AM: all day.
	v = save(0, 0)
	stored(0, 1440)
	echo(v, 0, 0)
	if v["instantClosesAt"] != nil || v["instantOpenNow"] != true {
		t.Fatalf("all day: open %v closes %v", v["instantOpenNow"], v["instantClosesAt"])
	}
	for _, at := range []time.Time{ihAt(3, 0), ihAt(22, 30), ihAt(23, 59), ihAt(24, 0)} {
		instant(at, true)
	}

	// An API client's 1440 is still midnight; a zone never saved keeps 07:00-22:00.
	save(540, 1440)
	stored(540, 1440)
	never := ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	if never.InstantCloseMin != 0 {
		t.Fatalf("precondition: never saved, got close %d", never.InstantCloseMin)
	}
	echo(zoneViewAt(never, store, ihAt(12, 0)), 420, 1320)
	instant(ihAt(22, 30), false)
}

// ── 4) the paused label tells the truth ────────────────────────────────────
//
// A pause with no end (stored before pauses carried one) holds until the
// manager switches it off; it does not end at the next opening time.
// /serviceability used to tell the shopper "Instant resumes tomorrow at 7:00
// AM" for such a store, which stayed shut at 7:00. (A pause switched on
// since ends at the next opening time and the label names it:
// instant_pause_test.go.)

func TestInstantHoursPausedLabelNamesNoTime(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantPaused: true})
	ctx := context.Background()
	p1 := pointAtBearing(guardCenter, 1000, 270)
	for _, at := range []time.Time{ihAt(10, 0), ihAt(23, 0), ihAt(24+7, 0), ihAt(24+7, 5)} {
		sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", at)
		if err != nil || sv.Instant || !sv.InstantClosed || sv.InstantResumesLabel != "when the store turns it back on" || sv.InstantResumesAt != "" {
			t.Fatalf("paused at %s: %+v %v", at.In(istZone).Format("15:04"), sv, err)
		}
	}
	// Closed by the hours (not paused), the label still names the opening time.
	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	if sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", ihAt(23, 0)); err != nil || sv.InstantResumesLabel != "tomorrow at 7:00 AM" {
		t.Fatalf("closed by the hours at 23:00: %+v %v", sv, err)
	}
}

// ── 6) no zone drawn, INSTANT_TEST_OPEN on: the console's 07:00-22:00 ──────
//
// ICR-02. Before: with the flag on and no zone drawn, /serviceability offered
// instant round the clock (the app promised "20 min" at 11 PM and the order
// guard took it), while the Zone tab shows such a store 07:00-22:00 (the
// defaults zoneViewAt(nil) sends). The no-zone lane now follows those hours
// on /serviceability and in the order guard. Flag off: nothing changes. A
// store with no zone still gets no closing alert and has nothing to extend or
// close: those controls come with the zone.

func TestInstantHoursNoZoneFlagLaneFollowsTheConsoleHours(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	if err := w.svc.repo.ensureInstantAlertIndexes(context.Background()); err != nil {
		t.Fatalf("alert index: %v", err)
	}
	ctx := context.Background()
	store := w.storeID.Hex()
	p1 := pointAtBearing(guardCenter, 1000, 0) // inside the 5 km no-zone fence
	p6 := pointAtBearing(guardCenter, 6000, 0) // outside it
	seven := ihUTC(ihAt(24+7, 0))

	// The console shows a store with no zone 07:00-22:00; that is what is enforced.
	nv := zoneViewAt(nil, store, ihAt(12, 0))
	if nv["instantOpenMin"] != effOpenMin(&zone{}) || nv["instantCloseMin"] != effCloseMin(&zone{}) ||
		nv["instant_open_min"] != 420 || nv["instant_close_min"] != 1320 {
		t.Fatalf("no-zone console hours: %v-%v", nv["instantOpenMin"], nv["instantCloseMin"])
	}

	t.Setenv("INSTANT_TEST_OPEN", "true")
	for _, c := range []struct {
		at    time.Time
		open  bool
		label string
		when  string
	}{
		{ihAt(6, 59), false, "today at 7:00 AM", ihUTC(ihAt(7, 0))},
		{ihAt(7, 0), true, "", ""},
		{ihAt(21, 59), true, "", ""},
		{ihAt(22, 0), false, "tomorrow at 7:00 AM", seven},
		{ihAt(23, 30), false, "tomorrow at 7:00 AM", seven},
	} {
		sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", c.at)
		if err != nil || !sv.Serviceable || !sv.DefaultOpen || sv.Instant != c.open || sv.InstantClosed == c.open ||
			sv.InstantResumesLabel != c.label || sv.InstantResumesAt != c.when {
			t.Fatalf("no zone, flag on, %s: %+v %v, want instant=%v %q %q", c.at.In(istZone).Format("15:04"), sv, err, c.open, c.label, c.when)
		}
	}
	// Outside the fence it stays out of area, on both lanes.
	if sv, err := w.svc.serviceabilityAt(ctx, p6.Lat, p6.Lng, "", ihAt(10, 0)); err != nil || sv.Serviceable || sv.Instant || sv.InstantClosed {
		t.Fatalf("no zone, flag on, 6 km: %+v %v", sv, err)
	}

	// The order guard gives the same answer at the order's own moment.
	cid := w.customer(t, "9000008601", 0)
	ihClock(w, ihAt(23, 30))
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusUnprocessableEntity || out["code"] != "INSTANT_CLOSED" {
		t.Fatalf("no zone, flag on, instant order at 23:30: %d %v, want 422 INSTANT_CLOSED", code, out)
	}
	if code, out := ihPost(t, w, cid, "morning", p1); code != http.StatusCreated {
		t.Fatalf("no zone, flag on, morning order at 23:30: %d %v, want 201", code, out)
	}
	ihClock(w, ihAt(24+10, 0))
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusCreated {
		t.Fatalf("no zone, flag on, instant order at 10:00: %d %v, want 201", code, out)
	}
	ihClock(w, ihAt(24+10, 0))
	if code, out := ihPost(t, w, cid, "instant", p6); code != http.StatusUnprocessableEntity || out["code"] != "NOT_SERVICEABLE" {
		t.Fatalf("no zone, flag on, instant 6 km out: %d %v", code, out)
	}

	// No zone, no controls: no closing alert, nothing to extend or close.
	for _, at := range []time.Time{ihAt(24+21, 45), ihAt(24+22, 0)} {
		w.svc.instantAlertsTick(ctx, at)
	}
	if n := len(ihNotes(t, w, w.mgr.PartyID)); n != 0 {
		t.Fatalf("no zone, flag on: %d alerts, want none", n)
	}
	ihClock(w, ihAt(24+21, 50))
	for _, op := range []string{"extend", "close-now"} {
		if code, e := ihOp(t, w, w.mgr, store, op, 60); code != http.StatusUnprocessableEntity || e["code"] != "INSTANT_NOT_CONFIGURED" {
			t.Fatalf("no zone, flag on, %s: %d %v", op, code, e)
		}
	}
	// The console's no-zone view is unchanged.
	if v := zoneViewAt(nil, store, ihAt(24+10, 0)); v["instantOpenNow"] != false || v["instantClosesAt"] != nil || v["configured"] != false {
		t.Fatalf("no-zone view: %v", v)
	}

	// No store has coordinates (the pilot kept open): the same hours.
	if _, err := w.db.Collection("org_units").UpdateOne(ctx, bson.D{{Key: "_id", Value: w.storeID}},
		bson.D{{Key: "$unset", Value: bson.D{{Key: "geo_lat", Value: ""}, {Key: "geo_lng", Value: ""}}}}); err != nil {
		t.Fatalf("unset store geo: %v", err)
	}
	if sv, err := w.svc.serviceabilityAt(ctx, p6.Lat, p6.Lng, "", ihAt(23, 30)); err != nil || !sv.DefaultOpen || sv.Instant || !sv.InstantClosed ||
		sv.InstantResumesLabel != "tomorrow at 7:00 AM" {
		t.Fatalf("no store geo, flag on, 23:30: %+v %v", sv, err)
	}
	if sv, err := w.svc.serviceabilityAt(ctx, p6.Lat, p6.Lng, "", ihAt(10, 0)); err != nil || !sv.Instant || sv.InstantClosed {
		t.Fatalf("no store geo, flag on, 10:00: %+v %v", sv, err)
	}

	// Flag off: exactly as before. No instant is offered, nothing is "closed",
	// and the default-open answer lets an instant order through at any hour.
	t.Setenv("INSTANT_TEST_OPEN", "")
	for _, at := range []time.Time{ihAt(10, 0), ihAt(23, 30)} {
		if sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", at); err != nil || !sv.DefaultOpen || sv.Instant || sv.InstantClosed || sv.InstantResumesLabel != "" {
			t.Fatalf("no zone, flag off, %s: %+v %v", at.In(istZone).Format("15:04"), sv, err)
		}
	}
	ihClock(w, ihAt(48+23, 30))
	if code, out := ihPost(t, w, cid, "instant", p1); code != http.StatusCreated {
		t.Fatalf("no zone, flag off, instant order at 23:30: %d %v, want 201 (unchanged)", code, out)
	}
}

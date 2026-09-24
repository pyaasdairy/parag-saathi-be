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
// Today: with a zone drawn, the flag widens instant to wherever the point is
// serviceable and the store's hours still gate that widened lane; with no
// zone drawn, the flag offers instant with no hours at all.

func TestInstantHoursKeepTheInstantTestOpenInteraction(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	p5 := pointAtBearing(guardCenter, 5000, 0) // standard circle only

	// No zone drawn: the flag's instant is not hours-gated (unchanged).
	t.Setenv("INSTANT_TEST_OPEN", "true")
	p1 := pointAtBearing(guardCenter, 1000, 0)
	sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", ihAt(23, 30))
	if err != nil || !sv.DefaultOpen || !sv.Instant || sv.InstantClosed {
		t.Fatalf("no zone, flag on, 23:30: %+v %v", sv, err)
	}

	ihZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000}) // hours never saved
	sv, err = w.svc.serviceabilityAt(ctx, p5.Lat, p5.Lng, "", ihAt(21, 59))
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

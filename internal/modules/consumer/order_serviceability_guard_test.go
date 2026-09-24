package consumer

// THE ORDER GUARD: POST /orders refuses exactly what GET /serviceability
// refuses, from the same decision, at the order's own delivery point.
//
// The E2E found the server taking orders it had just told the app it could not
// serve: an instant order 5 km out when the instant circle is 2.5 km, and a
// Delhi pin 426 km from the only (Lucknow) store, on both lanes. Each became a
// task at the Lucknow store; the Delhi one sat OFFERED with a 20-minute ETA
// outside every rider's 15 km offer pool, stuck for good.
//
// Every zone here is drawn explicitly, with no instant hours (instant_close_min
// 0 = open round the clock) and not paused, so no verdict depends on the time
// the suite runs. Orders go through createOrderAt on a fixed 09:00 IST clock.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The full-chain store's own coordinate: the zone is drawn around it, the way
// a store manager draws it in the console.
var guardCenter = geoPt{Lat: 26.7700, Lng: 81.0100}

// Delhi, 426 km from the store: the E2E probe's out-of-area pin.
var guardDelhi = geoPt{Lat: 28.6139, Lng: 77.2090}

const guardInstantMsg = "Instant delivery doesn't reach this address yet. Choose a morning delivery instead."

// guardZone draws the store's zone: an instant circle inside a standard one.
func guardZone(t *testing.T, w *chainWorld, z zone) {
	t.Helper()
	z.StoreID, z.Active = w.storeID.Hex(), true
	z.Center = newGeoPoint(guardCenter.Lat, guardCenter.Lng)
	if _, err := w.svc.repo.upsertZone(context.Background(), &z); err != nil {
		t.Fatalf("zone: %v", err)
	}
}

// guardClock is 09:00 IST today: before the noon cut-off, so an undated
// morning order is for tomorrow and never trips CUTOFF_PASSED.
func guardClock() time.Time { return istDayAt(istToday(time.Now()), 9, 0) }

// guardAddress saves an address for the customer and returns its id (hex). A
// nil point saves one with no coordinates.
func guardAddress(t *testing.T, w *chainWorld, cid primitive.ObjectID, pt *geoPt, pincode string) string {
	t.Helper()
	a := &address{
		ID: primitive.NewObjectID(), ConsumerID: cid, Label: "Probe",
		Line1: "Probe St", City: "X", Pincode: pincode, CreatedAt: time.Now().UTC(),
	}
	if pt != nil {
		lat, lng := pt.Lat, pt.Lng
		a.Lat, a.Lng = &lat, &lng
	}
	if _, err := w.db.Collection(collAddresses).InsertOne(context.Background(), a); err != nil {
		t.Fatalf("address: %v", err)
	}
	return a.ID.Hex()
}

// guardOrder places a one-item order the way the app does: its own pin (geo)
// and/or the saved address it names by id. Either may be absent.
func guardOrder(w *chainWorld, cid primitive.ObjectID, lane string, pin *geoPt, addressID string) (*order, error) {
	in := orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "cod", AddressLabel: "Home", AddressText: "Guard St 1, Lucknow",
		AddressID: addressID, Lane: lane, ConsumerName: "Guard Tester", Phone: "9000007000",
	}
	if pin != nil {
		in.Geo = &geoPoint{Lat: pin.Lat, Lng: pin.Lng}
	}
	return w.svc.createOrderAt(context.Background(), cid.Hex(), in, guardClock())
}

// guardRefused asserts a flat 422 with the given code.
func guardRefused(t *testing.T, what string, err error, code string) *apiError {
	t.Helper()
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("%s: want 422 %s, got %v", what, code, err)
	}
	if ae.status != http.StatusUnprocessableEntity || ae.Code != code {
		t.Fatalf("%s: want 422 %s, got %d %s (%s)", what, code, ae.status, ae.Code, ae.Message)
	}
	return ae
}

// guardNothingFor asserts a refused order left no trace: no order row and no
// delivery task for this customer anywhere.
func guardNothingFor(t *testing.T, w *chainWorld, cid primitive.ObjectID) {
	t.Helper()
	ctx := context.Background()
	if n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{{Key: "user_id", Value: cid.Hex()}}); n != 0 {
		t.Fatalf("a refused order was stored: %d order rows", n)
	}
	if n, _ := w.db.Collection(collDeliveries).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid.Hex()}}); n != 0 {
		t.Fatalf("a refused order minted %d delivery task(s)", n)
	}
}

// guardVerdict is what /serviceability's answer means for an order on a lane,
// restated from the contract: out of area → NOT_SERVICEABLE; instant shut →
// INSTANT_CLOSED; instant not offered where a zone decides → INSTANT_OUT_OF_RANGE;
// anything else (including the no-zone default-open answer) → accepted ("").
func guardVerdict(sv *serviceabilityResult, lane string) string {
	switch {
	case !sv.Serviceable:
		return "NOT_SERVICEABLE"
	case lane != "instant":
		return ""
	case sv.InstantClosed:
		return "INSTANT_CLOSED"
	case !sv.Instant && !sv.DefaultOpen:
		return "INSTANT_OUT_OF_RANGE"
	}
	return ""
}

func guardCode(err error) string {
	if err == nil {
		return ""
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return "error: " + err.Error()
}

// ── 1) the E2E's first finding: instant beyond the instant circle ───────────

func TestOrderGuardRefusesInstantBeyondTheInstantRadius(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	ctx := context.Background()

	p5 := pointAtBearing(guardCenter, 5000, 0)
	sv, err := w.svc.serviceability(ctx, p5.Lat, p5.Lng, "")
	if err != nil || !sv.Serviceable || sv.Instant {
		t.Fatalf("precondition: 5 km out should be standard-only, got %+v %v", sv, err)
	}

	cid := w.customer(t, "9000007101", 0)
	_, err = guardOrder(w, cid, "instant", &p5, "")
	ae := guardRefused(t, "instant 5 km out", err, "INSTANT_OUT_OF_RANGE")
	if ae.Message != guardInstantMsg {
		t.Fatalf("message the app shows verbatim: %q want %q", ae.Message, guardInstantMsg)
	}
	guardNothingFor(t, w, cid)

	// The same point is inside the standard circle: the morning lane serves it.
	ord, err := guardOrder(w, cid, "morning", &p5, "")
	if err != nil {
		t.Fatalf("morning 5 km out must be accepted: %v", err)
	}
	if d := chainTaskFor(t, w, ord.OrderID); d.StoreID != w.storeID.Hex() || d.Status != "ASSIGNED" {
		t.Fatalf("morning task: store %s status %s", d.StoreID, d.Status)
	}
}

// ── 2) the E2E's second finding: a Delhi pin, both lanes ────────────────────

func TestOrderGuardRefusesAPinOutsideEveryStandardArea(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	ctx := context.Background()

	if sv, err := w.svc.serviceability(ctx, guardDelhi.Lat, guardDelhi.Lng, ""); err != nil || sv.Serviceable {
		t.Fatalf("precondition: Delhi should be unserviceable, got %+v %v", sv, err)
	}
	cid := w.customer(t, "9000007102", 0)
	for _, lane := range []string{"instant", "morning"} {
		_, err := guardOrder(w, cid, lane, &guardDelhi, "")
		ae := guardRefused(t, "Delhi pin, "+lane, err, "NOT_SERVICEABLE")
		if strings.TrimSpace(ae.Message) == "" {
			t.Fatalf("NOT_SERVICEABLE needs a message the app can show")
		}
	}
	// The app sends the pin AND the address id; an order that names only the
	// saved address is judged at that address's exact coordinates.
	delhiAddr := guardAddress(t, w, cid, &guardDelhi, "110001")
	for _, lane := range []string{"instant", "morning"} {
		_, err := guardOrder(w, cid, lane, &guardDelhi, delhiAddr)
		guardRefused(t, "Delhi pin + address, "+lane, err, "NOT_SERVICEABLE")
		_, err = guardOrder(w, cid, lane, nil, delhiAddr)
		guardRefused(t, "Delhi address only, "+lane, err, "NOT_SERVICEABLE")
	}
	guardNothingFor(t, w, cid)
}

// ── 3) inside both circles: accepted, exactly as before ─────────────────────

func TestOrderGuardAcceptsAPinInsideBothRadii(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})

	p1 := pointAtBearing(guardCenter, 1000, 90)
	cid := w.customer(t, "9000007103", 0)
	addr := guardAddress(t, w, cid, &p1, "226030")

	inst, err := guardOrder(w, cid, "instant", &p1, addr)
	if err != nil {
		t.Fatalf("instant inside both radii: %v", err)
	}
	if d := chainTaskFor(t, w, inst.OrderID); d.Status != "OFFERED" || d.EtaAt == "" || !d.GeoExact {
		t.Fatalf("instant task: status %s eta %q exact %v", d.Status, d.EtaAt, d.GeoExact)
	}
	morn, err := guardOrder(w, cid, "morning", &p1, addr)
	if err != nil {
		t.Fatalf("morning inside both radii: %v", err)
	}
	if d := chainTaskFor(t, w, morn.OrderID); d.Status != "ASSIGNED" {
		t.Fatalf("morning task status %s", d.Status)
	}
	// Named by id only, the address's own coordinates decide: still in.
	if _, err := guardOrder(w, cid, "instant", nil, addr); err != nil {
		t.Fatalf("instant by address id inside both radii: %v", err)
	}
}

// ── 4) no coordinates: the deployed app's label-only order keeps working ────

func TestOrderGuardFailsOpenWithoutCoordinates(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	ctx := context.Background()

	cid := w.customer(t, "9000007104", 0)
	// The customer's saved "Home" is in Delhi. A label-only order must NOT be
	// judged by guessing that address from its label: it carries no point.
	if _, err := w.db.Collection(collAddresses).UpdateMany(ctx, bson.D{{Key: "consumer_id", Value: cid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "lat", Value: guardDelhi.Lat}, {Key: "lng", Value: guardDelhi.Lng}}}}); err != nil {
		t.Fatalf("move home: %v", err)
	}
	for _, lane := range []string{"instant", "morning"} {
		if _, err := guardOrder(w, cid, lane, nil, ""); err != nil {
			t.Fatalf("label-only %s order must be accepted: %v", lane, err)
		}
	}
	// An address id whose row has no coordinates is no point either.
	bare := guardAddress(t, w, cid, nil, "226030")
	for _, lane := range []string{"instant", "morning"} {
		if _, err := guardOrder(w, cid, lane, nil, bare); err != nil {
			t.Fatalf("address without coordinates, %s: %v", lane, err)
		}
	}
	// Someone else's address id is ignored (scoped to the shopper), exactly as
	// the delivery task ignores it: no point, accepted.
	other := w.customer(t, "9000007105", 0)
	foreign := guardAddress(t, w, other, &guardDelhi, "110001")
	if _, err := guardOrder(w, cid, "morning", nil, foreign); err != nil {
		t.Fatalf("a foreign address id must not decide this order: %v", err)
	}
}

// ── 5) no zone configured: the same answer as /serviceability ───────────────

func TestOrderGuardWithNoZoneMatchesServiceability(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	// No zone: /serviceability applies the 5 km fence around the nearest store
	// with coordinates (defaultFenceKm) and marks what it serves defaultOpen.
	points := map[string]geoPt{
		"1 km (inside the fence)":  pointAtBearing(guardCenter, 1000, 180),
		"6 km (outside the fence)": pointAtBearing(guardCenter, 6000, 180),
		"Delhi":                    guardDelhi,
	}
	want := map[string]string{
		"1 km (inside the fence)":  "",
		"6 km (outside the fence)": "NOT_SERVICEABLE",
		"Delhi":                    "NOT_SERVICEABLE",
	}
	n := 0
	for _, flag := range []string{"", "true"} {
		t.Setenv("INSTANT_TEST_OPEN", flag)
		for name, pt := range points {
			pt := pt
			sv, err := w.svc.serviceability(ctx, pt.Lat, pt.Lng, "")
			if err != nil {
				t.Fatalf("serviceability %s: %v", name, err)
			}
			for _, lane := range []string{"instant", "morning"} {
				n++
				cid := w.customer(t, fmt.Sprintf("90000072%02d", n), 0)
				_, oerr := guardOrder(w, cid, lane, &pt, "")
				got := guardCode(oerr)
				if exp := guardVerdict(sv, lane); got != exp {
					t.Fatalf("INSTANT_TEST_OPEN=%q %s %s: order %q, /serviceability says %q (%+v)", flag, name, lane, got, exp, sv)
				}
				if got != want[name] {
					t.Fatalf("INSTANT_TEST_OPEN=%q %s %s: got %q want %q", flag, name, lane, got, want[name])
				}
			}
		}
	}
}

// ── 6) INSTANT_TEST_OPEN: honoured exactly as /serviceability honours it ────

func TestOrderGuardHonoursInstantTestOpen(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	ctx := context.Background()
	p5 := pointAtBearing(guardCenter, 5000, 270)

	n := 0
	for _, c := range []struct {
		flag     string
		instant5 string // the instant order 5 km out
	}{
		{"true", ""}, {"TRUE", ""}, {"True", ""}, // EqualFold, as /serviceability reads it
		{"", "INSTANT_OUT_OF_RANGE"}, {"false", "INSTANT_OUT_OF_RANGE"}, {"1", "INSTANT_OUT_OF_RANGE"},
	} {
		t.Setenv("INSTANT_TEST_OPEN", c.flag)
		sv, err := w.svc.serviceability(ctx, p5.Lat, p5.Lng, "")
		if err != nil {
			t.Fatalf("serviceability: %v", err)
		}
		if exp := guardVerdict(sv, "instant"); exp != c.instant5 {
			t.Fatalf("INSTANT_TEST_OPEN=%q: /serviceability verdict %q, test expects %q", c.flag, exp, c.instant5)
		}
		n++
		cid := w.customer(t, fmt.Sprintf("90000073%02d", n), 0)
		_, err = guardOrder(w, cid, "instant", &p5, "")
		if got := guardCode(err); got != c.instant5 {
			t.Fatalf("INSTANT_TEST_OPEN=%q instant 5 km out: got %q want %q", c.flag, got, c.instant5)
		}
		// Test-open widens instant to wherever the point is serviceable; it
		// never makes an out-of-area point serviceable.
		_, err = guardOrder(w, cid, "instant", &guardDelhi, "")
		guardRefused(t, "INSTANT_TEST_OPEN="+c.flag+" Delhi instant", err, "NOT_SERVICEABLE")
	}
}

// ── 7) no store has coordinates: /serviceability keeps the pilot open ───────

func TestOrderGuardFailsOpenWhenNoStoreHasGeo(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ctx := context.Background()
	if _, err := w.db.Collection("org_units").UpdateOne(ctx, bson.D{{Key: "_id", Value: w.storeID}},
		bson.D{{Key: "$unset", Value: bson.D{{Key: "geo_lat", Value: ""}, {Key: "geo_lng", Value: ""}}}}); err != nil {
		t.Fatalf("unset store geo: %v", err)
	}
	sv, err := w.svc.serviceability(ctx, guardDelhi.Lat, guardDelhi.Lng, "")
	if err != nil || !sv.Serviceable || !sv.DefaultOpen {
		t.Fatalf("precondition: no store geo → default open, got %+v %v", sv, err)
	}
	cid := w.customer(t, "9000007106", 0)
	for _, lane := range []string{"instant", "morning"} {
		ord, err := guardOrder(w, cid, lane, &guardDelhi, "")
		if err != nil {
			t.Fatalf("no store geo, %s: must be accepted, got %v", lane, err)
		}
		chainTaskFor(t, w, ord.OrderID) // routed as before
	}
}

// ── 8) a lookup error: never go dark ────────────────────────────────────────

func TestOrderGuardFailsOpenOnAZoneLookupError(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	ctx := context.Background()
	// An active zone row that cannot be decoded: listActiveZones fails, so
	// /serviceability answers with an error (the app fails open on it).
	if _, err := w.db.Collection(collStoreZones).InsertOne(ctx, bson.D{
		{Key: "store_id", Value: "broken"}, {Key: "active", Value: true},
		{Key: "center", Value: newGeoPoint(guardCenter.Lat, guardCenter.Lng)},
		{Key: "standard_radius_m", Value: "eight km"}, // a string where a number belongs
	}); err != nil {
		t.Fatalf("seed broken zone: %v", err)
	}
	if _, err := w.svc.serviceability(ctx, guardDelhi.Lat, guardDelhi.Lng, ""); err == nil {
		t.Fatalf("precondition: a broken zone row should make serviceability error")
	}
	cid := w.customer(t, "9000007107", 0)
	for _, lane := range []string{"instant", "morning"} {
		if _, err := guardOrder(w, cid, lane, &guardDelhi, ""); err != nil {
			t.Fatalf("zone lookup error, %s: must fail open, got %v", lane, err)
		}
	}
}

// ── 9) INSTANT_CLOSED stays exactly as it was ───────────────────────────────

func TestOrderGuardKeepsInstantClosed(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantPaused: true})

	p1 := pointAtBearing(guardCenter, 1000, 45)
	cid := w.customer(t, "9000007108", 0)
	_, err := guardOrder(w, cid, "instant", &p1, "")
	ae := guardRefused(t, "instant while paused", err, "INSTANT_CLOSED")
	if ae.Message != "instant delivery is closed right now; please choose the morning slot" {
		t.Fatalf("INSTANT_CLOSED message changed: %q", ae.Message)
	}
	if _, err := guardOrder(w, cid, "morning", &p1, ""); err != nil {
		t.Fatalf("morning while instant is paused: %v", err)
	}
}

// ── 10) parity across the zone's whole rulebook, pincodes included ──────────

func TestOrderGuardMatchesServiceabilityAcrossTheZoneRules(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	guardZone(t, w, zone{
		InstantRadiusM: 2500, StandardRadiusM: 8000,
		IncludePincodes: []string{"226099"}, ExcludePincodes: []string{"226013"},
		MonsoonEnabled: true, MonsoonRupees: 15,
	})
	points := []geoPt{
		pointAtBearing(guardCenter, 1000, 0),
		pointAtBearing(guardCenter, 5000, 0),
		pointAtBearing(guardCenter, 12000, 0),
		guardDelhi,
	}
	n := 0
	for _, flag := range []string{"", "true"} {
		t.Setenv("INSTANT_TEST_OPEN", flag)
		for i, pt := range points {
			pt := pt
			for _, pin := range []string{"", "226099", "226013"} {
				// The app checks serviceability with the saved address's pincode
				// beside its coordinates; the order names that address by id.
				sv, err := w.svc.serviceability(ctx, pt.Lat, pt.Lng, pin)
				if err != nil {
					t.Fatalf("serviceability: %v", err)
				}
				for _, lane := range []string{"instant", "morning"} {
					n++
					cid := w.customer(t, fmt.Sprintf("9000074%03d", n), 0)
					addr := guardAddress(t, w, cid, &pt, pin)
					ord, oerr := guardOrder(w, cid, lane, &pt, addr)
					got, exp := guardCode(oerr), guardVerdict(sv, lane)
					if got != exp {
						t.Fatalf("flag=%q point#%d pincode=%q %s: order %q, /serviceability says %q (%+v)", flag, i, pin, lane, got, exp, sv)
					}
					// The monsoon surcharge still comes from the same answer.
					if oerr == nil && lane == "instant" && ord.MonsoonFee != float64(sv.MonsoonRupees) {
						t.Fatalf("monsoon fee %v want %v", ord.MonsoonFee, sv.MonsoonRupees)
					}
				}
			}
		}
	}
}

// ── 11) the wire: a flat {code, message} 422 the app shows verbatim ────────

func TestOrderGuardHandlerAnswersAFlat422(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000})
	cid := w.customer(t, "9000007109", 0)
	h := &handler{svc: w.svc}

	post := func(lane string, pt geoPt) (int, map[string]any) {
		t.Helper()
		body := fmt.Sprintf(`{"payment_method":"cod","lane":%q,"delivery_date":null,"order_type":"instant",`+
			`"order_items":[{"id":"i1","product_id":"gold-500ml","name":"Milk","variant":"500ml","price":35,"qty":1}],`+
			`"consumer_name":"Probe","phone":"9000007109","geo":{"lat":%v,"lng":%v},"address_label":"Probe","address_text":"Probe"}`,
			lane, pt.Lat, pt.Lng)
		req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
		rec := httptest.NewRecorder()
		h.createOrder(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	flat := func(what string, code int, out map[string]any, want string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity || out["code"] != want {
			t.Fatalf("%s: %d %v want 422 %s", what, code, out, want)
		}
		if len(out) != 2 || out["message"] == nil || out["message"] == "" {
			t.Fatalf("%s: body must be exactly {code, message}: %v", what, out)
		}
	}

	code, out := post("instant", pointAtBearing(guardCenter, 5000, 0))
	flat("instant 5 km out", code, out, "INSTANT_OUT_OF_RANGE")
	if out["message"] != guardInstantMsg {
		t.Fatalf("instant message: %v", out["message"])
	}
	code, out = post("instant", guardDelhi)
	flat("Delhi instant", code, out, "NOT_SERVICEABLE")
	code, out = post("morning", guardDelhi)
	flat("Delhi morning", code, out, "NOT_SERVICEABLE")

	if code, out = post("instant", pointAtBearing(guardCenter, 1000, 0)); code != http.StatusCreated {
		t.Fatalf("inside both radii: %d %v want 201", code, out)
	}
}

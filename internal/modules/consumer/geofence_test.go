package consumer

import (
	"math"
	"testing"
)

// A Barabanki-ish anchor (the pilot's neighbourhood) so the numbers read as real
// coordinates, not abstract ones.
var zoneCenter = geoPt{Lat: 26.9250, Lng: 81.1850}

// makeZone builds a single active zone with a 5 km instant circle and a 15 km
// standard circle centred on zoneCenter.
func makeZone() zone {
	return zone{
		StoreID:         "store1",
		Active:          true,
		Center:          newGeoPoint(zoneCenter.Lat, zoneCenter.Lng),
		InstantRadiusM:  5000,
		StandardRadiusM: 15000,
	}
}

// pointAtBearing returns a point `meters` away from origin along a bearing
// (degrees). Used to place test points at a precise distance from the centre.
func pointAtBearing(origin geoPt, meters, bearingDeg float64) geoPt {
	const R = 6371000.0
	br := bearingDeg * math.Pi / 180
	lat1 := origin.Lat * math.Pi / 180
	lng1 := origin.Lng * math.Pi / 180
	dr := meters / R
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(dr) + math.Cos(lat1)*math.Sin(dr)*math.Cos(br))
	lng2 := lng1 + math.Atan2(math.Sin(br)*math.Sin(dr)*math.Cos(lat1), math.Cos(dr)-math.Sin(lat1)*math.Sin(lat2))
	return geoPt{Lat: lat2 * 180 / math.Pi, Lng: lng2 * 180 / math.Pi}
}

// ── haversineM ──────────────────────────────────────────────────────────────

func TestHaversineM(t *testing.T) {
	// Zero distance.
	if d := haversineM(zoneCenter, zoneCenter); d != 0 {
		t.Errorf("haversineM(self) = %v, want 0", d)
	}
	// A point placed exactly 5000 m away should read back ~5000 m (< 1 m error).
	p := pointAtBearing(zoneCenter, 5000, 42)
	if d := haversineM(zoneCenter, p); math.Abs(d-5000) > 1.0 {
		t.Errorf("haversineM(5km point) = %v, want ~5000", d)
	}
	// One degree of latitude is ~111.19 km near the equator.
	oneDeg := haversineM(geoPt{Lat: 0, Lng: 0}, geoPt{Lat: 1, Lng: 0})
	if math.Abs(oneDeg-111194) > 200 {
		t.Errorf("haversineM(1 deg lat) = %v, want ~111194", oneDeg)
	}
	// Symmetry.
	a, b := geoPt{Lat: 12.9, Lng: 77.6}, geoPt{Lat: 28.6, Lng: 77.2}
	if math.Abs(haversineM(a, b)-haversineM(b, a)) > 1e-6 {
		t.Errorf("haversineM not symmetric")
	}
}

// ── pointInPolygon ──────────────────────────────────────────────────────────

func TestPointInPolygon(t *testing.T) {
	// A unit square (lng 0..1, lat 0..1).
	square := []geoPt{{Lat: 0, Lng: 0}, {Lat: 0, Lng: 1}, {Lat: 1, Lng: 1}, {Lat: 1, Lng: 0}}

	cases := []struct {
		name string
		pt   geoPt
		want bool
	}{
		{"center inside", geoPt{Lat: 0.5, Lng: 0.5}, true},
		{"clearly outside east", geoPt{Lat: 0.5, Lng: 1.5}, false},
		{"clearly outside north", geoPt{Lat: 1.5, Lng: 0.5}, false},
		{"outside negative", geoPt{Lat: -0.5, Lng: 0.5}, false},
		{"near corner inside", geoPt{Lat: 0.01, Lng: 0.01}, true},
	}
	for _, c := range cases {
		if got := pointInPolygon(c.pt, square); got != c.want {
			t.Errorf("%s: pointInPolygon = %v, want %v", c.name, got, c.want)
		}
	}

	// A concave (arrow / chevron) polygon: a point in the notch must be OUTSIDE.
	chevron := []geoPt{
		{Lat: 0, Lng: 0}, {Lat: 2, Lng: 1}, {Lat: 0, Lng: 2}, {Lat: 1, Lng: 1},
	}
	if pointInPolygon(geoPt{Lat: 0.2, Lng: 1.0}, chevron) {
		t.Errorf("point in the concave notch should be OUTSIDE")
	}
	if !pointInPolygon(geoPt{Lat: 1.0, Lng: 0.6}, chevron) {
		t.Errorf("point in a solid arm should be INSIDE")
	}

	// Degenerate rings are never "inside".
	if pointInPolygon(geoPt{Lat: 0.5, Lng: 0.5}, []geoPt{{Lat: 0, Lng: 0}, {Lat: 1, Lng: 1}}) {
		t.Errorf("a 2-point ring can never contain a point")
	}
}

// ── Serviceability priority table (the contract, EXACTLY) ───────────────────

func TestServiceabilityPriorityTable(t *testing.T) {
	base := makeZone()

	// Points at precise distances from the centre.
	at := func(m float64) geoPt { return pointAtBearing(zoneCenter, m, 90) }

	t.Run("inside instant circle -> instant", func(t *testing.T) {
		lvl, _ := evalZone(base, at(2000), "")
		if lvl != svcInstant {
			t.Errorf("2km: level = %d, want instant(%d)", lvl, svcInstant)
		}
	})

	t.Run("outside instant, inside standard -> standard", func(t *testing.T) {
		lvl, _ := evalZone(base, at(9000), "")
		if lvl != svcStandard {
			t.Errorf("9km: level = %d, want standard(%d)", lvl, svcStandard)
		}
	})

	t.Run("outside both circles -> none", func(t *testing.T) {
		lvl, _ := evalZone(base, at(20000), "")
		if lvl != svcNone {
			t.Errorf("20km: level = %d, want none(%d)", lvl, svcNone)
		}
	})

	t.Run("include pincode OUTSIDE circle -> standard (allowed)", func(t *testing.T) {
		z := makeZone()
		z.IncludePincodes = []string{"225001"}
		lvl, _ := evalZone(z, at(20000), "225001")
		if lvl != svcStandard {
			t.Errorf("include pincode @20km: level = %d, want standard(%d)", lvl, svcStandard)
		}
		// A different pincode at the same far point is still out of area.
		if lvl2, _ := evalZone(z, at(20000), "999999"); lvl2 != svcNone {
			t.Errorf("non-include pincode @20km: level = %d, want none", lvl2)
		}
	})

	t.Run("instant beats include-pincode when inside instant circle", func(t *testing.T) {
		z := makeZone()
		z.IncludePincodes = []string{"225001"}
		lvl, _ := evalZone(z, at(2000), "225001")
		if lvl != svcInstant {
			t.Errorf("include pincode @2km: level = %d, want instant(%d) — richest wins", lvl, svcInstant)
		}
	})

	t.Run("exclude polygon HOLE inside circle -> denied (none)", func(t *testing.T) {
		z := makeZone()
		// A small square hole around a point 2 km east (well inside the instant circle).
		holeCtr := at(2000)
		d := 0.01 // ~1.1 km half-side, comfortably enclosing holeCtr
		z.ExcludePolygons = [][]geoPt{{
			{Lat: holeCtr.Lat - d, Lng: holeCtr.Lng - d},
			{Lat: holeCtr.Lat - d, Lng: holeCtr.Lng + d},
			{Lat: holeCtr.Lat + d, Lng: holeCtr.Lng + d},
			{Lat: holeCtr.Lat + d, Lng: holeCtr.Lng - d},
		}}
		if lvl, _ := evalZone(z, holeCtr, ""); lvl != svcNone {
			t.Errorf("point in the hole (inside instant circle): level = %d, want none(%d)", lvl, svcNone)
		}
		// A hole must also override an include pincode.
		z.IncludePincodes = []string{"225001"}
		if lvl, _ := evalZone(z, holeCtr, "225001"); lvl != svcNone {
			t.Errorf("hole must deny even an include pincode: level = %d, want none", lvl)
		}
		// Just outside the hole but still inside the instant circle stays instant.
		outside := geoPt{Lat: holeCtr.Lat, Lng: holeCtr.Lng + 3*d}
		if lvl, _ := evalZone(z, outside, ""); lvl != svcInstant {
			t.Errorf("just outside the hole: level = %d, want instant(%d)", lvl, svcInstant)
		}
	})

	t.Run("exclude pincode -> none", func(t *testing.T) {
		z := makeZone()
		z.ExcludePincodes = []string{"225002"}
		// Even inside the instant circle, a blacklisted pincode is denied.
		if lvl, _ := evalZone(z, at(2000), "225002"); lvl != svcNone {
			t.Errorf("exclude pincode @2km: level = %d, want none(%d)", lvl, svcNone)
		}
	})
}

// TestServiceabilityBoundary pins the 4.99 km (inside) vs 5.01 km (outside)
// instant boundary — the exact edge behaviour of the instant circle.
func TestServiceabilityBoundary(t *testing.T) {
	base := makeZone() // instant radius = 5000 m

	inside := pointAtBearing(zoneCenter, 4990, 90) // 4.99 km
	if lvl, _ := evalZone(base, inside, ""); lvl != svcInstant {
		t.Errorf("4.99km: level = %d, want instant(%d)", lvl, svcInstant)
	}

	outside := pointAtBearing(zoneCenter, 5010, 90) // 5.01 km
	if lvl, _ := evalZone(base, outside, ""); lvl != svcStandard {
		t.Errorf("5.01km: level = %d, want standard(%d) — instant boundary crossed", lvl, svcStandard)
	}

	// Exactly on the radius is inclusive (<=).
	onEdge := pointAtBearing(zoneCenter, 5000, 90)
	if lvl, _ := evalZone(base, onEdge, ""); lvl != svcInstant {
		t.Errorf("exactly 5.00km: level = %d, want instant(%d) — radius is inclusive", lvl, svcInstant)
	}
}

// TestRichestMatchAcrossZones — with two active zones, the richer level wins
// even when a nearer-but-standard zone is evaluated first.
func TestRichestMatchAcrossZones(t *testing.T) {
	pt := geoPt{Lat: 26.9250, Lng: 81.1850}

	// Zone A: pt is inside the standard circle only (centre offset so pt is ~9 km out).
	zA := zone{
		StoreID: "A", Active: true,
		Center:         newGeoPoint(pointAtBearing(pt, 9000, 0).Lat, pointAtBearing(pt, 9000, 0).Lng),
		InstantRadiusM: 5000, StandardRadiusM: 15000,
	}
	// Zone B: pt is well inside the instant circle (centre ~1 km away).
	zB := zone{
		StoreID: "B", Active: true,
		Center:         newGeoPoint(pointAtBearing(pt, 1000, 0).Lat, pointAtBearing(pt, 1000, 0).Lng),
		InstantRadiusM: 5000, StandardRadiusM: 15000,
	}

	lvlA, _ := evalZone(zA, pt, "")
	lvlB, _ := evalZone(zB, pt, "")
	if lvlA != svcStandard {
		t.Fatalf("zone A level = %d, want standard(%d)", lvlA, svcStandard)
	}
	if lvlB != svcInstant {
		t.Fatalf("zone B level = %d, want instant(%d)", lvlB, svcInstant)
	}

	// The richest-across-zones reducer (mirrors service.serviceability).
	best := svcNone
	for _, z := range []zone{zA, zB} {
		if lvl, _ := evalZone(z, pt, ""); lvl > best {
			best = lvl
		}
	}
	if best != svcInstant {
		t.Errorf("richest across zones = %d (%s), want instant", best, modeString(best))
	}
}

// ── Validation ──────────────────────────────────────────────────────────────

func TestValidateRadius(t *testing.T) {
	for _, m := range []float64{100, 5000, 60000} {
		if err := validateRadius(m); err != nil {
			t.Errorf("validateRadius(%v) = %v, want nil", m, err)
		}
	}
	for _, m := range []float64{0, 99, 99.9, 60000.1, 100000, -5} {
		if err := validateRadius(m); err == nil {
			t.Errorf("validateRadius(%v) = nil, want error", m)
		}
	}
}

func TestCoordsSane(t *testing.T) {
	good := []geoPt{{Lat: 26.9, Lng: 81.1}, {Lat: -33.8, Lng: 151.2}, {Lat: 90, Lng: 180}, {Lat: -90, Lng: -180}}
	for _, p := range good {
		if !coordsSane(p.Lat, p.Lng) {
			t.Errorf("coordsSane(%v) = false, want true", p)
		}
	}
	bad := []geoPt{{Lat: 0, Lng: 0}, {Lat: 91, Lng: 0}, {Lat: 0, Lng: 181}, {Lat: -91, Lng: 0}, {Lat: 0, Lng: -181}}
	for _, p := range bad {
		if coordsSane(p.Lat, p.Lng) {
			t.Errorf("coordsSane(%v) = true, want false", p)
		}
	}
	if coordsSane(math.NaN(), 10) || coordsSane(10, math.Inf(1)) {
		t.Errorf("coordsSane must reject NaN/Inf")
	}
}

package consumer

import "testing"

// Regression cover for the no-zone STORE-OrgUnit delivery fence (defaultFenceKm).
// These are pure geo checks — no Mongo — pinned to the seeded STORE-LKO-01 centre.

func TestGeoSane(t *testing.T) {
	cases := []struct {
		lat, lng float64
		want     bool
	}{
		{26.85, 81.00, true}, // STORE-LKO-01 (valid)
		{0, 0, false},        // Null Island — a store that never got a geo
		{91, 10, false},      // lat out of range
		{10, 181, false},     // lng out of range
		{-90, 180, true},     // valid extremes
	}
	for _, c := range cases {
		if got := geoSane(c.lat, c.lng); got != c.want {
			t.Errorf("geoSane(%v,%v)=%v want %v", c.lat, c.lng, got, c.want)
		}
	}
}

func TestDefaultFenceBoundary(t *testing.T) {
	store := geoPt{Lat: 26.85, Lng: 81.00} // STORE-LKO-01, Lucknow

	// ~1.1 km north — inside the 5 km fence → serviceable.
	if d := haversineKm(store, geoPt{Lat: 26.86, Lng: 81.00}); d > defaultFenceKm {
		t.Errorf("near point must be serviceable: %.2f km > %.0f km", d, defaultFenceKm)
	}
	// Hyderabad (~1100 km) — the exact case in the bug report → non-serviceable.
	if d := haversineKm(store, geoPt{Lat: 17.385, Lng: 78.4867}); d <= defaultFenceKm {
		t.Errorf("Hyderabad must be non-serviceable: %.2f km <= %.0f km", d, defaultFenceKm)
	}
	// ~11 km away, same city outskirts — still outside the 5 km fence.
	if d := haversineKm(store, geoPt{Lat: 26.95, Lng: 81.05}); d <= defaultFenceKm {
		t.Errorf("11 km point must be non-serviceable: %.2f km <= %.0f km", d, defaultFenceKm)
	}
}

package consumer

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// GEOFENCE SERVICEABILITY — is a shopper's delivery address inside a Parag
// Store's serving area, and at what service level (instant doorstep vs the
// standard/morning run)? A store manager (Saathi operator, STORE_MANAGER) draws
// the store's zone; the consumer app asks GET /consumer/serviceability before
// letting a shopper check out, and offers a waitlist join when the answer is no.
//
// A zone is a small, self-contained geo policy per store:
//
//   - an INSTANT circle (center + radius): inside → richest "instant" service;
//   - a STANDARD circle (larger radius): inside → "standard" service;
//   - INCLUDE pincodes: serviceable (standard) even OUTSIDE the circles — the
//     manual "we deliver to this colony too" override;
//   - EXCLUDE pincodes: never serviceable (a blacklisted pincode);
//   - EXCLUDE polygons (holes): a carved-out area that is DENIED even when it
//     sits INSIDE a serving circle (a river, a restricted campus, a dead zone).
//
// Priority (evaluated per zone, EXACTLY):
//  1. inside an exclude polygon  → NONE  (a hole denies even inside a circle)
//  2. pincode in exclude list    → NONE
//  3. inside the instant circle  → INSTANT (richest; beats an include-pincode)
//  4. pincode in include list    → STANDARD (allowed even outside the circle)
//  5. inside an include polygon  → STANDARD
//  6. inside the standard circle → STANDARD
//  7. otherwise                  → NONE
//
// Across zones (the pilot usually has one) the RICHEST match wins:
// instant beats standard beats none. With NO active zone configured the system
// defaults OPEN (standard) — the pilot must not be dark before a manager draws
// the first zone.
//
// All point tests run on-device in Go (haversineM + ray-casting pointInPolygon)
// so serviceability is a pure, offline-testable function of the zone docs; the
// 2dsphere index on `center` is for later server-side $near store routing.

const collStoreZones = "store_zones"
const collConsumerWaitlist = "consumer_waitlist"

// Service levels, ranked so the richest match wins (instant > standard > none).
const (
	svcNone     = 0
	svcStandard = 1
	svcInstant  = 2
)

// Radius bounds (metres): a zone smaller than 100 m is a mistake; larger than
// 60 km outgrows a single last-mile store.
const (
	minRadiusM = 100.0
	maxRadiusM = 60000.0
)

func modeString(level int) string {
	switch level {
	case svcInstant:
		return "instant"
	case svcStandard:
		return "standard"
	default:
		return "none"
	}
}

// ── Geo primitives (pure, offline, unit-tested) ─────────────────────────────

// haversineM — great-circle distance in METRES between two lat/lng points. The
// serviceability circle tests are all in metres (radii are stored in metres).
func haversineM(a, b geoPt) float64 {
	const R = 6371000.0 // mean Earth radius, metres
	dLat := (b.Lat - a.Lat) * math.Pi / 180
	dLng := (b.Lng - a.Lng) * math.Pi / 180
	la1 := a.Lat * math.Pi / 180
	la2 := b.Lat * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * R * math.Asin(math.Sqrt(h))
}

// pointInPolygon — ray-casting (even-odd) test: is pt inside the closed ring?
// The ring is an ordered list of vertices (lng=x, lat=y); the closing edge back
// to ring[0] is implied. Points on an edge are treated consistently by the
// standard half-open comparison; a ring of <3 vertices is never "inside".
//
// For the small, flat serving areas of a single store (a few km across) planar
// ray-casting on raw lat/lng is accurate enough — no projection is warranted.
func pointInPolygon(pt geoPt, ring []geoPt) bool {
	n := len(ring)
	if n < 3 {
		return false
	}
	inside := false
	x, y := pt.Lng, pt.Lat
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := ring[i].Lng, ring[i].Lat
		xj, yj := ring[j].Lng, ring[j].Lat
		if ((yi > y) != (yj > y)) && (x < (xj-xi)*(y-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

func coordsSane(lat, lng float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return false
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return false
	}
	// Null Island (0,0) is the classic "no fix" sentinel — reject it as insane.
	return !(lat == 0 && lng == 0)
}

func containsStr(list []string, v string) bool {
	if v == "" {
		return false
	}
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ── Zone model ──────────────────────────────────────────────────────────────

// geoJSONPoint stores the zone centre as GeoJSON ([lng, lat]) so a 2dsphere
// index can later drive server-side $near store routing. Go-side point tests
// read it back via centerPt().
type geoJSONPoint struct {
	Type        string     `bson:"type"        json:"type"`
	Coordinates [2]float64 `bson:"coordinates" json:"coordinates"` // [lng, lat]
}

func newGeoPoint(lat, lng float64) geoJSONPoint {
	return geoJSONPoint{Type: "Point", Coordinates: [2]float64{lng, lat}}
}

// zone is one store's serviceability policy (see the file header for the rules).
type zone struct {
	MongoID         primitive.ObjectID `bson:"_id,omitempty"           json:"-"`
	StoreID         string             `bson:"store_id"                json:"storeId"`
	Active          bool               `bson:"active"                  json:"active"`
	Center          geoJSONPoint       `bson:"center"                  json:"center"`
	InstantRadiusM  float64            `bson:"instant_radius_m"        json:"instantRadiusM"`
	StandardRadiusM float64            `bson:"standard_radius_m"       json:"standardRadiusM"`
	IncludePincodes []string           `bson:"include_pincodes,omitempty" json:"includePincodes,omitempty"`
	ExcludePincodes []string           `bson:"exclude_pincodes,omitempty" json:"excludePincodes,omitempty"`
	// Polygon rings ([]geoPt, lat/lng). Include = an allowed area beyond the
	// circle (standard); exclude = a hole denied even inside a circle.
	IncludePolygons [][]geoPt `bson:"include_polygons,omitempty" json:"includePolygons,omitempty"`
	ExcludePolygons [][]geoPt `bson:"exclude_polygons,omitempty" json:"excludePolygons,omitempty"`
	UpdatedBy       string    `bson:"updated_by,omitempty"    json:"updatedBy,omitempty"`
	UpdatedAt       time.Time `bson:"updated_at"              json:"updatedAt"`
	CreatedAt       time.Time `bson:"created_at"              json:"-"`
}

// centerPt reads the GeoJSON centre back as a lat/lng point for the Go tests.
func (z *zone) centerPt() geoPt {
	return geoPt{Lat: z.Center.Coordinates[1], Lng: z.Center.Coordinates[0]}
}

// evalZone applies the priority rules to one zone and returns the service level
// plus the point's distance (metres) to the zone centre. This is the whole
// contract, and it is pure — geofence_test.go pins every branch.
func evalZone(z zone, pt geoPt, pincode string) (int, float64) {
	dist := haversineM(pt, z.centerPt())

	// 1. A hole denies outright — even inside a serving circle.
	for _, ring := range z.ExcludePolygons {
		if pointInPolygon(pt, ring) {
			return svcNone, dist
		}
	}
	// 2. A blacklisted pincode is never serviceable.
	if containsStr(z.ExcludePincodes, pincode) {
		return svcNone, dist
	}
	// 3. Inside the instant circle is the richest match (beats an include pincode).
	if z.InstantRadiusM > 0 && dist <= z.InstantRadiusM {
		return svcInstant, dist
	}
	// 4. An include pincode is serviceable (standard) even outside the circles.
	if containsStr(z.IncludePincodes, pincode) {
		return svcStandard, dist
	}
	// 5. An include polygon is an allowed area beyond the circle (standard).
	for _, ring := range z.IncludePolygons {
		if pointInPolygon(pt, ring) {
			return svcStandard, dist
		}
	}
	// 6. Inside the standard circle.
	if z.StandardRadiusM > 0 && dist <= z.StandardRadiusM {
		return svcStandard, dist
	}
	// 7. Out of area.
	return svcNone, dist
}

// ── Serviceability response ─────────────────────────────────────────────────

type serviceabilityResult struct {
	Serviceable bool    `json:"serviceable"`
	Mode        string  `json:"mode"`  // "instant" | "standard" | "none"
	Instant     bool    `json:"instant"`
	StoreID     string  `json:"storeId,omitempty"`
	DistanceM   float64 `json:"distanceM,omitempty"`
	Pincode     string  `json:"pincode,omitempty"`
	DefaultOpen bool    `json:"defaultOpen,omitempty"` // true → no zone configured, open by default
}

// ── Repository ──────────────────────────────────────────────────────────────

// ensureGeoIndexes owns the zone collection's indexes: at most one zone per
// store (unique store_id), and a 2dsphere on the GeoJSON centre for future
// server-side $near routing.
func (r *repository) ensureGeoIndexes(ctx context.Context) error {
	_, err := r.storeZones.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "store_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "center", Value: "2dsphere"}}},
		{Keys: bson.D{{Key: "active", Value: 1}}},
	})
	if err != nil {
		return err
	}
	// Waitlist: one row per phone (upsert-by-phone).
	_, err = r.waitlist.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "phone", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return err
}

// getZone loads a store's zone (nil, nil when none is configured yet).
func (r *repository) getZone(ctx context.Context, storeID string) (*zone, error) {
	var z zone
	err := r.storeZones.FindOne(ctx, bson.D{{Key: "store_id", Value: storeID}}).Decode(&z)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("zone lookup failed")
	}
	return &z, nil
}

// upsertZone replaces a store's whole zone policy (keyed by store_id). created_at
// is stamped once on insert; the returned doc reflects the stored state.
func (r *repository) upsertZone(ctx context.Context, z *zone) (*zone, error) {
	now := time.Now().UTC()
	z.UpdatedAt = now
	set := bson.D{
		{Key: "active", Value: z.Active},
		{Key: "center", Value: z.Center},
		{Key: "instant_radius_m", Value: z.InstantRadiusM},
		{Key: "standard_radius_m", Value: z.StandardRadiusM},
		{Key: "include_pincodes", Value: z.IncludePincodes},
		{Key: "exclude_pincodes", Value: z.ExcludePincodes},
		{Key: "include_polygons", Value: z.IncludePolygons},
		{Key: "exclude_polygons", Value: z.ExcludePolygons},
		{Key: "updated_by", Value: z.UpdatedBy},
		{Key: "updated_at", Value: now},
	}
	after := options.After
	var out zone
	err := r.storeZones.FindOneAndUpdate(ctx,
		bson.D{{Key: "store_id", Value: z.StoreID}},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}},
		},
		options.FindOneAndUpdate().SetReturnDocument(after).SetUpsert(true),
	).Decode(&out)
	if err != nil {
		return nil, errInternal("zone save failed")
	}
	return &out, nil
}

// listActiveZones returns every active zone (pilot: usually one). Serviceability
// takes the richest match across them.
func (r *repository) listActiveZones(ctx context.Context) ([]zone, error) {
	cur, err := r.storeZones.Find(ctx, bson.D{{Key: "active", Value: true}})
	if err != nil {
		return nil, errInternal("zone list failed")
	}
	out := []zone{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("zone decode failed")
	}
	return out, nil
}

// upsertWaitlist records/refreshes a would-be shopper's interest, keyed by
// phone (idempotent): re-submitting bumps a hit counter, never duplicates.
func (r *repository) upsertWaitlist(ctx context.Context, w waitlistEntry) error {
	now := time.Now().UTC()
	set := bson.D{{Key: "updated_at", Value: now}}
	if w.Pincode != "" {
		set = append(set, bson.E{Key: "pincode", Value: w.Pincode})
	}
	if w.Name != "" {
		set = append(set, bson.E{Key: "name", Value: w.Name})
	}
	if w.Lat != nil && w.Lng != nil {
		set = append(set, bson.E{Key: "lat", Value: *w.Lat}, bson.E{Key: "lng", Value: *w.Lng})
	}
	_, err := r.waitlist.UpdateOne(ctx,
		bson.D{{Key: "phone", Value: w.Phone}},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$inc", Value: bson.D{{Key: "hits", Value: 1}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}},
		},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return errInternal("waitlist save failed")
	}
	return nil
}

// ── Service ─────────────────────────────────────────────────────────────────

// serviceability answers "can we deliver here, and how fast?" for a coordinate
// (+ optional pincode). Defaults OPEN when no active zone is configured.
func (s *service) serviceability(ctx context.Context, lat, lng float64, pincode string) (*serviceabilityResult, error) {
	if !coordsSane(lat, lng) {
		return nil, errBadRequest("lat/lng are out of range")
	}
	pincode = strings.TrimSpace(pincode)

	zones, err := s.repo.listActiveZones(ctx)
	if err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		// No zone drawn yet — the pilot must not go dark. Open, standard service.
		return &serviceabilityResult{
			Serviceable: true, Mode: modeString(svcStandard), Instant: false,
			DefaultOpen: true, Pincode: pincode,
		}, nil
	}

	pt := geoPt{Lat: lat, Lng: lng}
	best := svcNone
	var bestStore string
	var bestDist float64
	for _, z := range zones {
		lvl, dist := evalZone(z, pt, pincode)
		if lvl > best {
			best, bestStore, bestDist = lvl, z.StoreID, dist
		}
	}

	res := &serviceabilityResult{
		Serviceable: best > svcNone,
		Mode:        modeString(best),
		Instant:     best == svcInstant,
		Pincode:     pincode,
	}
	if best > svcNone {
		res.StoreID = bestStore
		res.DistanceM = math.Round(bestDist)
	}
	return res, nil
}

// storeZone returns the store manager's configured zone (guarded to the store
// the actor manages). A store with no zone yet is a 404 the UI treats as "draw
// your first zone".
func (s *service) storeZone(ctx context.Context, actor auth.Actor, storeID string) (*zone, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	z, err := s.repo.getZone(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if z == nil {
		return nil, errNotFound("no serviceability zone configured for this store")
	}
	return z, nil
}

// zoneInput is the store manager's PUT body. Center is {lat,lng}. Radii accept
// either metres (camelCase, contract canonical) OR kilometres (snake_case, what
// the Saathi store console sends) — normalized to metres in upsertZone. Pincode
// lists accept both camel and snake keys. The backend is the tolerant seam so
// the two clients never have to agree on units.
type zoneInput struct {
	Active           *bool     `json:"active"`
	Center           *geoPt    `json:"center"`
	InstantRadiusM   float64   `json:"instantRadiusM"`
	StandardRadiusM  float64   `json:"standardRadiusM"`
	InstantRadiusKm  float64   `json:"instant_radius_km"`
	StandardRadiusKm float64   `json:"standard_radius_km"`
	IncludePincodes  []string  `json:"includePincodes"`
	ExcludePincodes  []string  `json:"excludePincodes"`
	IncludePincodesS []string  `json:"include_pincodes"`
	ExcludePincodesS []string  `json:"exclude_pincodes"`
	IncludePolygons  [][]geoPt `json:"includePolygons"`
	ExcludePolygons  [][]geoPt `json:"excludePolygons"`
}

// normalize folds the km + snake aliases onto the canonical metres/camel fields.
func (in *zoneInput) normalize() {
	if in.StandardRadiusM == 0 && in.StandardRadiusKm > 0 {
		in.StandardRadiusM = in.StandardRadiusKm * 1000
	}
	if in.InstantRadiusM == 0 && in.InstantRadiusKm > 0 {
		in.InstantRadiusM = in.InstantRadiusKm * 1000
	}
	if len(in.IncludePincodes) == 0 && len(in.IncludePincodesS) > 0 {
		in.IncludePincodes = in.IncludePincodesS
	}
	if len(in.ExcludePincodes) == 0 && len(in.ExcludePincodesS) > 0 {
		in.ExcludePincodes = in.ExcludePincodesS
	}
}

func validateRadius(m float64) *apiError {
	if m < minRadiusM || m > maxRadiusM {
		return errUnprocessable("INVALID_RADIUS", "radius must be between 100 and 60000 metres")
	}
	return nil
}

func validateRings(rings [][]geoPt, what string) *apiError {
	for _, ring := range rings {
		if len(ring) < 3 {
			return errUnprocessable("INVALID_POLYGON", what+" polygon needs at least 3 points")
		}
		for _, p := range ring {
			if !coordsSane(p.Lat, p.Lng) {
				return errUnprocessable("INVALID_POLYGON", what+" polygon has an out-of-range point")
			}
		}
	}
	return nil
}

func cleanPincodes(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// upsertZone validates a manager's zone config and stores it (guarded to the
// store they manage). Radii are range-checked (100..60000 m); if both circles
// are set, the instant circle must sit inside the standard one.
func (s *service) upsertZone(ctx context.Context, actor auth.Actor, storeID string, in zoneInput) (*zone, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	in.normalize() // fold km + snake aliases onto metres/camel
	if in.Center == nil || !coordsSane(in.Center.Lat, in.Center.Lng) {
		return nil, errUnprocessable("INVALID_CENTER", "a sane center {lat,lng} is required")
	}
	// A zone must serve at least the standard circle.
	if err := validateRadius(in.StandardRadiusM); err != nil {
		return nil, err
	}
	if in.InstantRadiusM > 0 {
		if err := validateRadius(in.InstantRadiusM); err != nil {
			return nil, err
		}
		if in.InstantRadiusM > in.StandardRadiusM {
			return nil, errUnprocessable("INVALID_RADIUS", "the instant radius must not exceed the standard radius")
		}
	}
	if err := validateRings(in.ExcludePolygons, "exclude"); err != nil {
		return nil, err
	}
	if err := validateRings(in.IncludePolygons, "include"); err != nil {
		return nil, err
	}

	active := true
	if in.Active != nil {
		active = *in.Active
	}
	z := &zone{
		StoreID:         storeID,
		Active:          active,
		Center:          newGeoPoint(in.Center.Lat, in.Center.Lng),
		InstantRadiusM:  in.InstantRadiusM,
		StandardRadiusM: in.StandardRadiusM,
		IncludePincodes: cleanPincodes(in.IncludePincodes),
		ExcludePincodes: cleanPincodes(in.ExcludePincodes),
		IncludePolygons: in.IncludePolygons,
		ExcludePolygons: in.ExcludePolygons,
		UpdatedBy:       actor.PartyID,
	}
	return s.repo.upsertZone(ctx, z)
}

// ── Waitlist ────────────────────────────────────────────────────────────────

type waitlistEntry struct {
	Phone   string
	Pincode string
	Name    string
	Lat     *float64
	Lng     *float64
}

// joinWaitlist records interest from an out-of-area shopper (upsert by phone).
func (s *service) joinWaitlist(ctx context.Context, in waitlistEntry) error {
	phone := normalizePhone(in.Phone)
	if !phoneRe.MatchString(phone) {
		return errBadRequest("phone must be a 10-digit Indian mobile number")
	}
	in.Phone = phone
	in.Pincode = strings.TrimSpace(in.Pincode)
	in.Name = strings.TrimSpace(in.Name)
	if in.Lat != nil && in.Lng != nil && !coordsSane(*in.Lat, *in.Lng) {
		// A bad coordinate should not block a waitlist join — just drop it.
		in.Lat, in.Lng = nil, nil
	}
	return s.repo.upsertWaitlist(ctx, in)
}

// ── Handlers ────────────────────────────────────────────────────────────────

// serviceability — GET /consumer/serviceability?lat=&lng=&pincode=.
// Consumer-app-gated (same X-Parag-App-Key as /catalog), raw-JSON.
func (h *handler) serviceability(w http.ResponseWriter, r *http.Request) {
	if !h.svc.appKeyOK(r) {
		writeErr(w, errForbidden("serviceability is available from the PARAG app only"))
		return
	}
	q := r.URL.Query()
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(q.Get("lat")), 64)
	lng, err2 := strconv.ParseFloat(strings.TrimSpace(q.Get("lng")), 64)
	if err1 != nil || err2 != nil {
		writeErr(w, errBadRequest("lat and lng query params are required"))
		return
	}
	res, err := h.svc.serviceability(r.Context(), lat, lng, q.Get("pincode"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// joinWaitlist — POST /consumer/waitlist. Consumer-app-gated, raw-JSON. Records
// an out-of-area shopper's interest (upsert by phone).
func (h *handler) joinWaitlist(w http.ResponseWriter, r *http.Request) {
	if !h.svc.appKeyOK(r) {
		writeErr(w, errForbidden("the waitlist is available from the PARAG app only"))
		return
	}
	var body struct {
		Phone   string   `json:"phone"`
		Pincode string   `json:"pincode"`
		Name    string   `json:"name"`
		Lat     *float64 `json:"lat"`
		Lng     *float64 `json:"lng"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.joinWaitlist(r.Context(), waitlistEntry{
		Phone: body.Phone, Pincode: body.Pincode, Name: body.Name, Lat: body.Lat, Lng: body.Lng,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "waitlisted": true})
}

// getZone — GET /consumer/stores/{storeId}/zone (STORE_MANAGER). Operator wire
// format ({data} envelope), consumed by the Saathi store console.
func (h *handler) getZone(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	storeID := chi.URLParam(r, "storeId")
	z, err := h.svc.storeZone(r.Context(), actor, storeID)
	if err != nil {
		// No zone yet → return editable OPEN defaults (not a 404) so the store
		// console shows a blank editable form instead of an error state.
		if ae, ok := err.(*apiError); ok && ae.status == http.StatusNotFound {
			httpx.JSON(w, http.StatusOK, zoneView(nil, storeID))
			return
		}
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, zoneView(z, storeID))
}

// zoneView emits a zone in BOTH unit systems (metres/camel + km/snake) with a
// flat center, so the Saathi store console (km/snake) and any metres client read
// it identically. nil → open defaults for a store with no zone configured yet.
func zoneView(z *zone, storeID string) map[string]any {
	if z == nil {
		return map[string]any{
			"storeId": storeID, "active": false, "configured": false,
			"center":             nil,
			"standardRadiusM":    8000.0, "instantRadiusM": 2500.0,
			"standard_radius_km": 8.0, "instant_radius_km": 2.5,
			"includePincodes":    []string{}, "excludePincodes": []string{},
			"include_pincodes":   []string{}, "exclude_pincodes": []string{},
		}
	}
	c := z.centerPt()
	return map[string]any{
		"storeId": z.StoreID, "active": z.Active, "configured": true,
		"center":             map[string]float64{"lat": c.Lat, "lng": c.Lng},
		"standardRadiusM":    z.StandardRadiusM, "instantRadiusM": z.InstantRadiusM,
		"standard_radius_km": z.StandardRadiusM / 1000, "instant_radius_km": z.InstantRadiusM / 1000,
		"includePincodes":    z.IncludePincodes, "excludePincodes": z.ExcludePincodes,
		"include_pincodes":   z.IncludePincodes, "exclude_pincodes": z.ExcludePincodes,
		"includePolygons":    z.IncludePolygons, "excludePolygons": z.ExcludePolygons,
		"updatedAt":          z.UpdatedAt,
	}
}

// putZone — PUT /consumer/stores/{storeId}/zone (STORE_MANAGER).
func (h *handler) putZone(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body zoneInput
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	z, err := h.svc.upsertZone(r.Context(), actor, chi.URLParam(r, "storeId"), body)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, z)
}

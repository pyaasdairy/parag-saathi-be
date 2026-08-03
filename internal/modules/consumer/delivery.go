package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Consumer home-delivery (last mile). The store manager and delivery rider are
// SAATHI operators (onboarded via KYC, STORE-scoped roles) — these routes reuse
// Saathi's operator auth (Authenticate + RequireRoles) and the operator wire
// format ({data} envelope, camelCase, {error} on failure) because they are
// consumed by the Saathi FE client (delivery.ts / store.ts, service:'consumer').
//
// Flow: consumer places an order (consumer app) → a delivery task is created for
// the nearest Parag Store → the store manager assigns a rider (nearest 15/30/60
// km tier; distance behind a maps seam) → the rider accepts → picks up (out for
// delivery, streams location) → delivers with photo+geo proof → the consumer's
// wallet is debited EXACTLY ONCE (same order-derived ref as settleDeliveredOrders)
// and the consumer sees the order 'delivered'. Riders are salaried — no
// delivery-payment logic here.

const collDeliveries = "consumer_deliveries"

// collRiderPresence — the rider's LIVE duty position: upserted from the fix the
// rider app sends with every offer poll (~8 s while the app is open), so the
// 15-km offer fence and the manager's assign ranking judge the rider where they
// are RIGHT NOW, not where their last delivery ended.
const collRiderPresence = "consumer_rider_presence"

// Distance tiers (km) a store manager escalates through when picking riders.
var riderTiersKm = []float64{15, 30, 60}

type geoPt struct {
	Lat float64 `bson:"lat" json:"lat"`
	Lng float64 `bson:"lng" json:"lng"`
}

type deliveryItem struct {
	Name string `bson:"name" json:"name"`
	Qty  int    `bson:"qty"  json:"qty"`
}

// delivery is the last-mile task. bson = storage; json = the camelCase FE
// Delivery shape (src/core/types/domain.ts) the Saathi client reads verbatim.
type delivery struct {
	MongoID      primitive.ObjectID `bson:"_id,omitempty"        json:"-"`
	ID           string             `bson:"delivery_id"          json:"id"`
	OrderID      string             `bson:"order_id"             json:"-"`
	OrderCode    string             `bson:"order_code"           json:"orderCode"`
	StoreID      string             `bson:"store_id"             json:"storeId"`
	RiderPartyID string             `bson:"rider_party_id"       json:"riderPartyId"`
	ConsumerID   string             `bson:"consumer_id"          json:"consumerPartyId,omitempty"`
	ConsumerName string             `bson:"consumer_name"        json:"consumerName"`
	PhoneMasked  string             `bson:"phone_masked"         json:"phoneMasked"`
	Phone        string             `bson:"phone,omitempty"      json:"phone,omitempty"`
	AddressLabel string             `bson:"address_label"        json:"addressLabel"`
	AddressLine  string             `bson:"address_line"         json:"addressLine"`
	Landmark     string             `bson:"landmark,omitempty"   json:"landmark,omitempty"`
	Geo          geoPt              `bson:"geo"                  json:"geo"`
	Items        []deliveryItem     `bson:"items"                json:"items"`
	Amount       float64            `bson:"amount"               json:"amount"`
	PaymentMode  string             `bson:"payment_mode"         json:"paymentMode"`
	// TrialEligible is set at creation when the order is a PYAAS Taaza subscription
	// item — only then may the 2-paid/2-free welcome trial waive its settle charge.
	TrialEligible bool   `bson:"trial_eligible,omitempty" json:"-"`
	Perishable    bool   `bson:"perishable"           json:"perishable"`
	Slot          string `bson:"slot"                 json:"slot"`
	// Lane mirrors the order's delivery lane: "instant" (≈20-min doorstep run,
	// nearby-rider ranked) or "morning" (the 5-7:30 AM subscription run).
	Lane string `bson:"lane,omitempty"       json:"lane,omitempty"`
	// EtaAt is set for instant deliveries only: placed-at + 20 min (RFC3339).
	EtaAt            string    `bson:"eta_at,omitempty"     json:"etaAt,omitempty"`
	DistanceKm       float64   `bson:"distance_km"          json:"distanceKm"`
	Status           string    `bson:"status"               json:"status"`
	AssignedAt       string    `bson:"assigned_at"          json:"assignedAt"`
	// Auto-dispatch (instant lane): the task is broadcast as OFFERED and the first
	// rider to claim it wins. OfferedAt anchors the offer; RejectedBy lists riders
	// who declined (never re-offered to them); ReofferCount counts re-broadcasts.
	OfferedAt        string    `bson:"offered_at,omitempty"    json:"offeredAt,omitempty"`
	AcceptedAt       string    `bson:"accepted_at,omitempty"   json:"acceptedAt,omitempty"`
	RejectedBy       []string  `bson:"rejected_by,omitempty"   json:"-"`
	ReofferCount     int       `bson:"reoffer_count,omitempty" json:"reofferCount,omitempty"`
	OutForDeliveryAt string    `bson:"out_for_delivery_at,omitempty" json:"outForDeliveryAt,omitempty"`
	DeliveredAt      string    `bson:"delivered_at,omitempty" json:"deliveredAt,omitempty"`
	ProofNote        string    `bson:"proof_note,omitempty" json:"proofNote,omitempty"`
	ProofPhotoURI    string    `bson:"proof_photo_uri,omitempty" json:"proofPhotoUri,omitempty"`
	ProofGeo         *geoPt    `bson:"proof_geo,omitempty"  json:"proofGeo,omitempty"`
	LastKnownGeo     *geoPt    `bson:"last_known_geo,omitempty" json:"lastKnownGeo,omitempty"`
	LastLocationAt   string    `bson:"last_location_at,omitempty" json:"lastLocationAt,omitempty"`
	FailureReason    string    `bson:"failure_reason,omitempty" json:"failureReason,omitempty"`
	DeliveryEventID  string    `bson:"delivery_event_id,omitempty" json:"deliveryEventId,omitempty"`
	CreatedAt        time.Time `bson:"created_at"           json:"-"`
	UpdatedAt        time.Time `bson:"updated_at"           json:"-"`
}

// riderSummary mirrors the FE RiderSummary (store roster).
type riderSummary struct {
	PartyID          string  `json:"partyId"`
	Name             string  `json:"name"`
	PhoneMasked      string  `json:"phoneMasked"`
	VehicleNo        string  `json:"vehicleNo"`
	ActiveDeliveries int     `json:"activeDeliveries"`
	CompletedToday   int     `json:"completedToday"`
	DistanceKm       float64 `json:"distanceKm"`
	WithinTierKm     float64 `json:"withinTierKm"`
}

func newDeliveryID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "del_" + hex.EncodeToString(b)
}

func maskPhone(p string) string {
	d := p
	if len(d) >= 10 {
		d = d[len(d)-10:]
		return d[:2] + "XXXXX" + d[7:]
	}
	return "XXXXXXXXXX"
}

// haversineKm — great-circle distance between two lat/lng points (the maps seam
// currently uses this on-device coords; a real maps/distance API drops in later).
func haversineKm(a, b geoPt) float64 {
	const R = 6371.0
	dLat := (b.Lat - a.Lat) * math.Pi / 180
	dLng := (b.Lng - a.Lng) * math.Pi / 180
	la1 := a.Lat * math.Pi / 180
	la2 := b.Lat * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * R * math.Asin(math.Sqrt(h))
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) ensureDeliveryIndexes(ctx context.Context) error {
	_, err := r.deliveries.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "delivery_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "store_id", Value: 1}, {Key: "assigned_at", Value: 1}}},
		{Keys: bson.D{{Key: "rider_party_id", Value: 1}}},
		{Keys: bson.D{{Key: "order_id", Value: 1}}, Options: options.Index().SetUnique(true)},
	})
	if err != nil {
		return err
	}
	_, err = r.riderPresence.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "party_id", Value: 1}}, Options: options.Index().SetUnique(true),
	})
	return err
}

// upsertRiderPresence records the rider's live duty position (one row per rider).
func (r *repository) upsertRiderPresence(ctx context.Context, partyID string, g geoPt, at time.Time) {
	upsert := true
	_, _ = r.riderPresence.UpdateOne(ctx,
		bson.D{{Key: "party_id", Value: partyID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "lat", Value: g.Lat},
			{Key: "lng", Value: g.Lng},
			{Key: "at", Value: at.UTC().Format(time.RFC3339)},
		}}},
		&options.UpdateOptions{Upsert: &upsert})
}

// findRiderPresence returns the rider's last duty ping (position + RFC3339 time).
func (r *repository) findRiderPresence(ctx context.Context, partyID string) (geoPt, string, bool) {
	var doc struct {
		Lat float64 `bson:"lat"`
		Lng float64 `bson:"lng"`
		At  string  `bson:"at"`
	}
	if err := r.riderPresence.FindOne(ctx, bson.D{{Key: "party_id", Value: partyID}}).Decode(&doc); err != nil {
		return geoPt{}, "", false
	}
	return geoPt{Lat: doc.Lat, Lng: doc.Lng}, doc.At, true
}

func (r *repository) insertDelivery(ctx context.Context, d *delivery) error {
	if _, err := r.deliveries.InsertOne(ctx, d); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // one delivery per order — already created
		}
		return errInternal("delivery create failed")
	}
	return nil
}

func (r *repository) findDeliveryByID(ctx context.Context, id string) (*delivery, error) {
	var d delivery
	err := r.deliveries.FindOne(ctx, bson.D{{Key: "delivery_id", Value: id}}).Decode(&d)
	if isNoDocs(err) {
		return nil, errNotFound("delivery not found")
	}
	if err != nil {
		return nil, errInternal("delivery lookup failed")
	}
	return &d, nil
}

// recentFulfillableOrders returns still-open orders (for backfilling any that
// are missing a delivery task — e.g. placed before a Parag Store existed).
// Subscription-scheduled orders are EXCLUDED: their delivery task is created by
// the subscription worker at the midnight lock, never earlier — the backfill
// must not leak tomorrow's still-modifiable upcoming order into the store queue.
func (r *repository) recentFulfillableOrders(ctx context.Context) ([]order, error) {
	cur, err := r.orders.Find(ctx,
		bson.D{
			{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"placed", "confirmed", "preparing", "assigned"}}}},
			{Key: "subscription_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		},
		options.Find().SetSort(bson.D{{Key: "placed_at", Value: -1}}).SetLimit(300))
	if err != nil {
		return nil, errInternal("orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("orders decode failed")
	}
	return out, nil
}

func (r *repository) findDeliveryByOrder(ctx context.Context, orderID string) (*delivery, error) {
	var d delivery
	err := r.deliveries.FindOne(ctx, bson.D{{Key: "order_id", Value: orderID}}).Decode(&d)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("delivery lookup failed")
	}
	return &d, nil
}

func (r *repository) listDeliveriesByStore(ctx context.Context, storeID string) ([]delivery, error) {
	return r.listDeliveries(ctx, bson.D{{Key: "store_id", Value: storeID}})
}

func (r *repository) listDeliveriesByRider(ctx context.Context, riderPartyID string) ([]delivery, error) {
	return r.listDeliveries(ctx, bson.D{{Key: "rider_party_id", Value: riderPartyID}})
}

func (r *repository) listDeliveries(ctx context.Context, filter bson.D) ([]delivery, error) {
	cur, err := r.deliveries.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "assigned_at", Value: 1}}).SetLimit(500))
	if err != nil {
		return nil, errInternal("deliveries lookup failed")
	}
	out := []delivery{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("deliveries decode failed")
	}
	return out, nil
}

func (r *repository) updateDelivery(ctx context.Context, id string, set bson.D, guard bson.D) (*delivery, error) {
	filter := append(bson.D{{Key: "delivery_id", Value: id}}, guard...)
	set = append(set, bson.E{Key: "updated_at", Value: time.Now().UTC()})
	after := options.After
	var d delivery
	err := r.deliveries.FindOneAndUpdate(ctx, filter, bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after}).Decode(&d)
	if isNoDocs(err) {
		return nil, errConflict("INVALID_STATE", "delivery is not in a valid state for this action")
	}
	if err != nil {
		return nil, errInternal("delivery update failed")
	}
	return &d, nil
}

// claimDelivery is the FIRST-ACCEPT-WINS atomic claim. The first rider whose call
// matches {OFFERED, no rider, not already declined by them} flips it to ACCEPTED and
// wins; every later caller matches nothing and gets a conflict. Single-writer via
// one Mongo FindOneAndUpdate — correct across replicas with no external lock/Redis.
func (r *repository) claimDelivery(ctx context.Context, id, riderPartyID string, now time.Time) (*delivery, error) {
	after := options.After
	var d delivery
	err := r.deliveries.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "delivery_id", Value: id},
			{Key: "status", Value: "OFFERED"},
			{Key: "rider_party_id", Value: ""},
			{Key: "rejected_by", Value: bson.D{{Key: "$ne", Value: riderPartyID}}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "status", Value: "ACCEPTED"},
			{Key: "assigned_at", Value: now.Format(time.RFC3339)},
			{Key: "accepted_at", Value: now.Format(time.RFC3339)},
			{Key: "updated_at", Value: now},
		}}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&d)
	if isNoDocs(err) {
		return nil, errConflict("CLAIMED_BY_OTHER", "another rider already took this order")
	}
	if err != nil {
		return nil, errInternal("delivery claim failed")
	}
	return &d, nil
}

// rejectOffer atomically returns a rider's accepted-but-declined task to the OFFERED
// pool (a re-broadcast), records the decline so it's never re-offered to that rider,
// and bumps the re-offer counter. Guarded to the current owner + pre-pickup states so
// it can never race a pickup/delivery.
func (r *repository) rejectOffer(ctx context.Context, id, riderPartyID string, now time.Time) (*delivery, error) {
	after := options.After
	var d delivery
	err := r.deliveries.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "delivery_id", Value: id},
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"ACCEPTED", "ASSIGNED"}}}},
		},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "status", Value: "OFFERED"},
				{Key: "rider_party_id", Value: ""},
				{Key: "offered_at", Value: now.Format(time.RFC3339)},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$addToSet", Value: bson.D{{Key: "rejected_by", Value: riderPartyID}}},
			{Key: "$inc", Value: bson.D{{Key: "reoffer_count", Value: 1}}},
		},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&d)
	if isNoDocs(err) {
		return nil, errConflict("INVALID_STATE", "this order can no longer be declined")
	}
	if err != nil {
		return nil, errInternal("delivery reject failed")
	}
	return &d, nil
}

// storesForRider lists the store ids the rider holds an ACTIVE DELIVERY_RIDER role at.
func (r *repository) storesForRider(ctx context.Context, riderPartyID string) ([]string, error) {
	oid, err := primitive.ObjectIDFromHex(riderPartyID)
	if err != nil {
		return nil, errBadRequest("bad rider id")
	}
	cur, err := r.roleAssignments.Find(ctx, bson.D{
		{Key: "party_id", Value: oid}, {Key: "role_code", Value: "DELIVERY_RIDER"}, {Key: "status", Value: "ACTIVE"},
	})
	if err != nil {
		return nil, errInternal("rider store lookup failed")
	}
	var rows []struct {
		OrgUnitID primitive.ObjectID `bson:"org_unit_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, errInternal("rider store decode failed")
	}
	ids := make([]string, 0, len(rows))
	for _, x := range rows {
		ids = append(ids, x.OrgUnitID.Hex())
	}
	return ids, nil
}

// listOfferedForStores is a rider's "available orders" feed: OFFERED (unclaimed)
// tasks across the stores they serve that they have NOT already declined.
func (r *repository) listOfferedForStores(ctx context.Context, storeIDs []string, riderPartyID string) ([]delivery, error) {
	if len(storeIDs) == 0 {
		return []delivery{}, nil
	}
	ids := bson.A{}
	for _, s := range storeIDs {
		ids = append(ids, s)
	}
	return r.listDeliveries(ctx, bson.D{
		{Key: "store_id", Value: bson.D{{Key: "$in", Value: ids}}},
		{Key: "status", Value: "OFFERED"},
		{Key: "rider_party_id", Value: ""},
		{Key: "rejected_by", Value: bson.D{{Key: "$ne", Value: riderPartyID}}},
	})
}

// storeGeo reads a STORE org-unit's centre coordinate (read-only) for distance.
func (r *repository) storeGeo(ctx context.Context, storeID string) (geoPt, bool) {
	oid, err := primitive.ObjectIDFromHex(storeID)
	if err != nil {
		return geoPt{}, false
	}
	var doc struct {
		Lat float64 `bson:"geo_lat"`
		Lng float64 `bson:"geo_lng"`
	}
	if err := r.orgUnits.FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&doc); err != nil {
		return geoPt{}, false
	}
	return geoPt{Lat: doc.Lat, Lng: doc.Lng}, true
}

// nearestStore returns the closest active STORE org id to a point (or the first
// store if the point has no geo). One integration seam for many stores later.
func (r *repository) nearestStore(ctx context.Context, at *geoPt) (string, geoPt, error) {
	cur, err := r.orgUnits.Find(ctx, bson.D{{Key: "type", Value: "STORE"}, {Key: "active", Value: true}})
	if err != nil {
		return "", geoPt{}, errInternal("store lookup failed")
	}
	var stores []struct {
		ID  primitive.ObjectID `bson:"_id"`
		Lat float64            `bson:"geo_lat"`
		Lng float64            `bson:"geo_lng"`
	}
	if err := cur.All(ctx, &stores); err != nil || len(stores) == 0 {
		return "", geoPt{}, errNotFound("no serving store")
	}
	best := 0
	if at != nil {
		bestD := math.MaxFloat64
		for i, s := range stores {
			d := haversineKm(*at, geoPt{Lat: s.Lat, Lng: s.Lng})
			if d < bestD {
				bestD, best = d, i
			}
		}
	}
	s := stores[best]
	return s.ID.Hex(), geoPt{Lat: s.Lat, Lng: s.Lng}, nil
}

// geoSane rejects out-of-range and Null-Island (0,0 = missing geo) coordinates,
// so a store that was never given a centre can't win "nearest".
func geoSane(lat, lng float64) bool {
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return false
	}
	return !(lat == 0 && lng == 0)
}

// nearestStoreNamed returns the closest active STORE org-unit WITH a usable geo to
// a point, plus its display name and the great-circle distance in km. ok=false
// when no store has valid coordinates (caller then keeps the pilot fail-open).
func (r *repository) nearestStoreNamed(ctx context.Context, at geoPt) (storeID, name string, geo geoPt, distanceKm float64, ok bool, err error) {
	cur, e := r.orgUnits.Find(ctx, bson.D{{Key: "type", Value: "STORE"}, {Key: "active", Value: true}})
	if e != nil {
		return "", "", geoPt{}, 0, false, errInternal("store lookup failed")
	}
	var stores []struct {
		ID   primitive.ObjectID `bson:"_id"`
		Name string             `bson:"name"`
		Lat  float64            `bson:"geo_lat"`
		Lng  float64            `bson:"geo_lng"`
	}
	if e := cur.All(ctx, &stores); e != nil {
		return "", "", geoPt{}, 0, false, errInternal("store decode failed")
	}
	bestD := math.MaxFloat64
	for _, s := range stores {
		if !geoSane(s.Lat, s.Lng) {
			continue
		}
		d := haversineKm(at, geoPt{Lat: s.Lat, Lng: s.Lng})
		if d < bestD {
			bestD = d
			storeID, name, geo, ok = s.ID.Hex(), s.Name, geoPt{Lat: s.Lat, Lng: s.Lng}, true
		}
	}
	distanceKm = bestD
	if !ok {
		distanceKm = 0
	}
	return storeID, name, geo, distanceKm, ok, nil
}

// storeName resolves a STORE org-unit's display name (for the serviceability +
// Coming-Soon copy). ok=false when the id is unknown.
func (r *repository) storeName(ctx context.Context, storeID string) (string, bool) {
	oid, err := primitive.ObjectIDFromHex(storeID)
	if err != nil {
		return "", false
	}
	var doc struct {
		Name string `bson:"name"`
	}
	if err := r.orgUnits.FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&doc); err != nil {
		return "", false
	}
	return doc.Name, true
}

// riderName resolves a rider party's display name + masked/real phone.
func (r *repository) riderName(ctx context.Context, partyID string) (name, phone string) {
	oid, err := primitive.ObjectIDFromHex(partyID)
	if err != nil {
		return partyID, ""
	}
	var p struct {
		FullName string `bson:"full_name"`
		Phone    string `bson:"phone"`
	}
	if err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&p); err != nil {
		return partyID, ""
	}
	return p.FullName, p.Phone
}

// ridersForStore returns the DELIVERY_RIDER party ids assigned to a store.
func (r *repository) ridersForStore(ctx context.Context, storeID string) ([]string, error) {
	oid, err := primitive.ObjectIDFromHex(storeID)
	if err != nil {
		return nil, errBadRequest("bad store id")
	}
	cur, err := r.roleAssignments.Find(ctx, bson.D{
		{Key: "org_unit_id", Value: oid}, {Key: "role_code", Value: "DELIVERY_RIDER"}, {Key: "status", Value: "ACTIVE"},
	})
	if err != nil {
		return nil, errInternal("rider lookup failed")
	}
	var rows []struct {
		PartyID primitive.ObjectID `bson:"party_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, errInternal("rider decode failed")
	}
	ids := make([]string, 0, len(rows))
	for _, x := range rows {
		ids = append(ids, x.PartyID.Hex())
	}
	return ids, nil
}

// storeForActor returns the STORE org id the operator's role is assigned to.
func (r *repository) storeForActor(ctx context.Context, partyID, roleCode string) (string, error) {
	oid, err := primitive.ObjectIDFromHex(partyID)
	if err != nil {
		return "", errBadRequest("bad actor")
	}
	var ra struct {
		OrgUnitID primitive.ObjectID `bson:"org_unit_id"`
	}
	err = r.roleAssignments.FindOne(ctx, bson.D{
		{Key: "party_id", Value: oid}, {Key: "role_code", Value: roleCode}, {Key: "status", Value: "ACTIVE"},
	}).Decode(&ra)
	if err != nil {
		return "", errForbidden("no active store assignment for this operator")
	}
	return ra.OrgUnitID.Hex(), nil
}

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
	MongoID          primitive.ObjectID `bson:"_id,omitempty"        json:"-"`
	ID               string             `bson:"delivery_id"          json:"id"`
	OrderID          string             `bson:"order_id"             json:"-"`
	OrderCode        string             `bson:"order_code"           json:"orderCode"`
	StoreID          string             `bson:"store_id"             json:"storeId"`
	RiderPartyID     string             `bson:"rider_party_id"       json:"riderPartyId"`
	ConsumerID       string             `bson:"consumer_id"          json:"consumerPartyId,omitempty"`
	ConsumerName     string             `bson:"consumer_name"        json:"consumerName"`
	PhoneMasked      string             `bson:"phone_masked"         json:"phoneMasked"`
	Phone            string             `bson:"phone,omitempty"      json:"phone,omitempty"`
	AddressLabel     string             `bson:"address_label"        json:"addressLabel"`
	AddressLine      string             `bson:"address_line"         json:"addressLine"`
	Landmark         string             `bson:"landmark,omitempty"   json:"landmark,omitempty"`
	Geo              geoPt              `bson:"geo"                  json:"geo"`
	Items            []deliveryItem     `bson:"items"                json:"items"`
	Amount           float64            `bson:"amount"               json:"amount"`
	PaymentMode      string             `bson:"payment_mode"         json:"paymentMode"`
	Perishable       bool               `bson:"perishable"           json:"perishable"`
	Slot             string             `bson:"slot"                 json:"slot"`
	DistanceKm       float64            `bson:"distance_km"          json:"distanceKm"`
	Status           string             `bson:"status"               json:"status"`
	AssignedAt       string             `bson:"assigned_at"          json:"assignedAt"`
	OutForDeliveryAt string             `bson:"out_for_delivery_at,omitempty" json:"outForDeliveryAt,omitempty"`
	DeliveredAt      string             `bson:"delivered_at,omitempty" json:"deliveredAt,omitempty"`
	ProofNote        string             `bson:"proof_note,omitempty" json:"proofNote,omitempty"`
	ProofPhotoURI    string             `bson:"proof_photo_uri,omitempty" json:"proofPhotoUri,omitempty"`
	ProofGeo         *geoPt             `bson:"proof_geo,omitempty"  json:"proofGeo,omitempty"`
	LastKnownGeo     *geoPt             `bson:"last_known_geo,omitempty" json:"lastKnownGeo,omitempty"`
	LastLocationAt   string             `bson:"last_location_at,omitempty" json:"lastLocationAt,omitempty"`
	FailureReason    string             `bson:"failure_reason,omitempty" json:"failureReason,omitempty"`
	DeliveryEventID  string             `bson:"delivery_event_id,omitempty" json:"deliveryEventId,omitempty"`
	CreatedAt        time.Time          `bson:"created_at"           json:"-"`
	UpdatedAt        time.Time          `bson:"updated_at"           json:"-"`
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
	return err
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

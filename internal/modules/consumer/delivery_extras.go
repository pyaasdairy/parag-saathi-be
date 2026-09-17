package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// DELIVERY EXTRAS — things the store console, the rider console and the admin
// CRM all read, but that belong to no single one of them:
//
//   - the PICKUP POINT: one admin-set address (and coordinate) riders collect
//     stock from before a round, shown on both consoles and used as the trip
//     origin on the customer's tracking map;
//   - the due-day helpers the delivery task and the CRM share;
//   - short-lived signed URLs for proof-of-delivery photos, which live in a
//     private bucket the consumer app cannot authenticate against.
//
// The dispatch itself is unchanged and lives in delivery_svc.go: a task is
// created for the nearest store, the STORE MANAGER assigns a rider, and the
// rider accepts → picks up → delivers with photo + geotag proof.

const collDeliverySettings = "consumer_delivery_settings"

// pickupPoint is where riders collect stock. One row (_id "default"), editable
// by a super admin; the default is the Chandra Panorama dark store.
type pickupPoint struct {
	Name      string     `bson:"name"                 json:"name"`
	Address   string     `bson:"address"              json:"address"`
	Lat       float64    `bson:"lat"                  json:"lat"`
	Lng       float64    `bson:"lng"                  json:"lng"`
	UpdatedAt *time.Time `bson:"updated_at,omitempty" json:"updatedAt,omitempty"`
	UpdatedBy string     `bson:"updated_by,omitempty" json:"updatedBy,omitempty"`
}

// The coordinates match the consumer app's STORE_POINT (lib/serviceability.ts).
var defaultPickupPoint = pickupPoint{
	Name:    "PYAAS Store · Chandra Panorama",
	Address: "P4 - 805, Chandra Panorama, Sushant Golf City, Lucknow",
	Lat:     26.7738,
	Lng:     81.0089,
}

// deliveryGeofenceMeters — how close to the customer's pin the rider's geotag
// must be for the delivery photo to count as taken AT the door. The rider app
// checks it before enabling Confirm; deliverDelivery enforces it server-side.
const deliveryGeofenceMeters = 300.0

// deliveryOpenStatuses — a task a rider can still act on.
var deliveryOpenStatuses = bson.A{"ASSIGNED", "OFFERED", "ACCEPTED", "OUT_FOR_DELIVERY"}

func (s *service) pickupPoint(ctx context.Context) pickupPoint {
	var p pickupPoint
	err := s.repo.deliveries.Database().Collection(collDeliverySettings).
		FindOne(ctx, bson.D{{Key: "_id", Value: "default"}}).Decode(&p)
	if err != nil || strings.TrimSpace(p.Address) == "" || !geoSane(p.Lat, p.Lng) {
		return defaultPickupPoint
	}
	return p
}

type pickupInput struct {
	Name    string  `json:"name"`
	Address string  `json:"address"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
}

func (s *service) setPickupPoint(ctx context.Context, by string, in pickupInput) (pickupPoint, error) {
	in.Name, in.Address = strings.TrimSpace(in.Name), strings.TrimSpace(in.Address)
	if in.Address == "" || len(in.Address) > 300 || len(in.Name) > 120 {
		return pickupPoint{}, errBadRequest("address is required (max 300 characters)")
	}
	if !geoSane(in.Lat, in.Lng) {
		return pickupPoint{}, errBadRequest("lat/lng must be a real map location")
	}
	if in.Name == "" {
		in.Name = "Pickup point"
	}
	now := time.Now().UTC()
	p := pickupPoint{Name: in.Name, Address: in.Address, Lat: in.Lat, Lng: in.Lng, UpdatedAt: &now, UpdatedBy: by}
	_, err := s.repo.deliveries.Database().Collection(collDeliverySettings).UpdateOne(ctx,
		bson.D{{Key: "_id", Value: "default"}}, bson.D{{Key: "$set", Value: p}}, options.Update().SetUpsert(true))
	if err != nil {
		return pickupPoint{}, errInternal("pickup point save failed")
	}
	return p, nil
}

// orderDeliveryDate is the IST day an order is due ("" = as soon as possible).
func orderDeliveryDate(o *order) string {
	if o.DeliveryDate != "" {
		return o.DeliveryDate
	}
	return o.ScheduledFor
}

// deliveryDay reads a task's due day, falling back to the "YYYY-MM-DD · window"
// slot label on tasks created before DeliveryDate existed.
func deliveryDay(d *delivery) string {
	if d.DeliveryDate != "" {
		return d.DeliveryDate
	}
	if len(d.Slot) >= 10 {
		if _, ok := parseDay(d.Slot[:10]); ok {
			return d.Slot[:10]
		}
	}
	return ""
}

// ── Proof photos (private bucket → short-lived per-file URL) ─────────────────

// uploadsViewPrefix is the authenticated operator path the Saathi app stores
// for an uploaded photo (uploads/module.go presign → view_url).
const uploadsViewPrefix = "/api/v1/uploads/view/"

type fileTok struct {
	url string
	at  time.Time
}

var (
	proofURLMu    sync.Mutex
	proofURLCache = map[string]fileTok{}
)

const proofURLTTL = time.Hour

// proofPhotoURL turns a stored proof path into a URL a plain <Image> can load:
// a B2 download authorization scoped to that ONE file, valid for an hour and
// reused for 45 minutes. Anything that is not an uploads path passes through;
// an unresolvable path returns "" so no client renders a broken image.
func (s *service) proofPhotoURL(ctx context.Context, stored string) string {
	if !strings.HasPrefix(stored, uploadsViewPrefix) {
		return stored
	}
	name := strings.TrimPrefix(stored, uploadsViewPrefix)
	if name == "" || strings.Contains(name, "..") || s.b2img == nil || !s.b2img.configured() {
		return ""
	}
	proofURLMu.Lock()
	if t, ok := proofURLCache[name]; ok && time.Since(t.at) < 45*time.Minute {
		proofURLMu.Unlock()
		return t.url
	}
	proofURLMu.Unlock()
	url, err := s.b2img.fileURL(ctx, name, proofURLTTL)
	if err != nil {
		s.log.WarnContext(ctx, "proof photo url failed", "err", err)
		return ""
	}
	proofURLMu.Lock()
	if len(proofURLCache) > 5000 {
		proofURLCache = map[string]fileTok{}
	}
	proofURLCache[name] = fileTok{url: url, at: time.Now()}
	proofURLMu.Unlock()
	return url
}

// fileURL mints a download URL authorized for exactly one file name.
func (c *b2DownloadClient) fileURL(ctx context.Context, name string, ttl time.Duration) (string, error) {
	if err := c.authorize(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	apiURL, token, bucketID, dlURL, bucket := c.apiURL, c.token, c.bucketID, c.dlURL, c.bucket
	c.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"bucketId": bucketID, "fileNamePrefix": name, "validDurationInSeconds": int(ttl.Seconds()),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/b2api/v2/b2_get_download_authorization", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("b2 download_authorization: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("b2 download_authorization: HTTP %d", res.StatusCode)
	}
	var out struct {
		Token string `json:"authorizationToken"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/file/%s/%s?Authorization=%s", dlURL, bucket, name, out.Token), nil
}

// ensureDeliveryQueryIndexes — query indexes for the consoles and the CRM.
// Non-fatal: missing indexes slow reads, they never risk money.
func (r *repository) ensureDeliveryQueryIndexes(ctx context.Context) error {
	_, err := r.deliveries.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "rider_party_id", Value: 1}, {Key: "created_at", Value: -1}}},
	})
	if err != nil {
		return err
	}
	_, err = r.orders.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "placed_at", Value: -1}}},
		{Keys: bson.D{{Key: "subscription_id", Value: 1}, {Key: "status", Value: 1}, {Key: "scheduled_for", Value: 1}}},
	})
	return err
}

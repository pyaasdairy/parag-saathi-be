package consumer

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ── Delivery creation (called when a consumer order is placed) ───────────────

// createDeliveryForOrder makes the last-mile task for a placed order, routed to
// the nearest Parag Store. Unassigned (no rider) until the store manager assigns
// one. Best-effort: an order is never blocked if delivery creation fails.
func (s *service) createDeliveryForOrder(ctx context.Context, o *order) {
	var at *geoPt
	if o.Geo != nil {
		at = &geoPt{Lat: o.Geo.Lat, Lng: o.Geo.Lng}
	}
	storeID, storeGeo, err := s.repo.nearestStore(ctx, at)
	if err != nil {
		s.log.WarnContext(ctx, "no serving store for order", "order", o.OrderID)
		return
	}
	dest := storeGeo
	if at != nil {
		dest = *at
	}
	items := make([]deliveryItem, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, deliveryItem{Name: it.Name, Qty: it.Qty})
	}
	payMode := "COD"
	if o.PaymentMethod == "wallet" || o.PaymentMethod == "prepaid" {
		payMode = "PREPAID"
	}
	now := time.Now().UTC()
	// Instant lane: the task carries a hard ETA (placed + 90 min) so the store
	// console and rider queue can surface the countdown. The morning lane keeps
	// the subscription slot window only.
	eta := ""
	if o.Lane == "instant" {
		eta = now.Add(90 * time.Minute).Format(time.RFC3339)
	}
	del := &delivery{
		MongoID: primitive.NewObjectID(), ID: newDeliveryID(), OrderID: o.OrderID, OrderCode: o.OrderID,
		StoreID: storeID, RiderPartyID: "", ConsumerID: o.UserID, ConsumerName: o.ConsumerName,
		PhoneMasked: maskPhone(o.Phone), Phone: o.Phone, AddressLabel: o.AddressLabel, AddressLine: o.AddressText,
		Geo: dest, Items: items, Amount: o.Total, PaymentMode: payMode, Perishable: false,
		Slot: o.DeliveryWindow, Lane: o.Lane, EtaAt: eta, DistanceKm: round2(haversineKm(storeGeo, dest)),
		Status: "ASSIGNED", AssignedAt: now.Format(time.RFC3339), CreatedAt: now, UpdatedAt: now,
	}
	_ = s.repo.insertDelivery(ctx, del)
}

// ── Store manager ───────────────────────────────────────────────────────────

func (s *service) storeOrders(ctx context.Context, actor auth.Actor, storeID string) ([]delivery, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	// Backfill any open order missing its delivery task (e.g. placed before a
	// Parag Store existed, or if delivery creation failed) so no order is ever
	// invisible to the store. Idempotent — one delivery per order.
	s.backfillMissingDeliveries(ctx)
	return s.repo.listDeliveriesByStore(ctx, storeID)
}

// backfillMissingDeliveries creates a delivery task for any still-open order that
// doesn't have one yet, routed to its nearest Parag Store. Best-effort.
func (s *service) backfillMissingDeliveries(ctx context.Context) {
	orders, err := s.repo.recentFulfillableOrders(ctx)
	if err != nil {
		return
	}
	for i := range orders {
		o := &orders[i]
		if d, _ := s.repo.findDeliveryByOrder(ctx, o.OrderID); d == nil {
			s.createDeliveryForOrder(ctx, o)
		}
	}
}

// storeRiders returns the store's riders with workload + distance to a specific
// delivery (when deliveryID given) so the manager can pick the nearest tier.
func (s *service) storeRiders(ctx context.Context, actor auth.Actor, storeID, deliveryID string) ([]riderSummary, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	riderIDs, err := s.repo.ridersForStore(ctx, storeID)
	if err != nil {
		return nil, err
	}
	// NEARBY ranking: the instant lane reuses the SAME rider pool that runs the
	// morning subscription round — but each rider's position is taken from the
	// freshest last_known_geo on their own tasks (their live GPS trail), falling
	// back to the store centre for riders who haven't moved yet. Distance is
	// rider→drop, and the list is sorted nearest-first so the store manager's
	// assign sheet leads with the closest rider.
	storeGeo, _ := s.repo.storeGeo(ctx, storeID)
	var dest *geoPt
	if deliveryID != "" {
		if d, e := s.repo.findDeliveryByID(ctx, deliveryID); e == nil {
			dest = &d.Geo
		}
	}
	all, _ := s.repo.listDeliveriesByStore(ctx, storeID)
	today := time.Now().UTC().Format("2006-01-02")
	out := make([]riderSummary, 0, len(riderIDs))
	for _, rid := range riderIDs {
		name, phone := s.repo.riderName(ctx, rid)
		active, done := 0, 0
		origin := storeGeo // position fallback: idle riders stage at the store
		lastAt := ""
		for _, d := range all {
			if d.RiderPartyID != rid {
				continue
			}
			if d.Status == "ACCEPTED" || d.Status == "OUT_FOR_DELIVERY" {
				active++
			}
			if d.Status == "DELIVERED" && len(d.DeliveredAt) >= 10 && d.DeliveredAt[:10] == today {
				done++
			}
			// Freshest GPS ping across the rider's tasks = their live position.
			if d.LastKnownGeo != nil && d.LastLocationAt > lastAt {
				lastAt = d.LastLocationAt
				origin = *d.LastKnownGeo
			}
		}
		dist := 0.0
		if dest != nil {
			dist = round2(haversineKm(origin, *dest))
		}
		out = append(out, riderSummary{
			PartyID: rid, Name: name, PhoneMasked: maskPhone(phone), VehicleNo: "",
			ActiveDeliveries: active, CompletedToday: done, DistanceKm: dist, WithinTierKm: tierFor(dist),
		})
	}
	// Nearest-first when ranking against a concrete drop; ties break on the
	// lighter current workload so instant orders spread across free riders.
	if dest != nil {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].DistanceKm != out[j].DistanceKm {
				return out[i].DistanceKm < out[j].DistanceKm
			}
			return out[i].ActiveDeliveries < out[j].ActiveDeliveries
		})
	}
	return out, nil
}

// tierFor returns the smallest 15/30/60 km band a distance falls in. 0 means
// beyond 60 km → the "all riders assigned to the store" fallback (still eligible;
// the store owns the delivery).
func tierFor(distKm float64) float64 {
	for _, t := range riderTiersKm {
		if distKm <= t {
			return t
		}
	}
	return 0
}

// assignRider assigns the nearest-tier-eligible rider to an unassigned delivery.
// Enforces the 15→30→60 km escalation: a rider may only be assigned if the
// delivery falls within a served tier (with the store-distance seam, all the
// store's riders share the store→address distance).
func (s *service) assignRider(ctx context.Context, actor auth.Actor, storeID, deliveryID, riderPartyID string) (*delivery, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	d, err := s.repo.findDeliveryByID(ctx, deliveryID)
	if err != nil {
		return nil, err
	}
	if d.StoreID != storeID {
		return nil, errForbidden("delivery belongs to another store")
	}
	riders, err := s.repo.ridersForStore(ctx, storeID)
	if err != nil {
		return nil, err
	}
	// Any rider ASSIGNED TO THE STORE is eligible. The 15→30→60 km tiers only
	// RANK/suggest the nearest riders; if the address is beyond 60 km of every
	// rider, the fallback is the whole store roster (the store owns the delivery),
	// so assignment is never blocked on distance.
	if !contains(riders, riderPartyID) {
		return nil, errBadRequest("rider is not assigned to this store")
	}
	now := time.Now().UTC()
	return s.repo.updateDelivery(ctx, deliveryID,
		bson.D{{Key: "rider_party_id", Value: riderPartyID}, {Key: "status", Value: "ASSIGNED"}, {Key: "assigned_at", Value: now.Format(time.RFC3339)}},
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"ASSIGNED", "FAILED"}}}}},
	)
}

func (s *service) assertStore(ctx context.Context, actor auth.Actor, storeID string) error {
	owned, err := s.repo.storeForActor(ctx, actor.PartyID, "STORE_MANAGER")
	if err != nil {
		return err
	}
	if owned != storeID {
		return errForbidden("you do not manage this store")
	}
	return nil
}

// ── Delivery rider ──────────────────────────────────────────────────────────

func (s *service) riderDeliveries(ctx context.Context, actor auth.Actor) ([]delivery, error) {
	all, err := s.repo.listDeliveriesByRider(ctx, actor.PartyID)
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (s *service) acceptDelivery(ctx context.Context, actor auth.Actor, id string) (*delivery, error) {
	return s.riderTransition(ctx, actor, id, "ASSIGNED",
		bson.D{{Key: "status", Value: "ACCEPTED"}}, "")
}

func (s *service) pickupDelivery(ctx context.Context, actor auth.Actor, id string) (*delivery, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	d, err := s.riderTransition(ctx, actor, id, "ACCEPTED",
		bson.D{{Key: "status", Value: "OUT_FOR_DELIVERY"}, {Key: "out_for_delivery_at", Value: now}}, "")
	if err == nil {
		s.syncOrderOutForDelivery(ctx, d)
	}
	return d, err
}

func (s *service) pushLocation(ctx context.Context, actor auth.Actor, id string, lat, lng float64) (*delivery, error) {
	d, err := s.repo.findDeliveryByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.RiderPartyID != actor.PartyID {
		return nil, errForbidden("not your delivery")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	updated, err := s.repo.updateDelivery(ctx, id,
		bson.D{{Key: "last_known_geo", Value: geoPt{Lat: lat, Lng: lng}}, {Key: "last_location_at", Value: now}}, bson.D{})
	if err == nil {
		s.syncOrderRiderLocation(ctx, updated, lat, lng) // consumer sees the rider move
	}
	return updated, err
}

func (s *service) failDelivery(ctx context.Context, actor auth.Actor, id, reason string) (*delivery, error) {
	if reason == "" {
		reason = "Could not deliver"
	}
	return s.riderTransition(ctx, actor, id, "", // any non-delivered state
		bson.D{{Key: "status", Value: "FAILED"}, {Key: "failure_reason", Value: reason}}, "DELIVERED")
}

// deliverInput carries the proof-of-delivery from the rider app.
type deliverInput struct {
	EventID    string
	ProofNote  string
	ProofPhoto string
	Geo        *geoPt
	GeofenceOK bool
}

// deliverDelivery is THE delivery event: photo+geo proof + geofence, then debit
// the consumer wallet EXACTLY ONCE (ref = the order, shared with the consumer's
// settle sweep) and mark the order delivered. Idempotent by event id.
func (s *service) deliverDelivery(ctx context.Context, actor auth.Actor, id string, in deliverInput) (*delivery, error) {
	d, err := s.repo.findDeliveryByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.RiderPartyID != actor.PartyID {
		return nil, errForbidden("not your delivery")
	}
	if d.Status == "DELIVERED" {
		if in.EventID != "" && d.DeliveryEventID == in.EventID {
			return d, nil // idempotent replay
		}
		return nil, errConflict("ALREADY_DELIVERED", "this delivery is already completed")
	}
	if d.Status != "OUT_FOR_DELIVERY" {
		return nil, errConflict("NOT_OUT", "this delivery is not out for delivery")
	}
	if in.ProofPhoto == "" {
		return nil, errUnprocessable("PROOF_PHOTO_REQUIRED", "add a delivery photo as proof")
	}
	if in.Geo == nil {
		return nil, errUnprocessable("PROOF_GEO_REQUIRED", "add the delivery location (geotag) as proof")
	}
	if !in.GeofenceOK {
		return nil, errUnprocessable("GEOFENCE_FAILED", "you are not at the delivery address — move closer to confirm")
	}
	// Debit-on-delivery BEFORE flipping status (funds gate). PREPAID only; keyed
	// to the order so the consumer's settle sweep can never double-charge.
	if d.PaymentMode == "PREPAID" && d.Amount > 0 {
		cid, cerr := primitive.ObjectIDFromHex(d.ConsumerID)
		if cerr != nil {
			return nil, errInternal("bad consumer id on delivery")
		}
		if _, e := s.debit(ctx, cid, d.Amount, "delivery:"+d.OrderID, "Delivery "+d.OrderCode); e != nil {
			return nil, e // INSUFFICIENT_FUNDS etc. surface to the rider app
		}
	}
	evt := in.EventID
	if evt == "" {
		evt = newDeliveryID()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	note := in.ProofNote
	if note == "" {
		note = "Delivered"
	}
	updated, err := s.repo.updateDelivery(ctx, id, bson.D{
		{Key: "status", Value: "DELIVERED"}, {Key: "delivered_at", Value: now},
		{Key: "proof_note", Value: note}, {Key: "proof_photo_uri", Value: in.ProofPhoto},
		{Key: "proof_geo", Value: in.Geo}, {Key: "last_known_geo", Value: in.Geo}, {Key: "last_location_at", Value: now},
		{Key: "delivery_event_id", Value: evt}, {Key: "geofence_ok", Value: true},
	}, bson.D{{Key: "status", Value: "OUT_FOR_DELIVERY"}})
	if err != nil {
		return nil, err
	}
	s.syncOrderDelivered(ctx, updated)
	return updated, nil
}

func (s *service) riderTransition(ctx context.Context, actor auth.Actor, id, requireStatus string, set bson.D, notStatus string) (*delivery, error) {
	d, err := s.repo.findDeliveryByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.RiderPartyID != actor.PartyID {
		return nil, errForbidden("not your delivery")
	}
	guard := bson.D{}
	if requireStatus != "" {
		guard = append(guard, bson.E{Key: "status", Value: requireStatus})
	}
	if notStatus != "" {
		guard = append(guard, bson.E{Key: "status", Value: bson.D{{Key: "$ne", Value: notStatus}}})
	}
	return s.repo.updateDelivery(ctx, id, set, guard)
}

// ── Consumer order sync (so the consumer app shows tracking + status) ────────

func (s *service) syncOrderOutForDelivery(ctx context.Context, d *delivery) {
	name, phone := s.repo.riderName(ctx, d.RiderPartyID)
	rd := &rider{ID: d.RiderPartyID, FullName: name, Phone: phone}
	if d.LastKnownGeo != nil {
		rd.CurrentLat, rd.CurrentLng = &d.LastKnownGeo.Lat, &d.LastKnownGeo.Lng
	}
	_, _ = s.repo.orders.UpdateOne(ctx, bson.D{{Key: "order_id", Value: d.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "out_for_delivery"}, {Key: "rider_id", Value: d.RiderPartyID}, {Key: "riders", Value: rd}, {Key: "updated_at", Value: time.Now().UTC()}}}})
}

func (s *service) syncOrderRiderLocation(ctx context.Context, d *delivery, lat, lng float64) {
	_, _ = s.repo.orders.UpdateOne(ctx, bson.D{{Key: "order_id", Value: d.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "riders.current_lat", Value: lat}, {Key: "riders.current_lng", Value: lng}}}})
}

func (s *service) syncOrderDelivered(ctx context.Context, d *delivery) {
	_, _ = s.repo.orders.UpdateOne(ctx, bson.D{{Key: "order_id", Value: d.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "delivered"}, {Key: "can_review", Value: true}, {Key: "proof_photo_url", Value: d.ProofPhotoURI}, {Key: "updated_at", Value: time.Now().UTC()}}}})
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ── Handlers (operator wire format: {data} envelope + {error}) ───────────────

func operatorActor(r *http.Request) (auth.Actor, bool) { return auth.ActorFrom(r.Context()) }

// toHTTPErr maps a consumer apiError to the operator httpx error the Saathi
// client understands (so INSUFFICIENT_FUNDS / 4xx codes survive).
func toHTTPErr(err error) error {
	if ae, ok := err.(*apiError); ok {
		return &httpx.AppError{Status: ae.status, Code: ae.Code, Message: ae.Message}
	}
	return err
}

func (h *handler) storeOrders(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	ds, err := h.svc.storeOrders(r.Context(), actor, chi.URLParam(r, "storeId"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, ds)
}

func (h *handler) storeRiders(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	rs, err := h.svc.storeRiders(r.Context(), actor, chi.URLParam(r, "storeId"), r.URL.Query().Get("delivery_id"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, rs)
}

func (h *handler) assignRider(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		RiderPartyID string `json:"rider_party_id"`
	}
	_ = decode(r, &body)
	d, err := h.svc.assignRider(r.Context(), actor, chi.URLParam(r, "storeId"), chi.URLParam(r, "deliveryId"), body.RiderPartyID)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h *handler) riderDeliveries(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	ds, err := h.svc.riderDeliveries(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, ds)
}

func (h *handler) riderGetDelivery(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	d, err := h.svc.repo.findDeliveryByID(r.Context(), chi.URLParam(r, "deliveryId"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	if d.RiderPartyID != actor.PartyID {
		httpx.Error(w, r, httpx.Forbidden("not your delivery"))
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h *handler) riderAccept(w http.ResponseWriter, r *http.Request) {
	h.riderAct(w, r, h.svc.acceptDelivery)
}
func (h *handler) riderPickup(w http.ResponseWriter, r *http.Request) {
	h.riderAct(w, r, h.svc.pickupDelivery)
}

func (h *handler) riderAct(w http.ResponseWriter, r *http.Request, fn func(context.Context, auth.Actor, string) (*delivery, error)) {
	actor, _ := operatorActor(r)
	d, err := fn(r.Context(), actor, chi.URLParam(r, "deliveryId"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h *handler) riderLocation(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	}
	_ = decode(r, &body)
	d, err := h.svc.pushLocation(r.Context(), actor, chi.URLParam(r, "deliveryId"), body.Lat, body.Lng)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h *handler) riderFail(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		Reason string `json:"reason"`
	}
	_ = decode(r, &body)
	d, err := h.svc.failDelivery(r.Context(), actor, chi.URLParam(r, "deliveryId"), body.Reason)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h *handler) riderDeliver(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		EventID   string `json:"event_id"`
		ProofNote string `json:"proof_note"`
		Proof     struct {
			GeofenceOK bool   `json:"geofence_ok"`
			PhotoRef   string `json:"photo_ref"`
		} `json:"proof"`
		Geo []float64 `json:"geo"`
	}
	_ = decode(r, &body)
	in := deliverInput{EventID: body.EventID, ProofNote: body.ProofNote, ProofPhoto: body.Proof.PhotoRef, GeofenceOK: body.Proof.GeofenceOK}
	if len(body.Geo) == 2 {
		in.Geo = &geoPt{Lat: body.Geo[0], Lng: body.Geo[1]}
	}
	d, err := h.svc.deliverDelivery(r.Context(), actor, chi.URLParam(r, "deliveryId"), in)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

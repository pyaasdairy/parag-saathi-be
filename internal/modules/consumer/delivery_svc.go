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
	trialEligible := false
	for _, it := range o.Items {
		items = append(items, deliveryItem{Name: it.Name, Qty: it.Qty})
		if isTrialProduct(it.ProductID) {
			trialEligible = true // 2+2 welcome trial (Taaza or Gold launch SKUs)
		}
	}
	payMode := "COD"
	if o.PaymentMethod == "wallet" || o.PaymentMethod == "prepaid" {
		payMode = "PREPAID"
	}
	now := time.Now().UTC()
	// Instant lane: the task carries a hard ETA anchored to the ORDER's
	// placed-at (+20 min) — not task-creation time, so a backfilled task keeps
	// the customer's original promise instead of restarting the clock.
	eta := ""
	if o.Lane == "instant" {
		base := o.PlacedAt
		if base.IsZero() {
			base = now
		}
		eta = base.Add(20 * time.Minute).Format(time.RFC3339)
	}
	// Instant orders BROADCAST as OFFERED to the store's riders — the first rider to
	// claim it wins (auto-dispatch). Morning/subscription tasks stay unassigned
	// ASSIGNED for the store manager's batch route-assign.
	status, offeredAt := "ASSIGNED", ""
	if o.Lane == "instant" {
		status, offeredAt = "OFFERED", now.Format(time.RFC3339)
	}
	del := &delivery{
		MongoID: primitive.NewObjectID(), ID: newDeliveryID(), OrderID: o.OrderID, OrderCode: o.OrderID,
		StoreID: storeID, RiderPartyID: "", ConsumerID: o.UserID, ConsumerName: o.ConsumerName,
		PhoneMasked: maskPhone(o.Phone), Phone: o.Phone, AddressLabel: o.AddressLabel, AddressLine: o.AddressText,
		Geo: dest, Items: items, Amount: o.Total, PaymentMode: payMode, TrialEligible: trialEligible, Perishable: false,
		Slot: o.DeliveryWindow, Lane: o.Lane, EtaAt: eta, DistanceKm: round2(haversineKm(storeGeo, dest)),
		Status: status, OfferedAt: offeredAt, AssignedAt: now.Format(time.RFC3339), CreatedAt: now, UpdatedAt: now,
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
	storeGeo, geoOK := s.repo.storeGeo(ctx, storeID)
	var dest *geoPt
	if deliveryID != "" {
		if d, e := s.repo.findDeliveryByID(ctx, deliveryID); e == nil {
			dest = &d.Geo
		}
	}
	// A store with no geo must not rank idle riders from (0,0) — thousands of
	// phantom km. Stage geo-less stores AT the drop point (distance 0), letting
	// riders with real GPS pings still rank truthfully.
	if !geoOK && dest != nil {
		storeGeo = *dest
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

// offerPingFreshness — how recent a rider's GPS ping must be to count as their
// LIVE position for the 15-km offer fence. Riders push location every ~20 s
// while on a delivery, so 10 minutes comfortably spans a run; anything older is
// a stale/stuck fix and falls back to the store-staging rule.
const offerPingFreshness = 10 * time.Minute

// offeredForRider is the rider's "available orders" feed — OFFERED (broadcast)
// instant tasks at any store they serve, minus ones they already declined,
// fenced to the 15-km broadcast radius (riderTiersKm[0]): the accept-request
// only reaches riders within 15 km of the drop; the first to claim wins
// (claimDelivery is atomic, so exactly one rider can ever take it).
func (s *service) offeredForRider(ctx context.Context, actor auth.Actor) ([]delivery, error) {
	stores, err := s.repo.storesForRider(ctx, actor.PartyID)
	if err != nil {
		return nil, err
	}
	offered, err := s.repo.listOfferedForStores(ctx, stores, actor.PartyID)
	if err != nil {
		return nil, err
	}
	// Rider origin = their freshest GPS ping across their own tasks (the live
	// trail) — but ONLY if it is FRESH. A stale/stuck ping (older than the
	// freshness window) is discarded: it may be yesterday's position, so it must
	// never include OR exclude a rider from an offer. No fresh ping → the rider
	// stages AT their store (the duty station), the same positioning rule the
	// manager's assign sheet uses.
	cutoff := time.Now().UTC().Add(-offerPingFreshness).Format(time.RFC3339)
	mine, _ := s.repo.listDeliveriesByRider(ctx, actor.PartyID)
	var origin *geoPt
	lastAt := ""
	for i := range mine {
		d := &mine[i]
		if d.LastKnownGeo != nil && d.LastLocationAt > lastAt && d.LastLocationAt >= cutoff {
			lastAt = d.LastLocationAt
			origin = d.LastKnownGeo
		}
	}
	storeGeos := map[string]*geoPt{}
	out := make([]delivery, 0, len(offered))
	for i := range offered {
		d := &offered[i]
		from := origin
		if from == nil {
			g, seen := storeGeos[d.StoreID]
			if !seen {
				if sg, ok := s.repo.storeGeo(ctx, d.StoreID); ok {
					cp := sg
					g = &cp
				}
				storeGeos[d.StoreID] = g
			}
			from = g
		}
		// Unknown geo on both sides → still offered: an order must never be
		// stranded just because we cannot rank the distance.
		if from == nil || haversineKm(*from, d.Geo) <= riderTiersKm[0] {
			out = append(out, *d)
		}
	}
	return out, nil
}

// claimOfferedDelivery is the FIRST-ACCEPT-WINS path: the first rider to claim an
// OFFERED task wins it atomically; everyone else gets CLAIMED_BY_OTHER (409).
func (s *service) claimOfferedDelivery(ctx context.Context, actor auth.Actor, id string) (*delivery, error) {
	d, err := s.repo.claimDelivery(ctx, id, actor.PartyID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	s.syncOrderAssigned(ctx, d) // consumer immediately sees the assigned rider
	return d, nil
}

// rejectOfferedDelivery returns a claimed (pre-pickup) task to the pool so a nearby
// rider can take it — the re-broadcast. Not allowed once picked up.
func (s *service) rejectOfferedDelivery(ctx context.Context, actor auth.Actor, id string) (*delivery, error) {
	d, err := s.repo.rejectOffer(ctx, id, actor.PartyID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	s.syncOrderFindingRider(ctx, d)
	return d, nil
}

// acceptDelivery is the MANAGER-ASSIGNED (morning/subscription) accept: a rider
// accepts a task the store manager already assigned to them. Instant/OFFERED tasks
// go through claimOfferedDelivery (first-accept-wins) instead.
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
	// Belt + braces with cancelOrder's task-failing: refuse to deliver a task
	// whose parent order was cancelled (a stale task must never debit money or
	// resurrect a cancelled order to delivered).
	if o, _ := s.repo.findOrderAnyUser(ctx, d.OrderID); o != nil && o.Status == "cancelled" {
		_, _ = s.repo.updateDelivery(ctx, d.ID,
			bson.D{{Key: "status", Value: "FAILED"}, {Key: "failure_reason", Value: "Order cancelled by the customer"}, {Key: "updated_at", Value: time.Now().UTC()}},
			bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED", "FAILED"}}}}},
		)
		return nil, errConflict("ORDER_CANCELLED", "this order was cancelled by the customer")
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
		amount := d.Amount
		// PYAAS Taaza subscription deliveries (the morning run) flow through the
		// "2 PAID then 2 FREE" welcome trial: the first 2 delivered days pay full,
		// the next 2 are on us (effective 0), then normal. The window counts
		// DELIVERED days (dated in IST) and is idempotent by this delivered-day key,
		// so a settle re-run never double-advances it. Only Taaza qualifies
		// (TrialEligible), so a gold/shakti/chai subscription never gets free days.
		if d.Lane == "morning" && d.TrialEligible {
			day := trialDay(time.Now())
			eff, _, terr := s.trialChargeFor(ctx, cid, trialDeliveryKey(cid, day), d.Amount)
			if terr != nil {
				return nil, terr
			}
			amount = eff
		}
		// A free trial day settles to 0 — no wallet movement (debit rejects ≤0).
		if amount > 0 {
			if _, e := s.debit(ctx, cid, amount, "delivery:"+d.OrderID, "Delivery "+d.OrderCode); e != nil {
				return nil, e // INSUFFICIENT_FUNDS etc. surface to the rider app
			}
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

// syncOrderAssigned surfaces the winning rider to the consumer the moment a rider
// claims (or is assigned) the task — the order flips placed→assigned.
func (s *service) syncOrderAssigned(ctx context.Context, d *delivery) {
	name, phone := s.repo.riderName(ctx, d.RiderPartyID)
	rd := &rider{ID: d.RiderPartyID, FullName: name, Phone: phone}
	_, _ = s.repo.orders.UpdateOne(ctx, bson.D{{Key: "order_id", Value: d.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "assigned"}, {Key: "rider_id", Value: d.RiderPartyID}, {Key: "riders", Value: rd}, {Key: "updated_at", Value: time.Now().UTC()}}}})
}

// syncOrderFindingRider drops the order back to placed (finding a rider) when a
// claimed task is rejected and re-broadcast, so the consumer sees it's re-offered.
func (s *service) syncOrderFindingRider(ctx context.Context, d *delivery) {
	_, _ = s.repo.orders.UpdateOne(ctx, bson.D{{Key: "order_id", Value: d.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "placed"}, {Key: "rider_id", Value: ""}, {Key: "updated_at", Value: time.Now().UTC()}}}})
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

// riderAvailable — GET /delivery/tasks/available: the OFFERED (broadcast) pool the
// rider's accept-popup polls.
func (h *handler) riderAvailable(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	ds, err := h.svc.offeredForRider(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, ds)
}

// riderClaim — POST /delivery/tasks/{id}/claim: first-accept-wins (409 if taken).
func (h *handler) riderClaim(w http.ResponseWriter, r *http.Request) {
	h.riderAct(w, r, h.svc.claimOfferedDelivery)
}

// riderReject — POST /delivery/tasks/{id}/reject: decline a claimed task, re-broadcast.
func (h *handler) riderReject(w http.ResponseWriter, r *http.Request) {
	h.riderAct(w, r, h.svc.rejectOfferedDelivery)
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

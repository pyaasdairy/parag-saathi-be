package consumer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
	switch {
	case errors.Is(err, errNoStoreGeo):
		// Routed anyway — a task nobody can see is worse than a task with a
		// meaningless distance. Loud, because it means a store org-unit is
		// missing its coordinates and someone has to fix the master data.
		s.log.ErrorContext(ctx, "no active store has coordinates — task routed on the first store; fix the store's geo",
			"order", o.OrderID, "store", storeID)
	case err != nil:
		s.log.WarnContext(ctx, "no serving store for order", "order", o.OrderID)
		return
	}
	dest := storeGeo
	geoExact := at != nil
	if at != nil {
		dest = *at
	}
	items := deliveryItemsFor(o.Items)
	trialEligible := false
	for _, it := range o.Items {
		if isTrialProduct(it.ProductID) {
			trialEligible = true // 2+2 welcome trial — full-cream (gold-*) only
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
	prefs := s.resolveDeliveryPrefs(ctx, o)
	// Carry the structured door onto the task when it is certain which saved
	// address the order ships to (structuredDoorFor): a route is grouped by
	// (society, tower, floor), and a guessed tower is worse than none.
	var society, societyID, tower, unit string
	var floor *int
	if door := s.structuredDoorFor(ctx, o); door != nil {
		society, societyID, tower, unit, floor = door.Society, door.SocietyID, door.Tower, door.Unit, door.Floor
	}

	del := &delivery{
		MongoID: primitive.NewObjectID(), ID: newDeliveryID(), OrderID: o.OrderID, OrderCode: o.OrderID,
		StoreID: storeID, RiderPartyID: "", ConsumerID: o.UserID, ConsumerName: o.ConsumerName,
		PhoneMasked: maskPhone(o.Phone), Phone: o.Phone, AddressLabel: o.AddressLabel, AddressLine: o.AddressText,
		Geo: dest, GeoExact: geoExact, Items: items, Amount: o.Total, PaymentMode: payMode, TrialEligible: trialEligible, Perishable: false,
		Slot: slotLabel(o), Lane: o.Lane, EtaAt: eta, DistanceKm: round2(haversineKm(storeGeo, dest)),
		Society: society, SocietyID: societyID, Tower: tower, Floor: floor, Unit: unit,
		DeliveryPrefs: prefs, DeliveryDate: orderDeliveryDate(o),
		Status: status, OfferedAt: offeredAt, AssignedAt: now.Format(time.RFC3339), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.insertDelivery(ctx, del); err != nil {
		return
	}
	// CRM (contract C6, inert unless CRM_ENABLED): order.confirmed once the
	// task exists. Instant and subscription orders both pass through here, so
	// this is the one place the confirmation is emitted. Best-effort.
	if crmEnabled() {
		if cid, cerr := primitive.ObjectIDFromHex(o.UserID); cerr == nil {
			s.emitCRMEvent(ctx, "order.confirmed", cid, map[string]any{
				"order_id": o.OrderID, "labelled_product": crmLabelledProductOf(o),
				"promotional_only": o.OfferPack > 0, "eta": crmOrderETA(o, eta, now),
			})
		}
	}
}

// resolveDeliveryPrefs collects the doorstep instructions the RIDER and the
// STORE MANAGER act on ("don't ring the bell, baby sleeping", "hand it to the
// guard", "call before you come"). Three sources, most specific per field wins:
//
//  1. the SAVED ADDRESS's doorstep capture (receiver_name / ring_bell /
//     call_before / instructions) — where a customer actually sets this, and
//     until now the one place nobody downstream ever read;
//  2. the account's standing prefs (PATCH /me delivery_prefs);
//  3. this order's own prefs (checkout).
//
// Merged rather than first-wins, so a standing "leave with the guard" survives
// an order that only carried "call before".
func (s *service) resolveDeliveryPrefs(ctx context.Context, o *order) *deliveryPrefsDoc {
	merged := &deliveryPrefsDoc{}
	any := false
	overlay := func(p *deliveryPrefsDoc) {
		if p == nil {
			return
		}
		any = true
		if p.Handover != "" {
			merged.Handover = p.Handover
		}
		if p.Note != "" {
			merged.Note = p.Note
		}
		if p.Receiver != "" {
			merged.Receiver = p.Receiver
		}
		if p.CallBefore {
			merged.CallBefore = true
		}
		if p.RingBell != nil {
			merged.RingBell = p.RingBell
		}
	}
	// Least specific first, most specific last — the overlay lets a later source
	// win field by field. The account's STANDING preference is the general rule,
	// the saved address is about this particular door, and the order is what the
	// customer said at this checkout, so that is the order they must be applied
	// in. (They used to run address-then-account, which let a standing setting
	// overrule the instruction attached to the door being delivered to.)
	if cid, cerr := primitive.ObjectIDFromHex(o.UserID); cerr == nil {
		if acct, aerr := s.repo.findAccountByID(ctx, cid); aerr == nil && acct != nil {
			overlay(bellSaid(acct.DeliveryPrefs))
		}
		overlay(s.addressPrefs(ctx, cid, o.AddressID, o.AddressLabel))
	}
	overlay(o.DeliveryPrefs)
	if !any {
		return nil
	}
	return merged
}

// bellSaid drops a bell setting the customer never actually chose.
//
// Applied to the ACCOUNT's standing prefs only. That object hard-codes
// `ring_bell: false` (pyaas-consumer lib/deliveryPrefs.ts:18) and posts it on
// every save of any unrelated field, so a false there says nothing about the
// bell — while the ADDRESS capture and the ORDER's own prefs keep the full
// tri-state, because those are where a customer sets a door instruction and a
// deliberate "baby sleeping, do not ring" must reach the rider intact.
func bellSaid(p *deliveryPrefsDoc) *deliveryPrefsDoc {
	if p == nil || p.RingBell == nil || *p.RingBell {
		return p
	}
	clone := *p
	clone.RingBell = nil
	return &clone
}

// addressPrefs reads the doorstep capture stored on the saved address this
// order ships to (matched by label, else the default address).
// addressFor picks the saved address an order ships to: the one whose label
// matches, else the default. Shared by the doorstep prefs and the structured
// door copied onto the task.
func (s *service) addressFor(ctx context.Context, consumerID primitive.ObjectID, addressID, label string) *address {
	// The saved address's id wins when the order carries one: two rows can
	// both be called "Home", and the label alone stamped the wrong flat on
	// the task. Scoped to this consumer, so someone else's id is ignored.
	if a := s.addressByID(ctx, consumerID, addressID); a != nil {
		return a
	}
	addrs, err := s.repo.listAddresses(ctx, consumerID)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	pick := &addrs[0] // no label match and no default: the first saved address
	for i := range addrs {
		a := &addrs[i]
		if label != "" && strings.EqualFold(strings.TrimSpace(a.Label), strings.TrimSpace(label)) {
			return a
		}
		if a.IsDefault {
			pick = a
		}
	}
	return pick
}

func (s *service) addressPrefs(ctx context.Context, consumerID primitive.ObjectID, addressID, label string) *deliveryPrefsDoc {
	pick := s.addressFor(ctx, consumerID, addressID, label)
	if pick == nil || len(pick.Preferences) == 0 {
		return nil
	}
	str := func(k string) string {
		v, _ := pick.Preferences[k].(string)
		return strings.TrimSpace(v)
	}
	flag := func(k string) bool {
		v, _ := pick.Preferences[k].(bool)
		return v
	}
	// ring_bell is only an instruction when the customer actually set it —
	// including setting it to FALSE ("do not ring").
	var ring *bool
	if v, ok := pick.Preferences["ring_bell"]; ok {
		if x, isBool := v.(bool); isBool {
			ring = &x
		}
	}
	p := &deliveryPrefsDoc{
		Note:       str("instructions"),
		Receiver:   str("receiver_name"),
		CallBefore: flag("call_before"),
		RingBell:   ring,
	}
	if p.Note == "" && p.Receiver == "" && !p.CallBefore && p.RingBell == nil {
		return nil
	}
	return p
}

// addressByID resolves the saved address an order names, scoped to the
// consumer so someone else's id is ignored. Nil when absent or foreign.
func (s *service) addressByID(ctx context.Context, consumerID primitive.ObjectID, addressID string) *address {
	id := strings.TrimSpace(addressID)
	if id == "" {
		return nil
	}
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return nil
	}
	a, err := s.repo.findAddress(ctx, oid, consumerID)
	if err != nil {
		return nil
	}
	return a
}

// structuredDoorFor picks the saved address whose society/tower/floor/unit
// is copied onto the task. A door is copied only when it is CERTAIN which
// row the order ships to: mis-grouping a route is worse than not grouping
// it. Nil means "copy nothing".
//
//   - address_id names the row (the app sends the server's id): certain.
//   - otherwise the rows that could be the destination are gathered: the
//     ones whose label matches the order's, narrowed to those that compose
//     to the order's address_text ([line1, line2, city, pincode] joined, the
//     way the app builds it) when any do. The door is copied only when every
//     candidate names the SAME door (sameDoor). Two rows saved twice by a
//     lost response are identical and still group; two "Home" rows in
//     different towers yield nothing.
func (s *service) structuredDoorFor(ctx context.Context, o *order) *address {
	cid, err := primitive.ObjectIDFromHex(o.UserID)
	if err != nil {
		return nil
	}
	if a := s.addressByID(ctx, cid, o.AddressID); a != nil {
		return a
	}
	list, lerr := s.repo.listAddresses(ctx, cid)
	if lerr != nil || len(list) == 0 {
		return nil
	}
	label := strings.TrimSpace(o.AddressLabel)
	cands := make([]*address, 0, len(list))
	for i := range list {
		if label == "" || strings.EqualFold(strings.TrimSpace(list[i].Label), label) {
			cands = append(cands, &list[i])
		}
	}
	if want := strings.TrimSpace(o.AddressText); want != "" {
		byText := make([]*address, 0, len(cands))
		for _, c := range cands {
			if joinAddress(c) == want {
				byText = append(byText, c)
			}
		}
		if len(byText) > 0 {
			cands = byText
		}
	}
	if len(cands) == 0 {
		return nil
	}
	hit := cands[0]
	for _, c := range cands[1:] {
		if !sameDoor(hit, c) {
			return nil // genuinely ambiguous: different doors
		}
	}
	return hit
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
		// Duty presence (the fix the rider's offer poll reports every ~8 s) is
		// the truest live position — task pings below only win if fresher.
		if g, at, ok := s.repo.findRiderPresence(ctx, rid); ok {
			lastAt = at
			origin = g
		}
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

// ── Store manager order surgery (damage at handover) ────────────────────────

// storeCancelDelivery — the manager cancels an order BEFORE it is delivered
// (e.g. the whole crate got damaged). The task fails, the parent order flips
// cancelled — and since money only ever moves ON delivery, nothing was charged
// and nothing needs refunding. A DELIVERED order can never be cancelled.
func (s *service) storeCancelDelivery(ctx context.Context, actor auth.Actor, storeID, deliveryID, reason string) (*delivery, error) {
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
	if d.Status == "DELIVERED" {
		return nil, errConflict("ALREADY_DELIVERED", "a delivered order can no longer be cancelled")
	}
	if reason == "" {
		reason = "Cancelled by the store"
	}
	// updateDelivery stamps updated_at itself; naming it here too made Mongo
	// reject the whole $set as a path conflict, so this cancel always 500ed.
	upd, err := s.repo.updateDelivery(ctx, deliveryID,
		bson.D{
			{Key: "status", Value: "FAILED"},
			{Key: "failure_reason", Value: reason},
		},
		bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED"}}}}},
	)
	if err != nil {
		return nil, err
	}
	// The consumer sees the cancellation immediately (their Orders screen).
	s.syncOrderFailed(ctx, d, reason)
	return upd, nil
}

// itemAdjust is one requested line change — the ABSOLUTE new quantity,
// reduction only (a store can shrink a bill, never inflate one).
type itemAdjust struct {
	// ProductID identifies the exact line. Name alone cannot: Gold 500 ml and
	// Gold 1 L share one product name and differ only by variant, so a
	// name-keyed adjustment silently rewrote BOTH lines and re-billed the
	// customer for a pack they still have. Clients that send it get exact
	// matching; older builds fall back to the name, as before.
	ProductID string `json:"product_id"`
	Variant   string `json:"variant"`
	Name      string `json:"name"`
	Qty       int    `json:"qty"`
}

// adjustKey identifies one order line for the at-handover adjustment. Product
// id when the client sent one, else name+variant, else the bare name.
func adjustKey(productID, name, variant string) string {
	if p := strings.TrimSpace(productID); p != "" {
		return "id:" + p
	}
	if v := strings.TrimSpace(variant); v != "" {
		return "nv:" + strings.ToLower(strings.TrimSpace(name)) + "|" + strings.ToLower(v)
	}
	return "n:" + strings.ToLower(strings.TrimSpace(name))
}

// storeAdjustDelivery — the manager reduces item quantities before handover
// (3 packets ordered, 1 damaged → deliver 2; qty 0 removes the line). The
// ORDER and its TASK are re-billed server-side (subtotal, delivery fee and
// total recomputed under the same rules as order creation), and because the
// wallet debit happens AT DELIVERY from the task's amount, the customer pays
// exactly for what is actually delivered. COD collects the updated amount;
// the morning Taaza trial maths also read the updated amount — no flow forks.
func (s *service) storeAdjustDelivery(ctx context.Context, actor auth.Actor, storeID, deliveryID string, changes []itemAdjust) (*delivery, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, errBadRequest("no item changes given")
	}
	d, err := s.repo.findDeliveryByID(ctx, deliveryID)
	if err != nil {
		return nil, err
	}
	if d.StoreID != storeID {
		return nil, errForbidden("delivery belongs to another store")
	}
	if d.Status == "DELIVERED" || d.Status == "FAILED" {
		return nil, errConflict("NOT_ADJUSTABLE", "only an open order can be adjusted")
	}
	o, err := s.repo.findOrderAnyUser(ctx, d.OrderID)
	if err != nil {
		return nil, err
	}
	if o.Status == "delivered" || o.Status == "cancelled" {
		return nil, errConflict("NOT_ADJUSTABLE", "only an open order can be adjusted")
	}
	want := map[string]int{}
	for _, c := range changes {
		want[adjustKey(c.ProductID, c.Name, c.Variant)] = c.Qty
	}
	newItems := make([]orderItem, 0, len(o.Items))
	var removed []orderItem // lines taken off entirely: D-05 tells the member
	changed := false
	for _, it := range o.Items {
		q, ok := want[adjustKey(it.ProductID, it.Name, it.Variant)]
		if !ok {
			// An older client sent a bare name: match on that, as it always did.
			q, ok = want[adjustKey("", it.Name, "")]
		}
		if !ok {
			newItems = append(newItems, it)
			continue
		}
		if q < 0 || q > it.Qty {
			return nil, errUnprocessable("REDUCE_ONLY", "an adjustment can only reduce a quantity, never raise it")
		}
		if q != it.Qty {
			changed = true
		}
		if q == 0 {
			removed = append(removed, it)
			continue // damaged out entirely — the line is removed
		}
		it.Qty = q
		newItems = append(newItems, it)
	}
	if !changed {
		return d, nil // idempotent no-op (absolute quantities)
	}
	if len(newItems) == 0 {
		return nil, errUnprocessable("USE_CANCEL", "removing every item cancels the order — use cancel instead")
	}
	var subtotal float64
	for _, it := range newItems {
		subtotal += it.Price * float64(it.Qty)
	}
	subtotal = round2(subtotal)
	fee := deliveryFeeFor(subtotal)
	total := round2(subtotal + fee + o.MonsoonFee)
	now := time.Now().UTC()
	if _, uerr := s.repo.orders.UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: o.OrderID}, {Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"delivered", "cancelled"}}}}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "order_items", Value: newItems},
			{Key: "subtotal", Value: subtotal},
			{Key: "delivery_fee", Value: fee},
			{Key: "total", Value: total},
			{Key: "updated_at", Value: now},
		}}}); uerr != nil {
		return nil, errInternal("order adjust failed")
	}
	// CRM order.line_cancelled (D-05), one event per removed line. Money
	// has not moved (settle-on-delivery), so amount is what comes off the
	// bill, not a refund; before_delivery is always true on this path.
	if crmEnabled() {
		if cid, cerr := primitive.ObjectIDFromHex(o.UserID); cerr == nil {
			for _, it := range removed {
				line := &order{Items: []orderItem{it}}
				s.emitCRMEvent(ctx, "order.line_cancelled", cid, map[string]any{
					"order_id": o.OrderID, "line_id": it.ID, "labelled_product": crmLabelledProductOf(line),
					"amount": round2(it.Price * float64(it.Qty)), "before_delivery": true,
					"scope_key": o.OrderID + ":" + it.ID,
				})
			}
		}
	}
	dItems := deliveryItemsFor(newItems)
	// updateDelivery stamps updated_at itself; naming it here too made Mongo
	// reject the whole $set as a path conflict (the same fault
	// storeCancelDelivery had), so the order was re-billed while the task
	// kept its old lines and amount.
	return s.repo.updateDelivery(ctx, deliveryID,
		bson.D{{Key: "items", Value: dItems}, {Key: "amount", Value: total}},
		bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED", "FAILED"}}}}},
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
func (s *service) offeredForRider(ctx context.Context, actor auth.Actor, live *geoPt) ([]delivery, error) {
	stores, err := s.repo.storesForRider(ctx, actor.PartyID)
	if err != nil {
		return nil, err
	}
	// The rider app sends its CURRENT fix with every offer poll — the position
	// at the very moment the offer would be shown. Record it as the rider's
	// duty presence so the manager's assign ranking sees it too.
	if live != nil {
		s.repo.upsertRiderPresence(ctx, actor.PartyID, *live, time.Now())
	}
	offered, err := s.repo.listOfferedForStores(ctx, stores, actor.PartyID)
	if err != nil {
		return nil, err
	}
	// Rider origin, best first: (1) the live fix on THIS poll; (2) their last
	// duty presence, if FRESH; (3) the freshest FRESH GPS ping across their own
	// tasks. A stale/stuck fix (older than the freshness window) is discarded —
	// it may be yesterday's position, so it must never include OR exclude a
	// rider from an offer. Nothing fresh → the rider stages AT their store (the
	// duty station), the same positioning rule the manager's assign sheet uses.
	cutoff := time.Now().UTC().Add(-offerPingFreshness).Format(time.RFC3339)
	origin := live
	if origin == nil {
		if g, at, ok := s.repo.findRiderPresence(ctx, actor.PartyID); ok && at >= cutoff {
			origin = &g
		}
	}
	if origin == nil {
		mine, _ := s.repo.listDeliveriesByRider(ctx, actor.PartyID)
		lastAt := ""
		for i := range mine {
			d := &mine[i]
			if d.LastKnownGeo != nil && d.LastLocationAt > lastAt && d.LastLocationAt >= cutoff {
				lastAt = d.LastLocationAt
				origin = d.LastKnownGeo
			}
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
	// Snapshot WHERE the stock was collected, at the moment it was collected.
	// The field pair exists on the task and the admin CRM renders a "pickup"
	// column from it, but nothing ever wrote it, so the column was permanently
	// blank. Recording it here (rather than reading the live setting later) is
	// what makes it an audit trail: moving the pickup point tomorrow must not
	// rewrite where yesterday's milk actually came from.
	p := s.pickupPoint(ctx)
	set := bson.D{
		{Key: "status", Value: "OUT_FOR_DELIVERY"},
		{Key: "out_for_delivery_at", Value: now},
		{Key: "pickup_address", Value: p.Address},
		{Key: "pickup_geo", Value: geoPt{Lat: p.Lat, Lng: p.Lng}},
	}
	d, err := s.riderTransition(ctx, actor, id, "ACCEPTED", set, "")
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
	d, err := s.riderTransition(ctx, actor, id, "", // any non-delivered state
		bson.D{{Key: "status", Value: "FAILED"}, {Key: "failure_reason", Value: reason}}, "DELIVERED")
	if err != nil {
		return nil, err
	}
	s.syncOrderFailed(ctx, d, reason) // the customer's order must not sit out_for_delivery forever
	return d, nil
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
	// resurrect a cancelled order to delivered). The parent is kept for the
	// trial gate below (trial pricing applies only to SUBSCRIPTION deliveries).
	parent, perr := s.repo.findOrderAnyUser(ctx, d.OrderID)
	if perr != nil {
		// A transient lookup failure must FAIL the settle, not silently skip the
		// cancelled-order guard and the trial gate below — skipping the trial
		// gate bills a FREE day at full price. The rider app retries.
		return nil, perr
	}
	if parent != nil && parent.Status == "cancelled" {
		_, _ = s.repo.updateDelivery(ctx, d.ID,
			bson.D{{Key: "status", Value: "FAILED"}, {Key: "failure_reason", Value: "Order cancelled by the customer"}},
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
	// The phone's own fence verdict only counts when the task points at the
	// customer's real pin: with a store fallback the rider at the real door is
	// legitimately far from Geo and would be refused forever.
	if d.GeoExact && !in.GeofenceOK {
		return nil, errUnprocessable("GEOFENCE_FAILED", "you are not at the delivery address — move closer to confirm")
	}
	// ...and MEASURE it here rather than believing the flag. Until now the fence
	// was asserted entirely by the phone: the server stored proof_distance_m but
	// never looked at it, so a client that sent geofence_ok:true — including the
	// app's own "distance unknown" case — was delivered anywhere in the world.
	// Only enforced when the task carries the CUSTOMER's own pin (GeoExact):
	// where the order had no coordinates the task falls back to the store's, and
	// a rider standing at the real door is legitimately kilometres from that.
	if d.GeoExact && in.Geo != nil && geoSane(d.Geo.Lat, d.Geo.Lng) {
		if m := haversineKm(*in.Geo, d.Geo) * 1000; m > deliveryGeofenceMeters {
			return nil, errUnprocessable("GEOFENCE_FAILED",
				fmt.Sprintf("you are %.0f m from the delivery address — move closer to confirm", m))
		}
	}
	// Debit-on-delivery BEFORE flipping status (funds gate). PREPAID only; keyed
	// to the order so the consumer's settle sweep can never double-charge.
	// PREPAID at ANY amount: an Amount==0 task (a Welcome Litre promotional
	// pack) must still consume the delivery ref with a ₹0 gate row, or the
	// exactly-once key is left dangling forever. Amount>0 flows are unchanged.
	if d.PaymentMode == "PREPAID" {
		cid, cerr := primitive.ObjectIDFromHex(d.ConsumerID)
		if cerr != nil {
			return nil, errInternal("bad consumer id on delivery")
		}
		amount := d.Amount
		// FULL-CREAM SUBSCRIPTION deliveries (the morning run) flow through the
		// "2 FREE then 2 PAID" welcome trial: the first 2 delivered days are on
		// us (effective 0), the next 2 pay full, then normal. The window counts
		// DELIVERED days (dated in IST) and is idempotent by this delivered-day key,
		// so a settle re-run never double-advances it. Gates: the SKU must be the
		// offer's full cream (TrialEligible ← isTrialProduct, gold-*) AND the
		// parent order must be SUBSCRIPTION-linked — a one-time morning order of
		// the same milk never consumes or earns trial days.
		if d.Amount > 0 && d.Lane == "morning" && d.TrialEligible && parent != nil && parent.SubscriptionID != "" {
			day := trialDay(time.Now())
			eff, _, terr := s.trialChargeFor(ctx, cid, trialDeliveryKey(cid, day), d.Amount)
			if terr != nil {
				return nil, terr
			}
			amount = eff
		}
		if amount > 0 {
			if _, e := s.debit(ctx, cid, amount, "delivery:"+d.OrderID, "Delivery "+d.OrderCode); e != nil {
				return nil, e // INSUFFICIENT_FUNDS etc. surface to the rider app
			}
			// CH-19: the first order with a SETTLED value > 0 marks the
			// customer as paid — never a promotional-only ₹0 settle.
			s.markHasPaidOrder(ctx, cid)
		} else {
			// FREE trial day: no money moves, but the delivery ref MUST still be
			// consumed with a zero-amount ledger row. The consumer app runs a
			// settle sweep that POSTs /wallet/debit with the order's STICKER total
			// for any delivered order whose ref it cannot see — before this row
			// existed, every backend-free day was silently back-charged at full
			// price by the member's own app. The gate row makes any later debit
			// on this ref dedupe into a no-op, and gives the member a ₹0 "free
			// day" line in their ledger to boot.
			remark := "Delivery " + d.OrderCode + " (trial free day)" // pre-CRM wording, unchanged
			if parent != nil && parent.OfferPack > 0 {
				remark = "Delivery " + d.OrderCode + " (free delivery)" // Welcome Litre pack
			}
			gate := walletTxn{
				ID: primitive.NewObjectID(), ConsumerID: cid,
				Type: "DEBIT", Bucket: "CASH", Amount: 0, RefType: "order",
				RefID: "delivery:" + d.OrderID, Status: "SUCCESS",
				Remark: remark, CreatedAt: time.Now().UTC(),
			}
			if _, e := s.repo.insertWalletTxnGate(ctx, gate); e != nil {
				return nil, e // must not deliver without consuming the ref
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
	set := bson.D{
		{Key: "status", Value: "DELIVERED"}, {Key: "delivered_at", Value: now},
		{Key: "proof_note", Value: note}, {Key: "proof_photo_uri", Value: in.ProofPhoto},
		{Key: "delivery_event_id", Value: evt}, {Key: "geofence_ok", Value: true},
	}
	if in.Geo != nil {
		set = append(set,
			bson.E{Key: "proof_geo", Value: in.Geo}, bson.E{Key: "last_known_geo", Value: in.Geo},
			bson.E{Key: "last_location_at", Value: now})
		if geoSane(d.Geo.Lat, d.Geo.Lng) {
			set = append(set, bson.E{Key: "proof_distance_m", Value: math.Round(haversineKm(*in.Geo, d.Geo) * 1000)})
		}
	}
	updated, err := s.repo.updateDelivery(ctx, id, set, bson.D{{Key: "status", Value: "OUT_FOR_DELIVERY"}})
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
	// CRM (contract C6, inert unless CRM_ENABLED): order.dispatched. Best-effort.
	// promotional_only rides along so D-02's condition can read it (a free
	// pack out for delivery sends no D-02).
	if crmEnabled() {
		if cid, cerr := primitive.ObjectIDFromHex(d.ConsumerID); cerr == nil {
			var o *order
			if found, err := s.repo.findOrderAnyUser(ctx, d.OrderID); err == nil {
				o = found
			}
			s.emitCRMEvent(ctx, "order.dispatched", cid, map[string]any{
				"order_id": d.OrderID, "labelled_product": crmLabelledProductOf(o), "promotional_only": o != nil && o.OfferPack > 0,
				"partner": crmPartnerName(name, d.RiderPartyID), "eta_min": crmDeliveryETAMinutes(d, time.Now()),
			})
		}
	}
}

func (s *service) syncOrderRiderLocation(ctx context.Context, d *delivery, lat, lng float64) {
	_, _ = s.repo.orders.UpdateOne(ctx, bson.D{{Key: "order_id", Value: d.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "riders.current_lat", Value: lat}, {Key: "riders.current_lng", Value: lng}}}})
}

func (s *service) syncOrderDelivered(ctx context.Context, d *delivery) {
	_, _ = s.repo.orders.UpdateOne(ctx, bson.D{{Key: "order_id", Value: d.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "delivered"}, {Key: "can_review", Value: true},
			{Key: "proof_photo_url", Value: d.ProofPhotoURI}, {Key: "delivered_at", Value: d.DeliveredAt},
			{Key: "updated_at", Value: time.Now().UTC()}}}})
	// CRM (inert unless CRM_ENABLED): order.delivered for EVERY order
	// (contract C6). offer_pack still rides along so a delivered Welcome Litre
	// pack advances the offer state machine (crmOnPackDelivered); the generic
	// router fires D-06 for ordinary orders only. Best-effort by contract.
	if crmEnabled() {
		if o, err := s.repo.findOrderAnyUser(ctx, d.OrderID); err == nil && o != nil {
			if cid, cerr := primitive.ObjectIDFromHex(o.UserID); cerr == nil {
				s.emitCRMEvent(ctx, "order.delivered", cid, map[string]any{
					"order_id": o.OrderID, "offer_pack": o.OfferPack,
					"promotional_only": o.OfferPack > 0, "labelled_product": crmLabelledProductOf(o),
				})
			}
		}
	}
}

// syncOrderFailed leaves the consumer order in a state the shipped app can
// render when the task FAILS (the rider could not deliver) or the store cancels
// it. The app's status union ends at "cancelled" and treats it as terminal, so
// that is what the order becomes; no status the deployed app cannot draw is
// invented. Guarded: a delivered or already-cancelled order is never touched,
// and only an order this call actually flipped emits order.failed (contract
// C6; the emit is inert unless CRM_ENABLED). A later re-assign of the FAILED
// task walks the order forward again through the normal pickup sync.
func (s *service) syncOrderFailed(ctx context.Context, d *delivery, reason string) {
	// cancelled_by marks that the TASK cancelled this order, so a rider's undo
	// of the FAILED marking can walk it back; a customer's own cancel never
	// carries it and is never resurrected.
	res, err := s.repo.orders.UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: d.OrderID}, {Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"delivered", "cancelled"}}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}, {Key: "cancelled_by", Value: orderCancelledByDelivery}, {Key: "updated_at", Value: time.Now().UTC()}}}})
	if err != nil || res.ModifiedCount == 0 || !crmEnabled() {
		return
	}
	if cid, cerr := primitive.ObjectIDFromHex(d.ConsumerID); cerr == nil {
		// reason is worded for the member: the rider's evidence trail
		// ("| photo=... | geo=...") never leaves the task record.
		s.emitCRMEvent(ctx, "order.failed", cid, map[string]any{
			"order_id": d.OrderID, "labelled_product": s.crmLabelledProduct(ctx, d.OrderID),
			"reason": crmFailureReasonForCustomer(reason),
		})
	}
}

// syncOrderFailedUndone is the undo of syncOrderFailed: when the rider undoes
// a FAILED marking inside the undo window, an order the task itself cancelled
// goes back to out_for_delivery. Keyed on cancelled_by so a customer's cancel
// is never undone from the rider's phone.
func (s *service) syncOrderFailedUndone(ctx context.Context, d *delivery) {
	_, _ = s.repo.orders.UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: d.OrderID}, {Key: "status", Value: "cancelled"}, {Key: "cancelled_by", Value: orderCancelledByDelivery}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: "out_for_delivery"}, {Key: "updated_at", Value: time.Now().UTC()}}},
			{Key: "$unset", Value: bson.D{{Key: "cancelled_by", Value: ""}}},
		})
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

// storeCancelDelivery — POST /stores/{storeId}/orders/{deliveryId}/cancel.
func (h *handler) storeCancelDelivery(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		Reason string `json:"reason"`
	}
	_ = decode(r, &body)
	d, err := h.svc.storeCancelDelivery(r.Context(), actor, chi.URLParam(r, "storeId"), chi.URLParam(r, "deliveryId"), body.Reason)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// storeAdjustDelivery — PATCH /stores/{storeId}/orders/{deliveryId}/items.
func (h *handler) storeAdjustDelivery(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		Items []itemAdjust `json:"items"`
	}
	_ = decode(r, &body)
	d, err := h.svc.storeAdjustDelivery(r.Context(), actor, chi.URLParam(r, "storeId"), chi.URLParam(r, "deliveryId"), body.Items)
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
// rider's accept-popup polls. The app sends its current GPS as ?lat=&lng= so the
// 15-km fence judges the rider's position AT THIS MOMENT (and the fix is stored
// as their duty presence for the manager's ranking).
func (h *handler) riderAvailable(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var live *geoPt
	if latS, lngS := r.URL.Query().Get("lat"), r.URL.Query().Get("lng"); latS != "" && lngS != "" {
		lat, e1 := strconv.ParseFloat(latS, 64)
		lng, e2 := strconv.ParseFloat(lngS, 64)
		// (0,0) is the null island a failed device fix reports — never a rider.
		if e1 == nil && e2 == nil && !(lat == 0 && lng == 0) {
			live = &geoPt{Lat: lat, Lng: lng}
		}
	}
	ds, err := h.svc.offeredForRider(r.Context(), actor, live)
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

// slotLabel is what the store/rider console sees on the task: a scheduled
// morning order carries its member-picked IST date in front of the window so
// a +N-days order is never worked as "due tomorrow".
func slotLabel(o *order) string {
	if o.DeliveryDate != "" {
		if o.DeliveryWindow != "" {
			return o.DeliveryDate + " · " + o.DeliveryWindow
		}
		return o.DeliveryDate
	}
	return o.DeliveryWindow
}

// sameDoor reports whether two saved addresses name the same physical door.
// A nil floor and a set floor are different answers, so the comparison is on
// the pointer's VALUE-or-absence, not on the pointer itself.
func sameDoor(a, b *address) bool {
	if a.SocietyID != b.SocietyID || a.Tower != b.Tower || a.Unit != b.Unit {
		return false
	}
	switch {
	case a.Floor == nil && b.Floor == nil:
		return true
	case a.Floor == nil || b.Floor == nil:
		return false
	default:
		return *a.Floor == *b.Floor
	}
}

package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SERVER-OWNED SUBSCRIPTIONS — the backend twin of the consumer app's local
// subscription rows (lib/subscriptions.ts), so the DAILY MORNING ORDER exists
// even when nobody opens the app.
//
// Model (mirrors the FE exactly):
//   one subscription = ONE product line (product, variant, qty, unit price) +
//   a cadence (daily | alternate | weekly) anchored at start_date, minus any
//   vacation ranges (a skip is a one-day vacation, start == end).
//
// MONEY MODEL — no charge at creation. The worker only CREATES the order
// (payment_method "wallet"); money moves exactly once ON DELIVERY through the
// existing settle path (deliverDelivery → debit "delivery:<orderID>"), which
// also applies the Taaza 2-paid-2-free welcome trial. Charging here as well
// would double-debit — the AutoPay mandate (mandate.go) remains the wallet
// top-up backstop, not the per-delivery charge.
//
// ADDRESS MODEL — a subscription NEVER carries its own address. Every order the
// worker creates resolves the consumer's CURRENT default saved address (with
// lat/lng, so geofence routes it to the serving store). One saved address feeds
// every subscription — new subscriptions never re-ask for a location, and an
// address edit re-points every future delivery automatically.
//
// EXACTLY-ONCE — at most one order per (subscription, IST day), enforced by an
// atomic last_order_date claim (UpdateOne guarded on $ne day), the same pattern
// as advanceMandateCharge. A crashed tick after the claim skips that day rather
// than ever double-ordering milk.

const collSubscriptions = "consumer_subscriptions"

// subscriptionDeliveryFee is the delivery fee on a SUBSCRIPTION morning delivery:
// always 0. The consumer app quotes and debits the bare subtotal on the Subscribe
// button, so the server-side recurring debit must match (never subtotal + ₹15).
const subscriptionDeliveryFee = 0.0

// NOTE: cadence math and day-claims use IST (istZone, shared with trial.go),
// matching the FE's local calendar dates, never UTC (UTC is "yesterday" until
// 05:30 IST).

// subscriptionFrequencies is the supported cadence set (FE Frequency minus the
// purely-local one_time/custom modes, which the app fulfils immediately itself).
var subscriptionFrequencies = map[string]bool{"daily": true, "alternate": true, "weekly": true}

// subscriptionTransitions — same shape as the mandate state machine.
var subscriptionTransitions = map[string]map[string]bool{
	"active":    {"paused": true, "cancelled": true},
	"paused":    {"active": true, "cancelled": true},
	"cancelled": {},
}

func subscriptionActionTarget(action string) (string, bool) {
	switch action {
	case "pause":
		return "paused", true
	case "resume":
		return "active", true
	case "cancel":
		return "cancelled", true
	default:
		return "", false
	}
}

// ── Documents + wire shapes (json mirrors the FE Subscription fields) ────────

type vacationRange struct {
	Start string `bson:"start" json:"start"` // YYYY-MM-DD inclusive
	End   string `bson:"end"   json:"end"`   // YYYY-MM-DD inclusive
}

type subscription struct {
	MongoID        primitive.ObjectID `bson:"_id,omitempty"            json:"-"`
	SubscriptionID string             `bson:"subscription_id"          json:"id"` // public "sub_…"
	ConsumerID     primitive.ObjectID `bson:"consumer_id"              json:"-"`
	ProductID      string             `bson:"product_id"               json:"product_id"`
	Name           string             `bson:"name"                     json:"name"`
	Variant        string             `bson:"variant,omitempty"        json:"variant"`
	Qty            int                `bson:"qty"                      json:"qty"`
	UnitPrice      float64            `bson:"unit_price"               json:"unit_price"`
	Frequency      string             `bson:"frequency"                json:"frequency"` // daily|alternate|weekly
	DeliverySlot   string             `bson:"delivery_slot,omitempty"  json:"delivery_slot,omitempty"`
	Status         string             `bson:"status"                   json:"status"`     // active|paused|cancelled
	StartDate      string             `bson:"start_date"               json:"start_date"` // YYYY-MM-DD (IST)
	Vacations      []vacationRange    `bson:"vacations,omitempty"      json:"vacations,omitempty"`
	// LastOrderDate is the exactly-once day claim (YYYY-MM-DD IST) — the worker
	// creates at most one order per subscription per day.
	LastOrderDate string    `bson:"last_order_date,omitempty" json:"last_order_date,omitempty"`
	LastOrderID   string    `bson:"last_order_id,omitempty"   json:"last_order_id,omitempty"`
	CreatedAt     time.Time `bson:"created_at"                json:"created_at"`
	UpdatedAt     time.Time `bson:"updated_at"                json:"updated_at"`
}

func newSubscriptionID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "sub_" + hex.EncodeToString(b)
}

// istToday returns the current IST calendar day as YYYY-MM-DD.
func istToday(now time.Time) string { return now.In(istZone).Format("2006-01-02") }

// parseDay parses a YYYY-MM-DD into an IST-midnight time (cadence anchor).
func parseDay(iso string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", iso, istZone)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// subscriptionDeliversOn — the cadence law, IDENTICAL to the FE
// (lib/subscriptions.ts): daily every day, alternate every 2nd day, weekly
// every 7th, all counted from start_date. Days before the start never deliver.
func subscriptionDeliversOn(frequency, startDate, dayISO string) bool {
	from, ok1 := parseDay(startDate)
	to, ok2 := parseDay(dayISO)
	if !ok1 || !ok2 {
		return false
	}
	d := int(to.Sub(from).Hours() / 24)
	if d < 0 {
		return false
	}
	switch frequency {
	case "daily":
		return true
	case "alternate":
		return d%2 == 0
	case "weekly":
		return d%7 == 0
	default:
		return false
	}
}

// subscriptionDueOn — cadence AND not inside any vacation/skip range (dates are
// YYYY-MM-DD so string compare is chronological; the FE rule verbatim).
func subscriptionDueOn(sub *subscription, dayISO string) bool {
	if sub.Status != "active" || !subscriptionDeliversOn(sub.Frequency, sub.StartDate, dayISO) {
		return false
	}
	for _, v := range sub.Vacations {
		if dayISO >= v.Start && dayISO <= v.End {
			return false
		}
	}
	return true
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) ensureSubscriptionIndexes(ctx context.Context) error {
	specs := []mongo.IndexModel{
		{Keys: bson.D{{Key: "subscription_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}}}, // the worker scans active subs
	}
	_, err := r.subscriptions.Indexes().CreateMany(ctx, specs)
	return err
}

func (r *repository) insertSubscription(ctx context.Context, s *subscription) error {
	if _, err := r.subscriptions.InsertOne(ctx, s); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("SUBSCRIPTION_EXISTS", "subscription already exists")
		}
		return errInternal("subscription create failed")
	}
	return nil
}

func (r *repository) findSubscription(ctx context.Context, subID string, consumerID primitive.ObjectID) (*subscription, error) {
	var s subscription
	err := r.subscriptions.FindOne(ctx, bson.D{{Key: "subscription_id", Value: subID}, {Key: "consumer_id", Value: consumerID}}).Decode(&s)
	if isNoDocs(err) {
		return nil, errNotFound("subscription not found")
	}
	if err != nil {
		return nil, errInternal("subscription lookup failed")
	}
	return &s, nil
}

func (r *repository) listSubscriptions(ctx context.Context, consumerID primitive.ObjectID) ([]subscription, error) {
	cur, err := r.subscriptions.Find(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}, {Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100))
	if err != nil {
		return nil, errInternal("subscriptions lookup failed")
	}
	out := []subscription{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("subscriptions decode failed")
	}
	return out, nil
}

// listActiveSubscriptions — the worker's scan (all consumers, active only).
func (r *repository) listActiveSubscriptions(ctx context.Context) ([]subscription, error) {
	cur, err := r.subscriptions.Find(ctx, bson.D{{Key: "status", Value: "active"}},
		options.Find().SetLimit(5000))
	if err != nil {
		return nil, errInternal("subscriptions scan failed")
	}
	out := []subscription{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("subscriptions scan decode failed")
	}
	return out, nil
}

func (r *repository) updateSubscription(ctx context.Context, subID string, consumerID primitive.ObjectID, set bson.D, guard bson.D) (*subscription, error) {
	filter := bson.D{{Key: "subscription_id", Value: subID}, {Key: "consumer_id", Value: consumerID}}
	filter = append(filter, guard...)
	set = append(set, bson.E{Key: "updated_at", Value: time.Now().UTC()})
	after := options.After
	var s subscription
	err := r.subscriptions.FindOneAndUpdate(ctx, filter,
		bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&s)
	if isNoDocs(err) {
		return nil, errConflict("SUBSCRIPTION_STATE", "subscription is not in the expected state")
	}
	if err != nil {
		return nil, errInternal("subscription update failed")
	}
	return &s, nil
}

// claimSubscriptionDay atomically claims (subscription, day) for order creation:
// the guard last_order_date != day admits exactly ONE winner per IST day —
// the same exactly-once shape as advanceMandateCharge. Returns whether THIS
// call won the claim.
func (r *repository) claimSubscriptionDay(ctx context.Context, subID, day string) (bool, error) {
	res, err := r.subscriptions.UpdateOne(ctx,
		bson.D{
			{Key: "subscription_id", Value: subID},
			{Key: "status", Value: "active"},
			{Key: "last_order_date", Value: bson.D{{Key: "$ne", Value: day}}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "last_order_date", Value: day},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
	if err != nil {
		return false, errInternal("subscription day claim failed")
	}
	return res.ModifiedCount == 1, nil
}

func (r *repository) setSubscriptionOrder(ctx context.Context, subID, orderID string) {
	_, _ = r.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: subID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "last_order_id", Value: orderID}}}})
}

// unclaimSubscriptionDay releases a day claim (guarded on the exact day) — used
// when the RECONCILER cancels a scheduled upcoming order because the plan
// changed (pause/vacation), so a resume before the midnight cutoff can schedule
// that day again. A shopper's DIRECT order cancel deliberately keeps the claim:
// that day stays skipped.
func (r *repository) unclaimSubscriptionDay(ctx context.Context, subID, day string) {
	_, _ = r.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: subID}, {Key: "last_order_date", Value: day}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "last_order_date", Value: ""},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
}

// findSubscriptionByID — UNSCOPED lookup for the worker (any consumer's sub).
// Returns (nil, nil) when absent. Never exposed on a route.
func (r *repository) findSubscriptionByID(ctx context.Context, subID string) (*subscription, error) {
	var s subscription
	err := r.subscriptions.FindOne(ctx, bson.D{{Key: "subscription_id", Value: subID}}).Decode(&s)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("subscription lookup failed")
	}
	return &s, nil
}

// listUnlockedSubOrders — SCHEDULED (pre-lock) subscription orders matching a
// scheduled_for filter (an exact day, or a range like {$lt: today}). Locked or
// cancelled orders never match.
func (r *repository) listUnlockedSubOrders(ctx context.Context, dayFilter any) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "subscription_id", Value: bson.D{{Key: "$gt", Value: ""}}},
		{Key: "scheduled_for", Value: dayFilter},
		{Key: "status", Value: "placed"},
		{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}, options.Find().SetLimit(5000))
	if err != nil {
		return nil, errInternal("scheduled orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("scheduled orders decode failed")
	}
	return out, nil
}

// ── Service ─────────────────────────────────────────────────────────────────

type subscriptionInput struct {
	ProductID    string          `json:"product_id"`
	Name         string          `json:"name"`
	Variant      string          `json:"variant"`
	Qty          int             `json:"qty"`
	UnitPrice    float64         `json:"unit_price"`
	Frequency    string          `json:"frequency"`
	DeliverySlot string          `json:"delivery_slot"`
	StartDate    string          `json:"start_date"` // YYYY-MM-DD (IST)
	Vacations    []vacationRange `json:"vacations"`
}

func (s *service) createSubscription(ctx context.Context, consumerID primitive.ObjectID, in subscriptionInput) (*subscription, error) {
	if in.ProductID == "" {
		return nil, errBadRequest("product_id is required")
	}
	if in.Qty <= 0 || in.Qty > maxQtyPerProduct {
		return nil, errBadRequest("qty must be between 1 and 10")
	}
	if !subscriptionFrequencies[in.Frequency] {
		return nil, errBadRequest("frequency must be one of: daily, alternate, weekly")
	}
	// The unit price is resolved SERVER-SIDE from the catalog (catalog_price.go)
	// — the client-sent unit_price is ignored, so a tampered app cannot start a
	// ₹0 (or inflated) daily plan. Unknown/hidden products are rejected.
	priceIx, err := s.loadPriceIndex(ctx)
	if err != nil {
		return nil, err
	}
	unitPrice, ok := priceIx.priceFor(in.ProductID, in.Variant)
	if !ok {
		return nil, errBadRequest("unknown product: " + in.ProductID)
	}
	now := time.Now().UTC()
	start := in.StartDate
	if start == "" {
		start = istToday(now)
	}
	if _, ok := parseDay(start); !ok {
		return nil, errBadRequest("start_date must be YYYY-MM-DD")
	}
	// HARD BACKSTOP (mirrors the FE's NEEDS_EXACT_LOCATION): a subscription may
	// never exist without a saved delivery point with coordinates — the morning
	// order must always route to a real door and its serving store.
	if _, aerr := s.subscriptionAddress(ctx, consumerID); aerr != nil {
		return nil, aerr
	}
	name := in.Name
	if name == "" {
		name = in.ProductID
	}
	sub := &subscription{
		MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(), ConsumerID: consumerID,
		ProductID: in.ProductID, Name: name, Variant: in.Variant, Qty: in.Qty, UnitPrice: round2(unitPrice),
		Frequency: in.Frequency, DeliverySlot: in.DeliverySlot, Status: "active", StartDate: start,
		Vacations: in.Vacations, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.insertSubscription(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// subscriptionAddress resolves the consumer's delivery point for subscription
// orders: the DEFAULT saved address (else the first) that carries lat/lng. One
// address serves every subscription — the app never asks again per subscription.
func (s *service) subscriptionAddress(ctx context.Context, consumerID primitive.ObjectID) (*address, error) {
	addrs, err := s.repo.listAddresses(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	var pick *address
	for i := range addrs {
		a := &addrs[i]
		if a.Lat == nil || a.Lng == nil {
			continue
		}
		if a.IsDefault {
			pick = a
			break
		}
		if pick == nil {
			pick = a
		}
	}
	if pick == nil {
		return nil, errUnprocessable("ADDRESS_REQUIRED", "save a delivery address (with its map location) before subscribing")
	}
	return pick, nil
}

func (s *service) listSubscriptionsFor(ctx context.Context, consumerID primitive.ObjectID) ([]subscription, error) {
	return s.repo.listSubscriptions(ctx, consumerID)
}

// setSubscriptionStatus — pause / resume / cancel with the state machine.
func (s *service) setSubscriptionStatus(ctx context.Context, consumerID primitive.ObjectID, subID, action string) (*subscription, error) {
	target, ok := subscriptionActionTarget(action)
	if !ok {
		return nil, errBadRequest("unknown subscription action")
	}
	sub, err := s.repo.findSubscription(ctx, subID, consumerID)
	if err != nil {
		return nil, err
	}
	if sub.Status == target {
		return sub, nil // idempotent
	}
	if !subscriptionTransitions[sub.Status][target] {
		return nil, errConflict("SUBSCRIPTION_STATE", fmt.Sprintf("cannot %s a %s subscription", action, sub.Status))
	}
	return s.repo.updateSubscription(ctx, subID, consumerID,
		bson.D{{Key: "status", Value: target}},
		bson.D{{Key: "status", Value: sub.Status}})
}

// patchSubscription edits the live plan (qty / frequency / slot / re-anchored
// start date / vacation ranges) — the FE's updateSubscription + reactivate +
// vacation mirror rides through here.
func (s *service) patchSubscription(ctx context.Context, consumerID primitive.ObjectID, subID string, in struct {
	Qty          *int             `json:"qty"`
	Frequency    *string          `json:"frequency"`
	DeliverySlot *string          `json:"delivery_slot"`
	StartDate    *string          `json:"start_date"`
	Vacations    *[]vacationRange `json:"vacations"`
}) (*subscription, error) {
	set := bson.D{}
	if in.Qty != nil {
		if *in.Qty <= 0 || *in.Qty > maxQtyPerProduct {
			return nil, errBadRequest("qty must be between 1 and 10")
		}
		set = append(set, bson.E{Key: "qty", Value: *in.Qty})
	}
	if in.Frequency != nil {
		if !subscriptionFrequencies[*in.Frequency] {
			return nil, errBadRequest("frequency must be one of: daily, alternate, weekly")
		}
		set = append(set, bson.E{Key: "frequency", Value: *in.Frequency})
	}
	if in.DeliverySlot != nil {
		set = append(set, bson.E{Key: "delivery_slot", Value: *in.DeliverySlot})
	}
	if in.StartDate != nil {
		if _, ok := parseDay(*in.StartDate); !ok {
			return nil, errBadRequest("start_date must be YYYY-MM-DD")
		}
		set = append(set, bson.E{Key: "start_date", Value: *in.StartDate})
	}
	if in.Vacations != nil {
		for _, v := range *in.Vacations {
			if _, ok1 := parseDay(v.Start); !ok1 {
				return nil, errBadRequest("vacation start must be YYYY-MM-DD")
			}
			if _, ok2 := parseDay(v.End); !ok2 || v.End < v.Start {
				return nil, errBadRequest("vacation end must be YYYY-MM-DD on/after its start")
			}
		}
		set = append(set, bson.E{Key: "vacations", Value: *in.Vacations})
	}
	if len(set) == 0 {
		return s.repo.findSubscription(ctx, subID, consumerID)
	}
	return s.repo.updateSubscription(ctx, subID, consumerID, set, bson.D{})
}

// ── The morning-order lifecycle (server twin of lib/subscriptionSweep.ts) ───
//
//   13:00 IST   SCHEDULE — tomorrow's order materialises as a VISIBLE upcoming
//               order (status placed, delivery_date=tomorrow, NO delivery task,
//               NO money). The shopper sees it in Orders and can still change
//               everything: subscription edits reconcile it, pause/vacation
//               cancel it (claim released), a direct order cancel skips the day.
//   00:00 IST   LOCK — the 11:59 PM cutoff. Each preview is re-checked against
//               the LIVE subscription, its line refreshed, the wallet floor
//               enforced, then the store delivery task is created. From here
//               subscription edits no longer touch it.
//   05:00–07:30 The morning route delivers; money settles on delivery.

// scheduleFromHourIST — tomorrow's upcoming order becomes visible from 1 PM IST.
const scheduleFromHourIST = 13

// addDaysIST offsets a YYYY-MM-DD day on the IST calendar.
func addDaysIST(dayISO string, n int) string {
	t, ok := parseDay(dayISO)
	if !ok {
		return dayISO
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// insertSubscriptionOrder materialises one subscription day as a consumer
// order. locked=false → a SCHEDULED preview (no delivery task yet, cancellable,
// reconciled until the midnight lock); locked=true → immediately live (delivery
// task created, store queue). Money never moves here — settle-on-delivery.
func (s *service) insertSubscriptionOrder(ctx context.Context, sub *subscription, addr *address, day string, locked bool, at time.Time) (*order, error) {
	acct, _ := s.repo.findAccountByID(ctx, sub.ConsumerID)
	name, phone := "", ""
	if acct != nil {
		phone = acct.Phone
		if acct.FullName != nil {
			name = *acct.FullName
		}
	}
	subtotal := round2(sub.UnitPrice * float64(sub.Qty))
	// SUBSCRIPTION deliveries carry NO delivery fee — the consumer app quotes and
	// charges the bare subtotal on the Subscribe button (lib/api.ts), so the
	// server must debit the same. A ₹15 fee here over-charged every subscription
	// delivery vs the price the member agreed to.
	fee := subscriptionDeliveryFee
	now := at.UTC()
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: sub.ConsumerID.Hex(), Status: "placed",
		Subtotal: subtotal, DeliveryFee: fee, Total: round2(subtotal + fee), PaymentMethod: "wallet",
		AddressLabel: addr.Label, AddressText: joinAddress(addr), RiderID: nil,
		PlacedAt: now, Priority: "normal",
		// The 05:00–07:30 morning route — same window the FE sweep prints.
		DeliveryWindow: "05:00 - 07:30 AM", Lane: "morning",
		Items: []orderItem{{
			ID: newItemID(), ProductID: sub.ProductID, Name: sub.Name,
			Variant: sub.Variant, Price: round2(sub.UnitPrice), Qty: sub.Qty,
		}},
		ConsumerName: name, Phone: phone, Geo: &geoPoint{Lat: *addr.Lat, Lng: *addr.Lng},
		CreatedAt: now, UpdatedAt: now,
		SubscriptionID: sub.SubscriptionID, ScheduledFor: day,
	}
	// Flag a free-day trial delivery for DISPLAY + ANALYTICS (badge "FREE" in the
	// app, count free milk in reporting). Peeking the trial ledger never advances
	// it and never gates the wallet/hold — the order is still placed only when
	// funded (unchanged); the actual 0 charge is applied at delivery.
	if isTrialProduct(sub.ProductID) {
		if t, terr := s.repo.getOrCreateTrial(ctx, sub.ConsumerID); terr == nil && trialPhaseFor(t.DeliveredPaid, t.DeliveredFree) == trialPhaseFree {
			o.TrialFree = true
		}
	}
	if locked {
		o.SubLockedAt = now.Format(time.RFC3339)
	}
	if err := s.repo.insertOrder(ctx, o); err != nil {
		return nil, err
	}
	if locked {
		s.createDeliveryForOrder(ctx, o)
	}
	s.repo.setSubscriptionOrder(ctx, sub.SubscriptionID, o.OrderID)
	return o, nil
}

// cancelScheduledSubOrder cancels a still-unlocked preview (worker path only —
// guarded so a locked or already-cancelled order is never touched).
func (s *service) cancelScheduledSubOrder(ctx context.Context, o *order) {
	_, _ = s.repo.updateOrder(ctx, o.OrderID, o.UserID,
		bson.D{{Key: "status", Value: "cancelled"}},
		bson.D{
			{Key: "status", Value: "placed"},
			{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		})
}

// refreshSubOrder re-derives a preview's line from the LIVE subscription, so a
// qty / price / variant edit made before the midnight lock shows on the
// upcoming order (and bills correctly at lock).
func (s *service) refreshSubOrder(ctx context.Context, o *order, sub *subscription) *order {
	if len(o.Items) == 1 &&
		o.Items[0].Qty == sub.Qty &&
		o.Items[0].Price == round2(sub.UnitPrice) &&
		o.Items[0].Variant == sub.Variant {
		return o
	}
	subtotal := round2(sub.UnitPrice * float64(sub.Qty))
	fee := subscriptionDeliveryFee // subscriptions never carry the ₹15 fee (see insertSubscriptionOrder)
	updated, err := s.repo.updateOrder(ctx, o.OrderID, o.UserID, bson.D{
		{Key: "order_items", Value: []orderItem{{
			ID: newItemID(), ProductID: sub.ProductID, Name: sub.Name,
			Variant: sub.Variant, Price: round2(sub.UnitPrice), Qty: sub.Qty,
		}}},
		{Key: "subtotal", Value: subtotal},
		{Key: "delivery_fee", Value: fee},
		{Key: "total", Value: round2(subtotal + fee)},
	}, bson.D{{Key: "status", Value: "placed"}})
	if err != nil {
		return o
	}
	return updated
}

// sweepSubscriptionOrders runs the full lifecycle for one tick: expire stale
// previews, LOCK today's, create same-day starts, SCHEDULE tomorrow's (from
// 13:00 IST), and reconcile tomorrow's previews against subscription edits.
// Exactly-once per (subscription, day) via the claim; returns how many orders
// went LIVE (locked) this tick.
func (s *service) sweepSubscriptionOrders(ctx context.Context, now time.Time) int {
	nowIST := now.In(istZone)
	today := istToday(now)
	tomorrow := addDaysIST(today, 1)
	placed := 0

	// 1) EXPIRE — previews whose day passed without ever locking (server down
	//    over midnight, wallet never funded): cancel; stale milk never ships.
	if stale, err := s.repo.listUnlockedSubOrders(ctx, bson.D{{Key: "$lt", Value: today}}); err == nil {
		for i := range stale {
			s.cancelScheduledSubOrder(ctx, &stale[i])
		}
	}

	// 2) LOCK — first tick after IST midnight. Re-evaluate each preview against
	//    the live subscription (the 11:59 PM edit law): a pause/cancel/vacation
	//    made before the cutoff kills it; otherwise refresh the line, enforce
	//    the wallet floor (unfunded → retried every tick), then create the
	//    store delivery task.
	if due, err := s.repo.listUnlockedSubOrders(ctx, today); err == nil {
		for i := range due {
			o := &due[i]
			sub, _ := s.repo.findSubscriptionByID(ctx, o.SubscriptionID)
			if sub == nil || !subscriptionDueOn(sub, today) {
				s.cancelScheduledSubOrder(ctx, o)
				continue
			}
			o = s.refreshSubOrder(ctx, o, sub)
			if wv, werr := s.wallet(ctx, sub.ConsumerID); werr != nil || wv.Available < o.Total {
				continue
			}
			upd, uerr := s.repo.updateOrder(ctx, o.OrderID, o.UserID,
				bson.D{{Key: "sub_locked_at", Value: now.UTC().Format(time.RFC3339)}},
				bson.D{
					{Key: "status", Value: "placed"},
					{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
				})
			if uerr != nil {
				continue // raced with another replica — it owns the lock
			}
			s.createDeliveryForOrder(ctx, upd)
			placed++
		}
	}

	subs, err := s.repo.listActiveSubscriptions(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "subscription sweep: scan failed")
		return placed
	}
	for i := range subs {
		sub := &subs[i]
		// 3) SAME-DAY — a subscription starting (or resumed) today that never got
		//    a preview still delivers today: wallet floor, claim, create LOCKED.
		if sub.LastOrderDate != today && subscriptionDueOn(sub, today) {
			if addr, aerr := s.subscriptionAddress(ctx, sub.ConsumerID); aerr == nil {
				lineTotal := round2(sub.UnitPrice * float64(sub.Qty))
				cost := lineTotal + subscriptionDeliveryFee // affordability check must match the fee-free debit
				if wv, werr := s.wallet(ctx, sub.ConsumerID); werr == nil && wv.Available >= cost {
					if won, _ := s.repo.claimSubscriptionDay(ctx, sub.SubscriptionID, today); won {
						if _, oerr := s.insertSubscriptionOrder(ctx, sub, addr, today, true, now); oerr == nil {
							placed++
						} else {
							s.log.WarnContext(ctx, "subscription sweep: same-day order failed",
								"subscription", sub.SubscriptionID, "day", today)
						}
					}
				}
			}
		}
		// 4) SCHEDULE — from 13:00 IST, materialise TOMORROW's delivery as a
		//    visible, still-modifiable upcoming order (no delivery task, no
		//    money, no wallet gate — the member can top up until midnight).
		if nowIST.Hour() >= scheduleFromHourIST && sub.LastOrderDate != tomorrow && subscriptionDueOn(sub, tomorrow) {
			if addr, aerr := s.subscriptionAddress(ctx, sub.ConsumerID); aerr == nil {
				if won, _ := s.repo.claimSubscriptionDay(ctx, sub.SubscriptionID, tomorrow); won {
					if _, oerr := s.insertSubscriptionOrder(ctx, sub, addr, tomorrow, false, now); oerr != nil {
						s.repo.unclaimSubscriptionDay(ctx, sub.SubscriptionID, tomorrow) // retry next tick
					}
				}
			}
		}
	}

	// 5) RECONCILE — tomorrow's previews against their live subscriptions:
	//    pause/cancel/vacation → cancel the upcoming order AND release the day
	//    claim (a resume before midnight re-schedules it); qty/price edits →
	//    refresh the shown line. A shopper's direct cancel is untouched here
	//    (its claim stays, so the day stays skipped).
	if upcoming, err := s.repo.listUnlockedSubOrders(ctx, tomorrow); err == nil {
		for i := range upcoming {
			o := &upcoming[i]
			sub, _ := s.repo.findSubscriptionByID(ctx, o.SubscriptionID)
			if sub == nil || !subscriptionDueOn(sub, tomorrow) {
				s.cancelScheduledSubOrder(ctx, o)
				s.repo.unclaimSubscriptionDay(ctx, o.SubscriptionID, tomorrow)
				continue
			}
			s.refreshSubOrder(ctx, o, sub)
		}
	}

	if placed > 0 {
		s.log.InfoContext(ctx, "subscription sweep placed morning orders", "day", today, "orders", placed)
	}
	return placed
}

func joinAddress(a *address) string {
	parts := []string{}
	for _, p := range []string{a.Line1, a.Line2, a.City, a.Pincode} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// subscriptionOrderWorker — the background scheduler (no external cron). Ticks
// every 15 minutes; each tick runs the full lifecycle: from 13:00 IST it
// SCHEDULES tomorrow's visible upcoming orders, keeps them reconciled with
// subscription edits until the 11:59 PM cutoff, and at the first tick after
// IST midnight it LOCKS them (delivery tasks created) — so the store manager's
// queue is filled before the 05:00 route WITHOUT any consumer opening the app.
// The day-claim + lock guard make every duplicate tick (or replica) a no-op.
func (s *service) subscriptionOrderWorker(ctx context.Context) {
	const tick = 15 * time.Minute
	// Immediate first sweep on boot (covers a server restart mid-morning).
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	s.sweepSubscriptionOrders(runCtx, time.Now())
	cancel()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			s.sweepSubscriptionOrders(runCtx, now)
			cancel()
		}
	}
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *handler) createSubscription(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in subscriptionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	sub, err := h.svc.createSubscription(r.Context(), id, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sub)
}

func (h *handler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.svc.listSubscriptionsFor(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) subscriptionAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, aerr := actorID(r)
		if aerr != nil {
			writeErr(w, aerr)
			return
		}
		sub, err := h.svc.setSubscriptionStatus(r.Context(), id, chi.URLParam(r, "id"), action)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sub)
	}
}

func (h *handler) patchSubscription(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	sub, err := h.svc.patchSubscription(r.Context(), id, chi.URLParam(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

// sweepSubscriptions — DEV-only manual tick (mirrors /mandate/{id}/execute), so
// the subscribe→morning-order→store-queue flow is testable without waiting for
// the 15-minute worker tick.
func (h *handler) sweepSubscriptions(w http.ResponseWriter, r *http.Request) {
	if _, aerr := actorID(r); aerr != nil {
		writeErr(w, aerr)
		return
	}
	if !h.svc.deps.Cfg.OTPDevMode {
		writeErr(w, errForbidden("not available"))
		return
	}
	placed := h.svc.sweepSubscriptionOrders(r.Context(), time.Now())
	writeJSON(w, http.StatusOK, map[string]int{"placed": placed})
}

package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Orders — the consumer purchase aggregate. Mirrors the shipped FE Order shape
// (lib/api.ts) exactly so the app's placeOrder/listOrders/getOrder/cancelOrder/
// reviewOrder seams compose without any FE change. The backend OWNS the order;
// money is debited on DELIVERY via the server wallet (the FE's
// settleDeliveredOrders sweep calls /wallet/debit, idempotent by order id), so
// creation never moves money. All rows are scoped to the authenticated shopper.

const collOrders = "consumer_orders"

// DELIVERY CHARGE: the One Voice rule, one rule everywhere (pyaas-one-voice.md
// 1.2, the founder's call of 25 Sep 2026): "Free above Rs 199. Below Rs 199,
// Rs 5 a delivery. Founding Family members: always free. Parag always at
// printed MRP with nothing extra."
//
//   - "Above Rs 199" is judged on the goods the order bills: the subtotal
//     after member prices, Parag lines included at MRP, before this fee and
//     before the instant monsoon surcharge. Rs 199.00 is free, anything below
//     pays; an order that bills nothing pays nothing.
//   - The amount is DELIVERY-FEE: the ERP service's price once the Dolibarr
//     sync has seen it, else FOUNDING_DELIVERY_FEE_PAISE (Rs 5).
//   - An active Founding Family member (foundingStanding) never pays it.
//   - Parag carries nothing extra: priceForMember never discounts or marks up
//     a PRG line, and this fee is the only charge an order can add.
//   - One-off orders only: a subscription morning never carries it
//     (subscriptionDeliveryFee, the founder's 18 Aug "subscriptions sell at
//     MRP", which the Subscribe button quotes).
//
// The consumer app computes the same number (lib/api.ts deliveryFeeFor) so
// the cart shows what is billed.
const freeDeliveryOver = 199.0

// Fair-use order caps — kept identical to the FE (lib/pricing.ts).
const maxQtyPerProduct = 10
const maxItemsPerOrder = 30

// oneVoiceDeliveryFee applies the rule to a billed subtotal: fee (the
// DELIVERY-FEE amount) below freeDeliveryOver, nothing at or above it,
// nothing for a member and nothing on an empty bill.
func oneVoiceDeliveryFee(subtotal, fee float64, member bool) float64 {
	if member || subtotal <= 0 || subtotal >= freeDeliveryOver || fee <= 0 {
		return 0
	}
	return round2(fee)
}

// orderDeliveryFee is the One Voice charge for a one-off order's subtotal.
func (s *service) orderDeliveryFee(ctx context.Context, subtotal float64, member bool) float64 {
	if member || subtotal <= 0 || subtotal >= freeDeliveryOver {
		return 0 // no DELIVERY-FEE read for an order that pays none
	}
	return oneVoiceDeliveryFee(subtotal, s.foundingDeliveryFee(ctx), member)
}

// errCodeCutoffPassed is the 422 code a one-off morning order for a closed
// morning used to get (with next_delivery_date). Since 24 Sep such an order
// is moved to the first open morning and accepted instead (createOrderAt),
// so the server no longer sends it; the code stays defined because shipped
// app builds still handle it.
const errCodeCutoffPassed = "CUTOFF_PASSED"

// Statuses a placed order may still be cancelled from. "assigned" is included so a
// customer can still cancel AFTER a rider claims/accepts but BEFORE pickup — once the
// order is picked up (out_for_delivery) it can no longer be cancelled or returned.
var cancellableStatuses = map[string]bool{"placed": true, "confirmed": true, "assigned": true}

// ── Documents (bson) + wire shape (json matches the FE Order) ────────────────

type orderItem struct {
	ID        string  `bson:"id"         json:"id"`
	ProductID string  `bson:"product_id" json:"product_id"`
	Name      string  `bson:"name"       json:"name"`
	Variant   string  `bson:"variant"    json:"variant"`
	Price     float64 `bson:"price"      json:"price"`
	Qty       int     `bson:"qty"        json:"qty"`
	// CH-04 promotional line (Welcome Litre): a free pack is a REAL ledger
	// line at price 0, minted SERVER-SIDE ONLY (crm_offers.go) — these fields
	// are never accepted from a request body, so the client price authority's
	// ₹0 rejection stays fully intact. All omitempty: invisible on every
	// existing order and inert for the published app (loose decode).
	IsPromotional    bool    `bson:"is_promotional,omitempty"     json:"is_promotional,omitempty"`
	PromotionalValue float64 `bson:"promotional_value,omitempty"  json:"promotional_value,omitempty"`
	SupplySource     string  `bson:"supply_source,omitempty"      json:"supply_source,omitempty"`
	Batch            string  `bson:"batch,omitempty"              json:"batch,omitempty"`
	Expiry           string  `bson:"expiry,omitempty"             json:"expiry,omitempty"`
}

type rider struct {
	ID         string   `bson:"id"          json:"id"`
	FullName   string   `bson:"full_name"   json:"full_name"`
	Phone      string   `bson:"phone"       json:"phone"`
	Vehicle    *string  `bson:"vehicle"     json:"vehicle"`
	Rating     *float64 `bson:"rating"      json:"rating"`
	CurrentLat *float64 `bson:"current_lat" json:"current_lat"`
	CurrentLng *float64 `bson:"current_lng" json:"current_lng"`
}

type orderReview struct {
	Rating    int       `bson:"rating"     json:"rating"`
	Comment   string    `bson:"comment"    json:"comment"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

// orderCancelledByDelivery is the CancelledBy marker for a task-driven cancel.
const orderCancelledByDelivery = "delivery"

type geoPoint struct {
	Lat float64 `bson:"lat" json:"lat"`
	Lng float64 `bson:"lng" json:"lng"`
}

// order is stored per shopper. json tags reproduce the FE Order EXACTLY; fields
// the FE never reads (owner/geo/audit) are json:"-".
type order struct {
	MongoID        primitive.ObjectID `bson:"_id,omitempty"           json:"-"`
	OrderID        string             `bson:"order_id"                json:"id"`
	UserID         string             `bson:"user_id"                 json:"user_id"` // consumer id (hex)
	Status         string             `bson:"status"                  json:"status"`
	Subtotal       float64            `bson:"subtotal"                json:"subtotal"`
	DeliveryFee    float64            `bson:"delivery_fee"            json:"delivery_fee"`
	MonsoonFee     float64            `bson:"monsoon_fee,omitempty"   json:"monsoon_fee,omitempty"`
	Total          float64            `bson:"total"                   json:"total"`
	PaymentMethod  string             `bson:"payment_method"          json:"payment_method"`
	AddressLabel   string             `bson:"address_label"           json:"address_label"`
	AddressText    string             `bson:"address_text"            json:"address_text"`
	AddressID      string             `bson:"address_id,omitempty"    json:"address_id,omitempty"`
	RiderID        *string            `bson:"rider_id"                json:"rider_id"`
	PlacedAt       time.Time          `bson:"placed_at"               json:"placed_at"`
	Priority       string             `bson:"priority,omitempty"      json:"priority,omitempty"`
	DeliveryWindow string             `bson:"delivery_window,omitempty" json:"delivery_window,omitempty"`
	DeliveryDate   string             `bson:"delivery_date,omitempty" json:"delivery_date,omitempty"`
	// RequestedDate is the morning a one-off order asked for (the body's
	// delivery_date, as sent) and DateMoved is true when that morning was
	// already closed at 12 noon the day before (or past) and the order was
	// moved to the first open one: delivery_date is always the real day.
	// Morning one-off orders only; additive, absent everywhere else.
	RequestedDate string `bson:"requested_date,omitempty" json:"requested_date,omitempty"`
	DateMoved     bool   `bson:"date_moved,omitempty"     json:"date_moved,omitempty"`
	// Welcome Litre linkage (crm_offers.go). OfferPack 1|2 marks a
	// promotional pack order; both omitempty → absent everywhere else.
	DeliveryPrefs *deliveryPrefsDoc `bson:"delivery_prefs,omitempty" json:"delivery_prefs,omitempty"`
	OfferID       string            `bson:"offer_id,omitempty"   json:"offer_id,omitempty"`
	OfferPack     int               `bson:"offer_pack,omitempty" json:"offer_pack,omitempty"`
	BuyerGSTIN    string            `bson:"buyer_gstin,omitempty"   json:"buyer_gstin,omitempty"`
	ProofPhotoURL string            `bson:"proof_photo_url,omitempty" json:"proof_photo_url,omitempty"`
	// Tracking extras the consumer app already reads (app/order/[id].tsx):
	// store_lat/store_lng is the pickup point the rider left from (the map's
	// trip origin); delivered_at comes from the delivery task.
	StoreLat    *float64 `bson:"store_lat,omitempty"      json:"store_lat,omitempty"`
	StoreLng    *float64 `bson:"store_lng,omitempty"      json:"store_lng,omitempty"`
	DeliveredAt string   `bson:"delivered_at,omitempty"   json:"delivered_at,omitempty"`
	Lane        string   `bson:"lane"                    json:"lane,omitempty"`
	// TrialFree marks a 2+2 free-day subscription delivery: the sticker Total
	// stands, but the wallet charge at delivery is 0 (trialChargeFor). Set at
	// creation from the trial phase — DISPLAY + ANALYTICS only, it never gates the
	// wallet or the hold. Absent (omitempty) on every normal order.
	TrialFree bool         `bson:"trial_free,omitempty"    json:"trial_free,omitempty"`
	Items     []orderItem  `bson:"order_items"             json:"order_items"`
	Rider     *rider       `bson:"riders,omitempty"        json:"riders"`
	CanReview bool         `bson:"can_review"              json:"can_review"`
	Review    *orderReview `bson:"review,omitempty"        json:"review"`
	// Owner-facing / delivery metadata — never sent back to the shopper client.
	ConsumerName string    `bson:"consumer_name,omitempty" json:"-"`
	Phone        string    `bson:"phone,omitempty"         json:"-"`
	Geo          *geoPoint `bson:"geo,omitempty"           json:"-"`
	CreatedAt    time.Time `bson:"created_at"              json:"-"`
	UpdatedAt    time.Time `bson:"updated_at"              json:"-"`
	// Subscription linkage (worker-created morning orders, subscriptions.go).
	// ScheduledFor is the IST day it delivers — surfaced as delivery_date (the
	// FE Order field), so tomorrow's order shows as UPCOMING in the app from
	// the moment it is scheduled. SubLockedAt is stamped at the noon lock (12:00
	// IST the day before delivery): before it, the order is a modifiable
	// preview (subscription edits reconcile it, the shopper may cancel it);
	// after it, the delivery task exists and the store owns it.
	SubscriptionID string `bson:"subscription_id,omitempty" json:"-"`
	ScheduledFor   string `bson:"scheduled_for,omitempty"   json:"scheduled_for,omitempty"`
	SubLockedAt    string `bson:"sub_locked_at,omitempty"   json:"-"`
	// CancelledBy is set when the DELIVERY TASK cancelled this order (a FAILED
	// marking or a store cancel), so a rider's undo can walk it back; a
	// customer's own cancel leaves it empty. Never sent to the shopper.
	CancelledBy string `bson:"cancelled_by,omitempty" json:"-"`
}

func newOrderID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "ord_" + hex.EncodeToString(b)
}
func newItemID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "item_" + hex.EncodeToString(b)
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) ensureOrderIndexes(ctx context.Context) error {
	specs := []mongo.IndexModel{
		{Keys: bson.D{{Key: "order_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "placed_at", Value: -1}}},
	}
	if _, err := r.orders.Indexes().CreateMany(ctx, specs); err != nil {
		return err
	}
	return nil
}

func (r *repository) insertOrder(ctx context.Context, o *order) error {
	if _, err := r.orders.InsertOne(ctx, o); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("ORDER_EXISTS", "order already exists")
		}
		return errInternal("order create failed")
	}
	return nil
}

func (r *repository) listOrders(ctx context.Context, userID string) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{{Key: "user_id", Value: userID}},
		options.Find().SetSort(bson.D{{Key: "placed_at", Value: -1}}).SetLimit(200))
	if err != nil {
		return nil, errInternal("orders lookup failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("orders decode failed")
	}
	return out, nil
}

func (r *repository) findOrder(ctx context.Context, orderID, userID string) (*order, error) {
	var o order
	err := r.orders.FindOne(ctx, bson.D{{Key: "order_id", Value: orderID}, {Key: "user_id", Value: userID}}).Decode(&o)
	if isNoDocs(err) {
		return nil, errNotFound("order not found")
	}
	if err != nil {
		return nil, errInternal("order lookup failed")
	}
	return &o, nil
}

// findOrderAnyUser — internal lookup with no ownership filter (delivery-side
// guards that need the parent order's status). Never exposed on a route.
func (r *repository) findOrderAnyUser(ctx context.Context, orderID string) (*order, error) {
	var o order
	err := r.orders.FindOne(ctx, bson.D{{Key: "order_id", Value: orderID}}).Decode(&o)
	if isNoDocs(err) {
		return nil, errNotFound("order not found")
	}
	if err != nil {
		return nil, errInternal("order lookup failed")
	}
	return &o, nil
}

// updateOrder applies a $set scoped to (order_id, user_id) and returns the fresh
// doc. optFilter adds extra guard conditions (e.g. a status precondition).
func (r *repository) updateOrder(ctx context.Context, orderID, userID string, set bson.D, guard bson.D) (*order, error) {
	filter := bson.D{{Key: "order_id", Value: orderID}, {Key: "user_id", Value: userID}}
	filter = append(filter, guard...)
	set = append(set, bson.E{Key: "updated_at", Value: time.Now().UTC()})
	after := options.After
	var o order
	err := r.orders.FindOneAndUpdate(ctx, filter,
		bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&o)
	if isNoDocs(err) {
		return nil, errNotFound("order not found or not in a valid state")
	}
	if err != nil {
		return nil, errInternal("order update failed")
	}
	return &o, nil
}

// ── Service ─────────────────────────────────────────────────────────────────

type orderInput struct {
	Items          []orderItem `json:"order_items"`
	PaymentMethod  string      `json:"payment_method"`
	AddressLabel   string      `json:"address_label"`
	AddressText    string      `json:"address_text"`
	AddressID      string      `json:"address_id"` // saved address id; see addressFor
	Priority       string      `json:"priority"`
	DeliveryWindow string      `json:"delivery_window"`
	Total          float64     `json:"total"` // client total (may carry a coupon discount)
	Lane           string      `json:"lane"`
	ConsumerName   string      `json:"consumer_name"`
	Phone          string      `json:"phone"`
	Geo            *geoPoint   `json:"geo"`
	// Scheduled morning orders: the member-picked IST delivery date
	// (YYYY-MM-DD, tomorrow..+7d). Empty = next morning / instant-now.
	DeliveryDate string `json:"delivery_date"`
	// Optional company GSTIN entered at checkout — printed on the invoice.
	BuyerGSTIN string `json:"buyer_gstin"`
	// Doorstep instructions for THIS order (ring-bell / call-before / note) —
	// sanitized server-side, copied onto the delivery task for the rider.
	DeliveryPrefs map[string]any `json:"delivery_prefs"`
}

func (s *service) createOrder(ctx context.Context, userID string, in orderInput) (*order, error) {
	return s.createOrderAt(ctx, userID, in, s.now())
}

// createOrderAt is createOrder on an explicit clock: the delivery-date window
// and the noon cut-off are IST-calendar rules, so tests drive them at a fixed
// moment the way the subscription sweep's tests do.
func (s *service) createOrderAt(ctx context.Context, userID string, in orderInput, at time.Time) (*order, error) {
	if len(in.Items) == 0 {
		return nil, errBadRequest("an order needs at least one item")
	}
	// Money is recomputed SERVER-SIDE: the client-supplied `total` is ignored
	// AND every unit price comes from the server's own catalog
	// (catalog_price.go) — a tampered client can neither deflate nor inflate a
	// line ("₹0 milk" closed). Unknown or hidden products are rejected, never
	// guessed.
	priceIx, err := s.loadPriceIndex(ctx)
	if err != nil {
		return nil, err
	}
	// Delivery lane: "morning" (5–7:30 subscription run) is the DEFAULT — the
	// instant ≈20-min lane is an explicit, validated opt-in (an unknown value
	// must never accidentally mint an instant ETA).
	lane := in.Lane
	if lane != "instant" {
		lane = "morning"
	}
	// Scheduled morning date (instant orders never carry one; the FE sends
	// null, and a stale client's date is ignored on the instant lane): a
	// morning past its noon cut-off is already moved to the first open
	// morning. A bad date is refused below, after the items, as before.
	deliveryDate, requested, moved, derr := morningDeliveryDate(lane, in.DeliveryDate, at)
	// FOUNDING FAMILY (founding.go): a member whose perks cover the DELIVERY
	// day bills PYAAS milk lines at level 3 and never pays delivery; Parag is
	// level 1 for everyone. The day is the morning the order goes out on (up
	// to 7 days ahead), or today on the instant lane, on the order's own
	// clock: the same per-day standing a subscription morning is judged by
	// (pyaasPlanGate), so a member whose paid month ends before that morning
	// is billed as a non-member.
	consumerOID, _ := primitive.ObjectIDFromHex(userID)
	standingDay := istToday(at)
	if derr == nil && deliveryDate != "" {
		standingDay = deliveryDate
	}
	_, memberActive := s.foundingStanding(ctx, consumerOID, standingDay)
	var subtotal float64
	var units int
	items := make([]orderItem, 0, len(in.Items))
	for _, it := range in.Items {
		if it.Qty <= 0 {
			return nil, errBadRequest("invalid order item")
		}
		if it.Qty > maxQtyPerProduct {
			return nil, errBadRequest("quantity per item exceeds the limit")
		}
		// SERVER-AUTHORITATIVE PRICE: the catalog's price index is the one billed —
		// the client's number is display-only, and unknown ids are refused.
		price, ok := priceIx.priceForMember(it.ProductID, it.Variant, memberActive)
		if !ok {
			return nil, errBadRequest("unknown product in order: " + it.ProductID)
		}
		if priceIx.isPyaasLine(it.ProductID) {
			if gerr := s.foundingGate(ctx, consumerOID, true, memberActive); gerr != nil {
				return nil, gerr
			}
		}
		name := priceIx.nameFor(it.ProductID)
		if name == "" {
			name = it.Name // catalog rows without a name keep the client label
		}
		units += it.Qty
		subtotal += price * float64(it.Qty)
		items = append(items, orderItem{
			// The size billed, not the client's label (a "1L" pill on a 500 ml
			// price packed the wrong crate); the name stays verbatim.
			ID: newItemID(), ProductID: it.ProductID, Name: name, Variant: priceIx.lineVariant(it.ProductID, it.Variant), Price: round2(price), Qty: it.Qty,
		})
	}
	if units > maxItemsPerOrder {
		return nil, errBadRequest("too many items in one order")
	}
	subtotal = round2(subtotal)
	// One Voice (see freeDeliveryOver): Rs 5 below Rs 199, free from Rs 199,
	// never for a member. Spec rule 5.3's separate non-member fee on PYAAS
	// milk (FOUNDING_PYAAS_NONMEMBER_FEE) is superseded by it: the founder
	// kept it off on 25 Sep, and switched on it adds nothing this rule does
	// not already charge, so an order never pays two fees and never pays one
	// from Rs 199.
	fee := s.orderDeliveryFee(ctx, subtotal, memberActive)
	total := round2(subtotal + fee)
	// Payment mode defaults to 'wallet' — the order is settled from the server
	// wallet on delivery (the settle sweep debits /wallet/debit, idempotent by
	// order id). A client may pass another mode (e.g. 'cod', or 'gateway' once the
	// /orders/{id}/pay direct-pay seam is wired to verify).
	pm := in.PaymentMethod
	if pm == "" {
		pm = "wallet"
	}
	// The scheduled morning date (decided above) is checked BEFORE the
	// serviceability guard, so the guard judges the order as it will be
	// delivered: its lane, and a morning past its noon cut-off already moved
	// to the first open morning.
	if derr != nil {
		return nil, derr
	}
	// SERVICEABILITY: the order is judged by the SAME decision GET /serviceability
	// gives the app (serviceability(), every env override included) at the order's
	// delivery point and at the order's own moment (the instant hours), and
	// refused only where that answer is a definitive no. With no point, or no
	// answer (a lookup error), it goes through as before: like serviceability
	// itself, ordering never goes dark on missing data. The moment matters only to
	// the instant lane, which leaves now; a morning order is judged by its point
	// alone (NOT_SERVICEABLE), which no clock changes, so the morning it goes out
	// on - moved past the noon cut-off or not - gets the same verdict, and instant
	// being shut tonight never refuses it.
	var sv *serviceabilityResult
	if pt, pincode, ok := s.orderDeliveryPoint(ctx, userID, in); ok {
		if res, sErr := s.serviceabilityAt(ctx, pt.Lat, pt.Lng, pincode, at); sErr == nil {
			sv = res
		}
	}
	if refusal := orderServiceabilityRefusal(sv, lane); refusal != nil {
		return nil, refusal
	}
	// Monsoon surcharge: INSTANT orders only, and only if the store manager enabled
	// it on the delivery location's zone. Authoritative here — read from the zone,
	// never trusted from the client payload, so a tampered client cannot skip it.
	monsoonFee := 0.0
	if lane == "instant" && sv != nil && sv.MonsoonEnabled && sv.MonsoonRupees > 0 {
		monsoonFee = float64(sv.MonsoonRupees)
	}
	total = round2(total + monsoonFee)
	priority := in.Priority
	if priority == "" {
		priority = "normal"
	}
	now := at.UTC()
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: userID, Status: "placed",
		Subtotal: subtotal, DeliveryFee: fee, MonsoonFee: monsoonFee, Total: total, PaymentMethod: pm,
		AddressLabel: in.AddressLabel, AddressText: in.AddressText, AddressID: strings.TrimSpace(in.AddressID), RiderID: nil,
		PlacedAt: now, Priority: priority, DeliveryWindow: in.DeliveryWindow, Lane: lane,
		DeliveryDate: deliveryDate, RequestedDate: requested, DateMoved: moved, BuyerGSTIN: strings.TrimSpace(in.BuyerGSTIN),
		Items: items, Rider: nil, CanReview: false, Review: nil,
		ConsumerName: in.ConsumerName, Phone: in.Phone, Geo: in.Geo, CreatedAt: now, UpdatedAt: now,
		DeliveryPrefs: sanitizeDeliveryPrefs(in.DeliveryPrefs),
	}
	if err := s.repo.insertOrder(ctx, o); err != nil {
		return nil, err
	}
	// Create the last-mile delivery task (routed to the nearest Parag Store,
	// unassigned until a store manager assigns a rider). Best-effort. On the
	// order's clock, so D-01 words the real day against the moment it was
	// placed.
	s.createDeliveryForOrderAt(ctx, o, at)
	return o, nil
}

// orderDeliveryPoint is where an order is going, by what the order itself
// carries: its own pin (the point its delivery task is routed to), else the
// exact coordinates of the saved address it names by id. The pincode is that
// saved address's, which the app sends beside the same coordinates when it asks
// /serviceability. ok=false when the order carries neither: a label-only order
// (the deployed app sends no pin and no address id) is never judged by guessing
// an address from its label.
func (s *service) orderDeliveryPoint(ctx context.Context, userID string, in orderInput) (geoPt, string, bool) {
	var addr *address
	if cid, err := primitive.ObjectIDFromHex(userID); err == nil {
		addr = s.addressByID(ctx, cid, in.AddressID) // scoped: someone else's id is ignored
	}
	pincode := ""
	if addr != nil {
		pincode = addr.Pincode
	}
	if in.Geo != nil && coordsSane(in.Geo.Lat, in.Geo.Lng) {
		return geoPt{Lat: in.Geo.Lat, Lng: in.Geo.Lng}, pincode, true
	}
	if addr != nil && addr.Lat != nil && addr.Lng != nil && coordsSane(*addr.Lat, *addr.Lng) {
		return geoPt{Lat: *addr.Lat, Lng: *addr.Lng}, pincode, true
	}
	return geoPt{}, "", false
}

// orderServiceabilityRefusal turns the /serviceability answer for an order's
// delivery point into a refusal, only where that answer is a definitive no:
//
//   - not serviceable (outside every zone, or beyond the no-zone store fence):
//     NOT_SERVICEABLE, on either lane;
//   - instant shut for the night / paused: INSTANT_CLOSED (unchanged; with no
//     zone drawn and INSTANT_TEST_OPEN on, that is outside the 07:00-22:00
//     the console shows);
//   - instant not offered where a drawn zone decides: INSTANT_OUT_OF_RANGE.
//
// Everything else goes through: no answer (nil), and the no-zone default-open
// answer, where no instant circle exists for the point to be outside of.
func orderServiceabilityRefusal(sv *serviceabilityResult, lane string) *apiError {
	if sv == nil {
		return nil
	}
	if !sv.Serviceable {
		return errUnprocessable("NOT_SERVICEABLE", "We don't deliver to this address yet. Choose another delivery address, or join the waitlist and we'll let you know when we reach you.")
	}
	if lane != "instant" {
		return nil
	}
	// Store shut for the night / paused → refuse instant (defence in depth;
	// the consumer already hides it, but a stale client must not slip through).
	if sv.InstantClosed {
		return errUnprocessable("INSTANT_CLOSED", "instant delivery is closed right now; please choose the morning slot")
	}
	if !sv.Instant && !sv.DefaultOpen {
		return errUnprocessable("INSTANT_OUT_OF_RANGE", "Instant delivery doesn't reach this address yet. Choose a morning delivery instead.")
	}
	return nil
}

// morningDeliveryDate decides the morning a one-off order is delivered on,
// validated against the IST calendar at `at`:
//
//   - the instant lane carries no day (deliveryDate "", nothing requested);
//   - a morning order that names no day (an older client, a direct API call)
//     is for the first morning still open to orders: tomorrow before noon,
//     the day after tomorrow from noon (undated, its task used to ride the
//     very next route and skip the noon cut-off);
//   - THE NOON CUT-OFF (One Voice 1.2, "Order by 12 noon, delivery by 7 AM";
//     owner, 24 Sep, R2): a named morning that is already closed - tomorrow
//     from 12:00 IST today, today, or a past day - is MOVED to the first open
//     morning and the order is accepted (moved = true, requested = the day
//     asked for). It used to be refused with 422 CUTOFF_PASSED, a dead end
//     for the shipped cart, which always sends tomorrow. The same
//     lockedThroughDay the subscription sweep uses decides "closed";
//   - more than 7 days ahead is still refused (BAD_DELIVERY_DATE), and a
//     malformed date is a 400.
func morningDeliveryDate(lane, requestedRaw string, at time.Time) (deliveryDate, requested string, moved bool, err error) {
	if lane != "morning" {
		return "", "", false, nil
	}
	first := firstEditableDay(at)
	requested = strings.TrimSpace(requestedRaw)
	if requested == "" {
		return first, "", false, nil
	}
	if _, perr := time.ParseInLocation("2006-01-02", requested, istZone); perr != nil {
		return "", "", false, errBadRequest("delivery_date must be YYYY-MM-DD")
	}
	if requested > addDaysIST(istToday(at), 7) {
		return "", "", false, errUnprocessable("BAD_DELIVERY_DATE", "pick a morning between tomorrow and 7 days from now")
	}
	if requested < first {
		return first, requested, true, nil // closed at noon the day before, or past
	}
	return requested, requested, false, nil
}

func (s *service) listOrders(ctx context.Context, userID string) ([]order, error) {
	return s.repo.listOrders(ctx, userID)
}

func (s *service) getOrder(ctx context.Context, userID, orderID string) (*order, error) {
	return s.repo.findOrder(ctx, orderID, userID)
}

func (s *service) cancelOrder(ctx context.Context, userID, orderID string) (*order, error) {
	return s.cancelOrderAt(ctx, userID, orderID, s.now())
}

// errOrderLockedMessage is ORDER_LOCKED's message (G7): the words the apps
// show when a member's cancel comes after the noon cut-off.
const errOrderLockedMessage = "Orders lock at 12 noon the day before delivery, so this one can no longer be cancelled."

// memberCancelLocked reports whether a member's cancel of o comes too late
// at now (G7, owner, 24 Sep): a MORNING order (one-off or subscription) is
// fixed from 12:00 IST the day before its delivery day - the store has
// procured for it, the noon lock has funded the member's day with it
// reserved - so from then, and on the day itself, the member can no longer
// cancel it. Only the cancellable statuses are judged (anything else keeps
// its own error). The instant lane and an undated legacy morning order have
// no cut-off. Store, rider and operator cancels never come through here.
func memberCancelLocked(o *order, now time.Time) bool {
	if o == nil || o.Lane == "instant" || (o.Status != "placed" && o.Status != "confirmed") {
		return false
	}
	day := orderDeliveryDate(o)
	return day != "" && day <= lockedThroughDay(now)
}

// cancelOrderAt is the member's cancel at an explicit moment.
func (s *service) cancelOrderAt(ctx context.Context, userID, orderID string, now time.Time) (*order, error) {
	// G7: a morning order past its noon cut-off stays as it is.
	if cur, ferr := s.repo.findOrder(ctx, orderID, userID); ferr == nil && memberCancelLocked(cur, now) {
		return nil, errConflict("ORDER_LOCKED", errOrderLockedMessage)
	}
	// Guard: only placed/confirmed orders may cancel — the $in precondition makes
	// this atomic (no cancelling an order that just went out for delivery).
	o, err := s.repo.updateOrder(ctx, orderID, userID,
		bson.D{{Key: "status", Value: "cancelled"}},
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"placed", "confirmed"}}}}},
	)
	if err != nil {
		return nil, err
	}
	// A cancelled order must not leave a LIVE delivery task behind — otherwise a
	// rider could still deliver it, debit the wallet, and flip the order back to
	// delivered. Fail the task (guarded: never touch one already terminal).
	// (updateDelivery stamps updated_at itself; naming it here as well made
	// Mongo reject the $set as a path conflict, so the task silently stayed live.)
	if d, _ := s.repo.findDeliveryByOrder(ctx, orderID); d != nil && d.Status != "DELIVERED" && d.Status != "FAILED" {
		_, _ = s.repo.updateDelivery(ctx, d.ID,
			bson.D{
				{Key: "status", Value: "FAILED"},
				{Key: "failure_reason", Value: "Order cancelled by the customer"},
			},
			bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED", "FAILED"}}}}},
		)
	}
	return o, nil
}

// errAlreadyReviewedMessage is ALREADY_REVIEWED's message: what the app shows
// when a second, different rating is sent for an order already rated.
const errAlreadyReviewedMessage = "You have already rated this order."

func (s *service) reviewOrder(ctx context.Context, userID, orderID string, rating int, comment string) (*order, error) {
	if rating < 1 || rating > 5 {
		return nil, errBadRequest("rating must be 1–5")
	}
	// Only a DELIVERED order can be reviewed, and only once: the review: null
	// guard (missing or null) lets the first review win atomically, even
	// against a concurrent second one.
	o, err := s.repo.updateOrder(ctx, orderID, userID,
		bson.D{
			{Key: "review", Value: orderReview{Rating: rating, Comment: comment, CreatedAt: s.now().UTC()}},
			{Key: "can_review", Value: false},
		},
		bson.D{{Key: "status", Value: "delivered"}, {Key: "review", Value: nil}},
	)
	if err != nil {
		// Already rated. The same rating and comment again is the app
		// retrying after a timeout: answer the stored review (nothing is
		// emitted twice). A different one never replaces the first.
		if cur, ferr := s.repo.findOrder(ctx, orderID, userID); ferr == nil && cur.Review != nil {
			if cur.Review.Rating == rating && cur.Review.Comment == comment {
				return cur, nil
			}
			return nil, errConflict("ALREADY_REVIEWED", errAlreadyReviewedMessage)
		}
		return nil, err
	}
	// CRM (contract C6, inert unless CRM_ENABLED): rating.submitted. Best-effort.
	if crmEnabled() {
		if cid, cerr := primitive.ObjectIDFromHex(userID); cerr == nil {
			s.emitCRMEvent(ctx, "rating.submitted", cid, map[string]any{"order_id": orderID, "rating": rating})
		}
	}
	return o, nil
}

// advanceOrder is a DEV-only status transition (rider/store surfaces own the real
// transitions in a later phase). Gated by OTP dev mode so it never exists in prod.
func (s *service) advanceOrder(ctx context.Context, userID, orderID, status string) (*order, error) {
	if !s.deps.Cfg.OTPDevMode {
		return nil, errForbidden("not available")
	}
	set := bson.D{{Key: "status", Value: status}}
	switch status {
	case "out_for_delivery":
		demoID := "rider-demo"
		veh := "Bike · UP32 CD 5678"
		rating := 4.8
		lat, lng := 26.8467, 80.9462
		set = append(set,
			bson.E{Key: "rider_id", Value: &demoID},
			bson.E{Key: "riders", Value: &rider{ID: demoID, FullName: "Ram Kumar", Phone: "+919999900000", Vehicle: &veh, Rating: &rating, CurrentLat: &lat, CurrentLng: &lng}},
		)
	case "delivered":
		set = append(set, bson.E{Key: "can_review", Value: true})
	}
	return s.repo.updateOrder(ctx, orderID, userID, set, bson.D{})
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *handler) createOrder(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in orderInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	o, err := h.svc.createOrder(r.Context(), id.Hex(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

func (h *handler) listOrders(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	// Scoped to the token's shopper — the ?user_id= query is ignored so one
	// shopper can never read another's orders.
	orders, err := h.svc.listOrders(r.Context(), id.Hex())
	if err != nil {
		writeErr(w, err)
		return
	}
	// The list carries NO proof photo. The stored value is a private-bucket path
	// no app can load, and signing every one of up to 200 rows serially on a
	// list the app polls every 15 s was the slowest thing on the path. The tag
	// is omitempty so the key disappears; the tracking screen reads the signed
	// URL from GET /orders/{id}, which still signs it.
	for i := range orders {
		orders[i].ProofPhotoURL = ""
	}
	writeJSON(w, http.StatusOK, orders)
}

func (h *handler) getOrder(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	o, err := h.svc.getOrder(r.Context(), id.Hex(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// Tracking screen: the proof photo lives in a private bucket — hand the app
	// a short-lived URL for this one file — and the map's trip origin is the
	// pickup point the rider leaves from.
	o.ProofPhotoURL = h.svc.proofPhotoURL(r.Context(), o.ProofPhotoURL)
	if o.StoreLat == nil && o.Rider != nil {
		p := h.svc.pickupPoint(r.Context())
		o.StoreLat, o.StoreLng = &p.Lat, &p.Lng
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	o, err := h.svc.cancelOrder(r.Context(), id.Hex(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *handler) reviewOrder(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Rating  int    `json:"rating"`
		Comment string `json:"comment"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	o, err := h.svc.reviewOrder(r.Context(), id.Hex(), chi.URLParam(r, "id"), body.Rating, body.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// payOrder — POST /orders/{id}/pay: create an amount-bound Razorpay order for
// the order total so the FE can pay it directly via the gateway (seam). The
// amount is the server-side order total; dev-gated in the service.
func (h *handler) payOrder(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.svc.createOrderPayment(r.Context(), id, chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) advanceOrder(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	o, err := h.svc.advanceOrder(r.Context(), id.Hex(), chi.URLParam(r, "id"), body.Status)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

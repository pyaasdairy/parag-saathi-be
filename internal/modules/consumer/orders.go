package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
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

// Delivery-fee policy — kept identical to the FE (lib/api.ts).
const freeDeliveryOver = 199.0
const deliveryFee = 15.0

// Fair-use order caps — kept identical to the FE (lib/pricing.ts).
const maxQtyPerProduct = 10
const maxItemsPerOrder = 30

func deliveryFeeFor(subtotal float64) float64 {
	if subtotal >= freeDeliveryOver || subtotal == 0 {
		return 0
	}
	return deliveryFee
}

// Statuses a placed order may still be cancelled from.
var cancellableStatuses = map[string]bool{"placed": true, "confirmed": true}

// ── Documents (bson) + wire shape (json matches the FE Order) ────────────────

type orderItem struct {
	ID        string  `bson:"id"         json:"id"`
	ProductID string  `bson:"product_id" json:"product_id"`
	Name      string  `bson:"name"       json:"name"`
	Variant   string  `bson:"variant"    json:"variant"`
	Price     float64 `bson:"price"      json:"price"`
	Qty       int     `bson:"qty"        json:"qty"`
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
	RiderID        *string            `bson:"rider_id"                json:"rider_id"`
	PlacedAt       time.Time          `bson:"placed_at"               json:"placed_at"`
	Priority       string             `bson:"priority,omitempty"      json:"priority,omitempty"`
	DeliveryWindow string             `bson:"delivery_window,omitempty" json:"delivery_window,omitempty"`
	ProofPhotoURL  string             `bson:"proof_photo_url,omitempty" json:"proof_photo_url,omitempty"`
	Lane           string             `bson:"lane"                    json:"lane,omitempty"`
	Items          []orderItem        `bson:"order_items"             json:"order_items"`
	Rider          *rider             `bson:"riders,omitempty"        json:"riders"`
	CanReview      bool               `bson:"can_review"              json:"can_review"`
	Review         *orderReview       `bson:"review,omitempty"        json:"review"`
	// Owner-facing / delivery metadata — never sent back to the shopper client.
	ConsumerName string    `bson:"consumer_name,omitempty" json:"-"`
	Phone        string    `bson:"phone,omitempty"         json:"-"`
	Geo          *geoPoint `bson:"geo,omitempty"           json:"-"`
	CreatedAt    time.Time `bson:"created_at"              json:"-"`
	UpdatedAt    time.Time `bson:"updated_at"              json:"-"`
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
	Priority       string      `json:"priority"`
	DeliveryWindow string      `json:"delivery_window"`
	Total          float64     `json:"total"` // client total (may carry a coupon discount)
	Lane           string      `json:"lane"`
	ConsumerName   string      `json:"consumer_name"`
	Phone          string      `json:"phone"`
	Geo            *geoPoint   `json:"geo"`
}

func (s *service) createOrder(ctx context.Context, userID string, in orderInput) (*order, error) {
	if len(in.Items) == 0 {
		return nil, errBadRequest("an order needs at least one item")
	}
	// Money is recomputed SERVER-SIDE from the items; the client-supplied `total`
	// is IGNORED (an authoritative total = subtotal + fee). A client cannot deflate
	// or inflate the bill. NOTE: unit prices are still taken from the item lines
	// pending a server-side product catalogue (a follow-up) — until then a coupon
	// discount is not applied server-side, so total = gross.
	var subtotal float64
	var units int
	items := make([]orderItem, 0, len(in.Items))
	for _, it := range in.Items {
		if it.Qty <= 0 || it.Price < 0 {
			return nil, errBadRequest("invalid order item")
		}
		if it.Qty > maxQtyPerProduct {
			return nil, errBadRequest("quantity per item exceeds the limit")
		}
		units += it.Qty
		subtotal += it.Price * float64(it.Qty)
		items = append(items, orderItem{
			ID: newItemID(), ProductID: it.ProductID, Name: it.Name, Variant: it.Variant, Price: round2(it.Price), Qty: it.Qty,
		})
	}
	if units > maxItemsPerOrder {
		return nil, errBadRequest("too many items in one order")
	}
	subtotal = round2(subtotal)
	fee := deliveryFeeFor(subtotal)
	total := round2(subtotal + fee)
	// Payment mode defaults to 'wallet' — the order is settled from the server
	// wallet on delivery (the settle sweep debits /wallet/debit, idempotent by
	// order id). A client may pass another mode (e.g. 'cod', or 'gateway' once the
	// /orders/{id}/pay direct-pay seam is wired to verify).
	pm := in.PaymentMethod
	if pm == "" {
		pm = "wallet"
	}
	// Delivery lane: "morning" (5–7:30 subscription run) is the DEFAULT — the
	// instant ≈20-min lane is an explicit, validated opt-in (an unknown value
	// must never accidentally mint an instant ETA).
	lane := in.Lane
	if lane != "instant" {
		lane = "morning"
	}
	// Monsoon surcharge: INSTANT orders only, and only if the store manager enabled
	// it on the delivery location's zone. Authoritative here — read from the zone,
	// never trusted from the client payload, so a tampered client cannot skip it.
	monsoonFee := 0.0
	if lane == "instant" && in.Geo != nil {
		if sv, sErr := s.serviceability(ctx, in.Geo.Lat, in.Geo.Lng, ""); sErr == nil {
			// Store shut for the night / paused → refuse instant (defence in depth;
			// the consumer already hides it, but a stale client must not slip through).
			if sv.InstantClosed {
				return nil, errUnprocessable("INSTANT_CLOSED", "instant delivery is closed right now; please choose the morning slot")
			}
			if sv.MonsoonEnabled && sv.MonsoonRupees > 0 {
				monsoonFee = float64(sv.MonsoonRupees)
			}
		}
	}
	total = round2(total + monsoonFee)
	priority := in.Priority
	if priority == "" {
		priority = "normal"
	}
	now := time.Now().UTC()
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: userID, Status: "placed",
		Subtotal: subtotal, DeliveryFee: fee, MonsoonFee: monsoonFee, Total: total, PaymentMethod: pm,
		AddressLabel: in.AddressLabel, AddressText: in.AddressText, RiderID: nil,
		PlacedAt: now, Priority: priority, DeliveryWindow: in.DeliveryWindow, Lane: lane,
		Items: items, Rider: nil, CanReview: false, Review: nil,
		ConsumerName: in.ConsumerName, Phone: in.Phone, Geo: in.Geo, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.insertOrder(ctx, o); err != nil {
		return nil, err
	}
	// Create the last-mile delivery task (routed to the nearest Parag Store,
	// unassigned until a store manager assigns a rider). Best-effort.
	s.createDeliveryForOrder(ctx, o)
	return o, nil
}

func (s *service) listOrders(ctx context.Context, userID string) ([]order, error) {
	return s.repo.listOrders(ctx, userID)
}

func (s *service) getOrder(ctx context.Context, userID, orderID string) (*order, error) {
	return s.repo.findOrder(ctx, orderID, userID)
}

func (s *service) cancelOrder(ctx context.Context, userID, orderID string) (*order, error) {
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
	if d, _ := s.repo.findDeliveryByOrder(ctx, orderID); d != nil && d.Status != "DELIVERED" && d.Status != "FAILED" {
		_, _ = s.repo.updateDelivery(ctx, d.ID,
			bson.D{
				{Key: "status", Value: "FAILED"},
				{Key: "failure_reason", Value: "Order cancelled by the customer"},
				{Key: "updated_at", Value: time.Now().UTC()},
			},
			bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED", "FAILED"}}}}},
		)
	}
	return o, nil
}

func (s *service) reviewOrder(ctx context.Context, userID, orderID string, rating int, comment string) (*order, error) {
	if rating < 1 || rating > 5 {
		return nil, errBadRequest("rating must be 1–5")
	}
	// Only a DELIVERED order can be reviewed.
	return s.repo.updateOrder(ctx, orderID, userID,
		bson.D{
			{Key: "review", Value: orderReview{Rating: rating, Comment: comment, CreatedAt: time.Now().UTC()}},
			{Key: "can_review", Value: false},
		},
		bson.D{{Key: "status", Value: "delivered"}},
	)
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

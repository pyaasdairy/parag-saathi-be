package consumer

import (
	"context"
	"crypto/subtle"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// ADMIN DELIVERY CRM — read-mostly visibility over every consumer order, its
// delivery, the riders and the subscriptions, plus the pickup-point setting
// and two ops actions (reassign, cancel). Mounted at /consumer/admin.
//
// Auth, either:
//   - a SUPER_ADMIN role token (the Saathi app's admin console), or
//   - X-Admin-Key: <ADMIN_API_KEY> from a SERVER (the pyaasdairy.com/admin
//     Next.js route). The key must be ≥ 32 characters; without ADMIN_API_KEY
//     set, key auth is off and only the role token works.
//
// Wire format is the operator {data} envelope, camelCase fields.

const adminKeyActorID = "admin-api-key"

// adminKeyMinLen — a shared key guarding the customer book has to be long
// enough that guessing it is hopeless. Anything shorter is refused outright
// (fail closed), and module.go logs which door is open at boot.
const adminKeyMinLen = 32

func adminGate(jwtm *auth.JWTManager) func(http.Handler) http.Handler {
	viaToken := func(next http.Handler) http.Handler {
		return middleware.Authenticate(jwtm)(middleware.RequireRoles()(next)) // SUPER_ADMIN only
	}
	return func(next http.Handler) http.Handler {
		tokenChain := viaToken(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Admin-Key")
			if key == "" {
				tokenChain.ServeHTTP(w, r)
				return
			}
			want := os.Getenv("ADMIN_API_KEY")
			if len(want) < adminKeyMinLen || subtle.ConstantTimeCompare([]byte(key), []byte(want)) != 1 {
				httpx.Error(w, r, httpx.Unauthorized("invalid admin key"))
				return
			}
			actor := auth.Actor{PartyID: adminKeyActorID, Kind: "service", RoleCode: "SUPER_ADMIN"}
			next.ServeHTTP(w, r.WithContext(auth.WithActor(r.Context(), actor)))
		})
	}
}

// ── Rows ────────────────────────────────────────────────────────────────────

type crmPerson struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Phone string `json:"phone,omitempty"`
}

type crmItem struct {
	ProductID     string  `json:"productId"`
	Name          string  `json:"name"`
	Variant       string  `json:"variant,omitempty"`
	Qty           int     `json:"qty"`
	Price         float64 `json:"price"`
	LineTotal     float64 `json:"lineTotal"`
	IsPromotional bool    `json:"isPromotional,omitempty"`
}

type crmOrderRow struct {
	OrderID          string     `json:"orderId"`
	PlacedAt         time.Time  `json:"placedAt"`
	DeliveryDate     string     `json:"deliveryDate,omitempty"`
	Type             string     `json:"type"` // subscription | one_time | offer
	SubscriptionID   string     `json:"subscriptionId,omitempty"`
	Lane             string     `json:"lane,omitempty"`
	Slot             string     `json:"slot,omitempty"`
	OrderStatus      string     `json:"orderStatus"`    // the customer-facing status
	DeliveryStatus   string     `json:"deliveryStatus"` // NO_TASK | TO_PICK_UP | ON_THE_WAY | DELIVERED | FAILED
	DeliveryID       string     `json:"deliveryId,omitempty"`
	Customer         crmPerson  `json:"customer"`
	AddressLabel     string     `json:"addressLabel,omitempty"`
	Address          string     `json:"address"`
	AddressGeo       *geoPt     `json:"addressGeo,omitempty"`
	Items            []crmItem  `json:"items"`
	ItemsCount       int        `json:"itemsCount"`
	Subtotal         float64    `json:"subtotal"`
	DeliveryFee      float64    `json:"deliveryFee"`
	MonsoonFee       float64    `json:"monsoonFee,omitempty"`
	Total            float64    `json:"total"`
	PaymentMethod    string     `json:"paymentMethod"`
	PaymentStatus    string     `json:"paymentStatus"` // PENDING | PAID | FREE | COD | NOT_CHARGED
	TrialFree        bool       `json:"trialFree,omitempty"`
	OfferPack        int        `json:"offerPack,omitempty"`
	Rider            *crmPerson `json:"rider,omitempty"`
	AssignedAt       string     `json:"assignedAt,omitempty"`
	StartedAt        string     `json:"startedAt,omitempty"`
	DeliveredAt      string     `json:"deliveredAt,omitempty"`
	MinutesToDeliver *int       `json:"minutesToDeliver,omitempty"`
	PickupAddress    string     `json:"pickupAddress,omitempty"`
	HasProofPhoto    bool       `json:"hasProofPhoto"`
	ProofDistanceM   *float64   `json:"proofDistanceM,omitempty"`
	FailureReason    string     `json:"failureReason,omitempty"`
	Rating           int        `json:"rating,omitempty"`
	ReviewComment    string     `json:"reviewComment,omitempty"`
}

func crmDeliveryStatus(d *delivery) string {
	if d == nil {
		return "NO_TASK"
	}
	switch d.Status {
	case "OUT_FOR_DELIVERY":
		return "ON_THE_WAY"
	case "DELIVERED", "FAILED":
		return d.Status
	default:
		return "TO_PICK_UP"
	}
}

func (s *service) crmRow(o *order, d *delivery, paid map[string]float64, names map[string]crmPerson) crmOrderRow {
	row := crmOrderRow{
		OrderID: o.OrderID, PlacedAt: o.PlacedAt, DeliveryDate: orderDeliveryDate(o),
		SubscriptionID: o.SubscriptionID, Lane: o.Lane, Slot: slotLabel(o),
		OrderStatus: o.Status, DeliveryStatus: crmDeliveryStatus(d),
		Customer:     crmPerson{ID: o.UserID, Name: o.ConsumerName, Phone: o.Phone},
		AddressLabel: o.AddressLabel, Address: o.AddressText,
		Subtotal: o.Subtotal, DeliveryFee: o.DeliveryFee, MonsoonFee: o.MonsoonFee, Total: o.Total,
		PaymentMethod: o.PaymentMethod, TrialFree: o.TrialFree, OfferPack: o.OfferPack,
		Items: []crmItem{},
	}
	switch {
	case o.OfferPack > 0:
		row.Type = "offer"
	case o.SubscriptionID != "":
		row.Type = "subscription"
	default:
		row.Type = "one_time"
	}
	if o.Geo != nil {
		row.AddressGeo = &geoPt{Lat: o.Geo.Lat, Lng: o.Geo.Lng}
	}
	if p, ok := names["c:"+o.UserID]; ok {
		if row.Customer.Name == "" {
			row.Customer.Name = p.Name
		}
		if row.Customer.Phone == "" {
			row.Customer.Phone = p.Phone
		}
	}
	for _, it := range o.Items {
		row.Items = append(row.Items, crmItem{
			ProductID: it.ProductID, Name: it.Name, Variant: it.Variant, Qty: it.Qty, Price: it.Price,
			LineTotal: round2(it.Price * float64(it.Qty)), IsPromotional: it.IsPromotional,
		})
		row.ItemsCount += it.Qty
	}
	if o.Review != nil {
		row.Rating, row.ReviewComment = o.Review.Rating, o.Review.Comment
	}
	// Payment truth comes from the ledger ("delivery:<order>" ref), not from
	// what the order document claims.
	amountPaid, charged := paid[o.OrderID]
	switch {
	case charged && amountPaid > 0:
		row.PaymentStatus = "PAID"
	case charged:
		row.PaymentStatus = "FREE"
	case o.Status == "cancelled":
		row.PaymentStatus = "NOT_CHARGED"
	case d != nil && d.PaymentMode == "COD" && d.Status == "DELIVERED":
		row.PaymentStatus = "COD"
	default:
		row.PaymentStatus = "PENDING"
	}
	if d != nil {
		row.DeliveryID = d.ID
		row.AssignedAt, row.StartedAt, row.DeliveredAt = d.AssignedAt, d.OutForDeliveryAt, d.DeliveredAt
		row.PickupAddress, row.HasProofPhoto, row.ProofDistanceM = d.PickupAddress, d.ProofPhotoURI != "", d.ProofDistanceM
		row.FailureReason = d.FailureReason
		if d.RiderPartyID != "" {
			p := names["r:"+d.RiderPartyID]
			p.ID = d.RiderPartyID
			row.Rider = &p
		}
		if st, e1 := time.Parse(time.RFC3339, d.OutForDeliveryAt); e1 == nil {
			if dt, e2 := time.Parse(time.RFC3339, d.DeliveredAt); e2 == nil {
				m := int(dt.Sub(st).Minutes())
				row.MinutesToDeliver = &m
			}
		}
	}
	return row
}

// crmHydrate batch-loads the deliveries, ledger debits and names behind a page
// of orders — a fixed number of queries whatever the page size.
func (s *service) crmHydrate(ctx context.Context, orders []order) []crmOrderRow {
	ids := make([]string, 0, len(orders))
	refs := make([]string, 0, len(orders))
	consumerIDs := bson.A{}
	for i := range orders {
		ids = append(ids, orders[i].OrderID)
		refs = append(refs, "delivery:"+orders[i].OrderID)
		if oid, err := primitive.ObjectIDFromHex(orders[i].UserID); err == nil {
			consumerIDs = append(consumerIDs, oid)
		}
	}
	dels := map[string]*delivery{}
	if cur, err := s.repo.deliveries.Find(ctx, bson.D{{Key: "order_id", Value: bson.D{{Key: "$in", Value: ids}}}}); err == nil {
		var rows []delivery
		if cur.All(ctx, &rows) == nil {
			for i := range rows {
				dels[rows[i].OrderID] = &rows[i]
			}
		}
	}
	paid := map[string]float64{}
	if cur, err := s.repo.walletTxns.Find(ctx, bson.D{
		{Key: "ref_id", Value: bson.D{{Key: "$in", Value: refs}}}, {Key: "type", Value: "DEBIT"},
	}); err == nil {
		var rows []walletTxn
		if cur.All(ctx, &rows) == nil {
			for _, t := range rows {
				oid := strings.TrimPrefix(t.RefID, "delivery:")
				paid[oid] = round2(paid[oid] + t.Amount)
			}
		}
	}
	names := map[string]crmPerson{}
	if len(consumerIDs) > 0 {
		if cur, err := s.repo.accounts.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: consumerIDs}}}}); err == nil {
			var rows []account
			if cur.All(ctx, &rows) == nil {
				for _, a := range rows {
					p := crmPerson{ID: a.ID.Hex(), Phone: a.Phone}
					if a.FullName != nil {
						p.Name = *a.FullName
					}
					names["c:"+a.ID.Hex()] = p
				}
			}
		}
	}
	for _, d := range dels {
		if d.RiderPartyID != "" {
			if _, seen := names["r:"+d.RiderPartyID]; !seen {
				n, ph := s.repo.riderName(ctx, d.RiderPartyID)
				names["r:"+d.RiderPartyID] = crmPerson{Name: n, Phone: ph}
			}
		}
	}
	out := make([]crmOrderRow, 0, len(orders))
	for i := range orders {
		out = append(out, s.crmRow(&orders[i], dels[orders[i].OrderID], paid, names))
	}
	return out
}

// ── Queries ─────────────────────────────────────────────────────────────────

// crmRange parses ?from=&to= (IST YYYY-MM-DD, inclusive) into UTC bounds;
// defaults to the last 30 days.
func crmRange(r *http.Request, now time.Time) (from, to string, start, end time.Time, err error) {
	to = strings.TrimSpace(r.URL.Query().Get("to"))
	from = strings.TrimSpace(r.URL.Query().Get("from"))
	if to == "" {
		to = istToday(now)
	}
	if from == "" {
		from = addDaysIST(to, -29)
	}
	s, _, e1 := istDayBounds(from)
	_, e, e2 := istDayBounds(to)
	if e1 != nil || e2 != nil || e.Before(s) {
		return "", "", time.Time{}, time.Time{}, httpx.BadRequest("INVALID_RANGE", "from/to must be YYYY-MM-DD with from ≤ to")
	}
	return from, to, s, e, nil
}

func crmPage(r *http.Request) (page, limit int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	return page, limit
}

func (h *handler) crmOrders(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	from, to, start, end, err := crmRange(r, time.Now())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	filter := bson.D{}
	if q.Get("date_field") == "delivery" {
		filter = append(filter, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "delivery_date", Value: bson.D{{Key: "$gte", Value: from}, {Key: "$lte", Value: to}}}},
			bson.D{{Key: "scheduled_for", Value: bson.D{{Key: "$gte", Value: from}, {Key: "$lte", Value: to}}}},
		}})
	} else {
		filter = append(filter, bson.E{Key: "placed_at", Value: bson.D{{Key: "$gte", Value: start}, {Key: "$lt", Value: end}}})
	}
	if st := q.Get("status"); st != "" {
		filter = append(filter, bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: strings.Split(st, ",")}}})
	}
	switch q.Get("type") {
	case "subscription":
		filter = append(filter, bson.E{Key: "subscription_id", Value: bson.D{{Key: "$gt", Value: ""}}})
	case "one_time":
		filter = append(filter, bson.E{Key: "subscription_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
			bson.E{Key: "offer_pack", Value: bson.D{{Key: "$in", Value: bson.A{nil, 0}}}})
	case "offer":
		filter = append(filter, bson.E{Key: "offer_pack", Value: bson.D{{Key: "$gt", Value: 0}}})
	}
	if lane := q.Get("lane"); lane != "" {
		filter = append(filter, bson.E{Key: "lane", Value: lane})
	}
	if search := strings.TrimSpace(q.Get("q")); search != "" {
		rx := primitive.Regex{Pattern: regexp.QuoteMeta(search), Options: "i"}
		filter = append(filter, bson.E{Key: "$and", Value: bson.A{bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "order_id", Value: rx}}, bson.D{{Key: "phone", Value: rx}},
			bson.D{{Key: "consumer_name", Value: rx}}, bson.D{{Key: "address_text", Value: rx}},
		}}}}})
	}
	if rider := q.Get("rider"); rider != "" {
		ids := []string{}
		if cur, e := h.svc.repo.deliveries.Find(ctx, bson.D{{Key: "rider_party_id", Value: rider}},
			options.Find().SetProjection(bson.D{{Key: "order_id", Value: 1}}).SetLimit(5000)); e == nil {
			var rows []struct {
				OrderID string `bson:"order_id"`
			}
			if cur.All(ctx, &rows) == nil {
				for _, x := range rows {
					ids = append(ids, x.OrderID)
				}
			}
		}
		filter = append(filter, bson.E{Key: "order_id", Value: bson.D{{Key: "$in", Value: ids}}})
	}
	page, limit := crmPage(r)
	total, err := h.svc.repo.orders.CountDocuments(ctx, filter)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("orders count failed")))
		return
	}
	cur, err := h.svc.repo.orders.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "placed_at", Value: -1}}).SetSkip(int64((page-1)*limit)).SetLimit(int64(limit)))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("orders lookup failed")))
		return
	}
	var orders []order
	if err := cur.All(ctx, &orders); err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("orders decode failed")))
		return
	}
	rows := h.svc.crmHydrate(ctx, orders)
	// Delivery-state filter applies to the hydrated page (it lives on the task).
	if ds := q.Get("delivery_status"); ds != "" {
		want := map[string]bool{}
		for _, x := range strings.Split(ds, ",") {
			want[x] = true
		}
		kept := rows[:0]
		for _, row := range rows {
			if want[row.DeliveryStatus] {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": rows, "total": total, "page": page, "limit": limit, "from": from, "to": to,
	})
}

type crmTimelineEvent struct {
	At    string `json:"at"`
	Event string `json:"event"`
	By    string `json:"by,omitempty"`
}

func (h *handler) crmOrderDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := h.svc.repo.findOrderAnyUser(ctx, chi.URLParam(r, "orderId"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	row := h.svc.crmHydrate(ctx, []order{*o})[0]
	d, _ := h.svc.repo.findDeliveryByOrder(ctx, o.OrderID)

	timeline := []crmTimelineEvent{{At: o.PlacedAt.UTC().Format(time.RFC3339), Event: "Order placed", By: "customer"}}
	if o.SubLockedAt != "" {
		timeline = append(timeline, crmTimelineEvent{At: o.SubLockedAt, Event: "Subscription day confirmed (midnight lock)", By: "system"})
	}
	detail := map[string]any{"order": row, "deliveryPrefs": o.DeliveryPrefs, "buyerGstin": o.BuyerGSTIN}
	if d != nil {
		rider := ""
		if row.Rider != nil {
			rider = row.Rider.Name
		}
		if d.CreatedAt.After(time.Time{}) {
			timeline = append(timeline, crmTimelineEvent{At: d.CreatedAt.UTC().Format(time.RFC3339), Event: "Sent to riders", By: "system"})
		}
		if d.AcceptedAt != "" {
			timeline = append(timeline, crmTimelineEvent{At: d.AcceptedAt, Event: "Rider accepted", By: rider})
		}
		if d.OutForDeliveryAt != "" {
			timeline = append(timeline, crmTimelineEvent{At: d.OutForDeliveryAt, Event: "Picked up · out for delivery", By: rider})
		}
		if d.DeliveredAt != "" {
			timeline = append(timeline, crmTimelineEvent{At: d.DeliveredAt, Event: "Delivered (photo taken)", By: rider})
		}
		if d.Status == "FAILED" {
			timeline = append(timeline, crmTimelineEvent{At: d.UpdatedAt.UTC().Format(time.RFC3339), Event: "Not delivered: " + d.FailureReason})
		}
		detail["delivery"] = map[string]any{
			"id": d.ID, "status": d.Status, "storeId": d.StoreID, "paymentMode": d.PaymentMode,
			"pickupAddress": d.PickupAddress, "pickupGeo": d.PickupGeo,
			"addressGeo": d.Geo, "proofGeo": d.ProofGeo, "proofDistanceM": d.ProofDistanceM,
			"lastKnownGeo": d.LastKnownGeo, "lastLocationAt": d.LastLocationAt,
			"proofNote": d.ProofNote, "proofPhotoUrl": h.svc.proofPhotoURL(ctx, d.ProofPhotoURI),
			"geofenceOk": d.ProofGeo != nil && d.ProofDistanceM != nil && *d.ProofDistanceM <= deliveryGeofenceMeters,
		}
	}
	if o.Status == "cancelled" {
		timeline = append(timeline, crmTimelineEvent{At: o.UpdatedAt.UTC().Format(time.RFC3339), Event: "Cancelled"})
	}
	if o.Review != nil {
		timeline = append(timeline, crmTimelineEvent{At: o.Review.CreatedAt.UTC().Format(time.RFC3339), Event: "Customer rated " + strconv.Itoa(o.Review.Rating) + "/5", By: "customer"})
	}
	detail["timeline"] = sortTimeline(timeline)

	if cid, cerr := primitive.ObjectIDFromHex(o.UserID); cerr == nil {
		if acct, _ := h.svc.repo.findAccountByID(ctx, cid); acct != nil {
			name := ""
			if acct.FullName != nil {
				name = *acct.FullName
			}
			customer := map[string]any{"id": acct.ID.Hex(), "name": name, "phone": acct.Phone, "joinedAt": acct.CreatedAt, "status": acct.Status}
			if acct.Email != nil {
				customer["email"] = *acct.Email
			}
			if wv, werr := h.svc.wallet(ctx, cid); werr == nil {
				customer["walletAvailable"] = wv.Available
			}
			detail["customer"] = customer
		}
		var txns []walletTxn
		if cur, e := h.svc.repo.walletTxns.Find(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "ref_id", Value: "delivery:" + o.OrderID}}); e == nil {
			_ = cur.All(ctx, &txns)
		}
		ledger := []map[string]any{}
		for _, t := range txns {
			ledger = append(ledger, map[string]any{"type": t.Type, "bucket": t.Bucket, "amount": t.Amount, "remark": t.Remark, "at": t.CreatedAt})
		}
		detail["payments"] = ledger
	}
	if o.SubscriptionID != "" {
		if sub, _ := h.svc.repo.findSubscriptionByID(ctx, o.SubscriptionID); sub != nil {
			detail["subscription"] = crmSubscriptionView(sub)
		}
	}
	httpx.JSON(w, http.StatusOK, detail)
}

func sortTimeline(ev []crmTimelineEvent) []crmTimelineEvent {
	for i := 1; i < len(ev); i++ {
		for j := i; j > 0 && ev[j].At < ev[j-1].At; j-- {
			ev[j], ev[j-1] = ev[j-1], ev[j]
		}
	}
	return ev
}

func crmSubscriptionView(sub *subscription) map[string]any {
	return map[string]any{
		"id": sub.SubscriptionID, "customerId": sub.ConsumerID.Hex(), "productId": sub.ProductID, "name": sub.Name,
		"variant": sub.Variant, "qty": sub.Qty, "unitPrice": sub.UnitPrice, "perDelivery": round2(sub.UnitPrice * float64(sub.Qty)),
		"frequency": sub.Frequency, "status": sub.Status, "startDate": sub.StartDate, "vacations": sub.Vacations,
		"lastOrderDate": sub.LastOrderDate, "lastOrderId": sub.LastOrderID, "createdAt": sub.CreatedAt, "updatedAt": sub.UpdatedAt,
	}
}

func (h *handler) crmSummary(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), time.Now()
	from, to, start, end, err := crmRange(r, now)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// The dashboard aggregates in Go, so the cap is what stands between a wide
	// date range and the whole order history in one process. Sort NEWEST FIRST
	// so a truncated window is the most recent slice rather than an arbitrary
	// one, and tell the caller when it happened — a silently clipped total is a
	// wrong number presented as a right one.
	const summaryMaxOrders = 20000
	cur, err := h.svc.repo.orders.Find(ctx, bson.D{{Key: "placed_at", Value: bson.D{{Key: "$gte", Value: start}, {Key: "$lt", Value: end}}}},
		options.Find().SetSort(bson.D{{Key: "placed_at", Value: -1}}).SetLimit(summaryMaxOrders))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("orders lookup failed")))
		return
	}
	var orders []order
	_ = cur.All(ctx, &orders)
	truncated := len(orders) >= summaryMaxOrders
	rows := h.svc.crmHydrate(ctx, orders)

	byStatus, byDelivery, byType, byPayment := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	var orderValue, deliveredValue float64
	type riderStat struct {
		PartyID          string `json:"partyId"`
		Name             string `json:"name"`
		Delivered        int    `json:"delivered"`
		OnTheWay         int    `json:"onTheWay"`
		AvgMins          *int   `json:"avgMinutesToDeliver,omitempty"`
		totalMins, timed int
	}
	riders := map[string]*riderStat{}
	perDay := map[string]map[string]int{}
	for _, row := range rows {
		byStatus[row.OrderStatus]++
		byDelivery[row.DeliveryStatus]++
		byType[row.Type]++
		byPayment[row.PaymentStatus]++
		if row.OrderStatus != "cancelled" {
			orderValue += row.Total
		}
		if row.DeliveryStatus == "DELIVERED" {
			deliveredValue += row.Total
		}
		day := row.PlacedAt.In(istZone).Format("2006-01-02")
		if perDay[day] == nil {
			perDay[day] = map[string]int{}
		}
		perDay[day]["placed"]++
		if row.DeliveryStatus == "DELIVERED" {
			perDay[day]["delivered"]++
		}
		if row.Rider != nil {
			st := riders[row.Rider.ID]
			if st == nil {
				st = &riderStat{PartyID: row.Rider.ID, Name: row.Rider.Name}
				riders[row.Rider.ID] = st
			}
			switch row.DeliveryStatus {
			case "DELIVERED":
				st.Delivered++
				if row.MinutesToDeliver != nil {
					st.totalMins += *row.MinutesToDeliver
					st.timed++
				}
			case "ON_THE_WAY":
				st.OnTheWay++
			}
		}
	}
	riderList := []*riderStat{}
	for _, st := range riders {
		if st.timed > 0 {
			avg := st.totalMins / st.timed
			st.AvgMins = &avg
		}
		riderList = append(riderList, st)
	}
	days := []map[string]any{}
	for d := from; d <= to; d = addDaysIST(d, 1) {
		days = append(days, map[string]any{"date": d, "placed": perDay[d]["placed"], "delivered": perDay[d]["delivered"]})
	}
	subs := map[string]int64{}
	for _, st := range []string{"active", "paused", "cancelled"} {
		n, _ := h.svc.repo.subscriptions.CountDocuments(ctx, bson.D{{Key: "status", Value: st}})
		subs[st] = n
	}
	openNow, _ := h.svc.repo.deliveries.CountDocuments(ctx, bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: deliveryOpenStatuses}}}})
	unassignedNow, _ := h.svc.repo.deliveries.CountDocuments(ctx, bson.D{
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"ASSIGNED", "OFFERED"}}}}, {Key: "rider_party_id", Value: ""},
	})
	httpx.JSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "ordersTotal": len(rows),
		"ordersByStatus": byStatus, "deliveriesByStatus": byDelivery, "ordersByType": byType, "ordersByPayment": byPayment,
		"orderValue": round2(orderValue), "deliveredValue": round2(deliveredValue),
		"riders": riderList, "perDay": days, "subscriptions": subs,
		"liveOpenDeliveries": openNow, "liveUnassignedDeliveries": unassignedNow,
		"pickup": h.svc.pickupPoint(ctx),
		// true = the range held more orders than one dashboard call reads, so
		// every total above covers only the most recent summaryMaxOrders. The
		// website must say so rather than print a short number as the truth.
		"truncated": truncated,
	})
}

func (h *handler) crmRiders(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), time.Now()
	cur, err := h.svc.repo.roleAssignments.Find(ctx, bson.D{{Key: "role_code", Value: "DELIVERY_RIDER"}, {Key: "status", Value: "ACTIVE"}})
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("riders lookup failed")))
		return
	}
	var ras []struct {
		PartyID primitive.ObjectID `bson:"party_id"`
	}
	_ = cur.All(ctx, &ras)
	dayStart, _, _ := istDayBounds(istToday(now))
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, ra := range ras {
		id := ra.PartyID.Hex()
		if seen[id] {
			continue
		}
		seen[id] = true
		name, phone := h.svc.repo.riderName(ctx, id)
		row := map[string]any{"partyId": id, "name": name, "phone": phone}
		if g, at, ok := h.svc.repo.findRiderPresence(ctx, id); ok {
			row["lastLocation"] = g
			row["lastSeenAt"] = at
			row["online"] = at >= now.UTC().Add(-offerPingFreshness).Format(time.RFC3339)
		} else {
			row["online"] = false
		}
		onTheWay, _ := h.svc.repo.deliveries.CountDocuments(ctx, bson.D{{Key: "rider_party_id", Value: id}, {Key: "status", Value: "OUT_FOR_DELIVERY"}})
		deliveredToday, _ := h.svc.repo.deliveries.CountDocuments(ctx, bson.D{
			{Key: "rider_party_id", Value: id}, {Key: "status", Value: "DELIVERED"}, {Key: "updated_at", Value: bson.D{{Key: "$gte", Value: dayStart}}},
		})
		deliveredAll, _ := h.svc.repo.deliveries.CountDocuments(ctx, bson.D{{Key: "rider_party_id", Value: id}, {Key: "status", Value: "DELIVERED"}})
		row["onTheWay"], row["deliveredToday"], row["deliveredAllTime"] = onTheWay, deliveredToday, deliveredAll
		out = append(out, row)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *handler) crmSubscriptions(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	filter := bson.D{}
	if st := q.Get("status"); st != "" {
		filter = append(filter, bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: strings.Split(st, ",")}}})
	}
	page, limit := crmPage(r)
	total, _ := h.svc.repo.subscriptions.CountDocuments(ctx, filter)
	cur, err := h.svc.repo.subscriptions.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).SetSkip(int64((page-1)*limit)).SetLimit(int64(limit)))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("subscriptions lookup failed")))
		return
	}
	var subs []subscription
	_ = cur.All(ctx, &subs)
	ids := bson.A{}
	for i := range subs {
		ids = append(ids, subs[i].ConsumerID)
	}
	accts := map[primitive.ObjectID]account{}
	if len(ids) > 0 {
		if c2, e := h.svc.repo.accounts.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}); e == nil {
			var rows []account
			if c2.All(ctx, &rows) == nil {
				for _, a := range rows {
					accts[a.ID] = a
				}
			}
		}
	}
	items := []map[string]any{}
	for i := range subs {
		v := crmSubscriptionView(&subs[i])
		if a, ok := accts[subs[i].ConsumerID]; ok {
			v["customerPhone"] = a.Phone
			if a.FullName != nil {
				v["customerName"] = *a.FullName
			}
		}
		items = append(items, v)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "limit": limit})
}

func (h *handler) crmGetSettings(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"pickup": h.svc.pickupPoint(r.Context())})
}

func (h *handler) crmPutSettings(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		Pickup pickupInput `json:"pickup"`
	}
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	p, err := h.svc.setPickupPoint(r.Context(), actor.PartyID, body.Pickup)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"pickup": p})
}

// crmAssign hands an open (not yet picked-up) delivery to a rider, or back to
// the shared pool with rider_party_id "".
func (h *handler) crmAssign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		RiderPartyID string `json:"rider_party_id"`
	}
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	id := chi.URLParam(r, "deliveryId")
	if body.RiderPartyID != "" {
		task, terr := h.svc.repo.findDeliveryByID(ctx, id)
		if terr != nil {
			httpx.Error(w, r, toHTTPErr(terr))
			return
		}
		oid, err := primitive.ObjectIDFromHex(body.RiderPartyID)
		n := int64(0)
		if err == nil {
			// Scope the rider to the store this task belongs to, the way the
			// store-manager path does (ridersForStore). Being an active rider
			// SOMEWHERE is not enough: sending a Lucknow order to a rider rostered
			// at another store creates a task nobody can deliver, and the customer
			// waits for a rider who will never come. The admin surface keeps its
			// extra power — it may still assign an OFFERED task the store console
			// cannot touch — but not the power to pick a stranger.
			n, _ = h.svc.repo.roleAssignments.CountDocuments(ctx, bson.D{
				{Key: "party_id", Value: oid}, {Key: "role_code", Value: "DELIVERY_RIDER"}, {Key: "status", Value: "ACTIVE"},
				{Key: "org_unit_id", Value: task.StoreID},
			})
			if n == 0 {
				if sid, serr := primitive.ObjectIDFromHex(task.StoreID); serr == nil {
					n, _ = h.svc.repo.roleAssignments.CountDocuments(ctx, bson.D{
						{Key: "party_id", Value: oid}, {Key: "role_code", Value: "DELIVERY_RIDER"}, {Key: "status", Value: "ACTIVE"},
						{Key: "org_unit_id", Value: sid},
					})
				}
			}
		}
		if n == 0 {
			httpx.Error(w, r, httpx.BadRequest("NOT_A_STORE_RIDER", "that person is not an active delivery rider at this store"))
			return
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	d, err := h.svc.repo.updateDelivery(ctx, id,
		bson.D{{Key: "rider_party_id", Value: body.RiderPartyID}, {Key: "status", Value: "ASSIGNED"}, {Key: "assigned_at", Value: now}},
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"ASSIGNED", "OFFERED", "ACCEPTED"}}}}})
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errConflict("NOT_ASSIGNABLE", "only an order that has not been picked up can be reassigned")))
		return
	}
	if body.RiderPartyID != "" {
		h.svc.syncOrderAssigned(ctx, d)
	} else {
		h.svc.syncOrderFindingRider(ctx, d)
	}
	httpx.JSON(w, http.StatusOK, d)
}

// crmCancel cancels an order that has not been delivered (nothing was charged:
// money only moves on delivery).
func (h *handler) crmCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Reason string `json:"reason"`
	}
	_ = decode(r, &body)
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "Cancelled by PYAAS"
	}
	orderID := chi.URLParam(r, "orderId")
	o, err := h.svc.repo.findOrderAnyUser(ctx, orderID)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	if o.Status == "delivered" {
		httpx.Error(w, r, toHTTPErr(errConflict("ALREADY_DELIVERED", "a delivered order can no longer be cancelled")))
		return
	}
	if d, _ := h.svc.repo.findDeliveryByOrder(ctx, orderID); d != nil {
		_, _ = h.svc.repo.updateDelivery(ctx, d.ID,
			bson.D{{Key: "status", Value: "FAILED"}, {Key: "failure_reason", Value: reason}},
			bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED", "FAILED"}}}}})
	}
	upd, err := h.svc.repo.updateOrder(ctx, orderID, o.UserID, bson.D{{Key: "status", Value: "cancelled"}},
		bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"delivered", "cancelled"}}}}})
	if err != nil && o.Status != "cancelled" {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	if upd == nil {
		upd = o
	}
	httpx.JSON(w, http.StatusOK, h.svc.crmHydrate(ctx, []order{*upd})[0])
}

// crmDuplicateSubscriptionOrders lists subscription days that have MORE THAN
// ONE live order — the footprint of the pre-fix sweep bug (see
// claimSubscriptionDay). Each group keeps its earliest order first; cancel the
// rest (POST /crm/orders/{id}/cancel) if they have not been delivered.
func (h *handler) crmDuplicateSubscriptionOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Bound the scan by day. Without a lower bound this walks every subscription
	// order ever placed and groups them all in memory — fine on a pilot, a slow
	// unindexed scan once a year of daily milk has accumulated. ?days= widens it
	// when somebody is hunting old duplicates on purpose.
	days := 60
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 730 {
			days = n
		}
	}
	since := istToday(time.Now().AddDate(0, 0, -days))
	cur, err := h.svc.repo.orders.Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "subscription_id", Value: bson.D{{Key: "$gt", Value: ""}}},
			{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
			{Key: "scheduled_for", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "placed_at", Value: 1}}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "sub", Value: "$subscription_id"}, {Key: "day", Value: "$scheduled_for"}}},
			{Key: "orders", Value: bson.D{{Key: "$push", Value: bson.D{
				{Key: "orderId", Value: "$order_id"}, {Key: "status", Value: "$status"},
				{Key: "total", Value: "$total"}, {Key: "placedAt", Value: "$placed_at"},
				{Key: "customerName", Value: "$consumer_name"}, {Key: "phone", Value: "$phone"},
			}}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "count", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "_id.day", Value: -1}}}},
		bson.D{{Key: "$limit", Value: 500}},
	})
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("duplicate scan failed")))
		return
	}
	var groups []struct {
		ID struct {
			Sub string `bson:"sub"`
			Day string `bson:"day"`
		} `bson:"_id"`
		Orders []bson.M `bson:"orders"`
		Count  int      `bson:"count"`
	}
	_ = cur.All(ctx, &groups)
	out := []map[string]any{}
	for _, g := range groups {
		out = append(out, map[string]any{"subscriptionId": g.ID.Sub, "day": g.ID.Day, "count": g.Count, "orders": g.Orders})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// auditAdminReads records every admin-surface request, INCLUDING GETs.
//
// The platform's AuditMutations middleware deliberately skips reads, which is
// right for ordinary routes. It is wrong here: seven of these ten routes are
// GETs, and between them they export the entire customer book — names, unmasked
// phones, addresses, home coordinates, wallet balances and ledgers, plus rider
// phones and live positions. Without this line, a leaked ADMIN_API_KEY could
// pull all of it and leave no trace at all. The shared key has no human behind
// it, so the actor is recorded as the key's service identity and the IP is what
// distinguishes one caller from another.
func auditAdminReads(rec *audit.Recorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if rec == nil || r.Method != http.MethodGet {
				return // mutations are already covered by AuditMutations
			}
			rec.Record(r.Context(), audit.Entry{
				Action:     "admin_crm.read " + r.URL.Path,
				TargetType: "consumer_admin",
				IP:         r.RemoteAddr,
				Meta:       map[string]any{"query": r.URL.RawQuery},
			})
		})
	}
}

// registerAdminCRM mounts /consumer/admin/*.
func registerAdminCRM(cr chi.Router, h *handler, jwtm *auth.JWTManager) {
	cr.Route("/admin", func(ar chi.Router) {
		ar.Use(adminGate(jwtm))
		ar.Use(auditAdminReads(h.svc.deps.Audit))
		ar.Get("/delivery-settings", h.crmGetSettings)
		ar.Put("/delivery-settings", h.crmPutSettings)
		ar.Get("/crm/summary", h.crmSummary)
		ar.Get("/crm/orders", h.crmOrders)
		ar.Get("/crm/orders/{orderId}", h.crmOrderDetail)
		ar.Post("/crm/orders/{orderId}/cancel", h.crmCancel)
		ar.Post("/crm/deliveries/{deliveryId}/assign", h.crmAssign)
		ar.Get("/crm/riders", h.crmRiders)
		ar.Get("/crm/subscriptions", h.crmSubscriptions)
		ar.Get("/crm/duplicate-subscription-orders", h.crmDuplicateSubscriptionOrders)
		ar.Get("/crm/complaints", h.crmComplaints)
		ar.Post("/crm/complaints/{complaintId}", h.crmUpdateComplaint)
	})
}

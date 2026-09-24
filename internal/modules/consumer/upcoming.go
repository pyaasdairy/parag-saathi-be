package consumer

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// Upcoming days (contract C1): GET /consumer/stores/{storeId}/upcoming.
//
// Tomorrow's IST day as the store sees it before any task exists: the
// subscription previews the 13:00 scheduler materialised (not yet locked at
// midnight, so no task) and the scheduled one-off morning orders that still
// have no task. Read-only: it creates nothing and backfills nothing; the
// midnight lock and the order path own task creation. Scoped to the store the
// same way createDeliveryForOrder routes an order: by its nearest store.

const (
	upcomingSourceSubscription = "subscription_preview"
	upcomingSourceScheduled    = "scheduled_order"
)

type upcomingItem struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	Variant   string `json:"variant"`
	Qty       int    `json:"qty"`
}

// upcomingRow is one preview line; wire names are snake_case like the order
// JSON. Every key is always present so a console can rely on the shape.
type upcomingRow struct {
	OrderID        string         `json:"order_id"`
	DeliveryDate   string         `json:"delivery_date"`
	DeliveryWindow string         `json:"delivery_window"`
	Lane           string         `json:"lane"`
	ConsumerName   string         `json:"consumer_name"`
	Phone          string         `json:"phone"`
	AddressLabel   string         `json:"address_label"`
	AddressText    string         `json:"address_text"`
	Society        string         `json:"society"`
	SocietyID      string         `json:"society_id"`
	Tower          string         `json:"tower"`
	Floor          *int           `json:"floor"`
	Unit           string         `json:"unit"`
	Items          []upcomingItem `json:"items"`
	Source         string         `json:"source"`
}

// scheduledOneOffOrders: still-open one-time morning orders whose picked
// delivery_date is day. Subscription days are previews with their own scan
// (listUnlockedSubOrders) and are excluded here.
func (r *repository) scheduledOneOffOrders(ctx context.Context, day string) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "delivery_date", Value: day},
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"placed", "confirmed", "preparing", "assigned"}}}},
		{Key: "subscription_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}, options.Find().SetSort(bson.D{{Key: "placed_at", Value: -1}}).SetLimit(1000))
	if err != nil {
		return nil, errInternal("scheduled orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("scheduled orders decode failed")
	}
	return out, nil
}

// orderRoutesToStore answers whether createDeliveryForOrder would put this
// order's task on storeID: the nearest store to the order's pin, or the
// fallback store when the order carries none (nearestStore never refuses).
// The stores are read once per listing (activeStoreGeos), not per order.
func orderRoutesToStore(stores []storeGeoRow, o *order, storeID string) bool {
	var at *geoPt
	if o.Geo != nil {
		at = &geoPt{Lat: o.Geo.Lat, Lng: o.Geo.Lng}
	}
	id, _, err := rankNearestStore(stores, at)
	if err != nil && !errors.Is(err, errNoStoreGeo) {
		return false
	}
	return id == storeID
}

// orderIDsWithTasks returns which of the given orders already have a
// delivery task, in one query.
func (r *repository) orderIDsWithTasks(ctx context.Context, orderIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(orderIDs) == 0 {
		return out, nil
	}
	cur, err := r.deliveries.Find(ctx, bson.D{{Key: "order_id", Value: bson.D{{Key: "$in", Value: orderIDs}}}},
		options.Find().SetProjection(bson.D{{Key: "order_id", Value: 1}}))
	if err != nil {
		return nil, errInternal("delivery lookup failed")
	}
	var rows []struct {
		OrderID string `bson:"order_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, errInternal("delivery lookup failed")
	}
	for _, d := range rows {
		out[d.OrderID] = true
	}
	return out, nil
}

func (s *service) upcomingRowFor(ctx context.Context, o *order, day, source string) upcomingRow {
	row := upcomingRow{
		OrderID: o.OrderID, DeliveryDate: day, DeliveryWindow: o.DeliveryWindow, Lane: o.Lane,
		ConsumerName: o.ConsumerName, Phone: o.Phone,
		AddressLabel: o.AddressLabel, AddressText: o.AddressText,
		Items: make([]upcomingItem, 0, len(o.Items)), Source: source,
	}
	if row.Lane == "" {
		row.Lane = "morning"
	}
	if cid, cerr := primitive.ObjectIDFromHex(o.UserID); cerr == nil {
		if a := s.addressFor(ctx, cid, o.AddressID, o.AddressLabel); a != nil {
			row.Society, row.SocietyID, row.Tower, row.Floor, row.Unit = a.Society, a.SocietyID, a.Tower, a.Floor, a.Unit
		}
	}
	for _, it := range o.Items {
		row.Items = append(row.Items, upcomingItem{ProductID: it.ProductID, Name: it.Name, Variant: it.Variant, Qty: it.Qty})
	}
	return row
}

// storeUpcoming lists tomorrow's previews for the store the actor manages.
func (s *service) storeUpcoming(ctx context.Context, actor auth.Actor, storeID string) ([]upcomingRow, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	tomorrow := addDaysIST(istToday(time.Now()), 1)
	previews, err := s.repo.listUnlockedSubOrders(ctx, tomorrow)
	if err != nil {
		return nil, err
	}
	scheduled, err := s.repo.scheduledOneOffOrders(ctx, tomorrow)
	if err != nil {
		return nil, err
	}
	rows := []upcomingRow{}
	// The console polls this every 12 s: the stores are read once and the
	// existing tasks with one query, not two queries per preview.
	stores, serr := s.repo.activeStoreGeos(ctx)
	if serr != nil {
		return rows, nil // no serving store: nothing routes here
	}
	ids := make([]string, 0, len(previews)+len(scheduled))
	for _, batch := range [][]order{previews, scheduled} {
		for i := range batch {
			ids = append(ids, batch[i].OrderID)
		}
	}
	tasked, terr := s.repo.orderIDsWithTasks(ctx, ids)
	if terr != nil {
		return nil, terr
	}
	for _, batch := range []struct {
		orders []order
		source string
	}{{previews, upcomingSourceSubscription}, {scheduled, upcomingSourceScheduled}} {
		for i := range batch.orders {
			o := &batch.orders[i]
			if tasked[o.OrderID] {
				continue // already a task: the orders console owns it
			}
			if !orderRoutesToStore(stores, o, storeID) {
				continue
			}
			rows = append(rows, s.upcomingRowFor(ctx, o, tomorrow, batch.source))
		}
	}
	return rows, nil
}

// storeUpcoming: GET /stores/{storeId}/upcoming (STORE_MANAGER).
func (h *handler) storeUpcoming(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	rows, err := h.svc.storeUpcoming(r.Context(), actor, chi.URLParam(r, "storeId"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, rows)
}

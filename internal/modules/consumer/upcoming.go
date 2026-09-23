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
// The days the store cannot see as tasks yet: from tomorrow through the first
// day still open to changes (tomorrow before noon; from noon, tomorrow's
// unfunded leftovers plus the day after), the subscription previews not yet
// locked (no task) and the scheduled one-off morning orders that still have
// no task. Read-only: it creates nothing and backfills nothing; the noon lock
// and the order path own task creation. Scoped to the store the same way
// createDeliveryForOrder routes an order: by its nearest store. Each row
// carries its own delivery_date.

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
func (s *service) orderRoutesToStore(ctx context.Context, o *order, storeID string) bool {
	var at *geoPt
	if o.Geo != nil {
		at = &geoPt{Lat: o.Geo.Lat, Lng: o.Geo.Lng}
	}
	id, _, err := s.repo.nearestStore(ctx, at)
	if err != nil && !errors.Is(err, errNoStoreGeo) {
		return false
	}
	return id == storeID
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

// storeUpcoming lists the upcoming previews for the store the actor manages.
func (s *service) storeUpcoming(ctx context.Context, actor auth.Actor, storeID string) ([]upcomingRow, error) {
	return s.storeUpcomingAt(ctx, actor, storeID, time.Now())
}

// storeUpcomingAt is storeUpcoming with the clock injected: the window runs
// from tomorrow through firstEditableDay(now).
func (s *service) storeUpcomingAt(ctx context.Context, actor auth.Actor, storeID string, now time.Time) ([]upcomingRow, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	tomorrow := addDaysIST(istToday(now), 1)
	last := firstEditableDay(now)
	if last < tomorrow {
		last = tomorrow
	}
	previews, err := s.repo.listUnlockedSubOrders(ctx, bson.D{{Key: "$gte", Value: tomorrow}, {Key: "$lte", Value: last}})
	if err != nil {
		return nil, err
	}
	scheduled := []order{}
	for day := tomorrow; day <= last; day = addDaysIST(day, 1) {
		batch, err := s.repo.scheduledOneOffOrders(ctx, day)
		if err != nil {
			return nil, err
		}
		scheduled = append(scheduled, batch...)
	}
	rows := []upcomingRow{}
	for _, batch := range []struct {
		orders []order
		source string
	}{{previews, upcomingSourceSubscription}, {scheduled, upcomingSourceScheduled}} {
		for i := range batch.orders {
			o := &batch.orders[i]
			if d, _ := s.repo.findDeliveryByOrder(ctx, o.OrderID); d != nil {
				continue // already a task: the orders console owns it
			}
			if !s.orderRoutesToStore(ctx, o, storeID) {
				continue
			}
			day := o.ScheduledFor
			if day == "" {
				day = o.DeliveryDate
			}
			if day == "" {
				day = tomorrow
			}
			rows = append(rows, s.upcomingRowFor(ctx, o, day, batch.source))
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

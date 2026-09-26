package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/push"
)

// OPERATOR PUSH (founder decision 8, 25 Sep): "a store manager's closing
// alert only visible while the app is open is not an alert". The store alerts
// that matter also ring the Saathi phones they concern, over FCM
// (internal/platform/push, deps.OperatorPush):
//
//   - STORE_INSTANT_ORDER, a new instant order broadcast as OFFERED: the
//     store's managers (every ACTIVE STORE_MANAGER assignment on the store)
//     and the riders whose offer pool shows it (duty_gate.go, founder
//     decision 10): the store's on-duty riders, or every ACTIVE
//     DELIVERY_RIDER of the store when none is on duty or the duty lookup
//     fails, as the pool itself falls back. An ORDER alert: rings in quiet
//     hours too. Push only; the order itself is the record.
//   - STORE_INSTANT_CLOSING, 15 minutes before instant closes: the store's
//     managers. A DELIVERY alert (the lane is running and the manager must act
//     before it closes, extended closes run to 02:00): rings in quiet hours.
//   - STORE_INSTANT_CLOSED, at the close: the store's managers. Informational:
//     held in quiet hours (a 22:00 close is at the start of them).
//   - STORE_LOW_STOCK, when a store's low set changes (a repeat of the same
//     set, posted on every Saathi launch, stays silent): the platform admins
//     who get the inbox row. Held in quiet hours.
//
// Every one of them except the new order also keeps its inbox row, so a held
// alert only skips the ring; the Saathi bell shows it on the next open.
// Recipient selection is deliberately simple: managers, plus the riders who
// can take the order now. A rider off duty while a colleague is on duty would
// only be told about an order they can neither see nor claim.
//
// Inert until FCM_SERVICE_ACCOUNT_JSON is set: no recipient query, no
// goroutine, byte-identical behaviour for every flow that raises an alert.

const (
	templateStoreInstantOrder = "STORE_INSTANT_ORDER"
)

// notifyOperators rings the Saathi phones of `recipients` with one alert.
// now is the caller's clock (quiet hours are judged on it). The recipient
// query and the sends run on their own goroutine with their own deadline, so
// the request or tick that raised the alert never waits on FCM; opPushSync
// (tests) runs them inline.
func (s *service) notifyOperators(ctx context.Context, now time.Time, a push.OperatorAlert,
	recipients func(context.Context) ([]primitive.ObjectID, error)) {
	if !s.opPush.Enabled() {
		return
	}
	if !a.Urgent && push.InQuietHours(now) {
		s.log.Info("operator push held for quiet hours (the inbox row stays)", slog.String("kind", a.Kind))
		return
	}
	run := func(ctx context.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("operator push panic", slog.String("kind", a.Kind), slog.Any("panic", rec))
			}
		}()
		to, err := recipients(ctx)
		if err != nil {
			s.log.Warn("operator push: recipients not resolved", slog.String("kind", a.Kind), slog.Any("err", err))
			return
		}
		if len(to) == 0 {
			return
		}
		res, err := s.opPush.Notify(ctx, now, to, a)
		if err != nil {
			s.log.Warn("operator push failed", slog.String("kind", a.Kind), slog.Int("devices", res.Devices), slog.Any("err", err))
			return
		}
		s.log.Info("operator push sent", slog.String("kind", a.Kind), slog.Int("recipients", len(to)),
			slog.Int("devices", res.Devices), slog.Int("sent", res.Sent), slog.Int("pruned", res.Pruned), slog.Int("failed", res.Failed))
	}
	if s.opPushSync {
		run(ctx)
		return
	}
	go func() {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		run(pctx)
	}()
}

// fixedRecipients is a recipients func for parties already resolved.
func fixedRecipients(ids []primitive.ObjectID) func(context.Context) ([]primitive.ObjectID, error) {
	return func(context.Context) ([]primitive.ObjectID, error) { return ids, nil }
}

func recipientIDs(rs []adminRecipient) []primitive.ObjectID {
	out := make([]primitive.ObjectID, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.id)
	}
	return out
}

// storeOperators is a store's ACTIVE STORE_MANAGER holders and its ACTIVE
// DELIVERY_RIDER holders, each list distinct (a party may be in both).
func (r *repository) storeOperators(ctx context.Context, storeID string) (managers, riders []primitive.ObjectID, err error) {
	storeOID, err := primitive.ObjectIDFromHex(storeID)
	if err != nil {
		return nil, nil, nil // not a STORE org unit id: nobody to tell
	}
	cur, err := r.roleAssignments.Find(ctx, bson.D{
		{Key: "org_unit_id", Value: storeOID},
		{Key: "role_code", Value: bson.D{{Key: "$in", Value: bson.A{domain.RoleStoreManager, domain.RoleDeliveryRider}}}},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}, options.Find().SetProjection(bson.D{{Key: "party_id", Value: 1}, {Key: "role_code", Value: 1}}).SetLimit(200))
	if err != nil {
		return nil, nil, httpx.Internal(fmt.Errorf("find store operators: %w", err))
	}
	var rows []struct {
		PartyID  primitive.ObjectID `bson:"party_id"`
		RoleCode string             `bson:"role_code"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, nil, httpx.Internal(fmt.Errorf("decode store operators: %w", err))
	}
	seenM, seenR := map[primitive.ObjectID]bool{}, map[primitive.ObjectID]bool{}
	for _, row := range rows {
		if row.PartyID.IsZero() {
			continue
		}
		switch row.RoleCode {
		case domain.RoleStoreManager:
			if !seenM[row.PartyID] {
				seenM[row.PartyID] = true
				managers = append(managers, row.PartyID)
			}
		case domain.RoleDeliveryRider:
			if !seenR[row.PartyID] {
				seenR[row.PartyID] = true
				riders = append(riders, row.PartyID)
			}
		}
	}
	return managers, riders, nil
}

// newOrderRecipients is who hears a new instant order at `now`: the store's
// managers, and the store's riders whose offer pool shows it (ridersInOfferPool).
// Distinct: a manager who also rides for the store rings once.
func (s *service) newOrderRecipients(ctx context.Context, storeID string, now time.Time) ([]primitive.ObjectID, error) {
	managers, riders, err := s.repo.storeOperators(ctx, storeID)
	if err != nil {
		return nil, err
	}
	seen := map[primitive.ObjectID]bool{}
	out := make([]primitive.ObjectID, 0, len(managers)+len(riders))
	for _, group := range [][]primitive.ObjectID{managers, s.ridersInOfferPool(ctx, storeID, riders, now)} {
		for _, id := range group {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// ridersInOfferPool narrows a store's riders to those who can see and claim
// its unclaimed offers at `now`, by the pool's own rule (duty_gate.go
// offerPoolStores): the riders on duty at this store today (its manager's mark,
// else their attendance); every one of them when none is on duty (the pool's
// fallback, so a missed check-in never leaves an order unheard) or when the
// duty lookup fails (fail open, as the pool does).
func (s *service) ridersInOfferPool(ctx context.Context, storeID string, riders []primitive.ObjectID, now time.Time) []primitive.ObjectID {
	if len(riders) == 0 {
		return riders
	}
	ids := make([]string, 0, len(riders))
	for _, id := range riders {
		ids = append(ids, id.Hex())
	}
	duty, err := s.repo.dutyForRiders(ctx, ids, storeID, istDay(now))
	if err != nil {
		s.log.WarnContext(ctx, "operator push: store duty lookup failed - every store rider hears the new order",
			slog.String("store", storeID), slog.Any("err", err))
		return riders
	}
	on := make([]primitive.ObjectID, 0, len(riders))
	for _, id := range riders {
		if duty[id.Hex()].OnDuty {
			on = append(on, id)
		}
	}
	if len(on) == 0 {
		return riders
	}
	return on
}

// pushNewInstantOrder rings the store's managers and its riders who can take
// it (newOrderRecipients) for a task just broadcast as OFFERED. Collapsed per
// task, so a re-send never stacks.
func (s *service) pushNewInstantOrder(ctx context.Context, now time.Time, d *delivery) {
	items := 0
	for _, it := range d.Items {
		if it.Qty > 0 {
			items += it.Qty
		} else {
			items++
		}
	}
	noun := "items"
	if items == 1 {
		noun = "item"
	}
	body := fmt.Sprintf("Order %s - %d %s - Rs %s", d.OrderCode, items, noun, rupeesText(d.Amount))
	if d.DistanceKm > 0 {
		body += fmt.Sprintf(" - %s km from the store", strconv.FormatFloat(d.DistanceKm, 'f', 1, 64))
	}
	body += ". The first rider to accept takes it."
	storeID := d.StoreID
	s.notifyOperators(ctx, now, push.OperatorAlert{
		Kind:  templateStoreInstantOrder,
		Title: "New instant order",
		Body:  body,
		Data: map[string]string{
			"store_id": storeID, "delivery_id": d.ID, "order_id": d.OrderID, "lane": "instant",
		},
		Urgent:      true,
		CollapseKey: "order-" + d.ID,
	}, func(ctx context.Context) ([]primitive.ObjectID, error) {
		return s.newOrderRecipients(ctx, storeID, now)
	})
}

// pushInstantAlert rings the store's managers for a closing / closed alert
// that has just been written to their inbox. The CLOSED alert replaces the
// CLOSING one on the handset (same collapse key).
func (s *service) pushInstantAlert(ctx context.Context, now time.Time, kind, storeID string, params map[string]any, managers []adminRecipient) {
	str := func(k string) string {
		v, _ := params[k].(string)
		return v
	}
	s.notifyOperators(ctx, now, push.OperatorAlert{
		Kind:  kind,
		Title: str("headline"),
		Body:  str("message"),
		Data: map[string]string{
			"store_id": storeID, "window_close": str("window_close"), "window_reopen": str("window_reopen"),
		},
		Urgent:      kind == templateStoreInstantClosing,
		CollapseKey: "instant-" + storeID,
	}, fixedRecipients(recipientIDs(managers)))
}

// pushLowStock rings the admins whose low-stock alert just re-armed.
func (s *service) pushLowStock(ctx context.Context, now time.Time, storeID string, body lowStockRequest, admins []primitive.ObjectID) {
	if len(admins) == 0 {
		return
	}
	s.notifyOperators(ctx, now, push.OperatorAlert{
		Kind:        templateStoreLowStock,
		Title:       "Low stock at " + body.StoreName,
		Body:        body.Summary,
		Data:        map[string]string{"store_id": storeID, "item_count": strconv.Itoa(body.ItemCount)},
		CollapseKey: "lowstock-" + storeID,
	}, fixedRecipients(admins))
}

// rupeesText prints an amount the way the store reads it: whole rupees bare,
// otherwise two decimals.
func rupeesText(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

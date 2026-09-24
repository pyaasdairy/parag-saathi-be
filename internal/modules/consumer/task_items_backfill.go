package consumer

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// backfillLegacyTaskItems gives task lines written by the pre-union backend
// their product_id and variant. That backend (release/26.07.03) stored task
// items as {name, qty} while its orders already carried both, so on the day
// the union backend is deployed every open task and the 3-day history would
// reach the store console with no pack size, count against the first
// Inventory row of the name, and key both sizes of one product as a single
// line in the item adjust.
//
// One-shot and idempotent, run from boot outside the fatal path. Only the
// tasks a console still shows (open, or finished within deliveryHistoryDays)
// and only when the order's lines line up with the task's one for one, by
// position and exact name; each task line keeps its own qty (the order and
// the task are adjusted together). Returns how many tasks it rewrote.
func (s *service) backfillLegacyTaskItems(ctx context.Context) (int, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -deliveryHistoryDays)
	legacy := bson.E{Key: "items.product_id", Value: bson.D{{Key: "$exists", Value: false}}}
	cur, err := s.repo.deliveries.Find(ctx, bson.D{
		{Key: "items.0", Value: bson.D{{Key: "$exists", Value: true}}},
		legacy,
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: deliveryOpenStatuses}}}},
			bson.D{{Key: "updated_at", Value: bson.D{{Key: "$gte", Value: cutoff}}}},
		}},
	}, options.Find().SetLimit(5000))
	if err != nil {
		return 0, errInternal("legacy task scan failed")
	}
	var tasks []delivery
	if err := cur.All(ctx, &tasks); err != nil {
		return 0, errInternal("legacy task decode failed")
	}
	n := 0
	for i := range tasks {
		d := &tasks[i]
		o, oerr := s.repo.findOrderAnyUser(ctx, d.OrderID)
		if oerr != nil || len(o.Items) != len(d.Items) {
			continue
		}
		items := deliveryItemsFor(o.Items)
		lined := true
		for j := range items {
			if strings.TrimSpace(items[j].Name) != strings.TrimSpace(d.Items[j].Name) {
				lined = false
				break
			}
			items[j].Qty = d.Items[j].Qty
		}
		if !lined {
			continue
		}
		res, uerr := s.repo.deliveries.UpdateOne(ctx,
			bson.D{{Key: "delivery_id", Value: d.ID}, legacy},
			bson.D{{Key: "$set", Value: bson.D{{Key: "items", Value: items}}}})
		if uerr == nil && res.ModifiedCount == 1 {
			n++
		}
	}
	return n, nil
}

package consumer

// R4-02: the deployed backend (release/26.07.03) writes task lines as
// {name, qty} only; its orders already carry product_id and variant. On the
// day the union backend is deployed every open task (a one-off morning order
// scheduled up to 7 days ahead mints its task at once) and the 3-day history
// would reach the store console with no pack size, count against the wrong
// Inventory row, and key both sizes of one product as one line in the item
// adjust. The boot backfill copies product_id and variant from the order.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run TaskItemsBackfill -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func TestTaskItemsBackfillGivesLegacyTasksTheirPackSizes(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	gold := shareGoldName(t, w)
	cid := w.customer(t, "9000005301", 1000)

	legacy := func(lane string) *order {
		t.Helper()
		o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
			Items: []orderItem{
				{ProductID: "gold-500ml", Name: gold, Variant: "500ml", Qty: 2, Price: 35},
				{ProductID: "gold-1l", Name: gold, Variant: "1L", Qty: 3, Price: 69},
			},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
			Lane: lane, ConsumerName: "Legacy", Phone: "9000005301",
		})
		if err != nil {
			t.Fatalf("createOrder: %v", err)
		}
		// What the deployed backend stored on the task.
		if _, err := w.db.Collection(collDeliveries).UpdateOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "items", Value: bson.A{
				bson.D{{Key: "name", Value: gold}, {Key: "qty", Value: 2}},
				bson.D{{Key: "name", Value: gold}, {Key: "qty", Value: 3}},
			}}}}}); err != nil {
			t.Fatalf("stage legacy task: %v", err)
		}
		return o
	}
	open := legacy("morning")
	// A task finished long ago is outside every console: left as it is.
	old := legacy("morning")
	if _, err := w.db.Collection(collDeliveries).UpdateOne(ctx, bson.D{{Key: "order_id", Value: old.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "DELIVERED"}, {Key: "updated_at", Value: time.Now().UTC().AddDate(0, 0, -30)}}}}); err != nil {
		t.Fatalf("age the old task: %v", err)
	}

	if _, err := w.svc.backfillLegacyTaskItems(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	queue, err := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeOrders: %v", err)
	}
	var task *delivery
	for i := range queue {
		if queue[i].OrderID == open.OrderID {
			task = &queue[i]
		}
	}
	if task == nil {
		t.Fatal("open legacy task missing from the store queue")
	}
	want := []deliveryItem{{ProductID: "gold-500ml", Name: gold, Variant: "500ml", Qty: 2}, {ProductID: "gold-1l", Name: gold, Variant: "1L", Qty: 3}}
	if len(task.Items) != 2 || task.Items[0] != want[0] || task.Items[1] != want[1] {
		t.Fatalf("legacy task items after the backfill: %+v, want %+v", task.Items, want)
	}
	// The name stays verbatim (stock reconciliation matches it exactly).
	if oldTask := chainTaskFor(t, w, old.OrderID); oldTask.Items[0].ProductID != "" {
		t.Fatalf("a task outside every console was rewritten: %+v", oldTask.Items)
	}
	// Idempotent: a second boot changes nothing.
	if n, err := w.svc.backfillLegacyTaskItems(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill: %d %v", n, err)
	}
}

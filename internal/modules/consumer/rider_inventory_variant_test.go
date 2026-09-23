package consumer

// F20: the rider's Verify-inventory sheet must keep each pack size on its own
// line and say which size it is. The size travelled only as `unit`, and when
// the order behind a task could not be read the fallback keyed lines by name
// with no size, so "Full Cream Milk" 500ml and 1L merged into one line and
// the rider verified a quantity that matched no crate.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run RiderInventoryVariant -v

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestRiderInventoryVariantKeepsSizesApart(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	rider := w.riderID.Hex()
	now := time.Now().UTC()
	day := istDay(now)
	const milk = "Full Cream Milk - Parag Gold"

	task := func(orderID string, items ...deliveryItem) {
		t.Helper()
		if _, err := w.db.Collection(collDeliveries).InsertOne(ctx, delivery{
			MongoID: primitive.NewObjectID(), ID: "dlv_" + orderID, OrderID: orderID,
			StoreID: w.storeID.Hex(), RiderPartyID: rider, ConsumerID: primitive.NewObjectID().Hex(),
			Status: "ASSIGNED", AssignedAt: rfc3339(now), Items: items,
		}); err != nil {
			t.Fatalf("seed task %s: %v", orderID, err)
		}
	}
	// Two tasks whose orders cannot be read: the fallback path.
	task("ord_gone_1", deliveryItem{Name: milk, Variant: "500ml", Qty: 2})
	task("ord_gone_2", deliveryItem{Name: milk, Variant: "1L", Qty: 1})
	// A task whose order is readable: the order-line path.
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: "ord_live_1", UserID: primitive.NewObjectID().Hex(), Status: "placed",
		Items:     []orderItem{{ID: "it1", ProductID: "gold-1l", Name: milk, Variant: "1L", Qty: 3, Price: 69}},
		CreatedAt: now, UpdatedAt: now, PlacedAt: now,
	}
	if err := w.svc.repo.insertOrder(ctx, o); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	task("ord_live_1", deliveryItem{ProductID: "gold-1l", Name: milk, Variant: "1L", Qty: 3})
	// One catalogue product sold in two sizes under one product id.
	p := &order{
		MongoID: primitive.NewObjectID(), OrderID: "ord_live_2", UserID: primitive.NewObjectID().Hex(), Status: "placed",
		Items: []orderItem{
			{ID: "it2", ProductID: "paneer", Name: "Paneer", Variant: "200g", Qty: 1, Price: 90},
			{ID: "it3", ProductID: "paneer", Name: "Paneer", Variant: "500g", Qty: 2, Price: 210},
		},
		CreatedAt: now, UpdatedAt: now, PlacedAt: now,
	}
	if err := w.svc.repo.insertOrder(ctx, p); err != nil {
		t.Fatalf("seed order 2: %v", err)
	}
	task("ord_live_2", deliveryItem{ProductID: "paneer", Name: "Paneer", Variant: "200g", Qty: 1},
		deliveryItem{ProductID: "paneer", Name: "Paneer", Variant: "500g", Qty: 2})

	lines, err := w.svc.repo.riderInventoryDemandForDay(ctx, rider, day)
	if err != nil {
		t.Fatalf("riderInventoryDemandForDay: %v", err)
	}
	type key struct{ id, variant string }
	got := map[key]int{}
	ids := map[string]bool{}
	for _, l := range lines {
		got[key{l.ProductID, l.Variant}] = l.DemandQty
		if ids[l.ProductID] {
			t.Fatalf("two lines share product_id %q (verify indexes lines by it): %+v", l.ProductID, lines)
		}
		ids[l.ProductID] = true
	}
	if len(lines) != 5 || got[key{"gold-1l", "1L"}] != 3 || got[key{"paneer:200g", "200g"}] != 1 || got[key{"paneer:500g", "500g"}] != 2 {
		t.Fatalf("lines: %+v", lines)
	}
	var half, litre int
	for k, q := range got {
		if strings.HasPrefix(k.id, "name:") {
			switch k.variant {
			case "500ml":
				half = q
			case "1L":
				litre = q
			}
		}
	}
	if half != 2 || litre != 1 {
		t.Fatalf("the unreadable-order fallback merged or lost sizes: %+v", lines)
	}

	// The wire carries the size as its own key; unit stays for the deployed app.
	view := riderInventoryView(&riderInventoryDoc{SessionID: "s1", Lines: lines})
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), `"variant":"1L"`) || !strings.Contains(string(raw), `"variant":"500ml"`) || !strings.Contains(string(raw), `"unit":`) {
		t.Fatalf("inventory wire: %s", raw)
	}
}

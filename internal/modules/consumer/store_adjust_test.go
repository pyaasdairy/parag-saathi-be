package consumer

// R4-08: the deployed Saathi build (release/26.07.03) adjusts items by name
// alone ([{name, qty}]). Gold 500 ml and Gold 1 L share one product name, so
// the bare-name fallback rewrote BOTH lines: "reduce Gold to 1" billed the
// member for one pack of each size. The backend can tell the lines apart now,
// so a bare name that matches two pack sizes is refused and nothing changes;
// an unambiguous name-only edit still works for that build.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run StoreAdjust -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// shareGoldName gives both Gold SKUs the one product name the real
// catalogue gives them ("Full Cream Milk - Parag Gold"; the size is the
// variant), which is what makes a name-only adjust ambiguous.
func shareGoldName(t *testing.T, w *chainWorld) string {
	t.Helper()
	const gold = "Full Cream Milk - Parag Gold"
	if _, err := w.db.Collection(collCatalog).UpdateMany(context.Background(),
		bson.D{{Key: "sku_id", Value: bson.D{{Key: "$in", Value: bson.A{"gold-500ml", "gold-1l"}}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: gold}}}}); err != nil {
		t.Fatalf("catalog names: %v", err)
	}
	return gold
}

func TestStoreAdjustByNameAloneRefusesWhenTwoSizesShareIt(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	gold := shareGoldName(t, w)
	cid := w.customer(t, "9000005201", 1000)
	o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items: []orderItem{
			{ProductID: "gold-500ml", Name: gold, Variant: "500ml", Qty: 2, Price: 35},
			{ProductID: "gold-1l", Name: gold, Variant: "1L", Qty: 3, Price: 69},
			{ProductID: "taaza-500ml", Name: "Milk taaza-500ml", Variant: "500ml", Qty: 2, Price: 29},
		},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Adjust Tester", Phone: "9000005201",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	task := chainTaskFor(t, w, o.OrderID)

	// The deployed build's body: the name only.
	_, err = w.svc.storeAdjustDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, []itemAdjust{{Name: gold, Qty: 1}})
	if code := geofenceCode(err); code != "AMBIGUOUS_LINE" {
		t.Fatalf("a name shared by two pack sizes: err=%v code=%q, want AMBIGUOUS_LINE", err, code)
	}
	got := w.orderByID(t, o.OrderID)
	if got.Items[0].Qty != 2 || got.Items[1].Qty != 3 || got.Total != o.Total {
		t.Fatalf("a refused adjust changed the order: %+v total %v", got.Items, got.Total)
	}

	// A name only one line carries is unambiguous and still adjusts.
	if _, err := w.svc.storeAdjustDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, []itemAdjust{{Name: "Milk taaza-500ml", Qty: 1}}); err != nil {
		t.Fatalf("an unambiguous name-only adjust: %v", err)
	}
	// The current build names the exact line.
	if _, err := w.svc.storeAdjustDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, []itemAdjust{{ProductID: "gold-1l", Variant: "1L", Name: gold, Qty: 1}}); err != nil {
		t.Fatalf("an exact adjust: %v", err)
	}
	got = w.orderByID(t, o.OrderID)
	want := map[string]int{"gold-500ml": 2, "gold-1l": 1, "taaza-500ml": 1}
	for _, it := range got.Items {
		if it.Qty != want[it.ProductID] {
			t.Fatalf("after the two adjusts: %+v", got.Items)
		}
	}
}

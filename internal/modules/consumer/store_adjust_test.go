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

// A store adjustment takes packs off an order the store could not fill. It
// never adds a delivery fee the order did not carry: not to an order that
// was over Rs 199 when the member placed it, not to a Founding Family
// member's order, not to a subscription morning (which never carries one).
// An order that already paid the One Voice Rs 5 keeps paying it.
func TestStoreAdjustNeverRaisesTheDeliveryFee(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	adjust := func(o *order, productID string, qty int) *order {
		t.Helper()
		task := chainTaskFor(t, w, o.OrderID)
		if _, err := w.svc.storeAdjustDelivery(ctx, w.mgr, w.storeID.Hex(), task.ID, []itemAdjust{{ProductID: productID, Qty: qty}}); err != nil {
			t.Fatalf("adjust %s: %v", o.OrderID, err)
		}
		got := w.orderByID(t, o.OrderID)
		if dt := chainTaskFor(t, w, o.OrderID); dt.Amount != got.Total {
			t.Fatalf("the task amount %v disagrees with the order total %v", dt.Amount, got.Total)
		}
		return got
	}
	place := func(cid string, productID string, qty int) *order {
		t.Helper()
		o, err := w.svc.createOrder(ctx, cid, orderInput{
			Items:         []orderItem{{ProductID: productID, Qty: qty, Price: 1}, {ProductID: "taaza-500ml", Qty: 1, Price: 1}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning",
		})
		if err != nil {
			t.Fatalf("createOrder: %v", err)
		}
		return o
	}

	// Over Rs 199 when placed (3 x 69 + 29 = 236, free): the store's shortage
	// takes it to 98 and it stays free.
	nm := w.customer(t, "9000005301", 1000)
	big := place(nm.Hex(), "gold-1l", 3)
	if big.DeliveryFee != 0 {
		t.Fatalf("setup: an order over Rs 199 pays no fee: %+v", big.DeliveryFee)
	}
	if got := adjust(big, "gold-1l", 1); got.DeliveryFee != 0 || got.Total != 98 {
		t.Fatalf("a shortage added a fee: fee %v total %v want 0 and 98", got.DeliveryFee, got.Total)
	}

	// Under Rs 199 when placed (69 + 29 + Rs 5): it keeps its Rs 5.
	small := place(nm.Hex(), "gold-1l", 1)
	if small.DeliveryFee != 5 {
		t.Fatalf("setup: under Rs 199 pays Rs 5: %v", small.DeliveryFee)
	}
	if got := adjust(small, "taaza-500ml", 0); got.DeliveryFee != 5 || got.Total != 74 {
		t.Fatalf("an order under Rs 199: fee %v total %v want 5 and 74", got.DeliveryFee, got.Total)
	}

	// A Founding Family member never pays it, adjusted or not.
	seedTestFarms(t, w, 1)
	member := w.customer(t, "9000005302", 1000)
	if _, err := w.svc.joinFoundingFamily(ctx, member, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	mo := place(member.Hex(), "gold-1l", 1)
	if mo.DeliveryFee != 0 {
		t.Fatalf("setup: a member pays no fee: %v", mo.DeliveryFee)
	}
	if got := adjust(mo, "taaza-500ml", 0); got.DeliveryFee != 0 || got.Total != 69 {
		t.Fatalf("a member's adjusted order: fee %v total %v want 0 and 69", got.DeliveryFee, got.Total)
	}

	// A subscription morning (2 x 29 = 58, no fee) reduced to one pack.
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	subber := w.customer(t, "9000005303", 1000)
	sub := walletLockPlan(t, w, subber, "taaza-500ml", 2, D1, istDayAt(addDaysIST(D, -2), 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	so := assertLocked(t, w, sub, D1)
	if so.DeliveryFee != 0 || so.Total != 58 {
		t.Fatalf("setup: a subscription morning carries no fee: %v %v", so.DeliveryFee, so.Total)
	}
	if got := adjust(so, "taaza-500ml", 1); got.DeliveryFee != 0 || got.Total != 29 {
		t.Fatalf("an adjusted subscription morning: fee %v total %v want 0 and 29", got.DeliveryFee, got.Total)
	}
}

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

package consumer

// R4-04: an order or plan line billed priceFor(product_id, variant) but
// stored the CLIENT's variant (and a plan stored the client's name before
// the catalogue's). {gold-500ml, variant "1L"} was billed 35 a pack while
// the store saw a "1L" pill and packed 1 L; an empty variant stayed empty
// although the catalogue knows the size; a plan posted with name "gold-1l"
// (the app's fallback before its catalogue loads) printed the id on every
// morning order, task and rider row.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run LineVariant -v

import (
	"context"
	"testing"
	"time"
)

// Pure: the size a stored line carries.
func TestLineVariantIsTheSizeThatWasBilled(t *testing.T) {
	ix := buildPriceIndex([]catalogDoc{
		{Kind: catalogKindProduct, SkuID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Price: fp(35)},
		{Kind: catalogKindAddition, SkuID: "ghee", Name: "Ghee", Variant: "500g", Price: fp(480), Variants: []variantDoc{
			{VariantID: "g1kg", Label: "1kg", Price: 900},
		}},
		{Kind: catalogKindAddition, SkuID: "unsized", Name: "Loose", Price: fp(10)},
	})
	for _, c := range []struct{ sku, in, want string }{
		{"gold-500ml", "1L", "500ml"},      // a size the SKU is not: billed 500 ml, labelled 500 ml
		{"gold-500ml", "", "500ml"},        // empty: the catalogue's size
		{"gold-500ml", "500 ML", "500 ML"}, // the same size in the client's spelling is kept
		{"gold-1l", "", "1L"},              // no catalogue size: the bundled seed's
		{"ghee", "1kg", "1kg"},             // a priced variant chose the price: kept
		{"ghee", "2kg", "500g"},            // an unknown label billed the base: the base size
		{"unsized", "tin", "tin"},          // nothing knows the size: the client's label, as before
	} {
		if got := ix.lineVariant(c.sku, c.in); got != c.want {
			t.Errorf("lineVariant(%q, %q) = %q, want %q", c.sku, c.in, got, c.want)
		}
	}
}

func TestLineVariantOnOrdersAndPlans(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000005401", 1000)

	o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items: []orderItem{
			{ProductID: "gold-500ml", Name: "x", Variant: "1L", Qty: 2, Price: 69},
			{ProductID: "gold-1l", Name: "x", Variant: "", Qty: 1, Price: 69},
		},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Variant Tester", Phone: "9000005401",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	if o.Subtotal != 2*35+69 {
		t.Fatalf("subtotal %v: billing must not change", o.Subtotal)
	}
	if o.Items[0].Variant != "500ml" || o.Items[1].Variant != "1L" {
		t.Fatalf("order line sizes: %q %q, want 500ml 1L", o.Items[0].Variant, o.Items[1].Variant)
	}
	task := chainTaskFor(t, w, o.OrderID)
	if task.Items[0].Variant != "500ml" || task.Items[1].Variant != "1L" {
		t.Fatalf("task line sizes: %+v", task.Items)
	}

	sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "gold-1l", Name: "gold-1l", Qty: 1, Frequency: "daily", StartDate: addDaysIST(istToday(time.Now()), 1)})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	if sub.Name != "Milk gold-1l" || sub.Variant != "1L" {
		t.Fatalf("plan line: name %q variant %q, want the catalogue's name and 1L", sub.Name, sub.Variant)
	}
}

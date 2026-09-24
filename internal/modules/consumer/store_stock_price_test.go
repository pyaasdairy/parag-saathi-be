package consumer

// R4-11: GET /stores/{id}/stock carried no price, so the Saathi store console
// (store_console_api.dart reads e['price'] ?? e['mrp']) fell back to its
// hard-coded per-name MRP and "Sold today" priced a 1 L sale billed at 69 as
// 33. Each stock line now carries the unit price an order for it is billed
// at (the server's price index: a store override beats the seeded price).
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run StoreStockPrice -v

import (
	"context"
	"encoding/json"
	"testing"
)

func TestStoreStockPriceIsTheBilledPrice(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	for sku, pr := range map[string]float64{"gold-500ml": 35, "gold-1l": 69} {
		p, n := pr, 40
		if _, err := w.db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
			StoreID: globalCatalogStore, SkuID: sku, Kind: catalogKindProduct, Price: &p, StockCount: &n,
			Name: "Full Cream Milk - Parag Gold", Category: "milk", Variant: map[string]string{"gold-500ml": "500ml", "gold-1l": "1L"}[sku],
		}); err != nil {
			t.Fatalf("seed stock row: %v", err)
		}
	}
	// The ERP price for 1 L lands as a store override and is what orders bill.
	erp := 72.0
	if _, err := w.db.Collection(collCatalog).InsertOne(ctx, catalogDoc{StoreID: w.storeID.Hex(), SkuID: "gold-1l", Kind: catalogKindOverride, Price: &erp}); err != nil {
		t.Fatalf("override: %v", err)
	}
	resp, err := w.svc.storeStock(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeStock: %v", err)
	}
	raw, _ := json.Marshal(resp)
	var wire struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(raw, &wire)
	want := map[string]float64{"gold-500ml": 35, "gold-1l": 72}
	seen := 0
	for _, it := range wire.Items {
		sku, _ := it["sku_id"].(string)
		if w, ok := want[sku]; ok {
			seen++
			if got, _ := it["price"].(float64); got != w {
				t.Errorf("%s: price %v on the wire, want %v (%v)", sku, it["price"], w, it)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("stock lines seen: %d, items %v", seen, wire.Items)
	}
}

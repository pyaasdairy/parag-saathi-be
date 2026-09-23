package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// The Saathi console never edits one variant: for add, edit and remove alike it
// PATCHes {"variants":[...]} as the product's whole new list, spelling the id
// "id". This pins the wire shape and that the list is a full replacement.
func TestPatchSkuVariantsIsAFullReplacement(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	store := w.storeID.Hex()

	created, err := w.svc.addStoreSku(ctx, w.mgr, store, addSkuRequest{Name: "Desi Ghee", Category: "ghee", Price: 500})
	if err != nil {
		t.Fatalf("addStoreSku: %v", err)
	}
	sku := created.ID

	patch := func(body string) (*catalogDoc, error) {
		t.Helper()
		var req patchSkuRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return w.svc.patchStoreSku(ctx, w.mgr, store, sku, req)
	}

	// Two variants, no ids yet: both minted.
	doc, err := patch(`{"variants":[{"label":"500 ml","price":250,"volume_ml":500,"out_of_stock":false},{"label":"1 L","price":480,"out_of_stock":true}]}`)
	if err != nil {
		t.Fatalf("first patch: %v", err)
	}
	if len(doc.Variants) != 2 {
		t.Fatalf("variants after first patch: %d want 2", len(doc.Variants))
	}
	keep := doc.Variants[0]
	if keep.VariantID == "" || doc.Variants[1].VariantID == "" || keep.VariantID == doc.Variants[1].VariantID {
		t.Fatalf("minted ids: %q %q", keep.VariantID, doc.Variants[1].VariantID)
	}
	if !doc.Variants[1].OutOfStock || doc.Variants[0].VolumeMl != 500 {
		t.Fatalf("variant fields lost: %+v", doc.Variants)
	}

	// Remove the second by resending only the first, the console's way ("id").
	doc, err = patch(fmt.Sprintf(`{"variants":[{"id":%q,"label":"500 ml","price":260,"out_of_stock":false}]}`, keep.VariantID))
	if err != nil {
		t.Fatalf("second patch: %v", err)
	}
	if len(doc.Variants) != 1 || doc.Variants[0].VariantID != keep.VariantID || doc.Variants[0].Price != 260 {
		t.Fatalf("variants after removal: %+v want one, id %s, price 260", doc.Variants, keep.VariantID)
	}

	// Read back through the store console listing.
	resp, err := w.svc.storeSkus(ctx, w.mgr, store)
	if err != nil {
		t.Fatalf("storeSkus: %v", err)
	}
	var found *storeAdditionView
	for i := range resp.Additions {
		if resp.Additions[i].ID == sku {
			found = &resp.Additions[i]
		}
	}
	if found == nil {
		t.Fatalf("addition %s missing from the store listing", sku)
	}
	if len(found.Variants) != 1 || found.Variants[0].VariantID != keep.VariantID {
		t.Fatalf("listing variants: %+v want only %s", found.Variants, keep.VariantID)
	}

	// The old single-variant edit keeps working for clients that still send it.
	doc, err = patch(fmt.Sprintf(`{"edit_variant":{"variantId":%q,"price":275}}`, keep.VariantID))
	if err != nil {
		t.Fatalf("edit_variant: %v", err)
	}
	if len(doc.Variants) != 1 || doc.Variants[0].Price != 275 {
		t.Fatalf("edit_variant result: %+v", doc.Variants)
	}

	// Duplicates are refused, by id and by label.
	for _, body := range []string{
		`{"variants":[{"id":"v1","label":"A","price":1},{"id":"v1","label":"B","price":2}]}`,
		`{"variants":[{"label":"Same","price":1},{"label":"same ","price":2}]}`,
	} {
		_, err := patch(body)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != "DUPLICATE_VARIANT" {
			t.Fatalf("%s: err %v want DUPLICATE_VARIANT", body, err)
		}
	}
	// A variant without a price is still invalid on the list path.
	if _, err := patch(`{"variants":[{"label":"No price"}]}`); err == nil {
		t.Fatalf("a priceless variant was accepted")
	}

	// Variants sent beside base fields (the edit-product form) land together.
	doc, err = patch(`{"name":"Desi Ghee Jar","price":520,"variants":[{"label":"200 g","price":110}]}`)
	if err != nil {
		t.Fatalf("combined patch: %v", err)
	}
	if doc.Name != "Desi Ghee Jar" || len(doc.Variants) != 1 || doc.Variants[0].Label != "200 g" {
		t.Fatalf("combined patch result: name %q variants %+v", doc.Name, doc.Variants)
	}

	// An empty array clears every variant; an absent key leaves them alone.
	doc, err = patch(`{"variants":[]}`)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(doc.Variants) != 0 {
		t.Fatalf("variants after clear: %+v want none", doc.Variants)
	}
	if _, err := patch(`{"variants":[{"label":"Back","price":9}]}`); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	doc, err = patch(`{"price":530}`)
	if err != nil {
		t.Fatalf("price-only: %v", err)
	}
	if len(doc.Variants) != 1 {
		t.Fatalf("a price-only patch touched the variants: %+v", doc.Variants)
	}
}

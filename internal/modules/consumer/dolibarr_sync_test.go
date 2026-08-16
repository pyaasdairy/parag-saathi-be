package consumer

import (
	"encoding/json"
	"testing"

	"github.com/pyaas/saathi-backend/internal/platform/dolibarr"
)

func dolProduct(t *testing.T, js string) dolibarr.Product {
	t.Helper()
	var p dolibarr.Product
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// The price rule, checked against REAL live ERP rows (captured 2026-08-16):
// this instance stores prices ex-GST (HT) with tva_tx unset and the rate only
// in default_vat_code — the consumer price is price × (1+rate).
func TestDolibarrEffectivePrice(t *testing.T) {
	cases := []struct {
		name string
		js   string
		want float64
	}{
		// milk, 0% GST → passes through
		{"milk 0%", `{"price_ttc":"35.00000000","tva_tx":"0.000","default_vat_code":"C+S-00"}`, 35},
		{"toned 30", `{"price_ttc":"30.00000000","tva_tx":"0.000","default_vat_code":"C+S-00"}`, 30},
		// 5% GST entered ex-tax → ×1.05, snaps to the exact seeded rupee price
		{"peda 250g → ₹125", `{"price_ttc":"119.04762000","tva_tx":"0.000","default_vat_code":"C+S-5"}`, 125},
		{"butter 100g → ₹60", `{"price_ttc":"57.14286000","tva_tx":"0.000","default_vat_code":"C+S-5"}`, 60},
		{"rasgolla 200g → ₹64", `{"price_ttc":"60.95238000","tva_tx":"0.000","default_vat_code":"C+S-5"}`, 64},
		{"gulab jamun 500g → ₹200", `{"price_ttc":"190.47619000","tva_tx":"0.000","default_vat_code":"C+S-5"}`, 200},
		{"shrikhand → ₹25", `{"price_ttc":"23.80952000","tva_tx":"0.000","default_vat_code":"C+S-5"}`, 25},
		// explicit tva_tx wins when present
		{"tva set", `{"price_ttc":"100","tva_tx":"12.000","default_vat_code":"C+S-5"}`, 112},
		// bad data → 0 (sync skips)
		{"zero", `{"price_ttc":"0","tva_tx":"0"}`, 0},
	}
	for _, c := range cases {
		if got := dolibarrEffectivePrice(dolProduct(t, c.js)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// The curated baseline map may only point at SKUs that actually exist in
// products_seed.json — a typo here would silently create phantom overrides.
func TestDolibarrBaselineMapTargetsExistInSeed(t *testing.T) {
	var seed []seedProduct
	if err := json.Unmarshal(embeddedProductsSeed, &seed); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, p := range seed {
		ids[p.ID] = true
	}
	for ref, sku := range dolibarrRefToSeedSku {
		if !ids[sku] {
			t.Errorf("map %s → %q: sku not in products_seed.json", ref, sku)
		}
	}
	if len(dolibarrRefToSeedSku) < 30 {
		t.Errorf("baseline map suspiciously small: %d", len(dolibarrRefToSeedSku))
	}
}

// Only the PRG-* master range may sync; legacy/test rows must never reach the
// catalog (they would duplicate seeded products).
func TestDolibarrRefGate(t *testing.T) {
	for ref, ok := range map[string]bool{
		"PRG-TONED-500ML":     true,
		"PRG-GHEE-SIKA-1LTR":  true,
		"Parag_Gold":          false, // legacy test row
		"Full_Cream_Milk_FCM": false, // legacy test row
		"prg-toned-500ml":     false, // gate runs on the UPPERCASED ref → "PRG-TONED-500ML"
		"":                    false,
	} {
		if got := dolibarrRefPattern.MatchString(ref); got != ok {
			t.Errorf("ref %q: gate=%v want %v", ref, got, ok)
		}
	}
}

func TestDolibarrCategory(t *testing.T) {
	for _, c := range []struct{ ref, label, want string }{
		{"PRG-TONED-500ML", "Parag Taza Toned Milk 500 ML", "milk"},
		{"PRG-TEA-1LTR", "Parag Tea Special Milk 1 LTR", "super_tea"},
		{"PRG-FLAVMILK-200ML", "Parag Flavoured Milk 200 ML", "flavoured_milk"},
		{"PRG-GHEE-SIKA-500ML", "Parag Ghee (Sika) 500 ML", "ghee"},
		{"PRG-DAHI-S-80GM", "Parag Dahi (Sada) 80 GM", "dahi"},
		{"PRG-KALAKAND-1KG", "Parag Kalakand 1 KG", "sweets"},
		{"PRG-CHHACH-430ML", "Parag Chhach 430 ML", "chaach"},
		{"PRG-NEWTHING-1KG", "Parag Fancy Milk Drink", "milk"}, // unknown segment, milky label
		{"PRG-NEWTHING-1KG", "Parag Fancy Barfi", "sweets"},    // unknown segment, sweet default
	} {
		if got := dolibarrCategory(c.ref, c.label); got != c.want {
			t.Errorf("%s (%s): got %q want %q", c.ref, c.label, got, c.want)
		}
	}
}

// Every category value the sync can emit must be one the seeded catalog already
// uses — the FE groups by these strings.
func TestDolibarrCategoriesMatchSeedVocabulary(t *testing.T) {
	var seed []seedProduct
	if err := json.Unmarshal(embeddedProductsSeed, &seed); err != nil {
		t.Fatal(err)
	}
	vocab := map[string]bool{}
	for _, p := range seed {
		vocab[p.Category] = true
	}
	for seg, cat := range dolibarrCategoryBySegment {
		if !vocab[cat] {
			t.Errorf("segment %s emits category %q not present in the seed vocabulary", seg, cat)
		}
	}
}

// ERP labels embed the pack size; the split is what lets the app group sizes of
// one product onto one card (the "same product 3-4 times" bug).
func TestDolibarrSplitLabel(t *testing.T) {
	for _, c := range []struct{ label, name, variant string }{
		{"Parag Dahi (Sada / Plain) 80 GM", "Parag Dahi (Sada / Plain)", "80 GM"},
		{"Parag Taza Toned Milk 130 ML", "Parag Taza Toned Milk", "130 ML"},
		{"Parag Gold Full Cream Milk (FCM) 1 LTR", "Parag Gold Full Cream Milk (FCM)", "1 LTR"},
		{"Parag Ghee (Tin) 15 LTR", "Parag Ghee (Tin)", "15 LTR"},
		{"Parag Table Butter 20 GM", "Parag Table Butter", "20 GM"},
		{"Parag Kheer (Chhena) 100 GM", "Parag Kheer (Chhena)", "100 GM"},
		{"Parag Gold 1 L", "Parag Gold", "1 L"},
		{"Parag Paneer 1.5 KG", "Parag Paneer", "1.5 KG"},
		{"Something With No Size", "Something With No Size", ""},
	} {
		n, v := dolibarrSplitLabel(c.label)
		if n != c.name || v != c.variant {
			t.Errorf("split(%q) = (%q,%q) want (%q,%q)", c.label, n, v, c.name, c.variant)
		}
	}
}

func TestDolibarrEffectiveMin(t *testing.T) {
	p := dolProduct(t, `{"price_ttc":"119.04762000","price_min":"119.04762000","tva_tx":"0.000","default_vat_code":"C+S-5"}`)
	if got := dolibarrEffectiveMin(p); got != 125 {
		t.Fatalf("mrp: got %v want 125", got)
	}
	none := dolProduct(t, `{"price_ttc":"30","price_min":"0.00000000"}`)
	if got := dolibarrEffectiveMin(none); got != 0 {
		t.Fatalf("no declared mrp must be 0, got %v", got)
	}
}

func TestDolibarrAdditionSkuAndImagePath(t *testing.T) {
	if got := dolibarrAdditionSku("PRG-KALAKAND-1KG"); got != "dol-prg-kalakand-1kg" {
		t.Fatalf("sku: %q", got)
	}
	// spaces/parens in real WhatsApp filenames must survive URL-escaping
	got := dolibarrImagePath("PRG-TONED-500ML", "WhatsApp Image 2026-08-16 at 9.52.07 PM (1).jpeg")
	want := "catalog/dolimg/PRG-TONED-500ML/WhatsApp%20Image%202026-08-16%20at%209.52.07%20PM%20%281%29.jpeg"
	if got != want {
		t.Fatalf("image path:\n got %q\nwant %q", got, want)
	}
}

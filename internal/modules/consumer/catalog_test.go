package consumer

import (
	"encoding/json"
	"testing"
)

func f64(v float64) *float64 { return &v }
func bptr(v bool) *bool      { return &v }

// TestValidatePrice pins the 1..100000 money guard the overlay enforces.
func TestValidatePrice(t *testing.T) {
	ok := []float64{1, 29, 410, 100000}
	for _, p := range ok {
		if err := validatePrice(p); err != nil {
			t.Errorf("validatePrice(%v) = %v, want nil", p, err)
		}
	}
	bad := []float64{0, 0.99, -5, 100000.01, 250000}
	for _, p := range bad {
		if err := validatePrice(p); err == nil {
			t.Errorf("validatePrice(%v) = nil, want error", p)
		}
	}
}

// TestValidateCategory allows only the consumer app's Category union.
func TestValidateCategory(t *testing.T) {
	for _, c := range []string{"milk", "dahi", "paneer", "super_tea", "sweets", "ghee"} {
		if err := validateCategory(c); err != nil {
			t.Errorf("validateCategory(%q) = %v, want nil", c, err)
		}
	}
	for _, c := range []string{"", "MILK", "electronics", "vegetables"} {
		if err := validateCategory(c); err == nil {
			t.Errorf("validateCategory(%q) = nil, want error", c)
		}
	}
}

// TestBaselineIntegrity guards the hardcoded milk baseline: the 8 core SKUs are
// present, each carries a valid category + in-range price, and the id set the
// ADD guard reads matches the slice.
func TestBaselineIntegrity(t *testing.T) {
	if len(consumerBaseline) != 8 {
		t.Fatalf("baseline size = %d, want 8", len(consumerBaseline))
	}
	want := map[string]struct{}{
		"taaza-500ml": {}, "taaza-1l": {}, "gold-500ml": {}, "gold-1l": {},
		"shakti-500ml": {}, "chai-special-500ml": {}, "dahi-sweet-200g": {}, "paneer-1kg": {},
	}
	for _, b := range consumerBaseline {
		if _, ok := want[b.ID]; !ok {
			t.Errorf("unexpected baseline id %q", b.ID)
		}
		if err := validateCategory(b.Category); err != nil {
			t.Errorf("baseline %q has invalid category %q", b.ID, b.Category)
		}
		if err := validatePrice(b.Price); err != nil {
			t.Errorf("baseline %q has out-of-range price %v", b.ID, b.Price)
		}
		if _, ok := baselineIDs[b.ID]; !ok {
			t.Errorf("baselineIDs missing %q", b.ID)
		}
	}
	if len(baselineIDs) != len(consumerBaseline) {
		t.Errorf("baselineIDs size = %d, want %d", len(baselineIDs), len(consumerBaseline))
	}
}

// TestValidateVariant pins the variant contract: a label and an in-range price
// are both required; variantId is optional (minted later).
func TestValidateVariant(t *testing.T) {
	ok := variantInput{Label: "500ml Pouch", Price: f64(29)}
	if err := validateVariant(ok); err != nil {
		t.Errorf("validateVariant(ok) = %v, want nil", err)
	}
	if err := validateVariant(variantInput{Price: f64(29)}); err == nil {
		t.Error("validateVariant(no label) = nil, want error")
	}
	if err := validateVariant(variantInput{Label: "x"}); err == nil {
		t.Error("validateVariant(no price) = nil, want error")
	}
	if err := validateVariant(variantInput{Label: "x", Price: f64(0)}); err == nil {
		t.Error("validateVariant(price 0) = nil, want error")
	}
	if err := validateVariant(variantInput{Label: "x", Price: f64(250000)}); err == nil {
		t.Error("validateVariant(price too high) = nil, want error")
	}
}

// TestValidatePhysical rejects only negative descriptors (0 = unset, allowed).
func TestValidatePhysical(t *testing.T) {
	if err := validatePhysical(nil); err != nil {
		t.Errorf("validatePhysical(nil) = %v, want nil", err)
	}
	if err := validatePhysical(&physicalInput{VolumeMl: 500, WeightG: 515, Dimensions: "8x8x18cm"}); err != nil {
		t.Errorf("validatePhysical(sane) = %v, want nil", err)
	}
	if err := validatePhysical(&physicalInput{WeightG: -1}); err == nil {
		t.Error("validatePhysical(negative weight) = nil, want error")
	}
	if err := validatePhysical(&physicalInput{VolumeMl: -1}); err == nil {
		t.Error("validatePhysical(negative volume) = nil, want error")
	}
}

// TestVariantInputSnakeCamelTolerance pins the km/unit tolerance style on the
// variant body: each key decodes from EITHER snake_case or camelCase, and
// normalize() folds the aliases onto the canonical field.
func TestVariantInputSnakeCamelTolerance(t *testing.T) {
	snake := []byte(`{"variant_id":"v1","label":"1L","price":57,"image_url":"s3://a","out_of_stock":true,"volume_ml":1000,"unit":"ml"}`)
	camel := []byte(`{"variantId":"v1","label":"1L","price":57,"imageUrl":"s3://a","outOfStock":true,"volumeMl":1000,"unit":"ml"}`)
	for name, raw := range map[string][]byte{"snake": snake, "camel": camel} {
		var v variantInput
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		v.normalize()
		if v.VariantID != "v1" {
			t.Errorf("%s: VariantID = %q, want v1", name, v.VariantID)
		}
		if v.ImageURL != "s3://a" {
			t.Errorf("%s: ImageURL = %q, want s3://a", name, v.ImageURL)
		}
		if v.OutOfStock == nil || !*v.OutOfStock {
			t.Errorf("%s: OutOfStock = %v, want true", name, v.OutOfStock)
		}
		if v.VolumeMl != 1000 {
			t.Errorf("%s: VolumeMl = %v, want 1000", name, v.VolumeMl)
		}
	}
}

// TestPhysicalInputSnakeCamelTolerance pins the same tolerance on physical{}.
func TestPhysicalInputSnakeCamelTolerance(t *testing.T) {
	for name, raw := range map[string][]byte{
		"snake": []byte(`{"volume_ml":500,"weight_g":515,"dimensions":"8x8x18cm"}`),
		"camel": []byte(`{"volumeMl":500,"weightG":515,"dimensions":"8x8x18cm"}`),
	} {
		var p physicalInput
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		p.normalize()
		if p.VolumeMl != 500 || p.WeightG != 515 {
			t.Errorf("%s: got volume=%v weight=%v, want 500/515", name, p.VolumeMl, p.WeightG)
		}
	}
}

// TestAddSkuRequestNormalizeNested pins that request normalize() cascades into
// nested variants[] and physical{}, and folds base_id/photoUrl aliases.
func TestAddSkuRequestNormalizeNested(t *testing.T) {
	raw := []byte(`{
		"baseId":"gold",
		"photoUrl":"s3://p",
		"name":"Full Cream",
		"category":"milk",
		"variants":[{"variantId":"g1","label":"1L","price":69,"volumeMl":1000}],
		"physical":{"weightG":1030}
	}`)
	var req addSkuRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	req.normalize()
	if req.BaseID != "gold" {
		t.Errorf("BaseID = %q, want gold", req.BaseID)
	}
	if req.PhotoURL != "s3://p" {
		t.Errorf("PhotoURL = %q, want s3://p", req.PhotoURL)
	}
	if len(req.Variants) != 1 || req.Variants[0].VariantID != "g1" || req.Variants[0].VolumeMl != 1000 {
		t.Fatalf("nested variant not normalized: %+v", req.Variants)
	}
	if req.Physical == nil || req.Physical.WeightG != 1030 {
		t.Fatalf("nested physical not normalized: %+v", req.Physical)
	}
}

// TestVariantToDoc round-trips a validated variantInput into its stored doc,
// minting a variantId when absent and carrying every physical attribute through.
func TestVariantToDoc(t *testing.T) {
	got := variantToDoc(variantInput{
		Label: " 1L Pouch ", Price: f64(57), VolumeMl: 1000, Unit: "ml",
		ImageURL: "s3://a", OutOfStock: bptr(true),
		Attributes: map[string]string{"fat": "6%"},
	})
	if got.VariantID == "" {
		t.Error("VariantID not minted")
	}
	if got.Label != "1L Pouch" {
		t.Errorf("Label = %q, want trimmed \"1L Pouch\"", got.Label)
	}
	if got.Price != 57 || got.VolumeMl != 1000 || got.Unit != "ml" || !got.OutOfStock {
		t.Errorf("doc lost fields: %+v", got)
	}
	if got.Attributes["fat"] != "6%" {
		t.Errorf("attributes lost: %+v", got.Attributes)
	}

	// A supplied id is preserved (idempotent variant identity for edits/deletes).
	if variantToDoc(variantInput{VariantID: "keepme", Label: "x", Price: f64(1)}).VariantID != "keepme" {
		t.Error("supplied VariantID not preserved")
	}
}

// TestAdditionViewRoundTrip pins the read-view serialization: a base addition doc
// projects baseId + variants[] + physical{} onto the consumer AND store views,
// and the JSON carries the contract's camelCase variant keys.
func TestAdditionViewRoundTrip(t *testing.T) {
	doc := catalogDoc{
		SkuID:    "gold",
		Kind:     catalogKindAddition,
		BaseID:   "dolibarr-42",
		Name:     "Full Cream Milk - PYAAS Gold",
		Category: "milk",
		Price:    f64(69),
		InStock:  bptr(true),
		Variants: []variantDoc{
			{VariantID: "g1", Label: "1L Pouch", Price: 69, VolumeMl: 1000, Unit: "ml", OutOfStock: false},
			{VariantID: "g2", Label: "500ml Pouch", Price: 35, VolumeMl: 500, Unit: "ml", OutOfStock: true},
		},
		Physical: &physicalDoc{VolumeMl: 1000, WeightG: 1030, Dimensions: "9x9x20cm"},
	}

	cv := additionViewFromDoc(doc)
	if cv.BaseID != "dolibarr-42" || len(cv.Variants) != 2 || cv.Physical == nil {
		t.Fatalf("consumer view dropped base/variants/physical: %+v", cv)
	}
	if !cv.InStock {
		t.Error("consumer view InStock = false, want true")
	}

	sv := storeAdditionViewFromDoc(doc)
	if sv.BaseID != "dolibarr-42" || len(sv.Variants) != 2 || sv.Physical == nil {
		t.Fatalf("store view dropped base/variants/physical: %+v", sv)
	}

	// The wire form must use the contract's camelCase variant keys.
	b, err := json.Marshal(cv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		BaseID   string `json:"baseId"`
		Variants []struct {
			VariantID  string  `json:"variantId"`
			OutOfStock bool    `json:"outOfStock"`
			VolumeMl   float64 `json:"volumeMl"`
		} `json:"variants"`
		Physical struct {
			WeightG float64 `json:"weightG"`
		} `json:"physical"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if wire.BaseID != "dolibarr-42" {
		t.Errorf("wire baseId = %q, want dolibarr-42", wire.BaseID)
	}
	if len(wire.Variants) != 2 || wire.Variants[0].VariantID != "g1" || wire.Variants[0].VolumeMl != 1000 {
		t.Fatalf("wire variants malformed: %+v", wire.Variants)
	}
	if !wire.Variants[1].OutOfStock {
		t.Error("wire variant[1].outOfStock = false, want true")
	}
	if wire.Physical.WeightG != 1030 {
		t.Errorf("wire physical.weightG = %v, want 1030", wire.Physical.WeightG)
	}
}

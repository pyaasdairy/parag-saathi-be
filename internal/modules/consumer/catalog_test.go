package consumer

import "testing"

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

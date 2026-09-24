package consumer

import (
	"testing"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
)

func fp(v float64) *float64 { return &v }
func bp(v bool) *bool       { return &v }

// The ₹0-milk hole: prices must come from the server catalog, never the client.
// This pins the resolver — seed price, override-beats-seed (how ERP prices land),
// additions, per-variant prices, hidden rejection, unknown rejection.
func TestPriceIndexResolvesServerSide(t *testing.T) {
	docs := []catalogDoc{
		{Kind: catalogKindProduct, SkuID: "taaza-500ml", Name: "Toned Milk - Parag Taaza", Variant: "500ml", Price: fp(29)},
		{Kind: catalogKindOverride, SkuID: "taaza-500ml", Price: fp(30)}, // Dolibarr price wins
		{Kind: catalogKindProduct, SkuID: "gold-500ml", Name: "Full Cream", Price: fp(35)},
		{Kind: catalogKindAddition, SkuID: "dol-prg-butter-20gm", Name: "Parag Table Butter", Variant: "20 GM", Price: fp(12)},
		{Kind: catalogKindAddition, SkuID: "combo", Name: "Combo", Price: fp(100), Variants: []variantDoc{
			{VariantID: "v1", Label: "1L Pouch", Price: 71},
		}},
		{Kind: catalogKindAddition, SkuID: "gone", Name: "Old", Price: fp(50), Hidden: bp(true)},
	}
	ix := buildPriceIndex(docs)

	cases := []struct {
		sku, variant string
		want         float64
		ok           bool
	}{
		{"taaza-500ml", "", 30, true},         // override (ERP) beats seed
		{"gold-500ml", "", 35, true},          // plain seed
		{"dol-prg-butter-20gm", "", 12, true}, // addition
		{"combo", "1L Pouch", 71, true},       // variant by label
		{"combo", "v1", 71, true},             // variant by id
		{"combo", "unknown size", 100, true},  // unpriced variant label → base
		{"gone", "", 0, false},                // hidden → not sellable
		{"no-such-product", "", 0, false},     // unknown → rejected
		{"", "", 0, false},                    // empty id → rejected
	}
	for _, c := range cases {
		got, ok := ix.priceFor(c.sku, c.variant)
		if got != c.want || ok != c.ok {
			t.Errorf("priceFor(%q,%q) = (%v,%v) want (%v,%v)", c.sku, c.variant, got, ok, c.want, c.ok)
		}
	}
	if n := ix.nameFor("taaza-500ml"); n != "Toned Milk - Parag Taaza" { // exact catalog name — the store-inventory reconciliation key
		t.Errorf("nameFor: %q", n)
	}
}

// FOUNDER DECISION: the Play-reviewer account (9999900000 / 123456) is ALWAYS
// ON — a permanent store-review sign-in, independent of any env flag or of
// OTP_DEV_MODE. This pins that contract so a future "harden it behind a flag"
// change can't silently break the reviewer's ability to log in.
func TestReviewLoginAlwaysOn(t *testing.T) {
	svc := &service{deps: &deps.Deps{Cfg: &config.Config{}}}
	for _, dev := range []bool{false, true} {
		svc.deps.Cfg.OTPDevMode = dev
		t.Setenv("REVIEW_LOGIN_ENABLED", "")
		if !svc.reviewLoginEnabled() {
			t.Fatalf("review login must be ON regardless of env (OTPDevMode=%v)", dev)
		}
	}
}

// Parag is never discounted (owner invariant): a member_price stored on a
// non-PYAAS row, on the product, an override or a variant, is ignored for
// members; only PYAAS milk lines carry a member price.
func TestMemberPriceNeverTouchesNonPyaasLines(t *testing.T) {
	ix := buildPriceIndex([]catalogDoc{
		{Kind: catalogKindProduct, SkuID: "gold-1l", Name: "Parag Gold", Category: "milk", Price: fp(71), MemberPrice: fp(60)},
		{Kind: catalogKindProduct, SkuID: "taaza-1l", Name: "Parag Taaza", Category: "milk", Price: fp(55)},
		{Kind: catalogKindOverride, SkuID: "taaza-1l", Price: fp(57), MemberPrice: fp(50)},
		{Kind: catalogKindAddition, SkuID: "dol-prg-dahi", BaseID: "PRG-DAHI", Name: "Parag Dahi", Category: "dairy", Price: fp(40), Variants: []variantDoc{
			{VariantID: "d1", Label: "400 g Cup", Price: 40, MemberPrice: fp(30)},
		}},
		{Kind: catalogKindProduct, SkuID: "pyaas-toned-1l", Name: "Toned Milk - PYAAS", Category: "milk", Unit: "1 L", Price: fp(85), MemberPrice: fp(80)},
	})
	for _, c := range []struct {
		sku, variant string
		want         float64
	}{
		{"gold-1l", "", 71},
		{"taaza-1l", "", 57},
		{"dol-prg-dahi", "400 g Cup", 40},
		{"pyaas-toned-1l", "", 80}, // a PYAAS line keeps its explicit level 3
	} {
		if got, ok := ix.priceForMember(c.sku, c.variant, true); !ok || got != c.want {
			t.Errorf("member price of %s %q = %v (%v), want %v", c.sku, c.variant, got, ok, c.want)
		}
	}
}

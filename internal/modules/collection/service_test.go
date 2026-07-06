package collection

import (
	"testing"
	"time"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// TestPricePourRounding verifies paise rounding of both the per-litre rate
// and the pour amount (blueprint §8.1 — receipts must match to the paisa).
func TestPricePourRounding(t *testing.T) {
	cases := []struct {
		name       string
		chart      domain.RateChart
		fat, snf   float64
		qty        float64
		wantRate   float64
		wantAmount float64
	}{
		{
			name:  "rate rounds up to next paisa",
			chart: domain.RateChart{BaseRatePerLitre: 10, FatRatePerPoint: 1, SNFRatePerPoint: 0},
			fat:   4.556, snf: 0, qty: 1,
			wantRate: 14.56, wantAmount: 14.56,
		},
		{
			name:  "rate rounds down to previous paisa",
			chart: domain.RateChart{BaseRatePerLitre: 10, FatRatePerPoint: 1, SNFRatePerPoint: 0},
			fat:   4.554, snf: 0, qty: 1,
			wantRate: 14.55, wantAmount: 14.55,
		},
		{
			name:  "amount rounds after multiplying exact rate by fractional quantity",
			chart: domain.RateChart{BaseRatePerLitre: 20, FatRatePerPoint: 5, SNFRatePerPoint: 3},
			fat:   6.5, snf: 8.5, qty: 10.333,
			wantRate: 78.00, wantAmount: 805.97,
		},
		{
			name:  "blueprint receipt example: 10.5L at 42.30 = 444.15",
			chart: domain.RateChart{BaseRatePerLitre: 0, FatRatePerPoint: 10, SNFRatePerPoint: 1},
			fat:   3.5, snf: 7.3, qty: 10.5,
			wantRate: 42.30, wantAmount: 444.15,
		},
		{
			name:  "amount uses the ROUNDED rate, not the raw rate",
			chart: domain.RateChart{BaseRatePerLitre: 10, FatRatePerPoint: 1, SNFRatePerPoint: 0},
			fat:   4.556, snf: 0, qty: 100,
			wantRate: 14.56, wantAmount: 1456.00, // 14.556*100 would be 1455.60
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, amount := tc.chart.PricePour(tc.fat, tc.snf, tc.qty)
			if rate != tc.wantRate {
				t.Errorf("rate = %v, want %v", rate, tc.wantRate)
			}
			if amount != tc.wantAmount {
				t.Errorf("amount = %v, want %v", amount, tc.wantAmount)
			}
		})
	}
}

// TestCheckPlausibilityMatrix exercises the physical-bounds matrix used to
// flag readings and hard-reject pours (§8.2).
func TestCheckPlausibilityMatrix(t *testing.T) {
	cases := []struct {
		name      string
		fat, snf  float64
		qty       float64
		wantFlags int
	}{
		{"all plausible", 4.5, 8.5, 10, 0},
		{"boundary values are plausible", domain.PlausibleFatMin, domain.PlausibleSNFMax, domain.PlausibleQtyMax, 0},
		{"fat below floor", 1.9, 8.5, 10, 1},
		{"fat above ceiling", 12.1, 8.5, 10, 1},
		{"snf below floor", 4.5, 6.9, 10, 1},
		{"snf above ceiling", 4.5, 11.6, 10, 1},
		{"quantity below floor", 4.5, 8.5, 0.05, 1},
		{"quantity above ceiling", 4.5, 8.5, 200.5, 1},
		{"zero quantity is allowed (reading without weighment)", 4.5, 8.5, 0, 0},
		{"implausible composition and quantity both flag", 1.0, 12.0, 500, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := domain.CheckPlausibility(tc.fat, tc.snf, tc.qty)
			if len(flags) != tc.wantFlags {
				t.Errorf("flags = %v (%d), want %d", flags, len(flags), tc.wantFlags)
			}
			for _, f := range flags {
				if f != domain.IntegrityFlagImplausibleValue {
					t.Errorf("unexpected flag %q", f)
				}
			}
		})
	}
}

// TestDeriveIntegrityFlags exercises the anti-tamper flag derivation matrix
// (§8.2: manual entry, low OCR confidence, missing geotag, clock skew).
func TestDeriveIntegrityFlags(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		mode      string
		ocrConf   float64
		hasGeo    bool
		deviceTS  time.Time
		wantFlags []string
	}{
		{
			name: "clean direct reading has no flags",
			mode: domain.ReadingModeDirect, ocrConf: 0, hasGeo: true,
			deviceTS:  now.Add(-1 * time.Minute),
			wantFlags: nil,
		},
		{
			name: "manual entry always flagged",
			mode: domain.ReadingModeManual, hasGeo: true,
			deviceTS:  now,
			wantFlags: []string{domain.IntegrityFlagManualEntry},
		},
		{
			name: "photo OCR below confidence threshold flagged",
			mode: domain.ReadingModePhotoOCR, ocrConf: 0.74, hasGeo: true,
			deviceTS:  now,
			wantFlags: []string{domain.IntegrityFlagLowOCRConfidence},
		},
		{
			name: "photo OCR at threshold not flagged",
			mode: domain.ReadingModePhotoOCR, ocrConf: 0.75, hasGeo: true,
			deviceTS:  now,
			wantFlags: nil,
		},
		{
			name: "missing geotag flagged",
			mode: domain.ReadingModeDirect, hasGeo: false,
			deviceTS:  now,
			wantFlags: []string{domain.IntegrityFlagMissingGeotag},
		},
		{
			name: "device clock behind by more than 10 minutes flagged",
			mode: domain.ReadingModeDirect, hasGeo: true,
			deviceTS:  now.Add(-11 * time.Minute),
			wantFlags: []string{domain.IntegrityFlagClockSkew},
		},
		{
			name: "device clock ahead by more than 10 minutes flagged",
			mode: domain.ReadingModeDirect, hasGeo: true,
			deviceTS:  now.Add(11 * time.Minute),
			wantFlags: []string{domain.IntegrityFlagClockSkew},
		},
		{
			name: "skew of exactly 10 minutes not flagged",
			mode: domain.ReadingModeDirect, hasGeo: true,
			deviceTS:  now.Add(-10 * time.Minute),
			wantFlags: nil,
		},
		{
			name: "absent device timestamp skips the skew check",
			mode: domain.ReadingModeDirect, hasGeo: true,
			deviceTS:  time.Time{},
			wantFlags: nil,
		},
		{
			name: "manual entry without geotag stacks both flags",
			mode: domain.ReadingModeManual, hasGeo: false,
			deviceTS:  now,
			wantFlags: []string{domain.IntegrityFlagManualEntry, domain.IntegrityFlagMissingGeotag},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveIntegrityFlags(tc.mode, tc.ocrConf, tc.hasGeo, tc.deviceTS, now)
			if len(got) != len(tc.wantFlags) {
				t.Fatalf("flags = %v, want %v", got, tc.wantFlags)
			}
			for i := range got {
				if got[i] != tc.wantFlags[i] {
					t.Errorf("flags[%d] = %q, want %q", i, got[i], tc.wantFlags[i])
				}
			}
		})
	}
}

package consumer

import (
	"testing"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/modules/publictrace"
)

// TestMapToMilkBatch verifies the operator public-scan → consumer MilkBatch
// projection (the new bridge logic), independent of any DB.
func TestMapToMilkBatch(t *testing.T) {
	resp := &publictrace.QRScanResponse{
		BatchNumber: "PARAG-LKO-20260701-TM-014",
		Product: publictrace.ProductInfo{
			Name: "Toned Milk (Blue)", SKU: "TM-500", UnitSize: "500 ml pouch",
			MfgDate: "2026-07-01", ExpiryDate: "2026-07-03",
		},
		Plant: publictrace.PlantInfo{Name: "Parag Dairy Plant, Lucknow", District: "Lucknow"},
		Quality: &publictrace.QualityInfo{Tests: []domain.QCTest{
			{Name: "FAT", Value: 3.0, Unit: "%", Pass: true},
			{Name: "SNF", Value: 8.5, Unit: "%", Pass: true},
			{Name: "Added water", Value: 0, Unit: "", Pass: true},
		}},
		Sourcing: publictrace.SourcingInfo{
			Samitis:      []publictrace.SamitiInfo{{Name: "Kakori DCS", Code: "K1"}, {Name: "Mall DCS", Code: "M2"}},
			FarmersTotal: 4180,
		},
		Ledger:   publictrace.LedgerInfo{Intact: true},
		Recalled: false,
	}

	mb := mapToMilkBatch("parag://trace/whatever", resp)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"batch_code", mb.BatchCode, "PARAG-LKO-20260701-TM-014"},
		{"product", mb.Product, "Toned Milk (Blue)"},
		{"pack_size", mb.PackSize, "500 ml pouch"},
		{"best_before", mb.BestBefore, "2026-07-03"},
		{"union_name", mb.UnionName, "Lucknow District Cooperative Dairy Union"},
		{"plant", mb.Plant, "Parag Dairy Plant, Lucknow"},
		{"district", mb.District, "Lucknow"},
		{"fat_pct", mb.FatPct, 3.0},
		{"snf_pct", mb.SnfPct, 8.5},
		{"member_villages", mb.MemberVillages, 2},
		{"pouring_members", mb.PouringMembers, 4180},
		{"verified", mb.Verified, true},
		{"tests_len", len(mb.Tests), 3},
		{"fat_test_value", mb.Tests[0].Value, "3%"},
		{"adulteration_value_empty", mb.Tests[2].Value, ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

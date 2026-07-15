package consumer

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// TestAppKeyOKFailClosed pins the consumer-app-only gate: an unset server key
// must deny EVERYONE (fail-closed) — a missing CONSUMER_APP_KEY on a
// deployment must never silently make traceability public — and a configured
// key must admit only an exactly-matching X-Parag-App-Key header.
func TestAppKeyOKFailClosed(t *testing.T) {
	mkReq := func(key string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/consumer/traceability/X", nil)
		if key != "" {
			r.Header.Set("X-Parag-App-Key", key)
		}
		return r
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name      string
		serverKey string
		sentKey   string
		want      bool
	}{
		{"unset server key denies bare request", "", "", false},
		{"unset server key denies even a keyed request", "", "some-key", false},
		{"configured key + match admits", "app-key-1", "app-key-1", true},
		{"configured key + mismatch denies", "app-key-1", "wrong", false},
		{"configured key + missing header denies", "app-key-1", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &service{appKey: tc.serverKey, log: log}
			if got := s.appKeyOK(mkReq(tc.sentKey)); got != tc.want {
				t.Fatalf("appKeyOK(server=%q, sent=%q) = %v, want %v", tc.serverKey, tc.sentKey, got, tc.want)
			}
		})
	}
}

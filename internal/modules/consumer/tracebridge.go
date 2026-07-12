package consumer

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/modules/publictrace"
)

// Traceability bridge — the ONE sanctioned Saathi↔consumer touchpoint. It lets
// the consumer app resolve a scanned pack QR to its honest cooperative
// provenance by REUSING the existing public trace resolver (publictrace.ScanQR),
// then maps the operator's QRScanResponse onto the consumer app's MilkBatch
// shape (lib/milk.ts) so the shipped lookupBatch() seam composes unchanged. A
// code that doesn't resolve returns 404 and the app falls back to its offline
// demo seed.
//
// The bridge reads operator provenance and touches NO operator business flow,
// PII, or money. The one incidental write is the public scan-count telemetry
// increment inside ScanQR — a consumer scan legitimately IS a public scan, and
// this is the exact behaviour the operator's own /public/qr endpoint already
// performs (best-effort, async, a bare counter). No new mutation is introduced.

// qualityTestView mirrors the FE QualityTest ({name, value?, pass}).
type qualityTestView struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	Pass  bool   `json:"pass"`
}

// milkBatchView mirrors the FE MilkBatch (lib/milk.ts) EXACTLY.
type milkBatchView struct {
	BatchCode      string            `json:"batch_code"`
	Product        string            `json:"product"`
	PackSize       string            `json:"pack_size"`
	BatchDate      string            `json:"batch_date"`
	PackedAt       string            `json:"packed_at"`
	BestBefore     string            `json:"best_before"`
	UnionName      string            `json:"union_name"`
	Plant          string            `json:"plant"`
	District       string            `json:"district"`
	State          string            `json:"state"`
	FatPct         float64           `json:"fat_pct"`
	SnfPct         float64           `json:"snf_pct"`
	MemberVillages int               `json:"member_villages"`
	PouringMembers int               `json:"pouring_members"`
	Tests          []qualityTestView `json:"tests"`
	Verified       bool              `json:"verified"`
	Recalled       bool              `json:"recalled,omitempty"`
	RecallNotice   string            `json:"recall_notice,omitempty"`
}

// mapToMilkBatch projects the operator's public QR-scan provenance onto the
// consumer MilkBatch. Pure function (no I/O) so it is unit-testable.
func mapToMilkBatch(code string, r *publictrace.QRScanResponse) milkBatchView {
	batchCode := r.BatchNumber
	if batchCode == "" {
		batchCode = strings.ToUpper(strings.TrimSpace(code))
	}
	union := "PARAG member dairy union"
	if r.Plant.District != "" {
		union = r.Plant.District + " District Cooperative Dairy Union"
	}
	mb := milkBatchView{
		BatchCode:      batchCode,
		Product:        r.Product.Name,
		PackSize:       r.Product.UnitSize,
		BatchDate:      r.Product.MfgDate,
		PackedAt:       r.Product.MfgDate,
		BestBefore:     r.Product.ExpiryDate,
		UnionName:      union,
		Plant:          r.Plant.Name,
		District:       r.Plant.District,
		State:          "Uttar Pradesh",
		MemberVillages: len(r.Sourcing.Samitis),
		PouringMembers: r.Sourcing.FarmersTotal,
		Tests:          []qualityTestView{},
		Verified:       r.Ledger.Intact,
		Recalled:       r.Recalled,
		RecallNotice:   r.RecallNotice,
	}
	if r.Quality != nil {
		for _, t := range r.Quality.Tests {
			mb.Tests = append(mb.Tests, qualityTestView{
				Name:  t.Name,
				Value: formatTestValue(t.Value, t.Unit),
				Pass:  t.Pass,
			})
			switch strings.ToUpper(strings.TrimSpace(t.Name)) {
			case "FAT":
				mb.FatPct = t.Value
			case "SNF":
				mb.SnfPct = t.Value
			}
		}
	}
	return mb
}

// formatTestValue renders a measured test value with its unit (e.g. "3.5%"),
// or "" for a boolean adulteration screen that carries no numeric reading.
func formatTestValue(v float64, unit string) string {
	if v == 0 && unit == "" {
		return ""
	}
	return fmt.Sprintf("%g%s", v, unit)
}

// traceByCode resolves a scanned code to the consumer provenance view, reusing
// the operator's public QR resolver read-only.
func (s *service) traceByCode(ctx context.Context, code string) (*milkBatchView, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errBadRequest("a batch/QR code is required")
	}
	resp, err := s.trace.ScanQR(ctx, code)
	if err != nil || resp == nil {
		// Not resolvable (unknown QR, or not a pack QR) — 404 so the app falls
		// back to its offline seed rather than showing another batch.
		return nil, errNotFound("no traceability found for this code")
	}
	mb := mapToMilkBatch(code, resp)
	return &mb, nil
}

// ── Handler ─────────────────────────────────────────────────────────────────

func (h *handler) traceByCode(w http.ResponseWriter, r *http.Request) {
	mb, err := h.svc.traceByCode(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mb)
}

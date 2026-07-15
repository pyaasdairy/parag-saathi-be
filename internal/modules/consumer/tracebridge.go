package consumer

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/pyaas/saathi-backend/internal/modules/publictrace"
)

// consumerAppScheme is the deep-link a printed pack QR encodes, so ONLY the
// consumer app (registered for parag://) can open it — a generic scanner can't.
const consumerAppScheme = "parag://trace/"

// appKeyOK enforces the consumer-app-only gate: the request must carry an
// X-Parag-App-Key matching CONSUMER_APP_KEY (constant-time). FAIL-CLOSED: an
// unset server key denies everyone rather than opening the gate — traceability
// and the PDF label are for the PARAG app only, and a missing env var on a
// deployment must not silently make them public. Local dev sets
// CONSUMER_APP_KEY in .env (see .env.example) to the same value the app ships
// as EXPO_PUBLIC_CONSUMER_APP_KEY.
func (s *service) appKeyOK(r *http.Request) bool {
	if s.appKey == "" {
		s.log.Warn("consumer traceability denied: CONSUMER_APP_KEY is not configured on this deployment (fail-closed)")
		return false
	}
	got := r.Header.Get("X-Parag-App-Key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.appKey)) == 1
}

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
// the operator's public QR resolver read-only. It mirrors the operator's public
// scan (ResolvePublicQR): resolves BOTH consignment batch QRs (the QC "send QR"
// tokens, lowercase hex) AND product-lot pack QRs (PRG-...). Case-insensitive,
// because the app uppercases scanned codes while consignment tokens are stored
// lowercase.
func (s *service) traceByCode(ctx context.Context, code string) (*milkBatchView, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errBadRequest("a batch/QR code is required")
	}
	for _, c := range []string{code, strings.ToLower(code), strings.ToUpper(code)} {
		res, err := s.trace.ResolvePublicQR(ctx, c)
		if err != nil || res == nil {
			continue // try the next case variant; unknown/integrity-failed → fall through
		}
		switch v := res.(type) {
		case *publictrace.QRScanResponse:
			mb := mapToMilkBatch(code, v)
			return &mb, nil
		case *publictrace.BatchQualityReport:
			mb := mapReportToMilkBatch(code, v)
			return &mb, nil
		}
	}
	// Not resolvable — 404 so the app falls back to its offline seed.
	return nil, errNotFound("no traceability found for this code")
}

// mapReportToMilkBatch projects a consignment batch quality report (a per-samiti
// pooled batch) onto the consumer MilkBatch — honest provenance: the contributing
// society, the tests it passed, the plant.
func mapReportToMilkBatch(code string, r *publictrace.BatchQualityReport) milkBatchView {
	batchCode := r.BatchCode
	if batchCode == "" {
		batchCode = strings.ToUpper(strings.TrimSpace(code))
	}
	district := r.Samiti.District
	union := "PARAG member dairy union"
	if district != "" {
		union = district + " District Cooperative Dairy Union"
	}
	plant := ""
	if r.Plant != nil {
		plant = r.Plant.Name
	}
	mb := milkBatchView{
		BatchCode:      batchCode,
		Product:        "Cooperative pooled milk batch",
		UnionName:      union,
		Plant:          plant,
		District:       district,
		State:          "Uttar Pradesh",
		MemberVillages: 1, // a batch pools from one contributing society
		PouringMembers: r.Farmers.Total,
		Tests:          []qualityTestView{},
		Verified:       r.Quality != nil && r.Quality.OverallPass,
	}
	if r.Collection.PickedUpAt != nil {
		mb.BatchDate = r.Collection.PickedUpAt.Format("2006-01-02")
		mb.PackedAt = r.Collection.PickedUpAt.Format("2006-01-02")
	}
	if r.Quality != nil {
		for _, t := range r.Quality.Tests {
			val := ""
			if t.Value != nil {
				val = fmt.Sprintf("%g%s", *t.Value, t.Unit)
			}
			mb.Tests = append(mb.Tests, qualityTestView{Name: t.Parameter, Value: val, Pass: t.Pass})
			switch strings.ToUpper(t.Parameter) {
			case "FAT":
				if t.Value != nil {
					mb.FatPct = *t.Value
				}
			case "SNF":
				if t.Value != nil {
					mb.SnfPct = *t.Value
				}
			}
		}
	}
	return mb
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *handler) traceByCode(w http.ResponseWriter, r *http.Request) {
	if !h.svc.appKeyOK(r) {
		writeErr(w, errForbidden("traceability is available from the PARAG app only"))
		return
	}
	mb, err := h.svc.traceByCode(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mb)
}

// traceLabel returns a self-contained HTML provenance label (all values + the
// pack QR image) that the app turns into a downloadable PDF (expo-print). Same
// consumer-app-only gate as the JSON resolve.
func (h *handler) traceLabel(w http.ResponseWriter, r *http.Request) {
	if !h.svc.appKeyOK(r) {
		writeErr(w, errForbidden("traceability is available from the PARAG app only"))
		return
	}
	code := chi.URLParam(r, "code")
	mb, err := h.svc.traceByCode(r.Context(), code)
	if err != nil {
		writeErr(w, err)
		return
	}
	htmlDoc, err := renderLabelHTML(code, mb)
	if err != nil {
		writeErr(w, errInternal("label render failed"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(htmlDoc))
}

// qrDataURI renders `content` as a QR PNG and returns a data: URI for inline
// embedding in the label HTML (self-contained — no external image request).
func qrDataURI(content string) (string, error) {
	png, err := qrcode.Encode(content, qrcode.Medium, 320)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// renderLabelHTML builds the provenance label. The embedded QR encodes the
// consumer-app deep link (parag://trace/<code>), so scanning the printed label
// re-opens the passport in the PARAG app only.
func renderLabelHTML(code string, mb *milkBatchView) (string, error) {
	deepLink := consumerAppScheme + strings.ToUpper(strings.TrimSpace(code))
	qr, err := qrDataURI(deepLink)
	if err != nil {
		return "", err
	}
	esc := html.EscapeString
	var rows strings.Builder
	row := func(k, v string) {
		if v == "" {
			return
		}
		rows.WriteString("<tr><td class=\"k\">" + esc(k) + "</td><td class=\"v\">" + esc(v) + "</td></tr>")
	}
	row("Batch code", mb.BatchCode)
	row("Product", mb.Product)
	row("Pack size", mb.PackSize)
	row("Packed on", mb.PackedAt)
	row("Best before", mb.BestBefore)
	row("Member union", mb.UnionName)
	row("Processing plant", mb.Plant)
	row("District", mb.District)
	row("State", mb.State)
	if mb.FatPct > 0 {
		row("Fat", fmt.Sprintf("%g%%", mb.FatPct))
	}
	if mb.SnfPct > 0 {
		row("SNF", fmt.Sprintf("%g%%", mb.SnfPct))
	}
	row("Contributing societies", fmt.Sprintf("%d", mb.MemberVillages))
	if mb.PouringMembers > 0 {
		row("Pouring farmer-members", fmt.Sprintf("%d", mb.PouringMembers))
	}

	var tests strings.Builder
	for _, t := range mb.Tests {
		mark := "✓"
		cls := "pass"
		if !t.Pass {
			mark, cls = "✗", "fail"
		}
		val := ""
		if t.Value != "" {
			val = " <span class=\"tv\">" + esc(t.Value) + "</span>"
		}
		tests.WriteString("<li class=\"" + cls + "\">" + mark + " " + esc(t.Name) + val + "</li>")
	}

	recall := ""
	if mb.Recalled {
		recall = "<div class=\"recall\">⚠ RECALL: " + esc(mb.RecallNotice) + "</div>"
	}
	verified := "Verified against the federation's QA records"
	if !mb.Verified {
		verified = "Provenance recorded"
	}

	doc := `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>PARAG Milk Passport — ` + esc(mb.BatchCode) + `</title>
<style>
:root{--flame:#E8491D;--ink:#1a1a1a;--muted:#666;}
*{box-sizing:border-box}body{font-family:-apple-system,system-ui,Segoe UI,Roboto,sans-serif;color:var(--ink);margin:0;padding:24px;background:#FFF6EC}
.card{max-width:520px;margin:0 auto;background:#fff;border-radius:16px;padding:24px;border:1px solid #f0e2d0}
.head{display:flex;justify-content:space-between;align-items:flex-start;gap:16px}
.brand{font-weight:800;font-size:20px;color:var(--flame);letter-spacing:.5px}
.sub{color:var(--muted);font-size:12px;margin-top:2px}
.qr{width:132px;height:132px;border:1px solid #eee;border-radius:8px}
h1{font-size:15px;margin:18px 0 6px}
table{width:100%;border-collapse:collapse;font-size:13px}
td{padding:6px 0;border-bottom:1px solid #f4f4f4;vertical-align:top}
.k{color:var(--muted);width:44%}.v{font-weight:600}
ul{list-style:none;padding:0;margin:8px 0 0;font-size:13px}
li{padding:4px 0}.pass{color:#177245}.fail{color:#b00020}.tv{color:var(--muted);font-weight:600}
.foot{margin-top:16px;font-size:11px;color:var(--muted);text-align:center}
.recall{background:#b00020;color:#fff;padding:8px 12px;border-radius:8px;font-weight:700;margin:12px 0}
.badge{display:inline-block;margin-top:8px;font-size:11px;color:#177245}
</style></head>
<body><div class="card">
<div class="head">
  <div><div class="brand">PARAG</div><div class="sub">Milk Provenance Passport</div>
  <div class="badge">✓ ` + esc(verified) + `</div></div>
  <img class="qr" src="` + qr + `" alt="Scan with the PARAG app"/>
</div>` + recall + `
<h1>Pack</h1><table>` + rows.String() + `</table>
<h1>Quality &amp; safety tests</h1><ul>` + tests.String() + `</ul>
<div class="foot">Scan the QR with the PARAG app to verify this pack's cooperative provenance.<br/>Pooled milk traces to the set of contributing societies, never a single farmer.</div>
</div></body></html>`
	return doc, nil
}

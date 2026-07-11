package publictrace

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// Handler is the thin HTTP layer: extract params, call the service, respond.
type Handler struct {
	service *Service
}

// ScanQR handles GET /public/qr/{qr_code} — the consumer scan. The code is
// dispatched on lookup order: a per-samiti batch QR (batch_code or token,
// F8 quality report) first, else the existing product-lot resolution.
func (h *Handler) ScanQR(w http.ResponseWriter, r *http.Request) {
	qrCode := chi.URLParam(r, "qr_code")
	if qrCode == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_QR_CODE", "qr_code path parameter is required"))
		return
	}
	resp, err := h.service.ResolvePublicQR(r.Context(), qrCode)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// VerifyLedger handles GET /public/ledger/verify?from=&to= — the public
// tamper-evidence check.
func (h *Handler) VerifyLedger(w http.ResponseWriter, r *http.Request) {
	resp, err := h.service.VerifyLedger(r.Context(), r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// TraceGraph handles GET /trace/{entity_type}/{entity_id} — the official
// upstream+downstream event-graph walk. entity_id is an ObjectID hex; it is
// validated here and passed to the ledger as its canonical hex form.
func (h *Handler) TraceGraph(w http.ResponseWriter, r *http.Request) {
	entityID, err := httpx.PathID(r, "entity_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.service.TraceGraph(r.Context(),
		chi.URLParam(r, "entity_type"), entityID.Hex())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// Timeline handles GET /trace/{entity_type}/{entity_id}/timeline — one
// entity's own events in chain order. entity_id is an ObjectID hex.
func (h *Handler) Timeline(w http.ResponseWriter, r *http.Request) {
	entityID, err := httpx.PathID(r, "entity_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page := httpx.ParsePage(r)
	events, total, err := h.service.Timeline(r.Context(),
		chi.URLParam(r, "entity_type"), entityID.Hex(), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, events, map[string]any{
		"limit":  page.Limit,
		"offset": page.Offset,
		"total":  total,
	})
}

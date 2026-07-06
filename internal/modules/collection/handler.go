package collection

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the collection module: decode, call service,
// respond. No business logic, no MongoDB.
type handler struct {
	svc *service
}

// requestActor returns the authenticated actor (guaranteed present by the
// Authenticate middleware on every route of this module).
func requestActor(r *http.Request) auth.Actor {
	actor, _ := auth.ActorFrom(r.Context())
	return actor
}

// createRateChart handles POST /collection/rate-charts.
func (h *handler) createRateChart(w http.ResponseWriter, r *http.Request) {
	var req CreateRateChartRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	chart, err := h.svc.CreateRateChart(r.Context(), requestActor(r), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, chart)
}

// getActiveRateChart handles GET /collection/rate-charts/active?dcs_id=.
func (h *handler) getActiveRateChart(w http.ResponseWriter, r *http.Request) {
	dcsID := r.URL.Query().Get("dcs_id")
	if dcsID == "" {
		httpx.Error(w, r, httpx.BadRequest("VALIDATION", "dcs_id query parameter is required"))
		return
	}
	chart, err := h.svc.ResolveActiveChart(r.Context(), requestActor(r), dcsID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, chart)
}

// createReading handles POST /collection/readings.
func (h *handler) createReading(w http.ResponseWriter, r *http.Request) {
	var req CreateReadingRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	reading, err := h.svc.CreateReading(r.Context(), requestActor(r), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, reading)
}

// listReadings handles GET /collection/readings?dcs_id=&date=.
func (h *handler) listReadings(w http.ResponseWriter, r *http.Request) {
	page := httpx.ParsePage(r)
	readings, total, err := h.svc.ListReadings(r.Context(), requestActor(r),
		r.URL.Query().Get("dcs_id"), r.URL.Query().Get("date"), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, readings, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// createPour handles POST /collection/pours.
func (h *handler) createPour(w http.ResponseWriter, r *http.Request) {
	var req CreatePourRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	pour, replay, err := h.svc.CreatePour(r.Context(), requestActor(r), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if replay {
		httpx.JSON(w, http.StatusOK, PourResponse{Pour: *pour, IdempotentReplay: true})
		return
	}
	httpx.JSON(w, http.StatusCreated, PourResponse{Pour: *pour})
}

// batchSyncPours handles POST /collection/pours/batch-sync.
func (h *handler) batchSyncPours(w http.ResponseWriter, r *http.Request) {
	var req BatchSyncRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	results, err := h.svc.BatchSyncPours(r.Context(), requestActor(r), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, results)
}

// supersedePour handles POST /collection/pours/{id}/supersede.
func (h *handler) supersedePour(w http.ResponseWriter, r *http.Request) {
	var req SupersedePourRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	pour, err := h.svc.SupersedePour(r.Context(), requestActor(r), chi.URLParam(r, "id"), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, PourResponse{Pour: *pour})
}

// listPours handles GET /collection/pours?dcs_id=&date=&shift=&farmer_party_id=.
func (h *handler) listPours(w http.ResponseWriter, r *http.Request) {
	page := httpx.ParsePage(r)
	q := r.URL.Query()
	pours, total, err := h.svc.ListPours(r.Context(), requestActor(r), pourListFilter{
		DCSID:         q.Get("dcs_id"),
		Date:          q.Get("date"),
		Shift:         q.Get("shift"),
		FarmerPartyID: q.Get("farmer_party_id"),
	}, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, pours, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// generateInvoices handles POST /collection/invoices/generate.
func (h *handler) generateInvoices(w http.ResponseWriter, r *http.Request) {
	var req GenerateInvoicesRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.GenerateInvoices(r.Context(), requestActor(r), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// listInvoices handles GET /collection/invoices?dcs_id=&date=&farmer_party_id=&status=.
func (h *handler) listInvoices(w http.ResponseWriter, r *http.Request) {
	page := httpx.ParsePage(r)
	q := r.URL.Query()
	invoices, total, err := h.svc.ListInvoices(r.Context(), requestActor(r), invoiceListFilter{
		DCSID:         q.Get("dcs_id"),
		Date:          q.Get("date"),
		FarmerPartyID: q.Get("farmer_party_id"),
		Status:        q.Get("status"),
	}, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, invoices, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// getInvoice handles GET /collection/invoices/{id}.
func (h *handler) getInvoice(w http.ResponseWriter, r *http.Request) {
	invoice, err := h.svc.GetInvoice(r.Context(), requestActor(r), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, invoice)
}

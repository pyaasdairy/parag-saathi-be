package settlement

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the settlement module: decode, validate shape,
// call the service, respond. No business logic, no MongoDB.
type handler struct {
	svc *service
}

// initiate handles POST /settlements.
func (h *handler) initiate(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var in InitiateSettlementRequest
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	batch, err := h.svc.Initiate(r.Context(), actor, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, batch)
}

// approve handles POST /settlements/{id}/approve.
func (h *handler) approve(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	batch, err := h.svc.Approve(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, batch)
}

// reject handles POST /settlements/{id}/reject.
func (h *handler) reject(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var in RejectSettlementRequest
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	batch, err := h.svc.Reject(r.Context(), actor, chi.URLParam(r, "id"), in.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, batch)
}

// execute handles POST /settlements/{id}/execute.
func (h *handler) execute(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	detail, err := h.svc.Execute(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, detail)
}

// list handles GET /settlements.
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	q := r.URL.Query()
	page := httpx.ParsePage(r)
	batches, total, err := h.svc.List(r.Context(), actor, q.Get("dcs_id"), q.Get("date"), q.Get("status"), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, batches, map[string]int64{
		"limit": page.Limit, "offset": page.Offset, "total": total,
	})
}

// detail handles GET /settlements/{id}.
func (h *handler) detail(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	d, err := h.svc.Detail(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// payouts handles GET /settlements/payouts.
func (h *handler) payouts(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	page := httpx.ParsePage(r)
	items, total, err := h.svc.PayoutHistory(r.Context(), actor, r.URL.Query().Get("farmer_party_id"), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, map[string]int64{
		"limit": page.Limit, "offset": page.Offset, "total": total,
	})
}

// createDBT handles POST /dbt/requests.
func (h *handler) createDBT(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var in CreateDBTRequestInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	req, err := h.svc.CreateDBT(r.Context(), actor, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, req)
}

// updateDBTStatus handles POST /dbt/requests/{id}/status.
func (h *handler) updateDBTStatus(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var in UpdateDBTStatusInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	req, err := h.svc.UpdateDBTStatus(r.Context(), actor, chi.URLParam(r, "id"), in.Status)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, req)
}

// listDBT handles GET /dbt/requests.
func (h *handler) listDBT(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	q := r.URL.Query()
	page := httpx.ParsePage(r)
	items, total, err := h.svc.ListDBT(r.Context(), actor, q.Get("farmer_party_id"), q.Get("status"), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, map[string]int64{
		"limit": page.Limit, "offset": page.Offset, "total": total,
	})
}

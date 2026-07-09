package dashboards

import (
	"net/http"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

type handler struct {
	svc *service
}

func newHandler(svc *service) *handler {
	return &handler{svc: svc}
}

// farmerSummary handles GET /dashboards/farmer/{partyID}.
func (h *handler) farmerSummary(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	id, err := httpx.PathID(r, "partyID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.farmerSummary(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// societyStats handles GET /dashboards/society/{dcsID}.
func (h *handler) societyStats(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	id, err := httpx.PathID(r, "dcsID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.societyStats(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

package logistics

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the logistics module: decode, delegate to the
// service, respond. No business logic, no MongoDB.
type handler struct {
	svc *service
}

// createConsignment handles POST /logistics/consignments.
func (h *handler) createConsignment(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req createConsignmentRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	consignment, err := h.svc.createConsignment(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, consignment)
}

// dispatchConsignment handles POST /logistics/consignments/{consignmentID}/dispatch.
func (h *handler) dispatchConsignment(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	consignment, err := h.svc.dispatchConsignment(r.Context(), actor, chi.URLParam(r, "consignmentID"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, consignment)
}

// listConsignments handles GET /logistics/consignments.
func (h *handler) listConsignments(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	query := consignmentListQuery{
		DCSID:  r.URL.Query().Get("dcs_id"),
		Date:   r.URL.Query().Get("date"),
		Status: r.URL.Query().Get("status"),
	}
	page := httpx.ParsePage(r)
	items, total, err := h.svc.listConsignments(r.Context(), actor, query, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// createTrip handles POST /logistics/trips.
func (h *handler) createTrip(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req createTripRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.createTrip(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, trip)
}

// pickupStop handles POST /logistics/trips/{tripID}/stops/{consignmentID}/pickup.
func (h *handler) pickupStop(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req pickupRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.pickupStop(r.Context(), actor,
		chi.URLParam(r, "tripID"), chi.URLParam(r, "consignmentID"), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// logColdChain handles POST /logistics/trips/{tripID}/cold-chain.
func (h *handler) logColdChain(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req coldChainRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.logColdChain(r.Context(), actor, chi.URLParam(r, "tripID"), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// deliverTrip handles POST /logistics/trips/{tripID}/deliver.
func (h *handler) deliverTrip(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req deliverRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.deliverTrip(r.Context(), actor, chi.URLParam(r, "tripID"), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// listTrips handles GET /logistics/trips.
func (h *handler) listTrips(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	query := tripListQuery{
		UnionID:         r.URL.Query().Get("union_id"),
		Date:            r.URL.Query().Get("date"),
		VanRiderPartyID: r.URL.Query().Get("van_rider_party_id"),
	}
	page := httpx.ParsePage(r)
	items, total, err := h.svc.listTrips(r.Context(), actor, query, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// getTrip handles GET /logistics/trips/{tripID}.
func (h *handler) getTrip(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	trip, err := h.svc.getTrip(r.Context(), actor, chi.URLParam(r, "tripID"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

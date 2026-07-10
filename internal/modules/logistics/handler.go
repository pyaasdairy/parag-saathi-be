package logistics

import (
	"net/http"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the logistics module: decode, delegate to the
// service, respond. No business logic, no MongoDB.
type handler struct {
	svc *service
}

// optionalQueryID parses an optional ObjectID query parameter; absence yields
// the zero ObjectID (meaning "not filtered").
func optionalQueryID(r *http.Request, param string) (primitive.ObjectID, error) {
	v := r.URL.Query().Get(param)
	if v == "" {
		return primitive.NilObjectID, nil
	}
	return httpx.ParseID(v, param)
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
	id, err := httpx.PathID(r, "consignmentID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	consignment, err := h.svc.dispatchConsignment(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, consignment)
}

// listConsignments handles GET /logistics/consignments.
func (h *handler) listConsignments(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	dcsID, err := optionalQueryID(r, "dcs_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	query := consignmentListQuery{
		DCSID:  dcsID,
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

// getConsignment handles GET /logistics/consignments/{consignmentID}.
func (h *handler) getConsignment(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	id, err := httpx.PathID(r, "consignmentID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	consignment, err := h.svc.getConsignment(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, consignment)
}

// approveForUnion handles POST /logistics/consignments/{consignmentID}/approve-union.
func (h *handler) approveForUnion(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	id, err := httpx.PathID(r, "consignmentID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	invoice, err := h.svc.approveForUnion(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, invoice)
}

// getConsignmentInvoice handles GET /logistics/consignments/{consignmentID}/invoice.
func (h *handler) getConsignmentInvoice(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	id, err := httpx.PathID(r, "consignmentID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	invoice, err := h.svc.getConsignmentInvoice(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, invoice)
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
	tripID, err := httpx.PathID(r, "tripID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	consignmentID, err := httpx.PathID(r, "consignmentID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req pickupRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.pickupStop(r.Context(), actor, tripID, consignmentID, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// logColdChain handles POST /logistics/trips/{tripID}/cold-chain.
func (h *handler) logColdChain(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	tripID, err := httpx.PathID(r, "tripID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req coldChainRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.logColdChain(r.Context(), actor, tripID, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// deliverTrip handles POST /logistics/trips/{tripID}/deliver.
func (h *handler) deliverTrip(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	tripID, err := httpx.PathID(r, "tripID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req deliverRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.deliverTrip(r.Context(), actor, tripID, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// listTrips handles GET /logistics/trips.
func (h *handler) listTrips(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	unionID, err := optionalQueryID(r, "union_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	riderID, err := optionalQueryID(r, "van_rider_party_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	query := tripListQuery{
		UnionID:         unionID,
		Date:            r.URL.Query().Get("date"),
		VanRiderPartyID: riderID,
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
	tripID, err := httpx.PathID(r, "tripID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.getTrip(r.Context(), actor, tripID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// recordLocation handles POST /logistics/trips/{tripID}/location — one live GPS
// ping from the van while en route.
func (h *handler) recordLocation(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	tripID, err := httpx.PathID(r, "tripID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req locationRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	trip, err := h.svc.recordLocation(r.Context(), actor, tripID, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trip)
}

// trackTrip handles GET /logistics/trips/{tripID}/track — the minimal live view
// for a source Sachiv or the destination BMC.
func (h *handler) trackTrip(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	tripID, err := httpx.PathID(r, "tripID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	track, err := h.svc.trackTrip(r.Context(), actor, tripID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, track)
}

// listActiveTracking handles GET /logistics/trips/tracking — every IN_PROGRESS
// trip the caller may watch (inbound vans for a BMC, DCS-bearing trips for a
// Sachiv, all live trips for a union supervisor).
func (h *handler) listActiveTracking(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	tracks, err := h.svc.listActiveTracking(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, tracks)
}

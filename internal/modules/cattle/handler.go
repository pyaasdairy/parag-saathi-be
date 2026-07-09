package cattle

import (
	"net/http"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler adapts HTTP to the service: decode, call, respond — no business
// logic and no MongoDB in this file. Path and query ObjectIDs are parsed
// here (httpx.PathID / httpx.ParseID) so the service sees typed IDs only.
type handler struct {
	svc *service
}

// newHandler binds the handler to the service.
func newHandler(svc *service) *handler {
	return &handler{svc: svc}
}

// queryID parses an optional ObjectID query parameter: absent → NilObjectID.
func queryID(r *http.Request, param string) (primitive.ObjectID, error) {
	v := r.URL.Query().Get(param)
	if v == "" {
		return primitive.NilObjectID, nil
	}
	return httpx.ParseID(v, param)
}

// registerAnimal handles POST /cattle/animals.
func (h *handler) registerAnimal(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req RegisterAnimalRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	animal, err := h.svc.RegisterAnimal(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, animal)
}

// listAnimals handles GET /cattle/animals.
func (h *handler) listAnimals(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	page := httpx.ParsePage(r)
	ownerPartyID, err := queryID(r, "owner_party_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dcsID, err := queryID(r, "dcs_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	animals, total, err := h.svc.ListAnimals(r.Context(), actor, ownerPartyID, dcsID, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, animals, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// getAnimal handles GET /cattle/animals/{animalID}.
func (h *handler) getAnimal(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	animalID, err := httpx.PathID(r, "animalID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	animal, err := h.svc.GetAnimal(r.Context(), actor, animalID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, animal)
}

// listHealthEvents handles GET /cattle/animals/{animalID}/health-events.
func (h *handler) listHealthEvents(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	page := httpx.ParsePage(r)
	animalID, err := httpx.PathID(r, "animalID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	events, total, err := h.svc.ListHealthEvents(r.Context(), actor, animalID, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, events, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// logHealthEvent handles POST /cattle/animals/{animalID}/health-events.
func (h *handler) logHealthEvent(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	animalID, err := httpx.PathID(r, "animalID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req LogHealthEventRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	event, err := h.svc.LogHealthEvent(r.Context(), actor, animalID, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, event)
}

// syncBharatPashudhan handles POST /cattle/animals/{animalID}/bp-sync.
func (h *handler) syncBharatPashudhan(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	animalID, err := httpx.PathID(r, "animalID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	result, err := h.svc.SyncBharatPashudhan(r.Context(), actor, animalID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

// createMVUCase handles POST /cattle/mvu-cases.
func (h *handler) createMVUCase(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req CreateMVUCaseRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	mvuCase, err := h.svc.CreateMVUCase(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, mvuCase)
}

// dispatchMVUCase handles POST /cattle/mvu-cases/{caseID}/dispatch.
func (h *handler) dispatchMVUCase(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	caseID, err := httpx.PathID(r, "caseID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	mvuCase, err := h.svc.DispatchMVUCase(r.Context(), actor, caseID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, mvuCase)
}

// closeMVUCase handles POST /cattle/mvu-cases/{caseID}/close.
func (h *handler) closeMVUCase(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	caseID, err := httpx.PathID(r, "caseID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req CloseMVUCaseRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	mvuCase, err := h.svc.CloseMVUCase(r.Context(), actor, caseID, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, mvuCase)
}

// listMVUCases handles GET /cattle/mvu-cases.
func (h *handler) listMVUCases(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	page := httpx.ParsePage(r)
	dcsID, err := queryID(r, "dcs_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	farmerPartyID, err := queryID(r, "farmer_party_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	cases, total, err := h.svc.ListMVUCases(r.Context(), actor, dcsID, farmerPartyID, r.URL.Query().Get("status"), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, cases, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// listEducation handles GET /cattle/education.
func (h *handler) listEducation(w http.ResponseWriter, r *http.Request) {
	page := httpx.ParsePage(r)
	q := r.URL.Query()
	items, total, err := h.svc.ListEducation(r.Context(), q.Get("topic"), q.Get("language"), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// createEducation handles POST /cattle/education.
func (h *handler) createEducation(w http.ResponseWriter, r *http.Request) {
	var req CreateEducationRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	content, err := h.svc.CreateEducation(r.Context(), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, content)
}

// ingestTelemetry handles POST /cattle/telemetry — the dormant collar path.
func (h *handler) ingestTelemetry(w http.ResponseWriter, r *http.Request) {
	var req TelemetryRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ack, err := h.svc.IngestTelemetry(r.Context(), req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, ack)
}

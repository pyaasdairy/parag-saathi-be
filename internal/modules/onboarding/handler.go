package onboarding

import (
	"net/http"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the onboarding module: decode, validate,
// delegate to the service, respond. No business logic, no MongoDB.
type handler struct {
	svc *service
}

// newHandler wires the handler.
func newHandler(svc *service) *handler {
	return &handler{svc: svc}
}

// actorOr401 extracts the authenticated actor; the middleware guarantees it is
// present on protected routes, so a miss is a hard 401.
func actorOr401(w http.ResponseWriter, r *http.Request) (auth.Actor, bool) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
	}
	return actor, ok
}

// submit handles POST /onboarding/requests.
func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	var req submitRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	request, err := h.svc.submit(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, request)
}

// list handles GET /onboarding/requests?status=&submitted_by=.
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != domain.OnboardingStatusPending &&
		status != domain.OnboardingStatusApproved && status != domain.OnboardingStatusRejected {
		httpx.Error(w, r, httpx.BadRequest("INVALID_STATUS", "status must be one of PENDING, APPROVED, REJECTED"))
		return
	}
	var submittedBy *primitive.ObjectID
	if raw := r.URL.Query().Get("submitted_by"); raw != "" {
		id, err := httpx.ParseID(raw, "submitted_by")
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		submittedBy = &id
	}
	page := httpx.ParsePage(r)
	items, total, err := h.svc.list(r.Context(), actor, status, submittedBy, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// approve handles POST /onboarding/requests/{id}/approve.
func (h *handler) approve(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	request, err := h.svc.approve(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, request)
}

// reject handles POST /onboarding/requests/{id}/reject.
func (h *handler) reject(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req rejectRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	request, err := h.svc.reject(r.Context(), actor, id, req.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, request)
}

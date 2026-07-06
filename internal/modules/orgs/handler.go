package orgs

import (
	"net/http"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the orgs module: decode, validate shape,
// delegate to the service, respond. No business logic, no Mongo.
type handler struct {
	svc *service
}

func newHandler(svc *service) *handler {
	return &handler{svc: svc}
}

// create handles POST /orgs.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	var req CreateOrgRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	org, err := h.svc.Create(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, org)
}

// update handles PATCH /orgs/{id}.
func (h *handler) update(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req UpdateOrgRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	org, err := h.svc.Update(r.Context(), actor, id, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, org)
}

// get handles GET /orgs/{id}.
func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	org, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, org)
}

// children handles GET /orgs/{id}/children.
func (h *handler) children(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page := httpx.ParsePage(r)
	units, total, err := h.svc.Children(r.Context(), id, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, units, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total, Count: len(units)})
}

// tree handles GET /orgs/{id}/tree.
func (h *handler) tree(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	nodes, truncated, err := h.svc.Tree(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, nodes, TreeMeta{Count: len(nodes), Truncated: truncated})
}

// list handles GET /orgs?type=&district=&code= (code is the unique business
// key, exact match — e.g. ?code=DCS-01842 resolves a seeded org's ObjectID).
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	page := httpx.ParsePage(r)
	q := r.URL.Query()
	units, total, err := h.svc.List(r.Context(), q.Get("type"), q.Get("district"), q.Get("code"), page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, units, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total, Count: len(units)})
}

// members handles GET /orgs/{id}/members.
func (h *handler) members(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page := httpx.ParsePage(r)
	members, total, err := h.svc.Members(r.Context(), actor, id, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, members, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total, Count: len(members)})
}

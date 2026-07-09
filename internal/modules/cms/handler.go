package cms

import (
	"net/http"
	"strconv"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the CMS module: decode, validate shape,
// delegate to the service, respond. No business logic, no Mongo.
type handler struct {
	svc *service
}

func newHandler(svc *service) *handler {
	return &handler{svc: svc}
}

// list handles GET /content?type=&since=<version>&scope= — the versioned
// delta pull.
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since int64
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpx.Error(w, r, httpx.BadRequest("INVALID_SINCE", "since must be an integer version cursor"))
			return
		}
		since = n
	}
	items, meta, err := h.svc.Delta(r.Context(), q.Get("type"), q.Get("scope"), since)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, meta)
}

// helpline handles GET /content/helpline?scope= — region-scoped helpline
// numbers for the Get-Help screen.
func (h *handler) helpline(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.Helpline(r.Context(), r.URL.Query().Get("scope"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, HelplineMeta{Count: len(items)})
}

// create handles POST /content.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	var req CreateContentRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	c, err := h.svc.Create(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}

// update handles PUT /content/{id}.
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
	var req UpdateContentRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	c, err := h.svc.Update(r.Context(), actor, id, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

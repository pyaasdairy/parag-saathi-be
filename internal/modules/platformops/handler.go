package platformops

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP layer: decode, validate shape, call service, respond.
type handler struct {
	svc *service
}

// newHandler wires the handler.
func newHandler(svc *service) *handler {
	return &handler{svc: svc}
}

// listFlags handles GET /admin/flags.
func (h *handler) listFlags(w http.ResponseWriter, r *http.Request) {
	all, err := h.svc.listFlags(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, all)
}

// setFlag handles PUT /admin/flags/{key}.
func (h *handler) setFlag(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}

	var req SetFlagRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.Enabled == nil {
		httpx.Error(w, r, httpx.BadRequest("MISSING_ENABLED", "enabled is required"))
		return
	}

	flag, err := h.svc.setFlag(r.Context(), actor, chi.URLParam(r, "key"), *req.Enabled)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, flag)
}

// adminStats handles GET /admin/stats — the read-only control-tower aggregate.
func (h *handler) adminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.svc.adminStats(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}

// listProducts handles GET /admin/products.
func (h *handler) listProducts(w http.ResponseWriter, r *http.Request) {
	products, err := h.svc.listProducts(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, products)
}

// upsertProduct handles PUT /admin/products — upsert by SKU.
func (h *handler) upsertProduct(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	var req UpsertProductRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	product, err := h.svc.upsertProduct(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, product)
}

// listActiveProducts handles GET /products — the session-readable active
// catalogue (any logged-in party; e.g. a plant operator picking product
// options for a lot).
func (h *handler) listActiveProducts(w http.ResponseWriter, r *http.Request) {
	products, err := h.svc.listActiveProducts(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, products)
}

// getSachivCap handles GET /admin/sachiv-cap.
func (h *handler) getSachivCap(w http.ResponseWriter, r *http.Request) {
	resp, err := h.svc.getSachivCap(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// setSachivCap handles PUT /admin/sachiv-cap.
func (h *handler) setSachivCap(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	var req SachivCapRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.setSachivCap(r.Context(), actor, req.Cap)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// listAuditLogs handles GET /audit/logs?actor_party_id=&target_type=&action=&from=&to=.
func (h *handler) listAuditLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := auditLogFilter{
		ActorPartyID: query.Get("actor_party_id"),
		TargetType:   query.Get("target_type"),
		Action:       query.Get("action"),
	}

	from, err := parseTimeParam(query.Get("from"), false)
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("INVALID_FROM", "from must be RFC3339 or YYYY-MM-DD"))
		return
	}
	to, err := parseTimeParam(query.Get("to"), true)
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("INVALID_TO", "to must be RFC3339 or YYYY-MM-DD"))
		return
	}
	filter.From, filter.To = from, to

	page := httpx.ParsePage(r)
	entries, total, err := h.svc.listAuditLogs(r.Context(), filter, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, entries, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// exportAuditLogs handles GET /audit/logs/export?from=&to= — the capped JSON
// attachment for the GIGW/DPDP immutable audit export (§12).
func (h *handler) exportAuditLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	from, err := parseTimeParam(query.Get("from"), false)
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("INVALID_FROM", "from must be RFC3339 or YYYY-MM-DD"))
		return
	}
	to, err := parseTimeParam(query.Get("to"), true)
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("INVALID_TO", "to must be RFC3339 or YYYY-MM-DD"))
		return
	}

	export, err := h.svc.exportAuditLogs(r.Context(), from, to)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	filename := "audit-export-" + export.ExportedAt.Format("20060102-150405") + ".json"
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	httpx.JSON(w, http.StatusOK, export)
}

// listNotifications handles GET /notifications?phone=&status=.
func (h *handler) listNotifications(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	phone := strings.TrimSpace(query.Get("phone"))
	status := query.Get("status")
	switch status {
	case "", domain.NotificationQueued, domain.NotificationSent, domain.NotificationFailed:
	default:
		httpx.Error(w, r, httpx.BadRequest("INVALID_STATUS", "status must be QUEUED, SENT or FAILED"))
		return
	}

	page := httpx.ParsePage(r)
	notifications, total, err := h.svc.listNotifications(r.Context(), phone, status, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, notifications, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// listMyNotifications handles GET /notifications/me — any authenticated
// party's own inbox, newest first, paged.
func (h *handler) listMyNotifications(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	page := httpx.ParsePage(r)
	notifications, total, err := h.svc.listMyNotifications(r.Context(), actor, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, notifications, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// markNotificationRead handles POST /notifications/{id}/read.
func (h *handler) markNotificationRead(w http.ResponseWriter, r *http.Request) {
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
	notification, err := h.svc.markNotificationRead(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, notification)
}

// runWorker handles POST /notifications/worker/run — the manual mock-SMS
// dispatch trigger (deterministic demo; production = cron/queue consumer).
func (h *handler) runWorker(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.runWorker(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

// lookupParty handles GET /support/parties/lookup?phone=.
func (h *handler) lookupParty(w http.ResponseWriter, r *http.Request) {
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_PHONE", "phone is required"))
		return
	}

	view, err := h.svc.lookupParty(r.Context(), phone)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, view)
}

// parseTimeParam accepts RFC3339 or plain YYYY-MM-DD (empty → nil). Date-only
// values mean UTC midnight; with endOfDay they cover the whole day, so
// ?to=2026-07-06 is inclusive of the 6th.
func parseTimeParam(value string, endOfDay bool) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		t = t.UTC()
		return &t, nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, err
	}
	if endOfDay {
		t = t.Add(24*time.Hour - time.Nanosecond)
	}
	t = t.UTC()
	return &t, nil
}

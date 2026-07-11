package quality

import (
	"net/http"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"

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

// recordQCResult handles POST /quality/qc-results.
func (h *handler) recordQCResult(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}

	var req RecordQCResultRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := validateRecordRequest(req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.svc.recordQCResult(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, result)
}

// validateRecordRequest checks required fields and test shape.
func validateRecordRequest(req RecordQCResultRequest) error {
	if strings.TrimSpace(req.SubjectType) == "" {
		return httpx.BadRequest("MISSING_SUBJECT_TYPE", "subject_type is required")
	}
	if req.SubjectID.IsZero() {
		return httpx.BadRequest("MISSING_SUBJECT_ID", "subject_id is required")
	}
	if strings.TrimSpace(req.Stage) == "" {
		return httpx.BadRequest("MISSING_STAGE", "stage is required")
	}
	if len(req.Tests) == 0 {
		return httpx.BadRequest("MISSING_TESTS", "at least one test is required")
	}
	for i, t := range req.Tests {
		if strings.TrimSpace(t.Name) == "" {
			return httpx.BadRequest("MISSING_TEST_NAME", "tests["+strconv.Itoa(i)+"].name is required")
		}
		if t.Value < 0 {
			return httpx.BadRequest("INVALID_TEST_VALUE", "tests["+strconv.Itoa(i)+"].value must not be negative")
		}
	}
	return nil
}

// resolveQCResult handles POST /quality/qc-results/{id}/resolve — the
// HOLD→PASS/REJECT resolution on a quarantined subject (§13.5).
func (h *handler) resolveQCResult(w http.ResponseWriter, r *http.Request) {
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
	var req ResolveQCResultRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	result, err := h.svc.resolveQCResult(r.Context(), actor, id, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

// listQCResults handles GET /quality/qc-results?subject_type=&subject_id=.
func (h *handler) listQCResults(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}

	subjectType := r.URL.Query().Get("subject_type")
	subjectID := primitive.NilObjectID
	if raw := r.URL.Query().Get("subject_id"); raw != "" {
		id, err := httpx.ParseID(raw, "subject_id")
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		subjectID = id
	}
	page := httpx.ParsePage(r)

	results, total, err := h.svc.listQCResults(r.Context(), actor, subjectType, subjectID, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, results, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// getQCResult handles GET /quality/qc-results/{id}.
func (h *handler) getQCResult(w http.ResponseWriter, r *http.Request) {
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

	result, err := h.svc.getQCResult(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

// getLimits handles GET /quality/limits.
func (h *handler) getLimits(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, h.svc.limits())
}

// getQCQueue handles GET /quality/qc-queue.
func (h *handler) getQCQueue(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	queue, err := h.svc.qcQueue(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, queue)
}

// issueCertificate handles POST /quality/batches/{id}/certificate.
func (h *handler) issueCertificate(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	batchID, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	cert, err := h.svc.issueCertificate(r.Context(), actor, batchID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, cert)
}

// traceBack handles GET /quality/batches/{id}/trace-back.
func (h *handler) traceBack(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	batchID, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	trace, err := h.svc.traceBack(r.Context(), actor, batchID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, trace)
}

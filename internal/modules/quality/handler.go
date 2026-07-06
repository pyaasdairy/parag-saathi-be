package quality

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

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
	if strings.TrimSpace(req.SubjectID) == "" {
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

// listQCResults handles GET /quality/qc-results?subject_type=&subject_id=.
func (h *handler) listQCResults(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}

	subjectType := r.URL.Query().Get("subject_type")
	subjectID := r.URL.Query().Get("subject_id")
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

	result, err := h.svc.getQCResult(r.Context(), actor, chi.URLParam(r, "id"))
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

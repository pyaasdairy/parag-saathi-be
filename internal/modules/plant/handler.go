package plant

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// Handler owns the plant module's HTTP surface: decode, validate shape,
// delegate to the service, respond. No business logic, no MongoDB.
type Handler struct {
	svc *Service
}

// NewHandler wires the handler to the service.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// CreateBMCLot handles POST /plant/bmc-lots.
func (h *Handler) CreateBMCLot(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req CreateBMCLotRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.BMCID == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "bmc_id is required"))
		return
	}
	if req.Shift != domain.ShiftMorning && req.Shift != domain.ShiftEvening {
		httpx.Error(w, r, httpx.BadRequest("INVALID_SHIFT", "shift must be MORNING or EVENING"))
		return
	}
	if len(req.ConsignmentIDs) == 0 {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "consignment_ids must be non-empty"))
		return
	}
	if req.Date != "" && !validDateKey(req.Date) {
		httpx.Error(w, r, httpx.BadRequest("INVALID_DATE", "date must be YYYY-MM-DD"))
		return
	}
	lot, err := h.svc.CreateBMCLot(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, lot)
}

// CloseBMCLot handles POST /plant/bmc-lots/{id}/close.
func (h *Handler) CloseBMCLot(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req CloseBMCLotRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.ChillingTempC == nil {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "chilling_temp_c is required"))
		return
	}
	lot, err := h.svc.CloseBMCLot(r.Context(), actor, chi.URLParam(r, "id"), *req.ChillingTempC)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, lot)
}

// DispatchBMCLot handles POST /plant/bmc-lots/{id}/dispatch — the safety gate.
func (h *Handler) DispatchBMCLot(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	lot, err := h.svc.DispatchBMCLot(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, lot)
}

// ListBMCLots handles GET /plant/bmc-lots?bmc_id=&date=&status=.
func (h *Handler) ListBMCLots(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	q := r.URL.Query()
	if d := q.Get("date"); d != "" && !validDateKey(d) {
		httpx.Error(w, r, httpx.BadRequest("INVALID_DATE", "date must be YYYY-MM-DD"))
		return
	}
	page := httpx.ParsePage(r)
	lots, total, err := h.svc.ListBMCLots(r.Context(), actor, BMCLotListFilter{
		BMCID:  q.Get("bmc_id"),
		Date:   q.Get("date"),
		Status: q.Get("status"),
	}, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, lots, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// CreateBatch handles POST /plant/batches.
func (h *Handler) CreateBatch(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req CreateBatchRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.PlantID == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "plant_id is required"))
		return
	}
	if len(req.BMCLotIDs) == 0 {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "bmc_lot_ids must be non-empty"))
		return
	}
	if req.ProductType == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "product_type is required"))
		return
	}
	batch, err := h.svc.CreateBatch(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, batch)
}

// CompleteBatch handles POST /plant/batches/{id}/complete.
func (h *Handler) CompleteBatch(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	batch, err := h.svc.CompleteBatch(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, batch)
}

// GetBatch handles GET /plant/batches/{id}.
func (h *Handler) GetBatch(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	batch, err := h.svc.GetBatch(r.Context(), actor, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, batch)
}

// ListBatches handles GET /plant/batches?plant_id=&status=.
func (h *Handler) ListBatches(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	q := r.URL.Query()
	page := httpx.ParsePage(r)
	batches, total, err := h.svc.ListBatches(r.Context(), actor, BatchListFilter{
		PlantID: q.Get("plant_id"),
		Status:  q.Get("status"),
	}, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, batches, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// CreateProductLot handles POST /plant/product-lots.
func (h *Handler) CreateProductLot(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req CreateProductLotRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	switch {
	case req.BatchID == "":
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "batch_id is required"))
		return
	case req.SKU == "":
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "sku is required"))
		return
	case req.ProductName == "":
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "product_name is required"))
		return
	case req.Units <= 0:
		httpx.Error(w, r, httpx.BadRequest("INVALID_UNITS", "units must be a positive integer"))
		return
	case req.UnitSize == "":
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "unit_size is required"))
		return
	case req.MRP < 0:
		httpx.Error(w, r, httpx.BadRequest("INVALID_MRP", "mrp cannot be negative"))
		return
	case req.ExpiryDate == "" || !validDateKey(req.ExpiryDate):
		httpx.Error(w, r, httpx.BadRequest("INVALID_DATE", "expiry_date must be YYYY-MM-DD"))
		return
	case req.MfgDate != "" && !validDateKey(req.MfgDate):
		httpx.Error(w, r, httpx.BadRequest("INVALID_DATE", "mfg_date must be YYYY-MM-DD"))
		return
	}
	lot, err := h.svc.CreateProductLot(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, lot)
}

// RecallProductLot handles POST /plant/product-lots/{id}/recall.
func (h *Handler) RecallProductLot(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req RecallProductLotRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.Reason == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "reason is required"))
		return
	}
	lot, err := h.svc.RecallProductLot(r.Context(), actor, chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, lot)
}

// IssueQR handles POST /plant/qrs.
func (h *Handler) IssueQR(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	var req IssueQRRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.ProductLotID == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_FIELD", "product_lot_id is required"))
		return
	}
	qr, err := h.svc.IssueQR(r.Context(), actor, req.ProductLotID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, qr)
}

// ListQRs handles GET /plant/qrs?product_lot_id=.
func (h *Handler) ListQRs(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.ActorFrom(r.Context())
	page := httpx.ParsePage(r)
	qrs, total, err := h.svc.ListQRs(r.Context(), actor, QRListFilter{
		ProductLotID: r.URL.Query().Get("product_lot_id"),
	}, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, qrs, ListMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// validDateKey reports whether s is a well-formed YYYY-MM-DD day key.
func validDateKey(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

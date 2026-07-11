package identity

import (
	"net/http"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// handler is the HTTP edge of the identity module: decode, validate,
// delegate to the service, respond. No business logic, no MongoDB.
type handler struct {
	svc *service
}

// newHandler wires the handler.
func newHandler(svc *service) *handler {
	return &handler{svc: svc}
}

// actorOr401 extracts the authenticated actor; the middleware guarantees it
// is present on protected routes, so a miss is a hard 401.
func actorOr401(w http.ResponseWriter, r *http.Request) (auth.Actor, bool) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
	}
	return actor, ok
}

// --- /auth ---

// requestOTP handles POST /auth/otp/request.
func (h *handler) requestOTP(w http.ResponseWriter, r *http.Request) {
	var req otpRequestRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.requestOTP(r.Context(), req.Phone)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// verifyOTP handles POST /auth/otp/verify.
func (h *handler) verifyOTP(w http.ResponseWriter, r *http.Request) {
	var req otpVerifyRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.verifyOTP(r.Context(), req.Phone, req.OTP)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// refresh handles POST /auth/refresh.
func (h *handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.refresh(r.Context(), req.RefreshToken)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// logout handles POST /auth/logout.
func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	var req logoutRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.logout(r.Context(), actor, req.RefreshToken); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"logged_out": true})
}

// listMyRoles handles GET /auth/roles.
func (h *handler) listMyRoles(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	roles, err := h.svc.listMyRoles(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, roles)
}

// selectRole handles POST /auth/role/select.
func (h *handler) selectRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	var req roleSelectRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.selectRole(r.Context(), actor, req.RoleAssignmentID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// --- /parties ---

// getMe handles GET /parties/me.
func (h *handler) getMe(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	resp, err := h.svc.getMe(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// patchMe handles PATCH /parties/me.
func (h *handler) patchMe(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	var req patchMeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	party, err := h.svc.patchMe(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, party)
}

// listPartiesByRole handles GET /parties?role=<CODE>&org_unit_id=<id> — the
// reviewer directory of parties holding an active role in an org unit (backs
// the FE listSachivs picker). Both query params are required.
func (h *handler) listPartiesByRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	roleCode := r.URL.Query().Get("role")
	if roleCode == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_ROLE", "role query parameter is required"))
		return
	}
	if !domain.IsValidRole(roleCode) {
		httpx.Error(w, r, httpx.BadRequest("INVALID_ROLE", "role is not in the role catalog"))
		return
	}
	// org_unit_id is OPTIONAL: omitted → the service scopes to the caller's own
	// org (or federation-wide for admins), so a bare ?role= call works.
	var orgUnitID primitive.ObjectID
	if raw := r.URL.Query().Get("org_unit_id"); raw != "" {
		id, err := httpx.ParseID(raw, "org_unit_id")
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		orgUnitID = id
	}
	page := httpx.ParsePage(r)
	parties, total, err := h.svc.listPartiesByRole(r.Context(), actor, roleCode, orgUnitID, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, parties, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// --- /kyc ---

// verifyAadhaar handles POST /kyc/aadhaar. A fresh submission returns 201;
// an idempotent replay of an existing PENDING request returns 200.
func (h *handler) verifyAadhaar(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	var req aadhaarKYCRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, created, err := h.svc.verifyAadhaar(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, resp)
}

// verifyBank handles POST /kyc/bank.
func (h *handler) verifyBank(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	var req bankKYCRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	record, err := h.svc.verifyBank(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, record)
}

// listMyKYC handles GET /kyc/me.
func (h *handler) listMyKYC(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	page := httpx.ParsePage(r)
	records, total, err := h.svc.listMyKYC(r.Context(), actor, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, records, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// listPendingKYC handles GET /kyc/pending — reviewer console.
func (h *handler) listPendingKYC(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	page := httpx.ParsePage(r)
	items, total, err := h.svc.listPendingKYC(r.Context(), actor, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, items, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

// pendingKYCCount handles GET /kyc/pending/count — the live badge value the
// reviewer dashboard renders and re-fetches on each "kyc.pending.changed" SSE
// nudge. Scoped to the reviewer's area.
func (h *handler) pendingKYCCount(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	count, capped, err := h.svc.pendingKYCCount(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pendingKYCCountResponse{Count: count, Capped: capped})
}

// approveKYC handles POST /kyc/{id}/approve.
func (h *handler) approveKYC(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.approveKYC(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// rejectKYC handles POST /kyc/{id}/reject.
func (h *handler) rejectKYC(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req kycRejectRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.rejectKYC(r.Context(), actor, id, req.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// verifyPartyKYC handles POST /parties/{id}/kyc/verify — an authorised reviewer
// directly vouches a party up to a tier (no PENDING record needed), the
// admin-side counterpart to approve/reject.
func (h *handler) verifyPartyKYC(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	partyID, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req kycVerifyRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	party, err := h.svc.verifyPartyKYC(r.Context(), actor, partyID, req.Tier, req.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, party)
}

// --- /roles ---

// createAssignment handles POST /roles/assignments.
func (h *handler) createAssignment(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	var req createAssignmentRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	assignment, err := h.svc.createAssignment(r.Context(), actor, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, assignment)
}

// revokeAssignment handles DELETE /roles/assignments/{id}.
func (h *handler) revokeAssignment(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	assignment, err := h.svc.revokeAssignment(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, assignment)
}

// transferAssignment handles POST /roles/assignments/{id}/transfer — move an
// ACTIVE assignment's role to another org unit (create new + revoke old).
func (h *handler) transferAssignment(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req transferAssignmentRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.transferAssignment(r.Context(), actor, id, req.ToOrgUnitID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// replaceHolder handles POST /orgs/{id}/replace-holder — swap THE holder of a
// role at an org unit (grant to the new party, revoke every other holder).
func (h *handler) replaceHolder(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	orgUnitID, err := httpx.PathID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req replaceHolderRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp, err := h.svc.replaceHolder(r.Context(), actor, orgUnitID, req.RoleCode, req.NewPartyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// listAssignments handles GET /roles/assignments?org_unit_id=&role_code=.
func (h *handler) listAssignments(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr401(w, r)
	if !ok {
		return
	}
	rawOrgUnitID := r.URL.Query().Get("org_unit_id")
	if rawOrgUnitID == "" {
		httpx.Error(w, r, httpx.BadRequest("MISSING_ORG_UNIT", "org_unit_id query parameter is required"))
		return
	}
	orgUnitID, err := httpx.ParseID(rawOrgUnitID, "org_unit_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	roleCode := r.URL.Query().Get("role_code")
	if roleCode != "" && !domain.IsValidRole(roleCode) {
		httpx.Error(w, r, httpx.BadRequest("INVALID_ROLE", "role_code is not in the role catalog"))
		return
	}
	page := httpx.ParsePage(r)
	assignments, total, err := h.svc.listAssignments(r.Context(), actor, orgUnitID, roleCode, page)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSONMeta(w, http.StatusOK, assignments, listMeta{Limit: page.Limit, Offset: page.Offset, Total: total})
}

package onboarding

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// httpxNotFound reports whether err is an httpx 404 — used to translate a
// lost concurrency race (the atomic status flip found nothing PENDING) into a
// 409 for the caller.
func httpxNotFound(err error) bool {
	var ae *httpx.AppError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// defaultLanguage is the vernacular fallback when a party has no preference.
const defaultLanguage = "hi"

// service holds every business rule of the onboarding module.
type service struct {
	deps *deps.Deps
	repo *repository
	log  *slog.Logger
}

// newService wires the service.
func newService(d *deps.Deps, repo *repository, log *slog.Logger) *service {
	return &service{deps: d, repo: repo, log: log}
}

// actorID parses the actor's party id (an ObjectID hex string in the JWT).
func actorID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// upgradedKYCTier returns the tier a party should hold after verification at
// requestedTier — upgrades only, never a downgrade (parallel tier-1 proofs are
// treated as equivalent by KYCTierSatisfies).
func upgradedKYCTier(currentTier, requestedTier string) string {
	if domain.KYCTierSatisfies(currentTier, requestedTier) {
		return currentTier
	}
	return requestedTier
}

// submit records a new assisted-onboarding request in the queue. The
// submitter must be authorised for the target org unit (RequireInScope); the
// requested role is validated at the handler edge.
func (s *service) submit(ctx context.Context, actor auth.Actor, req submitRequest) (*domain.OnboardingRequest, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, req.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "onboarding submit denied: out of scope",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("org_unit_id", req.OrgUnitID.Hex()),
			slog.String("requested_role", req.RequestedRole))
		return nil, err
	}

	now := time.Now().UTC()
	request := domain.OnboardingRequest{
		ID:              primitive.NewObjectID(),
		Phone:           req.Phone,
		FullName:        req.FullName,
		FullNameHi:      req.FullNameHi,
		RequestedRole:   req.RequestedRole,
		OrgUnitID:       req.OrgUnitID,
		RequestedTier:   req.RequestedTier,
		Note:            req.Note,
		DocumentRefs:    req.DocumentRefs,
		Village:         req.Village,
		DocumentType:    req.DocumentType,
		DocumentNumber:  req.DocumentNumber,
		KYCPhotoURL:     req.KYCPhotoURL,
		ProfilePhotoURL: req.ProfilePhotoURL,
		CattleCount:     req.CattleCount,
		CattleBreed:     req.CattleBreed,
		VehicleNumber:   req.VehicleNumber,
		EmployeeID:      req.EmployeeID,
		Status:          domain.OnboardingStatusPending,
		SubmittedBy:     aid,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.repo.insertRequest(ctx, request); err != nil {
		return nil, err
	}
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "onboarding.submit",
		TargetType: "onboarding_request",
		TargetID:   request.ID.Hex(),
		Meta: map[string]any{
			"phone":          req.Phone,
			"requested_role": req.RequestedRole,
			"requested_tier": req.RequestedTier,
			"org_unit_id":    req.OrgUnitID.Hex(),
		},
	})
	s.log.InfoContext(ctx, "onboarding request submitted",
		slog.String("request_id", request.ID.Hex()),
		slog.String("requested_role", req.RequestedRole),
		slog.String("requested_tier", req.RequestedTier),
		slog.String("org_unit_id", req.OrgUnitID.Hex()),
		slog.String("actor_party_id", actor.PartyID))
	return &request, nil
}

// list pages the onboarding queue, constrained to org units the reviewer can
// see, optionally filtered by status and submitter.
func (s *service) list(ctx context.Context, actor auth.Actor, status string, submittedBy *primitive.ObjectID, page httpx.Page) ([]domain.OnboardingRequest, int64, error) {
	requests, total, err := s.repo.listRequests(ctx, status, submittedBy, page)
	if err != nil {
		return nil, 0, err
	}
	// Org-scope gate: a scoped reviewer only ever sees requests whose target
	// org unit falls within their area. Platform-wide roles (SUPER_ADMIN,
	// PCDF_ADMIN rooted at the federation) pass RequireInScope naturally.
	out := make([]domain.OnboardingRequest, 0, len(requests))
	for _, req := range requests {
		if s.deps.Orgs.RequireInScope(ctx, actor, req.OrgUnitID) != nil {
			continue
		}
		out = append(out, req)
	}
	s.log.InfoContext(ctx, "onboarding queue listed",
		slog.String("actor_party_id", actor.PartyID),
		slog.Int("count", len(out)),
		slog.Int64("total", total))
	return out, total, nil
}

// approve runs the onboarding saga: it claims the PENDING request atomically,
// then create-or-finds the Party by phone, writes a VERIFIED KYCRecord at the
// requested tier, raises the party's KYC tier upward, grants the requested
// role@org and stamps CreatedParty on the request. The request must be PENDING
// and the reviewer must be in scope for the target org unit and authorised for
// the requested tier.
func (s *service) approve(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*domain.OnboardingRequest, error) {
	now := time.Now().UTC()
	rid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	request, err := s.repo.findRequestByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if request.Status != domain.OnboardingStatusPending {
		s.log.WarnContext(ctx, "onboarding approve rejected: not pending",
			slog.String("request_id", id.Hex()), slog.String("status", request.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("ONBOARDING_NOT_PENDING", "onboarding request is not pending review")
	}
	// Org scope: a reviewer may only decide requests within their area.
	if err := s.deps.Orgs.RequireInScope(ctx, actor, request.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "onboarding approve denied: out of scope",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("request_id", id.Hex()),
			slog.String("org_unit_id", request.OrgUnitID.Hex()))
		return nil, err
	}
	// Tier authority mirrors the KYC review console (§5.2): the reviewer's role
	// must be permitted to clear the requested tier.
	if !domain.CanApproveKYCTier(actor.RoleCode, request.RequestedTier) {
		s.log.WarnContext(ctx, "onboarding approve rejected: tier not approvable",
			slog.String("request_id", id.Hex()), slog.String("reviewer_role", actor.RoleCode),
			slog.String("requested_tier", request.RequestedTier))
		appErr := httpx.Forbidden("your role may not approve onboarding at the " + request.RequestedTier + " tier")
		appErr.Code = "KYC_TIER_NOT_APPROVABLE"
		return nil, appErr
	}

	// Claim the request FIRST (atomic PENDING→APPROVED) so a concurrent approve
	// loses the race before any party/kyc/assignment side effect runs.
	claimed, err := s.repo.markRequestApproved(ctx, id, rid, now)
	if err != nil {
		if httpxNotFound(err) {
			s.log.WarnContext(ctx, "onboarding approve lost race: no longer pending",
				slog.String("request_id", id.Hex()), slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Conflict("ONBOARDING_NOT_PENDING", "onboarding request is not pending review")
		}
		return nil, err
	}

	// 1. Find-or-create the Party by phone.
	party, err := s.repo.upsertPartyByPhone(ctx, claimed.Phone, claimed.FullName, now)
	if err != nil {
		return nil, err
	}

	// 2. Write a VERIFIED KYC record at the requested tier, reviewer-stamped.
	kyc := domain.KYCRecord{
		ID:             primitive.NewObjectID(),
		PartyID:        party.ID,
		RequestedTier:  claimed.RequestedTier,
		Status:         domain.KYCStatusVerified,
		ReviewedBy:     &rid,
		ReviewedByRole: actor.RoleCode,
		ReviewedAt:     &now,
		VerifiedAt:     &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.repo.insertKYCRecord(ctx, kyc); err != nil {
		return nil, err
	}

	// 3. Raise the party's KYC tier upward only.
	newTier := upgradedKYCTier(party.KYCTier, claimed.RequestedTier)
	if newTier != party.KYCTier {
		if err := s.repo.updatePartyKYCTier(ctx, party.ID, newTier, now); err != nil {
			return nil, err
		}
	}

	// 4. Grant the requested role@org (idempotent — skip if already held).
	exists, err := s.repo.activeAssignmentExists(ctx, party.ID, claimed.RequestedRole, claimed.OrgUnitID)
	if err != nil {
		return nil, err
	}
	if !exists {
		assignment := domain.RoleAssignment{
			ID:        primitive.NewObjectID(),
			PartyID:   party.ID,
			RoleCode:  claimed.RequestedRole,
			OrgUnitID: claimed.OrgUnitID,
			GrantedBy: &rid,
			ValidFrom: now,
			Status:    domain.RoleAssignmentActive,
			CreatedAt: now,
		}
		if err := s.repo.insertAssignment(ctx, assignment); err != nil {
			return nil, err
		}
	}

	// 5. Stamp the created party onto the request.
	updated, err := s.repo.setRequestCreatedParty(ctx, id, party.ID, now)
	if err != nil {
		return nil, err
	}

	// 6. Notify the new party (best-effort — a notify failure never fails the
	// saga, which is already durably committed).
	language := party.PreferredLanguage
	if language == "" {
		language = defaultLanguage
	}
	pid := party.ID
	if err := s.repo.insertNotification(ctx, domain.Notification{
		PartyID:     &pid,
		Phone:       party.Phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: domain.TemplateKYCApproved,
		Language:    language,
		Params:      map[string]string{"tier": claimed.RequestedTier},
		Status:      domain.NotificationQueued,
		QueuedAt:    now,
	}); err != nil {
		s.log.ErrorContext(ctx, "onboarding approve: party notify failed",
			slog.String("request_id", id.Hex()), slog.String("party_id", party.ID.Hex()), slog.Any("err", err))
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "onboarding.approve",
		TargetType: "onboarding_request",
		TargetID:   id.Hex(),
		Meta: map[string]any{
			"party_id":       party.ID.Hex(),
			"requested_role": claimed.RequestedRole,
			"requested_tier": claimed.RequestedTier,
			"new_tier":       newTier,
			"org_unit_id":    claimed.OrgUnitID.Hex(),
		},
	})
	s.log.InfoContext(ctx, "onboarding request approved",
		slog.String("request_id", id.Hex()),
		slog.String("party_id", party.ID.Hex()),
		slog.String("requested_role", claimed.RequestedRole),
		slog.String("requested_tier", claimed.RequestedTier),
		slog.String("new_tier", newTier),
		slog.String("actor_party_id", actor.PartyID))
	return updated, nil
}

// reject moves a PENDING request to REJECTED with a mandatory reason. Same
// scope authority as approve.
func (s *service) reject(ctx context.Context, actor auth.Actor, id primitive.ObjectID, reason string) (*domain.OnboardingRequest, error) {
	now := time.Now().UTC()
	rid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	request, err := s.repo.findRequestByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if request.Status != domain.OnboardingStatusPending {
		s.log.WarnContext(ctx, "onboarding reject rejected: not pending",
			slog.String("request_id", id.Hex()), slog.String("status", request.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("ONBOARDING_NOT_PENDING", "onboarding request is not pending review")
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, request.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "onboarding reject denied: out of scope",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("request_id", id.Hex()),
			slog.String("org_unit_id", request.OrgUnitID.Hex()))
		return nil, err
	}

	updated, err := s.repo.markRequestRejected(ctx, id, rid, reason, now)
	if err != nil {
		if httpxNotFound(err) {
			s.log.WarnContext(ctx, "onboarding reject lost race: no longer pending",
				slog.String("request_id", id.Hex()), slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Conflict("ONBOARDING_NOT_PENDING", "onboarding request is not pending review")
		}
		return nil, err
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "onboarding.reject",
		TargetType: "onboarding_request",
		TargetID:   id.Hex(),
		Meta: map[string]any{
			"phone":          updated.Phone,
			"requested_role": updated.RequestedRole,
			"reason":         reason,
		},
	})
	s.log.WarnContext(ctx, "onboarding request rejected",
		slog.String("request_id", id.Hex()),
		slog.String("requested_role", updated.RequestedRole),
		slog.String("actor_party_id", actor.PartyID),
		slog.String("reason", reason))
	return updated, nil
}

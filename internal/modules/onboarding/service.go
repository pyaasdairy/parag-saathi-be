package onboarding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

// Sachiv governance cap knob — the same app_settings key the identity grant
// path enforces (identity.sachivCapSettingKey), mirrored here so the approval
// saga cannot be used to bypass the per-DCS ceiling (§5.2).
const (
	sachivCapSettingKey = "sachiv_cap"
	defaultSachivCap    = 2
)

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

	// Duplicate guards (mirrors the FE mock contract): a phone that already owns
	// a Party is ALREADY_REGISTERED; a phone with a still-PENDING request is
	// ALREADY_PENDING — either way the doorstep enrolment must not create a
	// second queue item.
	if registered, err := s.repo.partyExistsByPhone(ctx, req.Phone); err != nil {
		return nil, err
	} else if registered {
		s.log.WarnContext(ctx, "onboarding submit rejected: phone already registered",
			slog.String("phone", req.Phone), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("ALREADY_REGISTERED", "a party already exists for this phone number")
	}
	if pending, err := s.repo.pendingRequestExistsByPhone(ctx, req.Phone); err != nil {
		return nil, err
	} else if pending {
		s.log.WarnContext(ctx, "onboarding submit rejected: request already pending",
			slog.String("phone", req.Phone), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("ALREADY_PENDING", "an onboarding request for this phone is already pending review")
	}

	now := time.Now().UTC()
	request := domain.OnboardingRequest{
		ID:               primitive.NewObjectID(),
		Phone:            req.Phone,
		FullName:         req.FullName,
		FullNameHi:       req.FullNameHi,
		RequestedRole:    req.RequestedRole,
		OrgUnitID:        req.OrgUnitID,
		RequestedTier:    req.RequestedTier,
		AlsoAssignSachiv: req.AlsoAssignSachiv,
		Note:             req.Note,
		DocumentRefs:     req.DocumentRefs,
		Village:          req.Village,
		AdminHierarchy:   req.AdminHierarchy,
		DocumentType:     req.DocumentType,
		DocumentNumber:   req.DocumentNumber,
		KYCPhotoURL:      req.KYCPhotoURL,
		ProfilePhotoURL:  req.ProfilePhotoURL,
		CattleCount:      req.CattleCount,
		CattleBreed:      req.CattleBreed,
		VehicleNumber:    req.VehicleNumber,
		EmployeeID:       req.EmployeeID,
		Status:           domain.OnboardingStatusPending,
		SubmittedBy:      aid,
		CreatedAt:        now,
		UpdatedAt:        now,
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
	// The granted role may demand a higher tier than the captured
	// requested_tier (domain.RequiredKYCTier — e.g. PLANT_OPERATOR needs HIGH).
	// The saga vouches the party up to that tier (Developer Note §2, mirroring
	// identity's vouch-on-grant), so authority and the tier write below are both
	// evaluated against the EFFECTIVE tier — otherwise approve would mint a role
	// its holder can never select (403 KYC_TIER_INSUFFICIENT).
	effectiveTier := request.RequestedTier
	if reqTier := domain.RequiredKYCTier[request.RequestedRole]; reqTier != "" && !domain.KYCTierSatisfies(effectiveTier, reqTier) {
		effectiveTier = reqTier
	}
	// The bundled Sachiv appointment (also_assign_sachiv) demands ITS tier too.
	if request.AlsoAssignSachiv {
		if reqTier := domain.RequiredKYCTier[domain.RoleSamitiSacheev]; reqTier != "" && !domain.KYCTierSatisfies(effectiveTier, reqTier) {
			effectiveTier = reqTier
		}
	}
	// Tier authority mirrors the KYC review console (§5.2): the reviewer's role
	// must be permitted to clear the tier this approval will vouch.
	if !domain.CanApproveKYCTier(actor.RoleCode, effectiveTier) {
		s.log.WarnContext(ctx, "onboarding approve rejected: tier not approvable",
			slog.String("request_id", id.Hex()), slog.String("reviewer_role", actor.RoleCode),
			slog.String("requested_tier", request.RequestedTier),
			slog.String("effective_tier", effectiveTier))
		appErr := httpx.Forbidden("your role may not approve onboarding at the " + effectiveTier + " tier")
		appErr.Code = "KYC_TIER_NOT_APPROVABLE"
		return nil, appErr
	}
	// Org-type + Sachiv-cap gates (§5): approval grants a role, so it must pass
	// the SAME governance guards as the identity grant path — the onboarding
	// queue can never be a side door around them. Checked before the atomic
	// claim so a rejected saga leaves the request PENDING for correction.
	targetOrg, err := s.deps.Orgs.Get(ctx, request.OrgUnitID)
	if err != nil {
		return nil, err
	}
	if !domain.RoleAllowedAtOrgType(request.RequestedRole, targetOrg.Type) {
		s.log.WarnContext(ctx, "onboarding approve rejected: role not allowed at org type",
			slog.String("request_id", id.Hex()),
			slog.String("requested_role", request.RequestedRole),
			slog.String("org_type", targetOrg.Type))
		allowed := strings.Join(domain.RoleAllowedOrgTypes[request.RequestedRole], ", ")
		return nil, httpx.BadRequest("INVALID_ORG_TYPE",
			request.RequestedRole+" may only be assigned at org type "+allowed+", not "+targetOrg.Type)
	}
	if request.AlsoAssignSachiv && !domain.RoleAllowedAtOrgType(domain.RoleSamitiSacheev, targetOrg.Type) {
		return nil, httpx.BadRequest("INVALID_ORG_TYPE",
			"SAMITI_SACHEEV may only be assigned at org type "+strings.Join(domain.RoleAllowedOrgTypes[domain.RoleSamitiSacheev], ", ")+", not "+targetOrg.Type)
	}
	// The Sachiv governance cap guards BOTH paths to the role: a direct Sachiv
	// request and an Adhyaksh request bundling the appointment.
	if request.RequestedRole == domain.RoleSamitiSacheev || request.AlsoAssignSachiv {
		capValue, err := s.repo.getIntSetting(ctx, sachivCapSettingKey, defaultSachivCap)
		if err != nil {
			return nil, err
		}
		appointed, err := s.repo.countActiveRoleHoldersInOrg(ctx, domain.RoleSamitiSacheev, request.OrgUnitID)
		if err != nil {
			return nil, err
		}
		if appointed >= capValue {
			s.log.WarnContext(ctx, "onboarding approve rejected: sachiv cap reached",
				slog.String("request_id", id.Hex()),
				slog.String("org_unit_id", request.OrgUnitID.Hex()),
				slog.Int("appointed", appointed), slog.Int("cap", capValue))
			return nil, httpx.Conflict("SACHIV_CAP_REACHED",
				fmt.Sprintf("this DCS already has %d of %d permitted Sachivs", appointed, capValue))
		}
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

	// 1b. Stamp the field-captured profile (photo URL, village, Hindi name)
	// onto the party — the profile must be visible in `parties` right after
	// verification, not stranded on the request document. Best-effort: the
	// request is already APPROVED (a hard failure here would strand it — the
	// retry hits the not-PENDING 409), and the KYC/role writes below matter
	// more than cosmetics, so log and continue.
	if err := s.repo.enrichPartyProfile(ctx, party.ID, claimed.FullNameHi, claimed.Village, claimed.ProfilePhotoURL, now); err != nil {
		s.log.ErrorContext(ctx, "onboarding approve: profile enrichment failed (continuing)",
			slog.String("request_id", id.Hex()), slog.String("party_id", party.ID.Hex()), slog.Any("err", err))
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

	// 3. Raise the party's KYC tier upward only — to the effective tier, so the
	// granted role is immediately activatable (vouch-on-grant, Developer Note §2).
	newTier := upgradedKYCTier(party.KYCTier, effectiveTier)
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

	// 4b. Bundled Sachiv appointment (also_assign_sachiv): grant SAMITI_SACHEEV
	// at the same samiti — cap + org-type were verified above, BEFORE the
	// atomic claim, so this can only fail on infrastructure errors. Idempotent
	// like the primary grant.
	if claimed.AlsoAssignSachiv {
		sachivExists, err := s.repo.activeAssignmentExists(ctx, party.ID, domain.RoleSamitiSacheev, claimed.OrgUnitID)
		if err != nil {
			return nil, err
		}
		if !sachivExists {
			sachivAssignment := domain.RoleAssignment{
				ID:        primitive.NewObjectID(),
				PartyID:   party.ID,
				RoleCode:  domain.RoleSamitiSacheev,
				OrgUnitID: claimed.OrgUnitID,
				GrantedBy: &rid,
				ValidFrom: now,
				Status:    domain.RoleAssignmentActive,
				CreatedAt: now,
			}
			if err := s.repo.insertAssignment(ctx, sachivAssignment); err != nil {
				// The primary Adhyaksh grant already committed (this saga is not
				// transactional — same as the single-role path). Make the partial
				// state LOUD, not silent: an explicit error log + an audit entry
				// so ops can complete the bundled appointment via People → grant.
				s.log.ErrorContext(ctx, "onboarding approve: bundled Sachiv grant failed after Adhyaksh grant — grant Sachiv manually",
					slog.String("request_id", id.Hex()), slog.String("party_id", party.ID.Hex()),
					slog.String("org_unit_id", claimed.OrgUnitID.Hex()), slog.Any("err", err))
				s.deps.Audit.Record(ctx, audit.Entry{
					Action: "onboarding.bundled_sachiv_failed", TargetType: "party", TargetID: party.ID.Hex(),
					Meta: map[string]any{"request_id": id.Hex(), "org_unit_id": claimed.OrgUnitID.Hex(),
						"note": "Adhyaksh granted; bundled Sachiv grant failed — appoint Sachiv manually"},
				})
				return nil, err
			}
			// Dedicated grant audit so the capped Sachiv appointment is
			// reconstructable from the log (who, when, which approval) —
			// mirroring the identity grant path's per-role audit.
			s.deps.Audit.Record(ctx, audit.Entry{
				Action: "role.grant", TargetType: "party", TargetID: party.ID.Hex(),
				Meta: map[string]any{"role_code": domain.RoleSamitiSacheev, "org_unit_id": claimed.OrgUnitID.Hex(),
					"assignment_id": sachivAssignment.ID.Hex(), "via": "onboarding.approve", "request_id": id.Hex()},
			})
		}
	}

	// 5. Stamp the created party onto the request.
	updated, err := s.repo.setRequestCreatedParty(ctx, id, party.ID, now)
	if err != nil {
		return nil, err
	}

	// 6. Notify the new party (best-effort — a notify failure never fails the
	// saga, which is already durably committed). Params carry human-readable
	// names (role + org name), never bare ids (onboarding.approved).
	language := party.PreferredLanguage
	if language == "" {
		language = defaultLanguage
	}
	orgName := claimed.OrgUnitID.Hex()
	if org, orgErr := s.deps.Orgs.Get(ctx, claimed.OrgUnitID); orgErr == nil && org != nil {
		orgName = org.Name
	}
	pid := party.ID
	if err := s.repo.insertNotification(ctx, domain.Notification{
		PartyID:     &pid,
		Phone:       party.Phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: domain.TemplateOnboardingApproved,
		Language:    language,
		Params: func() map[string]string {
			p := map[string]string{
				"role":     claimed.RequestedRole,
				"org_name": orgName,
				"tier":     claimed.RequestedTier,
			}
			// Tell the appointee they were ALSO made Sachiv (best-effort param;
			// templates that don't reference also_role simply ignore it).
			if claimed.AlsoAssignSachiv {
				p["also_role"] = domain.RoleSamitiSacheev
			}
			return p
		}(),
		Status:   domain.NotificationQueued,
		QueuedAt: now,
	}); err != nil {
		s.log.ErrorContext(ctx, "onboarding approve: party notify failed",
			slog.String("request_id", id.Hex()), slog.String("party_id", party.ID.Hex()), slog.Any("err", err))
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "onboarding.approve",
		TargetType: "onboarding_request",
		TargetID:   id.Hex(),
		Meta: map[string]any{
			"party_id":           party.ID.Hex(),
			"requested_role":     claimed.RequestedRole,
			"requested_tier":     claimed.RequestedTier,
			"new_tier":           newTier,
			"org_unit_id":        claimed.OrgUnitID.Hex(),
			"also_assign_sachiv": claimed.AlsoAssignSachiv,
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

	// Notify the applicant (onboarding.rejected): phone-addressed — no party
	// exists yet for a rejected request. Best-effort.
	if err := s.repo.insertNotification(ctx, domain.Notification{
		Phone:       updated.Phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: domain.TemplateOnboardingRejected,
		Language:    defaultLanguage,
		Params: map[string]string{
			"role":   updated.RequestedRole,
			"reason": reason,
		},
		Status:   domain.NotificationQueued,
		QueuedAt: now,
	}); err != nil {
		s.log.ErrorContext(ctx, "onboarding reject: applicant notify failed",
			slog.String("request_id", id.Hex()), slog.Any("err", err))
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

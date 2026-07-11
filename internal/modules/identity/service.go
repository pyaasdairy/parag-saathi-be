package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// Module-local constants. AADHAAR_EKYC is a consent purpose string, not a
// domain enum — the domain Consent struct deliberately leaves purposes open.
const (
	otpLength                 = 6
	maxOTPAttempts            = 5
	refreshHashLabel          = "refresh" // HMAC domain-separation label for refresh tokens
	consentPurposeAadhaarEKYC = "AADHAAR_EKYC"
	consentTextVersion        = "v1"
	defaultLanguage           = "hi"

	// msgAwaitingVerification is the human-readable status returned to a party
	// after submitting a KYC request — there is no auto-verify anymore.
	msgAwaitingVerification = "awaiting verification"

	// MOCK penny-drop output (§18-A): a real penny-drop adapter returns the
	// beneficiary-name match score from the bank; the mock always verifies
	// with this fixed score.
	mockPennyDropNameMatch = 0.92

	// sachivCapSettingKey is the app_settings key for the max-SAMITI_SACHEEV
	// per-DCS ceiling (owned by platformops; read here to enforce at grant).
	sachivCapSettingKey = "sachiv_cap"
	// defaultSachivCap is the ceiling applied until an admin sets one explicitly.
	defaultSachivCap = 2
)

// grantableRoles caps what each NON-admin granter role may hand out
// (blueprint §5.2). SUPER_ADMIN and PCDF_ADMIN are unrestricted admin
// granters and are handled in granterMayGrant.
var grantableRoles = map[string]map[string]bool{
	// A union president manages the village, logistics and union tiers.
	domain.RoleUnionPresident: {
		domain.RoleFarmer:               true,
		domain.RoleSamitiSacheev:        true,
		domain.RoleSamitiAdhyaksh:       true,
		domain.RoleMilkTester:           true,
		domain.RoleLRP:                  true,
		domain.RoleAITech:               true,
		domain.RoleVanRider:             true,
		domain.RoleDeliveryRider:        true,
		domain.RoleBMCOperator:          true,
		domain.RoleUnionFieldSupervisor: true,
		domain.RoleUnionPresident:       true,
	},
	// A samiti adhyaksh onboards only village workers inside their own DCS.
	domain.RoleSamitiAdhyaksh: {
		domain.RoleFarmer:     true,
		domain.RoleMilkTester: true,
		domain.RoleLRP:        true,
	},
	// An organising manager (ground-level field worker) enrols farmers and
	// consumers within their own org scope (enforced via RequireInScope).
	domain.RoleOrganisingManager: {
		domain.RoleFarmer:   true,
		domain.RoleConsumer: true,
	},
	// The app-coded onboarding executive has the same enrolment scope.
	domain.RoleOnboardingExecutive: {
		domain.RoleFarmer:   true,
		domain.RoleConsumer: true,
	},
}

// granterMayGrant reports whether granterRole may grant (or revoke)
// roleCode. Admin granters pass unconditionally.
func granterMayGrant(granterRole, roleCode string) bool {
	if granterRole == domain.RoleSuperAdmin || granterRole == domain.RolePCDFAdmin {
		return true
	}
	allowed, ok := grantableRoles[granterRole]
	return ok && allowed[roleCode]
}

// upgradedKYCTier returns the tier a party should hold after a successful
// verification at requestedTier — upgrades only, never a downgrade
// (KYCTierSatisfies treats parallel tier-1 proofs as equivalent).
func upgradedKYCTier(currentTier, requestedTier string) string {
	if domain.KYCTierSatisfies(currentTier, requestedTier) {
		return currentTier
	}
	return requestedTier
}

// maskAccount reduces a bank account number to its ****-prefixed last 4
// digits — the only form ever persisted.
func maskAccount(accountNumber string) string {
	if len(accountNumber) < 4 {
		return "****"
	}
	return "****" + accountNumber[len(accountNumber)-4:]
}

// isNotFound reports whether err is an httpx 404 (used to translate repo
// lookups into auth-appropriate 401s on the login path).
func isNotFound(err error) bool {
	var ae *httpx.AppError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// actorID parses the actor's party id (carried as an ObjectID hex string in
// the JWT) into a primitive.ObjectID — call once per service method that
// stores or compares the actor's identity.
func actorID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// service holds every business rule of the identity module.
type service struct {
	deps *deps.Deps
	repo *repository
	log  *slog.Logger
}

// newService wires the service. log is the module-scoped logger derived in
// Register.
func newService(d *deps.Deps, repo *repository, log *slog.Logger) *service {
	return &service{deps: d, repo: repo, log: log}
}

// --- Auth: OTP login ---

// requestOTP creates a hashed OTP challenge and queues the SMS through the
// notification outbox (the platformops worker delivers it).
func (s *service) requestOTP(ctx context.Context, phone string) (*otpRequestResponse, error) {
	now := time.Now().UTC()

	// REGISTRATION GATE (SMS-cost optimisation): only registered parties —
	// people an onboarding executive enrolled and an approver created — may
	// be sent an OTP. An unknown number costs an indexed lookup, never an
	// SMS. The response names the remedy so the caller isn't dead-ended.
	// (Runs behind the rate limiter, which also blunts phone enumeration.)
	registered, err := s.repo.findPartyByPhone(ctx, phone)
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	if registered == nil || isNotFound(err) {
		s.log.InfoContext(ctx, "otp refused: number not registered", slog.String("phone", phone))
		appErr := httpx.Unprocessable("NOT_REGISTERED",
			"this mobile number is not registered yet — contact your onboarding executive")
		return nil, appErr
	}

	code, err := auth.GenerateNumericOTP(otpLength)
	if err != nil {
		s.log.ErrorContext(ctx, "otp generation failed", slog.Any("err", err))
		return nil, httpx.Internal(err)
	}

	challenge := otpChallenge{
		ID:        uuid.NewString(),
		Phone:     phone,
		CodeHash:  auth.HMACHash(s.deps.Cfg.OTPHashSecret, phone, code),
		ExpiresAt: now.Add(s.deps.Cfg.OTPTTL),
		Attempts:  0,
	}
	if err := s.repo.insertOTPChallenge(ctx, challenge); err != nil {
		s.log.ErrorContext(ctx, "otp challenge insert failed", slog.String("phone", phone), slog.Any("err", err))
		return nil, err
	}

	// Outbox SMS: the plaintext code travels only inside the notification
	// params so the sender can deliver it; the challenge stores the hash.
	notification := domain.Notification{
		Phone:       phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: domain.TemplateOTP,
		Language:    defaultLanguage,
		Params:      map[string]string{"otp": code},
		Status:      domain.NotificationQueued,
		QueuedAt:    now,
	}
	// The registration gate above already loaded the party.
	pid := registered.ID
	notification.PartyID = &pid
	if registered.PreferredLanguage != "" {
		notification.Language = registered.PreferredLanguage
	}
	if err := s.repo.insertNotification(ctx, notification); err != nil {
		s.log.ErrorContext(ctx, "otp notification queue failed", slog.String("phone", phone), slog.Any("err", err))
		return nil, err
	}

	s.log.InfoContext(ctx, "otp requested", slog.String("phone", phone))
	resp := &otpRequestResponse{Phone: phone, ExpiresAt: challenge.ExpiresAt}
	if s.deps.Cfg.OTPDevMode {
		resp.DevOTP = code // dev only — config refuses OTP_DEV_MODE in prod
	}
	return resp, nil
}

// verifyOTP checks the challenge and logs the phone in, creating the Party
// on first login (find-or-create, §4.1).
func (s *service) verifyOTP(ctx context.Context, phone, otp string) (*authTokensResponse, error) {
	now := time.Now().UTC()

	challenge, err := s.repo.latestOTPChallenge(ctx, phone, now)
	if err != nil {
		if isNotFound(err) {
			s.log.WarnContext(ctx, "otp verify rejected: no active challenge", slog.String("phone", phone))
			appErr := httpx.Unauthorized("no active OTP for this phone — request a new code")
			appErr.Code = "OTP_NOT_FOUND" // distinct code so the app can localise
			return nil, appErr
		}
		return nil, err
	}
	if challenge.Attempts >= maxOTPAttempts {
		s.log.WarnContext(ctx, "otp verify rejected: too many attempts", slog.String("phone", phone))
		return nil, httpx.TooManyRequests("too many incorrect OTP attempts — request a new code")
	}
	expected := auth.HMACHash(s.deps.Cfg.OTPHashSecret, phone, otp)
	if !auth.ConstantTimeEqual(challenge.CodeHash, expected) {
		if err := s.repo.incrementOTPAttempts(ctx, challenge.ID); err != nil {
			return nil, err
		}
		s.log.WarnContext(ctx, "otp verify rejected: incorrect code", slog.String("phone", phone))
		appErr := httpx.Unauthorized("incorrect OTP")
		appErr.Code = "OTP_MISMATCH" // distinct code so the app can localise
		return nil, appErr
	}

	// Burn every outstanding challenge for this phone.
	if err := s.repo.deleteOTPChallenges(ctx, phone); err != nil {
		return nil, err
	}

	party, err := s.repo.upsertPartyByPhone(ctx, phone, now)
	if err != nil {
		return nil, err
	}
	if party.Status != domain.PartyStatusActive {
		s.log.WarnContext(ctx, "otp verify rejected: party suspended",
			slog.String("phone", phone), slog.String("party_id", party.ID.Hex()))
		return nil, httpx.Forbidden("party is suspended")
	}
	tokens, err := s.issueLoginTokens(ctx, party, now)
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "party logged in",
		slog.String("party_id", party.ID.Hex()), slog.String("phone", phone))
	return tokens, nil
}

// issueLoginTokens mints a SESSION access token plus a fresh rotating
// refresh token (stored only as an HMAC digest).
func (s *service) issueLoginTokens(ctx context.Context, party *domain.Party, now time.Time) (*authTokensResponse, error) {
	refreshToken, err := auth.RandomToken(32)
	if err != nil {
		s.log.ErrorContext(ctx, "refresh token generation failed", slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	doc := refreshTokenDoc{
		TokenHash: auth.HMACHash(s.deps.Cfg.OTPHashSecret, refreshHashLabel, refreshToken),
		PartyID:   party.ID,
		ExpiresAt: now.Add(s.deps.Cfg.RefreshTokenTTL),
		CreatedAt: now,
	}
	if err := s.repo.insertRefreshToken(ctx, doc); err != nil {
		return nil, err
	}
	accessToken, err := s.deps.JWT.IssueSessionToken(*party)
	if err != nil {
		s.log.ErrorContext(ctx, "session token issue failed",
			slog.String("party_id", party.ID.Hex()), slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	return &authTokensResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		Party:        party,
	}, nil
}

// refresh rotates a refresh token: the old token is consumed atomically and
// a new pair is issued — a replayed token always fails.
func (s *service) refresh(ctx context.Context, refreshToken string) (*authTokensResponse, error) {
	now := time.Now().UTC()
	tokenHash := auth.HMACHash(s.deps.Cfg.OTPHashSecret, refreshHashLabel, refreshToken)

	doc, err := s.repo.findRefreshToken(ctx, tokenHash)
	if err != nil {
		if isNotFound(err) {
			s.log.WarnContext(ctx, "refresh rejected: unknown token")
			return nil, httpx.Unauthorized("invalid refresh token")
		}
		return nil, err
	}
	if !now.Before(doc.ExpiresAt) {
		_, _ = s.repo.deleteRefreshToken(ctx, tokenHash)
		s.log.WarnContext(ctx, "refresh rejected: token expired", slog.String("party_id", doc.PartyID.Hex()))
		return nil, httpx.Unauthorized("refresh token expired — log in again")
	}

	// ROTATE: consume the old token first; DeletedCount==0 means a parallel
	// request won the race, so this presentation is treated as a replay.
	deleted, err := s.repo.deleteRefreshToken(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	if deleted == 0 {
		s.log.WarnContext(ctx, "refresh rejected: token already used", slog.String("party_id", doc.PartyID.Hex()))
		return nil, httpx.Unauthorized("refresh token already used")
	}

	party, err := s.repo.findPartyByID(ctx, doc.PartyID)
	if err != nil {
		if isNotFound(err) {
			s.log.WarnContext(ctx, "refresh rejected: party gone", slog.String("party_id", doc.PartyID.Hex()))
			return nil, httpx.Unauthorized("party no longer exists")
		}
		return nil, err
	}
	if party.Status != domain.PartyStatusActive {
		s.log.WarnContext(ctx, "refresh rejected: party suspended", slog.String("party_id", party.ID.Hex()))
		return nil, httpx.Forbidden("party is suspended")
	}
	return s.issueLoginTokens(ctx, party, now)
}

// logout revokes one refresh token belonging to the caller. Idempotent.
func (s *service) logout(ctx context.Context, actor auth.Actor, refreshToken string) error {
	aid, err := actorID(actor)
	if err != nil {
		return err
	}
	tokenHash := auth.HMACHash(s.deps.Cfg.OTPHashSecret, refreshHashLabel, refreshToken)
	return s.repo.deleteRefreshTokenForParty(ctx, tokenHash, aid)
}

// --- Auth: roles ---

// listMyRoles returns the actor's currently usable assignments, enriched
// with org-unit display fields for the role-switcher UI.
func (s *service) listMyRoles(ctx context.Context, actor auth.Actor) ([]assignmentWithOrg, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	assignments, err := s.repo.listActiveAssignments(ctx, aid)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]assignmentWithOrg, 0, len(assignments))
	for _, ra := range assignments {
		if !ra.UsableAt(now) {
			continue
		}
		out = append(out, s.withOrg(ctx, ra))
	}
	return out, nil
}

// withOrg copies org display fields onto an assignment; enrichment is
// best-effort (a missing org unit never breaks the listing).
func (s *service) withOrg(ctx context.Context, ra domain.RoleAssignment) assignmentWithOrg {
	enriched := assignmentWithOrg{RoleAssignment: ra}
	if org, err := s.deps.Orgs.Get(ctx, ra.OrgUnitID); err == nil {
		enriched.OrgName = org.Name
		enriched.OrgType = org.Type
		enriched.OrgCode = org.Code
	}
	return enriched
}

// selectRole exchanges the session for a ROLE-kind token pinned to one
// usable assignment, gated on the role's minimum KYC tier (§5.2). With
// auto-verify gone, this genuinely blocks un-approved parties.
func (s *service) selectRole(ctx context.Context, actor auth.Actor, assignmentID primitive.ObjectID) (*roleSelectResponse, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	assignment, err := s.repo.findAssignmentByID(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	if assignment.PartyID != aid {
		// Same shape as an unknown ID — never confirm other parties' grants.
		s.log.WarnContext(ctx, "role select rejected: not owner",
			slog.String("assignment_id", assignmentID.Hex()), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.NotFound("role assignment")
	}
	now := time.Now().UTC()
	if !assignment.UsableAt(now) {
		s.log.WarnContext(ctx, "role select rejected: assignment not usable",
			slog.String("assignment_id", assignmentID.Hex()), slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Forbidden("role assignment is revoked or outside its validity window")
	}

	// Re-read the party: the KYC tier may have upgraded since the session
	// token was minted.
	party, err := s.repo.findPartyByID(ctx, aid)
	if err != nil {
		return nil, err
	}
	if party.Status != domain.PartyStatusActive {
		s.log.WarnContext(ctx, "role select rejected: party suspended", slog.String("party_id", party.ID.Hex()))
		return nil, httpx.Forbidden("party is suspended")
	}
	requiredTier := domain.RequiredKYCTier[assignment.RoleCode]
	if !domain.KYCTierSatisfies(party.KYCTier, requiredTier) {
		s.log.WarnContext(ctx, "role select rejected: kyc tier insufficient",
			slog.String("party_id", party.ID.Hex()),
			slog.String("role_code", assignment.RoleCode),
			slog.String("current_tier", party.KYCTier),
			slog.String("required_tier", requiredTier))
		appErr := httpx.Forbidden("your KYC tier does not permit this role — complete verification first").
			WithDetails(map[string]string{
				"current_tier":  party.KYCTier,
				"required_tier": requiredTier,
			})
		appErr.Code = "KYC_TIER_INSUFFICIENT"
		return nil, appErr
	}

	org, err := s.deps.Orgs.Get(ctx, assignment.OrgUnitID)
	if err != nil {
		return nil, err
	}
	accessToken, err := s.deps.JWT.IssueRoleToken(*party, *assignment, org.Type)
	if err != nil {
		s.log.ErrorContext(ctx, "role token issue failed",
			slog.String("assignment_id", assignmentID.Hex()), slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	s.log.InfoContext(ctx, "role selected",
		slog.String("party_id", party.ID.Hex()),
		slog.String("assignment_id", assignmentID.Hex()),
		slog.String("role_code", assignment.RoleCode),
		slog.String("org_unit_id", assignment.OrgUnitID.Hex()))
	return &roleSelectResponse{
		AccessToken: accessToken,
		RoleCode:    assignment.RoleCode,
		OrgUnitID:   assignment.OrgUnitID,
		OrgType:     org.Type,
	}, nil
}

// --- Parties ---

// getMe aggregates the caller's party, usable assignments and latest KYC
// summary in one call.
func (s *service) getMe(ctx context.Context, actor auth.Actor) (*meResponse, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	party, err := s.repo.findPartyByID(ctx, aid)
	if err != nil {
		return nil, err
	}
	assignments, err := s.listMyRoles(ctx, actor)
	if err != nil {
		return nil, err
	}
	resp := &meResponse{Party: party, Assignments: assignments}
	if latest, err := s.repo.latestKYCRecord(ctx, aid); err == nil {
		resp.KYC = &kycSummary{
			RequestedTier:     latest.RequestedTier,
			Status:            latest.Status,
			AadhaarLast4:      latest.AadhaarLast4,
			BankAccountMasked: latest.BankAccountMasked,
			BankIFSC:          latest.BankIFSC,
			BankVerified:      latest.BankVerified,
			VerifiedAt:        latest.VerifiedAt,
		}
	} else if !isNotFound(err) {
		return nil, err
	}
	return resp, nil
}

// patchMe updates the caller's own profile fields.
func (s *service) patchMe(ctx context.Context, actor auth.Actor, req patchMeRequest) (*domain.Party, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	return s.repo.updatePartyProfile(ctx, aid, req.FullName, req.PreferredLanguage, req.PublicConsent, time.Now().UTC())
}

// listPartiesByRole returns the parties holding an ACTIVE assignment of
// roleCode inside orgUnitID (paged), each enriched with the org unit they hold
// it at — the reviewer-facing directory backing the FE listSachivs dropdown.
// The caller must be in scope for the org unit.
func (s *service) listPartiesByRole(ctx context.Context, actor auth.Actor, roleCode string, orgUnitID primitive.ObjectID, page httpx.Page) ([]partyWithRole, int64, error) {
	// Resolve the scope when the caller omits org_unit_id:
	//   • federation-wide roles (super admin / PCDF / mission / auditor) may
	//     list the role platform-wide (orgUnitID stays zero → no org filter);
	//   • everyone else is scoped to their OWN org token.
	if orgUnitID.IsZero() {
		switch actor.RoleCode {
		case domain.RoleSuperAdmin, domain.RolePCDFAdmin, domain.RoleMissionOfficial, domain.RoleStateAuditor:
			// federation-wide list; leave orgUnitID zero
		default:
			own, err := httpx.ParseID(actor.OrgUnitID, "actor org")
			if err != nil {
				return nil, 0, httpx.BadRequest("MISSING_ORG_UNIT", "org_unit_id is required for this role")
			}
			orgUnitID = own
		}
	}
	// A named org must be within the caller's scope. Match the org SUBTREE, not
	// the exact node: role holders sit at descendants (a union executive's
	// Sachivs are held at member DCS units, not at the union itself), so an exact
	// org_unit_id match would return an empty picker — RequireInScope already
	// authorises the descendants.
	var orgFilter []primitive.ObjectID // nil => federation-wide (no org filter)
	if !orgUnitID.IsZero() {
		if err := s.deps.Orgs.RequireInScope(ctx, actor, orgUnitID); err != nil {
			s.log.WarnContext(ctx, "list parties by role denied: out of scope",
				slog.String("actor_party_id", actor.PartyID),
				slog.String("org_unit_id", orgUnitID.Hex()),
				slog.String("role_code", roleCode))
			return nil, 0, err
		}
		ids, err := s.deps.Orgs.SubtreeIDs(ctx, orgUnitID)
		if err != nil {
			return nil, 0, err
		}
		orgFilter = ids
	}
	holders, total, err := s.repo.listRoleHoldersInOrg(ctx, roleCode, orgFilter, page)
	if err != nil {
		return nil, 0, err
	}
	// Enrich each holder with its org's display fields (name/code/village) so the
	// picker renders the society without a second call. Org lookups are cached and
	// best-effort — a missing org unit never breaks the listing.
	out := make([]partyWithRole, 0, len(holders))
	for _, h := range holders {
		row := partyWithRole{
			PartyID:          h.Party.ID.Hex(),
			FullName:         h.Party.FullName,
			Phone:            h.Party.Phone,
			KYCTier:          h.Party.KYCTier,
			OrgUnitID:        h.OrgUnitID.Hex(),
			RoleAssignmentID: h.AssignmentID.Hex(),
		}
		if org, oerr := s.deps.Orgs.Get(ctx, h.OrgUnitID); oerr == nil && org != nil {
			row.OrgName = org.Name
			row.OrgCode = org.Code
			row.Village = org.Village
		}
		out = append(out, row)
	}
	return out, total, nil
}

// --- KYC (mock DPI adapters — real UIDAI / penny-drop integration later) ---

// verifyAadhaar runs the MOCK Aadhaar eKYC capture (§18-A). The full Aadhaar
// number exists only in this request's memory: what is persisted is the
// last 4 digits plus an opaque reference from the (mock) Aadhaar Data Vault.
//
// There is NO auto-verify and NO tier change here: the record is created
// PENDING and enters the approval workflow. Returns created=false when an
// identical PENDING request already exists (idempotent replay → 200).
func (s *service) verifyAadhaar(ctx context.Context, actor auth.Actor, req aadhaarKYCRequest) (*aadhaarKYCResponse, bool, error) {
	now := time.Now().UTC()
	aid, err := actorID(actor)
	if err != nil {
		return nil, false, err
	}
	party, err := s.repo.findPartyByID(ctx, aid)
	if err != nil {
		return nil, false, err
	}

	// Idempotency: an existing PENDING request for the same tier is returned
	// as-is rather than duplicated.
	if existing, err := s.repo.findPendingKYCRecord(ctx, party.ID, req.RequestedTier); err == nil {
		s.log.WarnContext(ctx, "kyc submit replayed idempotently",
			slog.String("kyc_id", existing.ID.Hex()),
			slog.String("party_id", party.ID.Hex()),
			slog.String("requested_tier", req.RequestedTier))
		return &aadhaarKYCResponse{
			Record:  existing,
			Status:  domain.KYCStatusPending,
			Message: msgAwaitingVerification,
		}, false, nil
	} else if !isNotFound(err) {
		return nil, false, err
	}

	language := party.PreferredLanguage
	if language == "" {
		language = defaultLanguage
	}
	// Pre-generate the consent id so the KYC record can reference it in the
	// same flow.
	consentID := primitive.NewObjectID()
	consent := domain.Consent{
		ID:          consentID,
		PartyID:     party.ID,
		Purpose:     consentPurposeAadhaarEKYC,
		TextVersion: consentTextVersion,
		Language:    language,
		GrantedAt:   now,
	}
	if err := s.repo.insertConsent(ctx, consent); err != nil {
		return nil, false, err
	}

	// MOCK Aadhaar Data Vault (§18-A): a real deployment sends the number to
	// the vault service and receives an opaque token. The mock mints a UUID.
	vaultRef := uuid.NewString()
	last4 := req.AadhaarNumber[len(req.AadhaarNumber)-4:]

	record := domain.KYCRecord{
		ID:              primitive.NewObjectID(),
		PartyID:         party.ID,
		RequestedTier:   req.RequestedTier,
		AadhaarLast4:    last4,
		AadhaarVaultRef: vaultRef,
		ConsentID:       &consentID,
		Status:          domain.KYCStatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.repo.insertKYCRecord(ctx, record); err != nil {
		return nil, false, err
	}

	s.log.InfoContext(ctx, "kyc submitted",
		slog.String("kyc_id", record.ID.Hex()),
		slog.String("party_id", party.ID.Hex()),
		slog.String("requested_tier", req.RequestedTier))
	// Nudge live reviewer dashboards (ephemeral SSE badge)…
	s.publishKYCQueueChanged("submitted", record.ID, party.ID)
	// …AND queue a PERSISTED notification to the reviewers (super admin +
	// onboarding reviewers) so a pending verification reaches them even if no
	// dashboard is open — the durable half of "notification + verification
	// pending". Best-effort: a notify failure never fails the submission.
	s.notifyReviewersKYCPending(ctx, party, req.RequestedTier)
	return &aadhaarKYCResponse{
		Record:  &record,
		Status:  domain.KYCStatusPending,
		Message: msgAwaitingVerification,
	}, true, nil
}

// notifyReviewersKYCPending queues a KYC_PENDING notification to every party
// holding a reviewer role, so the pending verification durably reaches the
// super admin / onboarding reviewers (not just a live SSE badge).
func (s *service) notifyReviewersKYCPending(ctx context.Context, subject *domain.Party, tier string) {
	reviewers, err := s.repo.findPartiesByRoles(ctx, domain.OnboardingReviewerRoles, 200)
	if err != nil {
		s.log.ErrorContext(ctx, "kyc pending notify: reviewer lookup failed", slog.Any("err", err))
		return
	}
	now := time.Now().UTC()
	subjectName := subject.FullName
	if subjectName == "" {
		subjectName = subject.Phone
	}
	for _, rv := range reviewers {
		rvID := rv.ID
		lang := rv.PreferredLanguage
		if lang == "" {
			lang = defaultLanguage
		}
		if err := s.repo.insertNotification(ctx, domain.Notification{
			PartyID:     &rvID,
			Phone:       rv.Phone,
			Channel:     domain.ChannelSMS,
			TemplateKey: domain.TemplateKYCPending,
			Language:    lang,
			Params:      map[string]string{"subject": subjectName, "tier": tier},
			Status:      domain.NotificationQueued,
			QueuedAt:    now,
		}); err != nil {
			s.log.ErrorContext(ctx, "kyc pending notify: insert failed",
				slog.String("reviewer_party_id", rvID.Hex()), slog.Any("err", err))
		}
	}
	s.log.InfoContext(ctx, "kyc pending notifications queued",
		slog.String("subject_party_id", subject.ID.Hex()), slog.Int("reviewers", len(reviewers)))
}

// publishKYCQueueChanged nudges the live "pending KYC" dashboard badge via the
// event bus → SSE hub. It is a nudge only; subscribers re-query the scoped
// pending count. Never blocks the request path (the bus dispatches async).
func (s *service) publishKYCQueueChanged(reason string, kycID, subjectID primitive.ObjectID) {
	s.deps.Bus.Publish(eventbus.TopicKYCQueueChanged, eventbus.KYCQueueEvent{
		Reason:    reason,
		KYCID:     kycID.Hex(),
		SubjectID: subjectID.Hex(),
	})
}

// verifyBank runs the MOCK penny-drop verification and stores ONLY the
// masked account tail and IFSC on the caller's latest KYC record (creating a
// PENDING record carrying just the bank evidence when none exists yet).
func (s *service) verifyBank(ctx context.Context, actor auth.Actor, req bankKYCRequest) (*domain.KYCRecord, error) {
	now := time.Now().UTC()
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	party, err := s.repo.findPartyByID(ctx, aid)
	if err != nil {
		return nil, err
	}
	masked := maskAccount(req.AccountNumber)

	latest, err := s.repo.latestKYCRecord(ctx, aid)
	switch {
	case err == nil && latest.Status == domain.KYCStatusPending:
		// Only a PENDING record may accrue bank evidence in place — a reviewer
		// has not yet decided it. MOCK penny-drop: fixed name-match score.
		rec, err := s.repo.updateKYCRecordBank(ctx, latest.ID, masked, req.IFSC, mockPennyDropNameMatch, now)
		if err != nil {
			return nil, err
		}
		s.log.InfoContext(ctx, "bank evidence attached",
			slog.String("kyc_id", rec.ID.Hex()), slog.String("party_id", party.ID.Hex()))
		return rec, nil
	case err == nil, isNotFound(err):
		// The latest record is terminal (VERIFIED/REJECTED) or none exists yet:
		// a changed bank account must NOT silently overwrite reviewer-approved
		// evidence, so route it back through review as a fresh PENDING record.
		record := domain.KYCRecord{
			ID:                primitive.NewObjectID(),
			PartyID:           party.ID,
			RequestedTier:     party.KYCTier,
			BankAccountMasked: masked,
			BankIFSC:          req.IFSC,
			BankVerified:      true, // MOCK penny-drop: always succeeds
			BankNameMatch:     mockPennyDropNameMatch,
			Status:            domain.KYCStatusPending,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := s.repo.insertKYCRecord(ctx, record); err != nil {
			return nil, err
		}
		s.log.InfoContext(ctx, "bank evidence recorded on new pending kyc",
			slog.String("kyc_id", record.ID.Hex()), slog.String("party_id", party.ID.Hex()))
		return &record, nil
	default:
		return nil, err
	}
}

// listMyKYC pages the caller's KYC records (masked fields only — full
// numbers are never persisted anywhere).
func (s *service) listMyKYC(ctx context.Context, actor auth.Actor, page httpx.Page) ([]domain.KYCRecord, int64, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, 0, err
	}
	return s.repo.listKYCRecords(ctx, aid, page)
}

// --- KYC approval workflow (reviewer console) ---

// kycSubjectInScope reports whether the reviewer's org scope covers the KYC
// subject: true when the subject holds at least one active role assignment
// inside the reviewer's organisational scope (the KYC record itself carries
// no org, so the subject's org is resolved via their assignments — mirroring
// how createAssignment/revokeAssignment obtain the OrgUnitID). SUPER_ADMIN and
// STATE_AUDITOR are platform-wide and always cover; PCDF_ADMIN (rooted at the
// federation) covers naturally through RequireInScope's ancestry test.
func (s *service) kycSubjectInScope(ctx context.Context, actor auth.Actor, subjectPartyID primitive.ObjectID) (bool, error) {
	if actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor {
		return true, nil
	}
	assignments, err := s.repo.listActiveAssignments(ctx, subjectPartyID)
	if err != nil {
		return false, err
	}
	for _, ra := range assignments {
		if s.deps.Orgs.RequireInScope(ctx, actor, ra.OrgUnitID) == nil {
			return true, nil
		}
	}
	return false, nil
}

// requireKYCSubjectInScope denies the reviewer a KYC decision on a subject
// outside their organisational scope (a cross-org privilege escalation guard).
func (s *service) requireKYCSubjectInScope(ctx context.Context, actor auth.Actor, subjectPartyID primitive.ObjectID) error {
	inScope, err := s.kycSubjectInScope(ctx, actor, subjectPartyID)
	if err != nil {
		return err
	}
	if !inScope {
		s.log.WarnContext(ctx, "kyc review denied: subject out of scope",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("subject_party_id", subjectPartyID.Hex()))
		appErr := httpx.Forbidden("KYC subject is outside your organisational scope")
		appErr.Code = "KYC_SUBJECT_OUT_OF_SCOPE"
		return appErr
	}
	return nil
}

// listPendingKYC pages PENDING KYC records newest-first, constrained to
// subjects within the reviewer's org scope, enriched with a reviewer-facing
// party summary, and audits the access.
func (s *service) listPendingKYC(ctx context.Context, actor auth.Actor, page httpx.Page) ([]pendingKYCItem, int64, error) {
	records, total, err := s.repo.listPendingKYC(ctx, page)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]primitive.ObjectID, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.PartyID)
	}
	parties, err := s.repo.findPartiesByIDs(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	items := make([]pendingKYCItem, 0, len(records))
	for _, rec := range records {
		// Org-scope gate: a scoped reviewer only ever sees (and thus only ever
		// gets the contact PII of) subjects within their own area.
		inScope, err := s.kycSubjectInScope(ctx, actor, rec.PartyID)
		if err != nil {
			return nil, 0, err
		}
		if !inScope {
			continue
		}
		item := pendingKYCItem{KYCRecord: rec}
		if p, ok := parties[rec.PartyID]; ok {
			item.Party = &pendingPartySummary{
				ID:       p.ID,
				Phone:    p.Phone,
				FullName: p.FullName,
				KYCTier:  p.KYCTier,
			}
		}
		items = append(items, item)
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action: "kyc.pending_list",
		Meta:   map[string]any{"count": len(items), "total": total},
	})
	s.log.InfoContext(ctx, "kyc pending list accessed",
		slog.String("actor_party_id", actor.PartyID),
		slog.Int("count", len(items)),
		slog.Int64("total", total))
	return items, total, nil
}

// pendingKYCScanCap bounds the scoped-count scan. The pending queue is a
// human-drained work-list (realistically dozens, not millions); beyond the cap
// the badge shows "cap+" rather than paying for an unbounded scan.
const pendingKYCScanCap = 1000

// pendingKYCCount returns how many PENDING records fall within the reviewer's
// org scope — the live badge value. It reuses the exact scope logic of the
// pending list (so badge and list always agree) and dedupes the per-subject
// scope lookup. Returns (count, capped).
func (s *service) pendingKYCCount(ctx context.Context, actor auth.Actor) (int64, bool, error) {
	records, _, err := s.repo.listPendingKYC(ctx, httpx.Page{Limit: pendingKYCScanCap, Offset: 0})
	if err != nil {
		return 0, false, err
	}
	scopeCache := make(map[primitive.ObjectID]bool)
	var count int64
	for _, rec := range records {
		inScope, seen := scopeCache[rec.PartyID]
		if !seen {
			inScope, err = s.kycSubjectInScope(ctx, actor, rec.PartyID)
			if err != nil {
				return 0, false, err
			}
			scopeCache[rec.PartyID] = inScope
		}
		if inScope {
			count++
		}
	}
	capped := len(records) >= pendingKYCScanCap
	s.log.InfoContext(ctx, "kyc pending count",
		slog.String("actor_party_id", actor.PartyID), slog.Int64("count", count), slog.Bool("capped", capped))
	return count, capped, nil
}

// approveKYC verifies a PENDING record, upgrades the party's KYC tier upward
// only, notifies the party and audits the decision. The reviewer's role must
// be authorised for the requested tier — even SUPER_ADMIN passes through the
// approvable-tier map (which grants it everything).
func (s *service) approveKYC(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*kycReviewResponse, error) {
	now := time.Now().UTC()
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	record, err := s.repo.findKYCRecordByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if record.Status != domain.KYCStatusPending {
		s.log.WarnContext(ctx, "kyc approve rejected: record not pending",
			slog.String("kyc_id", id.Hex()), slog.String("status", record.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("KYC_NOT_PENDING", "kyc record is not pending review")
	}
	// Separation of duties: a reviewer may never decide their own KYC record.
	if record.PartyID == aid {
		s.log.WarnContext(ctx, "kyc approve rejected: self-review",
			slog.String("kyc_id", id.Hex()), slog.String("actor_party_id", actor.PartyID))
		appErr := httpx.Forbidden("you may not review your own KYC record")
		appErr.Code = "KYC_SELF_REVIEW"
		return nil, appErr
	}
	// Org scope: a reviewer may only decide KYC for subjects within their area.
	if err := s.requireKYCSubjectInScope(ctx, actor, record.PartyID); err != nil {
		return nil, err
	}
	if !domain.CanApproveKYCTier(actor.RoleCode, record.RequestedTier) {
		s.log.WarnContext(ctx, "kyc approve rejected: tier not approvable",
			slog.String("kyc_id", id.Hex()), slog.String("reviewer_role", actor.RoleCode),
			slog.String("requested_tier", record.RequestedTier))
		appErr := httpx.Forbidden("your role may not approve KYC at the " + record.RequestedTier + " tier")
		appErr.Code = "KYC_TIER_NOT_APPROVABLE"
		return nil, appErr
	}

	updated, err := s.repo.approveKYCRecord(ctx, id, aid, actor.RoleCode, now)
	if err != nil {
		if isNotFound(err) {
			// Lost the race — another reviewer already moved it out of PENDING.
			s.log.WarnContext(ctx, "kyc approve lost race: record no longer pending",
				slog.String("kyc_id", id.Hex()), slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Conflict("KYC_NOT_PENDING", "kyc record is not pending review")
		}
		return nil, err
	}

	party, err := s.repo.findPartyByID(ctx, updated.PartyID)
	if err != nil {
		return nil, err
	}
	newTier := upgradedKYCTier(party.KYCTier, updated.RequestedTier)
	if newTier != party.KYCTier {
		if err := s.repo.updatePartyKYCTier(ctx, party.ID, newTier, now); err != nil {
			return nil, err
		}
	}

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
		Params:      map[string]string{"tier": updated.RequestedTier},
		Status:      domain.NotificationQueued,
		QueuedAt:    now,
	}); err != nil {
		return nil, err
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "kyc.approve",
		TargetType: "kyc_record",
		TargetID:   id.Hex(),
		Meta: map[string]any{
			"party_id":       party.ID.Hex(),
			"requested_tier": updated.RequestedTier,
			"new_tier":       newTier,
		},
	})
	s.log.InfoContext(ctx, "kyc approved",
		slog.String("kyc_id", id.Hex()),
		slog.String("party_id", party.ID.Hex()),
		slog.String("requested_tier", updated.RequestedTier),
		slog.String("new_tier", newTier),
		slog.String("reviewer_role", actor.RoleCode),
		slog.String("actor_party_id", actor.PartyID))
	// Nudge reviewer dashboards: this record left the PENDING queue.
	s.publishKYCQueueChanged("approved", id, party.ID)
	return &kycReviewResponse{Record: updated, KYCTier: newTier}, nil
}

// rejectKYC moves a PENDING record to REJECTED with a mandatory reason,
// notifies the party and audits the decision. Same approvable-tier authority
// as approveKYC.
func (s *service) rejectKYC(ctx context.Context, actor auth.Actor, id primitive.ObjectID, reason string) (*kycReviewResponse, error) {
	now := time.Now().UTC()
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	record, err := s.repo.findKYCRecordByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if record.Status != domain.KYCStatusPending {
		s.log.WarnContext(ctx, "kyc reject rejected: record not pending",
			slog.String("kyc_id", id.Hex()), slog.String("status", record.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("KYC_NOT_PENDING", "kyc record is not pending review")
	}
	// Separation of duties: a reviewer may never decide their own KYC record.
	if record.PartyID == aid {
		s.log.WarnContext(ctx, "kyc reject rejected: self-review",
			slog.String("kyc_id", id.Hex()), slog.String("actor_party_id", actor.PartyID))
		appErr := httpx.Forbidden("you may not review your own KYC record")
		appErr.Code = "KYC_SELF_REVIEW"
		return nil, appErr
	}
	// Org scope: a reviewer may only decide KYC for subjects within their area.
	if err := s.requireKYCSubjectInScope(ctx, actor, record.PartyID); err != nil {
		return nil, err
	}
	if !domain.CanApproveKYCTier(actor.RoleCode, record.RequestedTier) {
		s.log.WarnContext(ctx, "kyc reject rejected: tier not approvable",
			slog.String("kyc_id", id.Hex()), slog.String("reviewer_role", actor.RoleCode),
			slog.String("requested_tier", record.RequestedTier))
		appErr := httpx.Forbidden("your role may not review KYC at the " + record.RequestedTier + " tier")
		appErr.Code = "KYC_TIER_NOT_APPROVABLE"
		return nil, appErr
	}

	updated, err := s.repo.rejectKYCRecord(ctx, id, aid, actor.RoleCode, reason, now)
	if err != nil {
		if isNotFound(err) {
			s.log.WarnContext(ctx, "kyc reject lost race: record no longer pending",
				slog.String("kyc_id", id.Hex()), slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Conflict("KYC_NOT_PENDING", "kyc record is not pending review")
		}
		return nil, err
	}

	party, err := s.repo.findPartyByID(ctx, updated.PartyID)
	if err != nil {
		return nil, err
	}
	language := party.PreferredLanguage
	if language == "" {
		language = defaultLanguage
	}
	pid := party.ID
	if err := s.repo.insertNotification(ctx, domain.Notification{
		PartyID:     &pid,
		Phone:       party.Phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: domain.TemplateKYCRejected,
		Language:    language,
		Params:      map[string]string{"tier": updated.RequestedTier, "reason": reason},
		Status:      domain.NotificationQueued,
		QueuedAt:    now,
	}); err != nil {
		return nil, err
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "kyc.reject",
		TargetType: "kyc_record",
		TargetID:   id.Hex(),
		Meta: map[string]any{
			"party_id":       party.ID.Hex(),
			"requested_tier": updated.RequestedTier,
			"reason":         reason,
		},
	})
	s.log.WarnContext(ctx, "kyc rejected",
		slog.String("kyc_id", id.Hex()),
		slog.String("party_id", party.ID.Hex()),
		slog.String("requested_tier", updated.RequestedTier),
		slog.String("reviewer_role", actor.RoleCode),
		slog.String("actor_party_id", actor.PartyID),
		slog.String("reason", reason))
	// Nudge reviewer dashboards: this record left the PENDING queue.
	s.publishKYCQueueChanged("rejected", id, party.ID)
	return &kycReviewResponse{Record: updated, KYCTier: party.KYCTier}, nil
}

// verifyPartyKYC is the admin direct-vouch path: an authorised reviewer raises
// a party to a KYC tier WITHOUT a self-submitted PENDING record (the
// counterpart to approve/reject, for staff who never self-submitted). It writes
// an append-only VERIFIED KYCRecord, lifts the party's tier UPWARD only, audits
// "kyc.tier_vouched" and returns the updated party.
func (s *service) verifyPartyKYC(ctx context.Context, actor auth.Actor, partyID primitive.ObjectID, tier, reason string) (*domain.Party, error) {
	now := time.Now().UTC()
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	// Authority: the reviewer's role must be permitted to set this tier.
	if !domain.CanApproveKYCTier(actor.RoleCode, tier) {
		s.log.WarnContext(ctx, "kyc vouch rejected: tier not approvable",
			slog.String("reviewer_role", actor.RoleCode), slog.String("tier", tier),
			slog.String("actor_party_id", actor.PartyID))
		appErr := httpx.Forbidden("your role may not verify KYC at the " + tier + " tier")
		appErr.Code = "KYC_TIER_NOT_APPROVABLE"
		return nil, appErr
	}
	party, err := s.repo.findPartyByID(ctx, partyID)
	if err != nil {
		return nil, err
	}
	// Separation of duties: a reviewer may never vouch their own tier.
	if party.ID == aid {
		s.log.WarnContext(ctx, "kyc vouch rejected: self-review",
			slog.String("party_id", party.ID.Hex()), slog.String("actor_party_id", actor.PartyID))
		appErr := httpx.Forbidden("you may not verify your own KYC")
		appErr.Code = "KYC_SELF_REVIEW"
		return nil, appErr
	}
	// Org scope: only vouch subjects within the reviewer's area.
	if err := s.requireKYCSubjectInScope(ctx, actor, party.ID); err != nil {
		return nil, err
	}

	reviewedAt := now
	reviewerID := aid
	record := domain.KYCRecord{
		ID:             primitive.NewObjectID(),
		PartyID:        party.ID,
		RequestedTier:  tier,
		Status:         domain.KYCStatusVerified,
		ReviewedBy:     &reviewerID,
		ReviewedByRole: actor.RoleCode,
		ReviewedAt:     &reviewedAt,
		VerifiedAt:     &reviewedAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.repo.insertKYCRecord(ctx, record); err != nil {
		return nil, err
	}

	newTier := upgradedKYCTier(party.KYCTier, tier)
	if newTier != party.KYCTier {
		if err := s.repo.updatePartyKYCTier(ctx, party.ID, newTier, now); err != nil {
			return nil, err
		}
		party.KYCTier = newTier
	}

	// Notify the vouched party (kyc.approved): the admin direct-vouch is a KYC
	// approval from the subject's perspective. Best-effort.
	vouchLanguage := party.PreferredLanguage
	if vouchLanguage == "" {
		vouchLanguage = defaultLanguage
	}
	vouchedID := party.ID
	if err := s.repo.insertNotification(ctx, domain.Notification{
		PartyID:     &vouchedID,
		Phone:       party.Phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: domain.TemplateKYCApproved,
		Language:    vouchLanguage,
		Params:      map[string]string{"tier": newTier},
		Status:      domain.NotificationQueued,
		QueuedAt:    now,
	}); err != nil {
		s.log.ErrorContext(ctx, "kyc vouch notify failed",
			slog.String("party_id", party.ID.Hex()), slog.Any("err", err))
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "kyc.tier_vouched",
		TargetType: "party",
		TargetID:   party.ID.Hex(),
		Meta: map[string]any{
			"tier":       newTier,
			"granted_by": actor.PartyID,
			"reason":     reason,
			"kyc_id":     record.ID.Hex(),
		},
	})
	s.log.InfoContext(ctx, "party KYC tier vouched by reviewer",
		slog.String("party_id", party.ID.Hex()),
		slog.String("tier", newTier),
		slog.String("reviewer_role", actor.RoleCode),
		slog.String("actor_party_id", actor.PartyID))
	// Nudge reviewer dashboards (this may clear a pending badge for the subject).
	s.publishKYCQueueChanged("vouched", record.ID, party.ID)
	return party, nil
}

// --- Role administration ---

// notifyRoleChange queues the ROLE_GRANTED / ROLE_REVOKED SMS to a party with
// human-readable role + org name params (never bare ids). Best-effort — a
// notify failure never voids the durable role change.
func (s *service) notifyRoleChange(ctx context.Context, party *domain.Party, templateKey, roleCode, orgName string, now time.Time) {
	language := party.PreferredLanguage
	if language == "" {
		language = defaultLanguage
	}
	pid := party.ID
	if err := s.repo.insertNotification(ctx, domain.Notification{
		PartyID:     &pid,
		Phone:       party.Phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: templateKey,
		Language:    language,
		Params: map[string]string{
			"role":     roleCode,
			"org_name": orgName,
		},
		Status:   domain.NotificationQueued,
		QueuedAt: now,
	}); err != nil {
		s.log.ErrorContext(ctx, "role change notify failed",
			slog.String("party_id", party.ID.Hex()),
			slog.String("template_key", templateKey), slog.Any("err", err))
	}
}

// vouchTierForRole lifts the party's KYC tier up to the role's required tier
// (Developer Note §2: the authorised grant IS the cooperative's verification
// for the appointment). No-op when the tier is already sufficient; tier only
// ever moves up, never down.
func (s *service) vouchTierForRole(ctx context.Context, actor auth.Actor, party *domain.Party, roleCode string, now time.Time) error {
	reqTier := domain.RequiredKYCTier[roleCode]
	if reqTier == "" || domain.KYCTierSatisfies(party.KYCTier, reqTier) {
		return nil
	}
	if err := s.repo.updatePartyKYCTier(ctx, party.ID, reqTier, now); err != nil {
		return err
	}
	party.KYCTier = reqTier
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "kyc.tier_vouched",
		TargetType: "party",
		TargetID:   party.ID.Hex(),
		Meta:       map[string]any{"tier": reqTier, "via_role": roleCode, "granted_by": actor.PartyID},
	})
	s.log.InfoContext(ctx, "party KYC tier vouched on role grant",
		slog.String("party_id", party.ID.Hex()),
		slog.String("tier", reqTier),
		slog.String("role_code", roleCode))
	return nil
}

// requireGrantAuthority enforces the non-admin granter matrix (§5.2):
// UNION_PRESIDENT manages village/logistics/union-tier roles anywhere in
// scope; SAMITI_ADHYAKSH manages only FARMER/MILK_TESTER/LRP inside their
// own DCS; ORGANISING_MANAGER enrols FARMER/CONSUMER within org scope.
// Admin granters pass.
func (s *service) requireGrantAuthority(actor auth.Actor, roleCode string, orgUnitID primitive.ObjectID) error {
	if !granterMayGrant(actor.RoleCode, roleCode) {
		return httpx.Forbidden("role " + actor.RoleCode + " may not grant or revoke " + roleCode)
	}
	if actor.RoleCode == domain.RoleSamitiAdhyaksh && orgUnitID.Hex() != actor.OrgUnitID {
		return httpx.Forbidden("SAMITI_ADHYAKSH may manage roles only within their own DCS")
	}
	return nil
}

// createAssignment grants a role to a party inside an org unit.
func (s *service) createAssignment(ctx context.Context, actor auth.Actor, req createAssignmentRequest) (*domain.RoleAssignment, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}

	// Resolve the target party (party_id wins over phone).
	var party *domain.Party
	if req.PartyID != nil {
		party, err = s.repo.findPartyByID(ctx, *req.PartyID)
	} else {
		party, err = s.repo.findPartyByPhone(ctx, req.Phone)
	}
	if err != nil {
		return nil, err
	}

	if err := s.deps.Orgs.RequireInScope(ctx, actor, req.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "assignment grant denied: out of scope",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("org_unit_id", req.OrgUnitID.Hex()),
			slog.String("role_code", req.RoleCode))
		return nil, err
	}
	if err := s.requireGrantAuthority(actor, req.RoleCode, req.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "assignment grant denied: granter not authorised",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("granter_role", actor.RoleCode),
			slog.String("role_code", req.RoleCode))
		return nil, err
	}

	// Org-type validation (§5.1): a role is only meaningful at certain tiers of
	// the hierarchy. Granting e.g. PLANT_OPERATOR@BMC or VAN_RIDER@DCS mints a
	// token that role-selects fine but lands on an empty/broken console, so the
	// mismatch is rejected here for every role centrally.
	targetOrg, err := s.deps.Orgs.Get(ctx, req.OrgUnitID)
	if err != nil {
		return nil, err
	}
	if !domain.RoleAllowedAtOrgType(req.RoleCode, targetOrg.Type) {
		s.log.WarnContext(ctx, "assignment grant rejected: role not allowed at org type",
			slog.String("role_code", req.RoleCode),
			slog.String("org_unit_id", req.OrgUnitID.Hex()),
			slog.String("org_type", targetOrg.Type))
		allowed := strings.Join(domain.RoleAllowedOrgTypes[req.RoleCode], ", ")
		return nil, httpx.BadRequest("INVALID_ORG_TYPE",
			req.RoleCode+" may only be assigned at org type "+allowed+", not "+targetOrg.Type)
	}

	exists, err := s.repo.activeAssignmentExists(ctx, party.ID, req.RoleCode, req.OrgUnitID)
	if err != nil {
		return nil, err
	}
	if exists {
		s.log.WarnContext(ctx, "assignment grant rejected: already exists",
			slog.String("party_id", party.ID.Hex()),
			slog.String("role_code", req.RoleCode),
			slog.String("org_unit_id", req.OrgUnitID.Hex()))
		return nil, httpx.Conflict("ASSIGNMENT_EXISTS", "party already holds an active "+req.RoleCode+" assignment in this org unit")
	}

	// Sachiv governance cap (§5.2): a DCS may appoint at most `sachiv_cap`
	// (setting, default 2) active SAMITI_SACHEEV. Enforced PER-DCS at grant so
	// the ceiling is real (not only checked when an admin lowers the knob).
	if req.RoleCode == domain.RoleSamitiSacheev {
		capValue, err := s.repo.getIntSetting(ctx, sachivCapSettingKey, defaultSachivCap)
		if err != nil {
			return nil, err
		}
		appointed, err := s.repo.countActiveRoleHoldersInOrg(ctx, domain.RoleSamitiSacheev, req.OrgUnitID)
		if err != nil {
			return nil, err
		}
		if appointed >= capValue {
			s.log.WarnContext(ctx, "assignment grant rejected: sachiv cap reached",
				slog.String("org_unit_id", req.OrgUnitID.Hex()),
				slog.Int("appointed", appointed), slog.Int("cap", capValue))
			return nil, httpx.Conflict("SACHIV_CAP_REACHED",
				fmt.Sprintf("this DCS already has %d of %d permitted Sachivs", appointed, capValue))
		}
	}

	now := time.Now().UTC()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = req.ValidFrom.UTC()
	}
	var validTo *time.Time
	if req.ValidTo != nil {
		t := req.ValidTo.UTC()
		validTo = &t
	}
	assignment := domain.RoleAssignment{
		ID:        primitive.NewObjectID(),
		PartyID:   party.ID,
		RoleCode:  req.RoleCode,
		OrgUnitID: req.OrgUnitID,
		GrantedBy: &aid,
		ValidFrom: validFrom,
		ValidTo:   validTo,
		Status:    domain.RoleAssignmentActive,
		CreatedAt: now,
	}
	if err := s.repo.insertAssignment(ctx, assignment); err != nil {
		return nil, err
	}
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "role.grant",
		TargetType: "role_assignment",
		TargetID:   assignment.ID.Hex(),
		Meta: map[string]any{
			"party_id":    party.ID.Hex(),
			"role_code":   req.RoleCode,
			"org_unit_id": req.OrgUnitID.Hex(),
		},
	})
	s.log.InfoContext(ctx, "role assignment created",
		slog.String("assignment_id", assignment.ID.Hex()),
		slog.String("party_id", party.ID.Hex()),
		slog.String("role_code", req.RoleCode),
		slog.String("org_unit_id", req.OrgUnitID.Hex()),
		slog.String("actor_party_id", actor.PartyID))

	// Notify the grantee (role.granted): human-readable role + org name, never
	// bare ids. Best-effort — a notify failure never voids the durable grant.
	s.notifyRoleChange(ctx, party, domain.TemplateRoleGranted, req.RoleCode, targetOrg.Name, now)

	// Developer Note §2: "a person receives the modules their role grants." The
	// authorised admin grant IS the cooperative's verification for this
	// appointment, so it vouches the party up to the tier the role requires —
	// otherwise a just-granted role would be un-activatable (role/select would
	// 403 KYC_TIER_INSUFFICIENT). Tier only ever moves up, never down.
	if err := s.vouchTierForRole(ctx, actor, party, req.RoleCode, now); err != nil {
		return nil, err
	}
	return &assignment, nil
}

// revokeAssignment flips an assignment to REVOKED — the record is never
// deleted (§4.1). Revocation authority mirrors grant authority.
func (s *service) revokeAssignment(ctx context.Context, actor auth.Actor, assignmentID primitive.ObjectID) (*domain.RoleAssignment, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	assignment, err := s.repo.findAssignmentByID(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, assignment.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "assignment revoke denied: out of scope",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("assignment_id", assignmentID.Hex()))
		return nil, err
	}
	if err := s.requireGrantAuthority(actor, assignment.RoleCode, assignment.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "assignment revoke denied: granter not authorised",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("granter_role", actor.RoleCode),
			slog.String("role_code", assignment.RoleCode))
		return nil, err
	}
	if assignment.Status != domain.RoleAssignmentActive {
		s.log.WarnContext(ctx, "assignment revoke rejected: already revoked",
			slog.String("assignment_id", assignmentID.Hex()))
		return nil, httpx.Conflict("ALREADY_REVOKED", "role assignment is already revoked")
	}
	revoked, err := s.repo.revokeAssignment(ctx, assignmentID, aid, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "role.revoke",
		TargetType: "role_assignment",
		TargetID:   assignmentID.Hex(),
		Meta: map[string]any{
			"party_id":    revoked.PartyID.Hex(),
			"role_code":   revoked.RoleCode,
			"org_unit_id": revoked.OrgUnitID.Hex(),
		},
	})
	s.log.InfoContext(ctx, "role assignment revoked",
		slog.String("assignment_id", assignmentID.Hex()),
		slog.String("party_id", revoked.PartyID.Hex()),
		slog.String("role_code", revoked.RoleCode),
		slog.String("actor_party_id", actor.PartyID))

	// Notify the former holder (role.revoked). Best-effort — a lookup or
	// notify failure never voids the durable revocation.
	if party, perr := s.repo.findPartyByID(ctx, revoked.PartyID); perr == nil {
		orgName := ""
		if org, oerr := s.deps.Orgs.Get(ctx, revoked.OrgUnitID); oerr == nil {
			orgName = org.Name
		}
		s.notifyRoleChange(ctx, party, domain.TemplateRoleRevoked, revoked.RoleCode, orgName, time.Now().UTC())
	} else {
		s.log.ErrorContext(ctx, "role revoke notify skipped: party lookup failed",
			slog.String("party_id", revoked.PartyID.Hex()), slog.Any("err", perr))
	}
	return revoked, nil
}

// transferAssignment moves an ACTIVE assignment's role to another org unit —
// the "move this van rider to another union / this sachiv to another DCS"
// admin action. Authority mirrors grant (requireGrantAuthority + org scope on
// BOTH ends); the target must pass the same org-type and sachiv-cap gates as a
// fresh grant. Executed as a best-effort saga: the NEW assignment is created
// first, then the old one is revoked — on create failure nothing changes; on
// revoke failure the just-created assignment is compensated away.
func (s *service) transferAssignment(ctx context.Context, actor auth.Actor, assignmentID, toOrgUnitID primitive.ObjectID) (*transferAssignmentResponse, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	assignment, err := s.repo.findAssignmentByID(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	if assignment.Status != domain.RoleAssignmentActive {
		s.log.WarnContext(ctx, "assignment transfer rejected: not active",
			slog.String("assignment_id", assignmentID.Hex()), slog.String("status", assignment.Status))
		return nil, httpx.Conflict("ASSIGNMENT_NOT_ACTIVE", "only an ACTIVE role assignment can be transferred")
	}
	if toOrgUnitID == assignment.OrgUnitID {
		return nil, httpx.BadRequest("SAME_ORG_UNIT", "the assignment already belongs to this org unit")
	}

	// Scope + granter authority on BOTH the source (being revoked) and the
	// target (being granted) org units — exactly the grant/revoke rules.
	if err := s.deps.Orgs.RequireInScope(ctx, actor, assignment.OrgUnitID); err != nil {
		s.log.WarnContext(ctx, "assignment transfer denied: source out of scope",
			slog.String("actor_party_id", actor.PartyID), slog.String("assignment_id", assignmentID.Hex()))
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, toOrgUnitID); err != nil {
		s.log.WarnContext(ctx, "assignment transfer denied: target out of scope",
			slog.String("actor_party_id", actor.PartyID), slog.String("to_org_unit_id", toOrgUnitID.Hex()))
		return nil, err
	}
	if err := s.requireGrantAuthority(actor, assignment.RoleCode, toOrgUnitID); err != nil {
		s.log.WarnContext(ctx, "assignment transfer denied: granter not authorised",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("granter_role", actor.RoleCode),
			slog.String("role_code", assignment.RoleCode))
		return nil, err
	}

	party, err := s.repo.findPartyByID(ctx, assignment.PartyID)
	if err != nil {
		return nil, err
	}
	sourceOrgName := ""
	if sourceOrg, oerr := s.deps.Orgs.Get(ctx, assignment.OrgUnitID); oerr == nil {
		sourceOrgName = sourceOrg.Name
	}

	// Org-type validation (§5.1): same central gate as a fresh grant.
	targetOrg, err := s.deps.Orgs.Get(ctx, toOrgUnitID)
	if err != nil {
		return nil, err
	}
	if !domain.RoleAllowedAtOrgType(assignment.RoleCode, targetOrg.Type) {
		allowed := strings.Join(domain.RoleAllowedOrgTypes[assignment.RoleCode], ", ")
		return nil, httpx.BadRequest("INVALID_ORG_TYPE",
			assignment.RoleCode+" may only be assigned at org type "+allowed+", not "+targetOrg.Type)
	}
	exists, err := s.repo.activeAssignmentExists(ctx, party.ID, assignment.RoleCode, toOrgUnitID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, httpx.Conflict("ASSIGNMENT_EXISTS", "party already holds an active "+assignment.RoleCode+" assignment in the target org unit")
	}
	// Sachiv governance cap (§5.2) applies at the TARGET DCS.
	if assignment.RoleCode == domain.RoleSamitiSacheev {
		capValue, err := s.repo.getIntSetting(ctx, sachivCapSettingKey, defaultSachivCap)
		if err != nil {
			return nil, err
		}
		appointed, err := s.repo.countActiveRoleHoldersInOrg(ctx, domain.RoleSamitiSacheev, toOrgUnitID)
		if err != nil {
			return nil, err
		}
		if appointed >= capValue {
			return nil, httpx.Conflict("SACHIV_CAP_REACHED",
				fmt.Sprintf("the target DCS already has %d of %d permitted Sachivs", appointed, capValue))
		}
	}

	now := time.Now().UTC()
	created := domain.RoleAssignment{
		ID:        primitive.NewObjectID(),
		PartyID:   party.ID,
		RoleCode:  assignment.RoleCode,
		OrgUnitID: toOrgUnitID,
		GrantedBy: &aid,
		ValidFrom: now,
		ValidTo:   assignment.ValidTo, // the transfer keeps the original validity ceiling
		Status:    domain.RoleAssignmentActive,
		CreatedAt: now,
	}
	// Saga step 1: create the NEW assignment first — on failure nothing changed.
	if err := s.repo.insertAssignment(ctx, created); err != nil {
		return nil, err
	}
	// Saga step 2: revoke the OLD assignment; on failure compensate by
	// revoking the just-created one so the transfer is all-or-nothing.
	revoked, err := s.repo.revokeAssignment(ctx, assignmentID, aid, now)
	if err != nil {
		if _, cerr := s.repo.revokeAssignment(ctx, created.ID, aid, now); cerr != nil {
			s.log.ErrorContext(ctx, "assignment transfer compensation failed — new assignment left active",
				slog.String("created_assignment_id", created.ID.Hex()), slog.Any("err", cerr))
		}
		return nil, err
	}

	// The grant vouches the tier (no-op when already sufficient — the party
	// held this very role, so this only matters after a manual downgrade).
	if err := s.vouchTierForRole(ctx, actor, party, assignment.RoleCode, now); err != nil {
		return nil, err
	}

	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "role.transfer",
		TargetType: "role_assignment",
		TargetID:   assignmentID.Hex(),
		Meta: map[string]any{
			"party_id":          party.ID.Hex(),
			"role_code":         assignment.RoleCode,
			"from_org_unit_id":  assignment.OrgUnitID.Hex(),
			"to_org_unit_id":    toOrgUnitID.Hex(),
			"new_assignment_id": created.ID.Hex(),
		},
	})
	s.log.InfoContext(ctx, "role assignment transferred",
		slog.String("assignment_id", assignmentID.Hex()),
		slog.String("new_assignment_id", created.ID.Hex()),
		slog.String("party_id", party.ID.Hex()),
		slog.String("role_code", assignment.RoleCode),
		slog.String("from_org_unit_id", assignment.OrgUnitID.Hex()),
		slog.String("to_org_unit_id", toOrgUnitID.Hex()),
		slog.String("actor_party_id", actor.PartyID))

	// Notify the party about both halves of the move. Best-effort.
	s.notifyRoleChange(ctx, party, domain.TemplateRoleGranted, created.RoleCode, targetOrg.Name, now)
	s.notifyRoleChange(ctx, party, domain.TemplateRoleRevoked, revoked.RoleCode, sourceOrgName, now)

	return &transferAssignmentResponse{Created: &created, Revoked: revoked}, nil
}

// replaceHolder swaps THE holder of a role at an org unit — "change the
// sachiv of this DCS": grants roleCode at orgUnitID to newPartyID and revokes
// every OTHER active holder of that role there. The sachiv cap is deliberately
// NOT checked (the swap is net-zero on headcount); org-type validation, org
// scope, granter authority and tier vouching all match a fresh grant.
func (s *service) replaceHolder(ctx context.Context, actor auth.Actor, orgUnitID primitive.ObjectID, roleCode string, newPartyID primitive.ObjectID) (*replaceHolderResponse, error) {
	aid, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, orgUnitID); err != nil {
		s.log.WarnContext(ctx, "replace holder denied: out of scope",
			slog.String("actor_party_id", actor.PartyID), slog.String("org_unit_id", orgUnitID.Hex()))
		return nil, err
	}
	if err := s.requireGrantAuthority(actor, roleCode, orgUnitID); err != nil {
		s.log.WarnContext(ctx, "replace holder denied: granter not authorised",
			slog.String("actor_party_id", actor.PartyID),
			slog.String("granter_role", actor.RoleCode),
			slog.String("role_code", roleCode))
		return nil, err
	}
	org, err := s.deps.Orgs.Get(ctx, orgUnitID)
	if err != nil {
		return nil, err
	}
	if !domain.RoleAllowedAtOrgType(roleCode, org.Type) {
		allowed := strings.Join(domain.RoleAllowedOrgTypes[roleCode], ", ")
		return nil, httpx.BadRequest("INVALID_ORG_TYPE",
			roleCode+" may only be assigned at org type "+allowed+", not "+org.Type)
	}
	newHolder, err := s.repo.findPartyByID(ctx, newPartyID)
	if err != nil {
		return nil, err
	}

	current, err := s.repo.listActiveAssignmentsForRole(ctx, roleCode, orgUnitID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	resp := &replaceHolderResponse{Revoked: []domain.RoleAssignment{}}

	// Grant to the incoming holder first (create-then-revoke saga, same as
	// transfer) unless they already hold the role here (idempotent replay).
	for _, ra := range current {
		if ra.PartyID == newPartyID {
			existing := ra
			resp.Assignment = &existing
			resp.AlreadyHolder = true
			break
		}
	}
	if resp.Assignment == nil {
		created := domain.RoleAssignment{
			ID:        primitive.NewObjectID(),
			PartyID:   newHolder.ID,
			RoleCode:  roleCode,
			OrgUnitID: orgUnitID,
			GrantedBy: &aid,
			ValidFrom: now,
			Status:    domain.RoleAssignmentActive,
			CreatedAt: now,
		}
		if err := s.repo.insertAssignment(ctx, created); err != nil {
			return nil, err
		}
		resp.Assignment = &created
	}

	// Revoke every OTHER active holder of this role at this org. Each
	// revocation notifies its former holder.
	for _, ra := range current {
		if ra.PartyID == newPartyID {
			continue
		}
		revoked, rerr := s.repo.revokeAssignment(ctx, ra.ID, aid, now)
		if rerr != nil {
			if isNotFound(rerr) {
				continue // a concurrent revoke won — already inactive
			}
			return nil, rerr
		}
		resp.Revoked = append(resp.Revoked, *revoked)
		if oldParty, perr := s.repo.findPartyByID(ctx, revoked.PartyID); perr == nil {
			s.notifyRoleChange(ctx, oldParty, domain.TemplateRoleRevoked, roleCode, org.Name, now)
		} else {
			s.log.ErrorContext(ctx, "replace holder revoke notify skipped: party lookup failed",
				slog.String("party_id", revoked.PartyID.Hex()), slog.Any("err", perr))
		}
	}

	// Vouch the incoming holder's tier so the new role is activatable.
	if err := s.vouchTierForRole(ctx, actor, newHolder, roleCode, now); err != nil {
		return nil, err
	}

	revokedIDs := make([]string, 0, len(resp.Revoked))
	for _, ra := range resp.Revoked {
		revokedIDs = append(revokedIDs, ra.ID.Hex())
	}
	s.deps.Audit.Record(ctx, audit.Entry{
		Action:     "role.replace_holder",
		TargetType: "org_unit",
		TargetID:   orgUnitID.Hex(),
		Meta: map[string]any{
			"role_code":              roleCode,
			"new_party_id":           newHolder.ID.Hex(),
			"new_assignment_id":      resp.Assignment.ID.Hex(),
			"already_holder":         resp.AlreadyHolder,
			"revoked_assignment_ids": revokedIDs,
		},
	})
	s.log.InfoContext(ctx, "role holder replaced",
		slog.String("org_unit_id", orgUnitID.Hex()),
		slog.String("role_code", roleCode),
		slog.String("new_party_id", newHolder.ID.Hex()),
		slog.Int("revoked", len(resp.Revoked)),
		slog.String("actor_party_id", actor.PartyID))

	// Notify the incoming holder (skip on an idempotent replay — they were
	// already told when first granted).
	if !resp.AlreadyHolder {
		s.notifyRoleChange(ctx, newHolder, domain.TemplateRoleGranted, roleCode, org.Name, now)
	}
	return resp, nil
}

// listAssignments pages assignments inside one org unit the actor can see.
func (s *service) listAssignments(ctx context.Context, actor auth.Actor, orgUnitID primitive.ObjectID, roleCode string, page httpx.Page) ([]domain.RoleAssignment, int64, error) {
	if err := s.deps.Orgs.RequireInScope(ctx, actor, orgUnitID); err != nil {
		return nil, 0, err
	}
	return s.repo.listAssignments(ctx, orgUnitID, roleCode, page)
}

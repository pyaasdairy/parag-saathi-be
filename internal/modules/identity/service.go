package identity

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
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

	// MOCK penny-drop output (§18-A): a real penny-drop adapter returns the
	// beneficiary-name match score from the bank; the mock always verifies
	// with this fixed score.
	mockPennyDropNameMatch = 0.92
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

// service holds every business rule of the identity module.
type service struct {
	deps *deps.Deps
	repo *repository
}

// newService wires the service.
func newService(d *deps.Deps, repo *repository) *service {
	return &service{deps: d, repo: repo}
}

// --- Auth: OTP login ---

// requestOTP creates a hashed OTP challenge and queues the SMS through the
// notification outbox (the platformops worker delivers it).
func (s *service) requestOTP(ctx context.Context, phone string) (*otpRequestResponse, error) {
	now := time.Now().UTC()
	code, err := auth.GenerateNumericOTP(otpLength)
	if err != nil {
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
		return nil, err
	}

	// Outbox SMS: the plaintext code travels only inside the notification
	// params so the sender can deliver it; the challenge stores the hash.
	notification := domain.Notification{
		ID:          uuid.NewString(),
		Phone:       phone,
		Channel:     domain.ChannelSMS,
		TemplateKey: domain.TemplateOTP,
		Language:    defaultLanguage,
		Params:      map[string]string{"otp": code},
		Status:      domain.NotificationQueued,
		QueuedAt:    now,
	}
	if party, err := s.repo.findPartyByPhone(ctx, phone); err == nil {
		notification.PartyID = party.ID
		if party.PreferredLanguage != "" {
			notification.Language = party.PreferredLanguage
		}
	}
	if err := s.repo.insertNotification(ctx, notification); err != nil {
		return nil, err
	}

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
			return nil, httpx.Unauthorized("no active OTP for this phone — request a new code")
		}
		return nil, err
	}
	if challenge.Attempts >= maxOTPAttempts {
		return nil, httpx.TooManyRequests("too many incorrect OTP attempts — request a new code")
	}
	expected := auth.HMACHash(s.deps.Cfg.OTPHashSecret, phone, otp)
	if !auth.ConstantTimeEqual(challenge.CodeHash, expected) {
		if err := s.repo.incrementOTPAttempts(ctx, challenge.ID); err != nil {
			return nil, err
		}
		return nil, httpx.Unauthorized("incorrect OTP")
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
		return nil, httpx.Forbidden("party is suspended")
	}
	return s.issueLoginTokens(ctx, party, now)
}

// issueLoginTokens mints a SESSION access token plus a fresh rotating
// refresh token (stored only as an HMAC digest).
func (s *service) issueLoginTokens(ctx context.Context, party *domain.Party, now time.Time) (*authTokensResponse, error) {
	refreshToken, err := auth.RandomToken(32)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	doc := refreshTokenDoc{
		ID:        uuid.NewString(),
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
			return nil, httpx.Unauthorized("invalid refresh token")
		}
		return nil, err
	}
	if !now.Before(doc.ExpiresAt) {
		_, _ = s.repo.deleteRefreshToken(ctx, tokenHash)
		return nil, httpx.Unauthorized("refresh token expired — log in again")
	}

	// ROTATE: consume the old token first; DeletedCount==0 means a parallel
	// request won the race, so this presentation is treated as a replay.
	deleted, err := s.repo.deleteRefreshToken(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	if deleted == 0 {
		return nil, httpx.Unauthorized("refresh token already used")
	}

	party, err := s.repo.findPartyByID(ctx, doc.PartyID)
	if err != nil {
		if isNotFound(err) {
			return nil, httpx.Unauthorized("party no longer exists")
		}
		return nil, err
	}
	if party.Status != domain.PartyStatusActive {
		return nil, httpx.Forbidden("party is suspended")
	}
	return s.issueLoginTokens(ctx, party, now)
}

// logout revokes one refresh token belonging to the caller. Idempotent.
func (s *service) logout(ctx context.Context, actor auth.Actor, refreshToken string) error {
	tokenHash := auth.HMACHash(s.deps.Cfg.OTPHashSecret, refreshHashLabel, refreshToken)
	return s.repo.deleteRefreshTokenForParty(ctx, tokenHash, actor.PartyID)
}

// --- Auth: roles ---

// listMyRoles returns the actor's currently usable assignments, enriched
// with org-unit display fields for the role-switcher UI.
func (s *service) listMyRoles(ctx context.Context, actor auth.Actor) ([]assignmentWithOrg, error) {
	assignments, err := s.repo.listActiveAssignments(ctx, actor.PartyID)
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
// usable assignment, gated on the role's minimum KYC tier (§5.2).
func (s *service) selectRole(ctx context.Context, actor auth.Actor, assignmentID string) (*roleSelectResponse, error) {
	assignment, err := s.repo.findAssignmentByID(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	if assignment.PartyID != actor.PartyID {
		// Same shape as an unknown ID — never confirm other parties' grants.
		return nil, httpx.NotFound("role assignment")
	}
	now := time.Now().UTC()
	if !assignment.UsableAt(now) {
		return nil, httpx.Forbidden("role assignment is revoked or outside its validity window")
	}

	// Re-read the party: the KYC tier may have upgraded since the session
	// token was minted.
	party, err := s.repo.findPartyByID(ctx, actor.PartyID)
	if err != nil {
		return nil, err
	}
	if party.Status != domain.PartyStatusActive {
		return nil, httpx.Forbidden("party is suspended")
	}
	requiredTier := domain.RequiredKYCTier[assignment.RoleCode]
	if !domain.KYCTierSatisfies(party.KYCTier, requiredTier) {
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
		return nil, httpx.Internal(err)
	}
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
	party, err := s.repo.findPartyByID(ctx, actor.PartyID)
	if err != nil {
		return nil, err
	}
	assignments, err := s.listMyRoles(ctx, actor)
	if err != nil {
		return nil, err
	}
	resp := &meResponse{Party: party, Assignments: assignments}
	if latest, err := s.repo.latestKYCRecord(ctx, actor.PartyID); err == nil {
		resp.KYC = &kycSummary{
			Tier:              latest.Tier,
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
	return s.repo.updatePartyProfile(ctx, actor.PartyID, req.FullName, req.PreferredLanguage, time.Now().UTC())
}

// --- KYC (mock DPI adapters — real UIDAI / penny-drop integration later) ---

// verifyAadhaar runs the MOCK Aadhaar eKYC flow (§18-A). The full Aadhaar
// number exists only in this request's memory: what is persisted is the
// last 4 digits plus an opaque reference from the (mock) Aadhaar Data Vault.
func (s *service) verifyAadhaar(ctx context.Context, actor auth.Actor, req aadhaarKYCRequest) (*aadhaarKYCResponse, error) {
	now := time.Now().UTC()
	party, err := s.repo.findPartyByID(ctx, actor.PartyID)
	if err != nil {
		return nil, err
	}

	language := party.PreferredLanguage
	if language == "" {
		language = defaultLanguage
	}
	consent := domain.Consent{
		ID:          uuid.NewString(),
		PartyID:     party.ID,
		Purpose:     consentPurposeAadhaarEKYC,
		TextVersion: consentTextVersion,
		Language:    language,
		GrantedAt:   now,
	}
	if err := s.repo.insertConsent(ctx, consent); err != nil {
		return nil, err
	}

	// MOCK Aadhaar Data Vault (§18-A): a real deployment sends the number to
	// the vault service and receives an opaque token. The mock mints a UUID.
	vaultRef := uuid.NewString()
	last4 := req.AadhaarNumber[len(req.AadhaarNumber)-4:]

	// MOCK UIDAI eKYC: always succeeds. The real adapter drives the OTP-based
	// eKYC exchange and may return PENDING/REJECTED.
	record := domain.KYCRecord{
		ID:              uuid.NewString(),
		PartyID:         party.ID,
		Tier:            req.RequestedTier,
		AadhaarLast4:    last4,
		AadhaarVaultRef: vaultRef,
		ConsentID:       consent.ID,
		Status:          domain.KYCStatusVerified,
		VerifiedAt:      &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.repo.insertKYCRecord(ctx, record); err != nil {
		return nil, err
	}

	newTier := upgradedKYCTier(party.KYCTier, req.RequestedTier)
	if newTier != party.KYCTier {
		if err := s.repo.updatePartyKYCTier(ctx, party.ID, newTier, now); err != nil {
			return nil, err
		}
	}
	return &aadhaarKYCResponse{Record: &record, KYCTier: newTier}, nil
}

// verifyBank runs the MOCK penny-drop verification and stores ONLY the
// masked account tail and IFSC on the caller's latest KYC record (creating
// one when none exists yet).
func (s *service) verifyBank(ctx context.Context, actor auth.Actor, req bankKYCRequest) (*domain.KYCRecord, error) {
	now := time.Now().UTC()
	party, err := s.repo.findPartyByID(ctx, actor.PartyID)
	if err != nil {
		return nil, err
	}
	masked := maskAccount(req.AccountNumber)

	latest, err := s.repo.latestKYCRecord(ctx, actor.PartyID)
	switch {
	case err == nil:
		// MOCK penny-drop: always verified with a fixed name-match score.
		return s.repo.updateKYCRecordBank(ctx, latest.ID, masked, req.IFSC, mockPennyDropNameMatch, now)
	case isNotFound(err):
		record := domain.KYCRecord{
			ID:                uuid.NewString(),
			PartyID:           party.ID,
			Tier:              party.KYCTier,
			BankAccountMasked: masked,
			BankIFSC:          req.IFSC,
			BankVerified:      true, // MOCK penny-drop: always succeeds
			BankNameMatch:     mockPennyDropNameMatch,
			Status:            domain.KYCStatusVerified,
			VerifiedAt:        &now,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := s.repo.insertKYCRecord(ctx, record); err != nil {
			return nil, err
		}
		return &record, nil
	default:
		return nil, err
	}
}

// listMyKYC pages the caller's KYC records (masked fields only — full
// numbers are never persisted anywhere).
func (s *service) listMyKYC(ctx context.Context, actor auth.Actor, page httpx.Page) ([]domain.KYCRecord, int64, error) {
	return s.repo.listKYCRecords(ctx, actor.PartyID, page)
}

// --- Role administration ---

// requireGrantAuthority enforces the non-admin granter matrix (§5.2):
// UNION_PRESIDENT manages village/logistics/union-tier roles anywhere in
// scope; SAMITI_ADHYAKSH manages only FARMER/MILK_TESTER/LRP inside their
// own DCS. Admin granters pass.
func (s *service) requireGrantAuthority(actor auth.Actor, roleCode, orgUnitID string) error {
	if !granterMayGrant(actor.RoleCode, roleCode) {
		return httpx.Forbidden("role " + actor.RoleCode + " may not grant or revoke " + roleCode)
	}
	if actor.RoleCode == domain.RoleSamitiAdhyaksh && orgUnitID != actor.OrgUnitID {
		return httpx.Forbidden("SAMITI_ADHYAKSH may manage roles only within their own DCS")
	}
	return nil
}

// createAssignment grants a role to a party inside an org unit.
func (s *service) createAssignment(ctx context.Context, actor auth.Actor, req createAssignmentRequest) (*domain.RoleAssignment, error) {
	// Resolve the target party (party_id wins over phone).
	var party *domain.Party
	var err error
	if req.PartyID != "" {
		party, err = s.repo.findPartyByID(ctx, req.PartyID)
	} else {
		party, err = s.repo.findPartyByPhone(ctx, req.Phone)
	}
	if err != nil {
		return nil, err
	}

	if err := s.deps.Orgs.RequireInScope(ctx, actor, req.OrgUnitID); err != nil {
		return nil, err
	}
	if err := s.requireGrantAuthority(actor, req.RoleCode, req.OrgUnitID); err != nil {
		return nil, err
	}

	exists, err := s.repo.activeAssignmentExists(ctx, party.ID, req.RoleCode, req.OrgUnitID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, httpx.Conflict("ASSIGNMENT_EXISTS", "party already holds an active "+req.RoleCode+" assignment in this org unit")
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
		ID:        uuid.NewString(),
		PartyID:   party.ID,
		RoleCode:  req.RoleCode,
		OrgUnitID: req.OrgUnitID,
		GrantedBy: actor.PartyID,
		ValidFrom: validFrom,
		ValidTo:   validTo,
		Status:    domain.RoleAssignmentActive,
		CreatedAt: now,
	}
	if err := s.repo.insertAssignment(ctx, assignment); err != nil {
		return nil, err
	}
	return &assignment, nil
}

// revokeAssignment flips an assignment to REVOKED — the record is never
// deleted (§4.1). Revocation authority mirrors grant authority.
func (s *service) revokeAssignment(ctx context.Context, actor auth.Actor, assignmentID string) (*domain.RoleAssignment, error) {
	assignment, err := s.repo.findAssignmentByID(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, assignment.OrgUnitID); err != nil {
		return nil, err
	}
	if err := s.requireGrantAuthority(actor, assignment.RoleCode, assignment.OrgUnitID); err != nil {
		return nil, err
	}
	if assignment.Status != domain.RoleAssignmentActive {
		return nil, httpx.Conflict("ALREADY_REVOKED", "role assignment is already revoked")
	}
	return s.repo.revokeAssignment(ctx, assignmentID, actor.PartyID, time.Now().UTC())
}

// listAssignments pages assignments inside one org unit the actor can see.
func (s *service) listAssignments(ctx context.Context, actor auth.Actor, orgUnitID, roleCode string, page httpx.Page) ([]domain.RoleAssignment, int64, error) {
	if err := s.deps.Orgs.RequireInScope(ctx, actor, orgUnitID); err != nil {
		return nil, 0, err
	}
	return s.repo.listAssignments(ctx, orgUnitID, roleCode, page)
}

package identity

import (
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// Request-shape validation patterns. Formats only — business rules live in
// the service layer.
var (
	phoneRegexp       = regexp.MustCompile(`^[6-9][0-9]{9}$`)        // 10-digit Indian mobile
	otpRegexp         = regexp.MustCompile(`^[0-9]{6}$`)             // 6-digit numeric OTP
	aadhaarRegexp     = regexp.MustCompile(`^[0-9]{12}$`)            // 12-digit Aadhaar number
	bankAccountRegexp = regexp.MustCompile(`^[0-9]{9,18}$`)          // NPCI-range account number
	ifscRegexp        = regexp.MustCompile(`^[A-Z]{4}0[A-Z0-9]{6}$`) // RBI IFSC format
)

// requestableTiers are the KYC tiers a party may ask to be verified at via
// POST /kyc/aadhaar. HIGHEST/SERVICE are never self-requested.
var requestableTiers = map[string]bool{
	domain.KYCTierFarmer:   true,
	domain.KYCTierStandard: true,
	domain.KYCTierRider:    true,
	domain.KYCTierHigh:     true,
}

// otpRequestRequest asks for a login OTP on a phone number.
type otpRequestRequest struct {
	Phone string `json:"phone"`
}

func (r otpRequestRequest) validate() error {
	if !phoneRegexp.MatchString(r.Phone) {
		return httpx.BadRequest("INVALID_PHONE", "phone must be a 10-digit Indian mobile number")
	}
	return nil
}

// otpRequestResponse acknowledges the queued OTP. DevOTP is populated ONLY
// when cfg.OTPDevMode is on (never in prod — config refuses to boot).
type otpRequestResponse struct {
	Phone     string    `json:"phone"`
	ExpiresAt time.Time `json:"expires_at"`
	DevOTP    string    `json:"dev_otp,omitempty"`
}

// otpVerifyRequest exchanges phone+OTP for tokens.
type otpVerifyRequest struct {
	Phone string `json:"phone"`
	OTP   string `json:"otp"`
}

func (r otpVerifyRequest) validate() error {
	if !phoneRegexp.MatchString(r.Phone) {
		return httpx.BadRequest("INVALID_PHONE", "phone must be a 10-digit Indian mobile number")
	}
	if !otpRegexp.MatchString(r.OTP) {
		return httpx.BadRequest("INVALID_OTP", "otp must be a 6-digit numeric code")
	}
	return nil
}

// authTokensResponse is returned by login and refresh: a SESSION-kind access
// token, an opaque rotating refresh token, and the party record.
type authTokensResponse struct {
	AccessToken  string        `json:"access_token"`
	RefreshToken string        `json:"refresh_token"`
	Party        *domain.Party `json:"party"`
}

// refreshRequest rotates a refresh token.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (r refreshRequest) validate() error {
	if strings.TrimSpace(r.RefreshToken) == "" {
		return httpx.BadRequest("MISSING_REFRESH_TOKEN", "refresh_token is required")
	}
	return nil
}

// logoutRequest revokes a refresh token.
type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (r logoutRequest) validate() error {
	if strings.TrimSpace(r.RefreshToken) == "" {
		return httpx.BadRequest("MISSING_REFRESH_TOKEN", "refresh_token is required")
	}
	return nil
}

// assignmentWithOrg is a role assignment enriched with the org unit's
// display fields so the role-switcher UI needs no second call.
type assignmentWithOrg struct {
	domain.RoleAssignment
	OrgName string `json:"org_name,omitempty"`
	OrgType string `json:"org_type,omitempty"`
	OrgCode string `json:"org_code,omitempty"`
}

// partyWithRole is one party holding a queried role, enriched with the org unit
// they hold it at — the shape the FE listSachivs picker consumes
// ({party_id, full_name, org_unit_id, org_name, village}). name_hi is not held
// on the party record today, so it is omitted.
type partyWithRole struct {
	PartyID          string `json:"party_id"`
	FullName         string `json:"full_name,omitempty"`
	Phone            string `json:"phone,omitempty"`
	KYCTier          string `json:"kyc_tier,omitempty"`
	OrgUnitID        string `json:"org_unit_id"`
	OrgName          string `json:"org_name,omitempty"`
	OrgCode          string `json:"org_code,omitempty"`
	Village          string `json:"village,omitempty"`
	RoleAssignmentID string `json:"role_assignment_id,omitempty"`
}

// roleSelectRequest picks one active assignment to operate under. The ID
// arrives as an ObjectID hex string and unmarshals natively.
type roleSelectRequest struct {
	RoleAssignmentID primitive.ObjectID `json:"role_assignment_id"`
}

func (r roleSelectRequest) validate() error {
	if r.RoleAssignmentID.IsZero() {
		return httpx.BadRequest("MISSING_ROLE_ASSIGNMENT_ID", "role_assignment_id is required")
	}
	return nil
}

// roleSelectResponse carries the fresh ROLE-kind access token.
type roleSelectResponse struct {
	AccessToken string             `json:"access_token"`
	RoleCode    string             `json:"role_code"`
	OrgUnitID   primitive.ObjectID `json:"org_unit_id"`
	OrgType     string             `json:"org_type"`
}

// kycSummary is the masked, client-safe digest of the latest KYC record.
type kycSummary struct {
	RequestedTier     string     `json:"requested_tier"`
	Status            string     `json:"status"`
	AadhaarLast4      string     `json:"aadhaar_last4,omitempty"`
	BankAccountMasked string     `json:"bank_account_masked,omitempty"`
	BankIFSC          string     `json:"bank_ifsc,omitempty"`
	BankVerified      bool       `json:"bank_verified"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`
}

// meResponse is the "who am I" aggregate: party, usable assignments, latest
// KYC summary.
type meResponse struct {
	Party       *domain.Party       `json:"party"`
	Assignments []assignmentWithOrg `json:"assignments"`
	KYC         *kycSummary         `json:"kyc,omitempty"`
}

// patchMeRequest updates the caller's own profile fields.
type patchMeRequest struct {
	FullName          *string `json:"full_name,omitempty"`
	PreferredLanguage *string `json:"preferred_language,omitempty"`
	// PublicConsent toggles the §6.7 opt-in to be named on the public QR trace.
	PublicConsent *bool `json:"public_consent,omitempty"`
	// ProfilePhotoURL points at the PRIVATE media seam (an authenticated
	// /api/v1/uploads/view/… path from the uploads module) — never a public
	// bucket URL. Empty string clears the photo.
	ProfilePhotoURL *string `json:"profile_photo_url,omitempty"`
}

func (r patchMeRequest) validate() error {
	if r.FullName == nil && r.PreferredLanguage == nil && r.PublicConsent == nil && r.ProfilePhotoURL == nil {
		return httpx.BadRequest("EMPTY_PATCH", "provide at least one of full_name, preferred_language, public_consent, profile_photo_url")
	}
	if r.ProfilePhotoURL != nil {
		u := strings.TrimSpace(*r.ProfilePhotoURL)
		if u != "" && (!strings.HasPrefix(u, "/api/v1/uploads/view/") || len(u) > 512 || strings.Contains(u, "..")) {
			return httpx.BadRequest("INVALID_PHOTO_URL", "profile_photo_url must be an /api/v1/uploads/view/… path")
		}
	}
	if r.FullName != nil {
		name := strings.TrimSpace(*r.FullName)
		if name == "" || len(name) > 120 {
			return httpx.BadRequest("INVALID_FULL_NAME", "full_name must be 1-120 characters")
		}
	}
	if r.PreferredLanguage != nil {
		lang := strings.TrimSpace(*r.PreferredLanguage)
		if lang == "" || len(lang) > 16 {
			return httpx.BadRequest("INVALID_LANGUAGE", "preferred_language must be 1-16 characters (e.g. \"hi\", \"en\")")
		}
	}
	return nil
}

// aadhaarKYCRequest submits an Aadhaar KYC request at a requested tier. The
// full number is used transiently and NEVER stored (blueprint §18-A). The
// record enters the approval workflow as PENDING — there is no auto-verify.
type aadhaarKYCRequest struct {
	AadhaarNumber string `json:"aadhaar_number"`
	Consent       bool   `json:"consent"`
	RequestedTier string `json:"requested_tier"` // FARMER | STANDARD | RIDER | HIGH
}

func (r aadhaarKYCRequest) validate() error {
	if !aadhaarRegexp.MatchString(r.AadhaarNumber) {
		return httpx.BadRequest("INVALID_AADHAAR", "aadhaar_number must be 12 digits")
	}
	if !r.Consent {
		return httpx.BadRequest("CONSENT_REQUIRED", "explicit consent is required for Aadhaar eKYC (DPDP)")
	}
	if !requestableTiers[r.RequestedTier] {
		return httpx.BadRequest("INVALID_TIER", "requested_tier must be one of FARMER, STANDARD, RIDER, HIGH")
	}
	return nil
}

// aadhaarKYCResponse returns the (PENDING) record and a human-readable
// status message for the approval workflow.
type aadhaarKYCResponse struct {
	Record  *domain.KYCRecord `json:"record"`
	Status  string            `json:"status"`
	Message string            `json:"message"`
}

// bankKYCRequest runs the (mock) penny-drop bank verification. Only the
// masked account tail and IFSC are ever persisted.
type bankKYCRequest struct {
	AccountNumber string `json:"account_number"`
	IFSC          string `json:"ifsc"`
}

func (r bankKYCRequest) validate() error {
	if !bankAccountRegexp.MatchString(r.AccountNumber) {
		return httpx.BadRequest("INVALID_ACCOUNT", "account_number must be 9-18 digits")
	}
	if !ifscRegexp.MatchString(r.IFSC) {
		return httpx.BadRequest("INVALID_IFSC", "ifsc must match the RBI IFSC format (e.g. SBIN0001234)")
	}
	return nil
}

// kycRejectRequest carries the mandatory human-readable rejection reason.
type kycRejectRequest struct {
	Reason string `json:"reason"`
}

func (r kycRejectRequest) validate() error {
	reason := strings.TrimSpace(r.Reason)
	if reason == "" || len(reason) > 500 {
		return httpx.BadRequest("INVALID_REASON", "reason must be 1-500 characters")
	}
	return nil
}

// settableKYCTiers are the tiers an authorised reviewer may set on a party via
// the admin direct-vouch endpoint. HIGHEST/SERVICE are included because a
// SUPER_ADMIN may set them; the per-role authority (which reviewer may set which
// tier) is enforced separately by domain.CanApproveKYCTier in the service.
var settableKYCTiers = map[string]bool{
	domain.KYCTierFarmer:   true,
	domain.KYCTierStandard: true,
	domain.KYCTierRider:    true,
	domain.KYCTierHigh:     true,
	domain.KYCTierHighest:  true,
	domain.KYCTierService:  true,
}

// kycVerifyRequest is the admin direct-vouch body: raise a party to `tier` with
// an optional human-readable reason (POST /parties/{id}/kyc/verify).
type kycVerifyRequest struct {
	Tier   string `json:"tier"`
	Reason string `json:"reason"`
}

func (r kycVerifyRequest) validate() error {
	if !settableKYCTiers[r.Tier] {
		return httpx.BadRequest("INVALID_TIER", "tier must be one of FARMER, STANDARD, RIDER, HIGH, HIGHEST, SERVICE")
	}
	if len(strings.TrimSpace(r.Reason)) > 500 {
		return httpx.BadRequest("INVALID_REASON", "reason must be at most 500 characters")
	}
	return nil
}

// pendingPartySummary is the reviewer-facing digest of the requesting party.
type pendingPartySummary struct {
	ID       primitive.ObjectID `json:"id"`
	Phone    string             `json:"phone"`
	FullName string             `json:"full_name,omitempty"`
	KYCTier  string             `json:"kyc_tier"`
}

// pendingKYCItem is one PENDING KYC record enriched with its party so the
// review console needs no second call.
type pendingKYCItem struct {
	domain.KYCRecord
	Party *pendingPartySummary `json:"party,omitempty"`
}

// pendingKYCCountResponse is the live badge value for the reviewer dashboard.
// Capped is true when the true count exceeds the scan cap (display "count+").
type pendingKYCCountResponse struct {
	Count  int64 `json:"count"`
	Capped bool  `json:"capped"`
}

// kycReviewResponse returns the reviewed record plus the party's KYC tier
// after the review (upgraded on approval when the requested tier is higher).
type kycReviewResponse struct {
	Record  *domain.KYCRecord `json:"record"`
	KYCTier string            `json:"kyc_tier"`
}

// createAssignmentRequest grants a role to a party inside an org unit. The
// target party is addressed by party_id or phone (party_id wins when both
// are present).
type createAssignmentRequest struct {
	PartyID   *primitive.ObjectID `json:"party_id,omitempty"`
	Phone     string              `json:"phone,omitempty"`
	RoleCode  string              `json:"role_code"`
	OrgUnitID primitive.ObjectID  `json:"org_unit_id"`
	ValidFrom *time.Time          `json:"valid_from,omitempty"`
	ValidTo   *time.Time          `json:"valid_to,omitempty"`
}

func (r createAssignmentRequest) validate() error {
	if r.PartyID == nil && r.Phone == "" {
		return httpx.BadRequest("MISSING_PARTY", "one of party_id or phone is required")
	}
	if r.PartyID == nil && !phoneRegexp.MatchString(r.Phone) {
		return httpx.BadRequest("INVALID_PHONE", "phone must be a 10-digit Indian mobile number")
	}
	if !domain.IsValidRole(r.RoleCode) {
		return httpx.BadRequest("INVALID_ROLE", "role_code is not in the role catalog")
	}
	if r.OrgUnitID.IsZero() {
		return httpx.BadRequest("MISSING_ORG_UNIT", "org_unit_id is required")
	}
	if r.ValidFrom != nil && r.ValidTo != nil && !r.ValidTo.After(*r.ValidFrom) {
		return httpx.BadRequest("INVALID_VALIDITY_WINDOW", "valid_to must be after valid_from")
	}
	return nil
}

// transferAssignmentRequest moves an ACTIVE assignment to another org unit
// (POST /roles/assignments/{id}/transfer).
type transferAssignmentRequest struct {
	ToOrgUnitID primitive.ObjectID `json:"to_org_unit_id"`
}

func (r transferAssignmentRequest) validate() error {
	if r.ToOrgUnitID.IsZero() {
		return httpx.BadRequest("MISSING_ORG_UNIT", "to_org_unit_id is required")
	}
	return nil
}

// transferAssignmentResponse carries both halves of the completed move.
type transferAssignmentResponse struct {
	Created *domain.RoleAssignment `json:"created"`
	Revoked *domain.RoleAssignment `json:"revoked"`
}

// replaceHolderRequest swaps THE holder of a role at an org unit
// (POST /orgs/{id}/replace-holder).
type replaceHolderRequest struct {
	RoleCode   string             `json:"role_code"`
	NewPartyID primitive.ObjectID `json:"new_party_id"`
}

func (r replaceHolderRequest) validate() error {
	if !domain.IsValidRole(r.RoleCode) {
		return httpx.BadRequest("INVALID_ROLE", "role_code is not in the role catalog")
	}
	if r.NewPartyID.IsZero() {
		return httpx.BadRequest("MISSING_PARTY", "new_party_id is required")
	}
	return nil
}

// replaceHolderResponse reports the incoming holder's assignment (created, or
// pre-existing when already_holder) and every displaced assignment revoked.
type replaceHolderResponse struct {
	Assignment    *domain.RoleAssignment  `json:"assignment"`
	AlreadyHolder bool                    `json:"already_holder"`
	Revoked       []domain.RoleAssignment `json:"revoked"`
}

// listMeta is the pagination meta block for list endpoints.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}

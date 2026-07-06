package identity

import (
	"regexp"
	"strings"
	"time"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// Request-shape validation patterns. Formats only — business rules live in
// the service layer.
var (
	phoneRegexp       = regexp.MustCompile(`^[6-9][0-9]{9}$`)      // 10-digit Indian mobile
	otpRegexp         = regexp.MustCompile(`^[0-9]{6}$`)           // 6-digit numeric OTP
	aadhaarRegexp     = regexp.MustCompile(`^[0-9]{12}$`)          // 12-digit Aadhaar number
	bankAccountRegexp = regexp.MustCompile(`^[0-9]{9,18}$`)        // NPCI-range account number
	ifscRegexp        = regexp.MustCompile(`^[A-Z]{4}0[A-Z0-9]{6}$`) // RBI IFSC format
)

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

// roleSelectRequest picks one active assignment to operate under.
type roleSelectRequest struct {
	RoleAssignmentID string `json:"role_assignment_id"`
}

func (r roleSelectRequest) validate() error {
	if strings.TrimSpace(r.RoleAssignmentID) == "" {
		return httpx.BadRequest("MISSING_ROLE_ASSIGNMENT_ID", "role_assignment_id is required")
	}
	return nil
}

// roleSelectResponse carries the fresh ROLE-kind access token.
type roleSelectResponse struct {
	AccessToken string `json:"access_token"`
	RoleCode    string `json:"role_code"`
	OrgUnitID   string `json:"org_unit_id"`
	OrgType     string `json:"org_type"`
}

// kycSummary is the masked, client-safe digest of the latest KYC record.
type kycSummary struct {
	Tier              string     `json:"tier"`
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
}

func (r patchMeRequest) validate() error {
	if r.FullName == nil && r.PreferredLanguage == nil {
		return httpx.BadRequest("EMPTY_PATCH", "provide at least one of full_name, preferred_language")
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

// aadhaarKYCRequest runs the (mock) Aadhaar eKYC flow. The full number is
// used transiently and NEVER stored (blueprint §18-A).
type aadhaarKYCRequest struct {
	AadhaarNumber string `json:"aadhaar_number"`
	Consent       bool   `json:"consent"`
	RequestedTier string `json:"requested_tier"` // FARMER | STANDARD
}

func (r aadhaarKYCRequest) validate() error {
	if !aadhaarRegexp.MatchString(r.AadhaarNumber) {
		return httpx.BadRequest("INVALID_AADHAAR", "aadhaar_number must be 12 digits")
	}
	if !r.Consent {
		return httpx.BadRequest("CONSENT_REQUIRED", "explicit consent is required for Aadhaar eKYC (DPDP)")
	}
	if r.RequestedTier != domain.KYCTierFarmer && r.RequestedTier != domain.KYCTierStandard {
		return httpx.BadRequest("INVALID_TIER", "requested_tier must be FARMER or STANDARD")
	}
	return nil
}

// aadhaarKYCResponse returns the created record and the party's (possibly
// upgraded) KYC tier.
type aadhaarKYCResponse struct {
	Record  *domain.KYCRecord `json:"record"`
	KYCTier string            `json:"kyc_tier"`
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

// createAssignmentRequest grants a role to a party inside an org unit. The
// target party is addressed by party_id or phone (party_id wins when both
// are present).
type createAssignmentRequest struct {
	PartyID   string     `json:"party_id,omitempty"`
	Phone     string     `json:"phone,omitempty"`
	RoleCode  string     `json:"role_code"`
	OrgUnitID string     `json:"org_unit_id"`
	ValidFrom *time.Time `json:"valid_from,omitempty"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`
}

func (r createAssignmentRequest) validate() error {
	if r.PartyID == "" && r.Phone == "" {
		return httpx.BadRequest("MISSING_PARTY", "one of party_id or phone is required")
	}
	if r.PartyID == "" && !phoneRegexp.MatchString(r.Phone) {
		return httpx.BadRequest("INVALID_PHONE", "phone must be a 10-digit Indian mobile number")
	}
	if !domain.IsValidRole(r.RoleCode) {
		return httpx.BadRequest("INVALID_ROLE", "role_code is not in the role catalog")
	}
	if strings.TrimSpace(r.OrgUnitID) == "" {
		return httpx.BadRequest("MISSING_ORG_UNIT", "org_unit_id is required")
	}
	if r.ValidFrom != nil && r.ValidTo != nil && !r.ValidTo.After(*r.ValidFrom) {
		return httpx.BadRequest("INVALID_VALIDITY_WINDOW", "valid_to must be after valid_from")
	}
	return nil
}

// listMeta is the pagination meta block for list endpoints.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}

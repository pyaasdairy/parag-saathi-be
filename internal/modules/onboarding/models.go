package onboarding

import (
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// phoneRegexp validates a 10-digit Indian mobile number — the natural key a
// submitted onboarding request is enrolled against.
var phoneRegexp = regexp.MustCompile(`^[6-9][0-9]{9}$`)

// requestableTiers are the KYC tiers an assisted-onboarding request may ask a
// new party to be verified at. HIGHEST/SERVICE are never field-onboarded.
var requestableTiers = map[string]bool{
	domain.KYCTierMinimal:  true,
	domain.KYCTierFarmer:   true,
	domain.KYCTierStandard: true,
	domain.KYCTierRider:    true,
	domain.KYCTierHigh:     true,
}

// submitRequest is the field executive's assisted-onboarding submission.
type submitRequest struct {
	Phone         string             `json:"phone"`
	FullName      string             `json:"full_name"`
	RequestedRole string             `json:"requested_role"`
	OrgUnitID     primitive.ObjectID `json:"org_unit_id"`
	RequestedTier string             `json:"requested_tier"`
	Note          string             `json:"note,omitempty"`
	DocumentRefs  []string           `json:"document_refs,omitempty"`
}

func (r submitRequest) validate() error {
	if !phoneRegexp.MatchString(r.Phone) {
		return httpx.BadRequest("INVALID_PHONE", "phone must be a 10-digit Indian mobile number")
	}
	name := strings.TrimSpace(r.FullName)
	if name == "" || len(name) > 120 {
		return httpx.BadRequest("INVALID_FULL_NAME", "full_name must be 1-120 characters")
	}
	if !domain.IsValidRole(r.RequestedRole) {
		return httpx.BadRequest("INVALID_ROLE", "requested_role is not in the role catalog")
	}
	if r.OrgUnitID.IsZero() {
		return httpx.BadRequest("MISSING_ORG_UNIT", "org_unit_id is required")
	}
	if !requestableTiers[r.RequestedTier] {
		return httpx.BadRequest("INVALID_TIER", "requested_tier must be one of MINIMAL, FARMER, STANDARD, RIDER, HIGH")
	}
	if len(r.Note) > 1000 {
		return httpx.BadRequest("INVALID_NOTE", "note must be at most 1000 characters")
	}
	if len(r.DocumentRefs) > 20 {
		return httpx.BadRequest("TOO_MANY_DOCS", "document_refs must contain at most 20 entries")
	}
	return nil
}

// rejectRequest carries the mandatory human-readable rejection reason.
type rejectRequest struct {
	Reason string `json:"reason"`
}

func (r rejectRequest) validate() error {
	reason := strings.TrimSpace(r.Reason)
	if reason == "" || len(reason) > 500 {
		return httpx.BadRequest("INVALID_REASON", "reason must be 1-500 characters")
	}
	return nil
}

// listMeta is the pagination meta block for the queue listing.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}

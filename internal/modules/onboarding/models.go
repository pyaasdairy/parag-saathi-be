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

// validDocTypes is the closed set of KYC document types a submission may name
// (mirrors the frontend KycDocType union). Every doorstep enrolment must pick
// one — the reviewer needs to know which document backs the identity.
var validDocTypes = map[string]bool{
	"AADHAAR":            true,
	"AADHAAR_DIGILOCKER": true,
	"DIGILOCKER":         true,
	"VOTER_ID":           true,
	"DRIVING_LICENSE":    true,
	"PAN":                true,
}

// submitRequest is the field executive's assisted-onboarding submission. Beyond
// the identity essentials it carries the rich doorstep capture (village, cattle,
// vehicle, staff id, document, photo URLs) which is stored verbatim on the queue
// item for the reviewer console — the approval saga still only needs the
// essentials.
type submitRequest struct {
	Phone         string             `json:"phone"`
	FullName      string             `json:"full_name"`
	FullNameHi    string             `json:"full_name_hi,omitempty"`
	RequestedRole string             `json:"requested_role"`
	OrgUnitID     primitive.ObjectID `json:"org_unit_id"`
	RequestedTier string             `json:"requested_tier"`
	// Adhyaksh-only: also appoint this person as the samiti's Sachiv on approval.
	AlsoAssignSachiv bool     `json:"also_assign_sachiv,omitempty"`
	Note             string   `json:"note,omitempty"`
	DocumentRefs     []string `json:"document_refs,omitempty"`
	// Rich field capture (optional).
	Village         string                  `json:"village,omitempty"`
	// Notice प्रारूप fields: पिता/पति का नाम (required for farmers at FE) + माता का नाम.
	FatherHusbandName string                `json:"father_husband_name,omitempty"`
	MotherName        string                `json:"mother_name,omitempty"`
	AdminHierarchy  *domain.AdminHierarchy  `json:"admin_hierarchy,omitempty"`
	DocumentType    string `json:"document_type,omitempty"`
	DocumentNumber  string `json:"document_number,omitempty"`
	KYCPhotoURL     string `json:"kyc_photo_url,omitempty"`
	ProfilePhotoURL string `json:"profile_photo_url,omitempty"`
	CattleCount     int    `json:"cattle_count,omitempty"`
	CattleBreed     string `json:"cattle_breed,omitempty"`
	VehicleNumber   string `json:"vehicle_number,omitempty"`
	EmployeeID      string `json:"employee_id,omitempty"`
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
	if r.AlsoAssignSachiv && r.RequestedRole != domain.RoleSamitiAdhyaksh {
		return httpx.BadRequest("VALIDATION", "also_assign_sachiv applies only to SAMITI_ADHYAKSH requests")
	}
	if !requestableTiers[r.RequestedTier] {
		return httpx.BadRequest("INVALID_TIER", "requested_tier must be one of MINIMAL, FARMER, STANDARD, RIDER, HIGH")
	}
	if !validDocTypes[r.DocumentType] {
		return httpx.Unprocessable("DOC_TYPE_REQUIRED", "document_type is required and must be a valid KYC document type")
	}
	if len(r.Note) > 1000 {
		return httpx.BadRequest("INVALID_NOTE", "note must be at most 1000 characters")
	}
	if len(r.DocumentRefs) > 20 {
		return httpx.BadRequest("TOO_MANY_DOCS", "document_refs must contain at most 20 entries")
	}
	if r.CattleCount < 0 || r.CattleCount > 10000 {
		return httpx.BadRequest("INVALID_CATTLE_COUNT", "cattle_count must be between 0 and 10000")
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

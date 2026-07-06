package domain

import "time"

// Party statuses.
const (
	PartyStatusActive    = "ACTIVE"
	PartyStatusSuspended = "SUSPENDED"
)

// Party is the single identity record per human (blueprint §4.1):
// one phone number = one Party = many org-scoped RoleAssignments.
type Party struct {
	ID                string    `bson:"_id"    json:"id"`
	Phone             string    `bson:"phone"  json:"phone"` // E.164 without +91 prefix normalisation is done at the edge
	FullName          string    `bson:"full_name,omitempty"          json:"full_name,omitempty"`
	PreferredLanguage string    `bson:"preferred_language,omitempty" json:"preferred_language,omitempty"` // "hi", "en", ...
	KYCTier           string    `bson:"kyc_tier"                     json:"kyc_tier"`
	Status            string    `bson:"status"                       json:"status"`
	CreatedAt         time.Time `bson:"created_at"                   json:"created_at"`
	UpdatedAt         time.Time `bson:"updated_at"                   json:"updated_at"`
}

// RoleAssignment statuses.
const (
	RoleAssignmentActive  = "ACTIVE"
	RoleAssignmentRevoked = "REVOKED"
)

// RoleAssignment grants one role to one party inside one org unit for a
// validity window. Revoking a Sacheev after an election is a single record
// update — never an account deletion (blueprint §4.1).
type RoleAssignment struct {
	ID        string     `bson:"_id"         json:"id"`
	PartyID   string     `bson:"party_id"    json:"party_id"`
	RoleCode  string     `bson:"role_code"   json:"role_code"`
	OrgUnitID string     `bson:"org_unit_id" json:"org_unit_id"`
	GrantedBy string     `bson:"granted_by,omitempty" json:"granted_by,omitempty"` // party ID of granter
	ValidFrom time.Time  `bson:"valid_from"  json:"valid_from"`
	ValidTo   *time.Time `bson:"valid_to,omitempty"   json:"valid_to,omitempty"` // nil = open-ended
	Status    string     `bson:"status"      json:"status"`
	RevokedAt *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	RevokedBy string     `bson:"revoked_by,omitempty" json:"revoked_by,omitempty"`
	CreatedAt time.Time  `bson:"created_at"  json:"created_at"`
}

// UsableAt reports whether the assignment is active and inside its validity
// window at instant t.
func (ra RoleAssignment) UsableAt(t time.Time) bool {
	if ra.Status != RoleAssignmentActive {
		return false
	}
	if t.Before(ra.ValidFrom) {
		return false
	}
	if ra.ValidTo != nil && t.After(*ra.ValidTo) {
		return false
	}
	return true
}

// KYC record statuses.
const (
	KYCStatusPending  = "PENDING"
	KYCStatusVerified = "VERIFIED"
	KYCStatusRejected = "REJECTED"
)

// KYCRecord stores verification evidence for a party at a given tier.
// Aadhaar compliance (blueprint §18-A): only the LAST 4 digits and an opaque
// vault reference are ever stored — never the full number, never biometrics.
type KYCRecord struct {
	ID                string     `bson:"_id"      json:"id"`
	PartyID           string     `bson:"party_id" json:"party_id"`
	Tier              string     `bson:"tier"     json:"tier"`
	AadhaarLast4      string     `bson:"aadhaar_last4,omitempty"       json:"aadhaar_last4,omitempty"`
	AadhaarVaultRef   string     `bson:"aadhaar_vault_ref,omitempty"   json:"aadhaar_vault_ref,omitempty"` // opaque token from the Aadhaar Data Vault
	BankAccountMasked string     `bson:"bank_account_masked,omitempty" json:"bank_account_masked,omitempty"`
	BankIFSC          string     `bson:"bank_ifsc,omitempty"           json:"bank_ifsc,omitempty"`
	BankVerified      bool       `bson:"bank_verified"                 json:"bank_verified"`
	BankNameMatch     float64    `bson:"bank_name_match,omitempty"     json:"bank_name_match,omitempty"` // penny-drop name-match score 0..1
	DigiLockerDocs    []DocRef   `bson:"digilocker_docs,omitempty"     json:"digilocker_docs,omitempty"`
	VideoKYCRef       string     `bson:"video_kyc_ref,omitempty"       json:"video_kyc_ref,omitempty"`
	ConsentID         string     `bson:"consent_id,omitempty"          json:"consent_id,omitempty"`
	Status            string     `bson:"status"      json:"status"`
	RejectionReason   string     `bson:"rejection_reason,omitempty" json:"rejection_reason,omitempty"`
	VerifiedAt        *time.Time `bson:"verified_at,omitempty"      json:"verified_at,omitempty"`
	CreatedAt         time.Time  `bson:"created_at"  json:"created_at"`
	UpdatedAt         time.Time  `bson:"updated_at"  json:"updated_at"`
}

// DocRef points at an externally fetched document (DigiLocker etc.).
type DocRef struct {
	DocType   string    `bson:"doc_type"  json:"doc_type"` // LAND_RECORD, CASTE_CERT, DL, RC ...
	Issuer    string    `bson:"issuer"    json:"issuer"`
	RefID     string    `bson:"ref_id"    json:"ref_id"`
	FetchedAt time.Time `bson:"fetched_at" json:"fetched_at"`
}

// Consent is a DPDP-grade consent artefact: plain-language, versioned,
// per-purpose, withdrawable (blueprint §18-A).
type Consent struct {
	ID          string     `bson:"_id"          json:"id"`
	PartyID     string     `bson:"party_id"     json:"party_id"`
	Purpose     string     `bson:"purpose"      json:"purpose"` // e.g. "AADHAAR_EKYC", "AA_FINANCIAL_DATA"
	TextVersion string     `bson:"text_version" json:"text_version"`
	Language    string     `bson:"language"     json:"language"`
	GrantedAt   time.Time  `bson:"granted_at"   json:"granted_at"`
	WithdrawnAt *time.Time `bson:"withdrawn_at,omitempty" json:"withdrawn_at,omitempty"`
}

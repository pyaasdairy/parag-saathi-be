package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Onboarding request statuses — the assisted-onboarding queue lifecycle:
// an ONBOARDING_EXECUTIVE / ORGANISING_MANAGER submits a doorstep enrolment
// request (PENDING) → an authorised reviewer approves (creating the Party,
// a VERIFIED KYCRecord and the RoleAssignment in one saga) or rejects it.
const (
	OnboardingStatusPending  = "PENDING"
	OnboardingStatusApproved = "APPROVED"
	OnboardingStatusRejected = "REJECTED"
)

// OnboardingRequest is one assisted-onboarding submission captured in the
// field (blueprint §4 identity, ONBOARDING_EXECUTIVE flow). It is the durable
// work-item behind the onboarding review console: the submitter records the
// prospective party's phone, name, the role and tier they should hold and any
// supporting document references; a reviewer then approves — which atomically
// creates-or-finds the Party by phone, verifies KYC at RequestedTier and
// grants RequestedRole@OrgUnitID — or rejects with a reason.
type OnboardingRequest struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Phone         string             `bson:"phone"         json:"phone"`
	FullName      string             `bson:"full_name"     json:"full_name"`
	FullNameHi    string             `bson:"full_name_hi,omitempty" json:"full_name_hi,omitempty"`
	RequestedRole string             `bson:"requested_role" json:"requested_role"`
	OrgUnitID     primitive.ObjectID `bson:"org_unit_id"   json:"org_unit_id"`
	RequestedTier string             `bson:"requested_tier" json:"requested_tier"`
	Note          string             `bson:"note,omitempty" json:"note,omitempty"`
	DocumentRefs  []string           `bson:"document_refs,omitempty" json:"document_refs,omitempty"`
	// Rich field-capture carried from the doorstep enrolment form (blueprint §4).
	// These are stored verbatim for the reviewer console; the approval saga still
	// only needs phone/full_name/requested_role/org_unit_id/requested_tier.
	Village         string              `bson:"village,omitempty"         json:"village,omitempty"`
	DocumentType    string              `bson:"document_type,omitempty"   json:"document_type,omitempty"`
	DocumentNumber  string              `bson:"document_number,omitempty" json:"document_number,omitempty"`
	KYCPhotoURL     string              `bson:"kyc_photo_url,omitempty"     json:"kyc_photo_url,omitempty"`
	ProfilePhotoURL string              `bson:"profile_photo_url,omitempty" json:"profile_photo_url,omitempty"`
	CattleCount     int                 `bson:"cattle_count,omitempty"    json:"cattle_count,omitempty"`
	CattleBreed     string              `bson:"cattle_breed,omitempty"    json:"cattle_breed,omitempty"`
	VehicleNumber   string              `bson:"vehicle_number,omitempty"  json:"vehicle_number,omitempty"`
	EmployeeID      string              `bson:"employee_id,omitempty"     json:"employee_id,omitempty"`
	Status          string              `bson:"status"        json:"status"`
	SubmittedBy     primitive.ObjectID  `bson:"submitted_by"  json:"submitted_by"`
	ReviewedBy      *primitive.ObjectID `bson:"reviewed_by,omitempty"      json:"reviewed_by,omitempty"`
	ReviewedAt      *time.Time          `bson:"reviewed_at,omitempty"      json:"reviewed_at,omitempty"`
	RejectionReason string              `bson:"rejection_reason,omitempty" json:"rejection_reason,omitempty"`
	// CreatedParty is stamped on approval with the id of the Party the saga
	// created or found for Phone.
	CreatedParty *primitive.ObjectID `bson:"created_party,omitempty" json:"created_party,omitempty"`
	CreatedAt    time.Time           `bson:"created_at" json:"created_at"`
	UpdatedAt    time.Time           `bson:"updated_at" json:"updated_at"`
}

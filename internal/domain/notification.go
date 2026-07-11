package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Notification channels — SMS/IVR are essential, not optional, for low-literacy
// users (blueprint §13).
const (
	ChannelSMS  = "SMS"
	ChannelIVR  = "IVR"
	ChannelPush = "PUSH"
	ChannelApp  = "APP" // in-app inbox only (served on GET /notifications/me)
)

// Notification statuses (outbox pattern: write locally, provider worker sends).
const (
	NotificationQueued = "QUEUED"
	NotificationSent   = "SENT"
	NotificationFailed = "FAILED"
)

// Template keys used across modules.
const (
	TemplatePourReceipt    = "POUR_RECEIPT" // "10.5L @ ₹42.30 = ₹444.15 credited to today's bill"
	TemplateInvoiceIssued  = "INVOICE_ISSUED"
	TemplatePayoutCredited = "PAYOUT_CREDITED" // "₹444.15 credited to a/c ****1234, UTR ..."
	TemplateSafetyBlock    = "SAFETY_BLOCK"    // supervisor alert on gate failure
	TemplateOTP            = "OTP"
	TemplateMVUDispatched  = "MVU_DISPATCHED"
	TemplateMVUClosed      = "MVU_CLOSED" // "the MVU visit for your animal is complete"
	TemplateKYCApproved    = "KYC_APPROVED" // "your KYC was approved — you can now use your role"
	TemplateKYCRejected    = "KYC_REJECTED"
	TemplateKYCPending     = "KYC_PENDING" // reviewer alert: "a KYC verification is pending your review"

	// Role/onboarding lifecycle (params carry human-readable names, not ids).
	TemplateRoleGranted         = "ROLE_GRANTED"         // to the grantee: role + org name
	TemplateRoleRevoked         = "ROLE_REVOKED"         // to the former holder: role + org name
	TemplateOnboardingApproved  = "ONBOARDING_APPROVED"  // to the onboarded party: role + org name
	TemplateOnboardingRejected  = "ONBOARDING_REJECTED"  // to the applicant phone: role + reason

	// Per-samiti batch lifecycle (params: batch_code, samiti/plant names).
	TemplateConsignmentAccepted = "CONSIGNMENT_PLANT_ACCEPTED" // to the samiti sachiv
	TemplateConsignmentRejected = "CONSIGNMENT_PLANT_REJECTED" // to the samiti sachiv
	TemplateQCResult            = "QC_RESULT"                  // to sachiv + plant operator: PASS|HOLD|FAIL
	TemplateBatchQRMinted       = "BATCH_QR_MINTED"            // to the samiti sachiv: public QR live
)

// Notification is one queued vernacular message to a party.
type Notification struct {
	ID          primitive.ObjectID  `bson:"_id,omitempty" json:"id"`
	PartyID     *primitive.ObjectID `bson:"party_id,omitempty" json:"party_id,omitempty"`
	Phone       string              `bson:"phone"    json:"phone"`
	Channel     string              `bson:"channel"  json:"channel"`
	TemplateKey string              `bson:"template_key" json:"template_key"`
	Language    string              `bson:"language" json:"language"`
	Params      map[string]string   `bson:"params,omitempty" json:"params,omitempty"`
	Status      string              `bson:"status"   json:"status"`
	ProviderRef string              `bson:"provider_ref,omitempty" json:"provider_ref,omitempty"`
	Error       string              `bson:"error,omitempty"        json:"error,omitempty"`
	QueuedAt    time.Time           `bson:"queued_at" json:"queued_at"`
	SentAt      *time.Time          `bson:"sent_at,omitempty" json:"sent_at,omitempty"`
	// ReadAt is stamped when the addressed party marks the notification read
	// in-app (POST /notifications/{id}/read); nil = unread.
	ReadAt *time.Time `bson:"read_at,omitempty" json:"read_at,omitempty"`
}

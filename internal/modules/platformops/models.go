package platformops

import (
	"time"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/audit"
)

// SetFlagRequest is the PUT /admin/flags/{key} body. Enabled is a pointer so
// an absent field is distinguishable from an explicit false.
type SetFlagRequest struct {
	Enabled *bool `json:"enabled"`
}

// SetFlagResponse echoes the applied flag state back to the admin console.
type SetFlagResponse struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// AuditExport is the attachment body of GET /audit/logs/export — the
// GIGW/DPDP "immutable audit export" (blueprint §12). Entries come straight
// from the insert-only audit_logs collection; there is deliberately no write
// or delete API surface over them.
type AuditExport struct {
	ExportedAt time.Time     `json:"exported_at"`
	From       *time.Time    `json:"from,omitempty"`
	To         *time.Time    `json:"to,omitempty"`
	Count      int           `json:"count"`
	Entries    []audit.Entry `json:"entries"`
}

// StoredNotification is a notifications document as this module reads and
// writes it: the shared domain shape plus the worker-written meta block
// (rendered SMS text kept for demo/ops visibility).
type StoredNotification struct {
	domain.Notification `bson:",inline"`

	Meta map[string]any `bson:"meta,omitempty" json:"meta,omitempty"`
}

// WorkerRunResponse reports how many queued notifications the mock SMS
// worker dispatched in this run.
type WorkerRunResponse struct {
	Sent int `json:"sent"`
}

// PartyRoleView is one active role grant in the support lookup view.
type PartyRoleView struct {
	RoleCode  string `json:"role_code"`
	OrgUnitID string `json:"org_unit_id"`
	OrgName   string `json:"org_name,omitempty"`
}

// PartyLookupResponse is the limited-PII support view (§5.2 role 20):
// identity basics and role grants only — never KYC document numbers.
type PartyLookupResponse struct {
	PartyID  string          `json:"party_id"`
	FullName string          `json:"full_name,omitempty"`
	KYCTier  string          `json:"kyc_tier"`
	Status   string          `json:"status"`
	Roles    []PartyRoleView `json:"roles"`
}

// listMeta is the pagination metadata attached to list responses.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}

// auditLogFilter carries the validated GET /audit/logs query filters.
type auditLogFilter struct {
	ActorPartyID string
	TargetType   string
	Action       string
	From         *time.Time
	To           *time.Time
}

// payoutCreditedEvent mirrors the settlement module's payload on
// eventbus.TopicPayoutCredited. Modules never import each other, so bus
// payloads are re-declared by JSON shape and decoded structurally.
type payoutCreditedEvent struct {
	FarmerPartyID string  `json:"farmer_party_id"`
	Phone         string  `json:"phone"`
	Amount        float64 `json:"amount"`
	UTR           string  `json:"utr"`
}

// gateBlockedEvent mirrors the quality module's payload on
// eventbus.TopicGateBlocked.
type gateBlockedEvent struct {
	SubjectType    string   `json:"subject_type"`
	SubjectID      string   `json:"subject_id"`
	QCResultID     string   `json:"qc_result_id"`
	Stage          string   `json:"stage"`
	FailureReasons []string `json:"failure_reasons"`
}

// mvuDispatchedEvent mirrors the cattle module's payload on
// eventbus.TopicMVUDispatched. All common key spellings for the case ID are
// accepted ("case_id" is the documented contract; "id" covers a raw domain
// document) so the subscriber stays tolerant of the publisher's exact shape.
type mvuDispatchedEvent struct {
	CaseID        string `json:"case_id"`
	MVUCaseID     string `json:"mvu_case_id"`
	ID            string `json:"id"`
	FarmerPartyID string `json:"farmer_party_id"`
	Phone         string `json:"phone"`
	AnimalID      string `json:"animal_id"`
	DCSID         string `json:"dcs_id"`
	ETA           string `json:"eta"`
}

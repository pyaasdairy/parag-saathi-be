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

// OrgUnitCounts breaks the cooperative backbone down by node type for the
// control-tower stats card (blueprint §5.1).
type OrgUnitCounts struct {
	DCS   int64 `json:"dcs"`
	BMC   int64 `json:"bmc"`
	Plant int64 `json:"plant"`
}

// AdminStats is the GET /admin/stats control-tower aggregate (blueprint §12):
// a read-only snapshot of the platform's key operational counters. Every
// figure comes from a CountDocuments over the shared collections (today's
// litres is the one summed figure); nothing here mutates state.
type AdminStats struct {
	Parties               int64         `json:"parties"`
	ActiveRoleAssignments int64         `json:"active_role_assignments"`
	OrgUnits              OrgUnitCounts `json:"org_units"`
	TodayPours            int64         `json:"today_pours"`
	TodayLitres           float64       `json:"today_litres"`
	PendingKYC            int64         `json:"pending_kyc"`
	PendingInvoices       int64         `json:"pending_invoices"`
	BlockedQCSubjects     int64         `json:"blocked_qc_subjects"`
	// SettledAmount30d is the total money disbursed by EXECUTED settlement
	// batches in the last 30 days; OpenBatches is the count of processing
	// batches not yet COMPLETED or BLOCKED; FailedBatches30d is the count of
	// BLOCKED batches created in the last 30 days.
	SettledAmount30d float64 `json:"settled_amount_30d"`
	OpenBatches      int64   `json:"open_batches"`
	FailedBatches30d int64   `json:"failed_batches_30d"`
	GeneratedAt      time.Time `json:"generated_at"`
}

// UpsertProductRequest is the PUT /admin/products body. SKU is the upsert key;
// Active is a pointer so an absent field is distinguishable from an explicit
// false (default on insert is active=true).
type UpsertProductRequest struct {
	SKU           string  `json:"sku"`
	Name          string  `json:"name"`
	NameHi        string  `json:"name_hi"`
	Category      string  `json:"category"` // MILK|DAHI|PANEER|GHEE|BUTTER|OTHER (defaults OTHER)
	MRP           float64 `json:"mrp"`
	UnitSize      string  `json:"unit_size"`
	ShelfLifeDays int     `json:"shelf_life_days"`
	Active        *bool   `json:"active"`
}

// SachivCapRequest is the PUT /admin/sachiv-cap body: the governance ceiling on
// how many SAMITI_SACHEEV a DCS may appoint. SetByPartyID is derived from the
// token; the client value (if any) is ignored.
type SachivCapRequest struct {
	Cap int `json:"cap"`
}

// SachivCapResponse is the GET/PUT /admin/sachiv-cap body: the PER-DCS cap plus
// Appointed — the tightest per-DCS occupancy (the count of appointed ACTIVE
// SAMITI_SACHEEV at the busiest single DCS). Appointed is measured per-DCS, not
// federation-wide, so it is directly comparable to the per-DCS cap.
type SachivCapResponse struct {
	Cap       int `json:"cap"`
	Appointed int `json:"appointed"`
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

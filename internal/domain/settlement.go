package domain

import "time"

// Settlement batch statuses. Payment guardrail (blueprint §8.1): Saathi
// computes and *initiates*; an authorised human *approves*; the licensed
// Payment Aggregator *executes*. Money never moves autonomously.
const (
	SettlementStatusInitiated       = "INITIATED"
	SettlementStatusPendingApproval = "PENDING_APPROVAL"
	SettlementStatusApproved        = "APPROVED"
	SettlementStatusExecuting       = "EXECUTING"
	SettlementStatusExecuted        = "EXECUTED"
	SettlementStatusPartial         = "PARTIAL" // some payouts failed
	SettlementStatusFailed          = "FAILED"
	SettlementStatusRejected        = "REJECTED" // approver declined
)

// SettlementBatch bundles a DCS day's invoices for one disbursement run.
// Dual control: InitiatedBy (Sacheev) must differ from ApprovedBy (Adhyaksh /
// authorised signatory).
type SettlementBatch struct {
	ID            string     `bson:"_id"     json:"id"`
	DCSID         string     `bson:"dcs_id"  json:"dcs_id"`
	Date          string     `bson:"date"    json:"date"` // YYYY-MM-DD
	InvoiceIDs    []string   `bson:"invoice_ids"  json:"invoice_ids"`
	TotalAmount   float64    `bson:"total_amount" json:"total_amount"`
	Status        string     `bson:"status"       json:"status"`
	InitiatedBy   string     `bson:"initiated_by" json:"initiated_by"`
	ApprovedBy    string     `bson:"approved_by,omitempty"  json:"approved_by,omitempty"`
	RejectedBy    string     `bson:"rejected_by,omitempty"  json:"rejected_by,omitempty"`
	RejectReason  string     `bson:"reject_reason,omitempty" json:"reject_reason,omitempty"`
	PARef         string     `bson:"pa_ref,omitempty"       json:"pa_ref,omitempty"` // payment-aggregator batch reference
	FailReason    string     `bson:"fail_reason,omitempty"  json:"fail_reason,omitempty"`
	ApprovedAt    *time.Time `bson:"approved_at,omitempty"  json:"approved_at,omitempty"`
	// ExecutingAt is the execution lease stamp: set on the APPROVED→EXECUTING
	// claim so an interrupted run can be safely resumed once the lease is stale.
	ExecutingAt *time.Time `bson:"executing_at,omitempty" json:"executing_at,omitempty"`
	ExecutedAt  *time.Time `bson:"executed_at,omitempty"  json:"executed_at,omitempty"`
	ProvenanceSeq int64      `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
	CreatedAt     time.Time  `bson:"created_at"   json:"created_at"`
}

// Payout instruction statuses.
const (
	PayoutStatusQueued  = "QUEUED"
	PayoutStatusSuccess = "SUCCESS"
	PayoutStatusFailed  = "FAILED"
)

// PayoutInstruction is one farmer credit inside a settlement batch.
type PayoutInstruction struct {
	ID                string     `bson:"_id"     json:"id"`
	SettlementBatchID string     `bson:"settlement_batch_id" json:"settlement_batch_id"`
	InvoiceID         string     `bson:"invoice_id"      json:"invoice_id"`
	FarmerPartyID     string     `bson:"farmer_party_id" json:"farmer_party_id"`
	Amount            float64    `bson:"amount"  json:"amount"`
	BankAccountMasked string     `bson:"bank_account_masked,omitempty" json:"bank_account_masked,omitempty"`
	UTR               string     `bson:"utr,omitempty"   json:"utr,omitempty"` // bank transaction reference
	Status            string     `bson:"status"  json:"status"`
	FailureReason     string     `bson:"failure_reason,omitempty" json:"failure_reason,omitempty"`
	CreditedAt        *time.Time `bson:"credited_at,omitempty"    json:"credited_at,omitempty"`
	CreatedAt         time.Time  `bson:"created_at" json:"created_at"`
}

// DBT subsidy request statuses — subsidy money is strictly PFMS/DBT rail.
const (
	DBTStatusSubmitted = "SUBMITTED"
	DBTStatusAccepted  = "ACCEPTED"
	DBTStatusCredited  = "CREDITED"
	DBTStatusRejected  = "REJECTED"
)

// DBTRequest routes a scheme subsidy to an Aadhaar-linked beneficiary account
// via PFMS. Saathi tracks status; it never disburses subsidies itself.
type DBTRequest struct {
	ID            string     `bson:"_id"         json:"id"`
	SchemeCode    string     `bson:"scheme_code" json:"scheme_code"` // e.g. NAND_BABA_SUBSIDY
	FarmerPartyID string     `bson:"farmer_party_id" json:"farmer_party_id"`
	Amount        float64    `bson:"amount"      json:"amount"`
	PFMSRef       string     `bson:"pfms_ref,omitempty" json:"pfms_ref,omitempty"`
	Status        string     `bson:"status"      json:"status"`
	SubmittedBy   string     `bson:"submitted_by" json:"submitted_by"`
	CreditedAt    *time.Time `bson:"credited_at,omitempty" json:"credited_at,omitempty"`
	CreatedAt     time.Time  `bson:"created_at"  json:"created_at"`
}

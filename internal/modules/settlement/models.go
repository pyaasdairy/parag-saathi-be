package settlement

import (
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// InitiateSettlementRequest asks Saathi to bundle a DCS day's ISSUED invoices
// into one settlement batch (blueprint §8.1 step 1: Saathi computes and
// *initiates* — it never moves money). DCSID arrives as a plain hex string in
// JSON and unmarshals natively into an ObjectID.
type InitiateSettlementRequest struct {
	DCSID primitive.ObjectID `json:"dcs_id"`
	Date  string             `json:"date,omitempty"` // YYYY-MM-DD; defaults to today (IST)
}

// RejectSettlementRequest carries the approver's reason for declining a batch.
type RejectSettlementRequest struct {
	Reason string `json:"reason"`
}

// SettlementDetail is a batch together with its payout instructions.
type SettlementDetail struct {
	Batch   domain.SettlementBatch     `json:"batch"`
	Payouts []domain.PayoutInstruction `json:"payout_instructions"`
}

// CreateDBTRequestInput submits a scheme subsidy for PFMS/DBT routing
// (blueprint §13). Subsidy money is strictly separate from milk payments.
// scheme_name / scheme_name_hi are optional display labels carried through to
// the request (the FE apply flow has a scheme name but not a numeric amount at
// application time, so amount may be 0 until the subsidy is costed).
type CreateDBTRequestInput struct {
	SchemeCode    string             `json:"scheme_code"`
	SchemeName    string             `json:"scheme_name,omitempty"`
	SchemeNameHi  string             `json:"scheme_name_hi,omitempty"`
	FarmerPartyID primitive.ObjectID `json:"farmer_party_id"`
	Amount        float64            `json:"amount"`
}

// UpdateDBTStatusInput tracks the DBT rail's progress for a request.
type UpdateDBTStatusInput struct {
	Status string `json:"status"` // ACCEPTED | CREDITED | REJECTED
}

// PayoutCreditedEvent is the payload this module publishes on
// eventbus.TopicPayoutCredited — one per credited payout. The platformops
// module consumes it to queue the TemplatePayoutCredited SMS
// ("₹ credited, UTR …" — blueprint §8.1 last step). FarmerPartyID crosses the
// bus seam as an ObjectID hex string (bus payloads are a JSON-shape contract).
type PayoutCreditedEvent struct {
	FarmerPartyID string  `json:"farmer_party_id"`
	Phone         string  `json:"phone"`
	Amount        float64 `json:"amount"`
	UTR           string  `json:"utr"`
}

package quality

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// QCTestInput is one measurement submitted for gate evaluation. The Pass
// verdict is computed server-side by domain.EvaluateQCTests — clients never
// send it.
type QCTestInput struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// RecordQCResultRequest is the POST /quality/qc-results payload. SubjectID is
// the subject document's ObjectID, sent as its plain hex string.
type RecordQCResultRequest struct {
	SubjectType string             `json:"subject_type"` // domain.QCSubject*
	SubjectID   primitive.ObjectID `json:"subject_id"`
	Stage       string             `json:"stage"` // domain.QCStageBMCRapid | domain.QCStagePlantLab
	Tests       []QCTestInput      `json:"tests"`
	LabRef      string             `json:"lab_ref,omitempty"`
	// Verdict/Hold request a HOLD (§13.5): the subject is QUARANTINED pending
	// resolution instead of PASSED/BLOCKED. A panel with a hard FSSAI failure
	// can never be held — it always REJECTs. PASS/REJECT are derived from the
	// tests server-side and cannot be requested.
	Verdict string `json:"verdict,omitempty"` // only "HOLD" is honoured
	Hold    bool   `json:"hold,omitempty"`
}

// ResolveQCResultRequest is the POST /quality/qc-results/{id}/resolve body —
// the analyst's HOLD→PASS/REJECT decision on a quarantined subject.
type ResolveQCResultRequest struct {
	Verdict string `json:"verdict"` // PASS | REJECT
	Notes   string `json:"notes,omitempty"`
}

// QCLimit is one FSSAI gate limit, shaped for client display.
type QCLimit struct {
	TestName    string  `json:"test_name"`
	Limit       float64 `json:"limit"`
	Unit        string  `json:"unit"`
	Comparison  string  `json:"comparison"` // MAX | MUST_BE_NEGATIVE
	Description string  `json:"description"`
}

// LimitsResponse is the GET /quality/limits body — the FSSAI constants the
// safety gate enforces (blueprint §8.3), as reference data.
type LimitsResponse struct {
	Authority string    `json:"authority"`
	Limits    []QCLimit `json:"limits"`
}

// GateBlockedPayload is published on eventbus.TopicGateBlocked when a subject
// fails the safety gate. A blocked lot is quarantined and can never advance —
// the plant module subscribes and enforces downstream. ObjectIDs marshal to
// plain hex strings, so structural (JSON-shape) subscribers are unaffected.
type GateBlockedPayload struct {
	SubjectType    string             `json:"subject_type"`
	SubjectID      primitive.ObjectID `json:"subject_id"`
	QCResultID     primitive.ObjectID `json:"qc_result_id"`
	Stage          string             `json:"stage"`
	FailureReasons []string           `json:"failure_reasons"`
}

// QCRecordedPayload is published on eventbus.TopicQCRecorded after every QC
// verdict (pass or fail). The server bridges it to the SSE "quality.changed"
// topic so open lab dashboards re-fetch their queue.
type QCRecordedPayload struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Stage       string `json:"stage"`
	OverallPass bool   `json:"overall_pass"`
	Verdict     string `json:"verdict,omitempty"` // PASS | HOLD | REJECT
}

// BatchQCRecordedEvent is published on eventbus.TopicBatchQCRecorded after a
// per-samiti batch QC verdict (F7) — platformops notifies the samiti sachiv
// and the receiving plant's operator. IDs travel as hex strings.
type BatchQCRecordedEvent struct {
	ConsignmentID    string   `json:"consignment_id"`
	BatchCode        string   `json:"batch_code"`
	DCSID            string   `json:"dcs_id"`
	PlantID          string   `json:"plant_id,omitempty"`
	Verdict          string   `json:"verdict"` // PASS | HOLD | REJECT
	FailedParameters []string `json:"failed_parameters,omitempty"`
}

// BatchQRMintedEvent is published on eventbus.TopicBatchQRMinted when a
// passing batch auto-mints its public QR — platformops notifies the sachiv.
type BatchQRMintedEvent struct {
	ConsignmentID string `json:"consignment_id"`
	BatchCode     string `json:"batch_code"`
	DCSID         string `json:"dcs_id"`
	Token         string `json:"token"`
}

// listMeta is the pagination metadata attached to list responses.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}

// QCQueueItem is one gate-eligible subject currently awaiting a verdict,
// annotated with the stage's mandatory tests split into those already recorded
// and those still pending. It is a lean projection — enough for the QC queue
// screen without echoing full subject documents.
type QCQueueItem struct {
	SubjectType    string             `json:"subject_type"` // domain.QCSubject*
	SubjectID      primitive.ObjectID `json:"subject_id"`
	Stage          string             `json:"stage"`        // the QC stage this subject is gated at
	Reference      string             `json:"reference"`    // human label (lot date/shift or batch number)
	InputLitres    float64            `json:"input_litres"` // volume awaiting QC (lot litres / batch input litres)
	OrgUnitID      primitive.ObjectID `json:"org_unit_id"`
	MandatoryTests []string           `json:"mandatory_tests"`
	RecordedTests  []string           `json:"recorded_tests"`
	PendingTests   []string           `json:"pending_tests"`
	CreatedAt      time.Time          `json:"created_at"`
}

// QCQueueResponse is the GET /quality/qc-queue body.
type QCQueueResponse struct {
	Items []QCQueueItem `json:"items"`
	Total int           `json:"total"`
}

// TraceBackResponse is the GET /quality/batches/{id}/trace-back body — the
// root-cause tool. It resolves the contributing societies of a batch (§7.4
// honest set-valued pooling) enriched from the org directory, plus the batch's
// recorded QC results.
type TraceBackResponse struct {
	BatchID               primitive.ObjectID    `json:"batch_id"`
	BatchNumber           string                `json:"batch_number"`
	PlantID               primitive.ObjectID    `json:"plant_id"`
	Status                string                `json:"status"`
	InputLitres           float64               `json:"input_litres"` // total volume the batch pooled
	BlockReason           string                `json:"block_reason,omitempty"`
	ContributingSocieties []ContributingSociety `json:"contributing_societies"`
	QCResults             []domain.QCResult     `json:"qc_results"`
}

// ContributingSociety is one DCS whose milk fed a batch, enriched from the org
// directory and with its weighted share of the pool. Resolved is false (and
// Name/Code empty) when the org lookup fails — the trace stays honest rather
// than silently dropping a contributor.
type ContributingSociety struct {
	OrgUnitID    primitive.ObjectID `json:"org_unit_id"`
	Code         string             `json:"code,omitempty"`
	Name         string             `json:"name,omitempty"`
	NameHi       string             `json:"name_hi,omitempty"`
	Village      string             `json:"village,omitempty"`
	District     string             `json:"district,omitempty"`
	VolumeLitres float64            `json:"volume_litres"`
	VolumeShare  float64            `json:"volume_share"` // 0..1 of the batch pool
	PourCount    int                `json:"pour_count"`
	CollectedOn  string             `json:"collected_on,omitempty"`
	Resolved     bool               `json:"resolved"`
}

package quality

import "go.mongodb.org/mongo-driver/bson/primitive"

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

// listMeta is the pagination metadata attached to list responses.
type listMeta struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
	Total  int64 `json:"total"`
}

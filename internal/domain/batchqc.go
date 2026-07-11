package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Per-samiti batch QC parameters (F7). These are the MOCK criteria the
// PLANT_LAB_ANALYST records manually per delivered batch — configurable later.
// AFLATOXIN_M1 keeps its hard FSSAI ceiling (0.5 µg/kg) server-side: a value
// over the limit can never be recorded as a pass, whatever the client sent.
const (
	BatchTestAflatoxinM1 = "AFLATOXIN_M1" // µg/kg, limit 0.5
	BatchTestAddedWater  = "ADDED_WATER"  // freezing-point depression check
	BatchTestAdulterants = "ADULTERANTS"  // neutralisers/detergents/urea/starch — pass/fail
	BatchTestAntibiotics = "ANTIBIOTICS"  // pass/fail
	BatchTestFatPct      = "FAT_PCT"      // %
	BatchTestSNFPct      = "SNF_PCT"      // %
)

// BatchQCParameters is the closed vocabulary of recordable batch QC
// parameters. Anything else is rejected instead of silently recorded.
var BatchQCParameters = map[string]struct{}{
	BatchTestAflatoxinM1: {},
	BatchTestAddedWater:  {},
	BatchTestAdulterants: {},
	BatchTestAntibiotics: {},
	BatchTestFatPct:      {},
	BatchTestSNFPct:      {},
}

// BatchQCTest is one manually recorded measurement of a per-samiti batch.
// Value is a pointer so pass/fail-only parameters (ADULTERANTS, ANTIBIOTICS)
// can omit a numeric reading instead of fabricating a zero.
type BatchQCTest struct {
	Parameter string   `bson:"parameter"       json:"parameter"`
	Value     *float64 `bson:"value,omitempty" json:"value,omitempty"`
	Unit      string   `bson:"unit,omitempty"  json:"unit,omitempty"`
	Pass      bool     `bson:"pass"            json:"pass"`
}

// ConsignmentQC is the per-batch (per-samiti consignment) QC result recorded
// at the plant lab (F7). Overall pass = every test passed; a pass auto-mints
// the batch QR, a fail rejects the consignment (no QR).
type ConsignmentQC struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"   json:"id"`
	ConsignmentID primitive.ObjectID `bson:"consignment_id"  json:"consignment_id"`
	BatchCode     string             `bson:"batch_code"      json:"batch_code"`
	Tests         []BatchQCTest      `bson:"tests"           json:"tests"`
	OverallPass   bool               `bson:"overall_pass"    json:"overall_pass"`
	// Verdict is PASS | HOLD | REJECT (§13.5). Empty on legacy documents — read
	// paths derive it from OverallPass via EffectiveQCVerdict. A HOLD keeps the
	// consignment ACCEPTED with qc_hold=true and may be re-tested.
	Verdict       string             `bson:"verdict,omitempty" json:"verdict,omitempty"`
	Notes         string             `bson:"notes,omitempty" json:"notes,omitempty"`
	TestedBy      primitive.ObjectID `bson:"tested_by"       json:"tested_by"`
	TestedAt      time.Time          `bson:"tested_at"       json:"tested_at"`
	// History keeps the prior HOLD attempts when the batch is re-tested — the
	// document stays one-per-consignment (unique index) but no attempt is lost.
	History       []ConsignmentQCAttempt `bson:"history,omitempty" json:"history,omitempty"`
	ProvenanceSeq int64              `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
}

// ConsignmentQCAttempt is one superseded (HOLD) test attempt retained on the
// consignment's QC document when the batch is re-tested.
type ConsignmentQCAttempt struct {
	Tests    []BatchQCTest      `bson:"tests"           json:"tests"`
	Verdict  string             `bson:"verdict"         json:"verdict"`
	Notes    string             `bson:"notes,omitempty" json:"notes,omitempty"`
	TestedBy primitive.ObjectID `bson:"tested_by"       json:"tested_by"`
	TestedAt time.Time          `bson:"tested_at"       json:"tested_at"`
}

// ConsignmentBatchQR is the public QR minted automatically when a per-samiti
// batch passes QC (F7): code == the consignment's batch_code, token is a
// short HMAC over the batch code (signed with the QR secret) so a scan can
// verify the record was minted by this server. Resolved publicly at
// GET /public/qr/{batch_code or token} into the batch quality report (F8).
type ConsignmentBatchQR struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"  json:"id"`
	ConsignmentID primitive.ObjectID `bson:"consignment_id" json:"consignment_id"`
	BatchCode     string             `bson:"batch_code"     json:"batch_code"`
	Token         string             `bson:"token"          json:"token"`
	IssuedAt      time.Time          `bson:"issued_at"      json:"issued_at"`
}

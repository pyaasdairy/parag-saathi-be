package domain

import (
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// FSSAI limits for liquid milk (blueprint §8.3). Aflatoxin M1 is heat-stable
// and survives pasteurisation, so it MUST be gated upstream (BMC + plant),
// never only at the finished product.
const (
	FSSAIAflatoxinM1MaxMicrogramPerKg = 0.5  // AFM1 ≤ 0.5 µg/kg
	FSSAIColiformMaxCFUPerMl          = 10.0 // coliform ≤ 10 CFU/ml
	FSSAITetracyclineMaxMgPerKg       = 0.1  // antibiotic MRL example
)

// QC test names.
const (
	TestAflatoxinM1 = "AFLATOXIN_M1"            // µg/kg
	TestColiform    = "COLIFORM"                // CFU/ml
	TestTPC         = "TPC"                     // CFU/ml, graded — recorded, not hard-gated here
	TestAntibiotic  = "ANTIBIOTIC_TETRACYCLINE" // mg/kg
	TestPhosphatase = "PHOSPHATASE"             // 0 = negative (pass post-pasteurisation), 1 = positive
	TestFat         = "FAT"                     // %
	TestSNF         = "SNF"                     // %
)

// QC stages.
const (
	QCStageBMCRapid = "BMC_RAPID" // strip/rapid tests at the chilling centre
	QCStagePlantLab = "PLANT_LAB" // full lab: ELISA/HPLC, culture, LC-MS/MS
)

// QC subject types.
const (
	QCSubjectDCSConsignment  = "DCS_CONSIGNMENT"
	QCSubjectBMCLot          = "BMC_LOT"
	QCSubjectProcessingBatch = "PROCESSING_BATCH"
)

// QC verdicts (Developer Note §6.5/§13.5): the Note's machine is
// SAMPLED→HOLD→PASS/REJECT. HOLD quarantines the subject pending a re-test or
// an analyst resolution — it is distinct from a terminal REJECT.
const (
	QCVerdictPass   = "PASS"
	QCVerdictHold   = "HOLD"
	QCVerdictReject = "REJECT"
)

// EffectiveQCVerdict returns the stored verdict, deriving PASS/REJECT from the
// legacy two-state overall_pass for results recorded before HOLD existed.
func EffectiveQCVerdict(stored string, overallPass bool) string {
	if stored != "" {
		return stored
	}
	if overallPass {
		return QCVerdictPass
	}
	return QCVerdictReject
}

// QCTest is a single measurement inside a QC result.
type QCTest struct {
	Name  string  `bson:"name"  json:"name"`
	Value float64 `bson:"value" json:"value"`
	Unit  string  `bson:"unit"  json:"unit"`
	Pass  bool    `bson:"pass"  json:"pass"`
}

// QCResult is the recorded outcome of testing one subject at one stage.
// OverallPass drives the safety gate; a certificate is attached to provenance
// on pass, a block + quarantine on fail.
type QCResult struct {
	ID                primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SubjectType       string             `bson:"subject_type"  json:"subject_type"`
	SubjectID         primitive.ObjectID `bson:"subject_id"    json:"subject_id"`
	Stage             string             `bson:"stage"         json:"stage"`
	Tests             []QCTest           `bson:"tests"         json:"tests"`
	OverallPass       bool               `bson:"overall_pass"  json:"overall_pass"`
	FailureReasons    []string           `bson:"failure_reasons,omitempty" json:"failure_reasons,omitempty"`
	AnalystPartyID    primitive.ObjectID `bson:"analyst_party_id" json:"analyst_party_id"`
	LabRef            string             `bson:"lab_ref,omitempty"            json:"lab_ref,omitempty"`
	CertificateNumber string             `bson:"certificate_number,omitempty" json:"certificate_number,omitempty"`
	// Verdict is PASS | HOLD | REJECT (§13.5). Empty on legacy documents —
	// read paths derive it from OverallPass via EffectiveQCVerdict.
	Verdict string `bson:"verdict,omitempty" json:"verdict,omitempty"`
	// HOLD resolution (POST /quality/qc-results/{id}/resolve): who resolved the
	// quarantine, when, and with what notes.
	ResolvedBy      *primitive.ObjectID `bson:"resolved_by,omitempty"      json:"resolved_by,omitempty"`
	ResolvedAt      *time.Time          `bson:"resolved_at,omitempty"      json:"resolved_at,omitempty"`
	ResolutionNotes string              `bson:"resolution_notes,omitempty" json:"resolution_notes,omitempty"`
	// Superseded marks a result voided after losing the gate race — the
	// verdict on the subject came from a different, earlier result.
	Superseded    bool      `bson:"superseded,omitempty" json:"superseded,omitempty"`
	RecordedAt    time.Time `bson:"recorded_at"   json:"recorded_at"`
	ProvenanceSeq int64     `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
}

// NormalizeTestName maps common field aliases to the canonical FSSAI test
// name so the safety gate can NEVER be bypassed by a spelling difference
// (e.g. the app sends "AFM1"; the gate keys on "AFLATOXIN_M1"). A test whose
// name still doesn't resolve to a gated parameter is recorded but not gated.
func NormalizeTestName(name string) string {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "AFM1", "AFLATOXIN", "AFLATOXINM1", "AFLATOXIN_M1", "AFLATOXIN-M1":
		return TestAflatoxinM1
	case "COLIFORM", "COLIFORMS":
		return TestColiform
	case "TPC", "TOTAL_PLATE_COUNT":
		return TestTPC
	case "ANTIBIOTIC", "TETRACYCLINE", "ANTIBIOTIC_TETRACYCLINE":
		return TestAntibiotic
	case "PHOSPHATASE", "ALP":
		return TestPhosphatase
	case "FAT":
		return TestFat
	case "SNF":
		return TestSNF
	default:
		return strings.ToUpper(strings.TrimSpace(name))
	}
}

// EvaluateQCTests applies the FSSAI gate to a set of tests: it normalizes each
// test name, fills its Pass verdict, and returns the overall verdict plus
// human-readable failure reasons. Unknown test names are recorded but do not
// gate. The returned tests carry the CANONICAL name so storage is consistent.
func EvaluateQCTests(tests []QCTest) (overallPass bool, failures []string, evaluated []QCTest) {
	overallPass = true
	evaluated = make([]QCTest, 0, len(tests))
	for _, t := range tests {
		t.Name = NormalizeTestName(t.Name) // canonicalise so the gate cannot be dodged by alias
		pass := true
		switch t.Name {
		case TestAflatoxinM1:
			if t.Value > FSSAIAflatoxinM1MaxMicrogramPerKg {
				pass = false
				failures = append(failures, fmt.Sprintf(
					"AFLATOXIN_M1 %.3f µg/kg exceeds FSSAI limit %.1f µg/kg", t.Value, FSSAIAflatoxinM1MaxMicrogramPerKg))
			}
		case TestColiform:
			if t.Value > FSSAIColiformMaxCFUPerMl {
				pass = false
				failures = append(failures, fmt.Sprintf(
					"COLIFORM %.1f CFU/ml exceeds FSSAI limit %.0f CFU/ml", t.Value, FSSAIColiformMaxCFUPerMl))
			}
		case TestAntibiotic:
			if t.Value > FSSAITetracyclineMaxMgPerKg {
				pass = false
				failures = append(failures, fmt.Sprintf(
					"ANTIBIOTIC_TETRACYCLINE %.3f mg/kg exceeds MRL %.1f mg/kg", t.Value, FSSAITetracyclineMaxMgPerKg))
			}
		case TestPhosphatase:
			if t.Value != 0 {
				pass = false
				failures = append(failures, "PHOSPHATASE positive — pasteurisation check failed (must be negative)")
			}
		}
		t.Pass = pass
		if !pass {
			overallPass = false
		}
		evaluated = append(evaluated, t)
	}
	return overallPass, failures, evaluated
}

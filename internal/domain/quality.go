package domain

import (
	"fmt"
	"time"
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
	TestAflatoxinM1  = "AFLATOXIN_M1"           // µg/kg
	TestColiform     = "COLIFORM"               // CFU/ml
	TestTPC          = "TPC"                    // CFU/ml, graded — recorded, not hard-gated here
	TestAntibiotic   = "ANTIBIOTIC_TETRACYCLINE" // mg/kg
	TestPhosphatase  = "PHOSPHATASE"            // 0 = negative (pass post-pasteurisation), 1 = positive
	TestFat          = "FAT"                    // %
	TestSNF          = "SNF"                    // %
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
	ID                string    `bson:"_id"          json:"id"`
	SubjectType       string    `bson:"subject_type" json:"subject_type"`
	SubjectID         string    `bson:"subject_id"   json:"subject_id"`
	Stage             string    `bson:"stage"        json:"stage"`
	Tests             []QCTest  `bson:"tests"        json:"tests"`
	OverallPass       bool      `bson:"overall_pass" json:"overall_pass"`
	FailureReasons    []string  `bson:"failure_reasons,omitempty" json:"failure_reasons,omitempty"`
	AnalystPartyID    string    `bson:"analyst_party_id" json:"analyst_party_id"`
	LabRef            string    `bson:"lab_ref,omitempty"            json:"lab_ref,omitempty"`
	CertificateNumber string    `bson:"certificate_number,omitempty" json:"certificate_number,omitempty"`
	// Superseded marks a result voided after losing the gate race — the
	// verdict on the subject came from a different, earlier result.
	Superseded bool      `bson:"superseded,omitempty" json:"superseded,omitempty"`
	RecordedAt time.Time `bson:"recorded_at"  json:"recorded_at"`
	ProvenanceSeq     int64     `bson:"provenance_seq,omitempty" json:"provenance_seq,omitempty"`
}

// EvaluateQCTests applies the FSSAI gate to a set of tests: it fills each
// test's Pass verdict and returns the overall verdict plus human-readable
// failure reasons. Unknown test names are recorded but do not gate.
func EvaluateQCTests(tests []QCTest) (overallPass bool, failures []string, evaluated []QCTest) {
	overallPass = true
	evaluated = make([]QCTest, 0, len(tests))
	for _, t := range tests {
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

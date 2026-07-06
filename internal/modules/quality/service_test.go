package quality

import (
	"strings"
	"testing"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// TestEvaluateQCTests exercises the full FSSAI gate matrix (blueprint §8.3):
// boundary behaviour at each limit, phosphatase positivity, non-gating tests,
// and multi-test mixed failures.
func TestEvaluateQCTests(t *testing.T) {
	cases := []struct {
		name          string
		tests         []domain.QCTest
		wantPass      bool
		wantFailures  int
		wantTestPass  []bool   // per-test verdicts, aligned with tests
		wantReasonSub []string // substrings that must appear in the joined reasons
	}{
		{
			name:         "AFM1 exactly at 0.5 limit passes (limit is inclusive)",
			tests:        []domain.QCTest{{Name: domain.TestAflatoxinM1, Value: 0.5, Unit: "µg/kg"}},
			wantPass:     true,
			wantTestPass: []bool{true},
		},
		{
			name:          "AFM1 just above 0.5 fails",
			tests:         []domain.QCTest{{Name: domain.TestAflatoxinM1, Value: 0.501, Unit: "µg/kg"}},
			wantPass:      false,
			wantFailures:  1,
			wantTestPass:  []bool{false},
			wantReasonSub: []string{"AFLATOXIN_M1"},
		},
		{
			name:         "AFM1 well below limit passes",
			tests:        []domain.QCTest{{Name: domain.TestAflatoxinM1, Value: 0.02, Unit: "µg/kg"}},
			wantPass:     true,
			wantTestPass: []bool{true},
		},
		{
			name:         "coliform exactly at 10 CFU/ml passes (limit is inclusive)",
			tests:        []domain.QCTest{{Name: domain.TestColiform, Value: 10, Unit: "CFU/ml"}},
			wantPass:     true,
			wantTestPass: []bool{true},
		},
		{
			name:          "coliform just above 10 fails",
			tests:         []domain.QCTest{{Name: domain.TestColiform, Value: 10.1, Unit: "CFU/ml"}},
			wantPass:      false,
			wantFailures:  1,
			wantTestPass:  []bool{false},
			wantReasonSub: []string{"COLIFORM"},
		},
		{
			name:         "tetracycline exactly at 0.1 MRL passes",
			tests:        []domain.QCTest{{Name: domain.TestAntibiotic, Value: 0.1, Unit: "mg/kg"}},
			wantPass:     true,
			wantTestPass: []bool{true},
		},
		{
			name:          "tetracycline above MRL fails",
			tests:         []domain.QCTest{{Name: domain.TestAntibiotic, Value: 0.11, Unit: "mg/kg"}},
			wantPass:      false,
			wantFailures:  1,
			wantTestPass:  []bool{false},
			wantReasonSub: []string{"ANTIBIOTIC_TETRACYCLINE"},
		},
		{
			name:         "phosphatase negative (0) passes",
			tests:        []domain.QCTest{{Name: domain.TestPhosphatase, Value: 0, Unit: ""}},
			wantPass:     true,
			wantTestPass: []bool{true},
		},
		{
			name:          "phosphatase positive (1) fails",
			tests:         []domain.QCTest{{Name: domain.TestPhosphatase, Value: 1, Unit: ""}},
			wantPass:      false,
			wantFailures:  1,
			wantTestPass:  []bool{false},
			wantReasonSub: []string{"PHOSPHATASE"},
		},
		{
			name:          "phosphatase any non-zero value fails",
			tests:         []domain.QCTest{{Name: domain.TestPhosphatase, Value: 0.5, Unit: ""}},
			wantPass:      false,
			wantFailures:  1,
			wantTestPass:  []bool{false},
			wantReasonSub: []string{"PHOSPHATASE"},
		},
		{
			name: "unknown / graded tests are recorded but never gate",
			tests: []domain.QCTest{
				{Name: domain.TestFat, Value: 99, Unit: "%"},
				{Name: domain.TestSNF, Value: 99, Unit: "%"},
				{Name: domain.TestTPC, Value: 1e9, Unit: "CFU/ml"},
			},
			wantPass:     true,
			wantTestPass: []bool{true, true, true},
		},
		{
			name: "multi-test mixed failure collects every reason",
			tests: []domain.QCTest{
				{Name: domain.TestAflatoxinM1, Value: 0.9, Unit: "µg/kg"},
				{Name: domain.TestColiform, Value: 5, Unit: "CFU/ml"},
				{Name: domain.TestPhosphatase, Value: 1, Unit: ""},
				{Name: domain.TestFat, Value: 4.1, Unit: "%"},
			},
			wantPass:      false,
			wantFailures:  2,
			wantTestPass:  []bool{false, true, false, true},
			wantReasonSub: []string{"AFLATOXIN_M1", "PHOSPHATASE"},
		},
		{
			name:         "empty test list evaluates to pass (rejected earlier by the handler)",
			tests:        nil,
			wantPass:     true,
			wantTestPass: []bool{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pass, failures, evaluated := domain.EvaluateQCTests(tc.tests)

			if pass != tc.wantPass {
				t.Fatalf("overall pass = %v, want %v (failures: %v)", pass, tc.wantPass, failures)
			}
			if len(failures) != tc.wantFailures {
				t.Fatalf("got %d failure reasons %v, want %d", len(failures), failures, tc.wantFailures)
			}
			if len(evaluated) != len(tc.tests) {
				t.Fatalf("evaluated %d tests, want %d", len(evaluated), len(tc.tests))
			}
			for i, want := range tc.wantTestPass {
				if evaluated[i].Pass != want {
					t.Errorf("test %s Pass = %v, want %v", evaluated[i].Name, evaluated[i].Pass, want)
				}
			}
			joined := strings.Join(failures, "; ")
			for _, sub := range tc.wantReasonSub {
				if !strings.Contains(joined, sub) {
					t.Errorf("failure reasons %q missing substring %q", joined, sub)
				}
			}
		})
	}
}

// TestRequiredRoleForStage pins the stage×role pairing the service enforces.
func TestRequiredRoleForStage(t *testing.T) {
	cases := []struct {
		stage    string
		wantRole string
		wantOK   bool
	}{
		{domain.QCStageBMCRapid, domain.RoleBMCOperator, true},
		{domain.QCStagePlantLab, domain.RolePlantLabAnalyst, true},
		{"FIELD_TEST", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		role, ok := requiredRoleForStage(tc.stage)
		if role != tc.wantRole || ok != tc.wantOK {
			t.Errorf("requiredRoleForStage(%q) = (%q, %v), want (%q, %v)", tc.stage, role, ok, tc.wantRole, tc.wantOK)
		}
	}
}

// TestValidateTestNames pins the closed test vocabulary: a typo'd gated test
// ("AFLATOXIN_M") must be rejected, never silently recorded as non-gating.
func TestValidateTestNames(t *testing.T) {
	if err := validateTestNames([]QCTestInput{
		{Name: domain.TestAflatoxinM1, Value: 0.2},
		{Name: domain.TestFat, Value: 4.1},
	}); err != nil {
		t.Fatalf("known tests must validate, got %v", err)
	}
	if err := validateTestNames([]QCTestInput{{Name: "AFLATOXIN_M", Value: 99}}); err == nil {
		t.Fatal("typo'd test name must be rejected")
	}
	if err := validateTestNames([]QCTestInput{{Name: "MOISTURE", Value: 1}}); err == nil {
		t.Fatal("unknown test name must be rejected")
	}
}

// TestMissingMandatoryTests pins the per-stage mandatory set (§8.3): the gate
// cannot be PASSED without the heat-stable AFM1 measurement (and companions).
func TestMissingMandatoryTests(t *testing.T) {
	// Full BMC_RAPID submission → nothing missing.
	if missing := missingMandatoryTests(domain.QCStageBMCRapid, []QCTestInput{
		{Name: domain.TestAflatoxinM1}, {Name: domain.TestColiform},
	}); len(missing) != 0 {
		t.Fatalf("complete BMC_RAPID submission reported missing: %v", missing)
	}
	// FAT alone must NOT satisfy the BMC gate.
	missing := missingMandatoryTests(domain.QCStageBMCRapid, []QCTestInput{{Name: domain.TestFat}})
	if len(missing) != 2 {
		t.Fatalf("FAT-only BMC_RAPID submission missing = %v, want AFM1+coliform", missing)
	}
	// PLANT_LAB additionally requires the pasteurisation (phosphatase) proof.
	missing = missingMandatoryTests(domain.QCStagePlantLab, []QCTestInput{
		{Name: domain.TestAflatoxinM1}, {Name: domain.TestColiform},
	})
	if len(missing) != 1 || missing[0] != domain.TestPhosphatase {
		t.Fatalf("PLANT_LAB without phosphatase missing = %v, want [PHOSPHATASE]", missing)
	}
}

// TestCertificateNumber pins the certificate format "QC-"+stage+"-"+seq.
func TestCertificateNumber(t *testing.T) {
	got := certificateNumber(domain.QCStagePlantLab, 42)
	if got != "QC-PLANT_LAB-000042" {
		t.Errorf("certificateNumber = %q, want QC-PLANT_LAB-000042", got)
	}
	got = certificateNumber(domain.QCStageBMCRapid, 1)
	if got != "QC-BMC_RAPID-000001" {
		t.Errorf("certificateNumber = %q, want QC-BMC_RAPID-000001", got)
	}
}

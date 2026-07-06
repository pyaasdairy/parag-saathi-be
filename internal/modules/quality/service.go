package quality

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

// service holds the safety-gate business logic (blueprint §8.3): evaluate
// tests against FSSAI limits, write the verdict onto the subject, chain the
// provenance events, and broadcast blocks to downstream modules.
type service struct {
	deps *deps.Deps
	repo *repository
}

// newService wires the service.
func newService(d *deps.Deps, repo *repository) *service {
	return &service{deps: d, repo: repo}
}

// gateSubject is what the gate needs to know about an eligible subject.
type gateSubject struct {
	entityType string // provenance entity const for the subject
	orgUnitID  string // owning org unit (BMC or plant) for scope + ledger
	status     string
}

// requiredRoleForStage returns the one role allowed to record results at a
// stage (BMC_RAPID↔BMC_OPERATOR, PLANT_LAB↔PLANT_LAB_ANALYST), and whether
// the stage is known at all.
func requiredRoleForStage(stage string) (string, bool) {
	switch stage {
	case domain.QCStageBMCRapid:
		return domain.RoleBMCOperator, true
	case domain.QCStagePlantLab:
		return domain.RolePlantLabAnalyst, true
	default:
		return "", false
	}
}

// certificateNumber builds the human-readable QC certificate identifier from
// the per-stage counter, e.g. "QC-PLANT_LAB-000042".
func certificateNumber(stage string, seq int64) string {
	return fmt.Sprintf("QC-%s-%06d", stage, seq)
}

// knownQCTests is the closed vocabulary of recordable test names: the four
// gated FSSAI tests plus the explicitly whitelisted grading tests. Anything
// else (including a typo like "AFLATOXIN_M") is rejected instead of being
// silently recorded as a non-gating pass.
var knownQCTests = map[string]struct{}{
	domain.TestAflatoxinM1: {},
	domain.TestColiform:    {},
	domain.TestAntibiotic:  {},
	domain.TestPhosphatase: {},
	domain.TestTPC:         {},
	domain.TestFat:         {},
	domain.TestSNF:         {},
}

// mandatoryTestsForStage returns the tests a stage MUST measure before a PASS
// verdict may be issued (blueprint §8.3: heat-stable aflatoxin M1 is gated at
// BMC and plant; phosphatase proves pasteurisation at the plant lab).
func mandatoryTestsForStage(stage string) []string {
	switch stage {
	case domain.QCStageBMCRapid:
		return []string{domain.TestAflatoxinM1, domain.TestColiform}
	case domain.QCStagePlantLab:
		return []string{domain.TestAflatoxinM1, domain.TestColiform, domain.TestPhosphatase}
	default:
		return nil
	}
}

// validateTestNames rejects any test outside the known vocabulary.
func validateTestNames(tests []QCTestInput) *httpx.AppError {
	for _, t := range tests {
		if _, ok := knownQCTests[t.Name]; !ok {
			return httpx.BadRequest("UNKNOWN_TEST",
				"unknown test name "+t.Name+" — gated tests: "+domain.TestAflatoxinM1+", "+
					domain.TestColiform+", "+domain.TestAntibiotic+", "+domain.TestPhosphatase)
		}
	}
	return nil
}

// missingMandatoryTests lists the stage's mandatory tests absent from the
// submission.
func missingMandatoryTests(stage string, tests []QCTestInput) []string {
	present := make(map[string]bool, len(tests))
	for _, t := range tests {
		present[t.Name] = true
	}
	var missing []string
	for _, name := range mandatoryTestsForStage(stage) {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// recordQCResult evaluates the submitted tests against the FSSAI gate,
// persists the result, applies the PASSED/BLOCKED verdict to the subject and
// chains the provenance events. This is the only write path that moves a
// subject out of QC_PENDING.
func (s *service) recordQCResult(ctx context.Context, actor auth.Actor, req RecordQCResultRequest) (*domain.QCResult, error) {
	// Stage×role pairing — RequireRoles admits both analyst roles, the exact
	// pairing is enforced here.
	requiredRole, ok := requiredRoleForStage(req.Stage)
	if !ok {
		return nil, httpx.BadRequest("INVALID_STAGE", "stage must be "+domain.QCStageBMCRapid+" or "+domain.QCStagePlantLab)
	}
	if actor.RoleCode != requiredRole {
		return nil, httpx.Forbidden("STAGE_ROLE_MISMATCH: stage " + req.Stage + " results may only be recorded by " + requiredRole)
	}

	subject, err := s.loadGateSubject(ctx, req.SubjectType, req.SubjectID)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, subject.orgUnitID); err != nil {
		return nil, err
	}

	// Closed test vocabulary: a typo'd or irrelevant test name must never be
	// able to satisfy the safety gate.
	if aerr := validateTestNames(req.Tests); aerr != nil {
		return nil, aerr
	}

	// Evaluate against the FSSAI limits (domain owns the rules).
	overallPass, failures, evaluated := domain.EvaluateQCTests(toDomainTests(req.Tests))

	// A PASS verdict requires every mandatory test of the stage to be present
	// AND within limits — the gate cannot be satisfied by a contentless
	// submission (blueprint §8.3). A failing partial submission still records
	// its BLOCK: quarantining on partial evidence is safety-conservative.
	if missing := missingMandatoryTests(req.Stage, req.Tests); len(missing) > 0 && overallPass {
		return nil, httpx.Unprocessable("MISSING_MANDATORY_TESTS",
			"stage "+req.Stage+" cannot PASS without its mandatory tests").
			WithDetails(map[string]any{"missing_tests": missing})
	}

	resultID := uuid.NewString()
	now := time.Now().UTC()

	result := &domain.QCResult{
		ID:             resultID,
		SubjectType:    req.SubjectType,
		SubjectID:      req.SubjectID,
		Stage:          req.Stage,
		Tests:          evaluated,
		OverallPass:    overallPass,
		AnalystPartyID: actor.PartyID,
		LabRef:         req.LabRef,
		RecordedAt:     now,
	}

	// The write sequence below must not be aborted midway by a client
	// disconnect: detach from the request context, keep a server-side bound.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	blockReason := ""
	if overallPass {
		seq, err := s.repo.nextCertificateSeq(wctx, req.Stage)
		if err != nil {
			return nil, err
		}
		result.CertificateNumber = certificateNumber(req.Stage, seq)
	} else {
		result.FailureReasons = failures
		blockReason = strings.Join(failures, "; ")
	}

	// Ordering is safety-critical: evidence (qc_results doc) and provenance
	// (hash-chained events) are persisted FIRST; the irreversible status flip
	// on the subject happens LAST. A crash mid-sequence can leave an extra
	// auditable result document, never a PASSED lot without evidence.
	if err := s.repo.insertResult(wctx, result); err != nil {
		return nil, err
	}

	// Provenance: qc.recorded, then the gate verdict event (blueprint §7).
	ledgerActor := provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode}
	recorded, err := s.deps.Ledger.Append(wctx, provenance.AppendInput{
		Type:       domain.EventQCRecorded,
		EntityType: domain.EntityQCResult,
		EntityID:   resultID,
		Refs: []provenance.Ref{
			{EntityType: subject.entityType, EntityID: req.SubjectID, Relation: "tested"},
		},
		Actor:     ledgerActor,
		OrgUnitID: subject.orgUnitID,
		Payload: map[string]any{
			"stage":        req.Stage,
			"subject_type": req.SubjectType,
			"overall_pass": overallPass,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}

	gateEvent := provenance.AppendInput{
		EntityType: domain.EntityQCResult,
		EntityID:   resultID,
		Actor:      ledgerActor,
		OrgUnitID:  subject.orgUnitID,
	}
	if overallPass {
		gateEvent.Type = domain.EventGatePassed
		gateEvent.Refs = []provenance.Ref{
			{EntityType: subject.entityType, EntityID: req.SubjectID, Relation: "certifies"},
		}
		gateEvent.Payload = map[string]any{"certificate_number": result.CertificateNumber}
	} else {
		gateEvent.Type = domain.EventGateBlocked
		gateEvent.Refs = []provenance.Ref{
			{EntityType: subject.entityType, EntityID: req.SubjectID, Relation: "blocks"},
		}
		gateEvent.Payload = map[string]any{"failure_reasons": failures}
	}
	if _, err := s.deps.Ledger.Append(wctx, gateEvent); err != nil {
		return nil, httpx.Internal(err)
	}

	if err := s.repo.setResultProvenanceSeq(wctx, resultID, recorded.Seq); err != nil {
		return nil, err
	}
	result.ProvenanceSeq = recorded.Seq

	// LAST: apply the verdict. The conditional QC_PENDING filter remains the
	// concurrency guard — a lost race voids OUR result (dangling evidence is
	// auditable and harmless, unlike a dangling status flip).
	gated, err := s.applyGate(wctx, req.SubjectType, req.SubjectID, resultID, overallPass, blockReason)
	if err != nil {
		return nil, err
	}
	if !gated {
		if err := s.repo.markResultSuperseded(wctx, resultID); err != nil {
			return nil, err
		}
		return nil, httpx.Conflict("SUBJECT_ALREADY_GATED", "subject is no longer awaiting QC — a verdict was already recorded")
	}

	// Broadcast the block so the plant module quarantines downstream — a
	// blocked lot can never advance.
	if !overallPass {
		s.deps.Bus.Publish(eventbus.TopicGateBlocked, GateBlockedPayload{
			SubjectType:    req.SubjectType,
			SubjectID:      req.SubjectID,
			QCResultID:     resultID,
			Stage:          req.Stage,
			FailureReasons: failures,
		})
	}
	return result, nil
}

// loadGateSubject fetches a gate-eligible subject and verifies it is awaiting
// QC. Only BMC lots and processing batches carry the software safety gate.
func (s *service) loadGateSubject(ctx context.Context, subjectType, subjectID string) (gateSubject, error) {
	switch subjectType {
	case domain.QCSubjectBMCLot:
		lot, err := s.repo.findBMCLot(ctx, subjectID)
		if err != nil {
			return gateSubject{}, err
		}
		if lot.Status != domain.BMCLotStatusQCPending {
			return gateSubject{}, httpx.Conflict("SUBJECT_NOT_QC_PENDING", "bmc lot is in status "+lot.Status+" — only QC_PENDING subjects can be gated")
		}
		return gateSubject{entityType: domain.EntityBMCLot, orgUnitID: lot.BMCID, status: lot.Status}, nil

	case domain.QCSubjectProcessingBatch:
		batch, err := s.repo.findBatch(ctx, subjectID)
		if err != nil {
			return gateSubject{}, err
		}
		if batch.Status != domain.BatchStatusQCPending {
			return gateSubject{}, httpx.Conflict("SUBJECT_NOT_QC_PENDING", "processing batch is in status "+batch.Status+" — only QC_PENDING subjects can be gated")
		}
		return gateSubject{entityType: domain.EntityBatch, orgUnitID: batch.PlantID, status: batch.Status}, nil

	case domain.QCSubjectDCSConsignment:
		return gateSubject{}, httpx.Unprocessable("SUBJECT_NOT_GATE_ELIGIBLE", "DCS consignments are not gate subjects — gate at the BMC lot or processing batch")

	default:
		return gateSubject{}, httpx.BadRequest("INVALID_SUBJECT_TYPE", "subject_type must be one of "+domain.QCSubjectBMCLot+", "+domain.QCSubjectProcessingBatch)
	}
}

// applyGate routes the conditional verdict write to the subject's collection,
// mapping the verdict onto that subject kind's status constants.
func (s *service) applyGate(ctx context.Context, subjectType, subjectID, resultID string, pass bool, blockReason string) (bool, error) {
	switch subjectType {
	case domain.QCSubjectBMCLot:
		status := domain.BMCLotStatusBlocked
		if pass {
			status = domain.BMCLotStatusPassed
		}
		return s.repo.gateBMCLot(ctx, subjectID, resultID, status, blockReason)
	case domain.QCSubjectProcessingBatch:
		status := domain.BatchStatusBlocked
		if pass {
			status = domain.BatchStatusPassed
		}
		return s.repo.gateBatch(ctx, subjectID, resultID, status, blockReason)
	default:
		return false, httpx.BadRequest("INVALID_SUBJECT_TYPE", "subject_type is not gate-eligible")
	}
}

// listQCResults returns a page of results, newest first. When the filter
// identifies an org-owned subject, the caller must be in that org's scope.
func (s *service) listQCResults(ctx context.Context, actor auth.Actor, subjectType, subjectID string, page httpx.Page) ([]domain.QCResult, int64, error) {
	if subjectType != "" && !isValidSubjectType(subjectType) {
		return nil, 0, httpx.BadRequest("INVALID_SUBJECT_TYPE", "unknown subject_type "+subjectType)
	}
	if subjectID != "" && subjectType == "" {
		return nil, 0, httpx.BadRequest("MISSING_SUBJECT_TYPE", "subject_id requires subject_type")
	}
	if subjectID != "" {
		if err := s.requireSubjectScope(ctx, actor, subjectType, subjectID); err != nil {
			return nil, 0, err
		}
	}
	return s.repo.listResults(ctx, subjectType, subjectID, page)
}

// getQCResult returns one result (its tests carry the stored pass verdicts),
// enforcing the subject org scope.
func (s *service) getQCResult(ctx context.Context, actor auth.Actor, resultID string) (*domain.QCResult, error) {
	result, err := s.repo.findResultByID(ctx, resultID)
	if err != nil {
		return nil, err
	}
	if err := s.requireSubjectScope(ctx, actor, result.SubjectType, result.SubjectID); err != nil {
		return nil, err
	}
	return result, nil
}

// requireSubjectScope enforces the caller's org scope over the org unit that
// owns a QC subject. Subjects that no longer resolve (or subject types that
// carry no org) skip the check — the role gate still applies.
func (s *service) requireSubjectScope(ctx context.Context, actor auth.Actor, subjectType, subjectID string) error {
	orgID, err := s.subjectOrgUnit(ctx, subjectType, subjectID)
	if err != nil {
		return err
	}
	if orgID == "" {
		return nil
	}
	return s.deps.Orgs.RequireInScope(ctx, actor, orgID)
}

// subjectOrgUnit resolves the owning org unit of a subject, or "" when the
// subject is missing or not an org-owned gate subject.
func (s *service) subjectOrgUnit(ctx context.Context, subjectType, subjectID string) (string, error) {
	switch subjectType {
	case domain.QCSubjectBMCLot:
		lot, err := s.repo.findBMCLot(ctx, subjectID)
		if err != nil {
			return "", ignoreNotFound(err)
		}
		return lot.BMCID, nil
	case domain.QCSubjectProcessingBatch:
		batch, err := s.repo.findBatch(ctx, subjectID)
		if err != nil {
			return "", ignoreNotFound(err)
		}
		return batch.PlantID, nil
	default:
		return "", nil
	}
}

// ignoreNotFound swallows 404 AppErrors and passes everything else through.
func ignoreNotFound(err error) error {
	var appErr *httpx.AppError
	if errors.As(err, &appErr) && appErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// isValidSubjectType reports whether t is a known QC subject type.
func isValidSubjectType(t string) bool {
	switch t {
	case domain.QCSubjectDCSConsignment, domain.QCSubjectBMCLot, domain.QCSubjectProcessingBatch:
		return true
	default:
		return false
	}
}

// toDomainTests converts request test inputs to domain tests for evaluation.
func toDomainTests(in []QCTestInput) []domain.QCTest {
	out := make([]domain.QCTest, 0, len(in))
	for _, t := range in {
		out = append(out, domain.QCTest{Name: t.Name, Value: t.Value, Unit: t.Unit})
	}
	return out
}

// limits returns the FSSAI gate constants (blueprint §8.3) as reference data.
func (s *service) limits() LimitsResponse {
	return LimitsResponse{
		Authority: "FSSAI",
		Limits: []QCLimit{
			{
				TestName:    domain.TestAflatoxinM1,
				Limit:       domain.FSSAIAflatoxinM1MaxMicrogramPerKg,
				Unit:        "µg/kg",
				Comparison:  "MAX",
				Description: "Aflatoxin M1 is heat-stable and survives pasteurisation — gated upstream at BMC and plant, never only at the finished product",
			},
			{
				TestName:    domain.TestColiform,
				Limit:       domain.FSSAIColiformMaxCFUPerMl,
				Unit:        "CFU/ml",
				Comparison:  "MAX",
				Description: "Coliform count indicates hygiene failure in collection or chilling",
			},
			{
				TestName:    domain.TestAntibiotic,
				Limit:       domain.FSSAITetracyclineMaxMgPerKg,
				Unit:        "mg/kg",
				Comparison:  "MAX",
				Description: "Tetracycline maximum residue limit — antibiotic withdrawal-period violations",
			},
			{
				TestName:    domain.TestPhosphatase,
				Limit:       0,
				Unit:        "0 = negative, 1 = positive",
				Comparison:  "MUST_BE_NEGATIVE",
				Description: "Alkaline phosphatase must be negative after pasteurisation — a positive result means under-pasteurised milk",
			},
		},
	}
}

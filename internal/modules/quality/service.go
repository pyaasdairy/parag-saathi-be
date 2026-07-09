package quality

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

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
	log  *slog.Logger
}

// newService wires the service with its module-scoped logger.
func newService(d *deps.Deps, repo *repository, log *slog.Logger) *service {
	return &service{deps: d, repo: repo, log: log}
}

// actorID parses the actor's party ObjectID out of its JWT hex string.
func actorID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// gateSubject is what the gate needs to know about an eligible subject.
type gateSubject struct {
	entityType string             // provenance entity const for the subject
	orgUnitID  primitive.ObjectID // owning org unit (BMC or plant) for scope + ledger
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
	// Canonicalise test names FIRST — before any gate guard runs — so an alias
	// like "AFM1" resolves to "AFLATOXIN_M1" and can never dodge the aflatoxin
	// gate (safety-critical §8.3). All three decision points below then key on
	// the canonical name.
	for i := range req.Tests {
		req.Tests[i].Name = domain.NormalizeTestName(req.Tests[i].Name)
	}

	// Stage×role pairing — RequireRoles admits both analyst roles, the exact
	// pairing is enforced here.
	requiredRole, ok := requiredRoleForStage(req.Stage)
	if !ok {
		return nil, httpx.BadRequest("INVALID_STAGE", "stage must be "+domain.QCStageBMCRapid+" or "+domain.QCStagePlantLab)
	}
	if actor.RoleCode != requiredRole {
		s.log.WarnContext(ctx, "qc stage-role mismatch rejected",
			slog.String("stage", req.Stage),
			slog.String("required_role", requiredRole),
			slog.String("actor_role", actor.RoleCode),
			slog.String("actor_party_id", actor.PartyID),
			slog.String("subject_type", req.SubjectType),
			slog.String("subject_id", req.SubjectID.Hex()))
		return nil, httpx.Forbidden("STAGE_ROLE_MISMATCH: stage " + req.Stage + " results may only be recorded by " + requiredRole)
	}

	analystID, err := actorID(actor)
	if err != nil {
		return nil, err
	}

	subject, err := s.loadGateSubject(ctx, req.SubjectType, req.SubjectID)
	if err != nil {
		var appErr *httpx.AppError
		if errors.As(err, &appErr) && appErr.Status == http.StatusConflict {
			s.log.WarnContext(ctx, "qc subject not awaiting verdict",
				slog.String("subject_type", req.SubjectType),
				slog.String("subject_id", req.SubjectID.Hex()),
				slog.String("actor_party_id", actor.PartyID),
				slog.String("reason", appErr.Error()))
		}
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, subject.orgUnitID); err != nil {
		s.log.WarnContext(ctx, "qc scope denied",
			slog.String("subject_type", req.SubjectType),
			slog.String("subject_id", req.SubjectID.Hex()),
			slog.String("subject_org_unit_id", subject.orgUnitID.Hex()),
			slog.String("actor_party_id", actor.PartyID),
			slog.Any("err", err))
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
		s.log.WarnContext(ctx, "qc pass rejected: mandatory tests missing",
			slog.String("stage", req.Stage),
			slog.String("subject_type", req.SubjectType),
			slog.String("subject_id", req.SubjectID.Hex()),
			slog.Any("missing_tests", missing),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("MISSING_MANDATORY_TESTS",
			"stage "+req.Stage+" cannot PASS without its mandatory tests").
			WithDetails(map[string]any{"missing_tests": missing})
	}

	// Pre-generate the result ID so the insert, the provenance refs and the
	// gate write all share one consistent identifier.
	resultID := primitive.NewObjectID()
	now := time.Now().UTC()

	result := &domain.QCResult{
		ID:             resultID,
		SubjectType:    req.SubjectType,
		SubjectID:      req.SubjectID,
		Stage:          req.Stage,
		Tests:          evaluated,
		OverallPass:    overallPass,
		AnalystPartyID: analystID,
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
			s.logInternal(ctx, "issue qc certificate seq", req, err)
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
		s.logInternal(ctx, "insert qc result", req, err)
		return nil, err
	}

	// Provenance: qc.recorded, then the gate verdict event (blueprint §7).
	ledgerActor := provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode}
	recorded, err := s.deps.Ledger.Append(wctx, provenance.AppendInput{
		Type:       domain.EventQCRecorded,
		EntityType: domain.EntityQCResult,
		EntityID:   resultID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: subject.entityType, EntityID: req.SubjectID.Hex(), Relation: "tested"},
		},
		Actor:     ledgerActor,
		OrgUnitID: subject.orgUnitID.Hex(),
		Payload: map[string]any{
			"stage":        req.Stage,
			"subject_type": req.SubjectType,
			"overall_pass": overallPass,
		},
	})
	if err != nil {
		s.logInternal(ctx, "append qc.recorded provenance event", req, err)
		return nil, httpx.Internal(err)
	}

	gateEvent := provenance.AppendInput{
		EntityType: domain.EntityQCResult,
		EntityID:   resultID.Hex(),
		Actor:      ledgerActor,
		OrgUnitID:  subject.orgUnitID.Hex(),
	}
	if overallPass {
		gateEvent.Type = domain.EventGatePassed
		gateEvent.Refs = []provenance.Ref{
			{EntityType: subject.entityType, EntityID: req.SubjectID.Hex(), Relation: "certifies"},
		}
		gateEvent.Payload = map[string]any{"certificate_number": result.CertificateNumber}
	} else {
		gateEvent.Type = domain.EventGateBlocked
		gateEvent.Refs = []provenance.Ref{
			{EntityType: subject.entityType, EntityID: req.SubjectID.Hex(), Relation: "blocks"},
		}
		gateEvent.Payload = map[string]any{"failure_reasons": failures}
	}
	if _, err := s.deps.Ledger.Append(wctx, gateEvent); err != nil {
		s.logInternal(ctx, "append gate verdict provenance event", req, err)
		return nil, httpx.Internal(err)
	}

	if err := s.repo.setResultProvenanceSeq(wctx, resultID, recorded.Seq); err != nil {
		s.logInternal(ctx, "stamp qc result provenance seq", req, err)
		return nil, err
	}
	result.ProvenanceSeq = recorded.Seq

	// LAST: apply the verdict. The conditional QC_PENDING filter remains the
	// concurrency guard — a lost race voids OUR result (dangling evidence is
	// auditable and harmless, unlike a dangling status flip).
	gated, err := s.applyGate(wctx, req.SubjectType, req.SubjectID, resultID, overallPass, blockReason)
	if err != nil {
		s.logInternal(ctx, "apply gate verdict", req, err)
		return nil, err
	}
	if !gated {
		if err := s.repo.markResultSuperseded(wctx, resultID); err != nil {
			s.logInternal(ctx, "mark qc result superseded", req, err)
			return nil, err
		}
		s.log.WarnContext(ctx, "qc gate race lost: result superseded",
			slog.String("qc_result_id", resultID.Hex()),
			slog.String("subject_type", req.SubjectType),
			slog.String("subject_id", req.SubjectID.Hex()),
			slog.String("stage", req.Stage),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("SUBJECT_ALREADY_GATED", "subject is no longer awaiting QC — a verdict was already recorded")
	}

	// A QC verdict (pass OR fail) removes the subject from the lab's live queue,
	// so nudge any open quality dashboards to re-fetch. Fire-and-forget bus event
	// bridged to the SSE "quality.changed" topic.
	s.deps.Bus.Publish(eventbus.TopicQCRecorded, QCRecordedPayload{
		SubjectType: req.SubjectType,
		SubjectID:   req.SubjectID.Hex(),
		Stage:       req.Stage,
		OverallPass: overallPass,
	})

	if overallPass {
		s.log.InfoContext(ctx, "qc gate passed",
			slog.String("qc_result_id", resultID.Hex()),
			slog.String("subject_type", req.SubjectType),
			slog.String("subject_id", req.SubjectID.Hex()),
			slog.String("stage", req.Stage),
			slog.String("certificate_number", result.CertificateNumber),
			slog.String("actor_party_id", actor.PartyID))
	} else {
		// Gate block is a WARN: the subject is quarantined and can never advance.
		s.log.WarnContext(ctx, "qc gate blocked: subject quarantined",
			slog.String("qc_result_id", resultID.Hex()),
			slog.String("subject_type", req.SubjectType),
			slog.String("subject_id", req.SubjectID.Hex()),
			slog.String("stage", req.Stage),
			slog.Any("failure_reasons", failures),
			slog.String("actor_party_id", actor.PartyID))
		// Broadcast the block so the plant module quarantines downstream — a
		// blocked lot can never advance.
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

// logInternal records an unexpected failure with the operation and subject
// context before the error surfaces as a 500.
func (s *service) logInternal(ctx context.Context, op string, req RecordQCResultRequest, err error) {
	s.log.ErrorContext(ctx, "qc record failed: "+op,
		slog.String("subject_type", req.SubjectType),
		slog.String("subject_id", req.SubjectID.Hex()),
		slog.String("stage", req.Stage),
		slog.Any("err", err))
}

// loadGateSubject fetches a gate-eligible subject and verifies it is awaiting
// QC. Only BMC lots and processing batches carry the software safety gate.
func (s *service) loadGateSubject(ctx context.Context, subjectType string, subjectID primitive.ObjectID) (gateSubject, error) {
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
func (s *service) applyGate(ctx context.Context, subjectType string, subjectID, resultID primitive.ObjectID, pass bool, blockReason string) (bool, error) {
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
// A zero subjectID means "no subject filter".
func (s *service) listQCResults(ctx context.Context, actor auth.Actor, subjectType string, subjectID primitive.ObjectID, page httpx.Page) ([]domain.QCResult, int64, error) {
	if subjectType != "" && !isValidSubjectType(subjectType) {
		return nil, 0, httpx.BadRequest("INVALID_SUBJECT_TYPE", "unknown subject_type "+subjectType)
	}
	if !subjectID.IsZero() && subjectType == "" {
		return nil, 0, httpx.BadRequest("MISSING_SUBJECT_TYPE", "subject_id requires subject_type")
	}
	if !subjectID.IsZero() {
		if err := s.requireSubjectScope(ctx, actor, subjectType, subjectID); err != nil {
			return nil, 0, err
		}
	}
	return s.repo.listResults(ctx, subjectType, subjectID, page)
}

// getQCResult returns one result (its tests carry the stored pass verdicts),
// enforcing the subject org scope.
func (s *service) getQCResult(ctx context.Context, actor auth.Actor, resultID primitive.ObjectID) (*domain.QCResult, error) {
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
func (s *service) requireSubjectScope(ctx context.Context, actor auth.Actor, subjectType string, subjectID primitive.ObjectID) error {
	orgID, err := s.subjectOrgUnit(ctx, subjectType, subjectID)
	if err != nil {
		return err
	}
	if orgID.IsZero() {
		return nil
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, orgID); err != nil {
		s.log.WarnContext(ctx, "qc read scope denied",
			slog.String("subject_type", subjectType),
			slog.String("subject_id", subjectID.Hex()),
			slog.String("subject_org_unit_id", orgID.Hex()),
			slog.String("actor_party_id", actor.PartyID),
			slog.Any("err", err))
		return err
	}
	return nil
}

// subjectOrgUnit resolves the owning org unit of a subject, or the zero
// ObjectID when the subject is missing or not an org-owned gate subject.
func (s *service) subjectOrgUnit(ctx context.Context, subjectType string, subjectID primitive.ObjectID) (primitive.ObjectID, error) {
	switch subjectType {
	case domain.QCSubjectBMCLot:
		lot, err := s.repo.findBMCLot(ctx, subjectID)
		if err != nil {
			return primitive.NilObjectID, ignoreNotFound(err)
		}
		return lot.BMCID, nil
	case domain.QCSubjectProcessingBatch:
		batch, err := s.repo.findBatch(ctx, subjectID)
		if err != nil {
			return primitive.NilObjectID, ignoreNotFound(err)
		}
		return batch.PlantID, nil
	default:
		return primitive.NilObjectID, nil
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

// qcQueue lists every gate-eligible subject currently awaiting a verdict (BMC
// lots at BMC_RAPID, processing batches at PLANT_LAB), each annotated with its
// stage's mandatory tests split into recorded vs still-pending. The mandatory
// set is derived from the SAME stage logic as missingMandatoryTests, so the
// queue can never disagree with the gate about what a PASS requires.
func (s *service) qcQueue(ctx context.Context) (*QCQueueResponse, error) {
	lots, err := s.repo.listBMCLotsByStatus(ctx, domain.BMCLotStatusQCPending)
	if err != nil {
		s.log.ErrorContext(ctx, "qc queue: list bmc lots failed", slog.Any("err", err))
		return nil, err
	}
	batches, err := s.repo.listBatchesByStatus(ctx, domain.BatchStatusQCPending)
	if err != nil {
		s.log.ErrorContext(ctx, "qc queue: list batches failed", slog.Any("err", err))
		return nil, err
	}

	items := make([]QCQueueItem, 0, len(lots)+len(batches))
	for _, lot := range lots {
		item, err := s.queueItem(ctx, domain.QCSubjectBMCLot, lot.ID, domain.QCStageBMCRapid,
			lot.Date+" "+lot.Shift, lot.TotalQuantityLitres, lot.BMCID, lot.CreatedAt)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	for _, batch := range batches {
		item, err := s.queueItem(ctx, domain.QCSubjectProcessingBatch, batch.ID, domain.QCStagePlantLab,
			batch.BatchNumber, batch.InputLitres, batch.PlantID, batch.CreatedAt)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}

	return &QCQueueResponse{Items: items, Total: len(items)}, nil
}

// queueItem builds one queue row: it derives the stage's mandatory tests and
// splits them against the parameters already on file for the subject.
func (s *service) queueItem(ctx context.Context, subjectType string, subjectID primitive.ObjectID, stage, reference string, inputLitres float64, orgUnitID primitive.ObjectID, createdAt time.Time) (QCQueueItem, error) {
	recordedSet, err := s.repo.recordedTestNames(ctx, subjectType, subjectID)
	if err != nil {
		s.log.ErrorContext(ctx, "qc queue: recorded tests lookup failed",
			slog.String("subject_type", subjectType),
			slog.String("subject_id", subjectID.Hex()),
			slog.Any("err", err))
		return QCQueueItem{}, err
	}
	mandatory := mandatoryTestsForStage(stage)
	recorded := make([]string, 0, len(mandatory))
	pending := make([]string, 0, len(mandatory))
	for _, name := range mandatory {
		if recordedSet[name] {
			recorded = append(recorded, name)
		} else {
			pending = append(pending, name)
		}
	}
	return QCQueueItem{
		SubjectType:    subjectType,
		SubjectID:      subjectID,
		Stage:          stage,
		Reference:      reference,
		InputLitres:    inputLitres,
		OrgUnitID:      orgUnitID,
		MandatoryTests: mandatory,
		RecordedTests:  recorded,
		PendingTests:   pending,
		CreatedAt:      createdAt,
	}, nil
}

// issueCertificate mints a first-class QC certificate for a processing batch
// whose PLANT_LAB safety gate has already PASSED. The batch must be PASSED or
// COMPLETED — issuing for a CREATED/QC_PENDING/BLOCKED batch is a 409, because
// a certificate must never assert a verdict the gate has not reached. The
// certificate rosters the batch's recorded QC results and carries the plant's
// FSSAI licence for independent verification.
func (s *service) issueCertificate(ctx context.Context, actor auth.Actor, batchID primitive.ObjectID) (*domain.QCCertificate, error) {
	issuerID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	batch, err := s.repo.findBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, batch.PlantID); err != nil {
		s.log.WarnContext(ctx, "qc certificate scope denied",
			slog.String("batch_id", batchID.Hex()),
			slog.String("plant_id", batch.PlantID.Hex()),
			slog.String("actor_party_id", actor.PartyID),
			slog.Any("err", err))
		return nil, err
	}
	if batch.Status != domain.BatchStatusPassed && batch.Status != domain.BatchStatusCompleted {
		s.log.WarnContext(ctx, "qc certificate rejected: batch not passed",
			slog.String("batch_id", batchID.Hex()),
			slog.String("status", batch.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BATCH_NOT_PASSED",
			"batch is in status "+batch.Status+" — a certificate can only be issued for a PASSED or COMPLETED batch")
	}

	// FSSAI licence is best-effort enrichment: resolve the plant org unit but
	// never fail issuance over the directory lookup. The OrgUnit model carries
	// no licence field today, so this resolves to "" until one is populated.
	fssaiLicNo := ""
	if org, oerr := s.deps.Orgs.Get(ctx, batch.PlantID); oerr == nil && org != nil {
		fssaiLicNo = plantFSSAILicNo(org)
	}

	seq, err := s.repo.nextCertificateSeq(ctx, "BATCH_CERT")
	if err != nil {
		s.log.ErrorContext(ctx, "qc certificate: seq failed",
			slog.String("batch_id", batchID.Hex()), slog.Any("err", err))
		return nil, err
	}

	now := time.Now().UTC()
	cert := &domain.QCCertificate{
		ID:                primitive.NewObjectID(),
		BatchID:           batchID,
		CertificateNumber: fmt.Sprintf("QCC-%06d", seq),
		TestResultIDs:     batch.QCResultIDs,
		FSSAILicNo:        fssaiLicNo,
		IssuedByPartyID:   issuerID,
		IssuedAt:          now,
	}

	// Detach from the request context: evidence (certificate doc) then
	// provenance must complete even if the client disconnects mid-write.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	if err := s.repo.insertCertificate(wctx, cert); err != nil {
		var appErr *httpx.AppError
		if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict {
			s.log.ErrorContext(ctx, "qc certificate: insert failed",
				slog.String("batch_id", batchID.Hex()), slog.Any("err", err))
		}
		return nil, err
	}

	if _, err := s.deps.Ledger.Append(wctx, provenance.AppendInput{
		Type:       "qc.certificate_issued",
		EntityType: "QC_CERTIFICATE",
		EntityID:   cert.ID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityBatch, EntityID: batchID.Hex(), Relation: "certifies"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: batch.PlantID.Hex(),
		Payload: map[string]any{
			"certificate_number": cert.CertificateNumber,
			"batch_number":       batch.BatchNumber,
			"test_result_count":  len(cert.TestResultIDs),
		},
	}); err != nil {
		s.log.ErrorContext(ctx, "qc certificate: append provenance failed",
			slog.String("batch_id", batchID.Hex()),
			slog.String("certificate_id", cert.ID.Hex()),
			slog.Any("err", err))
		return nil, httpx.Internal(err)
	}

	s.log.InfoContext(ctx, "qc certificate issued",
		slog.String("certificate_id", cert.ID.Hex()),
		slog.String("certificate_number", cert.CertificateNumber),
		slog.String("batch_id", batchID.Hex()),
		slog.String("actor_party_id", actor.PartyID))
	return cert, nil
}

// plantFSSAILicNo extracts the plant's FSSAI licence number from the org unit.
// The OrgUnit model has no licence field yet, so this returns "" — the seam is
// here for when the directory carries it.
func plantFSSAILicNo(_ *domain.OrgUnit) string {
	return ""
}

// traceBack is the root-cause tool: for a (typically failed) batch it resolves
// the SET of contributing societies from batch.ContributingDCSIDs (§7.4 honest
// pooling, materialised by the plant module) enriched via the org directory,
// plus every QC result recorded against the batch. A society whose org lookup
// fails is still listed (Resolved=false) so the trace never silently drops a
// contributor.
func (s *service) traceBack(ctx context.Context, actor auth.Actor, batchID primitive.ObjectID) (*TraceBackResponse, error) {
	batch, err := s.repo.findBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Orgs.RequireInScope(ctx, actor, batch.PlantID); err != nil {
		s.log.WarnContext(ctx, "qc trace-back scope denied",
			slog.String("batch_id", batchID.Hex()),
			slog.String("plant_id", batch.PlantID.Hex()),
			slog.String("actor_party_id", actor.PartyID),
			slog.Any("err", err))
		return nil, err
	}

	// Per-society weighted breakdown: resolve the batch's BMC lots → consignments
	// → per-DCS litres/pour-count/date (§7.4). The share denominator is the sum
	// of contributing litres (never zero-division).
	contrib, cerr := s.repo.consignmentContributions(ctx, batch.BMCLotIDs)
	if cerr != nil {
		s.log.WarnContext(ctx, "qc trace-back: contribution aggregation failed (shares omitted)",
			slog.String("batch_id", batchID.Hex()), slog.Any("err", cerr))
		contrib = map[primitive.ObjectID]societyContribution{}
	}
	var totalLitres float64
	for _, c := range contrib {
		totalLitres += c.Litres
	}

	societies := make([]ContributingSociety, 0, len(batch.ContributingDCSIDs))
	for _, dcsID := range batch.ContributingDCSIDs {
		cs := ContributingSociety{OrgUnitID: dcsID}
		if c, ok := contrib[dcsID]; ok {
			cs.VolumeLitres = round2(c.Litres)
			cs.PourCount = c.PourCount
			cs.CollectedOn = c.CollectedOn
			if totalLitres > 0 {
				cs.VolumeShare = round4(c.Litres / totalLitres)
			}
		}
		if org, oerr := s.deps.Orgs.Get(ctx, dcsID); oerr == nil && org != nil {
			cs.Code = org.Code
			cs.Name = org.Name
			cs.NameHi = org.NameHi
			cs.Village = org.Village
			cs.District = org.District
			cs.Resolved = true
		} else {
			s.log.WarnContext(ctx, "qc trace-back: contributing dcs unresolved",
				slog.String("batch_id", batchID.Hex()),
				slog.String("dcs_id", dcsID.Hex()))
		}
		societies = append(societies, cs)
	}

	results, _, err := s.repo.listResults(ctx, domain.QCSubjectProcessingBatch, batchID, httpx.Page{Limit: 200, Offset: 0})
	if err != nil {
		s.log.ErrorContext(ctx, "qc trace-back: list results failed",
			slog.String("batch_id", batchID.Hex()), slog.Any("err", err))
		return nil, err
	}

	return &TraceBackResponse{
		BatchID:               batchID,
		BatchNumber:           batch.BatchNumber,
		PlantID:               batch.PlantID,
		Status:                batch.Status,
		InputLitres:           round2(batch.InputLitres),
		BlockReason:           batch.BlockReason,
		ContributingSocieties: societies,
		QCResults:             results,
	}, nil
}

// round2/round4 keep litres and share values at sensible precision.
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

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

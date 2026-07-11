package quality

// Per-samiti batch QC (F7): the PLANT_LAB_ANALYST works a queue of ACCEPTED
// per-samiti batches (DCS consignments accepted at plant intake), records the
// mock test panel manually, and the backend derives the overall verdict —
// PASS auto-mints the public batch QR (resolved at GET /public/qr/{code}),
// FAIL rejects the consignment (no QR), surfaced to the plant and the samiti
// through the consignment status.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

// batchQRTokenLen is the length of the short hex token minted for a passing
// batch (the first 8 hex chars of HMAC(qr_secret, batch_code)).
const batchQRTokenLen = 8

// ---------------------------------------------------------------------------
// Wire models
// ---------------------------------------------------------------------------

// BatchQCTestInput is one manually recorded parameter of the per-batch panel.
// The analyst supplies the verdict per parameter (mock criteria, configurable
// later); the server still floors AFLATOXIN_M1 at its FSSAI limit.
type BatchQCTestInput struct {
	Parameter string   `json:"parameter"`
	Value     *float64 `json:"value,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Pass      bool     `json:"pass"`
}

// recordBatchQCRequest is the POST /quality/consignments/{id}/qc payload.
// Verdict/Hold request a HOLD (§13.5): the consignment stays ACCEPTED with
// qc_hold=true and the panel may be re-submitted while held. A panel with a
// failing test (or AFM1 over the FSSAI ceiling) always REJECTs — a hold can
// never soften the safety gate.
type recordBatchQCRequest struct {
	Tests   []BatchQCTestInput `json:"tests"`
	Notes   string             `json:"notes,omitempty"`
	Verdict string             `json:"verdict,omitempty"` // only "HOLD" is honoured
	Hold    bool               `json:"hold,omitempty"`
}

// BatchQCOutcome is the record-QC response: the stored result, the batch QR
// when the verdict passed, and the consignment's resulting status.
type BatchQCOutcome struct {
	QC                *domain.ConsignmentQC      `json:"qc"`
	BatchQR           *domain.ConsignmentBatchQR `json:"batch_qr,omitempty"`
	ConsignmentStatus string                     `json:"consignment_status"`
	QCHold            bool                       `json:"qc_hold,omitempty"` // verdict HOLD: re-test allowed
}

// BatchQueueSamiti names the batch's source society on a queue row.
type BatchQueueSamiti struct {
	OrgUnitID string `json:"org_unit_id"`
	Name      string `json:"name,omitempty"`
	NameHi    string `json:"name_hi,omitempty"`
	Code      string `json:"code,omitempty"`
	Village   string `json:"village,omitempty"`
	District  string `json:"district,omitempty"`
}

// BatchQueueItem is one ACCEPTED per-samiti batch awaiting QC, carrying all
// the van-entered pickup data plus the plant intake evidence (F7 queue row).
type BatchQueueItem struct {
	ConsignmentID       string           `json:"consignment_id"`
	BatchCode           string           `json:"batch_code"`
	Samiti              BatchQueueSamiti `json:"samiti"`
	Date                string           `json:"date"`
	Shift               string           `json:"shift"`
	TotalQuantityLitres float64          `json:"total_quantity_litres"`
	MeasuredVolume      *float64         `json:"measured_volume_litres,omitempty"`
	AvgFatPct           float64          `json:"avg_fat_pct,omitempty"`
	AvgSNFPct           float64          `json:"avg_snf_pct,omitempty"`
	SealCode            string           `json:"seal_code,omitempty"`
	PickupPhotoURI      string           `json:"pickup_photo_uri,omitempty"`
	AnalyzerPhotoURI    string           `json:"analyzer_photo_uri,omitempty"`
	IntakePhotoURI      string           `json:"intake_photo_uri,omitempty"`
	PickedUpAt          *time.Time       `json:"picked_up_at,omitempty"`
	DeliveredAt         *time.Time       `json:"delivered_at,omitempty"`
	AcceptedAt          *time.Time       `json:"accepted_at,omitempty"`
}

// BatchQueueResponse is the GET /quality/batch-queue body.
type BatchQueueResponse struct {
	Items []BatchQueueItem `json:"items"`
	Total int              `json:"total"`
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// findConsignment loads one per-samiti batch (consignment) or NotFound.
func (r *repository) findConsignment(ctx context.Context, id primitive.ObjectID) (*domain.DCSConsignment, error) {
	var c domain.DCSConsignment
	err := r.consignments.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("consignment")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &c, nil
}

// insertConsignmentQC persists a batch QC result; the unique consignment_id
// index turns a duplicate submission into a 409.
func (r *repository) insertConsignmentQC(ctx context.Context, qc *domain.ConsignmentQC) error {
	if _, err := r.consignmentQC.InsertOne(ctx, qc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return httpx.Conflict("QC_EXISTS", "QC has already been recorded for this batch")
		}
		return httpx.Internal(err)
	}
	return nil
}

// retestConsignmentQC replaces a HELD panel with the re-test: the previous
// attempt is pushed onto the document's history (nothing is lost) and the live
// fields are overwritten. The conditional verdict filter guards the race — the
// document must still be the HOLD we loaded. Returns false on a lost race.
func (r *repository) retestConsignmentQC(ctx context.Context, qc *domain.ConsignmentQC, prior *domain.ConsignmentQC) (bool, error) {
	attempt := domain.ConsignmentQCAttempt{
		Tests:    prior.Tests,
		Verdict:  domain.EffectiveQCVerdict(prior.Verdict, prior.OverallPass),
		Notes:    prior.Notes,
		TestedBy: prior.TestedBy,
		TestedAt: prior.TestedAt,
	}
	res, err := r.consignmentQC.UpdateOne(ctx,
		bson.D{
			{Key: "_id", Value: prior.ID},
			{Key: "verdict", Value: domain.QCVerdictHold},
		},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "tests", Value: qc.Tests},
				{Key: "overall_pass", Value: qc.OverallPass},
				{Key: "verdict", Value: qc.Verdict},
				{Key: "notes", Value: qc.Notes},
				{Key: "tested_by", Value: qc.TestedBy},
				{Key: "tested_at", Value: qc.TestedAt},
			}},
			{Key: "$push", Value: bson.D{{Key: "history", Value: attempt}}},
		},
	)
	if err != nil {
		return false, httpx.Internal(err)
	}
	return res.MatchedCount == 1, nil
}

// findConsignmentQC returns the stored batch QC result, or NotFound.
func (r *repository) findConsignmentQC(ctx context.Context, consignmentID primitive.ObjectID) (*domain.ConsignmentQC, error) {
	var qc domain.ConsignmentQC
	err := r.consignmentQC.FindOne(ctx, bson.D{{Key: "consignment_id", Value: consignmentID}}).Decode(&qc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("batch qc result")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &qc, nil
}

// setConsignmentQCProvenanceSeq stamps the ledger sequence onto the QC doc.
func (r *repository) setConsignmentQCProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	if _, err := r.consignmentQC.UpdateByID(ctx, id,
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}}); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// insertBatchQR persists the auto-minted batch QR. On a duplicate (retry after
// a partial failure) the existing QR for the batch code is returned instead.
func (r *repository) insertBatchQR(ctx context.Context, qr *domain.ConsignmentBatchQR) (*domain.ConsignmentBatchQR, error) {
	if _, err := r.batchQRs.InsertOne(ctx, qr); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			var existing domain.ConsignmentBatchQR
			if ferr := r.batchQRs.FindOne(ctx, bson.D{{Key: "batch_code", Value: qr.BatchCode}}).Decode(&existing); ferr == nil {
				return &existing, nil
			}
			return nil, httpx.Internal(err)
		}
		return nil, httpx.Internal(err)
	}
	return qr, nil
}

// findBatchQR returns the minted QR for a consignment, or nil when none.
func (r *repository) findBatchQR(ctx context.Context, consignmentID primitive.ObjectID) (*domain.ConsignmentBatchQR, error) {
	var qr domain.ConsignmentBatchQR
	err := r.batchQRs.FindOne(ctx, bson.D{{Key: "consignment_id", Value: consignmentID}}).Decode(&qr)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &qr, nil
}

// stampConsignmentQCVerdict writes the QC verdict fields onto the consignment.
// PASS keeps ACCEPTED (qc_hold cleared); HOLD keeps ACCEPTED with qc_hold=true
// (re-test allowed, §13.5); REJECT flips the status to REJECTED (+ reason).
// Every write is conditional on the ACCEPTED status.
func (r *repository) stampConsignmentQCVerdict(ctx context.Context, id primitive.ObjectID, verdict string, testedAt time.Time, rejectReason string) (bool, error) {
	set := bson.D{
		{Key: "qc_overall_pass", Value: verdict == domain.QCVerdictPass},
		{Key: "qc_verdict", Value: verdict},
		{Key: "qc_hold", Value: verdict == domain.QCVerdictHold},
		{Key: "qc_tested_at", Value: testedAt},
	}
	filter := bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.ConsignmentStatusAccepted}}
	if verdict == domain.QCVerdictReject {
		set = append(set,
			bson.E{Key: "status", Value: domain.ConsignmentStatusRejected},
			bson.E{Key: "rejected_at", Value: testedAt},
			bson.E{Key: "reject_reason", Value: rejectReason})
	}
	res, err := r.consignments.UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, httpx.Internal(err)
	}
	return res.MatchedCount > 0, nil
}

// listAcceptedPendingQC returns ACCEPTED consignments carrying a batch_code
// that still need lab work: never tested, OR held (qc_hold — awaiting a
// re-test, §13.5). The F7 batch queue, oldest accepted first. A nil dcsIDs
// means no DCS fence (federation-wide oversight roles).
func (r *repository) listAcceptedPendingQC(ctx context.Context, dcsIDs []primitive.ObjectID) ([]domain.DCSConsignment, error) {
	filter := bson.D{
		{Key: "status", Value: domain.ConsignmentStatusAccepted},
		{Key: "batch_code", Value: bson.D{{Key: "$exists", Value: true}, {Key: "$ne", Value: ""}}},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "qc_tested_at", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "qc_hold", Value: true}},
		}},
	}
	if dcsIDs != nil {
		filter = append(filter, bson.E{Key: "dcs_id", Value: bson.D{{Key: "$in", Value: dcsIDs}}})
	}
	cur, err := r.consignments.Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "accepted_at", Value: 1}}).SetLimit(500))
	if err != nil {
		return nil, httpx.Internal(err)
	}
	items := []domain.DCSConsignment{}
	if err := cur.All(ctx, &items); err != nil {
		return nil, httpx.Internal(err)
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// validateBatchQCRequest checks the panel shape against the closed parameter
// vocabulary and rejects duplicates — a typo can never satisfy the gate.
func validateBatchQCRequest(req recordBatchQCRequest) *httpx.AppError {
	if len(req.Tests) == 0 {
		return httpx.BadRequest("MISSING_TESTS", "at least one test is required")
	}
	seen := make(map[string]bool, len(req.Tests))
	for i, t := range req.Tests {
		p := strings.ToUpper(strings.TrimSpace(t.Parameter))
		if p == "" {
			return httpx.BadRequest("MISSING_PARAMETER", "tests["+strconv.Itoa(i)+"].parameter is required")
		}
		if _, ok := domain.BatchQCParameters[p]; !ok {
			return httpx.BadRequest("UNKNOWN_PARAMETER",
				"unknown parameter "+t.Parameter+" — allowed: AFLATOXIN_M1, ADDED_WATER, ADULTERANTS, ANTIBIOTICS, FAT_PCT, SNF_PCT")
		}
		if seen[p] {
			return httpx.BadRequest("DUPLICATE_PARAMETER", "parameter "+p+" appears more than once")
		}
		seen[p] = true
	}
	return nil
}

// requireBatchScope fences batch QC reads/writes: plant-tier roles must share
// the consignment DCS's union (the plant is a sibling branch, so ancestor
// containment cannot apply); everyone else goes through the standard org
// scope check (Sachiv → own DCS, supervisor → union, auditors wide).
func (s *service) requireBatchScope(ctx context.Context, actor auth.Actor, dcsID primitive.ObjectID) error {
	switch actor.RoleCode {
	case domain.RolePlantOperator, domain.RolePlantLabAnalyst:
		orgID, err := httpx.ParseID(actor.OrgUnitID, "actor org unit")
		if err != nil {
			return err
		}
		ua, err := s.deps.Orgs.UnionAncestor(ctx, orgID)
		if err != nil {
			return err
		}
		ub, err := s.deps.Orgs.UnionAncestor(ctx, dcsID)
		if err != nil {
			return err
		}
		if ua.IsZero() || ub.IsZero() || ua != ub {
			return httpx.Forbidden("resource is outside your organisational scope")
		}
		return nil
	default:
		return s.deps.Orgs.RequireInScope(ctx, actor, dcsID)
	}
}

// recordBatchQC records the manual test panel for one ACCEPTED per-samiti
// batch: overall pass = every test passed (with AFLATOXIN_M1 floored at its
// FSSAI 0.5 µg/kg limit server-side). PASS auto-mints the batch QR; FAIL
// rejects the consignment (no QR).
func (s *service) recordBatchQC(ctx context.Context, actor auth.Actor, consignmentID primitive.ObjectID, req recordBatchQCRequest) (*BatchQCOutcome, error) {
	if aerr := validateBatchQCRequest(req); aerr != nil {
		return nil, aerr
	}
	analystID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	consignment, err := s.repo.findConsignment(ctx, consignmentID)
	if err != nil {
		return nil, err
	}
	if err := s.requireBatchScope(ctx, actor, consignment.DCSID); err != nil {
		s.log.WarnContext(ctx, "batch qc scope denied",
			slog.String("consignment_id", consignmentID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	if consignment.Status != domain.ConsignmentStatusAccepted {
		return nil, httpx.Conflict("CONSIGNMENT_NOT_ACCEPTED",
			"consignment is "+consignment.Status+"; only plant-ACCEPTED batches can be QC'd")
	}
	if consignment.BatchCode == "" {
		return nil, httpx.Conflict("BATCH_CODE_MISSING",
			"consignment carries no batch_code — it was never picked up through the per-samiti batch flow")
	}

	// Re-test discipline (§13.5): a second submission is allowed ONLY while the
	// live verdict is HOLD — a PASSed or REJECTed batch stays decided.
	prior, err := s.repo.findConsignmentQC(ctx, consignmentID)
	if err != nil && !isNotFoundAppErr(err) {
		return nil, err
	}
	if prior != nil && domain.EffectiveQCVerdict(prior.Verdict, prior.OverallPass) != domain.QCVerdictHold {
		return nil, httpx.Conflict("QC_EXISTS", "QC has already been recorded for this batch")
	}

	// Evaluate: the analyst's per-parameter verdicts drive the result (mock
	// criteria), EXCEPT aflatoxin — a reading over the FSSAI ceiling can never
	// pass (§8.3: AFM1 is heat-stable; the gate must hold upstream).
	overallPass := true
	tests := make([]domain.BatchQCTest, 0, len(req.Tests))
	var failed []string
	for _, t := range req.Tests {
		p := strings.ToUpper(strings.TrimSpace(t.Parameter))
		pass := t.Pass
		if p == domain.BatchTestAflatoxinM1 && t.Value != nil &&
			*t.Value > domain.FSSAIAflatoxinM1MaxMicrogramPerKg {
			pass = false
		}
		if !pass {
			overallPass = false
			failed = append(failed, p)
		}
		tests = append(tests, domain.BatchQCTest{Parameter: p, Value: t.Value, Unit: t.Unit, Pass: pass})
	}

	// Verdict machine: tests derive PASS/REJECT; the analyst may request HOLD
	// (consignment stays ACCEPTED, qc_hold=true, re-test allowed) — but a
	// failing panel can never be softened to a hold.
	verdict := domain.QCVerdictPass
	if !overallPass {
		verdict = domain.QCVerdictReject
	}
	holdRequested := req.Hold || strings.EqualFold(strings.TrimSpace(req.Verdict), domain.QCVerdictHold)
	if holdRequested && overallPass {
		verdict = domain.QCVerdictHold
		overallPass = false // a hold is not a pass: no QR yet
	}

	now := time.Now().UTC()
	qc := &domain.ConsignmentQC{
		ID:            primitive.NewObjectID(),
		ConsignmentID: consignmentID,
		BatchCode:     consignment.BatchCode,
		Tests:         tests,
		OverallPass:   overallPass,
		Verdict:       verdict,
		Notes:         req.Notes,
		TestedBy:      analystID,
		TestedAt:      now,
	}

	// Detach from the request context: evidence, provenance and the verdict
	// stamp must complete even if the client disconnects mid-sequence.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	if prior == nil {
		if err := s.repo.insertConsignmentQC(wctx, qc); err != nil {
			return nil, err
		}
	} else {
		// Re-test of a HELD batch: same document, prior attempt kept in history.
		replaced, err := s.repo.retestConsignmentQC(wctx, qc, prior)
		if err != nil {
			return nil, err
		}
		if !replaced {
			return nil, httpx.Conflict("QC_EXISTS", "the held QC was decided concurrently — reload the batch")
		}
		qc.ID = prior.ID
		qc.History = append(prior.History, domain.ConsignmentQCAttempt{
			Tests:    prior.Tests,
			Verdict:  domain.EffectiveQCVerdict(prior.Verdict, prior.OverallPass),
			Notes:    prior.Notes,
			TestedBy: prior.TestedBy,
			TestedAt: prior.TestedAt,
		})
	}

	rejectReason := ""
	if verdict == domain.QCVerdictReject {
		rejectReason = "QC failed: " + strings.Join(failed, ", ")
	}
	event, err := s.deps.Ledger.Append(wctx, provenance.AppendInput{
		Type:       domain.EventConsignmentQCRecorded,
		EntityType: domain.EntityConsignmentQC,
		EntityID:   qc.ID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityConsignment, EntityID: consignmentID.Hex(), Relation: "tested"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: actor.OrgUnitID,
		Payload: map[string]any{
			"batch_code":   consignment.BatchCode,
			"overall_pass": overallPass,
			"verdict":      verdict,
			"test_count":   len(tests),
			"retest":       prior != nil,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setConsignmentQCProvenanceSeq(wctx, qc.ID, event.Seq); err != nil {
		return nil, err
	}
	qc.ProvenanceSeq = event.Seq

	if _, err := s.repo.stampConsignmentQCVerdict(wctx, consignmentID, verdict, now, rejectReason); err != nil {
		return nil, err
	}

	// Notify the samiti sachiv + the receiving plant's operator (platformops
	// turns this into outbox notifications — qc.result).
	plantHex := ""
	if consignment.AcceptedPlantID != nil {
		plantHex = consignment.AcceptedPlantID.Hex()
	}
	s.deps.Bus.Publish(eventbus.TopicBatchQCRecorded, BatchQCRecordedEvent{
		ConsignmentID:    consignmentID.Hex(),
		BatchCode:        consignment.BatchCode,
		DCSID:            consignment.DCSID.Hex(),
		PlantID:          plantHex,
		Verdict:          verdict,
		FailedParameters: failed,
	})

	outcome := &BatchQCOutcome{QC: qc, ConsignmentStatus: domain.ConsignmentStatusAccepted, QCHold: verdict == domain.QCVerdictHold}
	if verdict == domain.QCVerdictPass {
		// PASS → auto-mint the public batch QR: code == batch_code, token is a
		// short HMAC over the batch code signed with the QR secret.
		token := auth.HMACHash(s.deps.Cfg.QRSigningSecret, consignment.BatchCode)[:batchQRTokenLen]
		qr, err := s.repo.insertBatchQR(wctx, &domain.ConsignmentBatchQR{
			ID:            primitive.NewObjectID(),
			ConsignmentID: consignmentID,
			BatchCode:     consignment.BatchCode,
			Token:         token,
			IssuedAt:      now,
		})
		if err != nil {
			return nil, err
		}
		if _, err := s.deps.Ledger.Append(wctx, provenance.AppendInput{
			Type:       domain.EventConsignmentQRIssued,
			EntityType: domain.EntityConsignmentBatchQR,
			EntityID:   qr.ID.Hex(),
			Refs: []provenance.Ref{
				{EntityType: domain.EntityConsignment, EntityID: consignmentID.Hex(), Relation: "labels"},
			},
			Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
			OrgUnitID: actor.OrgUnitID,
			Payload:   map[string]any{"batch_code": qr.BatchCode},
		}); err != nil {
			return nil, httpx.Internal(err)
		}
		outcome.BatchQR = qr
		// Notify the samiti sachiv that the public QR is live (batch.qr_minted).
		s.deps.Bus.Publish(eventbus.TopicBatchQRMinted, BatchQRMintedEvent{
			ConsignmentID: consignmentID.Hex(),
			BatchCode:     qr.BatchCode,
			DCSID:         consignment.DCSID.Hex(),
			Token:         qr.Token,
		})
		s.log.InfoContext(ctx, "batch qc passed: batch qr minted",
			slog.String("consignment_id", consignmentID.Hex()),
			slog.String("batch_code", qr.BatchCode),
			slog.String("token", qr.Token),
			slog.String("actor_party_id", actor.PartyID))
	} else if verdict == domain.QCVerdictHold {
		s.log.WarnContext(ctx, "batch qc held: consignment stays ACCEPTED, re-test allowed",
			slog.String("consignment_id", consignmentID.Hex()),
			slog.String("batch_code", consignment.BatchCode),
			slog.String("actor_party_id", actor.PartyID))
	} else {
		outcome.ConsignmentStatus = domain.ConsignmentStatusRejected
		s.log.WarnContext(ctx, "batch qc failed: consignment rejected",
			slog.String("consignment_id", consignmentID.Hex()),
			slog.String("batch_code", consignment.BatchCode),
			slog.String("reject_reason", rejectReason),
			slog.String("actor_party_id", actor.PartyID))
	}
	return outcome, nil
}

// isNotFoundAppErr reports whether err is a 404 AppError.
func isNotFoundAppErr(err error) bool {
	var appErr *httpx.AppError
	return errors.As(err, &appErr) && appErr.Status == http.StatusNotFound
}

// getBatchQC returns the stored QC result (plus the QR when one was minted).
func (s *service) getBatchQC(ctx context.Context, actor auth.Actor, consignmentID primitive.ObjectID) (*BatchQCOutcome, error) {
	consignment, err := s.repo.findConsignment(ctx, consignmentID)
	if err != nil {
		return nil, err
	}
	if err := s.requireBatchScope(ctx, actor, consignment.DCSID); err != nil {
		return nil, err
	}
	qc, err := s.repo.findConsignmentQC(ctx, consignmentID)
	if err != nil {
		return nil, err
	}
	qc.Verdict = domain.EffectiveQCVerdict(qc.Verdict, qc.OverallPass)
	qr, err := s.repo.findBatchQR(ctx, consignmentID)
	if err != nil {
		return nil, err
	}
	return &BatchQCOutcome{
		QC:                qc,
		BatchQR:           qr,
		ConsignmentStatus: consignment.Status,
		QCHold:            consignment.QCHold,
	}, nil
}

// batchQueue lists the ACCEPTED per-samiti batches still awaiting QC (F7),
// each row enriched with the samiti's directory data and the van-entered
// pickup evidence. Plant-tier and union-tier roles are fenced to their own
// union's societies; federation oversight (mission/auditor) reads wide.
func (s *service) batchQueue(ctx context.Context, actor auth.Actor) (*BatchQueueResponse, error) {
	var dcsIDs []primitive.ObjectID // nil = no fence
	switch actor.RoleCode {
	case domain.RolePlantOperator, domain.RolePlantLabAnalyst, domain.RoleUnionFieldSupervisor:
		orgID, err := httpx.ParseID(actor.OrgUnitID, "actor org unit")
		if err != nil {
			return nil, err
		}
		unionID, err := s.deps.Orgs.UnionAncestor(ctx, orgID)
		if err != nil {
			return nil, err
		}
		if unionID.IsZero() {
			return nil, httpx.BadRequest("NO_UNION", "no union in the org hierarchy")
		}
		ids, err := s.deps.Orgs.DescendantIDs(ctx, unionID, domain.OrgTypeDCS)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return &BatchQueueResponse{Items: []BatchQueueItem{}, Total: 0}, nil
		}
		dcsIDs = ids
	default:
		// MISSION_OFFICIAL / STATE_AUDITOR: federation-wide oversight view.
	}
	rows, err := s.repo.listAcceptedPendingQC(ctx, dcsIDs)
	if err != nil {
		s.log.ErrorContext(ctx, "batch queue: list failed", slog.Any("err", err))
		return nil, err
	}
	items := make([]BatchQueueItem, 0, len(rows))
	for i := range rows {
		items = append(items, s.batchQueueItem(ctx, &rows[i]))
	}
	return &BatchQueueResponse{Items: items, Total: len(items)}, nil
}

// batchQueueItem projects one consignment onto the queue row, enriching the
// samiti block from the org directory (best-effort — a failed lookup leaves
// the id and omits names rather than fabricating them).
func (s *service) batchQueueItem(ctx context.Context, c *domain.DCSConsignment) BatchQueueItem {
	item := BatchQueueItem{
		ConsignmentID:       c.ID.Hex(),
		BatchCode:           c.BatchCode,
		Samiti:              BatchQueueSamiti{OrgUnitID: c.DCSID.Hex()},
		Date:                c.Date,
		Shift:               c.Shift,
		TotalQuantityLitres: c.TotalQuantityLitres,
		MeasuredVolume:      c.MeasuredVolumeLitres,
		AvgFatPct:           c.AvgFatPct,
		AvgSNFPct:           c.AvgSNFPct,
		SealCode:            c.SealCode,
		PickupPhotoURI:      c.PickupPhotoURI,
		AnalyzerPhotoURI:    c.AnalyzerPhotoURI,
		IntakePhotoURI:      c.IntakePhotoURI,
		PickedUpAt:          c.PickedUpAt,
		DeliveredAt:         c.DeliveredAt,
		AcceptedAt:          c.AcceptedAt,
	}
	if org, err := s.deps.Orgs.Get(ctx, c.DCSID); err == nil && org != nil {
		item.Samiti.Name = org.Name
		item.Samiti.NameHi = org.NameHi
		item.Samiti.Code = org.Code
		item.Samiti.Village = org.Village
		item.Samiti.District = org.District
	}
	return item
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// recordBatchQC handles POST /quality/consignments/{consignmentID}/qc.
func (h *handler) recordBatchQC(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	id, err := httpx.PathID(r, "consignmentID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req recordBatchQCRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	outcome, err := h.svc.recordBatchQC(r.Context(), actor, id, req)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, outcome)
}

// getBatchQC handles GET /quality/consignments/{consignmentID}/qc.
func (h *handler) getBatchQC(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	id, err := httpx.PathID(r, "consignmentID")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	outcome, err := h.svc.getBatchQC(r.Context(), actor, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, outcome)
}

// getBatchQueue handles GET /quality/batch-queue.
func (h *handler) getBatchQueue(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.Unauthorized("authentication required"))
		return
	}
	queue, err := h.svc.batchQueue(r.Context(), actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, queue)
}

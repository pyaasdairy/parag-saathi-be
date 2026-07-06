package plant

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

// codeSafetyGateBlocked is the stable error code every gate refusal carries —
// the frontend switches on it to show the quarantine screen (blueprint §8.3).
const codeSafetyGateBlocked = "SAFETY_GATE_BLOCKED"

// Service holds the plant module's business logic: lot pooling, the safety
// gate, batch numbering, product lot packaging and QR minting.
type Service struct {
	repo     *Repo
	orgs     *orgscope.Resolver
	ledger   *provenance.Ledger
	qrSecret string
}

// NewService wires the service from the shared dependency container.
func NewService(repo *Repo, d *deps.Deps) *Service {
	return &Service{
		repo:     repo,
		orgs:     d.Orgs,
		ledger:   d.Ledger,
		qrSecret: d.Cfg.QRSigningSecret,
	}
}

// --- safety-gate predicates (§8.3) ---
//
// Pure functions so the refusal matrix is unit-testable without a database.
// Every mutation below ALSO re-checks the status inside the MongoDB update
// filter, so a concurrent QC block can never slip a bad lot through.

// canDispatchBMCLot: only a lot with a passing BMC rapid test may leave on a
// tanker. BLOCKED, QC_PENDING and OPEN lots stay put.
func canDispatchBMCLot(status string) bool {
	return status == domain.BMCLotStatusPassed
}

// canPoolBMCLot: only a DISPATCHED lot (which by construction went
// OPEN→QC_PENDING→PASSED→DISPATCHED) may enter a processing batch.
func canPoolBMCLot(status string) bool {
	return status == domain.BMCLotStatusDispatched
}

// canCompleteBatch: only a batch the plant lab has PASSED may complete.
func canCompleteBatch(status string) bool {
	return status == domain.BatchStatusPassed
}

// canYieldProductLot: only a COMPLETED batch may be packaged into SKU lots.
func canYieldProductLot(batchStatus string) bool {
	return batchStatus == domain.BatchStatusCompleted
}

// canIssueQR: a QR may only label an ACTIVE product lot whose batch is
// COMPLETED. This read-time check is advisory; IssueQR additionally confirms
// the lot is still ACTIVE with a conditional write after the QR insert, so a
// recall landing mid-request still stops the mint.
func canIssueQR(lotStatus, batchStatus string) bool {
	return lotStatus == domain.ProductLotStatusActive && batchStatus == domain.BatchStatusCompleted
}

// --- BMC lots ---

// CreateBMCLot pools DELIVERED consignments into a new OPEN lot at a BMC.
func (s *Service) CreateBMCLot(ctx context.Context, actor auth.Actor, req CreateBMCLotRequest) (*domain.BMCLot, error) {
	if err := s.orgs.RequireInScope(ctx, actor, req.BMCID); err != nil {
		return nil, err
	}
	org, err := s.orgs.Get(ctx, req.BMCID)
	if err != nil {
		return nil, err
	}
	if org.Type != domain.OrgTypeBMC {
		return nil, httpx.BadRequest("NOT_A_BMC", "org unit "+req.BMCID+" is not a BMC")
	}

	now := time.Now().UTC()
	date := req.Date
	if date == "" {
		date = domain.DateKeyIST(now)
	}

	ids := dedupe(req.ConsignmentIDs)
	consignments, err := s.repo.ConsignmentsByIDs(ctx, ids)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if len(consignments) != len(ids) {
		return nil, httpx.NotFound("consignment(s) " + strings.Join(missingIDs(ids, consignments), ", "))
	}
	var undelivered []string
	for _, c := range consignments {
		if c.Status != domain.ConsignmentStatusDelivered {
			undelivered = append(undelivered, c.ID)
		}
	}
	if len(undelivered) > 0 {
		return nil, httpx.Unprocessable("CONSIGNMENT_NOT_DELIVERED",
			"only DELIVERED consignments can be pooled into a BMC lot").
			WithDetails(map[string]any{"consignment_ids": undelivered})
	}

	// Optimistic claim: flip DELIVERED→ACCEPTED stamped with the new lot ID.
	// If another lot raced us on any consignment, release our claims and bail.
	lotID := uuid.NewString()
	claimed, err := s.repo.ClaimConsignments(ctx, ids, lotID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if claimed != int64(len(ids)) {
		if relErr := s.repo.ReleaseConsignments(ctx, lotID); relErr != nil {
			return nil, httpx.Internal(relErr)
		}
		return nil, httpx.Conflict("CONSIGNMENT_CLAIM_CONFLICT",
			"one or more consignments were pooled by another lot — reload and retry")
	}

	var totalLitres float64
	var tripIDs []string
	seenTrip := map[string]bool{}
	for _, c := range consignments {
		totalLitres += c.TotalQuantityLitres
		if c.RouteTripID != "" && !seenTrip[c.RouteTripID] {
			seenTrip[c.RouteTripID] = true
			tripIDs = append(tripIDs, c.RouteTripID)
		}
	}

	lot := &domain.BMCLot{
		ID:                  lotID,
		BMCID:               req.BMCID,
		Date:                date,
		Shift:               req.Shift,
		ConsignmentIDs:      ids,
		RouteTripIDs:        tripIDs,
		TotalQuantityLitres: totalLitres,
		Status:              domain.BMCLotStatusOpen,
		CreatedBy:           actor.PartyID,
		CreatedAt:           now,
	}
	if err := s.repo.InsertBMCLot(ctx, lot); err != nil {
		_ = s.repo.ReleaseConsignments(ctx, lotID)
		return nil, httpx.Internal(err)
	}

	refs := make([]provenance.Ref, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, provenance.Ref{
			EntityType: domain.EntityConsignment, EntityID: id, Relation: "pools",
		})
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBMCLotCreated,
		EntityType: domain.EntityBMCLot,
		EntityID:   lot.ID,
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  req.BMCID,
		Payload: map[string]any{
			"date": date, "shift": req.Shift, "total_quantity_litres": totalLitres,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetBMCLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	lot.ProvenanceSeq = ev.Seq
	return lot, nil
}

// CloseBMCLot transitions an OPEN lot to QC_PENDING with the chilling
// temperature. The quality module later flips QC_PENDING→PASSED/BLOCKED.
func (s *Service) CloseBMCLot(ctx context.Context, actor auth.Actor, id string, chillingTempC float64) (*domain.BMCLot, error) {
	lot, err := s.getBMCLot(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, lot.BMCID); err != nil {
		return nil, err
	}
	if lot.Status != domain.BMCLotStatusOpen {
		return nil, httpx.Conflict("BMC_LOT_NOT_OPEN",
			"only an OPEN lot can be closed; lot is "+lot.Status)
	}
	now := time.Now().UTC()
	ok, err := s.repo.CloseBMCLot(ctx, id, chillingTempC, now)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !ok {
		return nil, httpx.Conflict("BMC_LOT_NOT_OPEN",
			"lot changed state concurrently — reload and retry")
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBMCLotClosed,
		EntityType: domain.EntityBMCLot,
		EntityID:   lot.ID,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  lot.BMCID,
		Payload:    map[string]any{"chilling_temp_c": chillingTempC},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetBMCLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	lot.Status = domain.BMCLotStatusQCPending
	lot.ChillingTempC = chillingTempC
	lot.ClosedAt = &now
	lot.ProvenanceSeq = ev.Seq
	return lot, nil
}

// DispatchBMCLot transitions PASSED→DISPATCHED. THE safety gate (§8.3): a
// BLOCKED or untested lot never leaves the chilling centre.
func (s *Service) DispatchBMCLot(ctx context.Context, actor auth.Actor, id string) (*domain.BMCLot, error) {
	lot, err := s.getBMCLot(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, lot.BMCID); err != nil {
		return nil, err
	}
	if lot.Status == domain.BMCLotStatusDispatched {
		return nil, httpx.Conflict("BMC_LOT_ALREADY_DISPATCHED", "lot is already dispatched")
	}
	if !canDispatchBMCLot(lot.Status) {
		msg := "lot cannot be dispatched: safety gate not passed (status " + lot.Status + ")"
		if lot.BlockReason != "" {
			msg += " — " + lot.BlockReason
		}
		return nil, httpx.Unprocessable(codeSafetyGateBlocked, msg).
			WithDetails(map[string]any{"status": lot.Status, "block_reason": lot.BlockReason})
	}
	now := time.Now().UTC()
	ok, err := s.repo.DispatchBMCLot(ctx, id, now)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !ok {
		return nil, httpx.Conflict("BMC_LOT_STATE_CHANGED",
			"lot changed state concurrently — reload and retry")
	}
	// The custody transition — gate-passed milk physically leaving the
	// chilling centre — must live on the tamper-evident chain like every
	// other hop of the pour→QR path, not only in a mutable document field.
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBMCLotDispatched,
		EntityType: domain.EntityBMCLot,
		EntityID:   lot.ID,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  lot.BMCID,
		Payload: map[string]any{
			// String, not time.Time: payloads must round-trip BSON⇄JSON
			// byte-identically or chain re-verification would break.
			"dispatched_at":         now.Format(time.RFC3339Nano),
			"total_quantity_litres": lot.TotalQuantityLitres,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetBMCLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	lot.Status = domain.BMCLotStatusDispatched
	lot.DispatchedAt = &now
	lot.ProvenanceSeq = ev.Seq
	return lot, nil
}

// ListBMCLots pages lots by BMC/date/status. A BMC operator may only list
// within their own scope; downstream readers (plant operator, supervisor,
// lab) see across BMCs — the plant receives tankers from sibling BMCs, so an
// ancestry check against the plant org would wrongly refuse them.
func (s *Service) ListBMCLots(ctx context.Context, actor auth.Actor, f BMCLotListFilter, page httpx.Page) ([]domain.BMCLot, int64, error) {
	if f.BMCID != "" && actor.RoleCode == domain.RoleBMCOperator {
		if err := s.orgs.RequireInScope(ctx, actor, f.BMCID); err != nil {
			return nil, 0, err
		}
	}
	lots, total, err := s.repo.ListBMCLots(ctx, f, page)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return lots, total, nil
}

// --- processing batches ---

// CreateBatch pools DISPATCHED (gate-passed) BMC lots into one production
// run. The gate re-checks here belt-and-braces: aflatoxin M1 survives
// pasteurisation, so a blocked lot must never reach a vat (§8.3).
func (s *Service) CreateBatch(ctx context.Context, actor auth.Actor, req CreateBatchRequest) (*domain.ProcessingBatch, error) {
	if err := s.orgs.RequireInScope(ctx, actor, req.PlantID); err != nil {
		return nil, err
	}
	org, err := s.orgs.Get(ctx, req.PlantID)
	if err != nil {
		return nil, err
	}
	if org.Type != domain.OrgTypeProcessingPlant {
		return nil, httpx.BadRequest("NOT_A_PLANT", "org unit "+req.PlantID+" is not a processing plant")
	}

	ids := dedupe(req.BMCLotIDs)
	lots, err := s.repo.BMCLotsByIDs(ctx, ids)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if len(lots) != len(ids) {
		return nil, httpx.NotFound("BMC lot(s) " + strings.Join(missingLotIDs(ids, lots), ", "))
	}
	var offending []string
	var inputLitres float64
	for _, lot := range lots {
		if !canPoolBMCLot(lot.Status) {
			offending = append(offending, lot.ID)
			continue
		}
		inputLitres += lot.TotalQuantityLitres
	}
	if len(offending) > 0 {
		return nil, httpx.Unprocessable(codeSafetyGateBlocked,
			"one or more BMC lots have not cleared the safety gate (must be PASSED and DISPATCHED)").
			WithDetails(map[string]any{"blocked_lot_ids": offending})
	}

	now := time.Now().UTC()
	dateKey := domain.DateKeyIST(now)
	seq, err := s.repo.NextSequence(ctx, "batch_number:"+req.PlantID+":"+dateKey)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	batchNumber := fmt.Sprintf("BATCH-%s-%s-%04d",
		org.Code, strings.ReplaceAll(dateKey, "-", ""), seq)

	// Optimistic claim (mirrors ClaimConsignments): flip DISPATCHED→POOLED
	// stamped with the new batch ID BEFORE inserting the batch. One physical
	// lot of milk can therefore never be pooled into two batches — neither by
	// a double-submit nor by two concurrent CreateBatch calls.
	batchID := uuid.NewString()
	claimed, err := s.repo.ClaimBMCLots(ctx, ids, batchID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if claimed != int64(len(ids)) {
		if relErr := s.repo.ReleaseBMCLots(ctx, batchID); relErr != nil {
			return nil, httpx.Internal(relErr)
		}
		return nil, httpx.Conflict("BMC_LOT_CLAIM_CONFLICT",
			"one or more BMC lots were pooled by another batch — reload and retry")
	}

	// CREATED is a transient birth state: the plant lab must clear every
	// batch, so it is persisted straight into QC_PENDING.
	batch := &domain.ProcessingBatch{
		ID:          batchID,
		PlantID:     req.PlantID,
		BatchNumber: batchNumber,
		BMCLotIDs:   ids,
		ProductType: req.ProductType,
		InputLitres: inputLitres,
		Status:      domain.BatchStatusQCPending,
		StartedAt:   now,
		CreatedBy:   actor.PartyID,
		CreatedAt:   now,
	}
	if err := s.repo.InsertBatch(ctx, batch); err != nil {
		_ = s.repo.ReleaseBMCLots(ctx, batchID)
		return nil, httpx.Internal(err)
	}

	refs := make([]provenance.Ref, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, provenance.Ref{
			EntityType: domain.EntityBMCLot, EntityID: id, Relation: "pools",
		})
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBatchCreated,
		EntityType: domain.EntityBatch,
		EntityID:   batch.ID,
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  req.PlantID,
		Payload: map[string]any{
			"batch_number": batchNumber, "product_type": req.ProductType, "input_litres": inputLitres,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetBatchProvenanceSeq(ctx, batch.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	batch.ProvenanceSeq = ev.Seq
	return batch, nil
}

// CompleteBatch transitions PASSED→COMPLETED — only a batch the plant lab
// cleared may finish and go on to yield product lots.
func (s *Service) CompleteBatch(ctx context.Context, actor auth.Actor, id string) (*domain.ProcessingBatch, error) {
	batch, err := s.getBatch(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, batch.PlantID); err != nil {
		return nil, err
	}
	if batch.Status == domain.BatchStatusCompleted {
		return nil, httpx.Conflict("BATCH_ALREADY_COMPLETED", "batch is already completed")
	}
	if !canCompleteBatch(batch.Status) {
		msg := "batch cannot complete: plant-lab QC not passed (status " + batch.Status + ")"
		if batch.BlockReason != "" {
			msg += " — " + batch.BlockReason
		}
		return nil, httpx.Unprocessable(codeSafetyGateBlocked, msg).
			WithDetails(map[string]any{"status": batch.Status, "block_reason": batch.BlockReason})
	}
	now := time.Now().UTC()
	ok, err := s.repo.CompleteBatch(ctx, id, now)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !ok {
		return nil, httpx.Conflict("BATCH_STATE_CHANGED",
			"batch changed state concurrently — reload and retry")
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBatchCompleted,
		EntityType: domain.EntityBatch,
		EntityID:   batch.ID,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  batch.PlantID,
		Payload:    map[string]any{"batch_number": batch.BatchNumber},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetBatchProvenanceSeq(ctx, batch.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	batch.Status = domain.BatchStatusCompleted
	batch.CompletedAt = &now
	batch.ProvenanceSeq = ev.Seq
	return batch, nil
}

// GetBatch returns one batch (including its qc_result_ids) within scope.
func (s *Service) GetBatch(ctx context.Context, actor auth.Actor, id string) (*domain.ProcessingBatch, error) {
	batch, err := s.getBatch(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, batch.PlantID); err != nil {
		return nil, err
	}
	return batch, nil
}

// ListBatches pages batches by plant/status.
func (s *Service) ListBatches(ctx context.Context, actor auth.Actor, f BatchListFilter, page httpx.Page) ([]domain.ProcessingBatch, int64, error) {
	if f.PlantID != "" {
		if err := s.orgs.RequireInScope(ctx, actor, f.PlantID); err != nil {
			return nil, 0, err
		}
	}
	batches, total, err := s.repo.ListBatches(ctx, f, page)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return batches, total, nil
}

// --- product lots ---

// CreateProductLot packages a COMPLETED batch into a SKU lot (gate re-check).
func (s *Service) CreateProductLot(ctx context.Context, actor auth.Actor, req CreateProductLotRequest) (*domain.ProductLot, error) {
	batch, err := s.getBatch(ctx, req.BatchID)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, batch.PlantID); err != nil {
		return nil, err
	}
	if !canYieldProductLot(batch.Status) {
		msg := "batch has not completed the safety gate (status " + batch.Status + ") — no product lots may be made"
		if batch.BlockReason != "" {
			msg += " — " + batch.BlockReason
		}
		return nil, httpx.Unprocessable(codeSafetyGateBlocked, msg).
			WithDetails(map[string]any{"batch_status": batch.Status, "block_reason": batch.BlockReason})
	}

	now := time.Now().UTC()
	mfgDate := req.MfgDate
	if mfgDate == "" {
		mfgDate = domain.DateKeyIST(now)
	}
	if req.ExpiryDate <= mfgDate {
		return nil, httpx.BadRequest("INVALID_EXPIRY", "expiry_date must be after mfg_date")
	}

	lot := &domain.ProductLot{
		ID:          uuid.NewString(),
		BatchID:     batch.ID,
		PlantID:     batch.PlantID,
		SKU:         req.SKU,
		ProductName: req.ProductName,
		Units:       req.Units,
		UnitSize:    req.UnitSize,
		MRP:         req.MRP,
		MfgDate:     mfgDate,
		ExpiryDate:  req.ExpiryDate,
		Status:      domain.ProductLotStatusActive,
		CreatedAt:   now,
	}
	if err := s.repo.InsertProductLot(ctx, lot); err != nil {
		return nil, httpx.Internal(err)
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventProductLotMade,
		EntityType: domain.EntityProductLot,
		EntityID:   lot.ID,
		Refs: []provenance.Ref{
			{EntityType: domain.EntityBatch, EntityID: batch.ID, Relation: "yields"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: batch.PlantID,
		Payload: map[string]any{
			"sku": req.SKU, "units": req.Units, "unit_size": req.UnitSize,
			"mfg_date": mfgDate, "expiry_date": req.ExpiryDate,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetProductLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	lot.ProvenanceSeq = ev.Seq
	return lot, nil
}

// RecallProductLot pulls a lot from market (FSSAI recall path, §18-C).
func (s *Service) RecallProductLot(ctx context.Context, actor auth.Actor, id, reason string) (*domain.ProductLot, error) {
	lot, err := s.getProductLot(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, lot.PlantID); err != nil {
		return nil, err
	}
	if lot.Status == domain.ProductLotStatusRecalled {
		return nil, httpx.Conflict("PRODUCT_LOT_ALREADY_RECALLED", "product lot is already recalled")
	}
	ok, err := s.repo.RecallProductLot(ctx, id, reason)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !ok {
		return nil, httpx.Conflict("PRODUCT_LOT_ALREADY_RECALLED", "product lot is already recalled")
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventProductRecalled,
		EntityType: domain.EntityProductLot,
		EntityID:   lot.ID,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  lot.PlantID,
		Payload:    map[string]any{"reason": reason},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetProductLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	lot.Status = domain.ProductLotStatusRecalled
	lot.RecallReason = reason
	lot.ProvenanceSeq = ev.Seq
	return lot, nil
}

// --- QR issuance ---

// qrAlphabet is the 32-character human-safe set for short codes: A–Z and 2–9
// minus the confusables 0/O and 1/I. 32 divides 256, so a byte modulo the
// alphabet length is unbiased.
const qrAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// qrCodeLength is the number of random characters after the "PRG-" prefix
// (32^8 ≈ 1.1 trillion codes — collisions are retried on the unique index).
const qrCodeLength = 8

// newQRCode mints a short public code like "PRG-7F3K9QX2" from crypto/rand.
func newQRCode() (string, error) {
	buf := make([]byte, qrCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	code := make([]byte, qrCodeLength)
	for i, b := range buf {
		code[i] = qrAlphabet[int(b)%len(qrAlphabet)]
	}
	return "PRG-" + string(code), nil
}

// signQRToken builds the forgery-proof token printed inside the QR:
// base64url(qr_code|product_lot_id|issued_unix) + "." + hex(HMAC-SHA256).
// The consumer app resolves the payload; the signature proves it was minted
// by this server, so IDs cannot be guessed onto counterfeit packs.
func signQRToken(secret, qrCode, productLotID string, issuedAt time.Time) string {
	payload := qrCode + "|" + productLotID + "|" + strconv.FormatInt(issuedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// parseQRToken verifies and unpacks a signed QR token.
func parseQRToken(secret, token string) (qrCode, productLotID string, issuedAt time.Time, err error) {
	payloadB64, sigHex, found := strings.Cut(token, ".")
	if !found {
		return "", "", time.Time{}, errors.New("qr token: missing signature separator")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("qr token: bad payload encoding: %w", err)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("qr token: bad signature encoding: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payloadBytes)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", "", time.Time{}, errors.New("qr token: signature mismatch")
	}
	parts := strings.Split(string(payloadBytes), "|")
	if len(parts) != 3 {
		return "", "", time.Time{}, errors.New("qr token: malformed payload")
	}
	unix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("qr token: bad timestamp: %w", err)
	}
	return parts[0], parts[1], time.Unix(unix, 0).UTC(), nil
}

// IssueQR mints one signed QR for an ACTIVE product lot whose batch is
// COMPLETED. The gate is checked on read AND re-confirmed with a conditional
// write on the lot after the QR insert, so recalled or blocked product can
// never be freshly labelled even when the recall races this request.
func (s *Service) IssueQR(ctx context.Context, actor auth.Actor, productLotID string) (*domain.BatchQR, error) {
	lot, err := s.getProductLot(ctx, productLotID)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, lot.PlantID); err != nil {
		return nil, err
	}
	batch, err := s.getBatch(ctx, lot.BatchID)
	if err != nil {
		return nil, err
	}
	if !canIssueQR(lot.Status, batch.Status) {
		return nil, httpx.Unprocessable(codeSafetyGateBlocked,
			"QR issuance refused: product lot must be ACTIVE and its batch COMPLETED").
			WithDetails(map[string]any{
				"product_lot_status": lot.Status,
				"batch_status":       batch.Status,
				"recall_reason":      lot.RecallReason,
			})
	}

	now := time.Now().UTC()
	var qr *domain.BatchQR
	const maxMintAttempts = 5
	for attempt := 0; attempt < maxMintAttempts; attempt++ {
		code, err := newQRCode()
		if err != nil {
			return nil, httpx.Internal(err)
		}
		candidate := &domain.BatchQR{
			ID:           uuid.NewString(),
			QRCode:       code,
			ProductLotID: lot.ID,
			SignedToken:  signQRToken(s.qrSecret, code, lot.ID, now),
			IssuedBy:     actor.PartyID,
			ScanCount:    0,
			IssuedAt:     now,
		}
		insertErr := s.repo.InsertQR(ctx, candidate)
		if insertErr == nil {
			qr = candidate
			break
		}
		if mongo.IsDuplicateKeyError(insertErr) {
			continue // short-code collision — regenerate
		}
		return nil, httpx.Internal(insertErr)
	}
	if qr == nil {
		return nil, httpx.Internal(fmt.Errorf("qr mint: %d consecutive short-code collisions", maxMintAttempts))
	}

	// Recall re-check at WRITE time: the initial read cannot see a recall that
	// lands mid-request, so the mint is confirmed with a conditional no-op
	// write on the lot (filter status=ACTIVE). If the lot left ACTIVE, the
	// just-inserted QR is deleted and issuance refused — a freshly minted QR
	// can never label recalled product.
	active, err := s.repo.TouchActiveLot(ctx, lot.ID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !active {
		if delErr := s.repo.DeleteQR(ctx, qr.ID); delErr != nil {
			return nil, httpx.Internal(delErr)
		}
		return nil, httpx.Unprocessable(codeSafetyGateBlocked,
			"QR issuance refused: product lot left ACTIVE (e.g. recalled) during issuance")
	}

	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventQRIssued,
		EntityType: domain.EntityBatchQR,
		EntityID:   qr.ID,
		Refs: []provenance.Ref{
			{EntityType: domain.EntityProductLot, EntityID: lot.ID, Relation: "labels"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: lot.PlantID,
		Payload:   map[string]any{"qr_code": qr.QRCode},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetQRProvenanceSeq(ctx, qr.ID, ev.Seq); err != nil {
		return nil, httpx.Internal(err)
	}
	qr.ProvenanceSeq = ev.Seq
	return qr, nil
}

// ListQRs pages QRs, optionally narrowed to one product lot (scope-checked
// through the lot's plant).
func (s *Service) ListQRs(ctx context.Context, actor auth.Actor, f QRListFilter, page httpx.Page) ([]domain.BatchQR, int64, error) {
	if f.ProductLotID != "" {
		lot, err := s.getProductLot(ctx, f.ProductLotID)
		if err != nil {
			return nil, 0, err
		}
		if err := s.orgs.RequireInScope(ctx, actor, lot.PlantID); err != nil {
			return nil, 0, err
		}
	}
	qrs, total, err := s.repo.ListQRs(ctx, f, page)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return qrs, total, nil
}

// --- small lookups + helpers ---

func (s *Service) getBMCLot(ctx context.Context, id string) (*domain.BMCLot, error) {
	lot, err := s.repo.BMCLotByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("BMC lot")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return lot, nil
}

func (s *Service) getBatch(ctx context.Context, id string) (*domain.ProcessingBatch, error) {
	batch, err := s.repo.BatchByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("processing batch")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return batch, nil
}

func (s *Service) getProductLot(ctx context.Context, id string) (*domain.ProductLot, error) {
	lot, err := s.repo.ProductLotByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("product lot")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return lot, nil
}

// dedupe removes duplicate IDs preserving first-seen order.
func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// missingIDs lists requested consignment IDs absent from the fetched set.
func missingIDs(wanted []string, got []domain.DCSConsignment) []string {
	have := make(map[string]bool, len(got))
	for _, c := range got {
		have[c.ID] = true
	}
	var missing []string
	for _, id := range wanted {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// missingLotIDs lists requested BMC lot IDs absent from the fetched set.
func missingLotIDs(wanted []string, got []domain.BMCLot) []string {
	have := make(map[string]bool, len(got))
	for _, l := range got {
		have[l.ID] = true
	}
	var missing []string
	for _, id := range wanted {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

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
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
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

// defaultShelfLifeDays is the fallback expiry window when a product-lot mint
// derives expiry from a product that carries no explicit shelf_life_days.
const defaultShelfLifeDays = 90

// Service holds the plant module's business logic: lot pooling, the safety
// gate, batch numbering, product lot packaging and QR minting.
type Service struct {
	repo     *Repo
	orgs     *orgscope.Resolver
	ledger   *provenance.Ledger
	qrSecret string
	log      *slog.Logger
}

// NewService wires the service from the shared dependency container and the
// module-scoped logger.
func NewService(repo *Repo, d *deps.Deps, log *slog.Logger) *Service {
	return &Service{
		repo:     repo,
		orgs:     d.Orgs,
		ledger:   d.Ledger,
		qrSecret: d.Cfg.QRSigningSecret,
		log:      log,
	}
}

// actorID parses the actor's party identity — carried as an ObjectID hex
// string in JWT claims — into the ObjectID persisted on created documents.
func actorID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// fail wraps an unexpected error with its failing operation, logs it at ERROR
// and returns the opaque 500 — every internal failure must name its op in the
// logs.
func (s *Service) fail(ctx context.Context, op string, err error) error {
	err = fmt.Errorf("%s: %w", op, err)
	s.log.ErrorContext(ctx, "plant operation failed",
		slog.String("op", op), slog.Any("err", err))
	return httpx.Internal(err)
}

// requireScope enforces the actor's org scope and logs denials at WARN so
// cross-scope probing is visible.
func (s *Service) requireScope(ctx context.Context, actor auth.Actor, targetOrgID primitive.ObjectID, op string) error {
	err := s.orgs.RequireInScope(ctx, actor, targetOrgID)
	if err == nil {
		return nil
	}
	var app *httpx.AppError
	if errors.As(err, &app) && app.Status == http.StatusForbidden {
		s.log.WarnContext(ctx, "org scope denied",
			slog.String("op", op),
			slog.String("target_org_id", targetOrgID.Hex()),
			slog.String("actor_party_id", actor.PartyID),
			slog.String("actor_role", actor.RoleCode))
	}
	return err
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
	actID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, req.BMCID, "create bmc lot"); err != nil {
		return nil, err
	}
	org, err := s.orgs.Get(ctx, req.BMCID)
	if err != nil {
		return nil, err
	}
	if org.Type != domain.OrgTypeBMC {
		s.log.WarnContext(ctx, "bmc lot creation refused: org unit is not a BMC",
			slog.String("org_unit_id", req.BMCID.Hex()),
			slog.String("org_type", org.Type),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.BadRequest("NOT_A_BMC", "org unit "+req.BMCID.Hex()+" is not a BMC")
	}

	now := time.Now().UTC()
	date := req.Date
	if date == "" {
		date = domain.DateKeyIST(now)
	}

	ids := dedupe(req.ConsignmentIDs)
	consignments, err := s.repo.ConsignmentsByIDs(ctx, ids)
	if err != nil {
		return nil, s.fail(ctx, "load consignments", err)
	}
	if len(consignments) != len(ids) {
		return nil, httpx.NotFound("consignment(s) " + strings.Join(missingIDs(ids, consignments), ", "))
	}
	var undelivered []string
	deliveryTripIDs := make([]primitive.ObjectID, 0, len(consignments))
	for _, c := range consignments {
		if c.Status != domain.ConsignmentStatusDelivered {
			undelivered = append(undelivered, c.ID.Hex())
		}
		if c.RouteTripID != nil {
			deliveryTripIDs = append(deliveryTripIDs, *c.RouteTripID)
		}
	}
	if len(undelivered) > 0 {
		s.log.WarnContext(ctx, "bmc lot creation refused: consignment(s) not DELIVERED",
			slog.String("bmc_id", req.BMCID.Hex()),
			slog.String("consignment_ids", strings.Join(undelivered, ",")),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("CONSIGNMENT_NOT_DELIVERED",
			"only DELIVERED consignments can be pooled into a BMC lot").
			WithDetails(map[string]any{"consignment_ids": undelivered})
	}

	// Integrity: every pooled consignment must have been physically delivered
	// to THIS BMC (its trip's delivered_to_bmc_id == req.BMCID) — a BMC cannot
	// pool milk that a van dropped at a different chilling centre.
	tripBMC, err := s.repo.TripDeliveryBMCByIDs(ctx, deliveryTripIDs)
	if err != nil {
		return nil, s.fail(ctx, "load trip delivery destinations", err)
	}
	var wrongBMC []string
	for _, c := range consignments {
		if c.RouteTripID == nil {
			continue // no trip recorded — cannot attribute a destination
		}
		if dest, ok := tripBMC[*c.RouteTripID]; !ok || dest != req.BMCID {
			wrongBMC = append(wrongBMC, c.ID.Hex())
		}
	}
	if len(wrongBMC) > 0 {
		s.log.WarnContext(ctx, "bmc lot creation refused: consignment(s) delivered to a different BMC",
			slog.String("bmc_id", req.BMCID.Hex()),
			slog.String("consignment_ids", strings.Join(wrongBMC, ",")),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("WRONG_DELIVERY_BMC",
			"one or more consignments were delivered to a different BMC and cannot be pooled here").
			WithDetails(map[string]any{"consignment_ids": wrongBMC})
	}

	// Optimistic claim: flip DELIVERED→ACCEPTED stamped with the new lot ID.
	// The lot ID is pre-generated so claim, insert and ledger all agree.
	// If another lot raced us on any consignment, release our claims and bail.
	lotID := primitive.NewObjectID()
	claimed, err := s.repo.ClaimConsignments(ctx, ids, lotID)
	if err != nil {
		return nil, s.fail(ctx, "claim consignments", err)
	}
	if claimed != int64(len(ids)) {
		if relErr := s.repo.ReleaseConsignments(ctx, lotID); relErr != nil {
			return nil, s.fail(ctx, "release consignments after lost claim", relErr)
		}
		s.log.WarnContext(ctx, "bmc lot creation lost consignment claim race",
			slog.String("bmc_id", req.BMCID.Hex()),
			slog.Int64("claimed", claimed), slog.Int("requested", len(ids)),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("CONSIGNMENT_CLAIM_CONFLICT",
			"one or more consignments were pooled by another lot — reload and retry")
	}

	var totalLitres float64
	var tripIDs []primitive.ObjectID
	seenTrip := map[primitive.ObjectID]bool{}
	for _, c := range consignments {
		totalLitres += c.TotalQuantityLitres
		if c.RouteTripID != nil && !seenTrip[*c.RouteTripID] {
			seenTrip[*c.RouteTripID] = true
			tripIDs = append(tripIDs, *c.RouteTripID)
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
		CreatedBy:           actID,
		CreatedAt:           now,
	}
	if err := s.repo.InsertBMCLot(ctx, lot); err != nil {
		_ = s.repo.ReleaseConsignments(ctx, lotID)
		return nil, s.fail(ctx, "insert bmc lot", err)
	}

	refs := make([]provenance.Ref, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, provenance.Ref{
			EntityType: domain.EntityConsignment, EntityID: id.Hex(), Relation: "pools",
		})
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBMCLotCreated,
		EntityType: domain.EntityBMCLot,
		EntityID:   lot.ID.Hex(),
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  req.BMCID.Hex(),
		Payload: map[string]any{
			"date": date, "shift": req.Shift, "total_quantity_litres": totalLitres,
		},
	})
	if err != nil {
		return nil, s.fail(ctx, "append bmc_lot.created event", err)
	}
	if err := s.repo.SetBMCLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp bmc lot provenance seq", err)
	}
	lot.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "bmc lot created",
		slog.String("bmc_lot_id", lot.ID.Hex()),
		slog.String("bmc_id", req.BMCID.Hex()),
		slog.String("date", date), slog.String("shift", req.Shift),
		slog.Int("consignments", len(ids)),
		slog.Float64("total_quantity_litres", totalLitres),
		slog.String("actor_party_id", actor.PartyID))
	return lot, nil
}

// CloseBMCLot transitions an OPEN lot to QC_PENDING with the chilling
// temperature. The quality module later flips QC_PENDING→PASSED/BLOCKED.
func (s *Service) CloseBMCLot(ctx context.Context, actor auth.Actor, id primitive.ObjectID, chillingTempC float64) (*domain.BMCLot, error) {
	lot, err := s.getBMCLot(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, lot.BMCID, "close bmc lot"); err != nil {
		return nil, err
	}
	if lot.Status != domain.BMCLotStatusOpen {
		s.log.WarnContext(ctx, "bmc lot close refused: not OPEN",
			slog.String("bmc_lot_id", id.Hex()), slog.String("status", lot.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BMC_LOT_NOT_OPEN",
			"only an OPEN lot can be closed; lot is "+lot.Status)
	}
	now := time.Now().UTC()
	ok, err := s.repo.CloseBMCLot(ctx, id, chillingTempC, now)
	if err != nil {
		return nil, s.fail(ctx, "close bmc lot", err)
	}
	if !ok {
		s.log.WarnContext(ctx, "bmc lot close lost state race",
			slog.String("bmc_lot_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BMC_LOT_NOT_OPEN",
			"lot changed state concurrently — reload and retry")
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBMCLotClosed,
		EntityType: domain.EntityBMCLot,
		EntityID:   lot.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  lot.BMCID.Hex(),
		Payload:    map[string]any{"chilling_temp_c": chillingTempC},
	})
	if err != nil {
		return nil, s.fail(ctx, "append bmc_lot.closed event", err)
	}
	if err := s.repo.SetBMCLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp bmc lot provenance seq", err)
	}
	lot.Status = domain.BMCLotStatusQCPending
	lot.ChillingTempC = chillingTempC
	lot.ClosedAt = &now
	lot.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "bmc lot closed for QC",
		slog.String("bmc_lot_id", lot.ID.Hex()),
		slog.String("bmc_id", lot.BMCID.Hex()),
		slog.Float64("chilling_temp_c", chillingTempC),
		slog.String("actor_party_id", actor.PartyID))
	return lot, nil
}

// DispatchBMCLot transitions PASSED→DISPATCHED. THE safety gate (§8.3): a
// BLOCKED or untested lot never leaves the chilling centre.
func (s *Service) DispatchBMCLot(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*domain.BMCLot, error) {
	lot, err := s.getBMCLot(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, lot.BMCID, "dispatch bmc lot"); err != nil {
		return nil, err
	}
	if lot.Status == domain.BMCLotStatusDispatched {
		s.log.WarnContext(ctx, "bmc lot dispatch refused: already dispatched",
			slog.String("bmc_lot_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BMC_LOT_ALREADY_DISPATCHED", "lot is already dispatched")
	}
	if !canDispatchBMCLot(lot.Status) {
		s.log.WarnContext(ctx, "bmc lot dispatch refused: safety gate not passed",
			slog.String("bmc_lot_id", id.Hex()),
			slog.String("status", lot.Status),
			slog.String("block_reason", lot.BlockReason),
			slog.String("actor_party_id", actor.PartyID))
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
		return nil, s.fail(ctx, "dispatch bmc lot", err)
	}
	if !ok {
		s.log.WarnContext(ctx, "bmc lot dispatch lost state race",
			slog.String("bmc_lot_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BMC_LOT_STATE_CHANGED",
			"lot changed state concurrently — reload and retry")
	}
	// The custody transition — gate-passed milk physically leaving the
	// chilling centre — must live on the tamper-evident chain like every
	// other hop of the pour→QR path, not only in a mutable document field.
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBMCLotDispatched,
		EntityType: domain.EntityBMCLot,
		EntityID:   lot.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  lot.BMCID.Hex(),
		Payload: map[string]any{
			// String, not time.Time: payloads must round-trip BSON⇄JSON
			// byte-identically or chain re-verification would break.
			"dispatched_at":         now.Format(time.RFC3339Nano),
			"total_quantity_litres": lot.TotalQuantityLitres,
		},
	})
	if err != nil {
		return nil, s.fail(ctx, "append bmc_lot.dispatched event", err)
	}
	if err := s.repo.SetBMCLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp bmc lot provenance seq", err)
	}
	lot.Status = domain.BMCLotStatusDispatched
	lot.DispatchedAt = &now
	lot.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "bmc lot dispatched",
		slog.String("bmc_lot_id", lot.ID.Hex()),
		slog.String("bmc_id", lot.BMCID.Hex()),
		slog.Float64("total_quantity_litres", lot.TotalQuantityLitres),
		slog.String("actor_party_id", actor.PartyID))
	return lot, nil
}

// ListBMCLots pages lots by BMC/date/status. A BMC operator may only list
// within their own scope; downstream readers (plant operator, supervisor,
// lab) see across BMCs — the plant receives tankers from sibling BMCs, so an
// ancestry check against the plant org would wrongly refuse them.
func (s *Service) ListBMCLots(ctx context.Context, actor auth.Actor, f BMCLotListFilter, page httpx.Page) ([]domain.BMCLot, int64, error) {
	if !f.BMCID.IsZero() && actor.RoleCode == domain.RoleBMCOperator {
		if err := s.requireScope(ctx, actor, f.BMCID, "list bmc lots"); err != nil {
			return nil, 0, err
		}
	}
	lots, total, err := s.repo.ListBMCLots(ctx, f, page)
	if err != nil {
		return nil, 0, s.fail(ctx, "list bmc lots", err)
	}
	for i := range lots {
		s.enrichBMCLot(ctx, &lots[i])
	}
	return lots, total, nil
}

// enrichBMCLot fills a lot's read-time fields: a human silo-lot code and the
// §7.4 pooling-honesty contributor set (society ids + display names). Best
// effort — a directory miss leaves a name out but never fails the listing.
func (s *Service) enrichBMCLot(ctx context.Context, lot *domain.BMCLot) {
	lot.SiloLotCode = siloLotCode(lot)
	ids, err := s.repo.DistinctDCSIDsForConsignments(ctx, lot.ConsignmentIDs)
	if err != nil {
		s.log.WarnContext(ctx, "bmc lot enrichment: contributor resolve failed",
			slog.String("bmc_lot_id", lot.ID.Hex()), slog.Any("err", err))
		return
	}
	lot.ContributingDCSIDs = ids
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if org, oerr := s.orgs.Get(ctx, id); oerr == nil && org != nil {
			names = append(names, org.Name)
		}
	}
	lot.ContributingDCSNames = names
}

// siloLotCode synthesises a human-readable silo-lot code
// LOT-<YYYYMMDD>-<M|E>-<idsuffix> for a BMC lot (the domain carries no explicit
// code) so the branch console always has a non-empty handle to show.
func siloLotCode(lot *domain.BMCLot) string {
	shift := "M"
	if lot.Shift == domain.ShiftEvening {
		shift = "E"
	}
	day := strings.ReplaceAll(lot.Date, "-", "")
	suffix := lot.ID.Hex()
	if len(suffix) >= 6 {
		suffix = suffix[len(suffix)-6:]
	}
	return "LOT-" + day + "-" + shift + "-" + suffix
}

// --- processing batches ---

// CreateBatch pools DISPATCHED (gate-passed) BMC lots into one production
// run. The gate re-checks here belt-and-braces: aflatoxin M1 survives
// pasteurisation, so a blocked lot must never reach a vat (§8.3).
func (s *Service) CreateBatch(ctx context.Context, actor auth.Actor, req CreateBatchRequest) (*domain.ProcessingBatch, error) {
	actID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, req.PlantID, "create batch"); err != nil {
		return nil, err
	}
	org, err := s.orgs.Get(ctx, req.PlantID)
	if err != nil {
		return nil, err
	}
	if org.Type != domain.OrgTypeProcessingPlant {
		s.log.WarnContext(ctx, "batch creation refused: org unit is not a plant",
			slog.String("org_unit_id", req.PlantID.Hex()),
			slog.String("org_type", org.Type),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.BadRequest("NOT_A_PLANT", "org unit "+req.PlantID.Hex()+" is not a processing plant")
	}

	ids := dedupe(req.BMCLotIDs)
	lots, err := s.repo.BMCLotsByIDs(ctx, ids)
	if err != nil {
		return nil, s.fail(ctx, "load bmc lots", err)
	}
	if len(lots) != len(ids) {
		return nil, httpx.NotFound("BMC lot(s) " + strings.Join(missingLotIDs(ids, lots), ", "))
	}
	// Scope root the plant may pool from: its parent union (siblings' BMCs are
	// legitimate), or the plant itself when it sits directly under the federation.
	plantScopeRoot := org.ID
	if org.ParentID != nil {
		plantScopeRoot = *org.ParentID
	}
	var offending, foreign []string
	var inputLitres float64
	for _, lot := range lots {
		if !canPoolBMCLot(lot.Status) {
			offending = append(offending, lot.ID.Hex())
			continue
		}
		// A plant may only pool BMC lots from within its own union subtree —
		// never claim another union's foreign lot.
		inScope, serr := s.orgs.InScope(ctx, plantScopeRoot, lot.BMCID)
		if serr != nil {
			return nil, serr
		}
		if !inScope {
			foreign = append(foreign, lot.ID.Hex())
			continue
		}
		inputLitres += lot.TotalQuantityLitres
	}
	if len(offending) > 0 {
		s.log.WarnContext(ctx, "batch creation refused: bmc lot(s) have not cleared the safety gate",
			slog.String("plant_id", req.PlantID.Hex()),
			slog.String("blocked_lot_ids", strings.Join(offending, ",")),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable(codeSafetyGateBlocked,
			"one or more BMC lots have not cleared the safety gate (must be PASSED and DISPATCHED)").
			WithDetails(map[string]any{"blocked_lot_ids": offending})
	}
	if len(foreign) > 0 {
		s.log.WarnContext(ctx, "batch creation refused: bmc lot(s) outside the plant's union scope",
			slog.String("plant_id", req.PlantID.Hex()),
			slog.String("foreign_lot_ids", strings.Join(foreign, ",")),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Forbidden("one or more BMC lots are outside this plant's organisational scope").
			WithDetails(map[string]any{"foreign_lot_ids": foreign})
	}

	// §7.4 honest set-valued pooling: past the consignment boundary milk no
	// longer traces to a single society, so materialise the DISTINCT set of
	// contributing DCS at creation. Walk every poolable lot's consignments and
	// collapse to unique dcs_id — the consumer trace reads "made from N samitis"
	// without re-walking the graph.
	var consignmentIDs []primitive.ObjectID
	for _, lot := range lots {
		consignmentIDs = append(consignmentIDs, lot.ConsignmentIDs...)
	}
	contributingDCSIDs, err := s.repo.DistinctDCSIDsForConsignments(ctx, dedupe(consignmentIDs))
	if err != nil {
		return nil, s.fail(ctx, "resolve contributing societies", err)
	}

	now := time.Now().UTC()
	dateKey := domain.DateKeyIST(now)
	seq, err := s.repo.NextSequence(ctx, "batch_number:"+req.PlantID.Hex()+":"+dateKey)
	if err != nil {
		return nil, s.fail(ctx, "next batch number sequence", err)
	}
	batchNumber := fmt.Sprintf("BATCH-%s-%s-%04d",
		org.Code, strings.ReplaceAll(dateKey, "-", ""), seq)

	// Optimistic claim (mirrors ClaimConsignments): flip DISPATCHED→POOLED
	// stamped with the pre-generated batch ID BEFORE inserting the batch. One
	// physical lot of milk can therefore never be pooled into two batches —
	// neither by a double-submit nor by two concurrent CreateBatch calls.
	batchID := primitive.NewObjectID()
	claimed, err := s.repo.ClaimBMCLots(ctx, ids, batchID)
	if err != nil {
		return nil, s.fail(ctx, "claim bmc lots", err)
	}
	if claimed != int64(len(ids)) {
		if relErr := s.repo.ReleaseBMCLots(ctx, batchID); relErr != nil {
			return nil, s.fail(ctx, "release bmc lots after lost claim", relErr)
		}
		s.log.WarnContext(ctx, "batch creation lost bmc lot claim race",
			slog.String("plant_id", req.PlantID.Hex()),
			slog.Int64("claimed", claimed), slog.Int("requested", len(ids)),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BMC_LOT_CLAIM_CONFLICT",
			"one or more BMC lots were pooled by another batch — reload and retry")
	}

	// CREATED is a transient birth state: the plant lab must clear every
	// batch, so it is persisted straight into QC_PENDING.
	batch := &domain.ProcessingBatch{
		ID:                 batchID,
		PlantID:            req.PlantID,
		BatchNumber:        batchNumber,
		BMCLotIDs:          ids,
		ContributingDCSIDs: contributingDCSIDs,
		ProductType:        req.ProductType,
		InputLitres:        inputLitres,
		Status:             domain.BatchStatusQCPending,
		StartedAt:          now,
		CreatedBy:          actID,
		CreatedAt:          now,
	}
	if err := s.repo.InsertBatch(ctx, batch); err != nil {
		_ = s.repo.ReleaseBMCLots(ctx, batchID)
		return nil, s.fail(ctx, "insert batch", err)
	}

	refs := make([]provenance.Ref, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, provenance.Ref{
			EntityType: domain.EntityBMCLot, EntityID: id.Hex(), Relation: "pools",
		})
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBatchCreated,
		EntityType: domain.EntityBatch,
		EntityID:   batch.ID.Hex(),
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  req.PlantID.Hex(),
		Payload: map[string]any{
			"batch_number": batchNumber, "product_type": req.ProductType, "input_litres": inputLitres,
			"contributing_society_count": len(contributingDCSIDs),
		},
	})
	if err != nil {
		return nil, s.fail(ctx, "append batch.created event", err)
	}
	if err := s.repo.SetBatchProvenanceSeq(ctx, batch.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp batch provenance seq", err)
	}
	batch.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "processing batch created",
		slog.String("batch_id", batch.ID.Hex()),
		slog.String("batch_number", batchNumber),
		slog.String("plant_id", req.PlantID.Hex()),
		slog.String("product_type", req.ProductType),
		slog.Int("bmc_lots", len(ids)),
		slog.Int("contributing_societies", len(contributingDCSIDs)),
		slog.Float64("input_litres", inputLitres),
		slog.String("actor_party_id", actor.PartyID))
	return batch, nil
}

// CompleteBatch transitions PASSED→COMPLETED — only a batch the plant lab
// cleared may finish and go on to yield product lots.
func (s *Service) CompleteBatch(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*domain.ProcessingBatch, error) {
	batch, err := s.getBatch(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, batch.PlantID, "complete batch"); err != nil {
		return nil, err
	}
	if batch.Status == domain.BatchStatusCompleted {
		s.log.WarnContext(ctx, "batch completion refused: already completed",
			slog.String("batch_id", id.Hex()),
			slog.String("batch_number", batch.BatchNumber),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BATCH_ALREADY_COMPLETED", "batch is already completed")
	}
	if !canCompleteBatch(batch.Status) {
		s.log.WarnContext(ctx, "batch completion refused: plant-lab QC not passed",
			slog.String("batch_id", id.Hex()),
			slog.String("batch_number", batch.BatchNumber),
			slog.String("status", batch.Status),
			slog.String("block_reason", batch.BlockReason),
			slog.String("actor_party_id", actor.PartyID))
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
		return nil, s.fail(ctx, "complete batch", err)
	}
	if !ok {
		s.log.WarnContext(ctx, "batch completion lost state race",
			slog.String("batch_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("BATCH_STATE_CHANGED",
			"batch changed state concurrently — reload and retry")
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventBatchCompleted,
		EntityType: domain.EntityBatch,
		EntityID:   batch.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  batch.PlantID.Hex(),
		Payload:    map[string]any{"batch_number": batch.BatchNumber},
	})
	if err != nil {
		return nil, s.fail(ctx, "append batch.completed event", err)
	}
	if err := s.repo.SetBatchProvenanceSeq(ctx, batch.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp batch provenance seq", err)
	}
	batch.Status = domain.BatchStatusCompleted
	batch.CompletedAt = &now
	batch.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "processing batch completed",
		slog.String("batch_id", batch.ID.Hex()),
		slog.String("batch_number", batch.BatchNumber),
		slog.String("plant_id", batch.PlantID.Hex()),
		slog.String("actor_party_id", actor.PartyID))
	return batch, nil
}

// GetBatch returns one batch (including its qc_result_ids) within scope.
func (s *Service) GetBatch(ctx context.Context, actor auth.Actor, id primitive.ObjectID) (*domain.ProcessingBatch, error) {
	batch, err := s.getBatch(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, batch.PlantID, "get batch"); err != nil {
		return nil, err
	}
	// Read-time enrichment: the id of the QC certificate issued for this batch
	// (if any), so the lab console drives issued-state off a real lookup rather
	// than re-inviting a duplicate issue. Best effort — never fails the read.
	if certID, cerr := s.repo.CertificateIDForBatch(ctx, batch.ID); cerr != nil {
		s.log.WarnContext(ctx, "get batch: certificate lookup failed",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", cerr))
	} else {
		batch.CertificateID = certID
	}
	return batch, nil
}

// ListProductLots pages the product lots yielded by one batch (scope-checked
// through the batch's plant) — the plant/lab console's read of a completed
// batch's packaged outputs.
func (s *Service) ListProductLots(ctx context.Context, actor auth.Actor, batchID primitive.ObjectID, page httpx.Page) ([]domain.ProductLot, int64, error) {
	batch, err := s.getBatch(ctx, batchID)
	if err != nil {
		return nil, 0, err
	}
	if err := s.requireScope(ctx, actor, batch.PlantID, "list product lots"); err != nil {
		return nil, 0, err
	}
	lots, total, err := s.repo.ProductLotsByBatch(ctx, batchID, page)
	if err != nil {
		return nil, 0, s.fail(ctx, "list product lots", err)
	}
	return lots, total, nil
}

// ListBatches pages batches by plant/status.
func (s *Service) ListBatches(ctx context.Context, actor auth.Actor, f BatchListFilter, page httpx.Page) ([]domain.ProcessingBatch, int64, error) {
	if !f.PlantID.IsZero() {
		if err := s.requireScope(ctx, actor, f.PlantID, "list batches"); err != nil {
			return nil, 0, err
		}
	}
	batches, total, err := s.repo.ListBatches(ctx, f, page)
	if err != nil {
		return nil, 0, s.fail(ctx, "list batches", err)
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
	if err := s.requireScope(ctx, actor, batch.PlantID, "create product lot"); err != nil {
		return nil, err
	}
	if !canYieldProductLot(batch.Status) {
		s.log.WarnContext(ctx, "product lot creation refused: batch has not completed the safety gate",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("batch_number", batch.BatchNumber),
			slog.String("batch_status", batch.Status),
			slog.String("block_reason", batch.BlockReason),
			slog.String("actor_party_id", actor.PartyID))
		msg := "batch has not completed the safety gate (status " + batch.Status + ") — no product lots may be made"
		if batch.BlockReason != "" {
			msg += " — " + batch.BlockReason
		}
		return nil, httpx.Unprocessable(codeSafetyGateBlocked, msg).
			WithDetails(map[string]any{"batch_status": batch.Status, "block_reason": batch.BlockReason})
	}

	// When a product_id is supplied, the product master is the source of truth
	// for the SKU metadata (the frontend pack sheet only picks a product); any
	// explicit sku/name/unit_size on the request are overridden by the catalogue.
	if !req.ProductID.IsZero() {
		product, perr := s.repo.ProductByID(ctx, req.ProductID)
		if errors.Is(perr, mongo.ErrNoDocuments) {
			return nil, httpx.NotFound("product " + req.ProductID.Hex())
		}
		if perr != nil {
			return nil, s.fail(ctx, "load product for lot", perr)
		}
		req.SKU = product.SKU
		req.ProductName = product.Name
		req.UnitSize = product.UnitSize
		if req.MRP == 0 {
			req.MRP = product.MRP
		}
		if req.ExpiryDate == "" {
			shelf := product.ShelfLifeDays
			if shelf <= 0 {
				shelf = defaultShelfLifeDays
			}
			mfg := req.MfgDate
			if mfg == "" {
				mfg = domain.DateKeyIST(time.Now().UTC())
			}
			if t, terr := time.Parse("2006-01-02", mfg); terr == nil {
				req.ExpiryDate = t.AddDate(0, 0, shelf).Format("2006-01-02")
			}
		}
	}

	now := time.Now().UTC()
	mfgDate := req.MfgDate
	if mfgDate == "" {
		mfgDate = domain.DateKeyIST(now)
	}
	if req.ExpiryDate == "" {
		return nil, httpx.BadRequest("MISSING_FIELD", "expiry_date is required (or send a product_id with a shelf life)")
	}
	if req.ExpiryDate <= mfgDate {
		return nil, httpx.BadRequest("INVALID_EXPIRY", "expiry_date must be after mfg_date")
	}

	lot := &domain.ProductLot{
		ID:          primitive.NewObjectID(),
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
		return nil, s.fail(ctx, "insert product lot", err)
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventProductLotMade,
		EntityType: domain.EntityProductLot,
		EntityID:   lot.ID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityBatch, EntityID: batch.ID.Hex(), Relation: "yields"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: batch.PlantID.Hex(),
		Payload: map[string]any{
			"sku": req.SKU, "units": req.Units, "unit_size": req.UnitSize,
			"mfg_date": mfgDate, "expiry_date": req.ExpiryDate,
		},
	})
	if err != nil {
		return nil, s.fail(ctx, "append product_lot.created event", err)
	}
	if err := s.repo.SetProductLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp product lot provenance seq", err)
	}
	lot.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "product lot created",
		slog.String("product_lot_id", lot.ID.Hex()),
		slog.String("batch_id", batch.ID.Hex()),
		slog.String("batch_number", batch.BatchNumber),
		slog.String("sku", req.SKU),
		slog.Int("units", req.Units),
		slog.String("actor_party_id", actor.PartyID))
	return lot, nil
}

// RecallProductLot pulls a lot from market (FSSAI recall path, §18-C).
func (s *Service) RecallProductLot(ctx context.Context, actor auth.Actor, id primitive.ObjectID, reason string) (*domain.ProductLot, error) {
	lot, err := s.getProductLot(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, lot.PlantID, "recall product lot"); err != nil {
		return nil, err
	}
	if lot.Status == domain.ProductLotStatusRecalled {
		s.log.WarnContext(ctx, "product lot recall refused: already recalled",
			slog.String("product_lot_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("PRODUCT_LOT_ALREADY_RECALLED", "product lot is already recalled")
	}
	ok, err := s.repo.RecallProductLot(ctx, id, reason)
	if err != nil {
		return nil, s.fail(ctx, "recall product lot", err)
	}
	if !ok {
		s.log.WarnContext(ctx, "product lot recall lost state race: already recalled",
			slog.String("product_lot_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("PRODUCT_LOT_ALREADY_RECALLED", "product lot is already recalled")
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventProductRecalled,
		EntityType: domain.EntityProductLot,
		EntityID:   lot.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  lot.PlantID.Hex(),
		Payload:    map[string]any{"reason": reason},
	})
	if err != nil {
		return nil, s.fail(ctx, "append product_lot.recalled event", err)
	}
	if err := s.repo.SetProductLotProvenanceSeq(ctx, lot.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp product lot provenance seq", err)
	}
	lot.Status = domain.ProductLotStatusRecalled
	lot.RecallReason = reason
	lot.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "product lot recalled",
		slog.String("product_lot_id", lot.ID.Hex()),
		slog.String("plant_id", lot.PlantID.Hex()),
		slog.String("sku", lot.SKU),
		slog.String("reason", reason),
		slog.String("actor_party_id", actor.PartyID))
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
// base64url(qr_code|product_lot_hex|issued_unix) + "." + hex(HMAC-SHA256).
// productLotHex is the product lot ObjectID's hex form — the HMAC material is
// "qr_code|lot hex|unix", and the publictrace module verifies the exact same
// shape. The consumer app resolves the payload; the signature proves it was
// minted by this server, so IDs cannot be guessed onto counterfeit packs.
func signQRToken(secret, qrCode, productLotHex string, issuedAt time.Time) string {
	payload := qrCode + "|" + productLotHex + "|" + strconv.FormatInt(issuedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// parseQRToken verifies and unpacks a signed QR token, returning the product
// lot ObjectID as its hex string.
func parseQRToken(secret, token string) (qrCode, productLotHex string, issuedAt time.Time, err error) {
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
func (s *Service) IssueQR(ctx context.Context, actor auth.Actor, productLotID primitive.ObjectID) (*domain.BatchQR, error) {
	actID, err := actorID(actor)
	if err != nil {
		return nil, err
	}
	lot, err := s.getProductLot(ctx, productLotID)
	if err != nil {
		return nil, err
	}
	if err := s.requireScope(ctx, actor, lot.PlantID, "issue qr"); err != nil {
		return nil, err
	}
	batch, err := s.getBatch(ctx, lot.BatchID)
	if err != nil {
		return nil, err
	}
	if !canIssueQR(lot.Status, batch.Status) {
		s.log.WarnContext(ctx, "qr issuance refused: safety gate",
			slog.String("product_lot_id", productLotID.Hex()),
			slog.String("product_lot_status", lot.Status),
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("batch_status", batch.Status),
			slog.String("recall_reason", lot.RecallReason),
			slog.String("actor_party_id", actor.PartyID))
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
			return nil, s.fail(ctx, "generate qr code", err)
		}
		candidate := &domain.BatchQR{
			ID:           primitive.NewObjectID(),
			QRCode:       code,
			ProductLotID: lot.ID,
			SignedToken:  signQRToken(s.qrSecret, code, lot.ID.Hex(), now),
			IssuedBy:     actID,
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
		return nil, s.fail(ctx, "insert qr", insertErr)
	}
	if qr == nil {
		return nil, s.fail(ctx, "mint qr code",
			fmt.Errorf("%d consecutive short-code collisions", maxMintAttempts))
	}

	// Recall re-check at WRITE time: the initial read cannot see a recall that
	// lands mid-request, so the mint is confirmed with a conditional no-op
	// write on the lot (filter status=ACTIVE). If the lot left ACTIVE, the
	// just-inserted QR is deleted and issuance refused — a freshly minted QR
	// can never label recalled product.
	active, err := s.repo.TouchActiveLot(ctx, lot.ID)
	if err != nil {
		return nil, s.fail(ctx, "confirm product lot active", err)
	}
	if !active {
		if delErr := s.repo.DeleteQR(ctx, qr.ID); delErr != nil {
			return nil, s.fail(ctx, "delete qr after lot left ACTIVE", delErr)
		}
		s.log.WarnContext(ctx, "qr issuance refused: product lot left ACTIVE during issuance",
			slog.String("product_lot_id", lot.ID.Hex()),
			slog.String("qr_id", qr.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable(codeSafetyGateBlocked,
			"QR issuance refused: product lot left ACTIVE (e.g. recalled) during issuance")
	}

	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventQRIssued,
		EntityType: domain.EntityBatchQR,
		EntityID:   qr.ID.Hex(),
		Refs: []provenance.Ref{
			{EntityType: domain.EntityProductLot, EntityID: lot.ID.Hex(), Relation: "labels"},
		},
		Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID: lot.PlantID.Hex(),
		Payload:   map[string]any{"qr_code": qr.QRCode},
	})
	if err != nil {
		return nil, s.fail(ctx, "append qr.issued event", err)
	}
	if err := s.repo.SetQRProvenanceSeq(ctx, qr.ID, ev.Seq); err != nil {
		return nil, s.fail(ctx, "stamp qr provenance seq", err)
	}
	qr.ProvenanceSeq = ev.Seq
	s.log.InfoContext(ctx, "qr issued",
		slog.String("qr_id", qr.ID.Hex()),
		slog.String("qr_code", qr.QRCode),
		slog.String("product_lot_id", lot.ID.Hex()),
		slog.String("plant_id", lot.PlantID.Hex()),
		slog.String("actor_party_id", actor.PartyID))
	return qr, nil
}

// ListQRs pages QRs, narrowed either to one product lot or to a whole batch's
// product lots (scope-checked through the lot's / batch's plant).
func (s *Service) ListQRs(ctx context.Context, actor auth.Actor, f QRListFilter, batchID primitive.ObjectID, page httpx.Page) ([]domain.BatchQR, int64, error) {
	switch {
	case !f.ProductLotID.IsZero():
		lot, err := s.getProductLot(ctx, f.ProductLotID)
		if err != nil {
			return nil, 0, err
		}
		if err := s.requireScope(ctx, actor, lot.PlantID, "list qrs"); err != nil {
			return nil, 0, err
		}
	case !batchID.IsZero():
		batch, err := s.getBatch(ctx, batchID)
		if err != nil {
			return nil, 0, err
		}
		if err := s.requireScope(ctx, actor, batch.PlantID, "list qrs"); err != nil {
			return nil, 0, err
		}
		lotIDs, err := s.repo.ProductLotIDsByBatch(ctx, batchID)
		if err != nil {
			return nil, 0, s.fail(ctx, "resolve batch product lots", err)
		}
		if len(lotIDs) == 0 {
			return []domain.BatchQR{}, 0, nil
		}
		f.ProductLotIDs = lotIDs
	}
	qrs, total, err := s.repo.ListQRs(ctx, f, page)
	if err != nil {
		return nil, 0, s.fail(ctx, "list qrs", err)
	}
	return qrs, total, nil
}

// --- small lookups + helpers ---

func (s *Service) getBMCLot(ctx context.Context, id primitive.ObjectID) (*domain.BMCLot, error) {
	lot, err := s.repo.BMCLotByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("BMC lot")
	}
	if err != nil {
		return nil, s.fail(ctx, "load bmc lot", err)
	}
	return lot, nil
}

func (s *Service) getBatch(ctx context.Context, id primitive.ObjectID) (*domain.ProcessingBatch, error) {
	batch, err := s.repo.BatchByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("processing batch")
	}
	if err != nil {
		return nil, s.fail(ctx, "load batch", err)
	}
	return batch, nil
}

func (s *Service) getProductLot(ctx context.Context, id primitive.ObjectID) (*domain.ProductLot, error) {
	lot, err := s.repo.ProductLotByID(ctx, id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("product lot")
	}
	if err != nil {
		return nil, s.fail(ctx, "load product lot", err)
	}
	return lot, nil
}

// dedupe removes zero and duplicate IDs preserving first-seen order.
func dedupe(ids []primitive.ObjectID) []primitive.ObjectID {
	seen := make(map[primitive.ObjectID]bool, len(ids))
	out := make([]primitive.ObjectID, 0, len(ids))
	for _, id := range ids {
		if id.IsZero() || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// missingIDs lists requested consignment IDs (as hex) absent from the fetched
// set.
func missingIDs(wanted []primitive.ObjectID, got []domain.DCSConsignment) []string {
	have := make(map[primitive.ObjectID]bool, len(got))
	for _, c := range got {
		have[c.ID] = true
	}
	var missing []string
	for _, id := range wanted {
		if !have[id] {
			missing = append(missing, id.Hex())
		}
	}
	return missing
}

// missingLotIDs lists requested BMC lot IDs (as hex) absent from the fetched
// set.
func missingLotIDs(wanted []primitive.ObjectID, got []domain.BMCLot) []string {
	have := make(map[primitive.ObjectID]bool, len(got))
	for _, l := range got {
		have[l.ID] = true
	}
	var missing []string
	for _, id := range wanted {
		if !have[id] {
			missing = append(missing, id.Hex())
		}
	}
	return missing
}

package settlement

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
)

// initiateRefsCap bounds how many invoice refs the settlement.initiated
// ledger event carries (per module spec: cap 300).
const initiateRefsCap = 300

// service holds all settlement business logic: the §8.1/§18-B payment
// guardrail (compute+initiate → dual-control approve → licensed-PA execute)
// and the strictly separate DBT subsidy rail (§13).
type service struct {
	repo   *repo
	ledger *provenance.Ledger
	orgs   *orgscope.Resolver
	bus    *eventbus.Bus
	log    *slog.Logger
}

// actorObjectID parses the actor's party identity. JWT claims carry ObjectIDs
// as hex strings (tokens are JSON); every service method that compares or
// stores the actor's identity goes through this one helper.
func actorObjectID(actor auth.Actor) (primitive.ObjectID, error) {
	return httpx.ParseID(actor.PartyID, "actor")
}

// ---- pure guardrail checks (unit-tested without a database) ----

// validateApproval enforces the dual-control guardrail (§8.1/§18-B): the
// approving party MUST differ from the initiator — checked FIRST and on the
// party identity itself, so not even SUPER_ADMIN's role bypass can approve a
// batch it initiated. Then the batch must still be awaiting approval.
func validateApproval(b domain.SettlementBatch, approverPartyID primitive.ObjectID) error {
	if b.InitiatedBy == approverPartyID {
		appErr := httpx.Forbidden("dual-control violation: the settlement initiator cannot approve their own batch (§8.1/§18-B)").
			WithDetails(map[string]string{"reason": "DUAL_CONTROL_VIOLATION"})
		appErr.Code = "DUAL_CONTROL_VIOLATION"
		return appErr
	}
	if b.Status != domain.SettlementStatusPendingApproval {
		return httpx.Conflict("INVALID_STATUS", "settlement batch is "+b.Status+", expected "+domain.SettlementStatusPendingApproval)
	}
	return nil
}

// executionLease is how long an APPROVED→EXECUTING claim is considered live.
// A batch stuck EXECUTING longer than this (crashed executor) may be resumed —
// the per-invoice payout logic is idempotent, so a replay is safe.
const executionLease = 2 * time.Minute

// validateExecution gates which states may (re-)enter execution:
//   - APPROVED:  the normal dual-controlled first run.
//   - EXECUTING: resume of an interrupted run (the lease check in
//     claimExecution prevents stealing a live executor's batch).
//   - FAILED:    re-execution after a recorded mid-run failure.
//
// Anything pre-approval is a guardrail breach, not a race; EXECUTED/REJECTED
// are terminal.
func validateExecution(b domain.SettlementBatch) error {
	switch b.Status {
	case domain.SettlementStatusApproved, domain.SettlementStatusExecuting, domain.SettlementStatusFailed:
		return nil
	default:
		return httpx.Conflict("INVALID_STATUS", "settlement batch is "+b.Status+", must be "+domain.SettlementStatusApproved+" before execution")
	}
}

// mockUTR builds a mock bank transaction reference: "UTRMOCK" + 12 hex chars.
func mockUTR() string {
	return "UTRMOCK" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
}

// mockPARef builds a mock payment-aggregator batch reference.
func mockPARef() string {
	return "PA-MOCK-" + uuid.NewString()[:8]
}

// mockPFMSRef builds a mock PFMS acknowledgement reference (§13).
func mockPFMSRef() string {
	return "PFMS-MOCK-" + uuid.NewString()[:8]
}

// ---- settlement lifecycle ----

// Initiate collects a DCS day's ISSUED invoices into a new settlement batch
// and parks it PENDING_APPROVAL. The INITIATED status is momentary by design
// (§8.1): computation and initiation are one Saathi action; nothing proceeds
// until a different human approves.
func (s *service) Initiate(ctx context.Context, actor auth.Actor, in InitiateSettlementRequest) (*domain.SettlementBatch, error) {
	actorID, err := actorObjectID(actor)
	if err != nil {
		return nil, err
	}
	if in.DCSID.IsZero() {
		return nil, httpx.BadRequest("MISSING_FIELD", "dcs_id is required")
	}
	now := time.Now().UTC()
	date := in.Date
	if date == "" {
		date = domain.DateKeyIST(now)
	} else if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, httpx.BadRequest("INVALID_DATE", "date must be YYYY-MM-DD")
	}
	if err := s.orgs.RequireInScope(ctx, actor, in.DCSID); err != nil {
		s.log.WarnContext(ctx, "settlement initiation denied: out of scope",
			slog.String("dcs_id", in.DCSID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}

	candidates, err := s.repo.issuedInvoices(ctx, in.DCSID, date)
	if err != nil {
		s.log.ErrorContext(ctx, "settlement initiation: load issued invoices",
			slog.String("dcs_id", in.DCSID.Hex()), slog.String("date", date), slog.Any("err", err))
		return nil, err
	}
	if len(candidates) == 0 {
		s.log.WarnContext(ctx, "settlement initiation rejected: no ISSUED invoices",
			slog.String("dcs_id", in.DCSID.Hex()), slog.String("date", date),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("NO_INVOICES", "no ISSUED invoices for DCS "+in.DCSID.Hex()+" on "+date)
	}

	// Claim optimistically: the status guard on the update means each invoice
	// lands in exactly one batch even under concurrent initiations. The batch
	// ID is pre-generated so claim, insert and ledger all share one identity.
	batchID := primitive.NewObjectID()
	ids := make([]primitive.ObjectID, 0, len(candidates))
	for _, inv := range candidates {
		ids = append(ids, inv.ID)
	}
	if _, err := s.repo.claimInvoices(ctx, ids, batchID); err != nil {
		s.log.ErrorContext(ctx, "settlement initiation: claim invoices",
			slog.String("batch_id", batchID.Hex()), slog.Any("err", err))
		return nil, err
	}
	claimed, err := s.repo.invoicesByBatch(ctx, batchID)
	if err != nil {
		s.log.ErrorContext(ctx, "settlement initiation: read claimed invoices",
			slog.String("batch_id", batchID.Hex()), slog.Any("err", err))
		return nil, err
	}
	if len(claimed) == 0 {
		s.log.WarnContext(ctx, "settlement initiation rejected: invoices claimed by concurrent batch",
			slog.String("dcs_id", in.DCSID.Hex()), slog.String("date", date),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("NO_INVOICES", "invoices were claimed by a concurrent settlement for DCS "+in.DCSID.Hex()+" on "+date)
	}

	batch := &domain.SettlementBatch{
		ID:          batchID,
		DCSID:       in.DCSID,
		Date:        date,
		Status:      domain.SettlementStatusPendingApproval, // INITIATED → PENDING_APPROVAL immediately
		InitiatedBy: actorID,
		CreatedAt:   now,
	}
	for _, inv := range claimed {
		batch.InvoiceIDs = append(batch.InvoiceIDs, inv.ID)
		batch.TotalAmount += inv.TotalAmount
	}
	if err := s.repo.insertBatch(ctx, batch); err != nil {
		s.log.ErrorContext(ctx, "settlement initiation: insert batch",
			slog.String("batch_id", batchID.Hex()), slog.Any("err", err))
		// Best-effort release so the claimed invoices are not stranded.
		if relErr := s.repo.releaseInvoices(ctx, batchID); relErr != nil {
			s.log.ErrorContext(ctx, "settlement: release after failed insert",
				slog.String("batch_id", batchID.Hex()), slog.Any("err", relErr))
		}
		return nil, err
	}

	refs := make([]provenance.Ref, 0, min(len(claimed), initiateRefsCap))
	for i, inv := range claimed {
		if i >= initiateRefsCap {
			break
		}
		refs = append(refs, provenance.Ref{EntityType: domain.EntityInvoice, EntityID: inv.ID.Hex(), Relation: "settles"})
	}
	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventSettlementInitiated,
		EntityType: domain.EntitySettlement,
		EntityID:   batch.ID.Hex(),
		Refs:       refs,
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  batch.DCSID.Hex(),
		Payload: map[string]any{
			"dcs_id":        batch.DCSID.Hex(),
			"date":          batch.Date,
			"invoice_count": len(claimed),
			"total_amount":  batch.TotalAmount,
		},
	})
	if err != nil {
		s.log.ErrorContext(ctx, "settlement initiation: ledger append",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setBatchProvenanceSeq(ctx, batch.ID, ev.Seq); err != nil {
		s.log.ErrorContext(ctx, "settlement initiation: stamp provenance seq",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	batch.ProvenanceSeq = ev.Seq

	s.log.InfoContext(ctx, "settlement batch initiated",
		slog.String("batch_id", batch.ID.Hex()),
		slog.String("dcs_id", batch.DCSID.Hex()),
		slog.String("date", batch.Date),
		slog.Int("invoice_count", len(claimed)),
		slog.Float64("total_amount", batch.TotalAmount),
		slog.String("actor_party_id", actor.PartyID))
	return batch, nil
}

// Approve applies the human dual-control sign-off: PENDING_APPROVAL→APPROVED.
func (s *service) Approve(ctx context.Context, actor auth.Actor, batchID primitive.ObjectID) (*domain.SettlementBatch, error) {
	actorID, err := actorObjectID(actor)
	if err != nil {
		return nil, err
	}
	batch, err := s.repo.batchByID(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, batch.DCSID); err != nil {
		s.log.WarnContext(ctx, "settlement approval denied: out of scope",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("dcs_id", batch.DCSID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	if err := validateApproval(*batch, actorID); err != nil {
		s.log.WarnContext(ctx, "settlement approval rejected",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("status", batch.Status),
			slog.String("initiated_by", batch.InitiatedBy.Hex()),
			slog.String("actor_party_id", actor.PartyID),
			slog.String("reason", err.Error()))
		return nil, err
	}

	now := time.Now().UTC()
	ok, err := s.repo.transitionBatch(ctx, batch.ID, domain.SettlementStatusPendingApproval, bson.D{
		{Key: "status", Value: domain.SettlementStatusApproved},
		{Key: "approved_by", Value: actorID},
		{Key: "approved_at", Value: now},
	})
	if err != nil {
		s.log.ErrorContext(ctx, "settlement approval: transition batch",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	if !ok {
		s.log.WarnContext(ctx, "settlement approval rejected: batch left PENDING_APPROVAL concurrently",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_STATUS", "settlement batch already left "+domain.SettlementStatusPendingApproval)
	}
	batch.Status = domain.SettlementStatusApproved
	batch.ApprovedBy = &actorID
	batch.ApprovedAt = &now

	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventSettlementApproved,
		EntityType: domain.EntitySettlement,
		EntityID:   batch.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  batch.DCSID.Hex(),
		Payload: map[string]any{
			"approved_by":  actor.PartyID,
			"total_amount": batch.TotalAmount,
		},
	})
	if err != nil {
		s.log.ErrorContext(ctx, "settlement approval: ledger append",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setBatchProvenanceSeq(ctx, batch.ID, ev.Seq); err != nil {
		s.log.ErrorContext(ctx, "settlement approval: stamp provenance seq",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	batch.ProvenanceSeq = ev.Seq

	s.log.InfoContext(ctx, "settlement batch approved",
		slog.String("batch_id", batch.ID.Hex()),
		slog.String("dcs_id", batch.DCSID.Hex()),
		slog.Float64("total_amount", batch.TotalAmount),
		slog.String("actor_party_id", actor.PartyID))
	return batch, nil
}

// Reject declines a pending batch and returns its invoices to ISSUED so they
// can be re-settled.
func (s *service) Reject(ctx context.Context, actor auth.Actor, batchID primitive.ObjectID, reason string) (*domain.SettlementBatch, error) {
	actorID, err := actorObjectID(actor)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, httpx.BadRequest("MISSING_FIELD", "reason is required")
	}
	batch, err := s.repo.batchByID(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, batch.DCSID); err != nil {
		s.log.WarnContext(ctx, "settlement rejection denied: out of scope",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("dcs_id", batch.DCSID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	if batch.Status != domain.SettlementStatusPendingApproval {
		s.log.WarnContext(ctx, "settlement rejection refused: invalid status",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("status", batch.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_STATUS", "settlement batch is "+batch.Status+", expected "+domain.SettlementStatusPendingApproval)
	}

	ok, err := s.repo.transitionBatch(ctx, batch.ID, domain.SettlementStatusPendingApproval, bson.D{
		{Key: "status", Value: domain.SettlementStatusRejected},
		{Key: "rejected_by", Value: actorID},
		{Key: "reject_reason", Value: reason},
	})
	if err != nil {
		s.log.ErrorContext(ctx, "settlement rejection: transition batch",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	if !ok {
		s.log.WarnContext(ctx, "settlement rejection refused: batch left PENDING_APPROVAL concurrently",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_STATUS", "settlement batch already left "+domain.SettlementStatusPendingApproval)
	}
	if err := s.repo.releaseInvoices(ctx, batch.ID); err != nil {
		s.log.ErrorContext(ctx, "settlement rejection: release invoices",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	batch.Status = domain.SettlementStatusRejected
	batch.RejectedBy = &actorID
	batch.RejectReason = reason

	s.log.InfoContext(ctx, "settlement batch rejected",
		slog.String("batch_id", batch.ID.Hex()),
		slog.String("dcs_id", batch.DCSID.Hex()),
		slog.String("reason", reason),
		slog.String("actor_party_id", actor.PartyID))
	return batch, nil
}

// Execute hands the APPROVED batch to the payment-aggregator adapter and
// records the resulting payouts.
//
// MOCK LICENSED-PA ADAPTER — this is a stand-in for the real integration.
// In production (§18-B) execution goes through an RBI-licensed Payment
// Aggregator operating a nodal/escrow account: Saathi submits the payout
// file, the PA moves the money, and UTRs come back asynchronously via
// webhook. Saathi itself never touches funds. The mock credits every payout
// instantly with a synthetic UTR so the downstream flow (invoice→PAID,
// provenance, farmer SMS) is exercised end to end.
func (s *service) Execute(ctx context.Context, actor auth.Actor, batchID primitive.ObjectID) (*SettlementDetail, error) {
	batch, err := s.repo.batchByID(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, batch.DCSID); err != nil {
		s.log.WarnContext(ctx, "settlement execution denied: out of scope",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("dcs_id", batch.DCSID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	if err := validateExecution(*batch); err != nil {
		s.log.WarnContext(ctx, "settlement execution refused: invalid status",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("status", batch.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}

	// Guarded claim: first run (APPROVED), re-run after FAILED, or resume of
	// a stale EXECUTING lease. The per-invoice work below is idempotent
	// (unique payout-per-invoice index + payout reuse), so replays are safe.
	now := time.Now().UTC()
	claimed, err := s.repo.claimExecution(ctx, batch.ID, now, now.Add(-executionLease))
	if err != nil {
		s.log.ErrorContext(ctx, "settlement execution: claim",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	if !claimed {
		s.log.WarnContext(ctx, "settlement execution refused: live execution lease held elsewhere",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_STATUS", "settlement batch is already being executed")
	}

	// From here on, any error must not strand the batch in EXECUTING forever:
	// flip it to FAILED (best-effort) so a retry can claim FAILED→EXECUTING.
	failExecution := func(cause error) error {
		s.log.ErrorContext(ctx, "settlement execution failed mid-run",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", cause))
		if ferr := s.repo.markExecutionFailed(ctx, batch.ID, cause.Error()); ferr != nil {
			s.log.ErrorContext(ctx, "settlement: mark execution failed",
				slog.String("batch_id", batch.ID.Hex()), slog.Any("err", ferr))
		}
		return cause
	}

	invoices, err := s.repo.invoicesByBatch(ctx, batch.ID)
	if err != nil {
		return nil, failExecution(err)
	}

	payouts := make([]domain.PayoutInstruction, 0, len(invoices))
	paidInvoiceIDs := make([]primitive.ObjectID, 0, len(invoices))
	failedCount := 0
	for _, inv := range invoices {
		masked, err := s.repo.latestBankMasked(ctx, inv.FarmerPartyID)
		if err != nil {
			return nil, failExecution(err)
		}
		payout := &domain.PayoutInstruction{
			ID:                primitive.NewObjectID(),
			SettlementBatchID: batch.ID,
			InvoiceID:         inv.ID,
			FarmerPartyID:     inv.FarmerPartyID,
			Amount:            inv.TotalAmount,
			BankAccountMasked: masked,
			CreatedAt:         now,
		}
		if masked == "" {
			// No verified bank account → money cannot move. Record a FAILED
			// payout (no UTR, no credited_at) and DO NOT mark the invoice PAID.
			// The farmer's dues stay outstanding until a bank is verified and
			// the batch is re-executed (Execute is re-entrant).
			payout.Status = domain.PayoutStatusFailed
			payout.FailureReason = "no verified bank account on file"
			failedCount++
			s.log.WarnContext(ctx, "settlement payout failed: farmer has no verified bank account",
				slog.String("batch_id", batch.ID.Hex()),
				slog.String("invoice_id", inv.ID.Hex()),
				slog.String("farmer_party_id", inv.FarmerPartyID.Hex()))
		} else {
			creditedAt := now
			payout.UTR = mockUTR()
			payout.Status = domain.PayoutStatusSuccess
			payout.CreditedAt = &creditedAt
		}
		exists, err := s.repo.insertPayout(ctx, payout)
		if err != nil {
			return nil, failExecution(err)
		}
		if exists {
			// Retry of an interrupted run — reuse the payout already recorded.
			s.log.WarnContext(ctx, "settlement execution: payout already recorded, reusing (idempotent replay)",
				slog.String("batch_id", batch.ID.Hex()),
				slog.String("invoice_id", inv.ID.Hex()))
			existing, err := s.repo.payoutByInvoice(ctx, inv.ID)
			if err != nil {
				return nil, failExecution(err)
			}
			payout = existing
			if payout.Status == domain.PayoutStatusFailed {
				failedCount++
			}
		}
		if payout.Status == domain.PayoutStatusSuccess {
			paidInvoiceIDs = append(paidInvoiceIDs, inv.ID)
		}
		payouts = append(payouts, *payout)
	}

	// Mark ONLY the successfully-paid invoices PAID; failed ones stay ISSUED.
	if err := s.repo.markInvoicesPaidByIDs(ctx, paidInvoiceIDs); err != nil {
		return nil, failExecution(err)
	}

	// A batch with any failed payout ends PARTIAL, not EXECUTED — re-executable
	// once the missing bank details are verified.
	finalStatus := domain.SettlementStatusExecuted
	if failedCount > 0 {
		finalStatus = domain.SettlementStatusPartial
	}
	paRef := mockPARef()
	ok, err := s.repo.transitionBatch(ctx, batch.ID, domain.SettlementStatusExecuting, bson.D{
		{Key: "status", Value: finalStatus},
		{Key: "pa_ref", Value: paRef},
		{Key: "executed_at", Value: now},
	})
	if err != nil {
		s.log.ErrorContext(ctx, "settlement execution: transition to EXECUTED",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	if !ok {
		s.log.WarnContext(ctx, "settlement execution: batch left EXECUTING unexpectedly",
			slog.String("batch_id", batch.ID.Hex()))
		return nil, httpx.Conflict("INVALID_STATUS", "settlement batch left EXECUTING unexpectedly")
	}
	batch.Status = finalStatus
	batch.PARef = paRef
	batch.ExecutedAt = &now

	ev, err := s.ledger.Append(ctx, provenance.AppendInput{
		Type:       domain.EventSettlementExecuted,
		EntityType: domain.EntitySettlement,
		EntityID:   batch.ID.Hex(),
		Actor:      provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
		OrgUnitID:  batch.DCSID.Hex(),
		Payload: map[string]any{
			"pa_ref":       paRef,
			"payout_count": len(payouts),
			"failed_count": failedCount,
			"total_amount": batch.TotalAmount,
			"final_status": finalStatus,
		},
	})
	if err != nil {
		s.log.ErrorContext(ctx, "settlement execution: ledger append",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, httpx.Internal(err)
	}
	if err := s.repo.setBatchProvenanceSeq(ctx, batch.ID, ev.Seq); err != nil {
		s.log.ErrorContext(ctx, "settlement execution: stamp provenance seq",
			slog.String("batch_id", batch.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	batch.ProvenanceSeq = ev.Seq

	s.log.InfoContext(ctx, "settlement batch executed",
		slog.String("batch_id", batch.ID.Hex()),
		slog.String("dcs_id", batch.DCSID.Hex()),
		slog.String("pa_ref", paRef),
		slog.Int("payout_count", len(payouts)),
		slog.Float64("total_amount", batch.TotalAmount),
		slog.String("actor_party_id", actor.PartyID))

	// One payout.credited ledger event per invoice, then the SMS fan-out.
	// Per-payout ledger/lookup failures are logged, not fatal: the money
	// outcome is already anchored by settlement.executed above.
	for _, p := range payouts {
		// Only SUCCESSful payouts produce a credited event + SMS — a FAILED
		// (no-bank) payout must never tell the farmer money was credited.
		if p.Status != domain.PayoutStatusSuccess {
			continue
		}
		if _, err := s.ledger.Append(ctx, provenance.AppendInput{
			Type:       domain.EventPayoutCredited,
			EntityType: domain.EntityInvoice,
			EntityID:   p.InvoiceID.Hex(),
			Refs: []provenance.Ref{
				{EntityType: domain.EntitySettlement, EntityID: batch.ID.Hex(), Relation: "paid_by"},
			},
			Actor:     provenance.ActorRef{PartyID: actor.PartyID, RoleCode: actor.RoleCode},
			OrgUnitID: batch.DCSID.Hex(),
			Payload:   map[string]any{"utr": p.UTR, "amount": p.Amount},
		}); err != nil {
			s.log.ErrorContext(ctx, "settlement: payout.credited ledger append",
				slog.String("invoice_id", p.InvoiceID.Hex()), slog.Any("err", err))
		}

		phone, err := s.repo.partyPhone(ctx, p.FarmerPartyID)
		if err != nil {
			s.log.ErrorContext(ctx, "settlement: farmer phone lookup for payout SMS",
				slog.String("farmer_party_id", p.FarmerPartyID.Hex()), slog.Any("err", err))
			continue
		}
		s.bus.Publish(eventbus.TopicPayoutCredited, PayoutCreditedEvent{
			FarmerPartyID: p.FarmerPartyID.Hex(),
			Phone:         phone,
			Amount:        p.Amount,
			UTR:           p.UTR,
		})
	}

	return &SettlementDetail{Batch: *batch, Payouts: payouts}, nil
}

// List pages settlement batches. Callers with a DCS-scoped token default to
// their own DCS; wider roles must name a dcs_id (SUPER_ADMIN and
// STATE_AUDITOR may list unscoped).
func (s *service) List(ctx context.Context, actor auth.Actor, dcsID, date, status string, page httpx.Page) ([]domain.SettlementBatch, int64, error) {
	if dcsID == "" {
		switch {
		case actor.OrgType == domain.OrgTypeDCS:
			dcsID = actor.OrgUnitID
		case actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor:
			// federation-wide read roles may list across DCSes
		default:
			return nil, 0, httpx.BadRequest("MISSING_FIELD", "dcs_id is required for your scope")
		}
	}
	filter := bson.D{}
	if dcsID != "" {
		dcsOID, err := httpx.ParseID(dcsID, "dcs_id")
		if err != nil {
			return nil, 0, err
		}
		if err := s.orgs.RequireInScope(ctx, actor, dcsOID); err != nil {
			s.log.WarnContext(ctx, "settlement list denied: out of scope",
				slog.String("dcs_id", dcsOID.Hex()),
				slog.String("actor_party_id", actor.PartyID))
			return nil, 0, err
		}
		filter = append(filter, bson.E{Key: "dcs_id", Value: dcsOID})
	}
	if date != "" {
		filter = append(filter, bson.E{Key: "date", Value: date})
	}
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: status})
	}
	return s.repo.listBatches(ctx, filter, page)
}

// Detail returns one batch plus its payout instructions.
func (s *service) Detail(ctx context.Context, actor auth.Actor, batchID primitive.ObjectID) (*SettlementDetail, error) {
	batch, err := s.repo.batchByID(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.RequireInScope(ctx, actor, batch.DCSID); err != nil {
		s.log.WarnContext(ctx, "settlement detail denied: out of scope",
			slog.String("batch_id", batch.ID.Hex()),
			slog.String("dcs_id", batch.DCSID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, err
	}
	payouts, err := s.repo.payoutsByBatch(ctx, batch.ID)
	if err != nil {
		return nil, err
	}
	return &SettlementDetail{Batch: *batch, Payouts: payouts}, nil
}

// PayoutHistory pages payout instructions for one farmer, newest first.
// A FARMER token is always forced to its own party — "my payment history".
// Staff roles are bound to their org scope: the target farmer must hold an
// ACTIVE FARMER assignment at a DCS inside the caller's scope, so a village
// Sacheev cannot enumerate the financials of farmers nationwide.
func (s *service) PayoutHistory(ctx context.Context, actor auth.Actor, farmerPartyID string, page httpx.Page) ([]domain.PayoutInstruction, int64, error) {
	if actor.RoleCode == domain.RoleFarmer {
		farmerPartyID = actor.PartyID
	}
	if farmerPartyID == "" {
		return nil, 0, httpx.BadRequest("MISSING_FIELD", "farmer_party_id is required")
	}
	farmerOID, err := httpx.ParseID(farmerPartyID, "farmer_party_id")
	if err != nil {
		return nil, 0, err
	}
	if actor.RoleCode != domain.RoleFarmer &&
		actor.RoleCode != domain.RoleSuperAdmin && actor.RoleCode != domain.RoleStateAuditor {
		dcsIDs, err := s.repo.farmerDCSIDs(ctx, farmerOID)
		if err != nil {
			return nil, 0, err
		}
		inScope := false
		for _, dcsID := range dcsIDs {
			if err := s.orgs.RequireInScope(ctx, actor, dcsID); err == nil {
				inScope = true
				break
			}
		}
		if !inScope {
			s.log.WarnContext(ctx, "payout history denied: farmer out of scope",
				slog.String("farmer_party_id", farmerOID.Hex()),
				slog.String("actor_party_id", actor.PartyID))
			return nil, 0, httpx.Forbidden("farmer is outside your organisational scope")
		}
	}
	return s.repo.payoutsByFarmer(ctx, farmerOID, page)
}

// ---- DBT subsidy rail (§13) — strictly separate from milk payments ----

// CreateDBT submits a scheme subsidy request to the (mocked) PFMS rail.
// Saathi only tracks the request; the subsidy is disbursed by PFMS/DBT,
// never by Saathi and never through the milk-payment rail.
func (s *service) CreateDBT(ctx context.Context, actor auth.Actor, in CreateDBTRequestInput) (*domain.DBTRequest, error) {
	actorID, err := actorObjectID(actor)
	if err != nil {
		return nil, err
	}
	if in.SchemeCode == "" || in.FarmerPartyID.IsZero() {
		return nil, httpx.BadRequest("MISSING_FIELD", "scheme_code and farmer_party_id are required")
	}
	if in.Amount <= 0 {
		return nil, httpx.BadRequest("INVALID_AMOUNT", "amount must be positive")
	}
	if _, err := s.repo.partyPhone(ctx, in.FarmerPartyID); err != nil {
		return nil, err // farmer must exist
	}

	// MOCK PFMS — real integration files the beneficiary+amount with PFMS
	// and receives an acknowledgement reference (§13).
	req := &domain.DBTRequest{
		ID:            primitive.NewObjectID(),
		SchemeCode:    in.SchemeCode,
		FarmerPartyID: in.FarmerPartyID,
		Amount:        in.Amount,
		PFMSRef:       mockPFMSRef(),
		Status:        domain.DBTStatusSubmitted,
		SubmittedBy:   actorID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.repo.insertDBT(ctx, req); err != nil {
		s.log.ErrorContext(ctx, "dbt request insert",
			slog.String("farmer_party_id", in.FarmerPartyID.Hex()),
			slog.String("scheme_code", in.SchemeCode), slog.Any("err", err))
		return nil, err
	}

	s.log.InfoContext(ctx, "dbt request submitted",
		slog.String("dbt_request_id", req.ID.Hex()),
		slog.String("scheme_code", req.SchemeCode),
		slog.String("farmer_party_id", req.FarmerPartyID.Hex()),
		slog.Float64("amount", req.Amount),
		slog.String("pfms_ref", req.PFMSRef),
		slog.String("actor_party_id", actor.PartyID))
	return req, nil
}

// UpdateDBTStatus records the DBT rail's progress for a request. CREDITED
// stamps credited_at; CREDITED/REJECTED are terminal.
func (s *service) UpdateDBTStatus(ctx context.Context, actor auth.Actor, id primitive.ObjectID, status string) (*domain.DBTRequest, error) {
	switch status {
	case domain.DBTStatusAccepted, domain.DBTStatusCredited, domain.DBTStatusRejected:
	default:
		return nil, httpx.BadRequest("INVALID_STATUS", "status must be one of ACCEPTED, CREDITED, REJECTED")
	}
	req, err := s.repo.dbtByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.Status == domain.DBTStatusCredited || req.Status == domain.DBTStatusRejected {
		s.log.WarnContext(ctx, "dbt status update refused: terminal status",
			slog.String("dbt_request_id", req.ID.Hex()),
			slog.String("status", req.Status),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("TERMINAL_STATUS", "dbt request is already "+req.Status)
	}

	var creditedAt *time.Time
	if status == domain.DBTStatusCredited {
		now := time.Now().UTC()
		creditedAt = &now
	}
	ok, err := s.repo.transitionDBT(ctx, req.ID, req.Status, status, creditedAt)
	if err != nil {
		s.log.ErrorContext(ctx, "dbt status transition",
			slog.String("dbt_request_id", req.ID.Hex()), slog.Any("err", err))
		return nil, err
	}
	if !ok {
		s.log.WarnContext(ctx, "dbt status update refused: concurrent change",
			slog.String("dbt_request_id", req.ID.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Conflict("INVALID_STATUS", "dbt request changed concurrently, retry")
	}
	fromStatus := req.Status
	req.Status = status
	req.CreditedAt = creditedAt

	s.log.InfoContext(ctx, "dbt request status updated",
		slog.String("dbt_request_id", req.ID.Hex()),
		slog.String("from_status", fromStatus),
		slog.String("to_status", status),
		slog.String("actor_party_id", actor.PartyID))
	return req, nil
}

// ListDBT pages DBT requests. A FARMER token is always forced to its own
// party.
func (s *service) ListDBT(ctx context.Context, actor auth.Actor, farmerPartyID, status string, page httpx.Page) ([]domain.DBTRequest, int64, error) {
	if actor.RoleCode == domain.RoleFarmer {
		farmerPartyID = actor.PartyID
	}
	filter := bson.D{}
	if farmerPartyID != "" {
		farmerOID, err := httpx.ParseID(farmerPartyID, "farmer_party_id")
		if err != nil {
			return nil, 0, err
		}
		filter = append(filter, bson.E{Key: "farmer_party_id", Value: farmerOID})
	}
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: status})
	}
	return s.repo.listDBT(ctx, filter, page)
}

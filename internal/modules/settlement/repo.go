package settlement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// internalFetchCap bounds internal (non-paginated) reads so a pathological
// day can never pull an unbounded set into memory. A DCS day is a few
// hundred invoices at most.
const internalFetchCap = 5000

// repo owns every MongoDB access for the settlement module. No business
// logic lives here — only reads, guarded writes and error mapping.
type repo struct {
	batches     *mongo.Collection
	payouts     *mongo.Collection
	invoices    *mongo.Collection
	kyc         *mongo.Collection
	parties     *mongo.Collection
	dbt         *mongo.Collection
	assignments *mongo.Collection // read-only: resolve a farmer's DCS for payout-history scoping
}

// newRepo binds the repo to the shared database handle.
func newRepo(db *mongo.Database) *repo {
	return &repo{
		batches:     db.Collection(mongodb.CollSettlements),
		payouts:     db.Collection(mongodb.CollPayouts),
		invoices:    db.Collection(mongodb.CollInvoices),
		kyc:         db.Collection(mongodb.CollKYCRecords),
		parties:     db.Collection(mongodb.CollParties),
		dbt:         db.Collection(mongodb.CollDBTRequests),
		assignments: db.Collection(mongodb.CollRoleAssignments),
	}
}

// ---- invoices (read + settlement-lifecycle status flips only) ----

// issuedInvoices returns the day's ISSUED invoices for a DCS.
func (rp *repo) issuedInvoices(ctx context.Context, dcsID primitive.ObjectID, date string) ([]domain.Invoice, error) {
	cur, err := rp.invoices.Find(ctx,
		bson.D{
			{Key: "dcs_id", Value: dcsID},
			{Key: "invoice_date", Value: date},
			{Key: "status", Value: domain.InvoiceStatusIssued},
		},
		options.Find().SetSort(bson.D{{Key: "invoice_number", Value: 1}}).SetLimit(internalFetchCap),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find issued invoices: %w", err))
	}
	var out []domain.Invoice
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode issued invoices: %w", err))
	}
	return out, nil
}

// claimInvoices atomically flips still-ISSUED invoices to SETTLEMENT_PENDING
// under the given batch. The status guard makes concurrent initiations safe
// without transactions: each invoice is claimed by exactly one batch.
func (rp *repo) claimInvoices(ctx context.Context, invoiceIDs []primitive.ObjectID, batchID primitive.ObjectID) (int64, error) {
	res, err := rp.invoices.UpdateMany(ctx,
		bson.D{
			{Key: "_id", Value: bson.D{{Key: "$in", Value: invoiceIDs}}},
			{Key: "status", Value: domain.InvoiceStatusIssued},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.InvoiceStatusSettlementPending},
			{Key: "settlement_batch_id", Value: batchID},
		}}},
	)
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("claim invoices for settlement: %w", err))
	}
	return res.ModifiedCount, nil
}

// invoicesByBatch returns every invoice claimed by a settlement batch.
func (rp *repo) invoicesByBatch(ctx context.Context, batchID primitive.ObjectID) ([]domain.Invoice, error) {
	cur, err := rp.invoices.Find(ctx,
		bson.D{{Key: "settlement_batch_id", Value: batchID}},
		options.Find().SetSort(bson.D{{Key: "invoice_number", Value: 1}}).SetLimit(internalFetchCap),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find invoices by batch: %w", err))
	}
	var out []domain.Invoice
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode invoices by batch: %w", err))
	}
	return out, nil
}

// releaseInvoices puts a rejected batch's invoices back to ISSUED and clears
// the batch linkage, so a fresh initiation can pick them up.
func (rp *repo) releaseInvoices(ctx context.Context, batchID primitive.ObjectID) error {
	_, err := rp.invoices.UpdateMany(ctx,
		bson.D{
			{Key: "settlement_batch_id", Value: batchID},
			{Key: "status", Value: domain.InvoiceStatusSettlementPending},
		},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: domain.InvoiceStatusIssued}}},
			{Key: "$unset", Value: bson.D{{Key: "settlement_batch_id", Value: ""}}},
		},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("release settlement invoices: %w", err))
	}
	return nil
}

// markInvoicesPaid flips all of a batch's invoices to PAID after execution.
func (rp *repo) markInvoicesPaid(ctx context.Context, batchID primitive.ObjectID) error {
	_, err := rp.invoices.UpdateMany(ctx,
		bson.D{{Key: "settlement_batch_id", Value: batchID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: domain.InvoiceStatusPaid}}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("mark invoices paid: %w", err))
	}
	return nil
}

// ---- settlement batches ----

// insertBatch persists a new settlement batch.
func (rp *repo) insertBatch(ctx context.Context, b *domain.SettlementBatch) error {
	if _, err := rp.batches.InsertOne(ctx, b); err != nil {
		return httpx.Internal(fmt.Errorf("insert settlement batch: %w", err))
	}
	return nil
}

// batchByID loads one settlement batch.
func (rp *repo) batchByID(ctx context.Context, id primitive.ObjectID) (*domain.SettlementBatch, error) {
	var b domain.SettlementBatch
	err := rp.batches.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&b)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("settlement batch " + id.Hex())
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find settlement batch: %w", err))
	}
	return &b, nil
}

// transitionBatch performs a status-guarded update (optimistic concurrency —
// no transactions on a standalone server). Returns false when the batch was
// not in fromStatus, i.e. another actor won the transition.
func (rp *repo) transitionBatch(ctx context.Context, id primitive.ObjectID, fromStatus string, set bson.D) (bool, error) {
	res, err := rp.batches.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: fromStatus}},
		bson.D{{Key: "$set", Value: set}},
	)
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("transition settlement batch: %w", err))
	}
	return res.MatchedCount > 0, nil
}

// claimExecution atomically claims a batch for execution. Three legal claims:
//   - APPROVED  → EXECUTING (first run)
//   - FAILED    → EXECUTING (re-execution after a recorded failure)
//   - EXECUTING → EXECUTING with a STALE lease (resume of a run whose process
//     died before it could mark FAILED; staleBefore guards against stealing a
//     live executor's batch)
//
// Every claim stamps executing_at as the new lease. Returns false when no
// claim matched — someone else holds a fresh lease or the batch is terminal.
func (rp *repo) claimExecution(ctx context.Context, id primitive.ObjectID, now, staleBefore time.Time) (bool, error) {
	for _, from := range []string{domain.SettlementStatusApproved, domain.SettlementStatusFailed} {
		ok, err := rp.transitionBatch(ctx, id, from, bson.D{
			{Key: "status", Value: domain.SettlementStatusExecuting},
			{Key: "executing_at", Value: now},
		})
		if err != nil || ok {
			return ok, err
		}
	}
	res, err := rp.batches.UpdateOne(ctx,
		bson.D{
			{Key: "_id", Value: id},
			{Key: "status", Value: domain.SettlementStatusExecuting},
			{Key: "executing_at", Value: bson.D{{Key: "$lt", Value: staleBefore}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "executing_at", Value: now}}}},
	)
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("claim settlement execution: %w", err))
	}
	return res.MatchedCount > 0, nil
}

// markExecutionFailed best-effort flips EXECUTING→FAILED with the reason so a
// mid-execution error leaves a re-executable batch, never a stuck one.
func (rp *repo) markExecutionFailed(ctx context.Context, id primitive.ObjectID, reason string) error {
	_, err := rp.batches.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.SettlementStatusExecuting}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.SettlementStatusFailed},
			{Key: "fail_reason", Value: reason},
		}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("mark settlement execution failed: %w", err))
	}
	return nil
}

// setBatchProvenanceSeq stamps the batch with its latest ledger sequence.
func (rp *repo) setBatchProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	_, err := rp.batches.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("set settlement provenance seq: %w", err))
	}
	return nil
}

// listBatches pages settlement batches, newest day first.
func (rp *repo) listBatches(ctx context.Context, filter bson.D, page httpx.Page) ([]domain.SettlementBatch, int64, error) {
	total, err := rp.batches.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count settlement batches: %w", err))
	}
	cur, err := rp.batches.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "date", Value: -1}, {Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("find settlement batches: %w", err))
	}
	var out []domain.SettlementBatch
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode settlement batches: %w", err))
	}
	return out, total, nil
}

// ---- payout instructions ----

// insertPayout persists one payout. The unique index on invoice_id makes an
// execution retry harmless: alreadyExists=true means this invoice was paid
// in a previous (possibly interrupted) run.
func (rp *repo) insertPayout(ctx context.Context, p *domain.PayoutInstruction) (alreadyExists bool, err error) {
	if _, err := rp.payouts.InsertOne(ctx, p); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return true, nil
		}
		return false, httpx.Internal(fmt.Errorf("insert payout: %w", err))
	}
	return false, nil
}

// payoutByInvoice fetches the payout already recorded for an invoice.
func (rp *repo) payoutByInvoice(ctx context.Context, invoiceID primitive.ObjectID) (*domain.PayoutInstruction, error) {
	var p domain.PayoutInstruction
	err := rp.payouts.FindOne(ctx, bson.D{{Key: "invoice_id", Value: invoiceID}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("payout for invoice " + invoiceID.Hex())
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find payout by invoice: %w", err))
	}
	return &p, nil
}

// payoutsByBatch returns a batch's payout instructions.
func (rp *repo) payoutsByBatch(ctx context.Context, batchID primitive.ObjectID) ([]domain.PayoutInstruction, error) {
	cur, err := rp.payouts.Find(ctx,
		bson.D{{Key: "settlement_batch_id", Value: batchID}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}).SetLimit(internalFetchCap),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find payouts by batch: %w", err))
	}
	var out []domain.PayoutInstruction
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode payouts by batch: %w", err))
	}
	return out, nil
}

// payoutsByFarmer pages one farmer's payment history, newest first.
func (rp *repo) payoutsByFarmer(ctx context.Context, farmerPartyID primitive.ObjectID, page httpx.Page) ([]domain.PayoutInstruction, int64, error) {
	filter := bson.D{{Key: "farmer_party_id", Value: farmerPartyID}}
	total, err := rp.payouts.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count payouts by farmer: %w", err))
	}
	cur, err := rp.payouts.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("find payouts by farmer: %w", err))
	}
	var out []domain.PayoutInstruction
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode payouts by farmer: %w", err))
	}
	return out, total, nil
}

// ---- lookups in adjacent collections (read-only, per module spec) ----

// latestBankMasked returns the masked bank account from the farmer's most
// recent VERIFIED KYC record carrying one, or "" when none exists. Payouts
// must only ever be issued against a reviewer-approved bank account, so
// PENDING/REJECTED (self-submitted, unreviewed) evidence is ignored.
func (rp *repo) latestBankMasked(ctx context.Context, partyID primitive.ObjectID) (string, error) {
	var rec domain.KYCRecord
	err := rp.kyc.FindOne(ctx,
		bson.D{
			{Key: "party_id", Value: partyID},
			{Key: "status", Value: domain.KYCStatusVerified},
			{Key: "bank_account_masked", Value: bson.D{{Key: "$exists", Value: true}, {Key: "$ne", Value: ""}}},
		},
		options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}}),
	).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", httpx.Internal(fmt.Errorf("find latest bank masked: %w", err))
	}
	return rec.BankAccountMasked, nil
}

// farmerDCSIDs returns the org units where the party holds an ACTIVE FARMER
// assignment — the scope anchors for staff reads of the farmer's financials.
func (rp *repo) farmerDCSIDs(ctx context.Context, farmerPartyID primitive.ObjectID) ([]primitive.ObjectID, error) {
	cur, err := rp.assignments.Find(ctx,
		bson.D{
			{Key: "party_id", Value: farmerPartyID},
			{Key: "role_code", Value: domain.RoleFarmer},
			{Key: "status", Value: domain.RoleAssignmentActive},
		},
		options.Find().SetProjection(bson.D{{Key: "org_unit_id", Value: 1}}).SetLimit(50),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find farmer DCS assignments: %w", err))
	}
	var docs []struct {
		OrgUnitID primitive.ObjectID `bson:"org_unit_id"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode farmer DCS assignments: %w", err))
	}
	ids := make([]primitive.ObjectID, 0, len(docs))
	for _, d := range docs {
		if !d.OrgUnitID.IsZero() {
			ids = append(ids, d.OrgUnitID)
		}
	}
	return ids, nil
}

// partyPhone resolves a party's phone number (for the payout SMS event).
func (rp *repo) partyPhone(ctx context.Context, partyID primitive.ObjectID) (string, error) {
	var p domain.Party
	err := rp.parties.FindOne(ctx, bson.D{{Key: "_id", Value: partyID}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", httpx.NotFound("party " + partyID.Hex())
	}
	if err != nil {
		return "", httpx.Internal(fmt.Errorf("find party phone: %w", err))
	}
	return p.Phone, nil
}

// ---- DBT requests ----

// insertDBT persists a new subsidy request.
func (rp *repo) insertDBT(ctx context.Context, r *domain.DBTRequest) error {
	if _, err := rp.dbt.InsertOne(ctx, r); err != nil {
		return httpx.Internal(fmt.Errorf("insert dbt request: %w", err))
	}
	return nil
}

// dbtByID loads one DBT request.
func (rp *repo) dbtByID(ctx context.Context, id primitive.ObjectID) (*domain.DBTRequest, error) {
	var d domain.DBTRequest
	err := rp.dbt.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("dbt request " + id.Hex())
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find dbt request: %w", err))
	}
	return &d, nil
}

// transitionDBT performs a status-guarded DBT update; false = lost the race
// or the request already left fromStatus.
func (rp *repo) transitionDBT(ctx context.Context, id primitive.ObjectID, fromStatus, toStatus string, creditedAt *time.Time) (bool, error) {
	set := bson.D{{Key: "status", Value: toStatus}}
	if creditedAt != nil {
		set = append(set, bson.E{Key: "credited_at", Value: creditedAt})
	}
	res, err := rp.dbt.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: fromStatus}},
		bson.D{{Key: "$set", Value: set}},
	)
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("transition dbt request: %w", err))
	}
	return res.MatchedCount > 0, nil
}

// listDBT pages DBT requests, newest first.
func (rp *repo) listDBT(ctx context.Context, filter bson.D, page httpx.Page) ([]domain.DBTRequest, int64, error) {
	total, err := rp.dbt.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count dbt requests: %w", err))
	}
	cur, err := rp.dbt.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("find dbt requests: %w", err))
	}
	var out []domain.DBTRequest
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode dbt requests: %w", err))
	}
	return out, total, nil
}

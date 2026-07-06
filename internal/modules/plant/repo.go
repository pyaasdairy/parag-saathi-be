package plant

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// Repo is the plant module's MongoDB access layer. All state transitions use
// optimistic filters on the expected status — no transactions (standalone
// server), so a lost race simply matches zero documents and the service
// reports a conflict instead of corrupting state.
type Repo struct {
	consignments *mongo.Collection
	bmcLots      *mongo.Collection
	batches      *mongo.Collection
	productLots  *mongo.Collection
	qrs          *mongo.Collection
	counters     *mongo.Collection
}

// NewRepo binds the repo to the shared database.
func NewRepo(db *mongo.Database) *Repo {
	return &Repo{
		consignments: db.Collection(mongodb.CollConsignments),
		bmcLots:      db.Collection(mongodb.CollBMCLots),
		batches:      db.Collection(mongodb.CollBatches),
		productLots:  db.Collection(mongodb.CollProductLots),
		qrs:          db.Collection(mongodb.CollBatchQRs),
		counters:     db.Collection(mongodb.CollCounters),
	}
}

// --- consignments (read + claim; owned by the logistics module otherwise) ---

// ConsignmentsByIDs loads the given consignments (missing IDs simply absent).
func (r *Repo) ConsignmentsByIDs(ctx context.Context, ids []primitive.ObjectID) ([]domain.DCSConsignment, error) {
	cur, err := r.consignments.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, err
	}
	out := []domain.DCSConsignment{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimConsignments atomically flips DELIVERED consignments to ACCEPTED and
// stamps the pooling lot ID. Returns how many were actually claimed — fewer
// than requested means another lot raced us on at least one consignment.
func (r *Repo) ClaimConsignments(ctx context.Context, ids []primitive.ObjectID, bmcLotID primitive.ObjectID) (int64, error) {
	res, err := r.consignments.UpdateMany(ctx,
		bson.D{
			{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
			{Key: "status", Value: domain.ConsignmentStatusDelivered},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.ConsignmentStatusAccepted},
			{Key: "bmc_lot_id", Value: bmcLotID},
		}}},
	)
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}

// ReleaseConsignments undoes a partial claim (compensating action for a lost
// race): everything stamped with this lot ID goes back to DELIVERED.
func (r *Repo) ReleaseConsignments(ctx context.Context, bmcLotID primitive.ObjectID) error {
	_, err := r.consignments.UpdateMany(ctx,
		bson.D{{Key: "bmc_lot_id", Value: bmcLotID}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: domain.ConsignmentStatusDelivered}}},
			{Key: "$unset", Value: bson.D{{Key: "bmc_lot_id", Value: ""}}},
		},
	)
	return err
}

// --- BMC lots ---

// InsertBMCLot stores a new lot.
func (r *Repo) InsertBMCLot(ctx context.Context, lot *domain.BMCLot) error {
	_, err := r.bmcLots.InsertOne(ctx, lot)
	return err
}

// BMCLotByID loads one lot (mongo.ErrNoDocuments when absent).
func (r *Repo) BMCLotByID(ctx context.Context, id primitive.ObjectID) (*domain.BMCLot, error) {
	var lot domain.BMCLot
	if err := r.bmcLots.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&lot); err != nil {
		return nil, err
	}
	return &lot, nil
}

// BMCLotsByIDs loads the given lots (missing IDs simply absent).
func (r *Repo) BMCLotsByIDs(ctx context.Context, ids []primitive.ObjectID) ([]domain.BMCLot, error) {
	cur, err := r.bmcLots.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, err
	}
	out := []domain.BMCLot{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CloseBMCLot transitions OPEN→QC_PENDING with the chilling temperature.
// Returns false when the lot was not OPEN (already closed, or raced).
func (r *Repo) CloseBMCLot(ctx context.Context, id primitive.ObjectID, tempC float64, closedAt time.Time) (bool, error) {
	res, err := r.bmcLots.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.BMCLotStatusOpen}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.BMCLotStatusQCPending},
			{Key: "chilling_temp_c", Value: tempC},
			{Key: "closed_at", Value: closedAt},
		}}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// DispatchBMCLot transitions PASSED→DISPATCHED. The status filter IS the
// safety gate at the datastore level: a BLOCKED or QC_PENDING lot can never
// match this update.
func (r *Repo) DispatchBMCLot(ctx context.Context, id primitive.ObjectID, dispatchedAt time.Time) (bool, error) {
	res, err := r.bmcLots.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.BMCLotStatusPassed}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.BMCLotStatusDispatched},
			{Key: "dispatched_at", Value: dispatchedAt},
		}}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// ClaimBMCLots atomically claims DISPATCHED lots for a processing batch:
// DISPATCHED→POOLED stamped with the batch ID. Returns how many were actually
// claimed — fewer than requested means another batch raced us on at least one
// lot (or a lot was not DISPATCHED), and the caller must roll back.
func (r *Repo) ClaimBMCLots(ctx context.Context, ids []primitive.ObjectID, batchID primitive.ObjectID) (int64, error) {
	res, err := r.bmcLots.UpdateMany(ctx,
		bson.D{
			{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
			{Key: "status", Value: domain.BMCLotStatusDispatched},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.BMCLotStatusPooled},
			{Key: "batch_id", Value: batchID},
		}}},
	)
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}

// ReleaseBMCLots undoes a partial claim (compensating action for a lost
// race): every lot stamped with this batch ID goes back to DISPATCHED.
func (r *Repo) ReleaseBMCLots(ctx context.Context, batchID primitive.ObjectID) error {
	_, err := r.bmcLots.UpdateMany(ctx,
		bson.D{
			{Key: "batch_id", Value: batchID},
			{Key: "status", Value: domain.BMCLotStatusPooled},
		},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: domain.BMCLotStatusDispatched}}},
			{Key: "$unset", Value: bson.D{{Key: "batch_id", Value: ""}}},
		},
	)
	return err
}

// BMCLotListFilter narrows the BMC lot listing. Zero-valued fields are
// ignored.
type BMCLotListFilter struct {
	BMCID  primitive.ObjectID
	Date   string
	Status string
}

// ListBMCLots returns a page of lots plus the total match count.
func (r *Repo) ListBMCLots(ctx context.Context, f BMCLotListFilter, page httpx.Page) ([]domain.BMCLot, int64, error) {
	filter := bson.D{}
	if !f.BMCID.IsZero() {
		filter = append(filter, bson.E{Key: "bmc_id", Value: f.BMCID})
	}
	if f.Date != "" {
		filter = append(filter, bson.E{Key: "date", Value: f.Date})
	}
	if f.Status != "" {
		filter = append(filter, bson.E{Key: "status", Value: f.Status})
	}
	total, err := r.bmcLots.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := r.bmcLots.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "date", Value: -1}, {Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	out := []domain.BMCLot{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// --- processing batches ---

// NextSequence atomically increments and returns the named counter — the
// transaction-free way to mint gapless human-readable numbers.
func (r *Repo) NextSequence(ctx context.Context, key string) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := r.counters.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: key}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(1)}}}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return 0, err
	}
	return doc.Seq, nil
}

// InsertBatch stores a new processing batch.
func (r *Repo) InsertBatch(ctx context.Context, b *domain.ProcessingBatch) error {
	_, err := r.batches.InsertOne(ctx, b)
	return err
}

// BatchByID loads one batch (mongo.ErrNoDocuments when absent).
func (r *Repo) BatchByID(ctx context.Context, id primitive.ObjectID) (*domain.ProcessingBatch, error) {
	var b domain.ProcessingBatch
	if err := r.batches.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

// CompleteBatch transitions PASSED→COMPLETED. The status filter enforces the
// gate: an unpassed batch can never match.
func (r *Repo) CompleteBatch(ctx context.Context, id primitive.ObjectID, completedAt time.Time) (bool, error) {
	res, err := r.batches.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.BatchStatusPassed}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.BatchStatusCompleted},
			{Key: "completed_at", Value: completedAt},
		}}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// BatchListFilter narrows the batch listing. Zero-valued fields are ignored.
type BatchListFilter struct {
	PlantID primitive.ObjectID
	Status  string
}

// ListBatches returns a page of batches plus the total match count.
func (r *Repo) ListBatches(ctx context.Context, f BatchListFilter, page httpx.Page) ([]domain.ProcessingBatch, int64, error) {
	filter := bson.D{}
	if !f.PlantID.IsZero() {
		filter = append(filter, bson.E{Key: "plant_id", Value: f.PlantID})
	}
	if f.Status != "" {
		filter = append(filter, bson.E{Key: "status", Value: f.Status})
	}
	total, err := r.batches.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := r.batches.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "started_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	out := []domain.ProcessingBatch{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// --- product lots ---

// InsertProductLot stores a new product lot.
func (r *Repo) InsertProductLot(ctx context.Context, p *domain.ProductLot) error {
	_, err := r.productLots.InsertOne(ctx, p)
	return err
}

// ProductLotByID loads one product lot (mongo.ErrNoDocuments when absent).
func (r *Repo) ProductLotByID(ctx context.Context, id primitive.ObjectID) (*domain.ProductLot, error) {
	var p domain.ProductLot
	if err := r.productLots.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RecallProductLot flips any not-yet-recalled lot to RECALLED with the reason.
// Returns false when the lot was already recalled (idempotency guard).
func (r *Repo) RecallProductLot(ctx context.Context, id primitive.ObjectID, reason string) (bool, error) {
	res, err := r.productLots.UpdateOne(ctx,
		bson.D{
			{Key: "_id", Value: id},
			{Key: "status", Value: bson.D{{Key: "$ne", Value: domain.ProductLotStatusRecalled}}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.ProductLotStatusRecalled},
			{Key: "recall_reason", Value: reason},
		}}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// --- batch QRs ---

// InsertQR stores a freshly minted QR. A duplicate-key error means the random
// short code collided — the service regenerates and retries.
func (r *Repo) InsertQR(ctx context.Context, qr *domain.BatchQR) error {
	_, err := r.qrs.InsertOne(ctx, qr)
	return err
}

// DeleteQR removes a just-minted QR whose lot turned out to be no longer
// ACTIVE at write time (compensating action — the QR was never returned to
// the caller, so nothing references it).
func (r *Repo) DeleteQR(ctx context.Context, id primitive.ObjectID) error {
	_, err := r.qrs.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
	return err
}

// TouchActiveLot performs a no-op conditional write on an ACTIVE product lot
// ($inc of a stamp counter). MatchedCount == 0 proves the lot is no longer
// ACTIVE (e.g. recalled mid-request) at datastore level.
func (r *Repo) TouchActiveLot(ctx context.Context, lotID primitive.ObjectID) (bool, error) {
	res, err := r.productLots.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: lotID}, {Key: "status", Value: domain.ProductLotStatusActive}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "qr_issue_stamp", Value: int64(1)}}}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// QRListFilter narrows the QR listing. A zero ProductLotID is ignored.
type QRListFilter struct {
	ProductLotID primitive.ObjectID
}

// ListQRs returns a page of QRs plus the total match count.
func (r *Repo) ListQRs(ctx context.Context, f QRListFilter, page httpx.Page) ([]domain.BatchQR, int64, error) {
	filter := bson.D{}
	if !f.ProductLotID.IsZero() {
		filter = append(filter, bson.E{Key: "product_lot_id", Value: f.ProductLotID})
	}
	total, err := r.qrs.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := r.qrs.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "issued_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	out := []domain.BatchQR{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// --- provenance back-references ---

// SetBMCLotProvenanceSeq stamps the ledger sequence onto a lot document.
func (r *Repo) SetBMCLotProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	return r.setProvenanceSeq(ctx, r.bmcLots, id, seq)
}

// SetBatchProvenanceSeq stamps the ledger sequence onto a batch document.
func (r *Repo) SetBatchProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	return r.setProvenanceSeq(ctx, r.batches, id, seq)
}

// SetProductLotProvenanceSeq stamps the ledger sequence onto a product lot.
func (r *Repo) SetProductLotProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	return r.setProvenanceSeq(ctx, r.productLots, id, seq)
}

// SetQRProvenanceSeq stamps the ledger sequence onto a QR document.
func (r *Repo) SetQRProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	return r.setProvenanceSeq(ctx, r.qrs, id, seq)
}

func (r *Repo) setProvenanceSeq(ctx context.Context, coll *mongo.Collection, id primitive.ObjectID, seq int64) error {
	_, err := coll.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}},
	)
	return err
}

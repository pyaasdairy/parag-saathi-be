package publictrace

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// repo is the read-mostly MongoDB access layer for the trace views. It reads
// other modules' collections strictly via the mongodb.Coll* consts, exactly
// where the spec sanctions it (QR → lot → batch → qc / consignment lookups).
//
// Not-found is returned as a typed httpx.NotFound; every unexpected DB error
// is wrapped with its operation so the service's ERROR log names the failing
// op.
type repo struct {
	batchQRs     *mongo.Collection
	productLots  *mongo.Collection
	batches      *mongo.Collection
	qcResults    *mongo.Collection
	consignments *mongo.Collection
}

// newRepo binds the repo to the shared database.
func newRepo(db *mongo.Database) *repo {
	return &repo{
		batchQRs:     db.Collection(mongodb.CollBatchQRs),
		productLots:  db.Collection(mongodb.CollProductLots),
		batches:      db.Collection(mongodb.CollBatches),
		qcResults:    db.Collection(mongodb.CollQCResults),
		consignments: db.Collection(mongodb.CollConsignments),
	}
}

// qrByCode loads a batch QR by its short public code — qr_code is the unique
// human-readable business key (never an ObjectID).
func (r *repo) qrByCode(ctx context.Context, code string) (*domain.BatchQR, error) {
	var qr domain.BatchQR
	err := r.batchQRs.FindOne(ctx, bson.D{{Key: "qr_code", Value: code}}).Decode(&qr)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("qr code")
	}
	if err != nil {
		return nil, fmt.Errorf("find qr by code: %w", err)
	}
	return &qr, nil
}

// incrementScanCount bumps the QR's scan counter. Called fire-and-forget by
// the service so it never delays a consumer scan.
func (r *repo) incrementScanCount(ctx context.Context, qrID primitive.ObjectID) error {
	_, err := r.batchQRs.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: qrID}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "scan_count", Value: int64(1)}}}},
	)
	if err != nil {
		return fmt.Errorf("increment qr scan_count: %w", err)
	}
	return nil
}

// productLotByID loads a product lot.
func (r *repo) productLotByID(ctx context.Context, id primitive.ObjectID) (*domain.ProductLot, error) {
	var lot domain.ProductLot
	err := r.productLots.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&lot)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("product lot")
	}
	if err != nil {
		return nil, fmt.Errorf("find product lot: %w", err)
	}
	return &lot, nil
}

// batchByID loads a processing batch.
func (r *repo) batchByID(ctx context.Context, id primitive.ObjectID) (*domain.ProcessingBatch, error) {
	var batch domain.ProcessingBatch
	err := r.batches.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&batch)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("processing batch")
	}
	if err != nil {
		return nil, fmt.Errorf("find processing batch: %w", err)
	}
	return &batch, nil
}

// latestPlantLabPass returns the most recent passing PLANT_LAB QC result for
// a batch, or nil when none exists yet (absence is not an error — the public
// view simply omits the certificate).
func (r *repo) latestPlantLabPass(ctx context.Context, batchID primitive.ObjectID) (*domain.QCResult, error) {
	var qc domain.QCResult
	err := r.qcResults.FindOne(ctx,
		bson.D{
			{Key: "subject_type", Value: domain.QCSubjectProcessingBatch},
			{Key: "subject_id", Value: batchID},
			{Key: "stage", Value: domain.QCStagePlantLab},
			{Key: "overall_pass", Value: true},
			// A result voided after losing the gate race carries no verdict.
			{Key: "superseded", Value: bson.D{{Key: "$ne", Value: true}}},
		},
		options.FindOne().SetSort(bson.D{{Key: "recorded_at", Value: -1}}),
	).Decode(&qc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find latest plant lab pass: %w", err)
	}
	return &qc, nil
}

// consignmentsByIDs loads consignment documents by ObjectID — the fallback
// source of contributing-DCS ids and collection dates when the ledger events
// did not carry them in org_unit_id/payload. Input is bounded by the trace
// event cap.
func (r *repo) consignmentsByIDs(ctx context.Context, ids []primitive.ObjectID) ([]domain.DCSConsignment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cur, err := r.consignments.Find(ctx,
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
	)
	if err != nil {
		return nil, fmt.Errorf("find consignments by ids: %w", err)
	}
	var out []domain.DCSConsignment
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("decode consignments by ids: %w", err)
	}
	return out, nil
}

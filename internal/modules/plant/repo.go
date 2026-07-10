package plant

import (
	"context"
	"errors"
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
	routeTrips   *mongo.Collection
	bmcLots      *mongo.Collection
	batches      *mongo.Collection
	productLots  *mongo.Collection
	qrs          *mongo.Collection
	counters     *mongo.Collection
	products     *mongo.Collection // read-only view into the product master (derive lot SKU metadata)
	certificates *mongo.Collection // read-only: resolve a batch's issued QC certificate id
}

// NewRepo binds the repo to the shared database.
func NewRepo(db *mongo.Database) *Repo {
	return &Repo{
		consignments: db.Collection(mongodb.CollConsignments),
		routeTrips:   db.Collection(mongodb.CollRouteTrips),
		bmcLots:      db.Collection(mongodb.CollBMCLots),
		batches:      db.Collection(mongodb.CollBatches),
		productLots:  db.Collection(mongodb.CollProductLots),
		qrs:          db.Collection(mongodb.CollBatchQRs),
		counters:     db.Collection(mongodb.CollCounters),
		products:     db.Collection(mongodb.CollProducts),
		certificates: db.Collection(mongodb.CollQCCertificates),
	}
}

// CertificateIDForBatch returns the hex id of the QC certificate issued for a
// batch, or "" when none exists — read-time enrichment so the lab console can
// drive its issued-state off a real lookup instead of a duplicate issue.
func (r *Repo) CertificateIDForBatch(ctx context.Context, batchID primitive.ObjectID) (string, error) {
	var doc struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	err := r.certificates.FindOne(ctx, bson.D{{Key: "batch_id", Value: batchID}},
		options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 1}})).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return doc.ID.Hex(), nil
}

// ProductLotsByBatch returns the product lots yielded by a batch, newest first.
func (r *Repo) ProductLotsByBatch(ctx context.Context, batchID primitive.ObjectID, page httpx.Page) ([]domain.ProductLot, int64, error) {
	filter := bson.D{{Key: "batch_id", Value: batchID}}
	total, err := r.productLots.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := r.productLots.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	out := []domain.ProductLot{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ProductLotIDsByBatch returns the ids of every product lot yielded by a batch
// — the set a batch-scoped QR listing filters on.
func (r *Repo) ProductLotIDsByBatch(ctx context.Context, batchID primitive.ObjectID) ([]primitive.ObjectID, error) {
	cur, err := r.productLots.Find(ctx, bson.D{{Key: "batch_id", Value: batchID}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]primitive.ObjectID, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}

// ProductByID loads one product-master row (mongo.ErrNoDocuments when absent) —
// used to derive a product lot's SKU/name/unit_size from a chosen product_id.
func (r *Repo) ProductByID(ctx context.Context, id primitive.ObjectID) (*domain.Product, error) {
	var p domain.Product
	if err := r.products.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// TripDeliveryBMCByIDs returns, for each given route-trip id, the BMC it was
// delivered to (empty if the trip is not yet DELIVERED). Used to verify that a
// consignment being pooled was actually delivered to THIS BMC. Read-only view
// into the logistics module's route_trips (sanctioned cross-collection read).
func (r *Repo) TripDeliveryBMCByIDs(ctx context.Context, ids []primitive.ObjectID) (map[primitive.ObjectID]primitive.ObjectID, error) {
	cur, err := r.routeTrips.Find(ctx,
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "status", Value: domain.TripStatusDelivered}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}, {Key: "delivered_to_bmc_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID    primitive.ObjectID  `bson:"_id"`
		BMCID *primitive.ObjectID `bson:"delivered_to_bmc_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[primitive.ObjectID]primitive.ObjectID, len(rows))
	for _, row := range rows {
		if row.BMCID != nil {
			out[row.ID] = *row.BMCID
		}
	}
	return out, nil
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

// DistinctDCSIDsForConsignments resolves the given consignments to the DISTINCT
// set of societies (dcs_id) that fed them — the §7.4 honest-pooling contributor
// set materialised onto a processing batch. Missing consignments simply drop
// out; the result carries no zero-value ids.
func (r *Repo) DistinctDCSIDsForConsignments(ctx context.Context, ids []primitive.ObjectID) ([]primitive.ObjectID, error) {
	if len(ids) == 0 {
		return []primitive.ObjectID{}, nil
	}
	raw, err := r.consignments.Distinct(ctx, "dcs_id", bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, err
	}
	out := make([]primitive.ObjectID, 0, len(raw))
	for _, v := range raw {
		if oid, ok := v.(primitive.ObjectID); ok && !oid.IsZero() {
			out = append(out, oid)
		}
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
// ignored. BMCIDs (when non-empty) constrains the listing to a set of BMCs —
// the org-scope fence for a plant reader (every BMC under the plant's union).
type BMCLotListFilter struct {
	BMCID  primitive.ObjectID
	BMCIDs []primitive.ObjectID
	Date   string
	Status string
}

// ListBMCLots returns a page of lots plus the total match count.
func (r *Repo) ListBMCLots(ctx context.Context, f BMCLotListFilter, page httpx.Page) ([]domain.BMCLot, int64, error) {
	filter := bson.D{}
	if !f.BMCID.IsZero() {
		filter = append(filter, bson.E{Key: "bmc_id", Value: f.BMCID})
	}
	if len(f.BMCIDs) > 0 {
		filter = append(filter, bson.E{Key: "bmc_id", Value: bson.D{{Key: "$in", Value: f.BMCIDs}}})
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

// QRListFilter narrows the QR listing. A zero ProductLotID is ignored;
// ProductLotIDs (set by a batch-scoped listing) narrows to a lot set via $in.
type QRListFilter struct {
	ProductLotID  primitive.ObjectID
	ProductLotIDs []primitive.ObjectID
}

// ListQRs returns a page of QRs plus the total match count.
func (r *Repo) ListQRs(ctx context.Context, f QRListFilter, page httpx.Page) ([]domain.BatchQR, int64, error) {
	filter := bson.D{}
	switch {
	case !f.ProductLotID.IsZero():
		filter = append(filter, bson.E{Key: "product_lot_id", Value: f.ProductLotID})
	case len(f.ProductLotIDs) > 0:
		filter = append(filter, bson.E{Key: "product_lot_id", Value: bson.D{{Key: "$in", Value: f.ProductLotIDs}}})
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

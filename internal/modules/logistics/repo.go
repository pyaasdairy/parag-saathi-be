package logistics

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// repo owns every MongoDB access for the logistics module. It returns raw
// driver errors (including mongo.ErrNoDocuments); the service layer maps
// them to httpx errors. No business logic lives here.
type repo struct {
	consignments *mongo.Collection
	trips        *mongo.Collection
	pours        *mongo.Collection // read-only view into the collection module's pours (sanctioned by spec)
}

// newRepo binds the repo to its collections via the shared name constants.
func newRepo(db *mongo.Database) *repo {
	return &repo{
		consignments: db.Collection(mongodb.CollConsignments),
		trips:        db.Collection(mongodb.CollRouteTrips),
		pours:        db.Collection(mongodb.CollMilkPours),
	}
}

// pourRow is the projected slice of a milk pour needed for aggregation.
type pourRow struct {
	ID             string  `bson:"_id"`
	QuantityLitres float64 `bson:"quantity_litres"`
	FatPct         float64 `bson:"fat_pct"`
	SNFPct         float64 `bson:"snf_pct"`
}

// findRecordedPours returns the RECORDED pours of one DCS date+shift,
// projected down to the fields the consignment aggregate needs.
func (r *repo) findRecordedPours(ctx context.Context, dcsID, date, shift string) ([]pourRow, error) {
	filter := bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "pour_date", Value: date},
		{Key: "shift", Value: shift},
		{Key: "status", Value: domain.PourStatusRecorded},
	}
	projection := bson.D{
		{Key: "_id", Value: 1},
		{Key: "quantity_litres", Value: 1},
		{Key: "fat_pct", Value: 1},
		{Key: "snf_pct", Value: 1},
	}
	cur, err := r.pours.Find(ctx, filter,
		options.Find().SetProjection(projection).SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var rows []pourRow
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// insertConsignment stores a new consignment. The unique index on
// dcs_id+date+shift turns duplicates into a duplicate-key error.
func (r *repo) insertConsignment(ctx context.Context, c *domain.DCSConsignment) error {
	_, err := r.consignments.InsertOne(ctx, c)
	return err
}

// findConsignmentByID loads one consignment.
func (r *repo) findConsignmentByID(ctx context.Context, id string) (*domain.DCSConsignment, error) {
	var c domain.DCSConsignment
	if err := r.consignments.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// markConsignmentDispatched transitions OPEN→DISPATCHED optimistically:
// the status filter makes the guard atomic without transactions. The seal
// also re-stamps the pour aggregates (extraSet) so pours recorded between
// creation and dispatch are never lost from the consignment's totals.
func (r *repo) markConsignmentDispatched(ctx context.Context, id string, at time.Time, extraSet bson.D) (bool, error) {
	set := bson.D{
		{Key: "status", Value: domain.ConsignmentStatusDispatch},
		{Key: "dispatched_at", Value: at},
	}
	set = append(set, extraSet...)
	res, err := r.consignments.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.ConsignmentStatusOpen}},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// markConsignmentPickedUp transitions DISPATCHED→PICKED_UP and pins the
// carrying trip, guarded atomically by the status filter.
func (r *repo) markConsignmentPickedUp(ctx context.Context, id, tripID string) (bool, error) {
	res, err := r.consignments.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.ConsignmentStatusDispatch}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.ConsignmentStatusPickedUp},
			{Key: "route_trip_id", Value: tripID},
		}}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// markConsignmentsDelivered transitions every listed PICKED_UP consignment
// to DELIVERED (bulk companion of the trip delivery).
func (r *repo) markConsignmentsDelivered(ctx context.Context, ids []string) error {
	_, err := r.consignments.UpdateMany(ctx,
		bson.D{
			{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
			{Key: "status", Value: domain.ConsignmentStatusPickedUp},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: domain.ConsignmentStatusDelivered}}}})
	return err
}

// setConsignmentProvenanceSeq pins the latest ledger sequence on the doc.
func (r *repo) setConsignmentProvenanceSeq(ctx context.Context, id string, seq int64) error {
	_, err := r.consignments.UpdateByID(ctx, id,
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}})
	return err
}

// listConsignments returns a filtered, newest-first page plus the total count.
func (r *repo) listConsignments(ctx context.Context, dcsID, date, status string, page httpx.Page) ([]domain.DCSConsignment, int64, error) {
	filter := bson.D{}
	if dcsID != "" {
		filter = append(filter, bson.E{Key: "dcs_id", Value: dcsID})
	}
	if date != "" {
		filter = append(filter, bson.E{Key: "date", Value: date})
	}
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: status})
	}
	total, err := r.consignments.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := r.consignments.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	items := []domain.DCSConsignment{}
	if err := cur.All(ctx, &items); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// insertTrip stores a new route trip.
func (r *repo) insertTrip(ctx context.Context, t *domain.RouteTrip) error {
	_, err := r.trips.InsertOne(ctx, t)
	return err
}

// findTripByID loads one trip.
func (r *repo) findTripByID(ctx context.Context, id string) (*domain.RouteTrip, error) {
	var t domain.RouteTrip
	if err := r.trips.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

// markStopPickedUp stamps the matched stop with pickup time/temperature and
// moves the trip to IN_PROGRESS (idempotent once it is already there).
func (r *repo) markStopPickedUp(ctx context.Context, tripID, consignmentID string, at time.Time, tempC float64, notes string) error {
	_, err := r.trips.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: tripID}, {Key: "stops.consignment_id", Value: consignmentID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "stops.$.picked_up_at", Value: at},
			{Key: "stops.$.temp_c", Value: tempC},
			{Key: "stops.$.notes", Value: notes},
			{Key: "status", Value: domain.TripStatusInProgress},
		}}})
	return err
}

// pushColdChain appends one temperature sample to the trip's cold-chain log.
func (r *repo) pushColdChain(ctx context.Context, tripID string, entry domain.ColdChainEntry) error {
	_, err := r.trips.UpdateByID(ctx, tripID,
		bson.D{{Key: "$push", Value: bson.D{{Key: "cold_chain", Value: entry}}}})
	return err
}

// markTripDelivered transitions IN_PROGRESS→DELIVERED optimistically and
// records the receiving BMC.
func (r *repo) markTripDelivered(ctx context.Context, tripID, bmcID string, at time.Time) (bool, error) {
	res, err := r.trips.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: tripID}, {Key: "status", Value: domain.TripStatusInProgress}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.TripStatusDelivered},
			{Key: "delivered_to_bmc_id", Value: bmcID},
			{Key: "delivered_at", Value: at},
		}}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// setTripProvenanceSeq pins the latest ledger sequence on the trip doc.
func (r *repo) setTripProvenanceSeq(ctx context.Context, id string, seq int64) error {
	_, err := r.trips.UpdateByID(ctx, id,
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}})
	return err
}

// listTrips returns a filtered, newest-first page plus the total count.
func (r *repo) listTrips(ctx context.Context, unionID, date, vanRiderPartyID string, page httpx.Page) ([]domain.RouteTrip, int64, error) {
	filter := bson.D{}
	if unionID != "" {
		filter = append(filter, bson.E{Key: "union_id", Value: unionID})
	}
	if date != "" {
		filter = append(filter, bson.E{Key: "date", Value: date})
	}
	if vanRiderPartyID != "" {
		filter = append(filter, bson.E{Key: "van_rider_party_id", Value: vanRiderPartyID})
	}
	total, err := r.trips.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := r.trips.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	items := []domain.RouteTrip{}
	if err := cur.All(ctx, &items); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

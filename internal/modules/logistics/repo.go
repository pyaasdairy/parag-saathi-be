package logistics

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

// repo owns every MongoDB access for the logistics module. It returns raw
// driver errors wrapped with operation context (mongo.ErrNoDocuments stays
// detectable via errors.Is); the service layer maps them to httpx errors.
// No business logic lives here.
type repo struct {
	consignments *mongo.Collection
	trips        *mongo.Collection
	pours        *mongo.Collection // read-only view into the collection module's pours (sanctioned by spec)
	parties      *mongo.Collection // read-only: rider identity on track views (name+phone)
}

// newRepo binds the repo to its collections via the shared name constants.
func newRepo(db *mongo.Database) *repo {
	return &repo{
		consignments: db.Collection(mongodb.CollConsignments),
		trips:        db.Collection(mongodb.CollRouteTrips),
		pours:        db.Collection(mongodb.CollMilkPours),
		parties:      db.Collection(mongodb.CollParties),
	}
}

// pourRow is the projected slice of a milk pour needed for aggregation.
type pourRow struct {
	ID             primitive.ObjectID `bson:"_id"`
	QuantityLitres float64            `bson:"quantity_litres"`
	FatPct         float64            `bson:"fat_pct"`
	SNFPct         float64            `bson:"snf_pct"`
	Assurance      string             `bson:"assurance"`
}

// findRecordedPours returns the RECORDED pours of one DCS date+shift,
// projected down to the fields the consignment aggregate needs.
func (r *repo) findRecordedPours(ctx context.Context, dcsID primitive.ObjectID, date, shift string) ([]pourRow, error) {
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
		{Key: "assurance", Value: 1},
	}
	cur, err := r.pours.Find(ctx, filter,
		options.Find().SetProjection(projection).SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("find recorded pours: %w", err)
	}
	var rows []pourRow
	if err := cur.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("decode recorded pours: %w", err)
	}
	return rows, nil
}

// insertConsignment stores a new consignment (the service pre-generates its
// ObjectID). The unique index on dcs_id+date+shift turns duplicates into a
// duplicate-key error — kept unwrapped-detectable via %w.
func (r *repo) insertConsignment(ctx context.Context, c *domain.DCSConsignment) error {
	if _, err := r.consignments.InsertOne(ctx, c); err != nil {
		return fmt.Errorf("insert consignment: %w", err)
	}
	return nil
}

// findConsignmentByID loads one consignment.
func (r *repo) findConsignmentByID(ctx context.Context, id primitive.ObjectID) (*domain.DCSConsignment, error) {
	var c domain.DCSConsignment
	if err := r.consignments.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&c); err != nil {
		return nil, fmt.Errorf("find consignment %s: %w", id.Hex(), err)
	}
	return &c, nil
}

// markConsignmentDispatched transitions OPEN→DISPATCHED optimistically:
// the status filter makes the guard atomic without transactions. The seal
// also re-stamps the pour aggregates (extraSet) so pours recorded between
// creation and dispatch are never lost from the consignment's totals.
func (r *repo) markConsignmentDispatched(ctx context.Context, id primitive.ObjectID, at time.Time, extraSet bson.D) (bool, error) {
	set := bson.D{
		{Key: "status", Value: domain.ConsignmentStatusDispatch},
		{Key: "dispatched_at", Value: at},
	}
	set = append(set, extraSet...)
	res, err := r.consignments.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.ConsignmentStatusOpen}},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, fmt.Errorf("mark consignment dispatched: %w", err)
	}
	return res.MatchedCount > 0, nil
}

// markConsignmentPickedUp transitions DISPATCHED→PICKED_UP and pins the
// carrying trip, guarded atomically by the status filter. extraSet carries the
// pickup-time batch mint (batch_code, batch_minted_at) and the van-entered
// evidence so the transition and the mint land in ONE atomic write.
func (r *repo) markConsignmentPickedUp(ctx context.Context, id, tripID primitive.ObjectID, extraSet bson.D) (bool, error) {
	set := bson.D{
		{Key: "status", Value: domain.ConsignmentStatusPickedUp},
		{Key: "route_trip_id", Value: tripID},
	}
	set = append(set, extraSet...)
	res, err := r.consignments.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.ConsignmentStatusDispatch}},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, fmt.Errorf("mark consignment picked up: %w", err)
	}
	return res.MatchedCount > 0, nil
}

// markConsignmentsDelivered transitions every listed PICKED_UP consignment
// to DELIVERED (bulk companion of the trip delivery), stamping delivered_at.
func (r *repo) markConsignmentsDelivered(ctx context.Context, ids []primitive.ObjectID, at time.Time) error {
	_, err := r.consignments.UpdateMany(ctx,
		bson.D{
			{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
			{Key: "status", Value: domain.ConsignmentStatusPickedUp},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.ConsignmentStatusDelivered},
			{Key: "delivered_at", Value: at},
		}}})
	if err != nil {
		return fmt.Errorf("mark consignments delivered: %w", err)
	}
	return nil
}

// gateConsignmentIntake applies a plant intake verdict (F6) to a DELIVERED
// consignment: DELIVERED→ACCEPTED (approve) or DELIVERED→REJECTED (reject).
// The status filter makes the transition atomic; false = lost the state race.
func (r *repo) gateConsignmentIntake(ctx context.Context, id primitive.ObjectID, newStatus string, set bson.D) (bool, error) {
	fullSet := bson.D{{Key: "status", Value: newStatus}}
	fullSet = append(fullSet, set...)
	res, err := r.consignments.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.ConsignmentStatusDelivered}},
		bson.D{{Key: "$set", Value: fullSet}})
	if err != nil {
		return false, fmt.Errorf("gate consignment intake: %w", err)
	}
	return res.MatchedCount > 0, nil
}

// distinctFarmerCount counts the DISTINCT farmers behind a pour set — the
// <farmer count> segment of the pickup-minted batch code (F4).
func (r *repo) distinctFarmerCount(ctx context.Context, pourIDs []primitive.ObjectID) (int, error) {
	if len(pourIDs) == 0 {
		return 0, nil
	}
	vals, err := r.pours.Distinct(ctx, "farmer_party_id",
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: pourIDs}}}})
	if err != nil {
		return 0, fmt.Errorf("distinct farmer count: %w", err)
	}
	return len(vals), nil
}

// setConsignmentProvenanceSeq pins the latest ledger sequence on the doc.
func (r *repo) setConsignmentProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	_, err := r.consignments.UpdateByID(ctx, id,
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}})
	if err != nil {
		return fmt.Errorf("set consignment provenance seq: %w", err)
	}
	return nil
}

// listConsignments returns a filtered, newest-first page plus the total count.
// A nil/empty dcsIDs means "no DCS filter" (wide read roles); a single id
// filters to that society; many ids ($in) scope to a union's member societies.
func (r *repo) listConsignments(ctx context.Context, dcsIDs []primitive.ObjectID, date, status string, page httpx.Page) ([]domain.DCSConsignment, int64, error) {
	filter := bson.D{}
	switch len(dcsIDs) {
	case 0:
		// no DCS filter
	case 1:
		filter = append(filter, bson.E{Key: "dcs_id", Value: dcsIDs[0]})
	default:
		filter = append(filter, bson.E{Key: "dcs_id", Value: bson.D{{Key: "$in", Value: dcsIDs}}})
	}
	if date != "" {
		filter = append(filter, bson.E{Key: "date", Value: date})
	}
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: status})
	}
	total, err := r.consignments.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("count consignments: %w", err)
	}
	cur, err := r.consignments.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, fmt.Errorf("list consignments: %w", err)
	}
	items := []domain.DCSConsignment{}
	if err := cur.All(ctx, &items); err != nil {
		return nil, 0, fmt.Errorf("decode consignments: %w", err)
	}
	return items, total, nil
}

// consignmentBilling sums the billable amount and counts the distinct farmers
// across a consignment's RECORDED pours — the DCS→Union B2B invoice line (fresh
// milk is a single pooled line, GST-exempt). Missing/superseded pours drop out.
func (r *repo) consignmentBilling(ctx context.Context, pourIDs []primitive.ObjectID) (totalAmount float64, farmerCount int, err error) {
	if len(pourIDs) == 0 {
		return 0, 0, nil
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "$in", Value: pourIDs}}},
			{Key: "status", Value: domain.PourStatusRecorded},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "amount", Value: bson.D{{Key: "$sum", Value: "$amount"}}},
			{Key: "farmers", Value: bson.D{{Key: "$addToSet", Value: "$farmer_party_id"}}},
		}}},
	}
	cur, err := r.pours.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, 0, fmt.Errorf("consignment billing: %w", err)
	}
	var rows []struct {
		Amount  float64              `bson:"amount"`
		Farmers []primitive.ObjectID `bson:"farmers"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, 0, fmt.Errorf("decode consignment billing: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}
	return rows[0].Amount, len(rows[0].Farmers), nil
}

// markConsignmentUnionApproved stamps the DCS→Union B2B invoice fields onto a
// consignment (idempotent set — the caller guards against re-approval).
func (r *repo) markConsignmentUnionApproved(ctx context.Context, id primitive.ObjectID, set bson.D) error {
	_, err := r.consignments.UpdateByID(ctx, id, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return fmt.Errorf("mark consignment union approved: %w", err)
	}
	return nil
}

// insertTrip stores a new route trip (the service pre-generates its ObjectID).
func (r *repo) insertTrip(ctx context.Context, t *domain.RouteTrip) error {
	if _, err := r.trips.InsertOne(ctx, t); err != nil {
		return fmt.Errorf("insert trip: %w", err)
	}
	return nil
}

// findTripByID loads one trip.
func (r *repo) findTripByID(ctx context.Context, id primitive.ObjectID) (*domain.RouteTrip, error) {
	var t domain.RouteTrip
	if err := r.trips.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&t); err != nil {
		return nil, fmt.Errorf("find trip %s: %w", id.Hex(), err)
	}
	return &t, nil
}

// markStopPickedUp stamps the matched stop with pickup time/temperature (plus
// optional geo/photo evidence) and moves the trip to IN_PROGRESS (idempotent
// once it is already there). Nil lat/lng and an empty photoURI are "not
// captured" and leave the stop untouched.
func (r *repo) markStopPickedUp(ctx context.Context, tripID, consignmentID primitive.ObjectID, at time.Time, tempC float64, notes string, lat, lng *float64, photoURI, analyzerPhotoURI string, measuredVolumeLitres *float64, batchCode string) error {
	set := bson.D{
		{Key: "stops.$.picked_up_at", Value: at},
		{Key: "stops.$.temp_c", Value: tempC},
		{Key: "stops.$.notes", Value: notes},
		{Key: "status", Value: domain.TripStatusInProgress},
	}
	if lat != nil && lng != nil {
		set = append(set,
			bson.E{Key: "stops.$.lat", Value: *lat},
			bson.E{Key: "stops.$.lng", Value: *lng})
	}
	if photoURI != "" {
		set = append(set, bson.E{Key: "stops.$.photo_uri", Value: photoURI})
	}
	if analyzerPhotoURI != "" {
		set = append(set, bson.E{Key: "stops.$.analyzer_photo_uri", Value: analyzerPhotoURI})
	}
	if measuredVolumeLitres != nil {
		set = append(set, bson.E{Key: "stops.$.measured_volume_litres", Value: *measuredVolumeLitres})
	}
	if batchCode != "" {
		set = append(set, bson.E{Key: "stops.$.batch_code", Value: batchCode})
	}
	_, err := r.trips.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: tripID}, {Key: "stops.consignment_id", Value: consignmentID}},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return fmt.Errorf("mark stop picked up: %w", err)
	}
	return nil
}

// pushColdChain appends one temperature sample to the trip's cold-chain log.
func (r *repo) pushColdChain(ctx context.Context, tripID primitive.ObjectID, entry domain.ColdChainEntry) error {
	_, err := r.trips.UpdateByID(ctx, tripID,
		bson.D{{Key: "$push", Value: bson.D{{Key: "cold_chain", Value: entry}}}})
	if err != nil {
		return fmt.Errorf("push cold chain entry: %w", err)
	}
	return nil
}

// markTripDelivered transitions IN_PROGRESS→DELIVERED optimistically and
// records the receiving facility — a BMC or a PROCESSING_PLANT (F5).
// delivered_to_bmc_id keeps carrying the facility id for compatibility;
// delivered_to_facility_id/delivered_facility_type carry the new semantics.
func (r *repo) markTripDelivered(ctx context.Context, tripID, facilityID primitive.ObjectID, facilityType string, at time.Time, receivedBy, notes, photoURI string) (bool, error) {
	set := bson.D{
		{Key: "status", Value: domain.TripStatusDelivered},
		{Key: "delivered_to_bmc_id", Value: facilityID},
		{Key: "delivered_to_facility_id", Value: facilityID},
		{Key: "delivered_facility_type", Value: facilityType},
		{Key: "delivered_at", Value: at},
	}
	// Handover evidence (F6b) — only written when the rider captured it.
	if receivedBy != "" {
		set = append(set, bson.E{Key: "delivered_received_by", Value: receivedBy})
	}
	if notes != "" {
		set = append(set, bson.E{Key: "delivered_notes", Value: notes})
	}
	if photoURI != "" {
		set = append(set, bson.E{Key: "delivered_photo_uri", Value: photoURI})
	}
	res, err := r.trips.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: tripID}, {Key: "status", Value: domain.TripStatusInProgress}},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, fmt.Errorf("mark trip delivered: %w", err)
	}
	return res.MatchedCount > 0, nil
}

// partyByID loads one party (rider identity on track views). NotFound-safe:
// returns nil, nil when absent.
func (r *repo) partyByID(ctx context.Context, id primitive.ObjectID) (*domain.Party, error) {
	var p domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("party by id: %w", err)
	}
	return &p, nil
}

// setTripProvenanceSeq pins the latest ledger sequence on the trip doc.
func (r *repo) setTripProvenanceSeq(ctx context.Context, id primitive.ObjectID, seq int64) error {
	_, err := r.trips.UpdateByID(ctx, id,
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}})
	if err != nil {
		return fmt.Errorf("set trip provenance seq: %w", err)
	}
	return nil
}

// listTrips returns a filtered, newest-first page plus the total count.
// Zero ObjectIDs mean "no filter" on that field.
func (r *repo) listTrips(ctx context.Context, unionID primitive.ObjectID, date string, vanRiderPartyID primitive.ObjectID, page httpx.Page) ([]domain.RouteTrip, int64, error) {
	filter := bson.D{}
	if !unionID.IsZero() {
		filter = append(filter, bson.E{Key: "union_id", Value: unionID})
	}
	if date != "" {
		filter = append(filter, bson.E{Key: "date", Value: date})
	}
	if !vanRiderPartyID.IsZero() {
		filter = append(filter, bson.E{Key: "van_rider_party_id", Value: vanRiderPartyID})
	}
	total, err := r.trips.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("count trips: %w", err)
	}
	cur, err := r.trips.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, fmt.Errorf("list trips: %w", err)
	}
	items := []domain.RouteTrip{}
	if err := cur.All(ctx, &items); err != nil {
		return nil, 0, fmt.Errorf("decode trips: %w", err)
	}
	return items, total, nil
}

// recordTripLocation refreshes the van's last-known position (live tracking).
func (r *repo) recordTripLocation(ctx context.Context, tripID primitive.ObjectID, lat, lng float64, at time.Time) error {
	_, err := r.trips.UpdateByID(ctx, tripID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "last_geo_lat", Value: lat},
		{Key: "last_geo_lng", Value: lng},
		{Key: "last_location_at", Value: at},
	}}})
	if err != nil {
		return fmt.Errorf("record trip location: %w", err)
	}
	return nil
}

// activeTripsForUnion returns the union's IN_PROGRESS trips (newest first) — the
// set the destination BMC watches as inbound vans and a supervisor oversees.
func (r *repo) activeTripsForUnion(ctx context.Context, unionID primitive.ObjectID) ([]domain.RouteTrip, error) {
	cur, err := r.trips.Find(ctx,
		bson.D{{Key: "union_id", Value: unionID}, {Key: "status", Value: domain.TripStatusInProgress}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("active trips for union: %w", err)
	}
	items := []domain.RouteTrip{}
	if err := cur.All(ctx, &items); err != nil {
		return nil, fmt.Errorf("decode active trips: %w", err)
	}
	return items, nil
}

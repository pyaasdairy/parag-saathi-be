package dashboards

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// repo reads (never writes) the pour, invoice and role-assignment collections
// owned by other modules — dashboards is a pure read-side aggregate.
type repo struct {
	pours        *mongo.Collection
	invoices     *mongo.Collection
	assignments  *mongo.Collection
	animals      *mongo.Collection
	consignments *mongo.Collection
}

func newRepo(db *mongo.Database) *repo {
	return &repo{
		pours:        db.Collection(mongodb.CollMilkPours),
		invoices:     db.Collection(mongodb.CollInvoices),
		assignments:  db.Collection(mongodb.CollRoleAssignments),
		animals:      db.Collection(mongodb.CollAnimals),
		consignments: db.Collection(mongodb.CollConsignments),
	}
}

// pourRollup is the aggregation shape for grouped pour totals.
type pourRollup struct {
	Date  string  `bson:"_id"`
	Qty   float64 `bson:"qty"`
	Amt   float64 `bson:"amt"`
	Count int     `bson:"count"`
}

// rollupByDay groups RECORDED pours matching `match` by pour_date.
func (r *repo) rollupByDay(ctx context.Context, match bson.D) ([]pourRollup, error) {
	match = append(match, bson.E{Key: "status", Value: domain.PourStatusRecorded})
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$pour_date"},
			{Key: "qty", Value: bson.D{{Key: "$sum", Value: "$quantity_litres"}}},
			{Key: "amt", Value: bson.D{{Key: "$sum", Value: "$amount"}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "_id", Value: -1}}}},
	}
	cur, err := r.pours.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("rollup by day: %w", err)
	}
	var out []pourRollup
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("decode rollup: %w", err)
	}
	return out, nil
}

// farmerPourDays rolls up a farmer's pours since `sinceDate` (inclusive).
func (r *repo) farmerPourDays(ctx context.Context, farmerID primitive.ObjectID, sinceDate string) ([]pourRollup, error) {
	return r.rollupByDay(ctx, bson.D{
		{Key: "farmer_party_id", Value: farmerID},
		{Key: "pour_date", Value: bson.D{{Key: "$gte", Value: sinceDate}}},
	})
}

// dcsPourDays rolls up a DCS's pours since `sinceDate` (inclusive).
func (r *repo) dcsPourDays(ctx context.Context, dcsID primitive.ObjectID, sinceDate string) ([]pourRollup, error) {
	return r.rollupByDay(ctx, bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "pour_date", Value: bson.D{{Key: "$gte", Value: sinceDate}}},
	})
}

// pendingInvoices returns (count, totalAmount) of a farmer's unpaid invoices
// (anything not yet PAID).
func (r *repo) pendingInvoices(ctx context.Context, farmerID primitive.ObjectID) (int, float64, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "farmer_party_id", Value: farmerID},
			{Key: "status", Value: bson.D{{Key: "$ne", Value: domain.InvoiceStatusPaid}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "amt", Value: bson.D{{Key: "$sum", Value: "$total_amount"}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}
	cur, err := r.invoices.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, 0, fmt.Errorf("pending invoices: %w", err)
	}
	var rows []struct {
		Amt   float64 `bson:"amt"`
		Count int     `bson:"count"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, 0, fmt.Errorf("decode pending invoices: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}
	return rows[0].Count, rows[0].Amt, nil
}

// activeFarmersToday counts distinct farmers who poured at a DCS on `date`.
func (r *repo) activeFarmersToday(ctx context.Context, dcsID primitive.ObjectID, date string) (int, error) {
	vals, err := r.pours.Distinct(ctx, "farmer_party_id", bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "pour_date", Value: date},
		{Key: "status", Value: domain.PourStatusRecorded},
	})
	if err != nil {
		return 0, fmt.Errorf("active farmers: %w", err)
	}
	return len(vals), nil
}

// memberCount counts ACTIVE FARMER assignments at a DCS.
func (r *repo) memberCount(ctx context.Context, dcsID primitive.ObjectID) (int, error) {
	n, err := r.assignments.CountDocuments(ctx, bson.D{
		{Key: "org_unit_id", Value: dcsID},
		{Key: "role_code", Value: domain.RoleFarmer},
		{Key: "status", Value: domain.RoleAssignmentActive},
	})
	if err != nil {
		return 0, fmt.Errorf("member count: %w", err)
	}
	return int(n), nil
}

// animalCount counts the registered animals a farmer owns.
func (r *repo) animalCount(ctx context.Context, farmerID primitive.ObjectID) (int, error) {
	n, err := r.animals.CountDocuments(ctx, bson.D{{Key: "owner_party_id", Value: farmerID}})
	if err != nil {
		return 0, fmt.Errorf("animal count: %w", err)
	}
	return int(n), nil
}

// dcsAvgFatSnf returns the quantity-weighted average fat% and SNF% over a DCS's
// RECORDED pours since `sinceDate` (both 0 when there are no pours).
func (r *repo) dcsAvgFatSnf(ctx context.Context, dcsID primitive.ObjectID, sinceDate string) (avgFat, avgSNF float64, err error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "dcs_id", Value: dcsID},
			{Key: "pour_date", Value: bson.D{{Key: "$gte", Value: sinceDate}}},
			{Key: "status", Value: domain.PourStatusRecorded},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "qty", Value: bson.D{{Key: "$sum", Value: "$quantity_litres"}}},
			{Key: "fatw", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$multiply", Value: bson.A{"$fat_pct", "$quantity_litres"}}}}}},
			{Key: "snfw", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$multiply", Value: bson.A{"$snf_pct", "$quantity_litres"}}}}}},
		}}},
	}
	cur, err := r.pours.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, 0, fmt.Errorf("dcs avg fat/snf: %w", err)
	}
	var rows []struct {
		Qty  float64 `bson:"qty"`
		FatW float64 `bson:"fatw"`
		SNFW float64 `bson:"snfw"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, 0, fmt.Errorf("decode dcs avg fat/snf: %w", err)
	}
	if len(rows) == 0 || rows[0].Qty == 0 {
		return 0, 0, nil
	}
	return round2(rows[0].FatW / rows[0].Qty), round2(rows[0].SNFW / rows[0].Qty), nil
}

// dcsRejectedConsignments counts a DCS's consignments rejected since `sinceDate`
// — the "quality failures in the last 30 days" tile.
func (r *repo) dcsRejectedConsignments(ctx context.Context, dcsID primitive.ObjectID, sinceDate string) (int, error) {
	n, err := r.consignments.CountDocuments(ctx, bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "status", Value: domain.ConsignmentStatusRejected},
		{Key: "date", Value: bson.D{{Key: "$gte", Value: sinceDate}}},
	})
	if err != nil {
		return 0, fmt.Errorf("dcs rejected consignments: %w", err)
	}
	return int(n), nil
}

// dcsHasOpenConsignment reports whether an OPEN (unsealed) consignment exists
// for a DCS on `date` — the "today's shift still open" indicator.
func (r *repo) dcsHasOpenConsignment(ctx context.Context, dcsID primitive.ObjectID, date string) (bool, error) {
	n, err := r.consignments.CountDocuments(ctx, bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "date", Value: date},
		{Key: "status", Value: domain.ConsignmentStatusOpen},
	})
	if err != nil {
		return false, fmt.Errorf("dcs open consignment: %w", err)
	}
	return n > 0, nil
}

// monthStartIST returns the YYYY-MM-01 key for the IST month containing now.
func monthStartIST(now time.Time) string {
	ist := time.FixedZone("IST", 5*3600+1800)
	t := now.In(ist)
	return fmt.Sprintf("%04d-%02d-01", t.Year(), int(t.Month()))
}

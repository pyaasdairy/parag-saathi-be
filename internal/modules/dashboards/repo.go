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

// repo reads (never writes) the pour, invoice, role-assignment and animal
// collections owned by other modules — dashboards is a pure read-side aggregate.
type repo struct {
	pours       *mongo.Collection
	invoices    *mongo.Collection
	assignments *mongo.Collection
	animals     *mongo.Collection
}

func newRepo(db *mongo.Database) *repo {
	return &repo{
		pours:       db.Collection(mongodb.CollMilkPours),
		invoices:    db.Collection(mongodb.CollInvoices),
		assignments: db.Collection(mongodb.CollRoleAssignments),
		animals:     db.Collection(mongodb.CollAnimals),
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

// animalCount counts a farmer's ACTIVE animals.
func (r *repo) animalCount(ctx context.Context, farmerID primitive.ObjectID) (int, error) {
	n, err := r.animals.CountDocuments(ctx, bson.D{
		{Key: "owner_party_id", Value: farmerID},
		{Key: "status", Value: domain.AnimalStatusActive},
	})
	if err != nil {
		return 0, fmt.Errorf("animal count: %w", err)
	}
	return int(n), nil
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

// monthStartIST returns the YYYY-MM-01 key for the IST month containing now.
func monthStartIST(now time.Time) string {
	ist := time.FixedZone("IST", 5*3600+1800)
	t := now.In(ist)
	return fmt.Sprintf("%04d-%02d-01", t.Year(), int(t.Month()))
}

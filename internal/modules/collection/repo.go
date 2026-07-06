package collection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// repo owns every MongoDB access of the collection module. It maps
// mongo.ErrNoDocuments to httpx.NotFound and wraps everything unexpected in
// httpx.Internal, so the service layer only ever sees AppError-compatible
// errors.
type repo struct {
	rateCharts      *mongo.Collection
	readings        *mongo.Collection
	pours           *mongo.Collection
	invoices        *mongo.Collection
	roleAssignments *mongo.Collection
	parties         *mongo.Collection
	notifications   *mongo.Collection
	counters        *mongo.Collection
	consignments    *mongo.Collection // read-only view: freeze checks against the logistics pooling boundary
}

// newRepo binds the repo to the module's collections.
func newRepo(d *deps.Deps) *repo {
	return &repo{
		rateCharts:      d.DB.Collection(mongodb.CollRateCharts),
		readings:        d.DB.Collection(mongodb.CollAnalyzerReadings),
		pours:           d.DB.Collection(mongodb.CollMilkPours),
		invoices:        d.DB.Collection(mongodb.CollInvoices),
		roleAssignments: d.DB.Collection(mongodb.CollRoleAssignments),
		parties:         d.DB.Collection(mongodb.CollParties),
		notifications:   d.DB.Collection(mongodb.CollNotifications),
		counters:        d.DB.Collection(mongodb.CollCounters),
		consignments:    d.DB.Collection(mongodb.CollConsignments),
	}
}

// --- rate charts ---

// deactivateActiveCharts flips every active chart of an org unit to inactive
// (a new chart supersedes the old ones).
func (r *repo) deactivateActiveCharts(ctx context.Context, orgUnitID string) error {
	_, err := r.rateCharts.UpdateMany(ctx,
		bson.D{{Key: "org_unit_id", Value: orgUnitID}, {Key: "active", Value: true}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("deactivate rate charts: %w", err))
	}
	return nil
}

// insertRateChart stores a new chart.
func (r *repo) insertRateChart(ctx context.Context, rc *domain.RateChart) error {
	if _, err := r.rateCharts.InsertOne(ctx, rc); err != nil {
		return httpx.Internal(fmt.Errorf("insert rate chart: %w", err))
	}
	return nil
}

// activeChartsForOrgs returns active charts already effective at `now` for
// any of the given org units (a DCS plus its ancestor chain — a small set).
func (r *repo) activeChartsForOrgs(ctx context.Context, orgUnitIDs []string, now time.Time) ([]domain.RateChart, error) {
	cur, err := r.rateCharts.Find(ctx, bson.D{
		{Key: "org_unit_id", Value: bson.D{{Key: "$in", Value: orgUnitIDs}}},
		{Key: "active", Value: true},
		{Key: "effective_from", Value: bson.D{{Key: "$lte", Value: now}}},
	})
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find active rate charts: %w", err))
	}
	var out []domain.RateChart
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rate charts: %w", err))
	}
	return out, nil
}

// --- analyzer readings ---

// insertReading stores a new analyzer reading.
func (r *repo) insertReading(ctx context.Context, rd *domain.AnalyzerReading) error {
	if _, err := r.readings.InsertOne(ctx, rd); err != nil {
		return httpx.Internal(fmt.Errorf("insert reading: %w", err))
	}
	return nil
}

// listReadings pages a DCS's readings, optionally restricted to the
// [dayStart, dayEnd) window, newest first.
func (r *repo) listReadings(ctx context.Context, dcsID string, dayStart, dayEnd *time.Time, page httpx.Page) ([]domain.AnalyzerReading, int64, error) {
	filter := bson.D{{Key: "dcs_id", Value: dcsID}}
	if dayStart != nil && dayEnd != nil {
		filter = append(filter, bson.E{Key: "created_at", Value: bson.D{
			{Key: "$gte", Value: *dayStart},
			{Key: "$lt", Value: *dayEnd},
		}})
	}
	total, err := r.readings.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count readings: %w", err))
	}
	cur, err := r.readings.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list readings: %w", err))
	}
	out := []domain.AnalyzerReading{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode readings: %w", err))
	}
	return out, total, nil
}

// --- membership & parties (read-only views onto foundation collections) ---

// farmerIsActiveMember reports whether the party holds a usable ACTIVE FARMER
// assignment at the DCS at instant `now`.
func (r *repo) farmerIsActiveMember(ctx context.Context, farmerPartyID, dcsID string, now time.Time) (bool, error) {
	cur, err := r.roleAssignments.Find(ctx, bson.D{
		{Key: "party_id", Value: farmerPartyID},
		{Key: "org_unit_id", Value: dcsID},
		{Key: "role_code", Value: domain.RoleFarmer},
		{Key: "status", Value: domain.RoleAssignmentActive},
	})
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("find farmer assignment: %w", err))
	}
	var assignments []domain.RoleAssignment
	if err := cur.All(ctx, &assignments); err != nil {
		return false, httpx.Internal(fmt.Errorf("decode farmer assignments: %w", err))
	}
	for _, ra := range assignments {
		if ra.UsableAt(now) {
			return true, nil
		}
	}
	return false, nil
}

// getParty loads a party (for notification phone/language).
func (r *repo) getParty(ctx context.Context, partyID string) (*domain.Party, error) {
	var p domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: partyID}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party " + partyID)
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("get party: %w", err))
	}
	return &p, nil
}

// --- pours ---

// insertPour inserts a pour; duplicate=true means the client_event_id was
// already stored (offline replay — the caller fetches the existing doc).
func (r *repo) insertPour(ctx context.Context, p *domain.MilkPour) (duplicate bool, err error) {
	if _, err := r.pours.InsertOne(ctx, p); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return true, nil
		}
		return false, httpx.Internal(fmt.Errorf("insert pour: %w", err))
	}
	return false, nil
}

// pourByClientEventID fetches a pour by its idempotency key.
func (r *repo) pourByClientEventID(ctx context.Context, clientEventID string) (*domain.MilkPour, error) {
	var p domain.MilkPour
	err := r.pours.FindOne(ctx, bson.D{{Key: "client_event_id", Value: clientEventID}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("pour")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("get pour by client event: %w", err))
	}
	return &p, nil
}

// pourByID fetches a pour by ID.
func (r *repo) pourByID(ctx context.Context, id string) (*domain.MilkPour, error) {
	var p domain.MilkPour
	err := r.pours.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("pour " + id)
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("get pour: %w", err))
	}
	return &p, nil
}

// markPourSuperseded flips a RECORDED pour to SUPERSEDED. The status guard in
// the filter makes concurrent supersedes lose cleanly (optimistic, no
// transactions on a standalone server).
func (r *repo) markPourSuperseded(ctx context.Context, id string) (bool, error) {
	res, err := r.pours.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.PourStatusRecorded}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: domain.PourStatusSuperseded}}}},
	)
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("mark pour superseded: %w", err))
	}
	return res.MatchedCount == 1, nil
}

// setPourProvenanceSeq stamps the ledger sequence onto the pour document.
func (r *repo) setPourProvenanceSeq(ctx context.Context, id string, seq int64) error {
	_, err := r.pours.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("set pour provenance seq: %w", err))
	}
	return nil
}

// pourListFilter narrows a pour listing.
type pourListFilter struct {
	DCSID         string
	Date          string
	Shift         string
	FarmerPartyID string
}

// listPours pages pours matching the filter, newest first, plus the total
// matching count for pagination meta.
func (r *repo) listPours(ctx context.Context, f pourListFilter, page httpx.Page) ([]domain.MilkPour, int64, error) {
	filter := bson.D{}
	if f.DCSID != "" {
		filter = append(filter, bson.E{Key: "dcs_id", Value: f.DCSID})
	}
	if f.Date != "" {
		filter = append(filter, bson.E{Key: "pour_date", Value: f.Date})
	}
	if f.Shift != "" {
		filter = append(filter, bson.E{Key: "shift", Value: f.Shift})
	}
	if f.FarmerPartyID != "" {
		filter = append(filter, bson.E{Key: "farmer_party_id", Value: f.FarmerPartyID})
	}
	total, err := r.pours.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count pours: %w", err))
	}
	cur, err := r.pours.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "poured_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list pours: %w", err))
	}
	out := []domain.MilkPour{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode pours: %w", err))
	}
	return out, total, nil
}

// recordedPoursForDay returns a DCS's RECORDED pours for one settlement day
// (internal aggregation input for invoice generation).
func (r *repo) recordedPoursForDay(ctx context.Context, dcsID, dateKey string) ([]domain.MilkPour, error) {
	cur, err := r.pours.Find(ctx, bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "pour_date", Value: dateKey},
		{Key: "status", Value: domain.PourStatusRecorded},
	}, options.Find().SetSort(bson.D{{Key: "poured_at", Value: 1}}))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find recorded pours: %w", err))
	}
	var out []domain.MilkPour
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode recorded pours: %w", err))
	}
	return out, nil
}

// --- invoices ---

// pourIsInvoiced reports whether a non-HOLD invoice already aggregates the
// pour (such pours are frozen — corrections must go through settlement).
func (r *repo) pourIsInvoiced(ctx context.Context, pourID string) (bool, error) {
	n, err := r.invoices.CountDocuments(ctx, bson.D{
		{Key: "pour_ids", Value: pourID},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: domain.InvoiceStatusHold}}},
	})
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("count invoices for pour: %w", err))
	}
	return n > 0, nil
}

// invoicedPourIDs returns the set of pour IDs already aggregated by any
// invoice of the DCS on the given day.
func (r *repo) invoicedPourIDs(ctx context.Context, dcsID, dateKey string) (map[string]bool, error) {
	cur, err := r.invoices.Find(ctx, bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "invoice_date", Value: dateKey},
	}, options.Find().SetProjection(bson.D{{Key: "pour_ids", Value: 1}}))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find day invoices: %w", err))
	}
	var docs []struct {
		PourIDs []string `bson:"pour_ids"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode day invoices: %w", err))
	}
	set := make(map[string]bool)
	for _, d := range docs {
		for _, id := range d.PourIDs {
			set[id] = true
		}
	}
	return set, nil
}

// insertInvoice inserts an invoice; duplicate=true means the farmer already
// has an invoice for that DCS+day (the unique index is the true guard).
func (r *repo) insertInvoice(ctx context.Context, inv *domain.Invoice) (duplicate bool, err error) {
	if _, err := r.invoices.InsertOne(ctx, inv); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return true, nil
		}
		return false, httpx.Internal(fmt.Errorf("insert invoice: %w", err))
	}
	return false, nil
}

// setInvoiceProvenanceSeq stamps the ledger sequence onto the invoice document.
func (r *repo) setInvoiceProvenanceSeq(ctx context.Context, id string, seq int64) error {
	_, err := r.invoices.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "provenance_seq", Value: seq}}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("set invoice provenance seq: %w", err))
	}
	return nil
}

// invoiceByID fetches one invoice.
func (r *repo) invoiceByID(ctx context.Context, id string) (*domain.Invoice, error) {
	var inv domain.Invoice
	err := r.invoices.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("invoice " + id)
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("get invoice: %w", err))
	}
	return &inv, nil
}

// invoiceListFilter narrows an invoice listing.
type invoiceListFilter struct {
	DCSID         string
	Date          string
	FarmerPartyID string
	Status        string
}

// listInvoices pages invoices matching the filter, newest first, plus the
// total matching count for pagination meta.
func (r *repo) listInvoices(ctx context.Context, f invoiceListFilter, page httpx.Page) ([]domain.Invoice, int64, error) {
	filter := bson.D{}
	if f.DCSID != "" {
		filter = append(filter, bson.E{Key: "dcs_id", Value: f.DCSID})
	}
	if f.Date != "" {
		filter = append(filter, bson.E{Key: "invoice_date", Value: f.Date})
	}
	if f.FarmerPartyID != "" {
		filter = append(filter, bson.E{Key: "farmer_party_id", Value: f.FarmerPartyID})
	}
	if f.Status != "" {
		filter = append(filter, bson.E{Key: "status", Value: f.Status})
	}
	total, err := r.invoices.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count invoices: %w", err))
	}
	cur, err := r.invoices.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "issued_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list invoices: %w", err))
	}
	out := []domain.Invoice{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode invoices: %w", err))
	}
	return out, total, nil
}

// invoiceByFarmerDay loads the unique (farmer, DCS, day) invoice.
func (r *repo) invoiceByFarmerDay(ctx context.Context, farmerPartyID, dcsID, dateKey string) (*domain.Invoice, error) {
	var inv domain.Invoice
	err := r.invoices.FindOne(ctx, bson.D{
		{Key: "farmer_party_id", Value: farmerPartyID},
		{Key: "dcs_id", Value: dcsID},
		{Key: "invoice_date", Value: dateKey},
	}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("invoice for farmer " + farmerPartyID + " on " + dateKey)
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("get farmer day invoice: %w", err))
	}
	return &inv, nil
}

// appendPoursToInvoice merges late pours into a still-ISSUED invoice: the
// status guard in the filter makes the merge atomic against a concurrent
// settlement claim. Returns false when the invoice already left ISSUED.
func (r *repo) appendPoursToInvoice(ctx context.Context, invoiceID string, pourIDs []string, addQty, addAmount float64) (bool, error) {
	res, err := r.invoices.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: invoiceID}, {Key: "status", Value: domain.InvoiceStatusIssued}},
		bson.D{
			{Key: "$push", Value: bson.D{{Key: "pour_ids", Value: bson.D{{Key: "$each", Value: pourIDs}}}}},
			{Key: "$inc", Value: bson.D{
				{Key: "total_quantity_litres", Value: addQty},
				{Key: "total_amount", Value: addAmount},
			}},
		},
	)
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("append pours to invoice: %w", err))
	}
	return res.MatchedCount == 1, nil
}

// setInvoiceTotals stamps rounded totals after a float $inc merge.
func (r *repo) setInvoiceTotals(ctx context.Context, invoiceID string, qty, amount float64) error {
	_, err := r.invoices.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: invoiceID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "total_quantity_litres", Value: qty},
			{Key: "total_amount", Value: amount},
		}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("set invoice totals: %w", err))
	}
	return nil
}

// shiftConsigned reports whether a non-REJECTED consignment already pools the
// DCS's date+shift — once it exists the shift's pour set is frozen: new pours
// and corrections would silently diverge from the consignment's totals and
// trace refs.
func (r *repo) shiftConsigned(ctx context.Context, dcsID, dateKey, shift string) (bool, error) {
	n, err := r.consignments.CountDocuments(ctx, bson.D{
		{Key: "dcs_id", Value: dcsID},
		{Key: "date", Value: dateKey},
		{Key: "shift", Value: shift},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: domain.ConsignmentStatusRejected}}},
	})
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("count consignments for shift: %w", err))
	}
	return n > 0, nil
}

// --- counters & notifications ---

// nextInvoiceSeq atomically increments and returns the per-DCS-per-day
// invoice counter (findOneAndUpdate upsert — safe without transactions).
func (r *repo) nextInvoiceSeq(ctx context.Context, key string) (int, error) {
	var doc struct {
		Seq int `bson:"seq"`
	}
	err := r.counters.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: key}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: 1}}}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("next invoice seq: %w", err))
	}
	return doc.Seq, nil
}

// queueNotification writes one outbox notification document (the provider
// worker in the notifications module sends it).
func (r *repo) queueNotification(ctx context.Context, n *domain.Notification) error {
	if _, err := r.notifications.InsertOne(ctx, n); err != nil {
		return httpx.Internal(fmt.Errorf("queue notification: %w", err))
	}
	return nil
}

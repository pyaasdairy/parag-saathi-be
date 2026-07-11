package platformops

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
	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// repository is all MongoDB access for the platformops module. It owns the
// notifications outbox, reads audit_logs strictly read-only (writes go
// through deps.Audit only — the collection is insert-only by design, §12),
// and reads parties / role_assignments for the support lookup and for
// addressing safety alerts. bmc_lots / processing_batches are read solely to
// resolve a gate-blocked subject back to its org unit.
// The control-tower stats read (adminStats) additionally counts org_units,
// milk_pours, kyc_records, invoices and qc_results strictly read-only, and the
// product master owns the products collection.
type repository struct {
	auditLogs     *mongo.Collection
	notifications *mongo.Collection
	parties       *mongo.Collection
	assignments   *mongo.Collection
	bmcLots       *mongo.Collection
	batches       *mongo.Collection
	orgUnits      *mongo.Collection
	pours         *mongo.Collection
	kyc           *mongo.Collection
	invoices      *mongo.Collection
	qcResults     *mongo.Collection
	products      *mongo.Collection
	settings      *mongo.Collection
	settlements   *mongo.Collection // read-only: settled-amount aggregate for the control tower
}

// newRepository binds the repository to its collections.
func newRepository(db *mongo.Database) *repository {
	return &repository{
		auditLogs:     db.Collection(mongodb.CollAuditLogs),
		notifications: db.Collection(mongodb.CollNotifications),
		parties:       db.Collection(mongodb.CollParties),
		assignments:   db.Collection(mongodb.CollRoleAssignments),
		bmcLots:       db.Collection(mongodb.CollBMCLots),
		batches:       db.Collection(mongodb.CollBatches),
		orgUnits:      db.Collection(mongodb.CollOrgUnits),
		pours:         db.Collection(mongodb.CollMilkPours),
		kyc:           db.Collection(mongodb.CollKYCRecords),
		invoices:      db.Collection(mongodb.CollInvoices),
		qcResults:     db.Collection(mongodb.CollQCResults),
		products:      db.Collection(mongodb.CollProducts),
		settings:      db.Collection(mongodb.CollSettings),
		settlements:   db.Collection(mongodb.CollSettlements),
	}
}

// auditFilter builds the bson filter shared by the list and export reads.
func auditFilter(f auditLogFilter) bson.D {
	filter := bson.D{}
	if f.ActorPartyID != "" {
		filter = append(filter, bson.E{Key: "actor_party_id", Value: f.ActorPartyID})
	}
	if f.TargetType != "" {
		filter = append(filter, bson.E{Key: "target_type", Value: f.TargetType})
	}
	if f.Action != "" {
		filter = append(filter, bson.E{Key: "action", Value: f.Action})
	}
	if f.From != nil || f.To != nil {
		tsRange := bson.D{}
		if f.From != nil {
			tsRange = append(tsRange, bson.E{Key: "$gte", Value: *f.From})
		}
		if f.To != nil {
			tsRange = append(tsRange, bson.E{Key: "$lte", Value: *f.To})
		}
		filter = append(filter, bson.E{Key: "ts", Value: tsRange})
	}
	return filter
}

// listAuditLogs returns a page of DOMAIN governance entries (who-did-what),
// newest first, plus the total matching count. Raw HTTP access-log entries
// (action "http.*", written by the mutation-audit middleware) are excluded
// unless the caller explicitly filters on an action — the governance console
// wants business events, not a request log.
func (r *repository) listAuditLogs(ctx context.Context, f auditLogFilter, page httpx.Page) ([]audit.Entry, int64, error) {
	filter := auditFilter(f)
	if f.Action == "" {
		filter = append(filter, bson.E{Key: "action", Value: bson.D{
			{Key: "$not", Value: primitive.Regex{Pattern: "^http\\.", Options: ""}},
		}})
	}

	total, err := r.auditLogs.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count audit logs: %w", err))
	}

	cur, err := r.auditLogs.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "ts", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list audit logs: %w", err))
	}
	entries := []audit.Entry{}
	if err := cur.All(ctx, &entries); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode audit logs: %w", err))
	}
	return entries, total, nil
}

// exportAuditLogs returns up to maxRows entries in the window, newest first —
// the bounded read behind the immutable audit export (§12).
func (r *repository) exportAuditLogs(ctx context.Context, f auditLogFilter, maxRows int64) ([]audit.Entry, error) {
	cur, err := r.auditLogs.Find(ctx, auditFilter(f), options.Find().
		SetSort(bson.D{{Key: "ts", Value: -1}}).
		SetLimit(maxRows),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("export audit logs: %w", err))
	}
	entries := []audit.Entry{}
	if err := cur.All(ctx, &entries); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode audit log export: %w", err))
	}
	return entries, nil
}

// insertNotification queues one outbox document.
func (r *repository) insertNotification(ctx context.Context, n *StoredNotification) error {
	if _, err := r.notifications.InsertOne(ctx, n); err != nil {
		return httpx.Internal(fmt.Errorf("insert notification: %w", err))
	}
	return nil
}

// listNotifications returns a page of outbox documents, newest first,
// optionally filtered by phone and status.
func (r *repository) listNotifications(ctx context.Context, phone, status string, page httpx.Page) ([]StoredNotification, int64, error) {
	filter := bson.D{}
	if phone != "" {
		filter = append(filter, bson.E{Key: "phone", Value: phone})
	}
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: status})
	}

	total, err := r.notifications.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count notifications: %w", err))
	}

	cur, err := r.notifications.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "queued_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list notifications: %w", err))
	}
	notifications := []StoredNotification{}
	if err := cur.All(ctx, &notifications); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode notifications: %w", err))
	}
	return notifications, total, nil
}

// listNotificationsForParty returns one party's own notifications, newest
// first, plus the total count — the GET /notifications/me inbox read.
func (r *repository) listNotificationsForParty(ctx context.Context, partyID primitive.ObjectID, page httpx.Page) ([]StoredNotification, int64, error) {
	filter := bson.D{{Key: "party_id", Value: partyID}}

	total, err := r.notifications.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count party notifications: %w", err))
	}

	cur, err := r.notifications.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "queued_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit),
	)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list party notifications: %w", err))
	}
	notifications := []StoredNotification{}
	if err := cur.All(ctx, &notifications); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode party notifications: %w", err))
	}
	return notifications, total, nil
}

// markNotificationRead stamps read_at on one of the party's OWN notifications
// (the party filter makes cross-party reads impossible by construction) and
// returns the updated document. Already-read documents are returned as-is —
// the operation is idempotent; a foreign or unknown id is NotFound.
func (r *repository) markNotificationRead(ctx context.Context, id, partyID primitive.ObjectID, now time.Time) (*StoredNotification, error) {
	var n StoredNotification
	err := r.notifications.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "party_id", Value: partyID}},
		// $min keeps the FIRST read time on concurrent/repeated reads.
		bson.D{{Key: "$min", Value: bson.D{{Key: "read_at", Value: now}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&n)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("notification")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("mark notification read: %w", err))
	}
	return &n, nil
}

// claimQueuedNotification atomically claims the oldest QUEUED document
// (QUEUED→SENT, sent_at, provider_ref) and returns it, or nil when the queue
// is drained. FindOneAndUpdate makes the claim race-safe across concurrent
// worker runs — never find-then-update.
func (r *repository) claimQueuedNotification(ctx context.Context, providerRef string, now time.Time) (*StoredNotification, error) {
	var n StoredNotification
	err := r.notifications.FindOneAndUpdate(ctx,
		bson.D{{Key: "status", Value: domain.NotificationQueued}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: domain.NotificationSent},
			{Key: "sent_at", Value: now},
			{Key: "provider_ref", Value: providerRef},
		}}},
		options.FindOneAndUpdate().
			SetSort(bson.D{{Key: "queued_at", Value: 1}}).
			SetReturnDocument(options.After),
	).Decode(&n)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("claim queued notification: %w", err))
	}
	return &n, nil
}

// setNotificationMeta stamps the rendered-message meta block onto a claimed
// notification (visibility only — the claim has already succeeded).
func (r *repository) setNotificationMeta(ctx context.Context, notificationID primitive.ObjectID, meta map[string]any) error {
	_, err := r.notifications.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: notificationID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "meta", Value: meta}}}},
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("set notification meta: %w", err))
	}
	return nil
}

// findPartyByPhone returns the party owning a phone number, or NotFound.
func (r *repository) findPartyByPhone(ctx context.Context, phone string) (*domain.Party, error) {
	var party domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "phone", Value: phone}}).Decode(&party)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find party by phone: %w", err))
	}
	return &party, nil
}

// findPartyByID returns one party, or NotFound.
func (r *repository) findPartyByID(ctx context.Context, partyID primitive.ObjectID) (*domain.Party, error) {
	var party domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: partyID}}).Decode(&party)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find party: %w", err))
	}
	return &party, nil
}

// listActiveAssignmentsForParty returns a party's ACTIVE role assignments
// (bounded — a party holds at most a handful of roles).
func (r *repository) listActiveAssignmentsForParty(ctx context.Context, partyID primitive.ObjectID) ([]domain.RoleAssignment, error) {
	cur, err := r.assignments.Find(ctx,
		bson.D{
			{Key: "party_id", Value: partyID},
			{Key: "status", Value: domain.RoleAssignmentActive},
		},
		options.Find().SetLimit(100),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list assignments for party: %w", err))
	}
	assignments := []domain.RoleAssignment{}
	if err := cur.All(ctx, &assignments); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode assignments: %w", err))
	}
	return assignments, nil
}

// listActiveRoleHolders returns the ACTIVE assignments of one role inside one
// org unit (bounded — used to address supervisor safety alerts).
func (r *repository) listActiveRoleHolders(ctx context.Context, orgUnitID primitive.ObjectID, roleCode string) ([]domain.RoleAssignment, error) {
	cur, err := r.assignments.Find(ctx,
		bson.D{
			{Key: "org_unit_id", Value: orgUnitID},
			{Key: "role_code", Value: roleCode},
			{Key: "status", Value: domain.RoleAssignmentActive},
		},
		options.Find().SetLimit(50),
	)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list role holders: %w", err))
	}
	assignments := []domain.RoleAssignment{}
	if err := cur.All(ctx, &assignments); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode role holders: %w", err))
	}
	return assignments, nil
}

// subjectOrgUnit resolves a gate-blocked QC subject to the org unit that
// holds it (BMC lot → bmc_id, processing batch → plant_id). Both the subject
// _id and the reference field are ObjectIDs.
func (r *repository) subjectOrgUnit(ctx context.Context, subjectType string, subjectID primitive.ObjectID) (primitive.ObjectID, error) {
	var coll *mongo.Collection
	var field string
	switch subjectType {
	case domain.QCSubjectBMCLot:
		coll, field = r.bmcLots, "bmc_id"
	case domain.QCSubjectProcessingBatch:
		coll, field = r.batches, "plant_id"
	default:
		return primitive.NilObjectID, fmt.Errorf("gate-blocked subject type %q has no org resolution", subjectType)
	}

	var doc bson.M
	err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: subjectID}},
		options.FindOne().SetProjection(bson.D{{Key: field, Value: 1}}),
	).Decode(&doc)
	if err != nil {
		return primitive.NilObjectID, fmt.Errorf("resolve org of %s %s: %w", subjectType, subjectID.Hex(), err)
	}
	orgUnitID, _ := doc[field].(primitive.ObjectID)
	if orgUnitID.IsZero() {
		return primitive.NilObjectID, fmt.Errorf("%s %s carries no %s", subjectType, subjectID.Hex(), field)
	}
	return orgUnitID, nil
}

// ---- Admin: control-tower stats (§12) ----

// count is a small read-only CountDocuments helper that wraps failures as 500s
// with the collection/purpose named, so a broken counter is diagnosable.
func (r *repository) count(ctx context.Context, coll *mongo.Collection, filter bson.D, what string) (int64, error) {
	n, err := coll.CountDocuments(ctx, filter)
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("count %s: %w", what, err))
	}
	return n, nil
}

// adminStats assembles the control-tower snapshot from read-only counts over
// the shared collections. Today's pour litres is the one summed figure (a
// bounded $group over the day's pours); every other number is a CountDocuments.
func (r *repository) adminStats(ctx context.Context, dayKey string) (*AdminStats, error) {
	stats := &AdminStats{}
	var err error

	if stats.Parties, err = r.count(ctx, r.parties, bson.D{}, "parties"); err != nil {
		return nil, err
	}
	if stats.ActiveRoleAssignments, err = r.count(ctx, r.assignments,
		bson.D{{Key: "status", Value: domain.RoleAssignmentActive}}, "active role assignments"); err != nil {
		return nil, err
	}
	if stats.OrgUnits.DCS, err = r.count(ctx, r.orgUnits,
		bson.D{{Key: "type", Value: domain.OrgTypeDCS}}, "dcs org units"); err != nil {
		return nil, err
	}
	if stats.OrgUnits.BMC, err = r.count(ctx, r.orgUnits,
		bson.D{{Key: "type", Value: domain.OrgTypeBMC}}, "bmc org units"); err != nil {
		return nil, err
	}
	if stats.OrgUnits.Plant, err = r.count(ctx, r.orgUnits,
		bson.D{{Key: "type", Value: domain.OrgTypeProcessingPlant}}, "plant org units"); err != nil {
		return nil, err
	}
	if stats.TodayPours, err = r.count(ctx, r.pours,
		bson.D{{Key: "pour_date", Value: dayKey}}, "today pours"); err != nil {
		return nil, err
	}
	if stats.PendingKYC, err = r.count(ctx, r.kyc,
		bson.D{{Key: "status", Value: domain.KYCStatusPending}}, "pending kyc"); err != nil {
		return nil, err
	}
	// "Pending" invoices = issued but not yet paid (awaiting settlement or held).
	if stats.PendingInvoices, err = r.count(ctx, r.invoices,
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{
			domain.InvoiceStatusIssued, domain.InvoiceStatusSettlementPending, domain.InvoiceStatusHold,
		}}}}}, "pending invoices"); err != nil {
		return nil, err
	}
	// Blocked QC subjects = failed verdicts that were not superseded by a later
	// passing result (the safety-gate block still stands).
	if stats.BlockedQCSubjects, err = r.count(ctx, r.qcResults,
		bson.D{
			{Key: "overall_pass", Value: false},
			{Key: "superseded", Value: bson.D{{Key: "$ne", Value: true}}},
		}, "blocked qc subjects"); err != nil {
		return nil, err
	}

	litres, err := r.sumTodayLitres(ctx, dayKey)
	if err != nil {
		return nil, err
	}
	stats.TodayLitres = litres

	// Open batches = processing batches not yet terminal (neither COMPLETED nor
	// BLOCKED) — production in flight.
	if stats.OpenBatches, err = r.count(ctx, r.batches,
		bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{
			domain.BatchStatusCompleted, domain.BatchStatusBlocked,
		}}}}}, "open batches"); err != nil {
		return nil, err
	}
	// Failed batches (last 30 days) = BLOCKED batches created within the window.
	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	if stats.FailedBatches30d, err = r.count(ctx, r.batches,
		bson.D{
			{Key: "status", Value: domain.BatchStatusBlocked},
			{Key: "created_at", Value: bson.D{{Key: "$gte", Value: cutoff}}},
		}, "failed batches 30d"); err != nil {
		return nil, err
	}
	// Settled amount (last 30 days) = Σ total_amount of EXECUTED settlement
	// batches executed within the window.
	settled, err := r.sumSettledAmount(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	stats.SettledAmount30d = settled
	return stats, nil
}

// sumSettledAmount sums total_amount over settlement batches EXECUTED since
// cutoff — the control-tower "settled in the last 30 days" figure.
func (r *repository) sumSettledAmount(ctx context.Context, cutoff time.Time) (float64, error) {
	cur, err := r.settlements.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "status", Value: domain.SettlementStatusExecuted},
			{Key: "executed_at", Value: bson.D{{Key: "$gte", Value: cutoff}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "amount", Value: bson.D{{Key: "$sum", Value: "$total_amount"}}},
		}}},
	})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("sum settled amount: %w", err))
	}
	var rows []struct {
		Amount float64 `bson:"amount"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, httpx.Internal(fmt.Errorf("decode settled amount: %w", err))
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Amount, nil
}

// sumTodayLitres sums quantity_litres over the day's pours in a single bounded
// aggregation ($match on the indexed pour_date, then $group $sum).
func (r *repository) sumTodayLitres(ctx context.Context, dayKey string) (float64, error) {
	cur, err := r.pours.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "pour_date", Value: dayKey}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "litres", Value: bson.D{{Key: "$sum", Value: "$quantity_litres"}}},
		}}},
	})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("sum today litres: %w", err))
	}
	var rows []struct {
		Litres float64 `bson:"litres"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, httpx.Internal(fmt.Errorf("decode today litres: %w", err))
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Litres, nil
}

// ---- Admin: product master (§12) ----

// listProducts returns the full product master, ordered by SKU. The catalogue
// is small and admin-only, so it is returned unpaginated. When activeOnly is
// set, inactive rows are filtered out — the shape served to session callers on
// GET /products (a plant operator picking product options).
func (r *repository) listProducts(ctx context.Context, activeOnly bool) ([]domain.Product, error) {
	filter := bson.D{}
	if activeOnly {
		filter = bson.D{{Key: "active", Value: true}}
	}
	cur, err := r.products.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "sku", Value: 1}}))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list products: %w", err))
	}
	products := []domain.Product{}
	if err := cur.All(ctx, &products); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode products: %w", err))
	}
	return products, nil
}

// ---- Admin: governance settings (sachiv cap) ----

// storedSetting is one keyed governance value in the app_settings collection.
type storedSetting struct {
	Key      string `bson:"key"`
	IntValue int    `bson:"int_value"`
}

// getIntSetting reads a keyed int setting, returning (fallback, nil) when unset.
func (r *repository) getIntSetting(ctx context.Context, key string, fallback int) (int, error) {
	var doc storedSetting
	err := r.settings.FindOne(ctx, bson.D{{Key: "key", Value: key}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return fallback, nil
	}
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("get setting %s: %w", key, err))
	}
	return doc.IntValue, nil
}

// setIntSetting upserts a keyed int setting.
func (r *repository) setIntSetting(ctx context.Context, key string, value int) error {
	_, err := r.settings.UpdateOne(ctx,
		bson.D{{Key: "key", Value: key}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "key", Value: key}, {Key: "int_value", Value: value}}}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return httpx.Internal(fmt.Errorf("set setting %s: %w", key, err))
	}
	return nil
}

// maxActiveRoleHoldersPerOrg returns the largest number of ACTIVE holders of a
// role at any single org unit — the tightest per-DCS occupancy. The Sachiv cap
// is a PER-DCS ceiling, so this (not the federation-wide total) is what the cap
// is compared against: the busiest DCS is the constraint on how low the knob
// may be set. Returns 0 when the role is unheld anywhere.
func (r *repository) maxActiveRoleHoldersPerOrg(ctx context.Context, roleCode string) (int, error) {
	cur, err := r.assignments.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "role_code", Value: roleCode},
			{Key: "status", Value: domain.RoleAssignmentActive},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$org_unit_id"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "n", Value: -1}}}},
		{{Key: "$limit", Value: 1}},
	})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("max active %s per org: %w", roleCode, err))
	}
	defer cur.Close(ctx)
	var rows []struct {
		N int `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, httpx.Internal(fmt.Errorf("decode max active %s per org: %w", roleCode, err))
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].N, nil
}

// upsertProduct inserts or updates the product master row keyed by SKU and
// returns the persisted document. created_at is stamped only on insert.
func (r *repository) upsertProduct(ctx context.Context, sku string, set bson.D, now time.Time) (*domain.Product, error) {
	var product domain.Product
	err := r.products.FindOneAndUpdate(ctx,
		bson.D{{Key: "sku", Value: sku}},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}},
		},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&product)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("upsert product %s: %w", sku, err))
	}
	return &product, nil
}

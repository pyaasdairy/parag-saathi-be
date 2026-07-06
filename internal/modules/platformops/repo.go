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
type repository struct {
	auditLogs     *mongo.Collection
	notifications *mongo.Collection
	parties       *mongo.Collection
	assignments   *mongo.Collection
	bmcLots       *mongo.Collection
	batches       *mongo.Collection
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

// listAuditLogs returns a page of audit entries, newest first, plus the total
// matching count for pagination meta.
func (r *repository) listAuditLogs(ctx context.Context, f auditLogFilter, page httpx.Page) ([]audit.Entry, int64, error) {
	filter := auditFilter(f)

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

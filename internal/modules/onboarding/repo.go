package onboarding

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

// repository is the MongoDB gateway for the onboarding module. It is a
// legitimate cross-collection module: the approval saga finds-or-creates a
// Party, writes a VERIFIED KYCRecord, grants a RoleAssignment and queues a
// Notification — all reached through the shared mongodb.Coll* consts.
type repository struct {
	requests      *mongo.Collection
	parties       *mongo.Collection
	kycRecords    *mongo.Collection
	assignments   *mongo.Collection
	notifications *mongo.Collection
	settings      *mongo.Collection
}

// newRepository binds the repository to the module's collections.
func newRepository(db *mongo.Database) *repository {
	return &repository{
		requests:      db.Collection(mongodb.CollOnboarding),
		parties:       db.Collection(mongodb.CollParties),
		kycRecords:    db.Collection(mongodb.CollKYCRecords),
		assignments:   db.Collection(mongodb.CollRoleAssignments),
		notifications: db.Collection(mongodb.CollNotifications),
		settings:      db.Collection(mongodb.CollSettings),
	}
}

// --- Onboarding requests ---

// partyExistsByPhone reports whether a party already exists for the phone —
// the ALREADY_REGISTERED guard (one phone = one Party, §4.1).
func (r *repository) partyExistsByPhone(ctx context.Context, phone string) (bool, error) {
	n, err := r.parties.CountDocuments(ctx, bson.D{{Key: "phone", Value: phone}}, options.Count().SetLimit(1))
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("count parties by phone: %w", err))
	}
	return n > 0, nil
}

// pendingRequestExistsByPhone reports whether a PENDING onboarding request
// already exists for the phone — the ALREADY_PENDING guard.
func (r *repository) pendingRequestExistsByPhone(ctx context.Context, phone string) (bool, error) {
	n, err := r.requests.CountDocuments(ctx, bson.D{
		{Key: "phone", Value: phone},
		{Key: "status", Value: domain.OnboardingStatusPending},
	}, options.Count().SetLimit(1))
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("count pending requests by phone: %w", err))
	}
	return n > 0, nil
}

// insertRequest stores a fresh onboarding request.
func (r *repository) insertRequest(ctx context.Context, req domain.OnboardingRequest) error {
	if _, err := r.requests.InsertOne(ctx, req); err != nil {
		return httpx.Internal(fmt.Errorf("insert onboarding request: %w", err))
	}
	return nil
}

// findRequestByID loads one onboarding request.
func (r *repository) findRequestByID(ctx context.Context, id primitive.ObjectID) (*domain.OnboardingRequest, error) {
	var req domain.OnboardingRequest
	err := r.requests.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&req)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("onboarding request")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find onboarding request by id: %w", err))
	}
	return &req, nil
}

// listRequests pages onboarding requests newest-first, optionally filtered by
// status and/or submitter.
func (r *repository) listRequests(ctx context.Context, status string, submittedBy *primitive.ObjectID, page httpx.Page) ([]domain.OnboardingRequest, int64, error) {
	filter := bson.D{}
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: status})
	}
	if submittedBy != nil {
		filter = append(filter, bson.E{Key: "submitted_by", Value: *submittedBy})
	}
	total, err := r.requests.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count onboarding requests: %w", err))
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit)
	cur, err := r.requests.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list onboarding requests: %w", err))
	}
	out := make([]domain.OnboardingRequest, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode onboarding requests: %w", err))
	}
	return out, total, nil
}

// markRequestApproved flips a PENDING request to APPROVED with the reviewer
// stamp. Matching on status makes concurrent approvals lose cleanly (the loser
// sees NotFound and the service translates it to a 409) — this is the saga's
// idempotency guard, claimed BEFORE any party/kyc/assignment write.
func (r *repository) markRequestApproved(ctx context.Context, id, reviewedBy primitive.ObjectID, now time.Time) (*domain.OnboardingRequest, error) {
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "status", Value: domain.OnboardingStatusPending},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: domain.OnboardingStatusApproved},
		{Key: "reviewed_by", Value: reviewedBy},
		{Key: "reviewed_at", Value: now},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var req domain.OnboardingRequest
	err := r.requests.FindOneAndUpdate(ctx, filter, update, opts).Decode(&req)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("pending onboarding request")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("approve onboarding request: %w", err))
	}
	return &req, nil
}

// setRequestCreatedParty stamps the Party the saga created/found onto the
// approved request.
func (r *repository) setRequestCreatedParty(ctx context.Context, id, partyID primitive.ObjectID, now time.Time) (*domain.OnboardingRequest, error) {
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "created_party", Value: partyID},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var req domain.OnboardingRequest
	err := r.requests.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}}, update, opts).Decode(&req)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("onboarding request")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("stamp created party: %w", err))
	}
	return &req, nil
}

// markRequestRejected flips a PENDING request to REJECTED with the reviewer
// stamp and reason. Same concurrency semantics as markRequestApproved.
func (r *repository) markRequestRejected(ctx context.Context, id, reviewedBy primitive.ObjectID, reason string, now time.Time) (*domain.OnboardingRequest, error) {
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "status", Value: domain.OnboardingStatusPending},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: domain.OnboardingStatusRejected},
		{Key: "rejection_reason", Value: reason},
		{Key: "reviewed_by", Value: reviewedBy},
		{Key: "reviewed_at", Value: now},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var req domain.OnboardingRequest
	err := r.requests.FindOneAndUpdate(ctx, filter, update, opts).Decode(&req)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("pending onboarding request")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("reject onboarding request: %w", err))
	}
	return &req, nil
}

// --- Parties (saga) ---

// upsertPartyByPhone atomically finds or creates the party for a phone —
// race-safe under the unique phone index (one phone = one Party, §4.1). A
// freshly created party gets fullName and a MINIMAL tier; an existing party is
// returned untouched (its tier is raised separately, upward only).
func (r *repository) upsertPartyByPhone(ctx context.Context, phone, fullName string, now time.Time) (*domain.Party, error) {
	setOnInsert := bson.D{
		{Key: "_id", Value: primitive.NewObjectID()},
		{Key: "phone", Value: phone},
		{Key: "kyc_tier", Value: domain.KYCTierMinimal},
		{Key: "status", Value: domain.PartyStatusActive},
		{Key: "created_at", Value: now},
		{Key: "updated_at", Value: now},
	}
	if fullName != "" {
		setOnInsert = append(setOnInsert, bson.E{Key: "full_name", Value: fullName})
	}
	update := bson.D{{Key: "$setOnInsert", Value: setOnInsert}}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var p domain.Party
	if err := r.parties.FindOneAndUpdate(ctx, bson.D{{Key: "phone", Value: phone}}, update, opts).Decode(&p); err != nil {
		return nil, httpx.Internal(fmt.Errorf("upsert party by phone: %w", err))
	}
	return &p, nil
}

// updatePartyKYCTier sets the party's KYC tier.
func (r *repository) updatePartyKYCTier(ctx context.Context, id primitive.ObjectID, tier string, now time.Time) error {
	_, err := r.parties.UpdateByID(ctx, id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "kyc_tier", Value: tier},
		{Key: "updated_at", Value: now},
	}}})
	if err != nil {
		return httpx.Internal(fmt.Errorf("update party kyc tier: %w", err))
	}
	return nil
}

// --- KYC records (saga) ---

// insertKYCRecord stores a KYC record.
func (r *repository) insertKYCRecord(ctx context.Context, rec domain.KYCRecord) error {
	if _, err := r.kycRecords.InsertOne(ctx, rec); err != nil {
		return httpx.Internal(fmt.Errorf("insert kyc record: %w", err))
	}
	return nil
}

// --- Role assignments (saga) ---

// activeAssignmentExists reports whether the party already holds an ACTIVE
// assignment of roleCode in orgUnitID (grant idempotency guard, mirroring the
// identity module).
func (r *repository) activeAssignmentExists(ctx context.Context, partyID primitive.ObjectID, roleCode string, orgUnitID primitive.ObjectID) (bool, error) {
	filter := bson.D{
		{Key: "party_id", Value: partyID},
		{Key: "role_code", Value: roleCode},
		{Key: "org_unit_id", Value: orgUnitID},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	n, err := r.assignments.CountDocuments(ctx, filter, options.Count().SetLimit(1))
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("count active assignments: %w", err))
	}
	return n > 0, nil
}

// countActiveRoleHoldersInOrg counts ACTIVE assignments of one role at a single
// org unit — the per-DCS denominator the Sachiv governance cap is enforced
// against (mirrors the identity grant path so approval is never a side door).
func (r *repository) countActiveRoleHoldersInOrg(ctx context.Context, roleCode string, orgUnitID primitive.ObjectID) (int, error) {
	n, err := r.assignments.CountDocuments(ctx, bson.D{
		{Key: "role_code", Value: roleCode},
		{Key: "org_unit_id", Value: orgUnitID},
		{Key: "status", Value: domain.RoleAssignmentActive},
	})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("count active %s at org: %w", roleCode, err))
	}
	return int(n), nil
}

// getIntSetting reads a keyed int setting from app_settings, returning
// (fallback, nil) when the key is unset — same reader as identity's, so the
// approval saga honours the same governance knob.
func (r *repository) getIntSetting(ctx context.Context, key string, fallback int) (int, error) {
	var doc struct {
		IntValue int `bson:"int_value"`
	}
	err := r.settings.FindOne(ctx, bson.D{{Key: "key", Value: key}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return fallback, nil
	}
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("get setting %s: %w", key, err))
	}
	return doc.IntValue, nil
}

// insertAssignment stores a new role assignment.
func (r *repository) insertAssignment(ctx context.Context, ra domain.RoleAssignment) error {
	if _, err := r.assignments.InsertOne(ctx, ra); err != nil {
		return httpx.Internal(fmt.Errorf("insert assignment: %w", err))
	}
	return nil
}

// --- Notifications (outbox) ---

// insertNotification queues an outbox message for the platformops sender.
func (r *repository) insertNotification(ctx context.Context, n domain.Notification) error {
	if _, err := r.notifications.InsertOne(ctx, n); err != nil {
		return httpx.Internal(fmt.Errorf("insert notification: %w", err))
	}
	return nil
}

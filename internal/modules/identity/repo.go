package identity

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/google/uuid"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// otpChallenge is the persisted OTP challenge. Only the HMAC digest of the
// code is stored — a DB leak alone reveals no usable credential.
type otpChallenge struct {
	ID        string    `bson:"_id"`
	Phone     string    `bson:"phone"`
	CodeHash  string    `bson:"code_hash"`
	ExpiresAt time.Time `bson:"expires_at"`
	Attempts  int       `bson:"attempts"`
}

// refreshTokenDoc is the persisted refresh-token digest. The opaque token
// itself is only ever held by the client.
type refreshTokenDoc struct {
	ID        string    `bson:"_id"`
	TokenHash string    `bson:"token_hash"`
	PartyID   string    `bson:"party_id"`
	ExpiresAt time.Time `bson:"expires_at"`
	CreatedAt time.Time `bson:"created_at"`
}

// repository is the MongoDB gateway for the identity module. Pure data
// access — no business rules.
type repository struct {
	parties       *mongo.Collection
	challenges    *mongo.Collection
	tokens        *mongo.Collection
	assignments   *mongo.Collection
	kycRecords    *mongo.Collection
	consents      *mongo.Collection
	notifications *mongo.Collection
}

// newRepository binds the repository to the module's collections.
func newRepository(db *mongo.Database) *repository {
	return &repository{
		parties:       db.Collection(mongodb.CollParties),
		challenges:    db.Collection(mongodb.CollOTPChallenges),
		tokens:        db.Collection(mongodb.CollRefreshTokens),
		assignments:   db.Collection(mongodb.CollRoleAssignments),
		kycRecords:    db.Collection(mongodb.CollKYCRecords),
		consents:      db.Collection(mongodb.CollConsents),
		notifications: db.Collection(mongodb.CollNotifications),
	}
}

// --- OTP challenges ---

// insertOTPChallenge stores a fresh challenge.
func (r *repository) insertOTPChallenge(ctx context.Context, ch otpChallenge) error {
	if _, err := r.challenges.InsertOne(ctx, ch); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// latestOTPChallenge returns the most recent unexpired challenge for phone.
func (r *repository) latestOTPChallenge(ctx context.Context, phone string, now time.Time) (*otpChallenge, error) {
	filter := bson.D{
		{Key: "phone", Value: phone},
		{Key: "expires_at", Value: bson.D{{Key: "$gt", Value: now}}},
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "expires_at", Value: -1}})
	var ch otpChallenge
	err := r.challenges.FindOne(ctx, filter, opts).Decode(&ch)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("otp challenge")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &ch, nil
}

// incrementOTPAttempts counts one failed verification attempt.
func (r *repository) incrementOTPAttempts(ctx context.Context, id string) error {
	_, err := r.challenges.UpdateByID(ctx, id, bson.D{
		{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}},
	})
	if err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// deleteOTPChallenges removes every challenge for a phone (post-login burn).
func (r *repository) deleteOTPChallenges(ctx context.Context, phone string) error {
	if _, err := r.challenges.DeleteMany(ctx, bson.D{{Key: "phone", Value: phone}}); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// --- Notifications (outbox) ---

// insertNotification queues an outbox message for the platformops sender.
func (r *repository) insertNotification(ctx context.Context, n domain.Notification) error {
	if _, err := r.notifications.InsertOne(ctx, n); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// --- Parties ---

// findPartyByPhone loads a party by its unique phone number.
func (r *repository) findPartyByPhone(ctx context.Context, phone string) (*domain.Party, error) {
	var p domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "phone", Value: phone}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &p, nil
}

// findPartyByID loads a party by ID.
func (r *repository) findPartyByID(ctx context.Context, id string) (*domain.Party, error) {
	var p domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &p, nil
}

// upsertPartyByPhone atomically finds or creates the party for a phone —
// race-safe under the unique phone index (one phone = one Party, §4.1).
func (r *repository) upsertPartyByPhone(ctx context.Context, phone string, now time.Time) (*domain.Party, error) {
	update := bson.D{{Key: "$setOnInsert", Value: bson.D{
		{Key: "_id", Value: uuid.NewString()},
		{Key: "phone", Value: phone},
		{Key: "kyc_tier", Value: domain.KYCTierMinimal},
		{Key: "status", Value: domain.PartyStatusActive},
		{Key: "created_at", Value: now},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var p domain.Party
	if err := r.parties.FindOneAndUpdate(ctx, bson.D{{Key: "phone", Value: phone}}, update, opts).Decode(&p); err != nil {
		return nil, httpx.Internal(err)
	}
	return &p, nil
}

// updatePartyProfile sets the provided profile fields and returns the
// updated party.
func (r *repository) updatePartyProfile(ctx context.Context, id string, fullName, preferredLanguage *string, now time.Time) (*domain.Party, error) {
	set := bson.D{{Key: "updated_at", Value: now}}
	if fullName != nil {
		set = append(set, bson.E{Key: "full_name", Value: *fullName})
	}
	if preferredLanguage != nil {
		set = append(set, bson.E{Key: "preferred_language", Value: *preferredLanguage})
	}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var p domain.Party
	err := r.parties.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: set}}, opts).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &p, nil
}

// updatePartyKYCTier sets the party's KYC tier.
func (r *repository) updatePartyKYCTier(ctx context.Context, id, tier string, now time.Time) error {
	_, err := r.parties.UpdateByID(ctx, id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "kyc_tier", Value: tier},
		{Key: "updated_at", Value: now},
	}}})
	if err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// --- Refresh tokens ---

// insertRefreshToken stores a refresh-token digest.
func (r *repository) insertRefreshToken(ctx context.Context, doc refreshTokenDoc) error {
	if _, err := r.tokens.InsertOne(ctx, doc); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// findRefreshToken loads a refresh-token doc by digest.
func (r *repository) findRefreshToken(ctx context.Context, tokenHash string) (*refreshTokenDoc, error) {
	var doc refreshTokenDoc
	err := r.tokens.FindOne(ctx, bson.D{{Key: "token_hash", Value: tokenHash}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("refresh token")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &doc, nil
}

// deleteRefreshToken removes a token by digest and reports how many docs
// went away — 0 means a concurrent rotation already consumed it.
func (r *repository) deleteRefreshToken(ctx context.Context, tokenHash string) (int64, error) {
	res, err := r.tokens.DeleteOne(ctx, bson.D{{Key: "token_hash", Value: tokenHash}})
	if err != nil {
		return 0, httpx.Internal(err)
	}
	return res.DeletedCount, nil
}

// deleteRefreshTokenForParty removes a token only if it belongs to partyID
// (logout can never revoke someone else's session).
func (r *repository) deleteRefreshTokenForParty(ctx context.Context, tokenHash, partyID string) error {
	_, err := r.tokens.DeleteOne(ctx, bson.D{
		{Key: "token_hash", Value: tokenHash},
		{Key: "party_id", Value: partyID},
	})
	if err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// --- Role assignments ---

// listActiveAssignments returns a party's ACTIVE assignments (validity-window
// filtering happens in the service via UsableAt).
func (r *repository) listActiveAssignments(ctx context.Context, partyID string) ([]domain.RoleAssignment, error) {
	filter := bson.D{
		{Key: "party_id", Value: partyID},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	cur, err := r.assignments.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, httpx.Internal(err)
	}
	out := make([]domain.RoleAssignment, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(err)
	}
	return out, nil
}

// findAssignmentByID loads one role assignment.
func (r *repository) findAssignmentByID(ctx context.Context, id string) (*domain.RoleAssignment, error) {
	var ra domain.RoleAssignment
	err := r.assignments.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&ra)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("role assignment")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &ra, nil
}

// activeAssignmentExists reports whether the party already holds an ACTIVE
// assignment of roleCode in orgUnitID (grant idempotency guard).
func (r *repository) activeAssignmentExists(ctx context.Context, partyID, roleCode, orgUnitID string) (bool, error) {
	filter := bson.D{
		{Key: "party_id", Value: partyID},
		{Key: "role_code", Value: roleCode},
		{Key: "org_unit_id", Value: orgUnitID},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	n, err := r.assignments.CountDocuments(ctx, filter, options.Count().SetLimit(1))
	if err != nil {
		return false, httpx.Internal(err)
	}
	return n > 0, nil
}

// insertAssignment stores a new role assignment.
func (r *repository) insertAssignment(ctx context.Context, ra domain.RoleAssignment) error {
	if _, err := r.assignments.InsertOne(ctx, ra); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// revokeAssignment flips an ACTIVE assignment to REVOKED (the doc is never
// deleted — §4.1) and returns the updated record. Matching on status makes
// concurrent revokes lose cleanly.
func (r *repository) revokeAssignment(ctx context.Context, id, revokedBy string, at time.Time) (*domain.RoleAssignment, error) {
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: domain.RoleAssignmentRevoked},
		{Key: "revoked_at", Value: at},
		{Key: "revoked_by", Value: revokedBy},
	}}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var ra domain.RoleAssignment
	err := r.assignments.FindOneAndUpdate(ctx, filter, update, opts).Decode(&ra)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("active role assignment")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &ra, nil
}

// listAssignments pages assignments inside one org unit, optionally filtered
// by role code.
func (r *repository) listAssignments(ctx context.Context, orgUnitID, roleCode string, page httpx.Page) ([]domain.RoleAssignment, int64, error) {
	filter := bson.D{{Key: "org_unit_id", Value: orgUnitID}}
	if roleCode != "" {
		filter = append(filter, bson.E{Key: "role_code", Value: roleCode})
	}
	total, err := r.assignments.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit)
	cur, err := r.assignments.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	out := make([]domain.RoleAssignment, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return out, total, nil
}

// --- KYC records & consents ---

// insertKYCRecord stores a new KYC record.
func (r *repository) insertKYCRecord(ctx context.Context, rec domain.KYCRecord) error {
	if _, err := r.kycRecords.InsertOne(ctx, rec); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// latestKYCRecord returns the party's most recent KYC record.
func (r *repository) latestKYCRecord(ctx context.Context, partyID string) (*domain.KYCRecord, error) {
	opts := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})
	var rec domain.KYCRecord
	err := r.kycRecords.FindOne(ctx, bson.D{{Key: "party_id", Value: partyID}}, opts).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("kyc record")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &rec, nil
}

// updateKYCRecordBank sets the (masked) bank verification fields on an
// existing KYC record and returns the updated record.
func (r *repository) updateKYCRecordBank(ctx context.Context, id, accountMasked, ifsc string, nameMatch float64, now time.Time) (*domain.KYCRecord, error) {
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "bank_account_masked", Value: accountMasked},
		{Key: "bank_ifsc", Value: ifsc},
		{Key: "bank_verified", Value: true},
		{Key: "bank_name_match", Value: nameMatch},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var rec domain.KYCRecord
	err := r.kycRecords.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}}, update, opts).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("kyc record")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &rec, nil
}

// listKYCRecords pages the party's KYC records, newest first.
func (r *repository) listKYCRecords(ctx context.Context, partyID string, page httpx.Page) ([]domain.KYCRecord, int64, error) {
	filter := bson.D{{Key: "party_id", Value: partyID}}
	total, err := r.kycRecords.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit)
	cur, err := r.kycRecords.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	out := make([]domain.KYCRecord, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return out, total, nil
}

// insertConsent stores a DPDP consent artefact.
func (r *repository) insertConsent(ctx context.Context, c domain.Consent) error {
	if _, err := r.consents.InsertOne(ctx, c); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

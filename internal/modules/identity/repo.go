package identity

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

// otpChallenge is the persisted OTP challenge. Only the HMAC digest of the
// code is stored — a DB leak alone reveals no usable credential. Challenges
// are keyed by phone (string) — the party may not exist yet at login time.
type otpChallenge struct {
	ID        string    `bson:"_id"`
	Phone     string    `bson:"phone"`
	CodeHash  string    `bson:"code_hash"`
	ExpiresAt time.Time `bson:"expires_at"`
	Attempts  int       `bson:"attempts"`
}

// refreshTokenDoc is the persisted refresh-token digest. The opaque token
// itself is only ever held by the client. party_id is an ObjectID reference.
type refreshTokenDoc struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	TokenHash string             `bson:"token_hash"`
	PartyID   primitive.ObjectID `bson:"party_id"`
	ExpiresAt time.Time          `bson:"expires_at"`
	CreatedAt time.Time          `bson:"created_at"`
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
	settings      *mongo.Collection
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
		settings:      db.Collection(mongodb.CollSettings),
	}
}

// --- OTP challenges ---

// insertOTPChallenge stores a fresh challenge.
func (r *repository) insertOTPChallenge(ctx context.Context, ch otpChallenge) error {
	if _, err := r.challenges.InsertOne(ctx, ch); err != nil {
		return httpx.Internal(fmt.Errorf("insert otp challenge: %w", err))
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
		return nil, httpx.Internal(fmt.Errorf("find otp challenge: %w", err))
	}
	return &ch, nil
}

// incrementOTPAttempts counts one failed verification attempt.
func (r *repository) incrementOTPAttempts(ctx context.Context, id string) error {
	_, err := r.challenges.UpdateByID(ctx, id, bson.D{
		{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}},
	})
	if err != nil {
		return httpx.Internal(fmt.Errorf("increment otp attempts: %w", err))
	}
	return nil
}

// deleteOTPChallenges removes every challenge for a phone (post-login burn).
func (r *repository) deleteOTPChallenges(ctx context.Context, phone string) error {
	if _, err := r.challenges.DeleteMany(ctx, bson.D{{Key: "phone", Value: phone}}); err != nil {
		return httpx.Internal(fmt.Errorf("delete otp challenges: %w", err))
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

// --- Parties ---

// findPartyByPhone loads a party by its unique phone number (business key —
// display/login lookups only, never joins).
func (r *repository) findPartyByPhone(ctx context.Context, phone string) (*domain.Party, error) {
	var p domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "phone", Value: phone}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find party by phone: %w", err))
	}
	return &p, nil
}

// findPartyByID loads a party by ObjectID.
func (r *repository) findPartyByID(ctx context.Context, id primitive.ObjectID) (*domain.Party, error) {
	var p domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find party by id: %w", err))
	}
	return &p, nil
}

// findPartiesByIDs loads a batch of parties keyed by ObjectID (list
// enrichment; missing parties are simply absent from the map).
func (r *repository) findPartiesByIDs(ctx context.Context, ids []primitive.ObjectID) (map[primitive.ObjectID]domain.Party, error) {
	out := make(map[primitive.ObjectID]domain.Party, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cur, err := r.parties.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find parties by ids: %w", err))
	}
	var parties []domain.Party
	if err := cur.All(ctx, &parties); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode parties by ids: %w", err))
	}
	for _, p := range parties {
		out[p.ID] = p
	}
	return out, nil
}

// upsertPartyByPhone atomically finds or creates the party for a phone —
// race-safe under the unique phone index (one phone = one Party, §4.1).
func (r *repository) upsertPartyByPhone(ctx context.Context, phone string, now time.Time) (*domain.Party, error) {
	update := bson.D{{Key: "$setOnInsert", Value: bson.D{
		{Key: "_id", Value: primitive.NewObjectID()},
		{Key: "phone", Value: phone},
		{Key: "kyc_tier", Value: domain.KYCTierMinimal},
		{Key: "status", Value: domain.PartyStatusActive},
		{Key: "created_at", Value: now},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var p domain.Party
	if err := r.parties.FindOneAndUpdate(ctx, bson.D{{Key: "phone", Value: phone}}, update, opts).Decode(&p); err != nil {
		return nil, httpx.Internal(fmt.Errorf("upsert party by phone: %w", err))
	}
	return &p, nil
}

// updatePartyProfile sets the provided profile fields and returns the
// updated party.
func (r *repository) updatePartyProfile(ctx context.Context, id primitive.ObjectID, fullName, preferredLanguage *string, publicConsent *bool, profilePhotoURL *string, now time.Time) (*domain.Party, error) {
	set := bson.D{{Key: "updated_at", Value: now}}
	if fullName != nil {
		set = append(set, bson.E{Key: "full_name", Value: *fullName})
	}
	if preferredLanguage != nil {
		set = append(set, bson.E{Key: "preferred_language", Value: *preferredLanguage})
	}
	if publicConsent != nil {
		set = append(set, bson.E{Key: "public_consent", Value: *publicConsent})
	}
	if profilePhotoURL != nil {
		set = append(set, bson.E{Key: "profile_photo_url", Value: *profilePhotoURL})
	}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var p domain.Party
	err := r.parties.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: set}}, opts).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("party")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("update party profile: %w", err))
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

// --- Refresh tokens ---

// insertRefreshToken stores a refresh-token digest.
func (r *repository) insertRefreshToken(ctx context.Context, doc refreshTokenDoc) error {
	if _, err := r.tokens.InsertOne(ctx, doc); err != nil {
		return httpx.Internal(fmt.Errorf("insert refresh token: %w", err))
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
		return nil, httpx.Internal(fmt.Errorf("find refresh token: %w", err))
	}
	return &doc, nil
}

// deleteRefreshToken removes a token by digest and reports how many docs
// went away — 0 means a concurrent rotation already consumed it.
func (r *repository) deleteRefreshToken(ctx context.Context, tokenHash string) (int64, error) {
	res, err := r.tokens.DeleteOne(ctx, bson.D{{Key: "token_hash", Value: tokenHash}})
	if err != nil {
		return 0, httpx.Internal(fmt.Errorf("delete refresh token: %w", err))
	}
	return res.DeletedCount, nil
}

// deleteRefreshTokenForParty removes a token only if it belongs to partyID
// (logout can never revoke someone else's session).
func (r *repository) deleteRefreshTokenForParty(ctx context.Context, tokenHash string, partyID primitive.ObjectID) error {
	_, err := r.tokens.DeleteOne(ctx, bson.D{
		{Key: "token_hash", Value: tokenHash},
		{Key: "party_id", Value: partyID},
	})
	if err != nil {
		return httpx.Internal(fmt.Errorf("delete refresh token for party: %w", err))
	}
	return nil
}

// --- Role assignments ---

// listActiveAssignments returns a party's ACTIVE assignments (validity-window
// filtering happens in the service via UsableAt).
func (r *repository) listActiveAssignments(ctx context.Context, partyID primitive.ObjectID) ([]domain.RoleAssignment, error) {
	filter := bson.D{
		{Key: "party_id", Value: partyID},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	cur, err := r.assignments.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list active assignments: %w", err))
	}
	out := make([]domain.RoleAssignment, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode active assignments: %w", err))
	}
	return out, nil
}

// findPartiesByRoles returns the distinct parties holding an ACTIVE assignment
// for any of the given role codes — used to notify KYC reviewers that a
// verification is pending. Capped so a broad role never floods the outbox.
func (r *repository) findPartiesByRoles(ctx context.Context, roleCodes []string, limit int64) ([]domain.Party, error) {
	assignFilter := bson.D{
		{Key: "role_code", Value: bson.D{{Key: "$in", Value: roleCodes}}},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	cur, err := r.assignments.Find(ctx, assignFilter,
		options.Find().SetProjection(bson.D{{Key: "party_id", Value: 1}}).SetLimit(limit))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find reviewer assignments: %w", err))
	}
	var rows []struct {
		PartyID primitive.ObjectID `bson:"party_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode reviewer assignments: %w", err))
	}
	seen := make(map[primitive.ObjectID]struct{}, len(rows))
	ids := make([]primitive.ObjectID, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.PartyID]; ok {
			continue
		}
		seen[row.PartyID] = struct{}{}
		ids = append(ids, row.PartyID)
	}
	byID, err := r.findPartiesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Party, 0, len(byID))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// roleHolderRow is one party holding a role, paired with the org unit that
// assignment is scoped to — enough to render the FE listSachivs picker (which
// needs {party, org_unit_id, org_name}) without a second round-trip.
type roleHolderRow struct {
	AssignmentID primitive.ObjectID
	OrgUnitID    primitive.ObjectID
	Party        domain.Party
}

// listRoleHoldersInOrg pages the distinct parties holding an ACTIVE assignment
// of roleCode inside orgUnitID, each paired with the org unit of their (newest)
// matching assignment — backing the reviewer's "sachivs in this DCS" picker.
// total is the count of distinct matching parties.
func (r *repository) listRoleHoldersInOrg(ctx context.Context, roleCode string, orgUnitIDs []primitive.ObjectID, page httpx.Page) ([]roleHolderRow, int64, error) {
	assignFilter := bson.D{
		{Key: "role_code", Value: roleCode},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	// An empty org set means "federation-wide" (already authorised in the
	// service) — omit the org filter entirely. Otherwise match any org in the
	// caller's scoped subtree (the node itself plus its descendants).
	switch len(orgUnitIDs) {
	case 0:
		// federation-wide
	case 1:
		assignFilter = append(assignFilter, bson.E{Key: "org_unit_id", Value: orgUnitIDs[0]})
	default:
		assignFilter = append(assignFilter, bson.E{Key: "org_unit_id", Value: bson.D{{Key: "$in", Value: orgUnitIDs}}})
	}
	// Newest assignment first so the picker surfaces recent grants at the top;
	// dedupe preserves first-seen order (a party's newest matching assignment
	// supplies the org shown).
	cur, err := r.assignments.Find(ctx, assignFilter,
		options.Find().
			SetProjection(bson.D{{Key: "_id", Value: 1}, {Key: "party_id", Value: 1}, {Key: "org_unit_id", Value: 1}}).
			SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("find role assignments by org: %w", err))
	}
	var rows []struct {
		ID        primitive.ObjectID `bson:"_id"`
		PartyID   primitive.ObjectID `bson:"party_id"`
		OrgUnitID primitive.ObjectID `bson:"org_unit_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode role assignments by org: %w", err))
	}
	seen := make(map[primitive.ObjectID]roleHolderRow, len(rows))
	ordered := make([]primitive.ObjectID, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.PartyID]; ok {
			continue
		}
		seen[row.PartyID] = roleHolderRow{AssignmentID: row.ID, OrgUnitID: row.OrgUnitID}
		ordered = append(ordered, row.PartyID)
	}
	total := int64(len(ordered))

	// Page over the distinct party ids.
	lo := page.Offset
	if lo > total {
		lo = total
	}
	hi := lo + page.Limit
	if hi > total {
		hi = total
	}
	pageIDs := ordered[lo:hi]

	byID, err := r.findPartiesByIDs(ctx, pageIDs)
	if err != nil {
		return nil, 0, err
	}
	out := make([]roleHolderRow, 0, len(pageIDs))
	for _, id := range pageIDs {
		holder := seen[id]
		if p, ok := byID[id]; ok {
			holder.Party = p
		}
		out = append(out, holder)
	}
	return out, total, nil
}

// findAssignmentByID loads one role assignment.
func (r *repository) findAssignmentByID(ctx context.Context, id primitive.ObjectID) (*domain.RoleAssignment, error) {
	var ra domain.RoleAssignment
	err := r.assignments.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&ra)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("role assignment")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find assignment by id: %w", err))
	}
	return &ra, nil
}

// activeAssignmentExists reports whether the party already holds an ACTIVE
// assignment of roleCode in orgUnitID (grant idempotency guard).
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
// against at grant time.
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
// (fallback, nil) when the key is unset. Mirrors platformops' reader so the
// grant path can honour the same governance knob without importing that module.
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

// listActiveAssignmentsForRole returns every ACTIVE assignment of one role at
// a single org unit — the holder set the replace-holder swap operates on.
func (r *repository) listActiveAssignmentsForRole(ctx context.Context, roleCode string, orgUnitID primitive.ObjectID) ([]domain.RoleAssignment, error) {
	filter := bson.D{
		{Key: "role_code", Value: roleCode},
		{Key: "org_unit_id", Value: orgUnitID},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}
	cur, err := r.assignments.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list active %s at org: %w", roleCode, err))
	}
	out := make([]domain.RoleAssignment, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode active %s at org: %w", roleCode, err))
	}
	return out, nil
}

// insertAssignment stores a new role assignment.
func (r *repository) insertAssignment(ctx context.Context, ra domain.RoleAssignment) error {
	if _, err := r.assignments.InsertOne(ctx, ra); err != nil {
		return httpx.Internal(fmt.Errorf("insert assignment: %w", err))
	}
	return nil
}

// revokeAssignment flips an ACTIVE assignment to REVOKED (the doc is never
// deleted — §4.1) and returns the updated record. Matching on status makes
// concurrent revokes lose cleanly.
func (r *repository) revokeAssignment(ctx context.Context, id, revokedBy primitive.ObjectID, at time.Time) (*domain.RoleAssignment, error) {
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
		return nil, httpx.Internal(fmt.Errorf("revoke assignment: %w", err))
	}
	return &ra, nil
}

// listAssignments pages assignments inside one org unit, optionally filtered
// by role code.
func (r *repository) listAssignments(ctx context.Context, orgUnitID primitive.ObjectID, roleCode string, page httpx.Page) ([]domain.RoleAssignment, int64, error) {
	filter := bson.D{{Key: "org_unit_id", Value: orgUnitID}}
	if roleCode != "" {
		filter = append(filter, bson.E{Key: "role_code", Value: roleCode})
	}
	total, err := r.assignments.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count assignments: %w", err))
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit)
	cur, err := r.assignments.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list assignments: %w", err))
	}
	out := make([]domain.RoleAssignment, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode assignments: %w", err))
	}
	return out, total, nil
}

// --- KYC records & consents ---

// insertKYCRecord stores a new KYC record.
func (r *repository) insertKYCRecord(ctx context.Context, rec domain.KYCRecord) error {
	if _, err := r.kycRecords.InsertOne(ctx, rec); err != nil {
		return httpx.Internal(fmt.Errorf("insert kyc record: %w", err))
	}
	return nil
}

// findKYCRecordByID loads one KYC record.
func (r *repository) findKYCRecordByID(ctx context.Context, id primitive.ObjectID) (*domain.KYCRecord, error) {
	var rec domain.KYCRecord
	err := r.kycRecords.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("kyc record")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find kyc record by id: %w", err))
	}
	return &rec, nil
}

// latestKYCRecord returns the party's most recent KYC record (any status).
func (r *repository) latestKYCRecord(ctx context.Context, partyID primitive.ObjectID) (*domain.KYCRecord, error) {
	opts := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})
	var rec domain.KYCRecord
	err := r.kycRecords.FindOne(ctx, bson.D{{Key: "party_id", Value: partyID}}, opts).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("kyc record")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find latest kyc record: %w", err))
	}
	return &rec, nil
}

// findPendingKYCRecord returns the party's most recent PENDING record for one
// requested tier (submit idempotency guard).
func (r *repository) findPendingKYCRecord(ctx context.Context, partyID primitive.ObjectID, requestedTier string) (*domain.KYCRecord, error) {
	filter := bson.D{
		{Key: "party_id", Value: partyID},
		{Key: "requested_tier", Value: requestedTier},
		{Key: "status", Value: domain.KYCStatusPending},
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})
	var rec domain.KYCRecord
	err := r.kycRecords.FindOne(ctx, filter, opts).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("pending kyc record")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find pending kyc record: %w", err))
	}
	return &rec, nil
}

// listPendingKYC pages all PENDING KYC records, newest first, for the review
// console.
func (r *repository) listPendingKYC(ctx context.Context, page httpx.Page) ([]domain.KYCRecord, int64, error) {
	filter := bson.D{{Key: "status", Value: domain.KYCStatusPending}}
	total, err := r.kycRecords.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count pending kyc records: %w", err))
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit)
	cur, err := r.kycRecords.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list pending kyc records: %w", err))
	}
	out := make([]domain.KYCRecord, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode pending kyc records: %w", err))
	}
	return out, total, nil
}

// approveKYCRecord flips a PENDING record to VERIFIED with the reviewer
// stamp. Matching on status makes concurrent reviews lose cleanly (the loser
// sees NotFound and the service translates it to a 409).
func (r *repository) approveKYCRecord(ctx context.Context, id, reviewedBy primitive.ObjectID, reviewerRole string, now time.Time) (*domain.KYCRecord, error) {
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "status", Value: domain.KYCStatusPending},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: domain.KYCStatusVerified},
		{Key: "reviewed_by", Value: reviewedBy},
		{Key: "reviewed_by_role", Value: reviewerRole},
		{Key: "reviewed_at", Value: now},
		{Key: "verified_at", Value: now},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var rec domain.KYCRecord
	err := r.kycRecords.FindOneAndUpdate(ctx, filter, update, opts).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("pending kyc record")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("approve kyc record: %w", err))
	}
	return &rec, nil
}

// rejectKYCRecord flips a PENDING record to REJECTED with the reviewer stamp
// and reason. Same concurrency semantics as approveKYCRecord.
func (r *repository) rejectKYCRecord(ctx context.Context, id, reviewedBy primitive.ObjectID, reviewerRole, reason string, now time.Time) (*domain.KYCRecord, error) {
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "status", Value: domain.KYCStatusPending},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: domain.KYCStatusRejected},
		{Key: "rejection_reason", Value: reason},
		{Key: "reviewed_by", Value: reviewedBy},
		{Key: "reviewed_by_role", Value: reviewerRole},
		{Key: "reviewed_at", Value: now},
		{Key: "updated_at", Value: now},
	}}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var rec domain.KYCRecord
	err := r.kycRecords.FindOneAndUpdate(ctx, filter, update, opts).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("pending kyc record")
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("reject kyc record: %w", err))
	}
	return &rec, nil
}

// updateKYCRecordBank sets the (masked) bank verification fields on an
// existing KYC record and returns the updated record.
func (r *repository) updateKYCRecordBank(ctx context.Context, id primitive.ObjectID, accountMasked, ifsc string, nameMatch float64, now time.Time) (*domain.KYCRecord, error) {
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
		return nil, httpx.Internal(fmt.Errorf("update kyc record bank: %w", err))
	}
	return &rec, nil
}

// listKYCRecords pages the party's KYC records, newest first.
func (r *repository) listKYCRecords(ctx context.Context, partyID primitive.ObjectID, page httpx.Page) ([]domain.KYCRecord, int64, error) {
	filter := bson.D{{Key: "party_id", Value: partyID}}
	total, err := r.kycRecords.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("count kyc records: %w", err))
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).
		SetLimit(page.Limit)
	cur, err := r.kycRecords.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("list kyc records: %w", err))
	}
	out := make([]domain.KYCRecord, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode kyc records: %w", err))
	}
	return out, total, nil
}

// insertConsent stores a DPDP consent artefact.
func (r *repository) insertConsent(ctx context.Context, c domain.Consent) error {
	if _, err := r.consents.InsertOne(ctx, c); err != nil {
		return httpx.Internal(fmt.Errorf("insert consent: %w", err))
	}
	return nil
}

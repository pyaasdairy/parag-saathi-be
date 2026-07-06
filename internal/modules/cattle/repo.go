package cattle

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// repository is the module's only MongoDB touchpoint. It maps driver errors
// to httpx errors (mongo.ErrNoDocuments → NotFound, duplicate key → Conflict)
// so the service layer stays driver-free.
type repository struct {
	animals   *mongo.Collection
	health    *mongo.Collection
	mvuCases  *mongo.Collection
	education *mongo.Collection
}

// newRepository binds the repository to the module's collections.
func newRepository(db *mongo.Database) *repository {
	return &repository{
		animals:   db.Collection(mongodb.CollAnimals),
		health:    db.Collection(mongodb.CollHealthEvents),
		mvuCases:  db.Collection(mongodb.CollMVUCases),
		education: db.Collection(mongodb.CollEducation),
	}
}

// InsertAnimal persists a new animal. The unique index on pashu_aadhaar
// (mongodb.EnsureIndexes) turns a duplicate registration into ANIMAL_EXISTS.
func (r *repository) InsertAnimal(ctx context.Context, a *domain.Animal) error {
	if _, err := r.animals.InsertOne(ctx, a); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return httpx.Conflict("ANIMAL_EXISTS", "an animal with this pashu_aadhaar is already registered")
		}
		return httpx.Internal(err)
	}
	return nil
}

// FindAnimalByID loads one animal or returns NotFound.
func (r *repository) FindAnimalByID(ctx context.Context, id string) (*domain.Animal, error) {
	var a domain.Animal
	err := r.animals.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&a)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("animal")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &a, nil
}

// ListAnimals returns a page of animals filtered by owner and/or DCS,
// newest first, plus the total match count.
func (r *repository) ListAnimals(ctx context.Context, ownerPartyID, dcsID string, page httpx.Page) ([]domain.Animal, int64, error) {
	filter := bson.D{}
	if ownerPartyID != "" {
		filter = append(filter, bson.E{Key: "owner_party_id", Value: ownerPartyID})
	}
	if dcsID != "" {
		filter = append(filter, bson.E{Key: "dcs_id", Value: dcsID})
	}
	total, err := r.animals.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	cur, err := r.animals.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	out := []domain.Animal{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return out, total, nil
}

// InsertHealthEvent appends one entry to an animal's health history.
func (r *repository) InsertHealthEvent(ctx context.Context, e *domain.HealthEvent) error {
	if _, err := r.health.InsertOne(ctx, e); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// ListHealthEventsByAnimal returns a page of an animal's health history,
// most recent occurrence first, plus the total count.
func (r *repository) ListHealthEventsByAnimal(ctx context.Context, animalID string, page httpx.Page) ([]domain.HealthEvent, int64, error) {
	filter := bson.D{{Key: "animal_id", Value: animalID}}
	total, err := r.health.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	cur, err := r.health.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "occurred_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	out := []domain.HealthEvent{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return out, total, nil
}

// MarkPendingHealthEventsSynced flips every PENDING event of one animal to
// SYNCED with the given Bharat Pashudhan reference, returning how many synced.
func (r *repository) MarkPendingHealthEventsSynced(ctx context.Context, animalID, bpSyncRef string) (int64, error) {
	res, err := r.health.UpdateMany(ctx,
		bson.D{
			{Key: "animal_id", Value: animalID},
			{Key: "bp_sync_status", Value: domain.BPSyncPending},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "bp_sync_status", Value: domain.BPSyncSynced},
			{Key: "bp_sync_ref", Value: bpSyncRef},
		}}})
	if err != nil {
		return 0, httpx.Internal(err)
	}
	return res.ModifiedCount, nil
}

// InsertMVUCase persists a new MVU case.
func (r *repository) InsertMVUCase(ctx context.Context, c *domain.MVUCase) error {
	if _, err := r.mvuCases.InsertOne(ctx, c); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// FindMVUCaseByID loads one MVU case or returns NotFound.
func (r *repository) FindMVUCaseByID(ctx context.Context, id string) (*domain.MVUCase, error) {
	var c domain.MVUCase
	err := r.mvuCases.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("MVU case")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &c, nil
}

// DispatchMVUCase moves a case REQUESTED → DISPATCHED, recording the vet
// and/or driver. The status guard in the filter is the optimistic lock (no
// transactions on a standalone server): a false return means the case was
// not in REQUESTED state — e.g. a concurrent dispatch won the race.
func (r *repository) DispatchMVUCase(ctx context.Context, id, vetPartyID, driverPartyID string) (bool, error) {
	set := bson.D{{Key: "status", Value: domain.MVUCaseDispatched}}
	if vetPartyID != "" {
		set = append(set, bson.E{Key: "vet_party_id", Value: vetPartyID})
	}
	if driverPartyID != "" {
		set = append(set, bson.E{Key: "driver_party_id", Value: driverPartyID})
	}
	res, err := r.mvuCases.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: domain.MVUCaseRequested}},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, httpx.Internal(err)
	}
	return res.MatchedCount == 1, nil
}

// CloseMVUCase moves a DISPATCHED (or ARRIVED) case to CLOSED with the visit
// log. Same optimistic status-guard pattern as DispatchMVUCase.
func (r *repository) CloseMVUCase(ctx context.Context, id, visitNotes string, healthEventIDs []string, closedAt time.Time) (bool, error) {
	set := bson.D{
		{Key: "status", Value: domain.MVUCaseClosed},
		{Key: "visit_notes", Value: visitNotes},
		{Key: "closed_at", Value: closedAt},
	}
	if len(healthEventIDs) > 0 {
		set = append(set, bson.E{Key: "health_event_ids", Value: healthEventIDs})
	}
	res, err := r.mvuCases.UpdateOne(ctx,
		bson.D{
			{Key: "_id", Value: id},
			{Key: "status", Value: bson.D{{Key: "$in", Value: []string{
				domain.MVUCaseDispatched, domain.MVUCaseArrived,
			}}}},
		},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, httpx.Internal(err)
	}
	return res.MatchedCount == 1, nil
}

// ListMVUCases returns a page of cases filtered by DCS and/or status,
// most recently requested first, plus the total match count.
func (r *repository) ListMVUCases(ctx context.Context, dcsID, status string, page httpx.Page) ([]domain.MVUCase, int64, error) {
	filter := bson.D{}
	if dcsID != "" {
		filter = append(filter, bson.E{Key: "dcs_id", Value: dcsID})
	}
	if status != "" {
		filter = append(filter, bson.E{Key: "status", Value: status})
	}
	total, err := r.mvuCases.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	cur, err := r.mvuCases.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "requested_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	out := []domain.MVUCase{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return out, total, nil
}

// InsertEducation persists a new education-hub item.
func (r *repository) InsertEducation(ctx context.Context, e *domain.EducationContent) error {
	if _, err := r.education.InsertOne(ctx, e); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// ListPublishedEducation returns a page of published content filtered by
// topic and/or language, newest first, plus the total match count.
func (r *repository) ListPublishedEducation(ctx context.Context, topic, language string, page httpx.Page) ([]domain.EducationContent, int64, error) {
	filter := bson.D{{Key: "published", Value: true}}
	if topic != "" {
		filter = append(filter, bson.E{Key: "topic", Value: topic})
	}
	if language != "" {
		filter = append(filter, bson.E{Key: "language", Value: language})
	}
	total, err := r.education.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	cur, err := r.education.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(page.Offset).SetLimit(page.Limit))
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	out := []domain.EducationContent{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return out, total, nil
}

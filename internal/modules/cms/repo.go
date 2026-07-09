package cms

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// versionCounterKey names the shared counter that mints the monotonic CMS
// content version cursor (blueprint §6.1 delta pull).
const versionCounterKey = "cms_content_version"

// repository is the CMS module's only MongoDB access point: it reads and
// writes the cms_content collection and increments the shared counter that
// mints content versions.
//
// Error contract: missing documents surface as httpx.NotFound (a business
// outcome); every other failure is wrapped with the failing operation so the
// service can log it before mapping to a 500.
type repository struct {
	content  *mongo.Collection
	counters *mongo.Collection
}

func newRepository(db *mongo.Database) *repository {
	return &repository{
		content:  db.Collection(mongodb.CollCMSContent),
		counters: db.Collection(mongodb.CollCounters),
	}
}

// nextVersion atomically increments and returns the global CMS version
// counter — the transaction-free way to mint a monotonic cursor shared by
// every content item so the field app's delta pull is totally ordered.
func (r *repository) nextVersion(ctx context.Context) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := r.counters.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: versionCounterKey}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(1)}}}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return 0, fmt.Errorf("increment cms version counter: %w", err)
	}
	return doc.Seq, nil
}

// insert stores a new content item (its _id and version are pre-assigned by
// the service).
func (r *repository) insert(ctx context.Context, c *domain.CMSContent) error {
	if _, err := r.content.InsertOne(ctx, c); err != nil {
		return fmt.Errorf("insert cms content: %w", err)
	}
	return nil
}

// getByID fetches one content item by ObjectID.
func (r *repository) getByID(ctx context.Context, id primitive.ObjectID) (*domain.CMSContent, error) {
	var c domain.CMSContent
	err := r.content.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("content " + id.Hex())
	}
	if err != nil {
		return nil, fmt.Errorf("get cms content %s: %w", id.Hex(), err)
	}
	return &c, nil
}

// update applies a $set patch and returns the updated document.
func (r *repository) update(ctx context.Context, id primitive.ObjectID, set bson.D) (*domain.CMSContent, error) {
	var c domain.CMSContent
	err := r.content.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: set}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("content " + id.Hex())
	}
	if err != nil {
		return nil, fmt.Errorf("update cms content %s: %w", id.Hex(), err)
	}
	return &c, nil
}

// find runs an arbitrary filter with the given options and decodes the result.
func (r *repository) find(ctx context.Context, filter bson.D, opts *options.FindOptions) ([]domain.CMSContent, error) {
	cur, err := r.content.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("find cms content: %w", err)
	}
	items := []domain.CMSContent{}
	if err := cur.All(ctx, &items); err != nil {
		return nil, fmt.Errorf("decode cms content: %w", err)
	}
	return items, nil
}

// listDelta returns published items matching filter with version ascending,
// capped at limit — the versioned delta batch (blueprint §6.1).
func (r *repository) listDelta(ctx context.Context, filter bson.D, limit int64) ([]domain.CMSContent, error) {
	return r.find(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "version", Value: 1}}).
			SetLimit(limit))
}

// listHelpline returns published items matching filter ordered for display
// (order asc, then version asc as a stable tiebreak) — the Get-Help screen.
func (r *repository) listHelpline(ctx context.Context, filter bson.D) ([]domain.CMSContent, error) {
	return r.find(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "order", Value: 1}, {Key: "version", Value: 1}}))
}

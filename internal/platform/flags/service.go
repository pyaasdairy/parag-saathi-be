// Package flags implements capability gating (blueprint principle #6):
// future-scheme features ship dormant behind flags — e.g. collar telemetry —
// so a new government scheme is a flag flip, not a schema migration.
package flags

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collection = "feature_flags"

// Well-known flag keys.
const (
	FlagCollarTelemetry  = "collar_telemetry_enabled" // dormant until a collar scheme lands (§9)
	FlagPhotoOCR         = "photo_ocr_enabled"        // legacy-analyzer bridge (§8.2)
	FlagONDC             = "ondc_enabled"             // optional commerce network (§11)
	FlagConsumerCommerce = "consumer_commerce_enabled"
)

// Flag is the stored document. `_id` is a generated ObjectID; Key is the
// unique business identifier all lookups use.
type Flag struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Key         string             `bson:"key"           json:"key"`
	Enabled     bool               `bson:"enabled"       json:"enabled"`
	Description string             `bson:"description,omitempty" json:"description,omitempty"`
	UpdatedBy   string             `bson:"updated_by,omitempty"  json:"updated_by,omitempty"` // approver party ObjectID hex
	UpdatedAt   time.Time          `bson:"updated_at"    json:"updated_at"`
}

type cached struct {
	enabled bool
	at      time.Time
}

// Service reads flags with a short in-memory cache (hot path: every gated
// request) and writes through on change.
type Service struct {
	coll *mongo.Collection
	ttl  time.Duration

	mu    sync.RWMutex
	cache map[string]cached
}

// NewService binds the flag service to the database.
func NewService(db *mongo.Database) *Service {
	return &Service{
		coll:  db.Collection(collection),
		ttl:   30 * time.Second,
		cache: make(map[string]cached),
	}
}

// Enabled returns the flag value (missing flag = false, fail-closed).
func (s *Service) Enabled(ctx context.Context, key string) bool {
	s.mu.RLock()
	c, ok := s.cache[key]
	s.mu.RUnlock()
	if ok && time.Since(c.at) < s.ttl {
		return c.enabled
	}

	var f Flag
	err := s.coll.FindOne(ctx, bson.D{{Key: "key", Value: key}}).Decode(&f)
	enabled := err == nil && f.Enabled

	s.mu.Lock()
	s.cache[key] = cached{enabled: enabled, at: time.Now()}
	s.mu.Unlock()
	return enabled
}

// Set upserts a flag by key and invalidates the cache entry immediately.
func (s *Service) Set(ctx context.Context, key string, enabled bool, updatedBy string) error {
	_, err := s.coll.UpdateOne(ctx,
		bson.D{{Key: "key", Value: key}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "enabled", Value: enabled},
			{Key: "updated_by", Value: updatedBy},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[key] = cached{enabled: enabled, at: time.Now()}
	s.mu.Unlock()
	return nil
}

// All lists every flag (admin console).
func (s *Service) All(ctx context.Context) ([]Flag, error) {
	cur, err := s.coll.Find(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	var out []Flag
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureDefaults inserts missing flags without touching existing values.
func (s *Service) EnsureDefaults(ctx context.Context, defaults map[string]Flag) error {
	for key, f := range defaults {
		f.Key = key
		if f.UpdatedAt.IsZero() {
			f.UpdatedAt = time.Now().UTC()
		}
		_, err := s.coll.UpdateOne(ctx,
			bson.D{{Key: "key", Value: key}},
			bson.D{{Key: "$setOnInsert", Value: f}},
			options.Update().SetUpsert(true),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// Package orgscope answers the RBAC question "is this resource inside the
// caller's organisational scope?" in O(1) DB reads. Org units denormalise
// their ancestor chain into Path ([]ObjectID), so scope checking is a
// set-membership test — no recursive tree walks on the hot path.
package orgscope

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

const collection = "org_units"

type cachedOrg struct {
	org domain.OrgUnit
	at  time.Time
}

// Resolver caches org units briefly (the tree changes rarely, reads are hot).
type Resolver struct {
	coll *mongo.Collection
	ttl  time.Duration

	mu    sync.RWMutex
	cache map[string]cachedOrg // keyed by ObjectID hex
}

// NewResolver binds the resolver to the database.
func NewResolver(db *mongo.Database) *Resolver {
	return &Resolver{
		coll:  db.Collection(collection),
		ttl:   60 * time.Second,
		cache: make(map[string]cachedOrg),
	}
}

// Get returns an org unit by ObjectID (cached).
func (r *Resolver) Get(ctx context.Context, id primitive.ObjectID) (*domain.OrgUnit, error) {
	key := id.Hex()
	r.mu.RLock()
	c, ok := r.cache[key]
	r.mu.RUnlock()
	if ok && time.Since(c.at) < r.ttl {
		org := c.org
		return &org, nil
	}

	var org domain.OrgUnit
	err := r.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&org)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("org unit " + key)
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}

	r.mu.Lock()
	r.cache[key] = cachedOrg{org: org, at: time.Now()}
	r.mu.Unlock()
	return &org, nil
}

// GetByCode returns an org unit by its unique human-readable business code
// (e.g. "DCS-01842") — display/lookup use only, never for joins.
func (r *Resolver) GetByCode(ctx context.Context, code string) (*domain.OrgUnit, error) {
	var org domain.OrgUnit
	err := r.coll.FindOne(ctx, bson.D{{Key: "code", Value: code}}).Decode(&org)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("org unit with code " + code)
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &org, nil
}

// DescendantIDs returns the ObjectIDs of every org unit of orgType at-or-below
// rootID in the hierarchy (rootID itself included when it is of that type),
// resolved via the denormalised ancestor Path. It backs union-scoped listings
// (a van rider / BMC operator has no single DCS but may see every DCS under
// their union). Not cached — used on cold list paths, not the hot scope check.
func (r *Resolver) DescendantIDs(ctx context.Context, rootID primitive.ObjectID, orgType string) ([]primitive.ObjectID, error) {
	filter := bson.D{
		{Key: "type", Value: orgType},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "_id", Value: rootID}},
			bson.D{{Key: "path", Value: rootID}},
		}},
	}
	cur, err := r.coll.Find(ctx, filter,
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, httpx.Internal(err)
	}
	defer cur.Close(ctx)
	var rows []struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(err)
	}
	out := make([]primitive.ObjectID, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}

// SubtreeIDs returns rootID plus every org unit beneath it (any type), resolved
// via the denormalised ancestor Path. It backs scoped directory reads that span
// a subtree — e.g. a union executive listing the Sachivs held at the union's
// member DCS units, not only at the union node itself. Not cached (cold path).
func (r *Resolver) SubtreeIDs(ctx context.Context, rootID primitive.ObjectID) ([]primitive.ObjectID, error) {
	cur, err := r.coll.Find(ctx, bson.D{{Key: "path", Value: rootID}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, httpx.Internal(err)
	}
	defer cur.Close(ctx)
	var rows []struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(err)
	}
	out := make([]primitive.ObjectID, 0, len(rows)+1)
	out = append(out, rootID)
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}

// Invalidate drops a cached entry (call after updates).
func (r *Resolver) Invalidate(id primitive.ObjectID) {
	r.mu.Lock()
	delete(r.cache, id.Hex())
	r.mu.Unlock()
}

// InScope reports whether target sits at-or-below scope in the hierarchy:
// true when scope == target, or scope appears in target's ancestor Path.
func (r *Resolver) InScope(ctx context.Context, scopeOrgID, targetOrgID primitive.ObjectID) (bool, error) {
	if scopeOrgID == targetOrgID {
		return true, nil
	}
	target, err := r.Get(ctx, targetOrgID)
	if err != nil {
		return false, err
	}
	for _, ancestor := range target.Path {
		if ancestor == scopeOrgID {
			return true, nil
		}
	}
	return false, nil
}

// RequireInScope enforces the actor's org scope over a target org unit.
// SUPER_ADMIN and STATE_AUDITOR (read-wide roles rooted at the federation)
// pass automatically; everyone else needs ancestry. The actor carries its
// org unit as an ObjectID hex string (JWT claims are JSON); it is parsed here.
func (r *Resolver) RequireInScope(ctx context.Context, actor auth.Actor, targetOrgID primitive.ObjectID) error {
	if actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor {
		return nil
	}
	if actor.OrgUnitID == "" {
		return httpx.Forbidden("role token with org scope required")
	}
	scopeID, err := primitive.ObjectIDFromHex(actor.OrgUnitID)
	if err != nil {
		return httpx.Forbidden("role token carries an invalid org scope")
	}
	ok, err := r.InScope(ctx, scopeID, targetOrgID)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.Forbidden("resource is outside your organisational scope")
	}
	return nil
}

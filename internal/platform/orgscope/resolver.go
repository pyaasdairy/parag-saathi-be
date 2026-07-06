// Package orgscope answers the RBAC question "is this resource inside the
// caller's organisational scope?" in O(1) DB reads. Org units denormalise
// their ancestor chain into Path, so scope checking is a set-membership test
// — no recursive tree walks on the hot path.
package orgscope

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

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
	cache map[string]cachedOrg
}

// NewResolver binds the resolver to the database.
func NewResolver(db *mongo.Database) *Resolver {
	return &Resolver{
		coll:  db.Collection(collection),
		ttl:   60 * time.Second,
		cache: make(map[string]cachedOrg),
	}
}

// Get returns an org unit by ID (cached).
func (r *Resolver) Get(ctx context.Context, id string) (*domain.OrgUnit, error) {
	r.mu.RLock()
	c, ok := r.cache[id]
	r.mu.RUnlock()
	if ok && time.Since(c.at) < r.ttl {
		org := c.org
		return &org, nil
	}

	var org domain.OrgUnit
	err := r.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&org)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("org unit " + id)
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}

	r.mu.Lock()
	r.cache[id] = cachedOrg{org: org, at: time.Now()}
	r.mu.Unlock()
	return &org, nil
}

// Invalidate drops a cached entry (call after updates).
func (r *Resolver) Invalidate(id string) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// InScope reports whether target sits at-or-below scope in the hierarchy:
// true when scope == target, or scope appears in target's ancestor Path.
func (r *Resolver) InScope(ctx context.Context, scopeOrgID, targetOrgID string) (bool, error) {
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
// pass automatically; everyone else needs ancestry.
func (r *Resolver) RequireInScope(ctx context.Context, actor auth.Actor, targetOrgID string) error {
	if actor.RoleCode == domain.RoleSuperAdmin || actor.RoleCode == domain.RoleStateAuditor {
		return nil
	}
	if actor.OrgUnitID == "" {
		return httpx.Forbidden("role token with org scope required")
	}
	ok, err := r.InScope(ctx, actor.OrgUnitID, targetOrgID)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.Forbidden("resource is outside your organisational scope")
	}
	return nil
}

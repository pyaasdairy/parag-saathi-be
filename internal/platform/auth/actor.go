package auth

import (
	"context"
	"sync"
)

// Actor is the authenticated caller extracted from a verified token and
// stored on the request context by the auth middleware.
type Actor struct {
	PartyID          string
	Phone            string
	KYCTier          string
	Kind             string // session | role
	RoleAssignmentID string
	RoleCode         string
	OrgUnitID        string
	OrgType          string
}

// HasRole reports whether the actor operates under one of the given roles.
func (a Actor) HasRole(roles ...string) bool {
	for _, r := range roles {
		if a.RoleCode == r {
			return true
		}
	}
	return false
}

type actorCtxKey struct{}

// actorHolder is a mutable cell installed by OUTER middleware (audit) so
// identity established by INNER middleware (Authenticate) can flow back out.
// Context values only propagate inward; the shared pointer bridges the gap.
type actorHolder struct {
	mu sync.Mutex
	a  *Actor
}

type actorHolderCtxKey struct{}

// WithActorHolder installs an empty actor holder on the context. Middleware
// mounted before authentication uses it to observe the actor after the
// handler chain ran.
func WithActorHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, actorHolderCtxKey{}, &actorHolder{})
}

// HeldActor returns the actor captured by an inner WithActor call, if a
// holder was installed and authentication happened.
func HeldActor(ctx context.Context) (Actor, bool) {
	h, ok := ctx.Value(actorHolderCtxKey{}).(*actorHolder)
	if !ok {
		return Actor{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.a == nil {
		return Actor{}, false
	}
	return *h.a, true
}

// WithActor stores the actor on the context, and also fills the outward
// actor holder when one was installed upstream (see WithActorHolder).
func WithActor(ctx context.Context, a Actor) context.Context {
	if h, ok := ctx.Value(actorHolderCtxKey{}).(*actorHolder); ok {
		h.mu.Lock()
		cp := a
		h.a = &cp
		h.mu.Unlock()
	}
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// ActorFrom retrieves the actor, if any.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorCtxKey{}).(Actor)
	return a, ok
}

// Package ratelimit provides pluggable request rate limiting behind a single
// interface, so the HTTP middleware stays backend-agnostic.
//
// Two backends ship:
//   - Memory  — a per-key token bucket in process memory. Correct and fastest
//     for a SINGLE instance; each replica limits only the traffic it sees.
//   - Redis   — a shared token bucket in Redis, evaluated atomically by a Lua
//     script. Gives GLOBAL fairness across many API replicas — the reason to
//     reach for Redis in this system (blueprint §6/§17: rate-limiting at the
//     edge). This is a legitimate Redis use; duplicate-prevention is NOT (that
//     lives in MongoDB's atomic conditional updates, which are strictly safer
//     than any external lock).
//
// Selection is by configuration: set REDIS_URL and the server uses Redis,
// otherwise it falls back to Memory. Dev and tests need no Redis.
package ratelimit

import "context"

// Limiter decides whether a request keyed by `key` (typically the client IP)
// may proceed right now. Allow must be safe for concurrent use.
type Limiter interface {
	// Allow reports whether one unit of quota is available for key, consuming
	// it when true. On backend errors implementations fail OPEN (return true)
	// so a rate-limiter outage never takes down the API.
	Allow(ctx context.Context, key string) bool
	// Kind names the backend for logging/metrics ("memory" | "redis").
	Kind() string
}

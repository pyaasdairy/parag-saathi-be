package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Memory is an in-process, per-key token-bucket limiter. Each API instance
// enforces its own limit; at multi-replica scale switch to Redis for global
// fairness. A janitor evicts idle buckets so the map cannot grow unbounded.
type Memory struct {
	rps   float64
	burst int

	mu      sync.Mutex
	buckets map[string]*memBucket
}

type memBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewMemory builds an in-process limiter allowing `rps` requests/second per key
// with a `burst` allowance, and starts the idle-bucket janitor.
func NewMemory(rps float64, burst int) *Memory {
	m := &Memory{rps: rps, burst: burst, buckets: make(map[string]*memBucket)}
	go m.janitor()
	return m
}

// Allow consumes one token for key, refilling per the configured rate.
func (m *Memory) Allow(_ context.Context, key string) bool {
	m.mu.Lock()
	b, ok := m.buckets[key]
	if !ok {
		b = &memBucket{lim: rate.NewLimiter(rate.Limit(m.rps), m.burst)}
		m.buckets[key] = b
	}
	b.seen = time.Now()
	m.mu.Unlock()
	return b.lim.Allow()
}

// Kind identifies the backend.
func (m *Memory) Kind() string { return "memory" }

func (m *Memory) janitor() {
	for range time.Tick(5 * time.Minute) {
		m.mu.Lock()
		for key, b := range m.buckets {
			if time.Since(b.seen) > 10*time.Minute {
				delete(m.buckets, key)
			}
		}
		m.mu.Unlock()
	}
}

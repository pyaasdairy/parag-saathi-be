package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// PerIPRateLimit applies a token-bucket limit per client IP. In-memory by
// design: each API replica defends itself; global fairness belongs to the
// gateway/WAF tier in production (blueprint §6).
func PerIPRateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	type bucket struct {
		lim  *rate.Limiter
		seen time.Time
	}
	var (
		mu      sync.Mutex
		buckets = make(map[string]*bucket)
	)

	// Janitor: drop buckets idle >10 min so the map can't grow unbounded.
	go func() {
		for range time.Tick(5 * time.Minute) {
			mu.Lock()
			for ip, b := range buckets {
				if time.Since(b.seen) > 10*time.Minute {
					delete(buckets, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			mu.Lock()
			b, ok := buckets[ip]
			if !ok {
				b = &bucket{lim: rate.NewLimiter(rate.Limit(rps), burst)}
				buckets[ip] = b
			}
			b.seen = time.Now()
			mu.Unlock()

			if !b.lim.Allow() {
				httpx.Error(w, r, httpx.TooManyRequests("too many requests — slow down"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

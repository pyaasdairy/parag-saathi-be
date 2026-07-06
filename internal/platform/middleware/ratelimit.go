package middleware

import (
	"net"
	"net/http"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/ratelimit"
)

// RateLimit throttles requests per client IP using the supplied Limiter. The
// backend (in-process or Redis-shared) is chosen at wiring time in cmd/server,
// so this middleware is backend-agnostic — the same code path defends a single
// instance or a whole replica fleet.
func RateLimit(limiter ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			if !limiter.Allow(r.Context(), ip) {
				httpx.Error(w, r, httpx.TooManyRequests("too many requests — slow down"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

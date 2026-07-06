// Package httpapi assembles the HTTP surface: global middleware, operational
// endpoints (health/readiness/metrics/version), and the /api/v1 tree where
// every domain module mounts its routes.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
	"github.com/pyaas/saathi-backend/internal/platform/sse"
)

// Version is stamped via -ldflags at release build time.
var Version = "0.1.0-dev"

// New builds the fully wired HTTP handler.
func New(d *deps.Deps) http.Handler {
	r := chi.NewRouter()

	// Order matters: recover innermost-last so panics in anything above are caught.
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(middleware.Metrics)
	r.Use(middleware.RequestLogger(d.Log))
	r.Use(middleware.RateLimit(d.RateLimiter))
	r.Use(chimw.Recoverer)
	r.Use(chimw.Timeout(30 * time.Second))

	// Unmatched routes and wrong methods must carry the same JSON error
	// envelope every other response uses — never chi's plain-text defaults.
	// (chimw.Timeout's bare 504 on handler deadline is a known, accepted gap.)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.RouteNotFound())
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.MethodNotAllowed())
	})

	// Operational endpoints — outside /api/v1, never authenticated.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.DB.Client().Ping(ctx, nil); err != nil {
			httpx.Error(w, r, httpx.Internal(err))
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/version", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"version": Version, "service": "saathi-backend"})
	})

	r.Route("/api/v1", func(api chi.Router) {
		// Inner Route() calls create fresh chi Muxes — re-register the JSON
		// 404/405 handlers so the envelope also covers /api/v1 subtrees.
		api.NotFound(func(w http.ResponseWriter, r *http.Request) {
			httpx.Error(w, r, httpx.RouteNotFound())
		})
		api.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			httpx.Error(w, r, httpx.MethodNotAllowed())
		})
		api.Use(middleware.AuditMutations(d.Audit))

		// Live event stream (SSE) for already-open dashboards — any
		// authenticated party; the hub targets events by role. Mounted here
		// (cross-cutting, not owned by one domain module).
		api.With(middleware.Authenticate(d.JWT), middleware.RequireSession).
			Get("/events/stream", sse.StreamHandler(d.SSE))

		RegisterModules(api, d)
	})

	return r
}

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
	// NOTE: the 30s request deadline is applied to the MODULE routes only, not
	// globally — see the /api/v1 mount below. Applied globally it also wrapped
	// the SSE stream, whose whole purpose is to stay open: chimw.Timeout put a
	// 30s deadline on r.Context(), sse.StreamHandler selects on ctx.Done(), and
	// so every live dashboard was disconnected every 30 seconds.

	// Unmatched routes and wrong methods must carry the same JSON error
	// envelope every other response uses — never chi's plain-text defaults.
	// (chimw.Timeout's bare 504 on handler deadline is a known, accepted gap.)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.RouteNotFound())
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.MethodNotAllowed())
	})

	// Operational endpoints — outside /api/v1, never authenticated. `build` marks
	// the deployed code revision so a running instance can be matched to a commit.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok", "build": "26.07.03-serverprice-alwaysreview"})
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
		//
		// Registered OUTSIDE the request-deadline group below: a long-lived
		// stream must not carry a 30s deadline. Client disconnects still cancel
		// it, because the request context is cancelled on connection close.
		api.With(middleware.Authenticate(d.JWT), middleware.RequireSession).
			Get("/events/stream", sse.StreamHandler(d.SSE))

		// Every domain route gets the 30s deadline. Nothing under here is
		// long-polling or streaming, so a request still running after 30s is
		// stuck and should be shed.
		api.Group(func(timed chi.Router) {
			timed.Use(chimw.Timeout(30 * time.Second))
			RegisterModules(timed, d)
		})
	})

	return r
}

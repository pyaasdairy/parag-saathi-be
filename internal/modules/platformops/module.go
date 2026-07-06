// Package platformops is the platform-operations module (blueprint §12): the
// SUPER_ADMIN feature-flag console, the STATE_AUDITOR read-only audit surface
// with its GIGW/DPDP immutable export, the notifications outbox with the mock
// SMS dispatch worker (real deployment: MSG91 / telecom DLT gateway, §13),
// and the SUPPORT_AGENT audited limited-PII lookup (§5.2 role 20).
//
// It also subscribes to cross-module bus topics — payout credited, safety
// gate blocked, MVU dispatched — and turns them into queued vernacular SMS.
package platformops

import (
	"context"
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires the platformops module and mounts /admin, /audit,
// /notifications and /support (the router hands us the /api/v1 subtree).
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "platformops"))
	repo := newRepository(d.DB)
	svc := newService(d, repo, log)
	h := newHandler(svc)

	r.Route("/admin", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// PCDF_ADMIN may LIST flags; flipping one is SUPER_ADMIN only —
		// capability gating is a deliberate act (blueprint principle #6).
		r.With(middleware.RequireRoles(domain.RoleSuperAdmin, domain.RolePCDFAdmin)).
			Get("/flags", h.listFlags)
		r.With(middleware.RequireRoles(domain.RoleSuperAdmin)).
			Put("/flags/{key}", h.setFlag)
	})

	r.Route("/audit", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))
		r.Use(middleware.RequireRoles(domain.RoleStateAuditor, domain.RoleSuperAdmin))

		// Read-only by design: there is deliberately NO write/delete API
		// surface for audit logs (§12) — entries land via deps.Audit only.
		r.Get("/logs", h.listAuditLogs)
		r.Get("/logs/export", h.exportAuditLogs)
	})

	r.Route("/notifications", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// SUPER_ADMIN only: a help-desk role has no need to read outbox
		// message bodies, and the outbox carries credential-adjacent traffic
		// (OTP notifications — additionally redacted in the service).
		r.With(middleware.RequireRoles(domain.RoleSuperAdmin)).
			Get("/", h.listNotifications)
		r.With(middleware.RequireRoles(domain.RoleSuperAdmin)).
			Post("/worker/run", h.runWorker)
	})

	r.Route("/support", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))
		r.Use(middleware.RequireRoles(domain.RoleSupportAgent, domain.RoleSuperAdmin))

		r.Get("/parties/lookup", h.lookupParty)
	})

	// Cross-module reactions arrive over the bus (modules never import each
	// other). The bus already runs each handler on its own goroutine with a
	// bounded context and panic recovery; the wrapper below adds an explicit
	// second belt at this module's boundary.
	d.Bus.Subscribe(eventbus.TopicPayoutCredited, safeHandler(log, svc.onPayoutCredited))
	d.Bus.Subscribe(eventbus.TopicGateBlocked, safeHandler(log, svc.onGateBlocked))
	d.Bus.Subscribe(eventbus.TopicMVUDispatched, safeHandler(log, svc.onMVUDispatched))
}

// safeHandler wraps a bus reaction so a programming error in notification
// fan-out can never disturb the publishing flow.
func safeHandler(log *slog.Logger, reaction func(ctx context.Context, payload any)) eventbus.Handler {
	return func(ctx context.Context, topic string, payload any) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("platformops bus handler panic",
					slog.String("topic", topic), slog.Any("panic", rec))
			}
		}()
		reaction(ctx, payload)
	}
}

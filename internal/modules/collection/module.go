// Package collection implements the core procurement loop of Saathi
// (blueprint §8.1, §8.2): org-scoped rate charts, anti-tamper analyzer
// readings, offline-first idempotent milk pours with same-day pricing, and
// per-farmer daily invoices behind the same-day payment promise.
//
// Bus topics published by this module:
//   - eventbus.TopicPourRecorded  — payload domain.MilkPour (the newly recorded pour)
//   - eventbus.TopicInvoiceIssued — payload domain.Invoice (the newly issued invoice)
package collection

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register mounts the collection module under /collection and wires the
// handler → service → repo stack onto the shared platform dependencies.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "collection"))
	repo := newRepo(d)
	svc := newService(d, repo, log)
	h := &handler{svc: svc}

	r.Route("/collection", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// Rate charts: pricing policy is set at union/federation level and
		// resolved downward to each DCS (nearest ancestor wins).
		r.Route("/rate-charts", func(r chi.Router) {
			r.With(middleware.RequireRoles(domain.RolePCDFAdmin, domain.RoleUnionPresident)).
				Post("/", h.createRateChart)
			r.With(middleware.RequireSession).Get("/active", h.getActiveRateChart)
		})

		// Analyzer readings: flagged, never rejected (anti-tamper envelope §8.2).
		r.Route("/readings", func(r chi.Router) {
			r.With(middleware.RequireRoles(domain.RoleSamitiSacheev, domain.RoleMilkTester)).
				Post("/", h.createReading)
			r.With(middleware.RequireRoles(
				domain.RoleSamitiSacheev, domain.RoleMilkTester, domain.RoleUnionFieldSupervisor,
				domain.RoleStateAuditor,
			)).Get("/", h.listReadings)
		})

		// Pours: THE core loop — offline-first idempotent, hard plausibility gate.
		r.Route("/pours", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireRoles(domain.RoleSamitiSacheev, domain.RoleMilkTester))
				r.Post("/", h.createPour)
				r.Post("/batch-sync", h.batchSyncPours)
				r.Post("/{id}/supersede", h.supersedePour)
			})
			r.With(middleware.RequireRoles(
				domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh, domain.RoleMilkTester,
				domain.RoleUnionFieldSupervisor, domain.RoleFarmer, domain.RoleStateAuditor,
			)).Get("/", h.listPours)
		})

		// Invoices: the same-day settlement artefact (§8.1).
		r.Route("/invoices", func(r chi.Router) {
			r.With(middleware.RequireRoles(domain.RoleSamitiSacheev)).
				Post("/generate", h.generateInvoices)
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireRoles(
					domain.RoleFarmer, domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh,
					domain.RoleMilkTester, domain.RoleUnionFieldSupervisor,
					domain.RoleUnionPresident, domain.RolePCDFAdmin, domain.RoleStateAuditor,
				))
				r.Get("/", h.listInvoices)
				r.Get("/{id}", h.getInvoice)
			})
		})
	})
}

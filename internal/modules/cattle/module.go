// Package cattle owns the livestock side of Saathi: the animal registry keyed
// on the 12-digit Pashu Aadhaar ear-tag ID, the animal health history with its
// mock Bharat Pashudhan sync, the 1962 Mobile Veterinary Unit dispatch flow,
// the vernacular education hub, and the dormant collar-telemetry path that is
// provisioned but capability-gated until a government collar scheme lands
// (blueprint §9, §10).
package cattle

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires the cattle module under /cattle. The router mounts this
// subtree at /api/v1, so routes land at /api/v1/cattle/...
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "cattle"))
	repo := newRepository(d.DB)
	svc := newService(d, repo, log)
	h := newHandler(svc)

	r.Route("/cattle", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		r.Route("/animals", func(r chi.Router) {
			// Registration: farmers self-serve; village-tier staff and the
			// health tier register on a farmer's behalf.
			r.With(middleware.RequireRoles(
				domain.RoleFarmer, domain.RoleSamitiSacheev, domain.RoleLRP,
				domain.RoleAITech, domain.RoleVeterinarian,
			)).Post("/", h.registerAnimal)

			// Any authenticated party may list; the service pins farmers and
			// plain session tokens to their own herd.
			r.With(middleware.RequireSession).Get("/", h.listAnimals)

			// Reads: the owner farmer, or care-circle roles. Consent gating
			// is a v2 TODO — see canViewAnimal in service.go.
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireRoles(
					domain.RoleFarmer, domain.RoleVeterinarian,
					domain.RoleAITech, domain.RoleMissionOfficial,
				))
				r.Get("/{animalID}", h.getAnimal)
				r.Get("/{animalID}/health-events", h.listHealthEvents)
			})

			r.With(middleware.RequireRoles(domain.RoleVeterinarian, domain.RoleAITech)).
				Post("/{animalID}/health-events", h.logHealthEvent)

			// Mock Bharat Pashudhan push (§9) — real integration is a swap of
			// the service internals, not an API change.
			r.With(middleware.RequireRoles(domain.RoleVeterinarian, domain.RolePCDFAdmin)).
				Post("/{animalID}/bp-sync", h.syncBharatPashudhan)
		})

		// 1962 Mobile Veterinary Unit dispatch loop (§10).
		r.Route("/mvu-cases", func(r chi.Router) {
			r.With(middleware.RequireRoles(domain.RoleFarmer)).Post("/", h.createMVUCase)
			r.With(middleware.RequireRoles(
				domain.RoleFarmer, // may list their OWN cases (forced to self in the service)
				domain.RoleVeterinarian, domain.RoleMVUDriver,
				domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh, domain.RoleMissionOfficial,
			)).Get("/", h.listMVUCases)
			r.With(middleware.RequireRoles(domain.RoleVeterinarian, domain.RoleMVUDriver)).
				Post("/{caseID}/dispatch", h.dispatchMVUCase)
			r.With(middleware.RequireRoles(domain.RoleVeterinarian)).
				Post("/{caseID}/close", h.closeMVUCase)
		})

		// Education hub (§10): published content is readable by everyone.
		r.Route("/education", func(r chi.Router) {
			r.With(middleware.RequireSession).Get("/", h.listEducation)
			r.With(middleware.RequireRoles(domain.RolePCDFAdmin, domain.RoleMissionOfficial)).
				Post("/", h.createEducation)
		})

		// Dormant collar backdoor (§9): the endpoint ships now but stays
		// behind flags.FlagCollarTelemetry — activation is a flag flip.
		r.With(middleware.RequireRoles(domain.RoleServiceAccount)).
			Post("/telemetry", h.ingestTelemetry)
	})
}

// Package dashboards serves read-only home-screen aggregates (farmer summary,
// society stats) computed server-side to cut client round-trips. It writes
// nothing — it only reads the pour, invoice and role-assignment collections.
//
// Route prefix (mounted under /api/v1): /dashboards.
package dashboards

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires repo → service → handler and mounts the dashboard routes.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "dashboards"))
	svc := newService(d, newRepo(d.DB), log)
	h := newHandler(svc)

	r.Route("/dashboards", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// Farmer summary: the farmer themselves, or a society/union/admin role
		// (the service forces FARMER to self and org-scopes the rest).
		r.With(middleware.RequireRoles(
			domain.RoleFarmer,
			domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh,
			domain.RoleUnionFieldSupervisor, domain.RoleUnionPresident,
			domain.RolePCDFAdmin, domain.RoleMissionOfficial,
		)).Get("/farmer/{partyID}", h.farmerSummary)

		// Society stats: the DCS console roles (org-scoped in the service).
		r.With(middleware.RequireRoles(
			domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh, domain.RoleMilkTester,
			domain.RoleUnionFieldSupervisor, domain.RoleUnionPresident,
			domain.RolePCDFAdmin, domain.RoleMissionOfficial,
		)).Get("/society/{dcsID}", h.societyStats)
	})
}

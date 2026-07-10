// Package orgs implements the cooperative-hierarchy module (blueprint §5.1):
// the FEDERATION → MILK_UNION → {PROCESSING_PLANT, BMC, DCS} org-unit tree.
// It owns org-unit creation and edits (PCDF/state admins), directory reads
// for any authenticated party, subtree reads for oversight roles, and the
// membership roster (active role assignments joined with parties).
package orgs

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register mounts the orgs module under /orgs on the /api/v1 subtree.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "orgs"))
	repo := newRepository(d.DB)
	svc := newService(repo, d.Orgs, log)
	h := newHandler(svc)

	r.Route("/orgs", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// Hierarchy writes — state-apex administrators only.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRoles(domain.RoleSuperAdmin, domain.RolePCDFAdmin))
			r.Post("/", h.create)
			r.Patch("/{id}", h.update)
		})

		// Directory reads — any authenticated party (session or role token).
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireSession)
			r.Get("/", h.list)
			r.Get("/{id}", h.get)
			r.Get("/{id}/children", h.children)
		})

		// Subtree read — oversight roles, scope-checked in the service.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRoles(
				domain.RoleSuperAdmin,
				domain.RolePCDFAdmin,
				domain.RoleMissionOfficial,
				domain.RoleUnionPresident,
				domain.RoleStateAuditor,
			))
			r.Get("/{id}/tree", h.tree)
		})

		// Membership roster — org staff roles, scope-checked in the service.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRoles(
				domain.RoleSamitiSacheev,
				domain.RoleSamitiAdhyaksh,
				domain.RoleMilkTester,
				domain.RoleUnionFieldSupervisor,
				domain.RoleUnionPresident,
				domain.RolePCDFAdmin,
				domain.RoleSuperAdmin,
			))
			r.Get("/{id}/members", h.members)
		})
	})
}

// Package cms implements the content-management module (blueprint §6.1): the
// authored content the field app renders — scheme cards, explainer videos,
// articles and Get-Help helpline entries. Reads are a versioned delta pull
// (the app fetches only what changed since its last cursor); writes are
// restricted to federation/mission administrators and mint a fresh monotonic
// version. The type set is open, so a future content type (e.g. maps) slots
// in with no change to storage, sync or RBAC.
package cms

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register mounts the cms module under /content on the /api/v1 subtree.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "cms"))
	repo := newRepository(d.DB)
	svc := newService(repo, d.Orgs, log)
	h := newHandler(svc)

	r.Route("/content", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// Reads — any authenticated party (session or role token).
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireSession)
			r.Get("/", h.list)
			r.Get("/helpline", h.helpline)
		})

		// Authoring — federation / mission administrators only.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRoles(
				domain.RolePCDFAdmin,
				domain.RoleMissionOfficial,
				domain.RoleSuperAdmin,
			))
			r.Post("/", h.create)
			r.Put("/{id}", h.update)
		})
	})
}

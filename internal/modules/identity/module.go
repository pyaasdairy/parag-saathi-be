// Package identity owns authentication (OTP login, refresh-token rotation,
// role selection), the Party registry, org-scoped role assignments and KYC
// verification — blueprint §4 (identity) and §5 (roles & RBAC).
//
// Route prefixes (mounted under /api/v1): /auth, /parties, /roles, /kyc.
// This module emits no provenance events; identity changes are covered by
// the audit middleware.
package identity

import (
	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires repo → service → handler and mounts the identity routes.
func Register(r chi.Router, d *deps.Deps) {
	repo := newRepository(d.DB)
	svc := newService(d, repo)
	h := newHandler(svc)

	r.Route("/auth", func(r chi.Router) {
		// Public login flow — no token yet.
		r.Post("/otp/request", h.requestOTP)
		r.Post("/otp/verify", h.verifyOTP)
		r.Post("/refresh", h.refresh)

		// Any logged-in party (session or role token).
		r.Group(func(r chi.Router) {
			r.Use(middleware.Authenticate(d.JWT), middleware.RequireSession)
			r.Post("/logout", h.logout)
			r.Get("/roles", h.listMyRoles)
			r.Post("/role/select", h.selectRole)
		})
	})

	r.Route("/parties", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT), middleware.RequireSession)
		r.Get("/me", h.getMe)
		r.Patch("/me", h.patchMe)
	})

	r.Route("/kyc", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT), middleware.RequireSession)
		r.Post("/aadhaar", h.verifyAadhaar)
		r.Post("/bank", h.verifyBank)
		r.Get("/me", h.listMyKYC)
	})

	r.Route("/roles", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))
		// SUPER_ADMIN passes RequireRoles automatically; the fine-grained
		// granter matrix (§5.2) is enforced in the service.
		r.Use(middleware.RequireRoles(
			domain.RolePCDFAdmin,
			domain.RoleUnionPresident,
			domain.RoleSamitiAdhyaksh,
		))
		r.Post("/assignments", h.createAssignment)
		r.Delete("/assignments/{id}", h.revokeAssignment)
		r.Get("/assignments", h.listAssignments)
	})
}

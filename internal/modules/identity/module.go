// Package identity owns authentication (OTP login, refresh-token rotation,
// role selection), the Party registry, org-scoped role assignments and the
// KYC approval workflow — blueprint §4 (identity) and §5 (roles & RBAC).
//
// The KYC flow is: login → submit KYC (PENDING) → an authorised reviewer
// (Organising Manager / District Verifier / PCDF Admin / Super Admin)
// approves or rejects → only a VERIFIED tier unlocks the matching role token.
//
// Route prefixes (mounted under /api/v1): /auth, /parties, /roles, /kyc.
// This module emits no provenance events; identity changes are covered by
// the audit middleware and explicit audit records on KYC review.
package identity

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires repo → service → handler and mounts the identity routes.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "identity"))
	repo := newRepository(d.DB)
	svc := newService(d, repo, log)
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
		r.Use(middleware.Authenticate(d.JWT))

		// Self-service: any logged-in party reads/updates their own profile.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireSession)
			r.Get("/me", h.getMe)
			r.Patch("/me", h.patchMe)
		})

		// Reviewer directory: parties holding a role in an org unit (backs the
		// FE listSachivs picker). Reviewer roles only.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRoles(domain.OnboardingReviewerRoles...))
			r.Get("/", h.listPartiesByRole)
		})
	})

	r.Route("/kyc", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// Self-service KYC: any logged-in party submits evidence.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireSession)
			r.Post("/aadhaar", h.verifyAadhaar)
			r.Post("/bank", h.verifyBank)
			r.Get("/me", h.listMyKYC)
		})

		// Review console: ground staff and admins approve/reject PENDING KYC.
		// Both ORGANISING_MANAGER and the app-coded ONBOARDING_EXECUTIVE qualify.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRoles(domain.OnboardingReviewerRoles...))
			r.Get("/pending", h.listPendingKYC)
			r.Get("/pending/count", h.pendingKYCCount) // live badge value
			r.Post("/{id}/approve", h.approveKYC)
			r.Post("/{id}/reject", h.rejectKYC)
		})
	})

	r.Route("/roles", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))
		// SUPER_ADMIN passes RequireRoles automatically; the fine-grained
		// granter matrix (§5.2) is enforced in the service.
		r.Use(middleware.RequireRoles(
			domain.RolePCDFAdmin,
			domain.RoleUnionPresident,
			domain.RoleSamitiAdhyaksh,
			domain.RoleOrganisingManager,
			domain.RoleOnboardingExecutive,
		))
		r.Post("/assignments", h.createAssignment)
		r.Delete("/assignments/{id}", h.revokeAssignment)
		r.Get("/assignments", h.listAssignments)
	})
}

// Package onboarding owns the assisted-onboarding queue — the
// ONBOARDING_EXECUTIVE flow (blueprint §4 identity). A field executive submits
// a doorstep enrolment request (phone, name, requested role + KYC tier, any
// document references); an authorised reviewer then approves it — which runs a
// cross-collection saga (find-or-create the Party by phone, write a VERIFIED
// KYCRecord at the requested tier, raise the party's tier, grant the role
// assignment) — or rejects it with a reason.
//
// Route prefix (mounted under /api/v1): /onboarding. All endpoints require an
// authenticated ROLE token whose role is in domain.OnboardingReviewerRoles;
// org-scope is enforced per request in the service via Orgs.RequireInScope.
package onboarding

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires repo → service → handler and mounts the onboarding routes.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "onboarding"))
	repo := newRepository(d.DB)
	svc := newService(d, repo, log)
	h := newHandler(svc)

	r.Route("/onboarding", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))
		// Submitter and reviewer share the same authorised role set; finer
		// authority (org scope, approvable tier) is enforced in the service.
		r.Use(middleware.RequireRoles(domain.OnboardingReviewerRoles...))

		r.Route("/requests", func(r chi.Router) {
			r.Post("/", h.submit)
			r.Get("/", h.list)
			r.Post("/{id}/approve", h.approve)
			r.Post("/{id}/reject", h.reject)
		})
	})
}

// Package settlement implements the same-day payment rail under the
// blueprint §8.1/§18-B guardrail: Saathi computes and initiates a batch,
// a DIFFERENT authorised human approves it (dual control), and a licensed
// Payment Aggregator (mocked here) executes the payouts. Scheme subsidies
// ride the strictly separate PFMS/DBT rail (§13) — never the milk-payment
// rail, and never the reverse.
package settlement

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires the settlement module and mounts /settlements and /dbt
// under the /api/v1 subtree it receives.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "settlement"))
	svc := &service{
		repo:   newRepo(d.DB),
		ledger: d.Ledger,
		orgs:   d.Orgs,
		bus:    d.Bus,
		log:    log,
	}
	h := &handler{svc: svc}

	// Read access to batches: village signatories plus union/state oversight.
	readRoles := middleware.RequireRoles(
		domain.RoleSamitiSacheev,
		domain.RoleSamitiAdhyaksh,
		domain.RoleUnionPresident,
		domain.RoleUnionFieldSupervisor,
		domain.RoleStateAuditor,
	)

	r.Route("/settlements", func(sr chi.Router) {
		sr.Use(middleware.Authenticate(d.JWT))

		// Initiate: the Sacheev runs the collection console (§8.1 step 1).
		sr.With(middleware.RequireRoles(domain.RoleSamitiSacheev)).
			Post("/", h.initiate)

		// Approve / reject: a different authorised signatory (dual control is
		// additionally enforced on party identity inside the service).
		sr.With(middleware.RequireRoles(domain.RoleSamitiAdhyaksh, domain.RoleUnionPresident)).
			Post("/{id}/approve", h.approve)
		sr.With(middleware.RequireRoles(domain.RoleSamitiAdhyaksh, domain.RoleUnionPresident)).
			Post("/{id}/reject", h.reject)

		// Execute: hand the approved batch to the (mock) licensed PA.
		sr.With(middleware.RequireRoles(domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh)).
			Post("/{id}/execute", h.execute)

		sr.With(readRoles).Get("/", h.list)

		// Farmer payment history — FARMER tokens are forced to their own party.
		sr.With(middleware.RequireRoles(
			domain.RoleFarmer,
			domain.RoleSamitiSacheev,
			domain.RoleSamitiAdhyaksh,
			domain.RoleUnionPresident,
			domain.RoleUnionFieldSupervisor,
			domain.RoleStateAuditor,
		)).Get("/payouts", h.payouts)

		sr.With(readRoles).Get("/{id}", h.detail)
	})

	r.Route("/dbt", func(dr chi.Router) {
		dr.Use(middleware.Authenticate(d.JWT))

		// A FARMER may apply for a scheme for THEMSELVES (the service forces
		// farmer_party_id to the actor); officials may file for any beneficiary.
		dr.With(middleware.RequireRoles(domain.RoleMissionOfficial, domain.RolePCDFAdmin, domain.RoleFarmer)).
			Post("/requests", h.createDBT)
		dr.With(middleware.RequireRoles(domain.RoleMissionOfficial, domain.RolePCDFAdmin)).
			Post("/requests/{id}/status", h.updateDBTStatus)

		dr.With(middleware.RequireRoles(
			domain.RoleMissionOfficial,
			domain.RolePCDFAdmin,
			domain.RoleStateAuditor,
			domain.RoleFarmer, // forced to own requests in the service
		)).Get("/requests", h.listDBT)
	})
}

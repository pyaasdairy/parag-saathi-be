// Package quality implements QC recording and THE safety-gate verdict writer
// (blueprint §8.3). It is the only module permitted to move a BMC lot or a
// processing batch out of QC_PENDING: a passing result certifies the subject
// (PASSED + certificate), a failing result quarantines it (BLOCKED + reasons).
// Aflatoxin M1 is heat-stable and survives pasteurisation, so this gate runs
// upstream at the BMC and the plant lab — never only at the finished product.
//
// A blocked subject can never advance; the plant module enforces that by
// refusing blocked inputs and by reacting to eventbus.TopicGateBlocked.
package quality

import (
	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires the quality module and mounts its routes under /quality
// (the router hands us the /api/v1 subtree).
func Register(r chi.Router, d *deps.Deps) {
	repo := newRepository(d.DB)
	svc := newService(d, repo)
	h := newHandler(svc)

	r.Route("/quality", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		// FSSAI limit constants — reference data for any signed-in client.
		r.With(middleware.RequireSession).Get("/limits", h.getLimits)

		// Verdict writing: analyst roles only. Both roles share the endpoint;
		// the stage×role pairing (BMC_RAPID↔BMC_OPERATOR, PLANT_LAB↔
		// PLANT_LAB_ANALYST) is enforced in the service.
		r.With(middleware.RequireRoles(domain.RoleBMCOperator, domain.RolePlantLabAnalyst)).
			Post("/qc-results", h.recordQCResult)

		// Reads for supply-chain staff and oversight roles.
		readRoles := middleware.RequireRoles(
			domain.RoleBMCOperator,
			domain.RolePlantLabAnalyst,
			domain.RolePlantOperator,
			domain.RoleUnionFieldSupervisor,
			domain.RoleMissionOfficial,
			domain.RoleStateAuditor,
		)
		r.With(readRoles).Get("/qc-results", h.listQCResults)
		r.With(readRoles).Get("/qc-results/{id}", h.getQCResult)
	})
}

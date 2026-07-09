// Package plant implements the processing tier of the pour→QR chain:
// BMC lots (pooled consignments at the chilling centre), processing batches,
// product lots and QR issuance. It is the ENFORCEMENT layer of the safety
// gate (blueprint §8.3): a lot or batch that has not PASSED quality control
// can never advance — not into a tanker, not into a batch, not onto a shelf.
// Aflatoxin M1 survives pasteurisation, so the gate is checked at every hop,
// belt-and-braces.
package plant

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register wires the plant module under /api/v1/plant.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "plant"))
	repo := NewRepo(d.DB)
	svc := NewService(repo, d, log)
	h := NewHandler(svc)

	r.Route("/plant", func(pr chi.Router) {
		pr.Use(middleware.Authenticate(d.JWT))

		// BMC lots — created/closed/dispatched by the chilling-centre operator;
		// readable by everyone downstream who receives or supervises the milk.
		pr.Route("/bmc-lots", func(lr chi.Router) {
			lr.With(middleware.RequireRoles(
				domain.RoleBMCOperator, domain.RolePlantOperator,
				domain.RoleUnionFieldSupervisor, domain.RolePlantLabAnalyst, domain.RoleStateAuditor,
			)).Get("/", h.ListBMCLots)

			lr.Group(func(wr chi.Router) {
				wr.Use(middleware.RequireRoles(domain.RoleBMCOperator))
				wr.Post("/", h.CreateBMCLot)
				wr.Post("/{id}/close", h.CloseBMCLot)
				wr.Post("/{id}/dispatch", h.DispatchBMCLot)
			})
		})

		// Processing batches — plant operator pools dispatched (gate-passed)
		// BMC lots into a production run; plant lab + supervisor read along.
		pr.Route("/batches", func(br chi.Router) {
			read := middleware.RequireRoles(
				domain.RolePlantOperator, domain.RolePlantLabAnalyst,
				domain.RoleUnionFieldSupervisor, domain.RoleStateAuditor,
			)
			br.With(read).Get("/", h.ListBatches)
			br.With(read).Get("/{id}", h.GetBatch)

			br.Group(func(wr chi.Router) {
				wr.Use(middleware.RequireRoles(domain.RolePlantOperator))
				wr.Post("/", h.CreateBatch)
				wr.Post("/{id}/complete", h.CompleteBatch)
			})
		})

		// Product lots — packaged SKUs out of a COMPLETED batch. Recall is the
		// FSSAI path (§18-C): admin or plant lab pulls the lot from market.
		pr.Route("/product-lots", func(plr chi.Router) {
			plr.With(middleware.RequireRoles(
				domain.RolePlantOperator, domain.RolePlantLabAnalyst,
			)).Post("/", h.CreateProductLot)
			plr.With(middleware.RequireRoles(
				domain.RolePCDFAdmin, domain.RolePlantLabAnalyst,
			)).Post("/{id}/recall", h.RecallProductLot)
		})

		// QR issuance — the consumer-facing end of the trace chain (§7).
		pr.Route("/qrs", func(qr chi.Router) {
			qr.Use(middleware.RequireRoles(
				domain.RolePlantOperator, domain.RolePlantLabAnalyst,
			))
			qr.Post("/", h.IssueQR)
			qr.Get("/", h.ListQRs)
		})
	})

	// No bus subscriptions: the quality module flips lot/batch statuses in
	// their own collections, and every gate here re-reads status at use time.
}

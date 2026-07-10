// Package logistics moves sealed milk from village societies to chilling
// centres (blueprint §7.1): a Samiti Sacheev pools a shift's RECORDED pours
// into a DCS consignment and dispatches it; a van rider runs a route trip
// that picks consignments up stop by stop, logs cold-chain temperatures in
// transit, and hands the load to a BMC. Every hop is chained into the
// provenance ledger so the pour→QR trace survives the pooling boundary.
package logistics

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register mounts the logistics routes under /logistics (the router hands us
// the /api/v1 subtree) and wires handler → service → repo.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "logistics"))
	svc := newService(newRepo(d.DB), d.Orgs, d.Ledger, log)
	h := &handler{svc: svc}

	r.Route("/logistics", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))

		r.Route("/consignments", func(r chi.Router) {
			sacheevOnly := middleware.RequireRoles(domain.RoleSamitiSacheev)
			r.With(sacheevOnly).Post("/", h.createConsignment)
			r.With(sacheevOnly).Post("/{consignmentID}/dispatch", h.dispatchConsignment)
			// DCS→Union B2B invoice leg: the Sacheev submits the sealed
			// consignment to the parent Union (HSN 0401, GST-exempt).
			r.With(sacheevOnly).Post("/{consignmentID}/approve-union", h.approveForUnion)
			consignmentReaders := middleware.RequireRoles(
				domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh, domain.RoleVanRider,
				domain.RoleUnionFieldSupervisor, domain.RoleBMCOperator, domain.RoleStateAuditor,
			)
			r.With(consignmentReaders).Get("/", h.listConsignments)
			r.With(consignmentReaders).Get("/{consignmentID}", h.getConsignment)
			r.With(consignmentReaders).Get("/{consignmentID}/invoice", h.getConsignmentInvoice)
		})

		r.Route("/trips", func(r chi.Router) {
			riderOnly := middleware.RequireRoles(domain.RoleVanRider)
			tripReaders := middleware.RequireRoles(
				domain.RoleVanRider, domain.RoleUnionFieldSupervisor, domain.RoleUnionPresident,
				domain.RoleStateAuditor, domain.RoleBMCOperator,
			)
			// Live tracking is visible to the source Sachiv/Adhyaksh and the
			// destination BMC too — the service scopes each to trips they own.
			trackReaders := middleware.RequireRoles(
				domain.RoleVanRider, domain.RoleUnionFieldSupervisor, domain.RoleUnionPresident,
				domain.RoleStateAuditor, domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh,
				domain.RoleBMCOperator,
			)
			r.With(middleware.RequireRoles(domain.RoleUnionFieldSupervisor, domain.RoleVanRider)).
				Post("/", h.createTrip)
			r.With(riderOnly).Post("/{tripID}/stops/{consignmentID}/pickup", h.pickupStop)
			r.With(riderOnly).Post("/{tripID}/cold-chain", h.logColdChain)
			r.With(riderOnly).Post("/{tripID}/location", h.recordLocation)
			r.With(riderOnly).Post("/{tripID}/deliver", h.deliverTrip)
			// Static "/tracking" is registered before "/{tripID}" so chi matches
			// it literally, not as a trip id.
			r.With(trackReaders).Get("/tracking", h.listActiveTracking)
			r.With(trackReaders).Get("/{tripID}/track", h.trackTrip)
			r.With(tripReaders).Get("/", h.listTrips)
			r.With(tripReaders).Get("/{tripID}", h.getTrip)
		})
	})
}

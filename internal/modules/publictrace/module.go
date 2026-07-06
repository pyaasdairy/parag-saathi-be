// Package publictrace serves the two faces of milk traceability
// (blueprint §7, §8.4):
//
//   - /public — the consumer QR scan and public ledger tamper-check. These
//     routes are DELIBERATELY unauthenticated: a shopper points a phone at a
//     milk pack and must see its provenance with zero friction. This is not a
//     consumer-account backend (descoped) — the global per-IP rate limit is
//     the only throttle.
//   - /trace  — the official recall / root-cause tools: full upstream +
//     downstream event-graph walks for auditors, mission officials and lab
//     analysts (§8.3: a failed aflatoxin lot walks back to contributing
//     samitis and forward to affected batches and QRs).
//
// The consumer view is the HONEST provenance view (§7.4): pooled milk traces
// to the SET of contributing samitis, never to individual farmers.
package publictrace

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register mounts the public QR/ledger routes and the authed trace tools.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "publictrace"))
	service := NewService(d, log)
	handler := &Handler{service: service}

	// PUBLIC subtree — no Authenticate middleware by design (see package doc).
	r.Route("/public", func(pub chi.Router) {
		pub.Get("/qr/{qr_code}", handler.ScanQR)
		pub.Get("/ledger/verify", handler.VerifyLedger)
	})

	// Official trace tools — role-gated, read-only, cross-org by nature
	// (a recall walk necessarily spans DCS → BMC → plant boundaries, so no
	// per-resource org-scope check applies here; the role gate is the fence).
	r.Route("/trace", func(tr chi.Router) {
		tr.Use(middleware.Authenticate(d.JWT))
		tr.Use(middleware.RequireRoles(
			domain.RoleMissionOfficial,
			domain.RolePCDFAdmin,
			domain.RoleUnionPresident,
			domain.RoleUnionFieldSupervisor,
			domain.RolePlantLabAnalyst,
			domain.RoleStateAuditor,
		))
		tr.Get("/{entity_type}/{entity_id}", handler.TraceGraph)
		tr.Get("/{entity_type}/{entity_id}/timeline", handler.Timeline)
	})
}

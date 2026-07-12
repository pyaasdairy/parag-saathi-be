package httpapi

import (
	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/modules/cattle"
	"github.com/pyaas/saathi-backend/internal/modules/cms"
	"github.com/pyaas/saathi-backend/internal/modules/collection"
	"github.com/pyaas/saathi-backend/internal/modules/consumer"
	"github.com/pyaas/saathi-backend/internal/modules/dashboards"
	"github.com/pyaas/saathi-backend/internal/modules/identity"
	"github.com/pyaas/saathi-backend/internal/modules/logistics"
	"github.com/pyaas/saathi-backend/internal/modules/onboarding"
	"github.com/pyaas/saathi-backend/internal/modules/orgs"
	"github.com/pyaas/saathi-backend/internal/modules/plant"
	"github.com/pyaas/saathi-backend/internal/modules/platformops"
	"github.com/pyaas/saathi-backend/internal/modules/publictrace"
	"github.com/pyaas/saathi-backend/internal/modules/quality"
	"github.com/pyaas/saathi-backend/internal/modules/settlement"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
)

// RegisterModules mounts every domain module under /api/v1.
//
// Module contract: each package under internal/modules/<name> exposes
//
//	func Register(r chi.Router, d *deps.Deps)
//
// and mounts its own subtree with its own auth/RBAC middleware. Modules are
// the future microservice seams — they may import domain and platform
// packages, but NEVER each other; cross-module reactions go through the
// event bus.
func RegisterModules(api chi.Router, d *deps.Deps) {
	// publictrace first — it mounts /public without auth.
	publictrace.Register(api, d)

	identity.Register(api, d)
	orgs.Register(api, d)
	cattle.Register(api, d)
	collection.Register(api, d)
	logistics.Register(api, d)
	plant.Register(api, d)
	quality.Register(api, d)
	settlement.Register(api, d)
	platformops.Register(api, d)
	dashboards.Register(api, d)
	onboarding.Register(api, d)
	cms.Register(api, d)

	// Consumer app backend — ADD-ONLY, isolated (own collections + consumer
	// JWT), mounted at /api/v1/consumer. Nothing here mutates operator state.
	consumer.Register(api, d)
}

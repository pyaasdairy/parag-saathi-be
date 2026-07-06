// Package deps is the dependency container handed to every module at wiring
// time. One struct, constructed once in cmd/server, threaded everywhere —
// no globals, trivially fakeable in tests.
package deps

import (
	"log/slog"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
	"github.com/pyaas/saathi-backend/internal/platform/ratelimit"
	"github.com/pyaas/saathi-backend/internal/platform/sse"
)

// Deps carries every shared platform service.
type Deps struct {
	Cfg         *config.Config
	Log         *slog.Logger
	DB          *mongo.Database
	JWT         *auth.JWTManager
	Ledger      *provenance.Ledger
	Audit       *audit.Recorder
	Flags       *flags.Service
	Orgs        *orgscope.Resolver
	Bus         *eventbus.Bus
	SSE         *sse.Hub
	RateLimiter ratelimit.Limiter
}

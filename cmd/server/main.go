// Command server boots the Saathi backend API: config → logger → MongoDB
// (env-only URI) → indexes → platform services → HTTP router, with graceful
// shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/httpapi"
	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"github.com/pyaas/saathi-backend/internal/platform/logger"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
	"github.com/pyaas/saathi-backend/internal/platform/provenance"
	"github.com/pyaas/saathi-backend/internal/platform/ratelimit"
	"github.com/pyaas/saathi-backend/internal/platform/sse"

	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.LogLevel, cfg.Env)

	bootCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, db, err := mongodb.Connect(bootCtx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = client.Disconnect(shutdownCtx)
	}()
	log.Info("mongodb connected", slog.String("db", cfg.MongoDB))

	if err := mongodb.EnsureIndexes(bootCtx, db); err != nil {
		return err
	}
	log.Info("indexes ensured")

	flagSvc := flags.NewService(db)
	if err := flagSvc.EnsureDefaults(bootCtx, map[string]flags.Flag{
		flags.FlagCollarTelemetry:  {Enabled: false, Description: "Dormant collar/wearable telemetry ingestion (§9) — flip when a scheme lands"},
		flags.FlagPhotoOCR:         {Enabled: true, Description: "Photo-OCR bridge for legacy analyzers (§8.2)"},
		flags.FlagONDC:             {Enabled: false, Description: "ONDC commerce compatibility (§11, Phase 3)"},
		flags.FlagConsumerCommerce: {Enabled: false, Description: "Consumer commerce (§11) — descoped, ships dormant; public QR trace stays live"},
	}); err != nil {
		return err
	}

	bus := eventbus.New(log)
	sseHub := sse.NewHub(log)

	// Rate limiter: shared Redis token bucket when REDIS_URL is set (global
	// fairness across replicas), else an in-process bucket (single instance).
	// Duplicate-prevention deliberately does NOT use Redis — that lives in
	// MongoDB's atomic conditional updates, which are safer than any lock.
	var limiter ratelimit.Limiter
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("parse REDIS_URL: %w", err)
		}
		rdb := redis.NewClient(opt)
		if err := rdb.Ping(bootCtx).Err(); err != nil {
			return fmt.Errorf("redis ping (%s): %w", cfg.RedisURL, err)
		}
		defer rdb.Close()
		limiter = ratelimit.NewRedis(rdb, cfg.RateLimitRPS, cfg.RateLimitBurst, log)
		log.Info("rate limiter: redis (shared across replicas)")
	} else {
		limiter = ratelimit.NewMemory(cfg.RateLimitRPS, cfg.RateLimitBurst)
		log.Info("rate limiter: in-process (single instance)")
	}

	// Bridge the KYC review-queue topic to the SSE hub: when a record is
	// submitted/approved/rejected, nudge connected reviewer dashboards so their
	// "pending KYC" badge re-counts live. Reviewer roles only — the client's
	// own scoped count query does the org filtering (nudge, then re-count).
	bus.Subscribe(eventbus.TopicKYCQueueChanged, func(_ context.Context, _ string, payload any) {
		ev, ok := payload.(eventbus.KYCQueueEvent)
		if !ok {
			return
		}
		sseHub.Broadcast(sse.Event{
			Type:  "kyc.pending.changed",
			Data:  ev,
			Roles: domain.OnboardingReviewerRoles, // every reviewer incl. ONBOARDING_EXECUTIVE
		})
	})

	// Bridge the remaining cross-module bus topics to the live SSE dashboard
	// nudges the FE useLiveSync listens for (settlement.changed / quality.changed
	// / pour.recorded). Each is a nudge only — the client re-fetches the
	// authoritative scoped value on receipt, so role-level targeting is enough.
	bridgeSSE := func(topic, sseType string, roles []string) {
		bus.Subscribe(topic, func(_ context.Context, _ string, payload any) {
			sseHub.Broadcast(sse.Event{Type: sseType, Data: payload, Roles: roles})
		})
	}
	bridgeSSE(eventbus.TopicPourRecorded, "pour.recorded",
		[]string{domain.RoleSamitiSacheev, domain.RoleMilkTester, domain.RoleSamitiAdhyaksh, domain.RoleFarmer})
	bridgeSSE(eventbus.TopicQCRecorded, "quality.changed",
		[]string{domain.RolePlantLabAnalyst, domain.RoleBMCOperator, domain.RolePlantOperator, domain.RoleUnionFieldSupervisor})
	bridgeSSE(eventbus.TopicGateBlocked, "quality.changed",
		[]string{domain.RolePlantLabAnalyst, domain.RoleBMCOperator, domain.RolePlantOperator, domain.RoleUnionFieldSupervisor})
	// settlement.changed fires off the payout-credited event (published per farmer
	// as a batch executes) — the money-moving step the FE cares about.
	bridgeSSE(eventbus.TopicPayoutCredited, "settlement.changed",
		[]string{domain.RoleSamitiSacheev, domain.RoleSamitiAdhyaksh, domain.RoleUnionPresident, domain.RoleFarmer})

	d := &deps.Deps{
		Cfg:         cfg,
		Log:         log,
		DB:          db,
		JWT:         auth.NewJWTManager(cfg.JWTSecret, cfg.AccessTokenTTL),
		Ledger:      provenance.NewLedger(db),
		Audit:       audit.NewRecorder(db, log),
		Flags:       flagSvc,
		Orgs:        orgscope.NewResolver(db),
		Bus:         bus,
		SSE:         sseHub,
		RateLimiter: limiter,
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           httpapi.New(d),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("saathi backend listening",
			slog.Int("port", cfg.Port), slog.String("env", cfg.Env), slog.String("version", httpapi.Version))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Info("shutting down", slog.String("signal", sig.String()))
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

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

	d := &deps.Deps{
		Cfg:    cfg,
		Log:    log,
		DB:     db,
		JWT:    auth.NewJWTManager(cfg.JWTSecret, cfg.AccessTokenTTL),
		Ledger: provenance.NewLedger(db),
		Audit:  audit.NewRecorder(db, log),
		Flags:  flagSvc,
		Orgs:   orgscope.NewResolver(db),
		Bus:    eventbus.New(log),
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

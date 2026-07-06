// Package logger builds the process-wide structured logger.
// JSON output in prod (machine-parseable, ships to the log pipeline for the
// CERT-In ≥180-day retention requirement), human-readable text in dev.
package logger

import (
	"log/slog"
	"os"
)

// New returns a configured *slog.Logger and installs it as slog's default.
func New(level, env string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if env == "prod" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	log := slog.New(handler).With(slog.String("service", "saathi-backend"))
	slog.SetDefault(log)
	return log
}

package middleware

import (
	"log/slog"
	"net/http"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// RequestLogger emits one structured log line per request — method, path,
// status, duration, request ID, client IP, and the acting party/role when
// authenticated. This is the debugging backbone: every request is traceable
// end-to-end by request_id across handler/service/repo logs.
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("request_id", chimw.GetReqID(r.Context())),
				slog.String("ip", r.RemoteAddr),
			}
			if a, ok := auth.ActorFrom(r.Context()); ok {
				attrs = append(attrs,
					slog.String("actor_party_id", a.PartyID),
					slog.String("actor_role", a.RoleCode),
				)
			}

			level := slog.LevelInfo
			switch {
			case ww.Status() >= 500:
				level = slog.LevelError
			case ww.Status() >= 400:
				level = slog.LevelWarn
			}
			log.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

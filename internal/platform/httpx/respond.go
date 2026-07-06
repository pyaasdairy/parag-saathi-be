package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// envelope is the uniform success shape: {"data": ..., "meta": {...}?}.
type envelope struct {
	Data any `json:"data"`
	Meta any `json:"meta,omitempty"`
}

type errEnvelope struct {
	Error *AppError `json:"error"`
}

// JSON writes a success response wrapped in the standard envelope.
func JSON(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, envelope{Data: data})
}

// JSONMeta writes a success response with list metadata (pagination etc.).
func JSONMeta(w http.ResponseWriter, status int, data, meta any) {
	writeJSON(w, status, envelope{Data: data, Meta: meta})
}

// Error writes the standard error envelope. Unexpected (non-AppError) errors
// become an opaque 500 and the cause is logged with the request context.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	var ae *AppError
	if !errors.As(err, &ae) {
		ae = Internal(err)
	}
	if ae.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("code", ae.Code),
			slog.Any("cause", ae.CauseOrSelf()),
		)
	}
	writeJSON(w, ae.Status, errEnvelope{Error: ae})
}

// CauseOrSelf returns the wrapped cause if present, else the error itself.
func (e *AppError) CauseOrSelf() error {
	if e.cause != nil {
		return e.cause
	}
	return e
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

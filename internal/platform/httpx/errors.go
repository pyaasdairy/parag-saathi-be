// Package httpx is the HTTP toolkit shared by every module: typed errors,
// uniform JSON envelopes, strict request decoding, pagination.
package httpx

import (
	"fmt"
	"net/http"
)

// AppError is the single error type handlers return. It carries the HTTP
// status plus a stable machine-readable code the frontend can switch on.
type AppError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`

	cause error // wrapped internal error — logged server-side, never serialised
}

func (e *AppError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

// WithDetails returns a copy carrying extra structured context.
func (e *AppError) WithDetails(d any) *AppError {
	cp := *e
	cp.Details = d
	return &cp
}

func newErr(status int, code, msg string) *AppError {
	return &AppError{Status: status, Code: code, Message: msg}
}

// Constructors — use these instead of raw errors in handlers/services.

func BadRequest(code, msg string) *AppError { return newErr(http.StatusBadRequest, code, msg) }

func Unauthorized(msg string) *AppError { return newErr(http.StatusUnauthorized, "UNAUTHORIZED", msg) }

func Forbidden(msg string) *AppError { return newErr(http.StatusForbidden, "FORBIDDEN", msg) }

func NotFound(resource string) *AppError {
	return newErr(http.StatusNotFound, "NOT_FOUND", resource+" not found")
}

// RouteNotFound is the router-level 404 for unmatched paths, keeping the
// uniform JSON error envelope on typo'd routes.
func RouteNotFound() *AppError {
	return newErr(http.StatusNotFound, "NOT_FOUND", "route not found")
}

// MethodNotAllowed is the router-level 405 for wrong-method requests.
func MethodNotAllowed() *AppError {
	return newErr(http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed for this route")
}

func Conflict(code, msg string) *AppError { return newErr(http.StatusConflict, code, msg) }

func Unprocessable(code, msg string) *AppError {
	return newErr(http.StatusUnprocessableEntity, code, msg)
}

func TooManyRequests(msg string) *AppError {
	return newErr(http.StatusTooManyRequests, "RATE_LIMITED", msg)
}

// Internal wraps an unexpected error. The cause is logged server-side but
// never leaked to the client.
func Internal(err error) *AppError {
	e := newErr(http.StatusInternalServerError, "INTERNAL", "internal server error")
	e.cause = err
	return e
}

func (e *AppError) Unwrap() error { return e.cause }

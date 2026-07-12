// Package consumer is the ADD-ONLY backend for the PARAG consumer app
// (Saathi_Consumer_Hybrid_Note). It is deliberately isolated from the operator
// (Saathi) side:
//
//   - its own Mongo collections (consumer_*), never the operator `parties`;
//   - its own consumer JWT (kind="consumer"), never an operator session/role
//     token, so ROLES/KYC/RBAC can never apply to a shopper;
//   - RAW JSON responses + {message} errors, matching the shipped consumer
//     front-end's apiClient contract (NOT the operator {data} envelope);
//   - mounted under /api/v1/consumer, so the front-end's EXPO_PUBLIC_API_URL
//     points at that base and its bare paths (/auth/..., /users/me, /wallet)
//     compose without collision.
//
// The ONLY thing it shares with the Saathi supply side is READ access to the
// public QR/traceability (already public) — the "milk you got came from these
// societies" bridge — and outbound notifications (stock-complete → store
// manager, store → super admin alarm). Nothing here mutates operator state.
package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

// ── Raw-JSON responders (match the consumer FE apiClient contract) ──────────

// writeJSON emits a raw JSON body (no {data} envelope). 204 writes nothing.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusNoContent || body == nil {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// apiError is the error body the FE reads: it does `err?.message`.
// code is included for our own callers/tests; the FE ignores it.
type apiError struct {
	status  int
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

func errBadRequest(msg string) *apiError { return &apiError{http.StatusBadRequest, "BAD_REQUEST", msg} }
func errUnauthorized(msg string) *apiError {
	return &apiError{http.StatusUnauthorized, "UNAUTHORIZED", msg}
}
func errForbidden(msg string) *apiError { return &apiError{http.StatusForbidden, "FORBIDDEN", msg} }
func errNotFound(msg string) *apiError  { return &apiError{http.StatusNotFound, "NOT_FOUND", msg} }
func errConflict(code, msg string) *apiError {
	return &apiError{http.StatusConflict, code, msg}
}
func errUnprocessable(code, msg string) *apiError {
	return &apiError{http.StatusUnprocessableEntity, code, msg}
}
func errInternal(msg string) *apiError {
	return &apiError{http.StatusInternalServerError, "INTERNAL", msg}
}

// writeErr renders an *apiError (or a 500 for anything else). Never leaks a
// raw internal error message to the client.
func writeErr(w http.ResponseWriter, err error) {
	if ae, ok := err.(*apiError); ok {
		writeJSON(w, ae.status, ae)
		return
	}
	writeJSON(w, http.StatusInternalServerError, &apiError{Code: "INTERNAL", Message: "Something went wrong"})
}

// decode reads a JSON request body into dst (empty body tolerated).
func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return errBadRequest("invalid JSON body")
	}
	return nil
}

// ── Consumer JWT (kind="consumer") — self-contained, reuses only the secret ──

const (
	consumerTokenKind = "consumer"
	consumerIssuer    = "saathi-consumer"
)

type consumerClaims struct {
	Phone string `json:"phone,omitempty"`
	Kind  string `json:"kind"`
	jwt.RegisteredClaims
}

// signAccessToken mints a short-lived consumer access token.
func (s *service) signAccessToken(consumerID, phone string, now time.Time) (string, error) {
	claims := consumerClaims{
		Phone: phone,
		Kind:  consumerTokenKind,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   consumerID,
			Issuer:    consumerIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.deps.Cfg.AccessTokenTTL)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.consumerKey)
}

// parseAccessToken validates a consumer token and returns its consumer id.
func (s *service) parseAccessToken(token string) (consumerID, phone string, err error) {
	var claims consumerClaims
	parsed, perr := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errUnauthorized("bad token")
		}
		return s.consumerKey, nil
	}, jwt.WithIssuer(consumerIssuer))
	if perr != nil || !parsed.Valid {
		return "", "", errUnauthorized("invalid or expired token")
	}
	if claims.Kind != consumerTokenKind {
		return "", "", errUnauthorized("not a consumer token")
	}
	return claims.Subject, claims.Phone, nil
}

// ── Auth middleware: consumer id from the Bearer token into context ─────────

type ctxKey string

const consumerCtxKey ctxKey = "consumer"

type consumerActor struct {
	ID    string
	Phone string
}

func (s *service) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeErr(w, errUnauthorized("authentication required"))
			return
		}
		id, phone, err := s.parseAccessToken(strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			writeErr(w, err)
			return
		}
		ctx := context.WithValue(r.Context(), consumerCtxKey, consumerActor{ID: id, Phone: phone})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// actorFrom returns the authenticated consumer, or false.
func actorFrom(ctx context.Context) (consumerActor, bool) {
	a, ok := ctx.Value(consumerCtxKey).(consumerActor)
	return a, ok
}

// Package middleware carries the cross-cutting HTTP concerns: token
// authentication, role enforcement, audit capture, per-IP rate limiting,
// Prometheus metrics.
package middleware

import (
	"net/http"
	"strings"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// Authenticate verifies the Bearer token and installs the Actor on the
// request context. Requests without a valid token are rejected — public
// routes simply don't mount this middleware.
func Authenticate(jwtm *auth.JWTManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok || token == "" {
				httpx.Error(w, r, httpx.Unauthorized("missing bearer token"))
				return
			}
			claims, err := jwtm.Parse(token)
			if err != nil {
				httpx.Error(w, r, httpx.Unauthorized("invalid or expired token"))
				return
			}
			actor := auth.Actor{
				PartyID:          claims.PartyID,
				Phone:            claims.Phone,
				KYCTier:          claims.KYCTier,
				Kind:             claims.Kind,
				RoleAssignmentID: claims.RoleAssignmentID,
				RoleCode:         claims.RoleCode,
				OrgUnitID:        claims.OrgUnitID,
				OrgType:          claims.OrgType,
			}
			next.ServeHTTP(w, r.WithContext(auth.WithActor(r.Context(), actor)))
		})
	}
}

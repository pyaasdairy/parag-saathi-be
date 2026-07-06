package middleware

import (
	"net/http"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// RequireRoles allows only ROLE-kind tokens whose role is in the allow-list.
// SUPER_ADMIN passes every gate (break-glass admin — all its calls are
// audited by the audit middleware).
func RequireRoles(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := auth.ActorFrom(r.Context())
			if !ok {
				httpx.Error(w, r, httpx.Unauthorized("authentication required"))
				return
			}
			if actor.Kind != auth.TokenKindRole {
				httpx.Error(w, r, httpx.Forbidden("a role token is required — select a role first (POST /api/v1/auth/role/select)"))
				return
			}
			if actor.RoleCode == domain.RoleSuperAdmin {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := allowed[actor.RoleCode]; !ok {
				httpx.Error(w, r, httpx.Forbidden("role "+actor.RoleCode+" is not permitted for this action"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSession allows any authenticated party (session or role token) —
// used for identity endpoints like "list my roles".
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.ActorFrom(r.Context()); !ok {
			httpx.Error(w, r, httpx.Unauthorized("authentication required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

package middleware

import (
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/pyaas/saathi-backend/internal/platform/audit"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// AuditMutations records every state-changing request (non-GET/HEAD/OPTIONS)
// into the immutable audit trail: actor, action, status, IP, request ID.
// Runs after the handler so the final status code is captured; the write
// itself is async and adds no request latency.
//
// This middleware mounts OUTSIDE the per-module Authenticate middleware, so
// the actor installed on the inner request context never propagates back to
// this frame. An actor holder is installed on the context before the chain
// runs; auth.WithActor fills it, and the identity is read back here so the
// who-did-what contract holds for every HTTP mutation.
func AuditMutations(rec *audit.Recorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			r = r.WithContext(auth.WithActorHolder(r.Context()))
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			entry := audit.Entry{
				Action:    "http." + r.Method + " " + r.URL.Path,
				Status:    ww.Status(),
				IP:        r.RemoteAddr,
				RequestID: chimw.GetReqID(r.Context()),
			}
			if actor, ok := auth.HeldActor(r.Context()); ok {
				entry.ActorPartyID = actor.PartyID
				entry.ActorRole = actor.RoleCode
			}
			rec.Record(r.Context(), entry)
		})
	}
}

package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// heartbeatInterval keeps the connection alive through proxies/load balancers
// that would otherwise reap an idle stream.
const heartbeatInterval = 25 * time.Second

// StreamHandler returns the HTTP handler for GET /events/stream. It must be
// mounted behind Authenticate (it reads the actor from the request context).
// The connection stays open until the client disconnects or the server shuts
// down; on either the client is unregistered, so no goroutine or channel leaks.
func StreamHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := auth.ActorFrom(r.Context())
		if !ok {
			httpx.Error(w, r, httpx.Unauthorized("authentication required"))
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			httpx.Error(w, r, httpx.Internal(fmt.Errorf("streaming unsupported by this server")))
			return
		}
		// Clear the server's per-request write deadline for this connection —
		// an SSE stream is long-lived and must outlive the global WriteTimeout.
		if rc := http.NewResponseController(w); rc != nil {
			_ = rc.SetWriteDeadline(time.Time{})
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
		w.WriteHeader(http.StatusOK)

		client := hub.Register(actor.PartyID, actor.RoleCode, actor.OrgUnitID)
		defer hub.Unregister(client.ID)

		// Opening frame so the client knows the stream is live and can render
		// its first badge value from its normal REST count call.
		writeSSE(w, "ready", map[string]any{"connected": true})
		flusher.Flush()

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()

		for {
			select {
			case <-r.Context().Done():
				return // client disconnected — defer Unregister cleans up
			case ev, open := <-client.Events():
				if !open {
					return // hub closed this client (server shutdown)
				}
				writeSSE(w, ev.Type, ev.Data)
				flusher.Flush()
			case <-heartbeat.C:
				// SSE comment line — ignored by clients, keeps the socket warm.
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}

// writeSSE encodes one SSE frame: an event name + a JSON data line.
func writeSSE(w http.ResponseWriter, event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte("{}")
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
}

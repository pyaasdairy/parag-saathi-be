// Package sse implements a Server-Sent Events hub for pushing live updates to
// dashboards that are already open — e.g. an Organising Manager's "pending KYC:
// 33" badge ticking up without a refresh.
//
// Why SSE, not WebSocket: a dashboard badge is a one-way, server→client signal.
// SSE runs over plain HTTP (no new dependency), auto-reconnects in the browser,
// and is far simpler than a WebSocket. It cannot talk to a browser any more
// directly than that — the whole point is the server pushing down an open HTTP
// connection.
//
// Delivery model — "nudge, then re-count": the hub pushes a lightweight event
// (e.g. "kyc.pending.changed"), and the client re-fetches the authoritative,
// org-scoped count from its normal REST endpoint. The nudge is never trusted as
// the number itself, so a slightly-broad nudge is harmless — the scoped count
// query is the source of truth and can never drift.
//
// Single-instance scope: this hub fans out within ONE process. When the API
// runs as multiple replicas, an event raised on replica B must reach a client
// connected to replica A — at that point WireToBus is re-pointed at a shared
// pub/sub (Redis pub/sub or NATS), and nothing else changes. That is the ONLY
// place multi-node real-time needs external infrastructure; the atomic
// duplicate-prevention guarantees live entirely in MongoDB and are unaffected.
package sse

import (
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

// Event is one message pushed to matching clients.
type Event struct {
	Type string `json:"type"` // e.g. "kyc.pending.changed"
	Data any    `json:"data,omitempty"`
	// Roles restricts delivery to clients holding one of these role codes.
	// Empty means "every connected client". Because clients re-fetch a scoped
	// count on receipt, role-level targeting is sufficient — over-delivery only
	// costs a harmless re-count, never a wrong number.
	Roles []string `json:"-"`
}

// Client is one connected dashboard. send is buffered and lossy by design:
// if a slow client's buffer is full we drop the nudge rather than block the
// hub — the next nudge (or the client's periodic poll) re-syncs it.
type Client struct {
	ID        string
	PartyID   string
	RoleCode  string
	OrgUnitID string
	send      chan Event
}

// Events is the channel the HTTP handler ranges over to write the stream.
func (c *Client) Events() <-chan Event { return c.send }

const clientBuffer = 16

// Hub tracks connected clients and fans events out to them.
type Hub struct {
	log *slog.Logger

	mu      sync.RWMutex
	clients map[string]*Client
}

// NewHub builds an empty hub.
func NewHub(log *slog.Logger) *Hub {
	return &Hub{log: log, clients: make(map[string]*Client)}
}

// Register adds a client and returns it. The caller (HTTP handler) must call
// Unregister when the connection closes.
func (h *Hub) Register(partyID, roleCode, orgUnitID string) *Client {
	c := &Client{
		ID:        uuid.NewString(),
		PartyID:   partyID,
		RoleCode:  roleCode,
		OrgUnitID: orgUnitID,
		send:      make(chan Event, clientBuffer),
	}
	h.mu.Lock()
	h.clients[c.ID] = c
	n := len(h.clients)
	h.mu.Unlock()
	h.log.Debug("sse client connected",
		slog.String("client_id", c.ID), slog.String("role", roleCode), slog.Int("connections", n))
	return c
}

// Unregister removes a client and closes its channel. Safe to call once.
func (h *Hub) Unregister(id string) {
	h.mu.Lock()
	c, ok := h.clients[id]
	if ok {
		delete(h.clients, id)
	}
	n := len(h.clients)
	h.mu.Unlock()
	if ok {
		close(c.send)
		h.log.Debug("sse client disconnected",
			slog.String("client_id", id), slog.Int("connections", n))
	}
}

// Broadcast delivers ev to every matching client with a non-blocking send.
func (h *Hub) Broadcast(ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	delivered, dropped := 0, 0
	for _, c := range h.clients {
		if !roleMatches(ev.Roles, c.RoleCode) {
			continue
		}
		select {
		case c.send <- ev:
			delivered++
		default:
			dropped++ // buffer full — client is slow; the next nudge re-syncs it
		}
	}
	if delivered > 0 || dropped > 0 {
		h.log.Debug("sse broadcast",
			slog.String("type", ev.Type), slog.Int("delivered", delivered), slog.Int("dropped", dropped))
	}
}

// ConnectionCount reports currently-connected clients (metrics/debugging).
func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func roleMatches(roles []string, role string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// Package push provides the Expo push transport behind the CRM's "push"
// channel (contract C5).
//
// Expo push model as used here: the consumer app mints an
// ExponentPushToken[...] with expo-notifications and hands it to
// POST /consumer/push/register; WE post the rendered message to Expo's push
// API, which relays it to FCM / APNs. This package is ONLY the pipe: it knows
// nothing about members, templates or the token registry. Expo answers one
// ticket per message, in order; a ticket whose error is DeviceNotRegistered
// means no device holds that token any more and the CALLER prunes it from
// its registry.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const expoPushEndpoint = "https://exp.host/--/api/v2/push/send"

// ErrUnavailable marks a send whose outcome is UNKNOWN: a transport error or
// a 5xx from the push service. Callers treat it as transient (retry later,
// never fall back to another channel, which could double-deliver).
var ErrUnavailable = errors.New("push: service unavailable")

// Message is one Expo push message. Field names are Expo's wire names.
type Message struct {
	To        string            `json:"to"`
	Title     string            `json:"title,omitempty"`
	Body      string            `json:"body"`
	Data      map[string]string `json:"data,omitempty"`
	ChannelID string            `json:"channelId,omitempty"` // Android channel the app registered
	Sound     string            `json:"sound,omitempty"`     // "default" or empty
	Priority  string            `json:"priority,omitempty"`  // default | normal | high
}

// Ticket is Expo's per-message answer, in the order the messages were sent.
type Ticket struct {
	OK      bool
	ID      string // Expo receipt id when OK
	Error   string // Expo error code otherwise: DeviceNotRegistered, MessageTooBig, ...
	Message string
}

// DeviceNotRegistered reports whether Expo says no device holds the token
// any more, so the caller can prune it.
func (t Ticket) DeviceNotRegistered() bool { return t.Error == "DeviceNotRegistered" }

// Expo is a minimal client for Expo's push API. Safe for concurrent use.
type Expo struct {
	enabled     bool
	accessToken string
	endpoint    string
	client      *http.Client
}

// New builds a client. enabled is EXPO_PUSH_ENABLED == "true" (the caller
// reads env); accessToken is the optional EXPO_ACCESS_TOKEN sent as a Bearer.
// When disabled, Send refuses and callers fall back to their existing
// behaviour (the in-app inbox).
func New(enabled bool, accessToken string) *Expo {
	return &Expo{
		enabled:     enabled,
		accessToken: strings.TrimSpace(accessToken),
		endpoint:    expoPushEndpoint,
		client:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether real push delivery is configured.
func (e *Expo) Enabled() bool { return e != nil && e.enabled }

// SetEndpoint points the client at another push service. A test seam only:
// production always talks to exp.host.
func (e *Expo) SetEndpoint(u string) { e.endpoint = u }

// Send posts msgs as one JSON array and returns one Ticket per message, in
// order. A nil error means the service ACCEPTED the batch; individual
// tickets may still be errors (a dead token is not a failed batch). Errors
// split two ways: a definitive rejection (4xx, a request-level error in the
// body, or not configured) lets the caller fall back; ErrUnavailable
// (network error, 5xx) is transient and must not.
func (e *Expo) Send(ctx context.Context, msgs []Message) ([]Ticket, error) {
	if !e.Enabled() {
		return nil, fmt.Errorf("push: not configured")
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("push: no messages")
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return nil, fmt.Errorf("push: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("push: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	if e.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.accessToken)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("push: request failed: %v: %w", err, ErrUnavailable)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("push: status %d: %s: %w", resp.StatusCode, rb, ErrUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("push: send rejected (status %d): %s", resp.StatusCode, rb)
	}
	var out struct {
		Data []struct {
			Status  string `json:"status"` // ok | error
			ID      string `json:"id"`
			Message string `json:"message"`
			Details struct {
				Error string `json:"error"`
			} `json:"details"`
		} `json:"data"`
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("push: unreadable answer: %s", rb)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("push: send rejected: %s %s", out.Errors[0].Code, out.Errors[0].Message)
	}
	tickets := make([]Ticket, 0, len(out.Data))
	for _, d := range out.Data {
		tickets = append(tickets, Ticket{OK: d.Status == "ok", ID: d.ID, Error: d.Details.Error, Message: d.Message})
	}
	return tickets, nil
}

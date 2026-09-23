package push

// The Expo pipe: posts one JSON array in Expo's wire shape, carries the
// access token as a Bearer, hands back one ticket per message so the caller
// can prune a dead token, and tells a transient failure from a definitive one.
//
//	go test ./internal/platform/push/ -v

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func TestExpoDisabled(t *testing.T) {
	var nilClient *Expo
	if nilClient.Enabled() || New(false, "x").Enabled() {
		t.Fatal("nil or disabled client must report disabled")
	}
	if !New(true, "").Enabled() {
		t.Fatal("enabled client must report enabled")
	}
	if _, err := New(false, "").Send(context.Background(), []Message{{To: "t", Body: "b"}}); err == nil {
		t.Fatal("a disabled client must refuse to send")
	}
	if _, err := New(true, "").Send(context.Background(), nil); err == nil {
		t.Fatal("an empty batch must be refused")
	}
}

func TestExpoSendShapeAndTickets(t *testing.T) {
	var gotAuth, gotCT string
	var gotBody []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("content-type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"data":[{"status":"ok","id":"r1"},{"status":"error","message":"gone","details":{"error":"DeviceNotRegistered"}}]}`))
	}))
	defer srv.Close()
	e := New(true, " tok ")
	e.SetEndpoint(srv.URL)
	tickets, err := e.Send(context.Background(), []Message{
		{To: "ExponentPushToken[a]", Title: "PYAAS", Body: "hi", Data: map[string]string{"href": "/inbox"}, ChannelID: "orders", Sound: "default", Priority: "high"},
		{To: "ExponentPushToken[b]", Body: "hi"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotAuth != "Bearer tok" || !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("headers: auth=%q ct=%q", gotAuth, gotCT)
	}
	if len(gotBody) != 2 {
		t.Fatalf("want a 2-message array, got %v", gotBody)
	}
	keys := make([]string, 0, len(gotBody[0]))
	for k := range gotBody[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "body,channelId,data,priority,sound,title,to" {
		t.Fatalf("wire keys: %v", keys)
	}
	if len(gotBody[1]) != 2 { // optional fields are omitted, not sent empty
		t.Fatalf("bare message must carry only to+body, got %v", gotBody[1])
	}
	if len(tickets) != 2 || !tickets[0].OK || tickets[0].ID != "r1" || tickets[1].OK || !tickets[1].DeviceNotRegistered() || tickets[0].DeviceNotRegistered() {
		t.Fatalf("tickets: %+v", tickets)
	}
}

func TestExpoTransientVersusDefinitive(t *testing.T) {
	status := 503
	body := `{}`
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(status)
		rw.Write([]byte(body))
	}))
	defer srv.Close()
	e := New(true, "")
	e.SetEndpoint(srv.URL)
	msgs := []Message{{To: "ExponentPushToken[a]", Body: "hi"}}
	if _, err := e.Send(context.Background(), msgs); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("5xx must be ErrUnavailable, got %v", err)
	}
	status, body = 400, `{"errors":[{"code":"PUSH_TOO_MANY_EXPERIENCE_IDS","message":"bad"}]}`
	if _, err := e.Send(context.Background(), msgs); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("4xx must be a definitive rejection, got %v", err)
	}
	status, body = 200, `{"errors":[{"code":"PUSH_TOO_MANY_NOTIFICATIONS","message":"bad"}]}`
	if _, err := e.Send(context.Background(), msgs); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("a request-level error body must be a definitive rejection, got %v", err)
	}
	srv.Close()
	if _, err := e.Send(context.Background(), msgs); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a dead network must be ErrUnavailable, got %v", err)
	}
	// Bearer only when a token is set.
	var auth string
	plain := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		rw.Write([]byte(`{"data":[{"status":"ok","id":"r"}]}`))
	}))
	defer plain.Close()
	e.SetEndpoint(plain.URL)
	if _, err := e.Send(context.Background(), msgs); err != nil || auth != "" {
		t.Fatalf("no token must mean no Authorization header: err=%v auth=%q", err, auth)
	}
}

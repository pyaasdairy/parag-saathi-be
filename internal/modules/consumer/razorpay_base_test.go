package consumer

// Every Razorpay call goes through one REST root (rzpAPIBase): the real
// gateway by default, a stub when RAZORPAY_API_BASE_URL (or a test) says so.
// The path, method, auth and body are what production sends.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
)

func TestRzpBaseURLOverride(t *testing.T) {
	t.Setenv("RAZORPAY_API_BASE_URL", "")
	if got := rzpBaseFromEnv(); got != "" {
		t.Fatalf("unset override: %q, want empty (the real gateway)", got)
	}
	s := &service{}
	if got := s.rzpAPIBase(); got != "https://api.razorpay.com/v1" {
		t.Fatalf("default base %q", got)
	}
	t.Setenv("RAZORPAY_API_BASE_URL", "http://127.0.0.1:9009/")
	if got := rzpBaseFromEnv(); got != "http://127.0.0.1:9009/v1" {
		t.Fatalf("override base %q, want the origin + /v1", got)
	}
}

func TestRzpOrderGoesThroughTheBase(t *testing.T) {
	var gotPath, gotUser, gotPass string
	var body map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"order_stub_1"}`))
	}))
	defer gw.Close()
	s := &service{deps: &deps.Deps{Cfg: &config.Config{}}, rzpKeyID: "rzp_test_key", rzpKeySecret: "sec", rzpBase: gw.URL + "/v1"}
	id, err := s.createRzpOrder(context.Background(), 50000, "wtu_x")
	if err != nil || id != "order_stub_1" {
		t.Fatalf("createRzpOrder: %q %v", id, err)
	}
	if gotPath != "POST /v1/orders" || gotUser != "rzp_test_key" || gotPass != "sec" {
		t.Fatalf("request %q as %q/%q", gotPath, gotUser, gotPass)
	}
	if body["amount"] != float64(50000) || body["currency"] != "INR" || body["receipt"] != "wtu_x" {
		t.Fatalf("order body %v", body)
	}

	// A refusal is a refusal, an unreachable gateway is unknown: the status
	// tells them apart for a caller that must not read a timeout as "no".
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"description":"amount exceeds maximum amount allowed"}}`))
	}))
	defer refuse.Close()
	s.rzpBase = refuse.URL
	status, err := s.rzpCall(context.Background(), http.MethodPost, "/orders", map[string]any{"amount": 1}, nil)
	if status != http.StatusBadRequest || err == nil || rzpErrorText([]byte(`{"error":{"description":"x"}}`)) != "x" {
		t.Fatalf("refusal: %d %v", status, err)
	}
	s.rzpBase = "http://127.0.0.1:1" // nothing listens there
	if status, err := s.rzpCall(context.Background(), http.MethodGet, "/payments/pay_1", nil, nil); status != 0 || err == nil {
		t.Fatalf("unreachable: %d %v", status, err)
	}
	// No key secret: nothing is sent at all.
	s.rzpKeySecret = ""
	if status, err := s.rzpCall(context.Background(), http.MethodGet, "/payments/pay_1", nil, nil); status != 0 || err == nil {
		t.Fatalf("keyless call must not be sent: %d %v", status, err)
	}
}

package sms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// CRM_MSG91_BASE_URL is the dry-run seam: it replaces the origin and keeps the
// OTP path, so a local stub sees the exact request production sends. Unset,
// the client talks to the real endpoint.
func TestMSG91BaseURLOverrideKeepsOTPPath(t *testing.T) {
	t.Setenv("CRM_MSG91_BASE_URL", "")
	if got := NewMSG91("k", "tpl").endpoint; got != "https://control.msg91.com/api/v5/otp" {
		t.Fatalf("endpoint with the key unset: %q", got)
	}

	var gotMethod, gotPath, gotAuth string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth, gotQuery = r.Method, r.URL.Path, r.Header.Get("authkey"), r.URL.Query()
		_, _ = w.Write([]byte(`{"type":"success","request_id":"dry-run"}`))
	}))
	defer srv.Close()
	t.Setenv("CRM_MSG91_BASE_URL", srv.URL+"/")
	m := NewMSG91("dry-run-key", "1207160000000000002")
	if !m.Enabled() {
		t.Fatal("client must be enabled")
	}
	if err := m.SendOTP(context.Background(), "+91 98765 43210", "123456"); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v5/otp" || gotAuth != "dry-run-key" {
		t.Fatalf("stub saw %s %s (authkey %q)", gotMethod, gotPath, gotAuth)
	}
	if gotQuery.Get("template_id") != "1207160000000000002" || gotQuery.Get("mobile") != "919876543210" ||
		gotQuery.Get("otp") != "123456" || gotQuery.Get("otp_length") != "6" {
		t.Fatalf("query: %v", gotQuery)
	}
}

// A provider "error" with HTTP 200 is still a failed send.
func TestMSG91RejectsErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"type":"error","message":"invalid template"}`))
	}))
	defer srv.Close()
	t.Setenv("CRM_MSG91_BASE_URL", srv.URL)
	if err := NewMSG91("k", "tpl").SendOTP(context.Background(), "9876543210", "123456"); err == nil {
		t.Fatal("an error body must fail the send")
	}
}

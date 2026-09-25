package consumer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Razorpay wallet-top-up seam. The key SECRET lives ONLY here (server-side, from
// env) — never in the client. Two responsibilities (per the FE razorpay.ts
// security note): (1) CREATE the order so the amount is bound server-side, and
// (2) VERIFY the payment signature before crediting the ledger.
//
// Env: RAZORPAY_KEY_ID + RAZORPAY_KEY_SECRET. When the secret is absent we run a
// DEV seam (gated by OTP dev mode) that mints a local pseudo order id and signs
// with a fixed dev secret, so the whole recharge flow is exercisable offline
// without moving real money. In production the secret is set and the dev seam is
// unreachable.
const (
	// razorpayAPIOrigin is Razorpay's REST host. RAZORPAY_API_BASE_URL replaces
	// it (scheme://host, like the CRM_*_BASE_URL stubs): the /v1 path, method,
	// auth and body stay what production sends, so one local stub captures
	// every gateway call a dry run or an E2E makes. Unset = the real gateway.
	razorpayAPIOrigin = "https://api.razorpay.com"
	// devRazorpaySecret signs top-ups when no real key secret is configured. It
	// is ONLY trusted while OTP dev mode is on (never in production).
	devRazorpaySecret = "rzp_dev_secret_v1"
)

// rzpAPIBase is the REST root EVERY Razorpay call goes through (orders, the
// recurring charge, payment and token lookups, the reconciliation sweep):
// the configured origin + /v1, or a test's httptest URL set on the service.
func (s *service) rzpAPIBase() string {
	if s.rzpBase != "" {
		return s.rzpBase
	}
	return razorpayAPIBase
}

// rzpBaseFromEnv is the base newService stores: RAZORPAY_API_BASE_URL's origin
// + /v1 when set, else "" (rzpAPIBase then answers the real gateway).
func rzpBaseFromEnv() string {
	if o := crmProviderOrigin("RAZORPAY_API_BASE_URL", ""); o != "" {
		return o + "/v1"
	}
	return ""
}

// rzpCall is one authenticated JSON call to the gateway: body (nil for a
// GET) is marshalled, the answer decoded into out when it is a 2xx. status is
// the HTTP status (0 when the request never got an answer: a transport error
// or a timeout, whose outcome is UNKNOWN - the caller must not read it as a
// refusal). Keys come only from config; with no key secret nothing is sent.
func (s *service) rzpCall(ctx context.Context, method, path string, body, out any) (status int, err error) {
	if s.rzpKeySecret == "" {
		return 0, errInternal("payments are not configured")
	}
	var rd io.Reader
	if body != nil {
		raw, merr := json.Marshal(body)
		if merr != nil {
			return 0, errInternal("gateway request build failed")
		}
		rd = bytes.NewReader(raw)
	}
	req, rerr := http.NewRequestWithContext(ctx, method, s.rzpAPIBase()+path, rd)
	if rerr != nil {
		return 0, errInternal("gateway request build failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(s.rzpKeyID, s.rzpKeySecret)
	client := &http.Client{Timeout: 12 * time.Second}
	resp, derr := client.Do(req)
	if derr != nil {
		return 0, errInternal("payment gateway unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, errInternal(fmt.Sprintf("payment gateway answered %d: %s", resp.StatusCode, rzpErrorText(raw)))
	}
	if out != nil {
		if uerr := json.Unmarshal(raw, out); uerr != nil {
			return resp.StatusCode, errInternal("payment gateway response invalid")
		}
	}
	return resp.StatusCode, nil
}

// rzpErrorText is the gateway's own error description, for logs and a
// refused charge's reason ({"error":{"description":...}}), else "".
func rzpErrorText(raw []byte) string {
	var e struct {
		Error struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		return e.Error.Description
	}
	return ""
}

// rzpDevMode reports the offline dev seam is in effect (no real key secret AND
// OTP dev mode on). Any other combination requires a real secret to verify.
func (s *service) rzpDevMode() bool {
	return s.rzpKeySecret == "" && s.deps.Cfg.OTPDevMode
}

// signingSecret is the secret used to verify payment signatures.
func (s *service) signingSecret() (string, bool) {
	if s.rzpKeySecret != "" {
		return s.rzpKeySecret, true
	}
	if s.deps.Cfg.OTPDevMode {
		return devRazorpaySecret, true
	}
	return "", false // no way to verify — fail closed
}

// createRzpOrder returns an amount-bound order id. Real Razorpay order when a
// key secret is configured; a local pseudo id in the dev seam.
func (s *service) createRzpOrder(ctx context.Context, amountPaise int64, receipt string) (string, error) {
	if s.rzpKeySecret == "" {
		if !s.deps.Cfg.OTPDevMode {
			return "", errInternal("payments are not configured")
		}
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", errInternal("order id generation failed")
		}
		return "order_dev_" + hex.EncodeToString(b), nil
	}
	var out struct {
		ID string `json:"id"`
	}
	status, err := s.rzpCall(ctx, http.MethodPost, "/orders", map[string]any{"amount": amountPaise, "currency": "INR", "receipt": receipt}, &out)
	if err != nil {
		switch {
		case status == 0:
			return "", errInternal("payment gateway unreachable")
		case status < 300:
			return "", errInternal("payment gateway response invalid")
		}
		return "", errInternal(fmt.Sprintf("payment gateway rejected order (%d)", status))
	}
	if out.ID == "" {
		return "", errInternal("payment gateway response invalid")
	}
	return out.ID, nil
}

// verifyRzpSignature checks Razorpay's HMAC: hex(HMAC_SHA256(order|payment,
// secret)) — constant-time. Returns false (never errors) so a bad signature is
// an ordinary "not verified", not a 500.
func (s *service) verifyRzpSignature(orderID, paymentID, signature string) bool {
	secret, ok := s.signingSecret()
	if !ok || orderID == "" || paymentID == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(orderID + "|" + paymentID))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

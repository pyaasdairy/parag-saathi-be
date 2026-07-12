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
	razorpayOrdersURL = "https://api.razorpay.com/v1/orders"
	// devRazorpaySecret signs top-ups when no real key secret is configured. It
	// is ONLY trusted while OTP dev mode is on (never in production).
	devRazorpaySecret = "rzp_dev_secret_v1"
)

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
	body, _ := json.Marshal(map[string]any{"amount": amountPaise, "currency": "INR", "receipt": receipt})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, razorpayOrdersURL, bytes.NewReader(body))
	if err != nil {
		return "", errInternal("order request build failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.rzpKeyID, s.rzpKeySecret)
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", errInternal("payment gateway unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", errInternal(fmt.Sprintf("payment gateway rejected order (%d)", resp.StatusCode))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
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

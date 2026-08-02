// Package sms provides the transactional SMS transport for OTP delivery.
//
// MSG91 OTP model as used here: WE generate the 6-digit code and verify it
// ourselves (the existing HMAC challenge + attempt/expiry logic is untouched).
// MSG91 is ONLY the SMS pipe — we call its /otp endpoint with `otp=<our code>`
// so it renders the DLT-approved template and delivers it. We never rely on
// MSG91's own verify endpoint, so our brute-force/expiry guarantees still hold.
package sms

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const msg91OTPEndpoint = "https://control.msg91.com/api/v5/otp"

var nonDigits = regexp.MustCompile(`\D`)

// MSG91 is a minimal client for MSG91's OTP SMS API. Safe for concurrent use.
type MSG91 struct {
	authKey    string
	templateID string
	client     *http.Client
}

// NewMSG91 builds a client from the auth key + DLT template id (both from env).
// When either is empty the client is disabled (Enabled() == false) and callers
// fall back to their existing behaviour (dev echo / queued outbox).
func NewMSG91(authKey, templateID string) *MSG91 {
	return &MSG91{
		authKey:    strings.TrimSpace(authKey),
		templateID: strings.TrimSpace(templateID),
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether real SMS delivery is configured.
func (m *MSG91) Enabled() bool {
	return m != nil && m.authKey != "" && m.templateID != ""
}

// SendOTP asks MSG91 to SMS a specific `code` to an Indian mobile. `phone` may
// be any format; we reduce it to the last 10 digits and prefix the 91 country
// code. Returns an error on transport failure or a non-success MSG91 response
// so the caller can tell the user "couldn't send, try again" instead of leaving
// them waiting for an SMS that never comes.
func (m *MSG91) SendOTP(ctx context.Context, phone, code string) error {
	if !m.Enabled() {
		return fmt.Errorf("msg91: not configured")
	}
	digits := nonDigits.ReplaceAllString(phone, "")
	if len(digits) < 10 {
		return fmt.Errorf("msg91: invalid phone %q", phone)
	}
	mobile := "91" + digits[len(digits)-10:]

	q := url.Values{}
	q.Set("template_id", m.templateID)
	q.Set("mobile", mobile)
	q.Set("otp", code)
	q.Set("otp_length", "6")
	endpoint := msg91OTPEndpoint + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("authkey", m.authKey)
	req.Header.Set("accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("msg91: send request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	// MSG91 returns {"type":"success",...} on success and {"type":"error",...}
	// (often with HTTP 200) otherwise, so we check the body, not just the status.
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"success"`) {
		return fmt.Errorf("msg91: send rejected (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

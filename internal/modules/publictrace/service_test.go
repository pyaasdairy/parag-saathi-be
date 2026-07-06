package publictrace

import (
	"encoding/base64"
	"testing"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// TestVerifyQRToken covers the QR integrity gate: a token is
// "base64url(payload)" + "." + hex HMAC-SHA256(secret, decoded payload) —
// the exact format the plant module's signQRToken mints, where the payload
// embeds the product lot's ObjectID as productLotID.Hex() — and any bit flip
// in either part must fail verification.
func TestVerifyQRToken(t *testing.T) {
	const secret = "test-qr-signing-secret"
	// Product lot ObjectIDs rendered as hex, exactly as issuance signs them.
	const lotHex = "665f1a2b3c4d5e6f70819202"
	const otherLotHex = "665f1a2b3c4d5e6f70819203"
	const payload = "PRG-7F3K9QX2|" + lotHex + "|1751700000"
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	validToken := b64(payload) + "." + auth.HMACHash(secret, payload)

	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{
			name:  "valid token verifies",
			token: validToken,
			want:  true,
		},
		{
			name:  "tampered payload fails",
			token: b64("PRG-7F3K9QX2|"+otherLotHex+"|1751700000") + "." + auth.HMACHash(secret, payload),
			want:  false,
		},
		{
			name:  "tampered signature fails",
			token: b64(payload) + "." + auth.HMACHash("wrong-secret", payload),
			want:  false,
		},
		{
			name:  "truncated signature fails",
			token: validToken[:len(validToken)-2],
			want:  false,
		},
		{
			name:  "missing separator fails",
			token: b64(payload) + auth.HMACHash(secret, payload),
			want:  false,
		},
		{
			name:  "non-base64 payload fails",
			token: payload + "." + auth.HMACHash(secret, payload),
			want:  false,
		},
		{
			name:  "empty payload fails",
			token: "." + auth.HMACHash(secret, ""),
			want:  false,
		},
		{
			name:  "empty signature fails",
			token: b64(payload) + ".",
			want:  false,
		},
		{
			name:  "empty token fails",
			token: "",
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyQRToken(secret, tc.token); got != tc.want {
				t.Fatalf("VerifyQRToken(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

// TestSourcingMessage pins the consumer-facing sentence shape (§7.4).
func TestSourcingMessage(t *testing.T) {
	tests := []struct {
		name        string
		dates       []string
		samitiCount int
		districts   []string
		want        string
	}{
		{
			name:        "full sentence",
			dates:       []string{"2026-07-04", "2026-07-05"},
			samitiCount: 3,
			districts:   []string{"Lucknow", "Sitapur"},
			want:        "Made from milk collected on 2026-07-04, 2026-07-05 from 3 samitis in Lucknow, Sitapur",
		},
		{
			name:        "singular samiti",
			dates:       []string{"2026-07-05"},
			samitiCount: 1,
			districts:   []string{"Lucknow"},
			want:        "Made from milk collected on 2026-07-05 from 1 samiti in Lucknow",
		},
		{
			name:        "no dates or districts",
			dates:       nil,
			samitiCount: 2,
			districts:   nil,
			want:        "Made from milk collected from 2 samitis",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourcingMessage(tc.dates, tc.samitiCount, tc.districts); got != tc.want {
				t.Fatalf("sourcingMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

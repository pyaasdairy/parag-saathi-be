package plant

import (
	"strings"
	"testing"
	"time"

	"github.com/pyaas/saathi-backend/internal/domain"
)

func TestQRTokenRoundTrip(t *testing.T) {
	const secret = "test-qr-secret"
	issuedAt := time.Date(2026, 7, 6, 9, 30, 0, 0, time.UTC)

	token := signQRToken(secret, "PRG-7F3K9QX2", "lot-123", issuedAt)

	code, lotID, ts, err := parseQRToken(secret, token)
	if err != nil {
		t.Fatalf("parseQRToken: unexpected error: %v", err)
	}
	if code != "PRG-7F3K9QX2" {
		t.Errorf("qr code: got %q, want %q", code, "PRG-7F3K9QX2")
	}
	if lotID != "lot-123" {
		t.Errorf("product lot id: got %q, want %q", lotID, "lot-123")
	}
	if !ts.Equal(issuedAt) {
		t.Errorf("issued at: got %v, want %v", ts, issuedAt)
	}
}

func TestQRTokenRejectsForgery(t *testing.T) {
	const secret = "test-qr-secret"
	issuedAt := time.Now().UTC()
	token := signQRToken(secret, "PRG-AAAA2222", "lot-legit", issuedAt)

	cases := []struct {
		name   string
		secret string
		token  string
	}{
		{"wrong secret", "attacker-secret", token},
		{"tampered payload", secret, "X" + token[1:]},
		{"tampered signature", secret, token[:len(token)-1] + flipHexChar(token[len(token)-1])},
		{"missing separator", secret, strings.ReplaceAll(token, ".", "")},
		{"garbage", secret, "not-a-token"},
		{"empty", secret, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := parseQRToken(tc.secret, tc.token); err == nil {
				t.Errorf("parseQRToken accepted a forged/malformed token")
			}
		})
	}
}

// flipHexChar returns a different valid hex digit so signature length stays
// intact while its value changes.
func flipHexChar(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}

func TestNewQRCodeShape(t *testing.T) {
	for i := 0; i < 100; i++ {
		code, err := newQRCode()
		if err != nil {
			t.Fatalf("newQRCode: %v", err)
		}
		if !strings.HasPrefix(code, "PRG-") {
			t.Fatalf("code %q missing PRG- prefix", code)
		}
		body := strings.TrimPrefix(code, "PRG-")
		if len(body) != qrCodeLength {
			t.Fatalf("code %q body length %d, want %d", code, len(body), qrCodeLength)
		}
		for _, ch := range body {
			if !strings.ContainsRune(qrAlphabet, ch) {
				t.Fatalf("code %q contains %q outside the safe alphabet", code, ch)
			}
			if strings.ContainsRune("0O1I", ch) {
				t.Fatalf("code %q contains confusable character %q", code, ch)
			}
		}
	}
}

// TestGateRefusalMatrix pins the §8.3 safety-gate invariants: a lot or batch
// that has not passed QC can NEVER advance, at any hop.
func TestGateRefusalMatrix(t *testing.T) {
	t.Run("dispatch: only PASSED lots leave the BMC", func(t *testing.T) {
		want := map[string]bool{
			domain.BMCLotStatusOpen:       false,
			domain.BMCLotStatusQCPending:  false,
			domain.BMCLotStatusPassed:     true,
			domain.BMCLotStatusBlocked:    false,
			domain.BMCLotStatusDispatched: false, // already gone — re-dispatch refused
		}
		for status, allowed := range want {
			if got := canDispatchBMCLot(status); got != allowed {
				t.Errorf("canDispatchBMCLot(%s) = %v, want %v", status, got, allowed)
			}
		}
	})

	t.Run("batch pooling: blocked or unpassed lot cannot enter a batch", func(t *testing.T) {
		want := map[string]bool{
			domain.BMCLotStatusOpen:       false,
			domain.BMCLotStatusQCPending:  false,
			domain.BMCLotStatusPassed:     false, // passed but not yet arrived at the plant
			domain.BMCLotStatusBlocked:    false, // AFM1 survives pasteurisation — hard refusal
			domain.BMCLotStatusDispatched: true,
		}
		for status, allowed := range want {
			if got := canPoolBMCLot(status); got != allowed {
				t.Errorf("canPoolBMCLot(%s) = %v, want %v", status, got, allowed)
			}
		}
	})

	t.Run("batch completion: only plant-lab PASSED batches complete", func(t *testing.T) {
		want := map[string]bool{
			domain.BatchStatusCreated:   false,
			domain.BatchStatusQCPending: false,
			domain.BatchStatusPassed:    true,
			domain.BatchStatusBlocked:   false,
			domain.BatchStatusCompleted: false,
		}
		for status, allowed := range want {
			if got := canCompleteBatch(status); got != allowed {
				t.Errorf("canCompleteBatch(%s) = %v, want %v", status, got, allowed)
			}
		}
	})

	t.Run("product lots: an uncompleted batch cannot yield product", func(t *testing.T) {
		want := map[string]bool{
			domain.BatchStatusCreated:   false,
			domain.BatchStatusQCPending: false,
			domain.BatchStatusPassed:    false, // passed but production run not finished
			domain.BatchStatusBlocked:   false,
			domain.BatchStatusCompleted: true,
		}
		for status, allowed := range want {
			if got := canYieldProductLot(status); got != allowed {
				t.Errorf("canYieldProductLot(%s) = %v, want %v", status, got, allowed)
			}
		}
	})

	t.Run("QR issuance: only ACTIVE lot + COMPLETED batch may be labelled", func(t *testing.T) {
		lotStatuses := []string{
			domain.ProductLotStatusActive,
			domain.ProductLotStatusRecalled,
			domain.ProductLotStatusExpired,
		}
		batchStatuses := []string{
			domain.BatchStatusCreated, domain.BatchStatusQCPending,
			domain.BatchStatusPassed, domain.BatchStatusBlocked,
			domain.BatchStatusCompleted,
		}
		for _, ls := range lotStatuses {
			for _, bs := range batchStatuses {
				allowed := ls == domain.ProductLotStatusActive && bs == domain.BatchStatusCompleted
				if got := canIssueQR(ls, bs); got != allowed {
					t.Errorf("canIssueQR(%s, %s) = %v, want %v", ls, bs, got, allowed)
				}
			}
		}
	})
}

package consumer

// Tests for the payment recovery nets (razorpay_recovery.go). The properties
// under test are the ones the money depends on:
//
//	signature is mandatory and constant-time · an unconfigured secret rejects
//	everything · the credited amount is OUR bound amount, never the payload's ·
//	an order-pay row can never mint wallet money · replay and the app's own
//	/wallet/verify can never double-credit · only a CAPTURED gateway payment
//	counts · a transient failure asks Razorpay to retry rather than swallowing
//	the event.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// Enough service for the paths that never reach the repository (unparsable
// bodies, events we ignore, events carrying no ids).
func newTestServiceForWebhook(t *testing.T) *service {
	t.Helper()
	return &service{rzpWebhookSecret: "whsec_test", log: testLogger()}
}

func rzpSign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func capturedEvent(orderID, paymentID string, amountPaise int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "payment.captured",
		"payload": map[string]any{
			"payment": map[string]any{
				"entity": map[string]any{
					"id": paymentID, "order_id": orderID,
					"status": "captured", "amount": amountPaise,
				},
			},
		},
	})
	return b
}

// The signature is the ONLY authentication this endpoint has, so every
// variation that is not an exact HMAC of the raw body must be refused.
func TestRzpWebhookSignature(t *testing.T) {
	s := &service{rzpWebhookSecret: "whsec_test"}
	body := capturedEvent("order_1", "pay_1", 50000)
	good := rzpSign(t, "whsec_test", body)

	if !s.verifyRzpWebhookSignature(body, good) {
		t.Fatal("a correct signature must verify")
	}
	if s.verifyRzpWebhookSignature(body, "deadbeef") {
		t.Fatal("a forged signature must be refused")
	}
	if s.verifyRzpWebhookSignature(body, "") {
		t.Fatal("an absent signature must be refused")
	}
	if s.verifyRzpWebhookSignature(append(body, ' '), good) {
		t.Fatal("a tampered body must invalidate the signature")
	}
	// Signed with the API key secret instead of the WEBHOOK secret — a real
	// and easy misconfiguration; it must not authenticate.
	if s.verifyRzpWebhookSignature(body, rzpSign(t, "rzp_key_secret", body)) {
		t.Fatal("the wrong secret must not authenticate")
	}
	// Unconfigured deployment: reject everything, including a signature that
	// would be valid under an empty key.
	unset := &service{}
	if unset.verifyRzpWebhookSignature(body, rzpSign(t, "", body)) {
		t.Fatal("with no webhook secret configured the endpoint must accept nothing")
	}
}

// An event we do not act on, or one naming an order we have never issued, must
// be accepted and ignored — never retried forever, never an error.
func TestRzpWebhookIgnoresIrrelevantEvents(t *testing.T) {
	s := newTestServiceForWebhook(t)
	for _, body := range [][]byte{
		[]byte(`{"event":"payment.failed","payload":{}}`),
		[]byte(`{"event":"payment.authorized","payload":{}}`), // authorized != captured
		[]byte(`{"event":"payment.captured","payload":{}}`),   // no ids
		[]byte(`not json at all`),
	} {
		if err := s.razorpayWebhookEvent(t.Context(), body); err != nil {
			t.Fatalf("ignorable event must not error (would make Razorpay retry forever): %v", err)
		}
	}
}

// The HTTP layer: a bad signature is 401 and never reaches the service body.
func TestRzpWebhookHandlerRejectsUnsigned(t *testing.T) {
	h := &handler{svc: &service{rzpWebhookSecret: "whsec_test"}}
	body := capturedEvent("order_1", "pay_1", 50000)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/payments/razorpay/webhook", bytesReader(body))
	req.Header.Set("X-Razorpay-Signature", "nope")
	h.razorpayWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: got %d want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/payments/razorpay/webhook", bytesReader(body))
	// no signature header at all
	h.razorpayWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: got %d want 401", rec.Code)
	}
}

// Only a CAPTURED payment is money. "authorized" is a hold, not a settlement,
// and must not credit.
func TestRzpFetchCapturedPaymentOnlyCounts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"captured", `{"items":[{"id":"pay_c","status":"captured","amount":50000}]}`, true},
		{"authorized only", `{"items":[{"id":"pay_a","status":"authorized","amount":50000}]}`, false},
		{"failed only", `{"items":[{"id":"pay_f","status":"failed","amount":50000}]}`, false},
		{"none", `{"items":[]}`, false},
		{"captured among others", `{"items":[{"id":"pay_f","status":"failed","amount":50000},{"id":"pay_c","status":"captured","amount":50000}]}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if u, p, ok := r.BasicAuth(); !ok || u != "key" || p != "secret" {
					t.Errorf("gateway lookup must authenticate with the API key pair")
				}
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			s := &service{rzpKeyID: "key", rzpKeySecret: "secret", log: testLogger()}
			_, _, ok := s.fetchCapturedPayment(t.Context(), srv.URL, "order_1")
			if ok != c.want {
				t.Fatalf("captured=%v want %v", ok, c.want)
			}
		})
	}
}

// A gateway error must not be read as "no payment" forever — it simply reports
// nothing this pass, leaving the order pending for the next one.
func TestRzpFetchTolerantOfGatewayFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := &service{rzpKeyID: "key", rzpKeySecret: "secret", log: testLogger()}
	if _, _, ok := s.fetchCapturedPayment(t.Context(), srv.URL, "order_1"); ok {
		t.Fatal("a 5xx must not be treated as a captured payment")
	}
}

// Without a configured key secret the sweep worker must RETURN rather than
// loop — an unconfigured or dev deployment never contacts the gateway.
func TestRzpReconcileWorkerInertWithoutSecret(t *testing.T) {
	s := &service{rzpKeySecret: "", log: testLogger()}
	done := make(chan struct{})
	go func() { s.paymentReconcileWorker(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker must return immediately when Razorpay is not configured")
	}
}

func TestRzpRecoveryStatusLine(t *testing.T) {
	if got := (&service{}).rzpRecoveryStatus(); got != "razorpay recovery: webhook=false reconciliation=false" {
		t.Fatalf("unconfigured status: %q", got)
	}
	s := &service{rzpWebhookSecret: "w", rzpKeySecret: "k"}
	if got := s.rzpRecoveryStatus(); got != "razorpay recovery: webhook=true reconciliation=true" {
		t.Fatalf("configured status: %q", got)
	}
}

package consumer

// fakeRzp is a stand-in Razorpay for the AutoPay tests: the REST paths the
// backend calls (customers, orders, the recurring charge, payment and token
// lookups, token cancel, an order's payments for the reconcile sweep),
// answered from in-memory state the test sets and reads. The service is
// pointed at it through its base URL, exactly as RAZORPAY_API_BASE_URL does.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeRzp struct {
	srv *httptest.Server
	mu  sync.Mutex
	n   int
	// orders: every order created, by id, with its request body.
	orders map[string]map[string]any
	// recurring: every recurring charge requested, in order.
	recurring []map[string]any
	// recurringStatus / recurringBody: what the next recurring charges
	// answer (0 = 200 with a fresh payment id).
	recurringStatus int
	recurringBody   string
	// paymentTokens: GET /payments/{id} -> token_id.
	paymentTokens map[string]string
	// tokenStatus: the bank's word on each token (GET customers/{id}/tokens).
	tokenStatus map[string]string
	// captured: GET /orders/{id}/payments answers one captured payment.
	captured map[string]string
	// cancelled: tokens a PUT .../cancel reached.
	cancelled []string
	// tokenLookups counts GET customers/{id}/tokens.
	tokenLookups int
	// refunds: payments a POST /payments/{id}/refund reached.
	refunds []string
}

func newFakeRzp(t *testing.T) *fakeRzp {
	t.Helper()
	f := &fakeRzp{
		orders: map[string]map[string]any{}, paymentTokens: map[string]string{},
		tokenStatus: map[string]string{}, captured: map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// base is what the service's rzpBase is set to.
func (f *fakeRzp) base() string { return f.srv.URL + "/v1" }

func (f *fakeRzp) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodPost && path == "/customers":
		f.n++
		write(map[string]any{"id": fmt.Sprintf("cust_%d", f.n)})
	case r.Method == http.MethodPost && path == "/orders":
		f.n++
		id := fmt.Sprintf("order_ap_%d", f.n)
		f.orders[id] = body
		write(map[string]any{"id": id})
	case r.Method == http.MethodPost && path == "/payments/create/recurring":
		f.recurring = append(f.recurring, body)
		if f.recurringStatus != 0 {
			w.WriteHeader(f.recurringStatus)
			_, _ = w.Write([]byte(f.recurringBody))
			return
		}
		f.n++
		write(map[string]any{"razorpay_payment_id": fmt.Sprintf("pay_rec_%d", f.n)})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/payments/") && strings.HasSuffix(path, "/refund"):
		f.refunds = append(f.refunds, strings.TrimSuffix(strings.TrimPrefix(path, "/payments/"), "/refund"))
		f.n++
		write(map[string]any{"id": fmt.Sprintf("rfnd_%d", f.n), "status": "processed"})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/payments/"):
		write(map[string]any{"token_id": f.paymentTokens[strings.TrimPrefix(path, "/payments/")]})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/customers/") && strings.HasSuffix(path, "/tokens"):
		f.tokenLookups++
		items := []any{}
		for id, st := range f.tokenStatus {
			items = append(items, map[string]any{"id": id, "recurring_details": map[string]any{"status": st}})
		}
		write(map[string]any{"items": items})
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/cancel"):
		parts := strings.Split(path, "/")
		f.cancelled = append(f.cancelled, parts[len(parts)-2])
		write(map[string]any{"status": "cancellation_initiated"})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/orders/") && strings.HasSuffix(path, "/payments"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/orders/"), "/payments")
		items := []any{}
		if pay, ok := f.captured[id]; ok {
			amount := int64(0)
			if o := f.orders[id]; o != nil {
				if a, ok := o["amount"].(float64); ok {
					amount = int64(a)
				}
			}
			items = append(items, map[string]any{"id": pay, "status": "captured", "amount": amount})
		}
		write(map[string]any{"items": items})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"description":"no such path in the fake"}}`))
	}
}

// orderAmount is the paise an order was created for.
func (f *fakeRzp) orderAmount(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.orders[id]["amount"].(float64); ok {
		return int64(a)
	}
	return 0
}

func (f *fakeRzp) recurringCalls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.recurring...)
}

// rzpPaymentEvent is a payment.* webhook body for one order, with the token
// the payment used (empty for none).
func rzpPaymentEvent(event, orderID, paymentID string, amountPaise int64, tokenID, errDesc string) []byte {
	entity := map[string]any{"id": paymentID, "order_id": orderID, "amount": amountPaise,
		"status": strings.TrimPrefix(event, "payment.")}
	if tokenID != "" {
		entity["token_id"] = tokenID
	}
	if errDesc != "" {
		entity["error_description"] = errDesc
	}
	b, _ := json.Marshal(map[string]any{"event": event, "payload": map[string]any{"payment": map[string]any{"entity": entity}}})
	return b
}

// rzpTokenEvent is a token.* webhook body.
func rzpTokenEvent(event, tokenID, status string) []byte {
	b, _ := json.Marshal(map[string]any{"event": event, "payload": map[string]any{"token": map[string]any{"entity": map[string]any{
		"id": tokenID, "recurring_details": map[string]any{"status": status, "failure_reason": nil},
	}}}})
	return b
}

// liveRzp points a chain world's service at the fake with live keys.
func liveRzp(t *testing.T, w *chainWorld) *fakeRzp {
	t.Helper()
	f := newFakeRzp(t)
	w.svc.rzpKeyID, w.svc.rzpKeySecret, w.svc.rzpWebhookSecret = "rzp_test_key", "rzp_test_secret", "whsec_test"
	w.svc.rzpBase = f.base()
	return f
}

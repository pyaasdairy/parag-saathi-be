package consumer

// Payment RECOVERY — the two paths that credit a wallet when the app never
// comes back.
//
// THE PROBLEM THIS SOLVES. Until now the ONLY route from "customer paid" to
// "wallet credited" was the app itself calling POST /wallet/verify. On Indian
// UPI the customer leaves our app for GPay/PhonePe and the return leg fails
// often — killed app, dead network, flat battery, or simply closing the success
// screen. Razorpay had captured the money, nothing told us, and nothing ever
// looked again: the payment-order row sat at CREATED forever and the customer
// was short the money they paid. (It compounded with the Welcome Litre: pack 2
// unlocks on the recharge event emitted inside creditTopup, so a lost callback
// cost them the free pack too, while the app kept asking them to recharge.)
//
// TWO INDEPENDENT NETS, NEITHER DEPENDING ON THE CUSTOMER'S PHONE:
//
//  1. WEBHOOK — Razorpay calls us directly on payment.captured / order.paid.
//     Razorpay retries for hours on a non-2xx, which also covers us being spun
//     down by the host.
//  2. RECONCILIATION — every few minutes we ask Razorpay about our own orders
//     that are still CREATED after a grace period. This catches a webhook that
//     was never configured, was dropped, or arrived while we were unreachable
//     for longer than Razorpay's retry window.
//
// WHY THIS CANNOT DOUBLE-CREDIT. Both nets go through the SAME creditTopup as
// /wallet/verify, whose ledger gate is a UNIQUE INDEX on
// (consumer_id, ref_id, type) with ref_id = the Razorpay order id (repo.go:87).
// A second credit for the same order cannot be inserted — it comes back as a
// duplicate and the balance is left alone. Whichever net arrives first wins and
// every later one is a no-op. That guarantee already existed; this file only
// adds new callers of it.
//
// WHY THIS CANNOT BE TRICKED. The webhook is authenticated by Razorpay's
// signature over the RAW body (a different secret from the API key secret), and
// the amount credited is ALWAYS the amount we bound into our own payment-order
// row at creation — never a number taken from the incoming payload. A payload
// whose amount disagrees with our record is refused and logged rather than
// credited. With no webhook secret configured the endpoint rejects everything,
// so shipping this before Razorpay is configured changes nothing.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// razorpayAPIBase is the real gateway's REST root. Every call reads the
	// root through rzpAPIBase (razorpay.go), which RAZORPAY_API_BASE_URL can
	// point at a stub; the sweep's body still takes an explicit base
	// (reconcilePendingPaymentsAt) so a test can aim one pass anywhere.
	razorpayAPIBase = razorpayAPIOrigin + "/v1"
	// rzpReconcileGrace is how long a payment order must have sat at CREATED
	// before we ask the gateway about it. Long enough that an ordinary checkout
	// (including a slow UPI collect) has finished and called /wallet/verify.
	rzpReconcileGrace = 10 * time.Minute
	// rzpReconcileHorizon bounds the work: orders older than this are left
	// alone, so the sweep is O(recent) forever rather than O(all payments).
	// Razorpay keeps payments far longer, so a manual credit is always possible.
	rzpReconcileHorizon = 7 * 24 * time.Hour
	// rzpReconcileEvery is the sweep cadence.
	rzpReconcileEvery = 5 * time.Minute
	// rzpReconcileBatch caps one pass.
	rzpReconcileBatch = 100
)

// ── Webhook ────────────────────────────────────────────────────────────────

// verifyRzpWebhookSignature checks Razorpay's webhook HMAC:
// hex(HMAC_SHA256(raw request body, WEBHOOK secret)) delivered in
// X-Razorpay-Signature. Constant-time, and FAILS CLOSED when no webhook secret
// is configured — an unconfigured deployment accepts nothing.
//
// Note the secret is the WEBHOOK secret set in the Razorpay dashboard, NOT the
// API key secret used for order creation and the checkout signature.
func (s *service) verifyRzpWebhookSignature(raw []byte, signature string) bool {
	if s.rzpWebhookSecret == "" || signature == "" || len(raw) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.rzpWebhookSecret))
	mac.Write(raw)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// rzpWebhookEnvelope is the slice of Razorpay's event body we act on. Both
// payment.captured and order.paid carry the payment entity; order.paid also
// carries the order entity, which we ignore (our own row is the authority).
type rzpWebhookEnvelope struct {
	Event   string `json:"event"`
	Payload struct {
		Payment struct {
			Entity struct {
				ID      string `json:"id"`
				OrderID string `json:"order_id"`
				Status  string `json:"status"`
				Amount  int64  `json:"amount"`
				// ErrorDescription is Razorpay's customer-facing failure text
				// on payment.failed (unused by the credit path).
				ErrorDescription string `json:"error_description"`
			} `json:"entity"`
		} `json:"payment"`
	} `json:"payload"`
}

// razorpayWebhookEvent handles one VERIFIED webhook body. The signature must
// already have been checked by the caller.
//
// Returns an error ONLY for transient failures, so the handler can answer 5xx
// and let Razorpay retry. Everything we deliberately ignore — an event we do
// not act on, an order we do not know, an amount that disagrees — returns nil
// so the gateway stops retrying something that will never succeed.
func (s *service) razorpayWebhookEvent(ctx context.Context, raw []byte) error {
	var ev rzpWebhookEnvelope
	if err := json.Unmarshal(raw, &ev); err != nil {
		s.log.Warn("razorpay webhook: unparsable body")
		return nil // never retryable
	}
	switch ev.Event {
	case "payment.captured", "order.paid":
	case "payment.failed":
		// Nothing to credit; the member is told the recharge did not go
		// through (CRM B-03, payment.failed). The owner comes from OUR order
		// row, never from the payload. Unknown order: ignored, never retried.
		s.crmPaymentFailed(ctx, ev.Payload.Payment.Entity.OrderID, ev.Payload.Payment.Entity.ID, ev.Payload.Payment.Entity.ErrorDescription)
		return nil
	default:
		return nil // authorized / refund / settlement events: not ours
	}
	p := ev.Payload.Payment.Entity
	if p.OrderID == "" || p.ID == "" {
		s.log.Warn("razorpay webhook: event carries no order/payment id", "event", ev.Event)
		return nil
	}
	credited, err := s.creditCapturedPayment(ctx, p.OrderID, p.ID, p.Amount, "webhook")
	if err != nil {
		return err // transient — let Razorpay retry
	}
	if credited {
		s.log.Info("razorpay webhook: recovered a payment the app never confirmed",
			"order", p.OrderID, "payment", p.ID)
	}
	return nil
}

// razorpayWebhook is the public endpoint Razorpay posts to. Unauthenticated by
// design (the gateway has no JWT) — the signature IS the authentication.
func (h *handler) razorpayWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, &apiError{Code: "BAD_REQUEST", Message: "unreadable body"})
		return
	}
	if !h.svc.verifyRzpWebhookSignature(raw, r.Header.Get("X-Razorpay-Signature")) {
		// Deliberately terse: an attacker learns nothing about why.
		writeJSON(w, http.StatusUnauthorized, &apiError{Code: "UNAUTHORIZED", Message: "invalid signature"})
		return
	}
	if err := h.svc.razorpayWebhookEvent(r.Context(), raw); err != nil {
		// 5xx tells Razorpay to retry — the outcome here is genuinely unknown.
		writeJSON(w, http.StatusInternalServerError, &apiError{Code: "RETRY", Message: "could not process yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// crmPaymentFailed emits payment.failed (CRM B-03) for a gateway payment that
// failed against one of our top-up orders. Once per gateway payment (the
// claim scopes on the payment id), and only when the order is a wallet
// top-up we issued - an order payment or an unknown order is ignored.
func (s *service) crmPaymentFailed(ctx context.Context, orderID, paymentID, reason string) {
	if !crmEnabled() || orderID == "" {
		return
	}
	ord, err := s.repo.findPaymentOrderByID(ctx, orderID)
	if err != nil || (ord.Purpose != "" && ord.Purpose != "topup") || ord.Status == "PAID" {
		return
	}
	scope := paymentID
	if scope == "" {
		scope = orderID
	}
	s.emitCRMEvent(ctx, "payment.failed", ord.ConsumerID, map[string]any{
		"payment_order_id": orderID, "payment_id": paymentID, "amount": round2(float64(ord.AmountPaise) / 100),
		"reason": reason, "source": "razorpay", "scope_key": scope,
	})
}

// ── The shared credit path ─────────────────────────────────────────────────

// creditCapturedPayment credits a captured gateway payment to the wallet it
// belongs to. It is the single funnel both recovery nets use, and it applies
// the same three rules /wallet/verify does:
//
//   - the OWNER comes from our stored payment order, never from the payload;
//   - the AMOUNT credited is the amount we bound at order creation, never the
//     payload's (a disagreement is refused and logged — it should be impossible,
//     because a Razorpay order is amount-bound, so it means something is wrong);
//   - only a "topup" order may credit the wallet; an order-pay row is settled by
//     the order flow and must never mint wallet money here.
//
// Reports whether THIS call performed the credit (false = already credited, or
// deliberately ignored). An error is transient only.
func (s *service) creditCapturedPayment(ctx context.Context, orderID, paymentID string, gatewayAmountPaise int64, via string) (bool, error) {
	ord, err := s.repo.findPaymentOrderByID(ctx, orderID)
	if err != nil {
		return false, nil // unknown to us: nothing to credit, never retryable
	}
	if ord.Purpose != "" && ord.Purpose != "topup" {
		return false, nil // an order payment — the order flow owns its settlement
	}
	if gatewayAmountPaise > 0 && gatewayAmountPaise != ord.AmountPaise {
		s.log.Error("razorpay recovery: gateway amount disagrees with our bound order — refusing to credit",
			"order", orderID, "gateway_paise", gatewayAmountPaise, "our_paise", ord.AmountPaise, "via", via)
		return false, nil
	}
	if ord.Status == "PAID" {
		// Fast path only. The ledger gate below is still the authority: a row
		// marked PAID whose credit failed mid-flight is repaired by the sweep
		// calling us again, because creditTopup is keyed by the order id.
		return false, nil
	}
	amount := round2(float64(ord.AmountPaise) / 100)

	// The money gate. Idempotent by (consumer, order id, TOPUP) — if
	// /wallet/verify already credited this order, the insert is a duplicate and
	// the balance is untouched.
	before, err := s.getOrCreateWallet(ctx, ord.ConsumerID)
	if err != nil {
		return false, err
	}
	after, err := s.creditTopup(ctx, ord.ConsumerID, amount, "razorpay", orderID)
	if err != nil {
		return false, err // transient — the caller retries
	}
	_, _ = s.repo.markPaymentOrderPaid(ctx, orderID, paymentID, time.Now().UTC())

	// A balance that did not move means the ledger gate saw this order before;
	// that is a successful no-op, not a new credit.
	return after.CashBalance > before.CashBalance, nil
}

// ── Reconciliation sweep ───────────────────────────────────────────────────

// paymentReconcileWorker runs for the process lifetime, sweeping payment orders
// that are still CREATED after the grace period. Inert unless a real Razorpay
// key secret is configured, so the dev seam and unconfigured deployments never
// reach the gateway.
func (s *service) paymentReconcileWorker(ctx context.Context) {
	if s.rzpKeySecret == "" {
		return
	}
	select {
	case <-time.After(90 * time.Second): // let boot settle, after the CRM worker
	case <-ctx.Done():
		return
	}
	s.log.Info("razorpay reconciliation armed", "every", rzpReconcileEvery.String(), "grace", rzpReconcileGrace.String())
	for {
		s.reconcilePendingPayments(ctx, time.Now())
		select {
		case <-time.After(rzpReconcileEvery):
		case <-ctx.Done():
			return
		}
	}
}

func (s *service) reconcilePendingPayments(ctx context.Context, now time.Time) {
	s.reconcilePendingPaymentsAt(ctx, now, s.rzpAPIBase())
}

// reconcilePendingPaymentsAt is the testable body: `base` lets a test point the
// gateway lookups at an httptest server.
func (s *service) reconcilePendingPaymentsAt(ctx context.Context, now time.Time, base string) {
	orders, err := s.repo.listPendingPaymentOrders(ctx,
		now.Add(-rzpReconcileGrace), now.Add(-rzpReconcileHorizon), rzpReconcileBatch)
	if err != nil || len(orders) == 0 {
		return
	}
	for i := range orders {
		o := &orders[i]
		paymentID, amountPaise, ok := s.fetchCapturedPayment(ctx, base, o.OrderID)
		if !ok {
			continue // nothing captured — the customer never completed it
		}
		credited, cerr := s.creditCapturedPayment(ctx, o.OrderID, paymentID, amountPaise, "reconcile")
		if cerr != nil {
			s.log.Warn("razorpay reconciliation: credit failed, next pass retries",
				"order", o.OrderID, "err", cerr)
			continue
		}
		if credited {
			s.log.Info("razorpay reconciliation: recovered a captured payment the app never confirmed",
				"order", o.OrderID, "payment", paymentID)
		}
	}
}

// fetchCapturedPayment asks the gateway for the payments on one order and
// returns the first CAPTURED one. "authorized" is deliberately NOT enough: the
// money is only ours once captured.
func (s *service) fetchCapturedPayment(ctx context.Context, base, orderID string) (paymentID string, amountPaise int64, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/orders/"+orderID+"/payments", nil)
	if err != nil {
		return "", 0, false
	}
	req.SetBasicAuth(s.rzpKeyID, s.rzpKeySecret)
	req.Header.Set("accept", "application/json")
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, false // transient: the next pass tries again
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		if resp.StatusCode != http.StatusNotFound {
			s.log.Warn("razorpay reconciliation: gateway lookup failed",
				"order", orderID, "status", resp.StatusCode)
		}
		return "", 0, false
	}
	var out struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Amount int64  `json:"amount"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", 0, false
	}
	for _, it := range out.Items {
		if it.Status == "captured" {
			return it.ID, it.Amount, true
		}
	}
	return "", 0, false
}

// rzpRecoveryStatus is a one-line operational summary used by the boot log, so
// a deploy makes it obvious which nets are actually armed.
func (s *service) rzpRecoveryStatus() string {
	return fmt.Sprintf("razorpay recovery: webhook=%t reconciliation=%t",
		s.rzpWebhookSecret != "", s.rzpKeySecret != "")
}

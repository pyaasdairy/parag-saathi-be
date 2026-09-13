package consumer

// Mongo-backed proof for the payment recovery nets. These are the tests that
// justify the claim "this cannot double-credit and cannot be tricked", because
// the guarantee lives in a UNIQUE INDEX, not in Go code — so it can only be
// demonstrated against a real database.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run RzpRecoveryMongo -v

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pyaas/saathi-backend/internal/config"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/eventbus"
	"github.com/pyaas/saathi-backend/internal/platform/flags"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func rzpTestService(t *testing.T) (*service, func()) {
	t.Helper()
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the payment-recovery integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		cancel()
		t.Fatalf("mongo connect: %v", err)
	}
	db := client.Database("consumer_rzp_recovery_test")
	_ = db.Drop(ctx)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &deps.Deps{
		Cfg: &config.Config{JWTSecret: "rzp-recovery-test"},
		Log: log, DB: db,
		Flags: flags.NewService(db),
		Bus:   eventbus.New(log),
	}
	repo := newRepository(db)
	if err := repo.ensureIndexes(ctx); err != nil {
		cancel()
		t.Fatalf("ensure indexes (the money gate lives here): %v", err)
	}
	svc := newService(d, repo, log)
	svc.rzpKeyID, svc.rzpKeySecret, svc.rzpWebhookSecret = "key", "secret", "whsec_test"
	return svc, func() { _ = db.Drop(ctx); _ = client.Disconnect(ctx); cancel() }
}

// seedPaymentOrder stages a CREATED order exactly as createTopupOrder would.
func seedPaymentOrder(t *testing.T, s *service, orderID string, paise int64, purpose string, age time.Duration) primitive.ObjectID {
	t.Helper()
	consumerID := primitive.NewObjectID()
	err := s.repo.insertPaymentOrder(context.Background(), &paymentOrder{
		ID: primitive.NewObjectID(), OrderID: orderID, ConsumerID: consumerID,
		AmountPaise: paise, Receipt: "wtu_" + consumerID.Hex(), Purpose: purpose,
		Status: "CREATED", CreatedAt: time.Now().UTC().Add(-age),
	})
	if err != nil {
		t.Fatalf("seed payment order: %v", err)
	}
	return consumerID
}

func cash(t *testing.T, s *service, id primitive.ObjectID) float64 {
	t.Helper()
	w, err := s.getOrCreateWallet(context.Background(), id)
	if err != nil {
		t.Fatalf("wallet: %v", err)
	}
	return w.CashBalance
}

// THE CORE PROPERTY: a captured payment the app never confirmed is credited,
// and no amount of replay can credit it twice.
func TestRzpRecoveryMongoWebhookCreditsOnceAndOnlyOnce(t *testing.T) {
	s, done := rzpTestService(t)
	defer done()
	ctx := context.Background()

	cid := seedPaymentOrder(t, s, "order_webhook_1", 50000, "topup", 0)
	if got := cash(t, s, cid); got != 0 {
		t.Fatalf("precondition: wallet should start empty, got %v", got)
	}

	body := capturedEvent("order_webhook_1", "pay_1", 50000)
	if err := s.razorpayWebhookEvent(ctx, body); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if got := cash(t, s, cid); got != 500 {
		t.Fatalf("after webhook: cash %v want 500", got)
	}

	// Replay the SAME event four times — Razorpay genuinely does retry.
	for i := 0; i < 4; i++ {
		if err := s.razorpayWebhookEvent(ctx, body); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	if got := cash(t, s, cid); got != 500 {
		t.Fatalf("REPLAY DOUBLE-CREDITED: cash %v want 500", got)
	}

	ord, err := s.repo.findPaymentOrderByID(ctx, "order_webhook_1")
	if err != nil || ord.Status != "PAID" || ord.PaymentID != "pay_1" {
		t.Fatalf("order should be marked PAID with its payment id: %+v (%v)", ord, err)
	}
}

// The app's own /wallet/verify and the webhook racing the same order must
// produce ONE credit, in either order of arrival.
func TestRzpRecoveryMongoNeverDoubleCreditsWithVerify(t *testing.T) {
	s, done := rzpTestService(t)
	defer done()
	ctx := context.Background()

	// (a) app credits first, webhook arrives later
	cid := seedPaymentOrder(t, s, "order_race_a", 50000, "topup", 0)
	if _, err := s.creditTopup(ctx, cid, 500, "razorpay", "order_race_a"); err != nil {
		t.Fatalf("app credit: %v", err)
	}
	if err := s.razorpayWebhookEvent(ctx, capturedEvent("order_race_a", "pay_a", 50000)); err != nil {
		t.Fatalf("webhook after app: %v", err)
	}
	if got := cash(t, s, cid); got != 500 {
		t.Fatalf("app-then-webhook: cash %v want 500", got)
	}

	// (b) webhook first, app's verify-credit later
	cid2 := seedPaymentOrder(t, s, "order_race_b", 50000, "topup", 0)
	if err := s.razorpayWebhookEvent(ctx, capturedEvent("order_race_b", "pay_b", 50000)); err != nil {
		t.Fatalf("webhook first: %v", err)
	}
	if _, err := s.creditTopup(ctx, cid2, 500, "razorpay", "order_race_b"); err != nil {
		t.Fatalf("app credit after webhook: %v", err)
	}
	if got := cash(t, s, cid2); got != 500 {
		t.Fatalf("webhook-then-app: cash %v want 500", got)
	}
}

// The amount credited is OUR bound amount. A payload claiming a different
// figure is refused outright rather than credited at either value.
func TestRzpRecoveryMongoRefusesAmountMismatch(t *testing.T) {
	s, done := rzpTestService(t)
	defer done()
	cid := seedPaymentOrder(t, s, "order_mismatch", 50000, "topup", 0)

	// A payload inflating the amount...
	if err := s.razorpayWebhookEvent(context.Background(), capturedEvent("order_mismatch", "pay_x", 9900000)); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if got := cash(t, s, cid); got != 0 {
		t.Fatalf("INFLATED AMOUNT WAS CREDITED: cash %v want 0", got)
	}
	// ...and one deflating it. Neither may move money.
	if err := s.razorpayWebhookEvent(context.Background(), capturedEvent("order_mismatch", "pay_y", 100)); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if got := cash(t, s, cid); got != 0 {
		t.Fatalf("MISMATCHED AMOUNT WAS CREDITED: cash %v want 0", got)
	}
}

// An order-pay row must never mint wallet money through the recovery path —
// the same rule /wallet/verify enforces.
func TestRzpRecoveryMongoRefusesOrderPurpose(t *testing.T) {
	s, done := rzpTestService(t)
	defer done()
	cid := seedPaymentOrder(t, s, "order_purpose", 50000, "order", 0)

	if err := s.razorpayWebhookEvent(context.Background(), capturedEvent("order_purpose", "pay_o", 50000)); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if got := cash(t, s, cid); got != 0 {
		t.Fatalf("an order-pay row credited the WALLET: cash %v want 0", got)
	}
}

// An event for an order we never issued is ignored, not an error and not a
// credit (nothing to credit it to).
func TestRzpRecoveryMongoIgnoresUnknownOrder(t *testing.T) {
	s, done := rzpTestService(t)
	defer done()
	if err := s.razorpayWebhookEvent(context.Background(), capturedEvent("order_never_issued", "pay_z", 50000)); err != nil {
		t.Fatalf("unknown order must be ignored, not retried forever: %v", err)
	}
}

// THE SECOND NET: with no webhook at all, the sweep finds the captured payment
// and credits it — and leaves alone an order the customer never completed.
func TestRzpRecoveryMongoReconcileSweep(t *testing.T) {
	s, done := rzpTestService(t)
	defer done()
	ctx := context.Background()

	paid := seedPaymentOrder(t, s, "order_sweep_paid", 50000, "topup", 30*time.Minute)
	unpaid := seedPaymentOrder(t, s, "order_sweep_unpaid", 20000, "topup", 30*time.Minute)
	fresh := seedPaymentOrder(t, s, "order_sweep_fresh", 50000, "topup", 0) // inside the grace

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orders/order_sweep_paid/payments":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
				map[string]any{"id": "pay_swept", "status": "captured", "amount": 50000}}})
		case r.URL.Path == "/orders/order_sweep_fresh/payments":
			t.Error("an order still inside the grace window must NOT be swept")
			w.Write([]byte(`{"items":[]}`))
		default:
			w.Write([]byte(`{"items":[]}`)) // nothing captured
		}
	}))
	defer gw.Close()

	s.reconcilePendingPaymentsAt(ctx, time.Now(), gw.URL)

	if got := cash(t, s, paid); got != 500 {
		t.Fatalf("swept captured payment: cash %v want 500", got)
	}
	if got := cash(t, s, unpaid); got != 0 {
		t.Fatalf("an uncompleted checkout must never be credited: cash %v", got)
	}
	if got := cash(t, s, fresh); got != 0 {
		t.Fatalf("order inside the grace window credited: cash %v", got)
	}

	// Running the sweep again must not credit a second time.
	s.reconcilePendingPaymentsAt(ctx, time.Now(), gw.URL)
	if got := cash(t, s, paid); got != 500 {
		t.Fatalf("SECOND SWEEP DOUBLE-CREDITED: cash %v want 500", got)
	}
}

// Webhook and sweep both recovering the same payment must still credit once.
func TestRzpRecoveryMongoBothNetsOneCredit(t *testing.T) {
	s, done := rzpTestService(t)
	defer done()
	ctx := context.Background()
	cid := seedPaymentOrder(t, s, "order_both", 50000, "topup", 30*time.Minute)

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"id": "pay_both", "status": "captured", "amount": 50000}}})
	}))
	defer gw.Close()

	if err := s.razorpayWebhookEvent(ctx, capturedEvent("order_both", "pay_both", 50000)); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	s.reconcilePendingPaymentsAt(ctx, time.Now(), gw.URL)
	s.reconcilePendingPaymentsAt(ctx, time.Now(), gw.URL)

	if got := cash(t, s, cid); got != 500 {
		t.Fatalf("both nets credited twice: cash %v want 500", got)
	}
}

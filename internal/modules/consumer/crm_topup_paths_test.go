package consumer

// The owner's complaint: "after money is added to the wallet there is a
// template but it is not being invoked correctly". The template is B-06 on
// wallet.credited (T-B05-REFUND for the refundable account, T-B05-PROMO for
// Pyaas credit). This file drives EVERY path that credits a wallet through
// its real entry point, drains the real worker, and pins what the member
// reads: exactly one inbox row per credit, the rendered amount, no leftover
// [TOKEN], and no second message when two recovery nets see one payment.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMTopup -v

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// crmB06Rows is every B-06 inbox row a consumer holds, EN and HI bodies.
func crmB06Rows(t *testing.T, w *chainWorld, cid primitive.ObjectID) []struct{ EN, HI, Template string } {
	t.Helper()
	cur, err := w.db.Collection(collConsumerInbox).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "B-06"}})
	if err != nil {
		t.Fatalf("inbox read: %v", err)
	}
	var rows []struct {
		EN       string `bson:"body_en"`
		HI       string `bson:"body_hi"`
		Template string `bson:"template_id"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	out := make([]struct{ EN, HI, Template string }, 0, len(rows))
	for _, r := range rows {
		out = append(out, struct{ EN, HI, Template string }{r.EN, r.HI, r.Template})
	}
	return out
}

// crmExpectOneB06 drains the worker and asserts the consumer holds exactly
// one B-06 message, rendered from the wanted template with no token left.
func crmExpectOneB06(t *testing.T, w *chainWorld, cid primitive.ObjectID, path, template, wantEN string) {
	t.Helper()
	w.svc.crmProcessEvents(context.Background())
	rows := crmB06Rows(t, w, cid)
	if len(rows) != 1 {
		t.Fatalf("%s: B-06 inbox rows = %d, want exactly 1: %+v", path, len(rows), rows)
	}
	r := rows[0]
	if r.Template != template {
		t.Fatalf("%s: template %s, want %s", path, r.Template, template)
	}
	if !strings.Contains(r.EN, wantEN) {
		t.Fatalf("%s: body %q, want it to contain %q", path, r.EN, wantEN)
	}
	for _, body := range []string{r.EN, r.HI} {
		if m := crmTokenRe.FindString(body); m != "" {
			t.Fatalf("%s: unresolved token %s in %q", path, m, body)
		}
	}
}

// crmExpectNoValidityClaim (PM-02): Pyaas credit (the REWARDS bucket) never
// expires - nothing in the backend lapses it - so no B-06 row may tell the
// member it is "Valid 30 days" ("30 din valid"). The referral reward was the
// first production path to render that promise to both families.
func crmExpectNoValidityClaim(t *testing.T, path string, rows []struct{ EN, HI, Template string }) {
	t.Helper()
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.EN), "valid") || strings.Contains(strings.ToLower(r.HI), "valid") {
			t.Fatalf("%s: the message promises a validity nothing enforces: %q / %q", path, r.EN, r.HI)
		}
	}
}

func rzpTestSignature(secret, orderID, paymentID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(orderID + "|" + paymentID))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestCRMTopupMessageEveryCreditPath(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	w.svc.rzpKeyID, w.svc.rzpKeySecret, w.svc.rzpWebhookSecret = "key", "secret", "whsec_test"

	seedOrder := func(cid primitive.ObjectID, orderID string, paise int64, age time.Duration) {
		t.Helper()
		if err := w.svc.repo.insertPaymentOrder(ctx, &paymentOrder{
			ID: primitive.NewObjectID(), OrderID: orderID, ConsumerID: cid, AmountPaise: paise,
			Receipt: "wtu_" + cid.Hex(), Purpose: "topup", Status: "CREATED",
			CreatedAt: time.Now().UTC().Add(-age),
		}); err != nil {
			t.Fatalf("seed payment order: %v", err)
		}
	}
	const recharge500 = "₹500 added to your Wallet (recharge). Refundable. Ready for your next order."

	// 1) The app's own confirmation (POST /wallet/verify), then the webhook
	//    and the reconcile sweep both see the SAME payment afterwards.
	a := w.customer(t, "9000008101", 0)
	seedOrder(a, "order_verify_1", 50000, 30*time.Minute)
	v, err := w.svc.verifyPayment(ctx, a, "pay_v1", "order_verify_1", rzpTestSignature("secret", "order_verify_1", "pay_v1"))
	if err != nil || !v.Verified {
		t.Fatalf("verifyPayment: %+v %v", v, err)
	}
	if err := w.svc.razorpayWebhookEvent(ctx, capturedEvent("order_verify_1", "pay_v1", 50000)); err != nil {
		t.Fatalf("webhook after verify: %v", err)
	}
	gw := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/orders/"), "/payments")
		_ = json.NewEncoder(rw).Encode(map[string]any{"items": []any{
			map[string]any{"id": "pay_for_" + id, "status": "captured", "amount": 50000}}})
	}))
	defer gw.Close()
	w.svc.reconcilePendingPaymentsAt(ctx, time.Now(), gw.URL)
	crmExpectOneB06(t, w, a, "verify+webhook+sweep", "T-B05-REFUND", recharge500)
	if got := w.cash(t, a); got != 500 {
		t.Fatalf("verify path cash = %v, want 500", got)
	}

	// 2) The webhook recovers a payment the app never confirmed, then the
	//    app's late verify arrives for the same payment.
	b := w.customer(t, "9000008102", 0)
	seedOrder(b, "order_webhook_1", 50000, 0)
	if err := w.svc.razorpayWebhookEvent(ctx, capturedEvent("order_webhook_1", "pay_w1", 50000)); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if err := w.svc.razorpayWebhookEvent(ctx, capturedEvent("order_webhook_1", "pay_w1", 50000)); err != nil {
		t.Fatalf("webhook retry: %v", err)
	}
	if v, err := w.svc.verifyPayment(ctx, b, "pay_w1", "order_webhook_1", rzpTestSignature("secret", "order_webhook_1", "pay_w1")); err != nil || !v.Verified {
		t.Fatalf("late verify: %+v %v", v, err)
	}
	crmExpectOneB06(t, w, b, "webhook+retry+verify", "T-B05-REFUND", recharge500)

	// 2b) The app's verify and the webhook racing each other on one payment:
	//     the ledger gate admits one credit, so one message.
	r := w.customer(t, "9000008110", 0)
	seedOrder(r, "order_race_1", 50000, 0)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = w.svc.verifyPayment(ctx, r, "pay_r1", "order_race_1", rzpTestSignature("secret", "order_race_1", "pay_r1"))
		}()
		go func() {
			defer wg.Done()
			_ = w.svc.razorpayWebhookEvent(ctx, capturedEvent("order_race_1", "pay_r1", 50000))
		}()
	}
	wg.Wait()
	crmExpectOneB06(t, w, r, "verify racing webhook", "T-B05-REFUND", recharge500)
	if got := w.cash(t, r); got != 500 {
		t.Fatalf("race cash = %v, want 500", got)
	}

	// 3) The reconcile sweep is the only net that sees the payment; a second
	//    pass finds nothing new.
	c := w.customer(t, "9000008103", 0)
	seedOrder(c, "order_sweep_1", 50000, 30*time.Minute)
	w.svc.reconcilePendingPaymentsAt(ctx, time.Now(), gw.URL)
	w.svc.reconcilePendingPaymentsAt(ctx, time.Now(), gw.URL)
	crmExpectOneB06(t, w, c, "reconcile sweep", "T-B05-REFUND", recharge500)

	// 4) The dev test top-up (POST /wallet/topup, OTP dev mode only).
	d := w.customer(t, "9000008104", 0)
	if _, err := w.svc.topup(ctx, d, 250, "test", "test_ref_1"); err != nil {
		t.Fatalf("dev topup: %v", err)
	}
	crmExpectOneB06(t, w, d, "dev topup", "T-B05-REFUND", "₹250 added to your Wallet (recharge). Refundable.")

	// 5) Promo credit: the non-refundable wording, never "Refundable".
	e := w.customer(t, "9000008105", 0)
	if _, err := w.svc.promoCredit(ctx, e, 75, "promo_1", "Welcome credit"); err != nil {
		t.Fatalf("promoCredit: %v", err)
	}
	if _, err := w.svc.promoCredit(ctx, e, 75, "promo_1", "Welcome credit"); err != nil { // replay
		t.Fatalf("promoCredit replay: %v", err)
	}
	crmExpectOneB06(t, w, e, "promo credit", "T-B05-PROMO", "₹75 Pyaas credit added (Welcome credit). Usable on orders; not refundable in cash.")
	crmExpectNoValidityClaim(t, "promo credit", crmB06Rows(t, w, e))

	// 6) Refund.
	f := w.customer(t, "9000008106", 0)
	if _, err := w.svc.refund(ctx, f, 35, "rf_1", "one pack short"); err != nil {
		t.Fatalf("refund: %v", err)
	}
	crmExpectOneB06(t, w, f, "refund", "T-B05-REFUND", "₹35 added to your Wallet (one pack short). Refundable.")

	// 7) The rider's 15-minute undo hands the delivery debit back.
	g := w.customer(t, "9000008107", 1000)
	w.svc.crmProcessEvents(ctx) // the funding top-up's own receipt
	o := instantOrderDelivered(t, w, g)
	var task delivery
	if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&task); err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := w.svc.riderReverseDeliveryDebit(ctx, &task, time.Now().UTC()); err != nil {
		t.Fatalf("riderReverseDeliveryDebit: %v", err)
	}
	var back walletTxn
	if err := w.db.Collection(collWalletTxns).FindOne(ctx, bson.D{
		{Key: "consumer_id", Value: g}, {Key: "type", Value: "REFUND"}, {Key: "ref_type", Value: "delivery_undo"},
	}).Decode(&back); err != nil {
		t.Fatalf("undo ledger row: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	undoWant := "₹" + crmRupees(back.Amount) + " added to your Wallet (delivery " + strings.ToUpper(task.OrderCode[len(task.OrderCode)-6:]) + " reversed). Refundable."
	var undoRows int
	for _, r := range crmB06Rows(t, w, g) {
		if strings.Contains(r.EN, "reversed") {
			undoRows++
			if !strings.Contains(r.EN, undoWant) {
				t.Fatalf("undo body %q, want %q", r.EN, undoWant)
			}
		}
	}
	if undoRows != 1 {
		t.Fatalf("rider undo: %d B-06 rows mention the reversal, want 1", undoRows)
	}

	// 8) Mandate execution DEBITS the wallet (subscription auto-renewal); it
	//    is never a credit, so it must never say money was added.
	h := w.customer(t, "9000008108", 500)
	w.svc.crmProcessEvents(ctx)
	before := len(crmB06Rows(t, w, h))
	m := &mandate{
		ID: primitive.NewObjectID(), MandateID: "mnd_topup_paths", ConsumerID: h, Plan: "daily", Status: "active",
		Amount: 100, MaxAmount: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := w.svc.repo.insertMandate(ctx, m); err != nil {
		t.Fatalf("insertMandate: %v", err)
	}
	if _, err := w.svc.runMandateCharge(ctx, h, m.MandateID, time.Now()); err != nil {
		t.Fatalf("runMandateCharge: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	if after := len(crmB06Rows(t, w, h)); after != before {
		t.Fatalf("a mandate DEBIT produced a wallet-credit message: %d -> %d rows", before, after)
	}

	// 9) A credit whose balance move fails must not announce money that never
	//    arrived: the member would read "added to your Wallet" over an
	//    unchanged balance.
	x := w.customer(t, "9000008109", 0)
	if _, err := w.db.Collection(collWallets).InsertOne(ctx, bson.D{
		{Key: "_id", Value: primitive.NewObjectID()}, {Key: "consumer_id", Value: x},
		{Key: "currency", Value: "INR"}, {Key: "cash_balance", Value: "not-a-number"},
	}); err != nil {
		t.Fatalf("seed unreadable wallet: %v", err)
	}
	if _, err := w.svc.creditTopup(ctx, x, 500, "razorpay", "order_fail_1"); err == nil {
		t.Fatal("a credit whose $inc cannot apply must fail")
	}
	if evs := crmEventsOf(t, w.db, x, "wallet.credited"); len(evs) != 0 {
		t.Fatalf("wallet.credited emitted for a credit that never landed: %+v", evs)
	}
	if evs := crmEventsOf(t, w.db, x, "wallet.recharge_settled"); len(evs) != 0 {
		t.Fatalf("wallet.recharge_settled emitted for a credit that never landed: %+v", evs)
	}

	// 10) The referral reward (referrals.go): the referee's first paid
	//     delivery credits Rs 100 of Pyaas credit to BOTH wallets, and each
	//     side is told, once, in the non-refundable wording.
	referrer := w.customer(t, "9000008111", 0)
	referee := w.customer(t, "9000008112", 0)
	code, err := w.svc.referralCode(ctx, referrer)
	if err != nil {
		t.Fatalf("referral code: %v", err)
	}
	if _, err := w.svc.applyReferral(ctx, referee, code); err != nil {
		t.Fatalf("apply referral: %v", err)
	}
	if _, err := w.svc.creditTopup(ctx, referee, 500, "razorpay", "order_referee_1"); err != nil {
		t.Fatalf("referee top-up: %v", err)
	}
	w.svc.crmProcessEvents(ctx) // the top-up's own receipt
	paid := instantOrderDelivered(t, w, referee)
	// The reward waits out the rider's undo window (referralRewardWorker).
	afterHold := time.Now().Add(referralRewardHold + time.Hour)
	w.svc.payDueReferralRewards(ctx, afterHold)
	if ref, _ := w.svc.repo.findReferralByReferee(ctx, referee); ref == nil || ref.Status != referralCredited || ref.RewardOrderID != paid.OrderID {
		t.Fatalf("setup: the paid delivery must credit the referral: %+v", ref)
	}
	crmExpectOneB06(t, w, referrer, "referral reward (referrer)", "T-B05-PROMO", "₹100 Pyaas credit added (Referral reward")
	var refereeReward int
	for _, r := range crmB06Rows(t, w, referee) {
		if strings.Contains(r.EN, "Pyaas credit added (Referral reward") {
			refereeReward++
			if r.Template != "T-B05-PROMO" || crmTokenRe.FindString(r.EN) != "" {
				t.Fatalf("referral reward (referee): %+v", r)
			}
		}
	}
	if refereeReward != 1 {
		t.Fatalf("referral reward (referee): %d B-06 rows, want 1: %+v", refereeReward, crmB06Rows(t, w, referee))
	}
	crmExpectNoValidityClaim(t, "referral reward (referrer)", crmB06Rows(t, w, referrer))
	crmExpectNoValidityClaim(t, "referral reward (referee)", crmB06Rows(t, w, referee))
	// A second delivery pays nothing more and says nothing more.
	instantOrderDelivered(t, w, referee)
	w.svc.payDueReferralRewards(ctx, afterHold)
	w.svc.crmProcessEvents(ctx)
	if n := len(crmB06Rows(t, w, referrer)); n != 1 {
		t.Fatalf("referral reward (referrer) after a second delivery: %d B-06 rows, want 1", n)
	}
}

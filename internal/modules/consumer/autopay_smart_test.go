package consumer

// Smart Recharge (founder decision 1, 25 Sep): AutoPay FUNDS the wallet.
// A charge starts when the wallet, less what the next locked and upcoming
// days will take, falls below the member's threshold; the wallet is credited
// only when the bank's payment is CAPTURED, exactly once; a refusal is never
// credited and tells the member once; the member's "top up now" never
// debits; the charge is started early enough to fund the day that enters
// the funding horizon before its 12 noon lock.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run Autopay -v

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// armedMandate stores an ACTIVE mandate whose bank token is confirmed.
func armedMandate(t *testing.T, w *chainWorld, cid primitive.ObjectID, id string, amount, maxAmount, threshold float64) *mandate {
	t.Helper()
	now := time.Now().UTC()
	m := &mandate{
		ID: primitive.NewObjectID(), MandateID: id, ConsumerID: cid, Plan: "daily", Status: "active",
		Amount: amount, MaxAmount: maxAmount, Threshold: threshold,
		Token: "token_" + id, TokenStatus: "confirmed", RzpCustomerID: "cust_" + id,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := w.svc.repo.insertMandate(context.Background(), m); err != nil {
		t.Fatalf("insertMandate: %v", err)
	}
	return m
}

func topupsOf(t *testing.T, w *chainWorld, mandateID string) []autopayTopup {
	t.Helper()
	cur, err := w.db.Collection(collAutopayTopups).Find(context.Background(), bson.D{{Key: "mandate_id", Value: mandateID}})
	if err != nil {
		t.Fatalf("topups: %v", err)
	}
	var out []autopayTopup
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("topups decode: %v", err)
	}
	return out
}

func walletDebits(t *testing.T, w *chainWorld, cid primitive.ObjectID) int64 {
	t.Helper()
	n, err := w.db.Collection(collWalletTxns).CountDocuments(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "type", Value: "DEBIT"}})
	if err != nil {
		t.Fatalf("debit count: %v", err)
	}
	return n
}

func TestAutopaySmartRechargeCreditsOnceOnCapture(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	cid := w.customer(t, "9000013001", 100)
	m := armedMandate(t, w, cid, "mnd_sr1", 500, 2000, 200)
	now := istDayAt("2026-10-05", 9, 0)

	// ₹100 is under the ₹200 line: one charge of the member's ₹500 starts.
	if n := w.svc.sweepAutopay(ctx, now); n != 1 {
		t.Fatalf("sweep started %d charges, want 1", n)
	}
	calls := f.recurringCalls()
	if len(calls) != 1 || calls[0]["token"] != m.Token || calls[0]["customer_id"] != m.RzpCustomerID ||
		calls[0]["amount"] != float64(50000) || calls[0]["recurring"] != true || calls[0]["contact"] != "+919000013001" {
		t.Fatalf("recurring charge: %v", calls)
	}
	tps := topupsOf(t, w, m.MandateID)
	if len(tps) != 1 || tps[0].Status != "initiated" || !tps[0].Open || tps[0].Reason != "threshold" || tps[0].RzpOrderID == "" {
		t.Fatalf("charge row: %+v", tps)
	}
	tp := tps[0]
	if f.orderAmount(tp.RzpOrderID) != 50000 {
		t.Fatalf("order amount %d", f.orderAmount(tp.RzpOrderID))
	}
	// Nothing is credited before the bank's payment is captured.
	if got := w.cash(t, cid); got != 100 {
		t.Fatalf("credited before capture: %v", got)
	}
	// One charge in flight per mandate: later ticks start nothing.
	if n := w.svc.sweepAutopay(ctx, now.Add(15*time.Minute)); n != 0 {
		t.Fatalf("a second charge started while one is in flight")
	}

	// Captured: the webhook credits it; replays, the sweep and a racing
	// reconcile add nothing.
	captured := rzpPaymentEvent("payment.captured", tp.RzpOrderID, tp.RzpPaymentID, 50000, m.Token, "")
	for i := 0; i < 3; i++ {
		if err := w.svc.razorpayWebhookEvent(ctx, captured); err != nil {
			t.Fatalf("webhook: %v", err)
		}
	}
	f.captured[tp.RzpOrderID] = tp.RzpPaymentID
	w.svc.reconcilePendingPaymentsAt(ctx, time.Now().Add(time.Hour), f.base())
	if got := w.cash(t, cid); got != 600 {
		t.Fatalf("after capture: cash %v want 600", got)
	}
	tps = topupsOf(t, w, m.MandateID)
	if tps[0].Status != "captured" || tps[0].Open {
		t.Fatalf("captured charge row: %+v", tps[0])
	}
	var credits []walletTxn
	cur, _ := w.db.Collection(collWalletTxns).Find(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "ref_id", Value: tp.RzpOrderID}})
	_ = cur.All(ctx, &credits)
	if len(credits) != 1 || credits[0].Type != "TOPUP" || credits[0].RefType != "autopay" || credits[0].Amount != 500 {
		t.Fatalf("ledger for the charge: %+v", credits)
	}
	if evs := crmEventsOf(t, w.db, cid, "wallet.credited"); len(evs) != 2 || evs[1].Payload["reason"] != "AutoPay recharge" {
		t.Fatalf("money-added events: %+v", evs)
	}
	// Funded now: nothing more.
	if n := w.svc.sweepAutopay(ctx, now.Add(time.Hour)); n != 0 {
		t.Fatalf("a funded wallet was charged again")
	}
	if n := walletDebits(t, w, cid); n != 0 {
		t.Fatalf("AutoPay debited the wallet %d times", n)
	}

	// The reconcile sweep alone (no webhook) credits a charge too, once.
	b := w.customer(t, "9000013002", 50)
	mb := armedMandate(t, w, b, "mnd_sr2", 300, 1000, 100)
	if n := w.svc.sweepAutopay(ctx, now); n != 1 {
		t.Fatalf("sweep for b started %d", n)
	}
	tb := topupsOf(t, w, mb.MandateID)[0]
	f.captured[tb.RzpOrderID] = tb.RzpPaymentID
	for i := 0; i < 2; i++ {
		// The sweep's own clock: the charge was bound at the test's 09:00.
		w.svc.reconcilePendingPaymentsAt(ctx, now.Add(time.Hour), f.base())
	}
	if got := w.cash(t, b); got != 350 {
		t.Fatalf("swept charge: cash %v want 350", got)
	}
	if tb2 := topupsOf(t, w, mb.MandateID)[0]; tb2.Status != "captured" {
		t.Fatalf("swept charge row %+v", tb2)
	}
}

func TestAutopaySmartRechargeFailureIsNeverCredited(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	cid := w.customer(t, "9000013011", 50)
	m := armedMandate(t, w, cid, "mnd_fail", 500, 2000, 200)
	day1 := istDayAt("2026-10-05", 9, 0)

	if n := w.svc.sweepAutopay(ctx, day1); n != 1 {
		t.Fatalf("sweep: %d", n)
	}
	tp := topupsOf(t, w, m.MandateID)[0]
	// The bank's refusal is reported later that morning.
	w.svc.clock = func() time.Time { return day1.Add(2 * time.Hour) }
	failed := rzpPaymentEvent("payment.failed", tp.RzpOrderID, tp.RzpPaymentID, 50000, m.Token, "Payment was declined by the bank")
	for i := 0; i < 2; i++ {
		if err := w.svc.razorpayWebhookEvent(ctx, failed); err != nil {
			t.Fatalf("webhook: %v", err)
		}
	}
	if got := w.cash(t, cid); got != 50 {
		t.Fatalf("a failed charge moved money: %v", got)
	}
	tp = topupsOf(t, w, m.MandateID)[0]
	if tp.Status != "failed" || tp.Open {
		t.Fatalf("failed charge row: %+v", tp)
	}
	evs := crmEventsOf(t, w.db, cid, "payment.failed")
	if len(evs) != 1 || evs[0].Payload["source"] != "autopay" || evs[0].Payload["scope_key"] != tp.Ref {
		t.Fatalf("payment.failed for the refused charge, once: %+v", evs)
	}
	mm, _ := w.svc.repo.findMandate(ctx, m.MandateID, cid)
	if mm.TopupFailures != 1 || mm.LastFailureDay != "2026-10-05" {
		t.Fatalf("failure hold: %+v", mm)
	}
	// Held to the next IST day.
	if n := w.svc.sweepAutopay(ctx, day1.Add(3*time.Hour)); n != 0 {
		t.Fatalf("retried the same day after a refusal")
	}
	// The next day it tries again; the bank refuses at once this time.
	f.recurringStatus, f.recurringBody = 400, `{"error":{"description":"token is not active"}}`
	day2 := day1.Add(24 * time.Hour)
	if n := w.svc.sweepAutopay(ctx, day2); n != 0 {
		t.Fatalf("a refused charge counted as started")
	}
	day3 := day2.Add(24 * time.Hour)
	w.svc.sweepAutopay(ctx, day3)
	if got := w.cash(t, cid); got != 50 {
		t.Fatalf("refusals moved money: %v", got)
	}
	mm, _ = w.svc.repo.findMandate(ctx, m.MandateID, cid)
	if mm.TopupFailures != 3 {
		t.Fatalf("failures: %+v", mm)
	}
	if evs := crmEventsOf(t, w.db, cid, "payment.failed"); len(evs) != 3 {
		t.Fatalf("one message per refused charge: %d", len(evs))
	}
	// Three refusals in a row: Smart Recharge waits for the member.
	f.recurringStatus = 0
	if n := w.svc.sweepAutopay(ctx, day3.Add(48*time.Hour)); n != 0 {
		t.Fatalf("charged after three refusals in a row")
	}
	// The member resets it (a policy change); the next tick charges again.
	th := 250.0
	if _, err := w.svc.setMandatePolicy(ctx, cid, m.MandateID, &th, nil); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if n := w.svc.sweepAutopay(ctx, day3.Add(48*time.Hour)); n != 1 {
		t.Fatalf("after the reset: %d", n)
	}
	if n := walletDebits(t, w, cid); n != 0 {
		t.Fatalf("AutoPay debited the wallet")
	}
}

// The member's "top up now" (POST /mandate/{id}/execute): a charge on
// their own mandate, bounded by the cap, idempotent by the app's ref, and
// never a debit.
func TestAutopayExecuteIsATopUpNeverADebit(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	cid := w.customer(t, "9000013021", 400)
	m := armedMandate(t, w, cid, "mnd_exec", 500, 1000, 200)
	now := istDayAt("2026-10-05", 20, 0)

	tp, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 300, "app-ref-1", now)
	if err != nil || tp.Status != "initiated" || tp.Reason != "app" || tp.AmountPaise != 30000 {
		t.Fatalf("top up now: %+v %v", tp, err)
	}
	again, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 300, "app-ref-1", now)
	if err != nil || again.Ref != tp.Ref || len(f.recurringCalls()) != 1 {
		t.Fatalf("a retried ref must answer the same charge: %+v %v (%d calls)", again, err, len(f.recurringCalls()))
	}
	var ae *apiError
	if _, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 300, "app-ref-2", now); !errors.As(err, &ae) || ae.Code != "AUTOPAY_IN_FLIGHT" {
		t.Fatalf("a second charge while one is in flight: %v", err)
	}
	if _, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 1500, "app-ref-3", now); !errors.As(err, &ae) || ae.status != 400 {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 100, "", now); !errors.As(err, &ae) || ae.status != 400 {
		t.Fatalf("no ref: %v", err)
	}
	if got := w.cash(t, cid); got != 400 {
		t.Fatalf("top up now moved money before capture: %v", got)
	}
	if n := walletDebits(t, w, cid); n != 0 {
		t.Fatalf("top up now DEBITED the wallet")
	}
	if err := w.svc.razorpayWebhookEvent(ctx, rzpPaymentEvent("payment.captured", tp.RzpOrderID, tp.RzpPaymentID, 30000, "", "")); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if got := w.cash(t, cid); got != 700 {
		t.Fatalf("after capture %v want 700", got)
	}
	// Default amount = the member's recharge amount.
	tp2, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 0, "app-ref-4", now)
	if err != nil || tp2.AmountPaise != 50000 {
		t.Fatalf("default amount: %+v %v", tp2, err)
	}
	// Without keys nothing is ever sent.
	w.svc.rzpKeySecret = ""
	if _, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 100, "app-ref-5", now); !errors.As(err, &ae) || ae.Code != "AUTOPAY_UNAVAILABLE" {
		t.Fatalf("keyless top up now: %v", err)
	}
	if n := w.svc.sweepAutopay(ctx, now); n != 0 {
		t.Fatalf("keyless sweep charged")
	}
}

// The noon lock decides day D on the wallet as it stood at 12:00 on D-1, and
// a UPI AutoPay debit lands about 24-36 h after it starts. So the sweep
// counts every plan day whose lock falls inside the funding horizon (48 h)
// and starts the charge the moment a day it cannot cover enters it: 47 h
// before that day's lock, in time for it.
func TestAutopayChargesEarlyEnoughForTheNoonLock(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	liveRzp(t, w)
	cid := w.customer(t, "9000013031", 250)
	if _, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{
		ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, UnitPrice: 35,
		Frequency: "daily", StartDate: "2026-10-05",
	}, chainPlanMadeAt); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	m := armedMandate(t, w, cid, "mnd_noon", 500, 2000, 50)

	// 11:00 on 5 Oct: the horizon (to 11:00 on 7 Oct) holds the locks of
	// 6 Oct (12:00 on 5 Oct) and 7 Oct (12:00 on 6 Oct): ₹140 of milk.
	// ₹250 - ₹140 = ₹110 stays above the ₹50 line: nothing yet.
	early := istDayAt("2026-10-05", 11, 0)
	if need, err := w.svc.autopayNeed(ctx, cid, early); err != nil || need != 140 {
		t.Fatalf("need at 11:00: %v %v", need, err)
	}
	if n := w.svc.sweepAutopay(ctx, early); n != 0 {
		t.Fatalf("charged before a day was at risk")
	}
	// 13:00: 8 Oct's lock (12:00 on 7 Oct) enters the horizon. The noon
	// lock has run for 6 Oct (a locked order) and previewed 7 Oct; each is
	// counted once with 8 Oct: ₹210, so ₹250 no longer covers them above
	// the line. The charge starts now, 47 h before 8 Oct's lock.
	at := istDayAt("2026-10-05", 13, 0)
	w.svc.sweepSubscriptionOrders(ctx, at)
	if need, err := w.svc.autopayNeed(ctx, cid, at); err != nil || need != 210 {
		t.Fatalf("need at 13:00 after the lock: %v %v", need, err)
	}
	if n := w.svc.sweepAutopay(ctx, at); n != 1 {
		t.Fatalf("the charge must start when 8 Oct enters the horizon")
	}
	tp := topupsOf(t, w, m.MandateID)[0]
	if lead := lockMomentFor("2026-10-08").Sub(tp.CreatedAt); lead < autopayChargeLead {
		t.Fatalf("started only %v before 8 Oct's lock; a UPI debit needs %v", lead, autopayChargeLead)
	}
	// Amount: the member's ₹500 (the ₹10 shortfall is less).
	if tp.AmountPaise != 50000 {
		t.Fatalf("amount %d", tp.AmountPaise)
	}
}

// RV-AP-05 (founder decision 6, quiet hours): the server starts its own
// charges only from 07:00 to 22:00 IST, since each one makes the bank send
// a pre-debit notice and a refusal messages the member. A need at night
// waits for 07:00, the Founding Family seat's charge on a bill day too; the
// member's own "top up now" is never held.
func TestAutopayChargesStartInDaytimeOnly(t *testing.T) {
	t.Setenv("MANDATE_AUTODEBIT", "true")
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)

	// A refusal on 5 Oct holds the retry to 6 Oct, and not before 07:00.
	a := w.customer(t, "9000013061", 50)
	ma := armedMandate(t, w, a, "mnd_night", 500, 2000, 200)
	f.recurringStatus, f.recurringBody = 400, `{"error":{"description":"declined"}}`
	w.svc.sweepAutopay(ctx, istDayAt("2026-10-05", 10, 0))
	f.recurringStatus = 0
	refused := len(f.recurringCalls())
	for _, at := range []time.Time{istDayAt("2026-10-06", 0, 5), istDayAt("2026-10-06", 6, 59)} {
		if n := w.svc.sweepAutopay(ctx, at); n != 0 {
			t.Fatalf("a charge started at %s IST", at.In(istZone).Format("15:04"))
		}
	}
	if len(f.recurringCalls()) != refused {
		t.Fatalf("a night sweep reached the bank")
	}
	if n := w.svc.sweepAutopay(ctx, istDayAt("2026-10-06", 7, 0)); n != 1 {
		t.Fatalf("the held need must be charged at 07:00")
	}
	if tps := topupsOf(t, w, ma.MandateID); len(tps) != 2 {
		t.Fatalf("charges: %+v", tps)
	}

	// 22:00 is quiet; 21:59 is not.
	b := w.customer(t, "9000013062", 50)
	armedMandate(t, w, b, "mnd_late", 500, 2000, 200)
	if n := w.svc.sweepAutopay(ctx, istDayAt("2026-10-06", 22, 0)); n != 0 {
		t.Fatalf("a charge started at 22:00 IST")
	}
	if n := w.svc.sweepAutopay(ctx, istDayAt("2026-10-06", 21, 59)); n != 1 {
		t.Fatalf("a charge at 21:59 IST must start")
	}

	// The member's own top up now is theirs to time.
	c := w.customer(t, "9000013063", 50)
	mc := armedMandate(t, w, c, "mnd_own", 500, 2000, 200)
	if tp, err := w.svc.autopayTopupNow(ctx, c, mc.MandateID, 300, "own-late", istDayAt("2026-10-06", 23, 30)); err != nil || tp.Status != "initiated" {
		t.Fatalf("top up now at 23:30: %+v %v", tp, err)
	}

	// The seat on its bill day: nothing at 00:10, the charge at 07:00.
	seedTestFarms(t, w, 1)
	d := w.customer(t, "9000013064", 99)
	if _, err := w.svc.joinFoundingFamily(ctx, d, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	pinBillDate(t, w, d, "2026-10-31", 31)
	md := armedMandate(t, w, d, "mnd_seat_night", 500, 2000, 200)
	for _, at := range []time.Time{istDayAt("2026-10-31", 0, 10), istDayAt("2026-10-31", 6, 59)} {
		w.svc.billFoundingMembers(ctx, at)
	}
	if tps := topupsOf(t, w, md.MandateID); len(tps) != 0 {
		t.Fatalf("the seat charged at night: %+v", tps)
	}
	w.svc.billFoundingMembers(ctx, istDayAt("2026-10-31", 7, 0))
	if tps := topupsOf(t, w, md.MandateID); len(tps) != 1 || tps[0].Reason != "seat" {
		t.Fatalf("the seat's charge must start at 07:00: %+v", tps)
	}
	if m, _ := w.svc.repo.findFoundingMember(ctx, d); m.Status != memberActive {
		t.Fatalf("a night-held seat charge stopped the member: %+v", m)
	}
}

// The bank's word on the token: an unconfirmed token is looked up (at most
// hourly) and charged once confirmed; a token the bank cancels ends the
// mandate here too; an erased account is never charged.
func TestAutopayTokenAndErasure(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	cid := w.customer(t, "9000013041", 0)
	m := armedMandate(t, w, cid, "mnd_tok", 500, 2000, 200)
	if _, err := w.db.Collection(collMandates).UpdateOne(ctx, bson.D{{Key: "mandate_id", Value: m.MandateID}},
		bson.D{{Key: "$unset", Value: bson.D{{Key: "token_status", Value: ""}}}}); err != nil {
		t.Fatalf("unconfirm: %v", err)
	}
	f.tokenStatus[m.Token] = "initiated"
	now := istDayAt("2026-10-05", 9, 0)
	if n := w.svc.sweepAutopay(ctx, now); n != 0 || f.tokenLookups != 1 {
		t.Fatalf("an unconfirmed token was charged (%d) / looked up %d", n, f.tokenLookups)
	}
	if n := w.svc.sweepAutopay(ctx, now.Add(15*time.Minute)); n != 0 || f.tokenLookups != 1 {
		t.Fatalf("looked the token up again within the hour: %d", f.tokenLookups)
	}
	f.tokenStatus[m.Token] = "confirmed"
	if n := w.svc.sweepAutopay(ctx, now.Add(61*time.Minute)); n != 1 {
		t.Fatalf("a confirmed token must be charged")
	}

	// token.cancelled from the bank: the mandate ends; nothing more starts.
	b := w.customer(t, "9000013042", 0)
	mb := armedMandate(t, w, b, "mnd_bank", 500, 2000, 200)
	if err := w.svc.razorpayWebhookEvent(ctx, rzpTokenEvent("token.cancelled", mb.Token, "cancelled")); err != nil {
		t.Fatalf("token webhook: %v", err)
	}
	got, _ := w.svc.repo.findMandate(ctx, mb.MandateID, b)
	if got.Status != "cancelled" || got.CancelReason != "bank_cancelled" || got.TokenStatus != "cancelled" {
		t.Fatalf("bank-cancelled mandate: %+v", got)
	}
	if n := w.svc.sweepAutopay(ctx, now.Add(2*time.Hour)); n != 0 {
		t.Fatalf("charged a mandate the bank cancelled")
	}

	// Erasure: the mandate is cancelled (and its bank token), charges go.
	c := w.customer(t, "9000013043", 0)
	mc := armedMandate(t, w, c, "mnd_erase", 500, 2000, 200)
	if err := w.svc.erase(ctx, c); err != nil {
		t.Fatalf("erase: %v", err)
	}
	gc, _ := w.svc.repo.findMandate(ctx, mc.MandateID, c)
	if gc.Status != "cancelled" || gc.CancelReason != "erased" {
		t.Fatalf("erased account's mandate: %+v", gc)
	}
	found := false
	for _, tok := range f.cancelled {
		found = found || tok == mc.Token
	}
	if !found {
		t.Fatalf("the bank token of an erased account was not cancelled: %v", f.cancelled)
	}
	if n := w.svc.sweepAutopay(ctx, now.Add(3*time.Hour)); n != 0 {
		t.Fatalf("charged an erased account")
	}
}

// POST /mandate/{id}/policy: the member's line and amount, within the cap.
func TestAutopayPolicy(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000013051", 0)
	m := armedMandate(t, w, cid, "mnd_pol", 500, 2000, 200)
	th, amt := 300.0, 1000.0
	got, err := w.svc.setMandatePolicy(ctx, cid, m.MandateID, &th, &amt)
	if err != nil || got.Threshold != 300 || got.Amount != 1000 {
		t.Fatalf("policy: %+v %v", got, err)
	}
	over := 2500.0
	var ae *apiError
	if _, err := w.svc.setMandatePolicy(ctx, cid, m.MandateID, nil, &over); !errors.As(err, &ae) || ae.status != 400 {
		t.Fatalf("amount over the cap: %v", err)
	}
	zero := 0.0
	if got, err := w.svc.setMandatePolicy(ctx, cid, m.MandateID, &zero, nil); err != nil || got.Threshold != autopayDefaultThreshold {
		t.Fatalf("threshold 0 = the default: %+v %v", got, err)
	}
}

// RV-AP-09: a failed POST /orders (a bad key, a gateway config error) never
// reached the bank. It is our failure: not counted toward the three-refusal
// pause, no day hold, no "AutoPay couldn't add" message; the member's own
// top up now answers 502 AUTOPAY_GATEWAY, not a bank refusal.
func TestAutopayOrderCreateFailureIsOursNotARefusal(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	liveRzp(t, w)
	bad := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"error":{"description":"The api key provided is invalid"}}`))
	}))
	defer bad.Close()
	w.svc.rzpBase = bad.URL + "/v1"
	cid := w.customer(t, "9000013071", 10)
	m := armedMandate(t, w, cid, "mnd_badkey", 500, 2000, 200)
	day := istDayAt("2026-10-05", 9, 0)
	for i := 0; i < 4; i++ {
		if n := w.svc.sweepAutopay(ctx, day.Add(time.Duration(i)*15*time.Minute)); n != 0 {
			t.Fatalf("a charge whose order failed counted as started")
		}
	}
	mm, _ := w.svc.repo.findMandate(ctx, m.MandateID, cid)
	if mm.TopupFailures != 0 || mm.LastFailureDay != "" {
		t.Fatalf("an order failure counted as a bank refusal: %+v", mm)
	}
	if evs := crmEventsOf(t, w.db, cid, "payment.failed"); len(evs) != 0 {
		t.Fatalf("the member was told AutoPay failed for our own order failure: %+v", evs)
	}
	// Bounded: at most autopayMaxPerDay automatic tries a day.
	if tps := topupsOf(t, w, m.MandateID); len(tps) != autopayMaxPerDay || tps[0].Status != "failed" || tps[0].Open {
		t.Fatalf("order-failure rows: %+v", tps)
	}
	var ae *apiError
	if _, err := w.svc.autopayTopupNow(ctx, cid, m.MandateID, 300, "app-badkey", day); !errors.As(err, &ae) || ae.Code != "AUTOPAY_GATEWAY" {
		t.Fatalf("top up now on an order failure: %v", err)
	}
	if mm, _ = w.svc.repo.findMandate(ctx, m.MandateID, cid); mm.TopupFailures != 0 {
		t.Fatalf("top up now's order failure counted: %+v", mm)
	}
}

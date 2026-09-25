package consumer

// The Rs 99 Founding Family seat and AutoPay (founder decisions 1 and 3, 25
// Sep): the month still leaves the WALLET (Pyaas credit pays it first); a
// short wallet on the bill day with an ACTIVE mandate starts ONE AutoPay
// charge for that bill, the month is billed once when the charge is
// captured, and the membership is not stopped while the charge is on its
// way.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run FoundingSeatAutoPay -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func pinBillDate(t *testing.T, w *chainWorld, cid primitive.ObjectID, day string, anchor int) *foundingMember {
	t.Helper()
	ctx := context.Background()
	m, _ := w.svc.repo.findFoundingMember(ctx, cid)
	if m == nil || m.Status != memberActive {
		t.Fatalf("member not active: %+v", m)
	}
	if _, err := w.db.Collection(collFoundingMembers).UpdateByID(ctx, m.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "next_bill_date", Value: day}, {Key: "bill_day", Value: anchor}}}}); err != nil {
		t.Fatalf("pin bill date: %v", err)
	}
	m, _ = w.svc.repo.findFoundingMember(ctx, cid)
	return m
}

func foundingRows(t *testing.T, w *chainWorld, cid primitive.ObjectID) int64 {
	t.Helper()
	n, _ := w.db.Collection(collWalletTxns).CountDocuments(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "remark", Value: foundingLedgerLabel}})
	return n
}

func TestFoundingSeatAutoPayTopsUpAndBillsOnce(t *testing.T) {
	t.Setenv("MANDATE_AUTODEBIT", "true")
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	f := liveRzp(t, w)
	seedTestFarms(t, w, 1)
	a := w.customer(t, "9000014001", 99)
	if _, err := w.svc.joinFoundingFamily(ctx, a, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	pinBillDate(t, w, a, "2026-10-31", 31)
	md := armedMandate(t, w, a, "mnd_seat", 500, 2000, 200)
	at := func(day string, hour int) time.Time { return istDayAt(day, hour, 0) }

	// Bill day, empty wallet: no bill, one AutoPay charge for this bill.
	for i := 0; i < 3; i++ {
		if billed, stopped := w.svc.billFoundingMembers(ctx, at("2026-10-31", 9+i)); billed != 0 || stopped != 0 {
			t.Fatalf("a short wallet billed/stopped: %d %d", billed, stopped)
		}
	}
	tps := topupsOf(t, w, md.MandateID)
	if len(tps) != 1 || tps[0].Reason != "seat" || tps[0].AmountPaise != 50000 || !tps[0].Open {
		t.Fatalf("one seat charge for the bill: %+v", tps)
	}
	if len(f.recurringCalls()) != 1 {
		t.Fatalf("recurring charges: %d", len(f.recurringCalls()))
	}
	// Captured at 13:00: the wallet is funded and the month billed at once,
	// and a later tick bills nothing more.
	tp := tps[0]
	w.svc.clock = func() time.Time { return at("2026-10-31", 13) }
	for i := 0; i < 2; i++ {
		if err := w.svc.razorpayWebhookEvent(ctx, rzpPaymentEvent("payment.captured", tp.RzpOrderID, tp.RzpPaymentID, 50000, md.Token, "")); err != nil {
			t.Fatalf("capture: %v", err)
		}
	}
	if n := foundingRows(t, w, a); n != 2 {
		t.Fatalf("the capture must bill the month at once: FOUNDING-99 rows %d", n)
	}
	w.svc.billFoundingMembers(ctx, at("2026-10-31", 14))
	if got := w.cash(t, a); got != 500-99 {
		t.Fatalf("wallet after the seat top-up and the month: %v want 401", got)
	}
	m, _ := w.svc.repo.findFoundingMember(ctx, a)
	if m.Status != memberActive || m.LastBillDate != "2026-10-31" || m.NextBillDate != "2026-11-30" || m.BillAttempts != 0 {
		t.Fatalf("after the AutoPay-funded bill: %+v", m)
	}
	if n := foundingRows(t, w, a); n != 2 {
		t.Fatalf("FOUNDING-99 rows %d, want 2 (join + the month)", n)
	}
}

// A seat charge on its way holds the stop: the last retry day passes, the
// member stays active until the charge is settled. Refused: the next short
// day stops the membership as before.
func TestFoundingSeatAutoPayHoldsTheStopWhileInFlight(t *testing.T) {
	t.Setenv("MANDATE_AUTODEBIT", "true")
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	liveRzp(t, w)
	seedTestFarms(t, w, 1)
	b := w.customer(t, "9000014002", 99)
	if _, err := w.svc.joinFoundingFamily(ctx, b, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	pinBillDate(t, w, b, "2026-12-31", 31)
	md := armedMandate(t, w, b, "mnd_hold", 500, 2000, 200)
	days := []string{"2026-12-31", "2027-01-01", "2027-01-02", "2027-01-03"}
	for i, day := range days {
		billed, stopped := w.svc.billFoundingMembers(ctx, istDayAt(day, 9, 0))
		m, _ := w.svc.repo.findFoundingMember(ctx, b)
		if billed != 0 || stopped != 0 || m.Status != memberActive {
			t.Fatalf("day %d (%s): billed %d stopped %d %+v", i, day, billed, stopped, m)
		}
	}
	tp := topupsOf(t, w, md.MandateID)
	if len(tp) != 1 || !tp[0].Open {
		t.Fatalf("the seat charge must still be on its way: %+v", tp)
	}
	// The bank refuses it: the member is told, and the next short day stops.
	if err := w.svc.razorpayWebhookEvent(ctx, rzpPaymentEvent("payment.failed", tp[0].RzpOrderID, tp[0].RzpPaymentID, 50000, md.Token, "insufficient balance")); err != nil {
		t.Fatalf("refusal: %v", err)
	}
	billed, stopped := w.svc.billFoundingMembers(ctx, istDayAt("2027-01-04", 9, 0))
	m, _ := w.svc.repo.findFoundingMember(ctx, b)
	if billed != 0 || stopped != 1 || m.Status != memberStopped || m.StopReason != "wallet_short" || m.PerksUntil != "2026-12-30" {
		t.Fatalf("after the refused seat charge: billed %d stopped %d %+v", billed, stopped, m)
	}
	if got := w.cash(t, b); got != 0 {
		t.Fatalf("a refused charge moved money: %v", got)
	}

	// Without a mandate nothing changes: three short days, then the stop.
	c := w.customer(t, "9000014003", 99)
	if _, err := w.svc.joinFoundingFamily(ctx, c, "mishra-dairy"); err != nil {
		t.Fatalf("join mishra: %v", err)
	}
	if _, err := w.svc.upsertFoundingFarms(ctx, []foundingFarmInput{
		{ID: "mishra-dairy", Name: "Mishra Dairy", Farmer: "Abhishek Mishra", Place: "Gonda", UnlocksAt: 80, Status: "unlocked"},
	}, "test"); err != nil {
		t.Fatalf("unlock mishra: %v", err)
	}
	pinBillDate(t, w, c, "2026-12-31", 31)
	var stops int
	for _, day := range days {
		_, st := w.svc.billFoundingMembers(ctx, istDayAt(day, 10, 0))
		stops += st
	}
	if mc, _ := w.svc.repo.findFoundingMember(ctx, c); stops != 1 || mc.Status != memberStopped {
		t.Fatalf("no mandate: the usual three days then the stop: %d %+v", stops, mc)
	}
}

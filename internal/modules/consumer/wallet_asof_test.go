package consumer

// The noon lock decides on the wallet as it stood at 12:00:00 (the owner's
// rule of 24 Sep, decision 1: every movement after the lock moment is played
// back out, credits AND debits), so a tick at 12:00:01 and one at 12:14:59
// see the same balance.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run WalletAsOf -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// chainLedgerEpoch is where the test world stamps its seed ledger rows: long
// before any simulated lock moment, so no test depends on the real date.
var chainLedgerEpoch = istDayAt("2026-01-01", 9, 0)

// chainPlanMadeAt is when a test makes a plan for one of its fixed October
// days (tests backdate the member-change moment themselves where it
// matters): long before any of those days' cut-offs, as the wall clock was
// when the tests were written, but fixed, so the G4 re-anchor (a start on a
// locked morning moves to the first open one) never depends on the date or
// hour the suite runs.
var chainPlanMadeAt = istDayAt("2026-09-01", 9, 0)

// chainStampLedger moves one ledger row (consumer, ref, type) to `at`, the
// moment a test says the money moved.
func chainStampLedger(t *testing.T, w *chainWorld, cid primitive.ObjectID, refID, typ string, at time.Time) {
	t.Helper()
	res, err := w.db.Collection(collWalletTxns).UpdateOne(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "ref_id", Value: refID}, {Key: "type", Value: typ}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "created_at", Value: at.UTC()}}}})
	if err != nil || res.MatchedCount != 1 {
		t.Fatalf("stamp ledger %s %s: matched %v, %v", typ, refID, res, err)
	}
}

// chainCreditAt tops a member up and stamps the ledger row at `at`.
func chainCreditAt(t *testing.T, w *chainWorld, cid primitive.ObjectID, amount float64, ref string, at time.Time) {
	t.Helper()
	if _, err := w.svc.creditTopup(context.Background(), cid, amount, "razorpay", ref); err != nil {
		t.Fatalf("credit %s: %v", ref, err)
	}
	chainStampLedger(t, w, cid, ref, "TOPUP", at)
}

// chainDebitAt spends from a member's wallet and stamps the ledger row at `at`.
func chainDebitAt(t *testing.T, w *chainWorld, cid primitive.ObjectID, amount float64, ref string, at time.Time) {
	t.Helper()
	if _, err := w.svc.debit(context.Background(), cid, amount, ref, "test spend"); err != nil {
		t.Fatalf("debit %s: %v", ref, err)
	}
	chainStampLedger(t, w, cid, ref, "DEBIT", at)
}

func TestWalletAsOfReplaysTheLedger(t *testing.T) {
	const D = "2026-10-06"
	T := istDayAt(D, 12, 0)
	now := istDayAt(D, 12, 15)
	row := func(typ, status string, amount float64, at time.Time) walletTxn {
		return walletTxn{Type: typ, Status: status, Amount: amount, CreatedAt: at.UTC()}
	}
	rows := []walletTxn{
		row("TOPUP", "SUCCESS", 1000, istDayAt(D, 9, 0)),   // before the lock moment: part of the balance
		row("TOPUP", "SUCCESS", 7, T),                      // AT 12:00:00: already in the balance the lock sees
		row("TOPUP", "SUCCESS", 200, istDayAt(D, 12, 7)),   // after: played back out
		row("BONUS", "SUCCESS", 50, istDayAt(D, 12, 8)),    // after
		row("REFUND", "SUCCESS", 35, istDayAt(D, 12, 9)),   // after (a rider undo's credit)
		row("DEBIT", "SUCCESS", 85, istDayAt(D, 12, 5)),    // after: added back
		row("DEBIT", "REVERSED", 70, istDayAt(D, 12, 10)),  // a debit later undone: the money still left at 12:10
		row("DEBIT", "SUCCESS", 30, now),                   // exactly at now: inside the window
		row("TOPUP", "SUCCESS", 900, istDayAt(D, 12, 20)),  // after now: not in the balance now reads
		row("ADJUST", "SUCCESS", 999, istDayAt(D, 12, 11)), // unknown type: counted as nothing
		row("TOPUP", "PENDING", 400, istDayAt(D, 12, 12)),  // never settled: counted as nothing
	}
	available := 1500.0
	got, unknown := walletAsOfFromRows(available, rows, T, now)
	// 1500 - (200 + 50 + 35) + (85 + 70 + 30) = 1400
	if got != 1400 {
		t.Fatalf("as-of balance %v, want 1400", got)
	}
	if len(unknown) != 1 || unknown[0] != "ADJUST" {
		t.Fatalf("unknown types reported: %v", unknown)
	}
	if got, _ := walletAsOfFromRows(available, rows, now, now); got != available {
		t.Fatalf("as of now is the balance now: %v", got)
	}
	if got, _ := walletAsOfFromRows(available, nil, T, now); got != available {
		t.Fatalf("no movement after the lock moment: %v", got)
	}
}

// Against the real ledger: the balance at the lock moment, whatever moved
// after it and whenever the tick reads it.
func TestWalletAsOfReadsTheLedgerStore(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	cid := w.customer(t, "9000012001", 500) // seed stamped at the epoch
	chainDebitAt(t, w, cid, 70, "delivery:ord_asof_1", istDayAt(D, 12, 5))
	chainCreditAt(t, w, cid, 200, "order_asof_1", istDayAt(D, 12, 7))

	for _, tick := range []time.Time{istDayAt(D, 12, 7), istDayAt(D, 12, 14).Add(59 * time.Second), istDayAt(D, 23, 59)} {
		got, err := w.svc.walletAsOf(ctx, cid, istDayAt(D, 12, 0), tick)
		if err != nil {
			t.Fatalf("walletAsOf at %v: %v", tick, err)
		}
		if got != 500 {
			t.Fatalf("the wallet at 12:00:00, read at %s: %v, want 500", tick.In(istZone).Format("15:04:05"), got)
		}
	}
	// Before the debit it was 500; between the debit and the credit, 430.
	if got, _ := w.svc.walletAsOf(ctx, cid, istDayAt(D, 12, 6), istDayAt(D, 12, 15)); got != 430 {
		t.Fatalf("the wallet at 12:06: %v, want 430", got)
	}
	// The index the replay reads by exists.
	if !chainHasIndex(t, w, collWalletTxns, walletTxnsAsOfIndexName) {
		t.Fatalf("the (consumer_id, created_at) ledger index is missing")
	}
}

func chainHasIndex(t *testing.T, w *chainWorld, coll, name string) bool {
	t.Helper()
	cur, err := w.db.Collection(coll).Indexes().List(context.Background())
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var specs []bson.M
	if err := cur.All(context.Background(), &specs); err != nil {
		t.Fatalf("decode indexes: %v", err)
	}
	for _, s := range specs {
		if s["name"] == name {
			return true
		}
	}
	return false
}

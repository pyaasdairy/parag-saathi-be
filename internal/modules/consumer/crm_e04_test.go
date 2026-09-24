package consumer

// E-04 ("wrong / damaged / missing product") from the consumer E2E
// rehearsal (real app lib against the merged backend):
//
//   - E2E-01: [AMOUNT] was the order's TOTAL whatever was paid. An unpaid,
//     undelivered cash order (500ml x2 + 1L x1, Rs 154 with the Rs 15 fee)
//     was offered "or refund Rs 154 to your Wallet". The refund now offered is
//     what the member paid for the goods (the delivery debit, or the cash a
//     rider collected), never the delivery fee; with nothing paid the message
//     offers the redelivery only.
//   - E2E-03: a "missing" complaint filed without an order reached the E-04
//     renderer and was logged as an unresolved-token error. E-04 now needs
//     the member's own order by condition; E-02 still acknowledges.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRME04 -v

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// e2eMissing files a "missing" complaint and drains the worker.
func e2eMissing(t *testing.T, w *chainWorld, cid primitive.ObjectID, ref, orderID string) {
	t.Helper()
	if _, err := w.svc.fileComplaint(context.Background(), cid, complaintInput{Ref: ref, Category: "missing", OrderID: orderID, Detail: "a pack is missing"}); err != nil {
		t.Fatalf("fileComplaint %s: %v", ref, err)
	}
	w.svc.crmProcessEvents(context.Background())
}

// e04Bodies is every E-04 inbox row of a member, "EN | HI".
func e04Bodies(t *testing.T, w *chainWorld, cid primitive.ObjectID) []string {
	t.Helper()
	cur, err := w.db.Collection(collConsumerInbox).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "E-04"}})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	var rows []struct {
		EN string `bson:"body_en"`
		HI string `bson:"body_hi"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r.EN+" | "+r.HI)
	}
	return out
}

func TestCRME04OffersOnlyWhatWasPaid(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	e2eGoldCatalog(t, w)

	// (1) The rehearsal: an unpaid, undelivered cash order. Nothing was paid,
	//     so nothing is offered back: redelivery only.
	a := w.customer(t, "9000012101", 0)
	cod := e2eRehearsalOrder(t, w, a, "cod")
	if cod.DeliveryFee <= 0 {
		t.Fatalf("setup: the rehearsal order carries a delivery fee: %+v", cod)
	}
	e2eMissing(t, w, a, "PYS-E2E-1", cod.OrderID)
	got := e04Bodies(t, w, a)
	if len(got) != 1 || strings.Contains(got[0], "₹") || strings.Contains(strings.ToLower(got[0]), "refund") ||
		!strings.Contains(got[0], "Nothing has been charged for it.") || !strings.Contains(got[0], e2eGold+" 500ml x2 + 1L x1") {
		t.Fatalf("E-04 for an order nothing was paid for: %q", got)
	}

	// (2) A wallet order the rider delivered: the refund is what was debited
	//     for the goods, never the delivery fee.
	b := w.customer(t, "9000012102", 500)
	paid := instantOrderDelivered(t, w, b)
	if paid.DeliveryFee <= 0 {
		t.Fatalf("setup: a delivery fee to leave out: %+v", paid)
	}
	e2eMissing(t, w, b, "PYS-E2E-2", paid.OrderID)
	want := "refund ₹" + crmRupees(paid.Total-paid.DeliveryFee-paid.MonsoonFee) + " to your Wallet"
	if got = e04Bodies(t, w, b); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Fatalf("E-04 for a paid delivery: %q, want %q", got, want)
	}

	// (3) The same, after the rider undid the delivery (the debit went back):
	//     nothing is paid any more, so redelivery only.
	c := w.customer(t, "9000012103", 500)
	undone := instantOrderDelivered(t, w, c)
	if code := riderUndo(t, w, chainTaskFor(t, w, undone.OrderID).ID); code != 200 {
		t.Fatalf("rider undo: %d", code)
	}
	e2eMissing(t, w, c, "PYS-E2E-3", undone.OrderID)
	if got = e04Bodies(t, w, c); len(got) != 1 || strings.Contains(got[0], "₹") || !strings.Contains(got[0], "Nothing has been charged for it.") {
		t.Fatalf("E-04 after the debit was reversed: %q", got)
	}

	// (4) A cash order the rider delivered: the cash taken at the door, less
	//     the delivery fee.
	d := w.customer(t, "9000012104", 0)
	codPaid := e2eRehearsalOrder(t, w, d, "cod")
	task := chainTaskFor(t, w, codPaid.OrderID)
	if _, err := w.svc.claimOfferedDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	e2eMissing(t, w, d, "PYS-E2E-4", codPaid.OrderID)
	want = "refund ₹" + crmRupees(codPaid.Subtotal) + " to your Wallet"
	if got = e04Bodies(t, w, d); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Fatalf("E-04 for a delivered cash order: %q, want %q", got, want)
	}
}

// E2E-03: E-04 needs the member's own order. An order-less "missing"
// complaint is still acknowledged by E-02, and E-04 stays silent by its
// conditions, not by an unresolved-token error. Someone else's order id is
// no order of the member's: its product and amount are never rendered.
func TestCRME04NeedsTheMembersOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	var logs bytes.Buffer
	w.svc.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()
	cid := w.customer(t, "9000012201", 500)
	other := w.customer(t, "9000012202", 500)
	theirs := instantOrderDelivered(t, w, other)
	w.svc.crmProcessEvents(ctx)
	logs.Reset()

	e2eMissing(t, w, cid, "PYS-E2E-5", "")              // no order at all
	e2eMissing(t, w, cid, "PYS-E2E-6", theirs.OrderID)  // someone else's order
	e2eMissing(t, w, cid, "PYS-E2E-7", "ord_000000000") // an order that does not exist
	if n := inboxCount(t, w.db, cid, "E-04"); n != 0 {
		t.Fatalf("E-04 without the member's own order: %d rows %q", n, e04Bodies(t, w, cid))
	}
	if n := inboxCount(t, w.db, cid, "E-02"); n != 3 {
		t.Fatalf("E-02 still acknowledges every complaint: %d", n)
	}
	if s := logs.String(); strings.Contains(s, "unresolved template token") || strings.Contains(s, "fails closed") {
		t.Fatalf("an order-less E-04 must be a clean non-fire, logged:\n%s", s)
	}
}

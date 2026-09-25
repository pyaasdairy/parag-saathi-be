package consumer

// Founder decision 7 (25 Sep 2026): A-02 ("Delivered! Thanks for trying ...")
// was skipped when a member's first two orders were delivered inside one CRM
// worker tick. Its condition, orders_count == 1, was read when the worker
// drained the event, and by then both orders were delivered, so both events
// saw 2 and neither fired. The order.delivered payload now carries the
// member's delivered-order count as of that delivery (orders_count), and the
// condition reads that snapshot.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 \
//	  go test ./internal/modules/consumer/ -run CRMA02 -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The worker's tick in these tests runs on a fixed clock, never the wall
// clock: noon IST on a fixed day.
const crmA02Day = "2026-10-06"

var crmA02Tick = istDayAt(crmA02Day, 12, 0)

// crmInboxOrderIDs lists the order ids on a member's inbox rows for one trigger.
func crmInboxOrderIDs(t *testing.T, w *chainWorld, cid primitive.ObjectID, trigger string) []string {
	t.Helper()
	cur, err := w.db.Collection(collConsumerInbox).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}})
	if err != nil {
		t.Fatalf("inbox read: %v", err)
	}
	var rows []struct {
		OrderID string `bson:"order_id"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.OrderID)
	}
	return out
}

func TestCRMA02FirstOfTwoDeliveriesInOneTick(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000014101", 1000)
	first := instantOrderDelivered(t, w, cid)
	second := instantOrderDelivered(t, w, cid)
	// Both deliveries land before the worker's next tick; that one tick
	// drains both order.delivered events.
	w.svc.crmProcessEventsAt(ctx, crmA02Tick)

	if got := crmInboxOrderIDs(t, w, cid, "A-02"); len(got) != 1 || got[0] != first.OrderID {
		t.Fatalf("A-02 must fire exactly once, for the first order %s (second %s): inbox order ids %v, dispatch %v",
			first.OrderID, second.OrderID, got, crmDispatchStatuses(t, w.db, cid, "A-02"))
	}
	// Each event carries the count as of its own delivery.
	counts := map[string]float64{}
	for _, ev := range crmEventsOf(t, w.db, cid, "order.delivered") {
		id, _ := ev.Payload["order_id"].(string)
		n, ok := crmPayloadNumber(ev.Payload["orders_count"])
		if !ok {
			t.Fatalf("order.delivered for %s carries no orders_count snapshot: %v", id, ev.Payload)
		}
		counts[id] = n
	}
	if counts[first.OrderID] != 1 || counts[second.OrderID] != 2 {
		t.Fatalf("orders_count snapshots = %v, want %s:1 %s:2", counts, first.OrderID, second.OrderID)
	}
	// D-06 is untouched: one per order.
	if n := inboxCount(t, w.db, cid, "D-06"); n != 2 {
		t.Fatalf("D-06 rows = %d, want 2", n)
	}
}

// A member's second order drained on its own (the usual case) still sends no
// A-02, and a replay of the first event on a later day sends nothing more:
// the member's first Quick Pyaas delivery is thanked once, ever.
func TestCRMA02SecondDeliveryAloneAndReplay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000014102", 1000)
	first := instantOrderDelivered(t, w, cid)
	w.svc.crmProcessEventsAt(ctx, crmA02Tick)
	instantOrderDelivered(t, w, cid)
	w.svc.crmProcessEventsAt(ctx, crmA02Tick.Add(30*time.Minute))
	if got := crmInboxOrderIDs(t, w, cid, "A-02"); len(got) != 1 || got[0] != first.OrderID {
		t.Fatalf("A-02 inbox order ids %v, want only %s", got, first.OrderID)
	}

	// The first delivery's event replayed on the next IST day.
	var ev crmEvent
	if err := w.db.Collection(collCRMEvents).FindOne(ctx, bson.D{
		{Key: "consumer_id", Value: cid}, {Key: "topic", Value: "order.delivered"}, {Key: "payload.order_id", Value: first.OrderID},
	}).Decode(&ev); err != nil {
		t.Fatalf("event: %v", err)
	}
	ev.ID, ev.Status = primitive.NilObjectID, "NEW"
	if _, err := w.db.Collection(collCRMEvents).InsertOne(ctx, ev); err != nil {
		t.Fatalf("replay: %v", err)
	}
	w.svc.crmProcessEventsAt(ctx, istDayAt(addDaysIST(crmA02Day, 1), 12, 0))
	if got := crmInboxOrderIDs(t, w, cid, "A-02"); len(got) != 1 {
		t.Fatalf("A-02 after a next-day replay: %v", got)
	}
}

// The rank the emitter snapshots: the member's delivered orders up to and
// including this one, ordered by delivery time. A delivered order older than
// the delivered_at field counts as earlier; two deliveries stamped in the same
// second go by which order was placed first; another member's orders, and
// this member's orders that are not delivered, never count.
func TestCRMA02DeliveredOrderRank(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000014103", 0)
	other := w.customer(t, "9000014104", 0)

	stage := func(user primitive.ObjectID, status, deliveredAt string) *order {
		t.Helper()
		o := &order{
			MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: user.Hex(), Status: status, Lane: "instant",
			Items:    []orderItem{{ID: newItemID(), ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 1, Price: 35}},
			Subtotal: 35, Total: 35, PaymentMethod: "wallet", DeliveredAt: deliveredAt,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PlacedAt: time.Now().UTC(),
		}
		if err := w.svc.repo.insertOrder(ctx, o); err != nil {
			t.Fatalf("stage order: %v", err)
		}
		return o
	}
	const at = "2026-10-06T01:30:00Z"
	legacy := stage(cid, "delivered", "") // delivered before the field existed
	a := stage(cid, "delivered", at)
	b := stage(cid, "delivered", at) // the same second, placed after a
	stage(cid, "out_for_delivery", "")
	stage(cid, "cancelled", "")
	stage(other, "delivered", "2026-10-05T01:30:00Z")
	later := stage(cid, "delivered", "2026-10-06T01:31:00Z")

	for _, c := range []struct {
		o    *order
		want int
	}{{legacy, 1}, {a, 2}, {b, 3}, {later, 4}} {
		got, err := w.svc.repo.deliveredOrderRank(ctx, c.o)
		if err != nil || got != c.want {
			t.Errorf("rank of %s (delivered_at %q) = %d, %v; want %d", c.o.OrderID, c.o.DeliveredAt, got, err, c.want)
		}
	}
}

// An order.delivered written before the snapshot existed (in the outbox at
// the deploy) carries no orders_count: the condition reads the live count,
// as it always did.
func TestCRMA02EventWithoutSnapshotReadsLiveCount(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000014105", 0)
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: cid.Hex(), Status: "delivered", Lane: "instant",
		Items:    []orderItem{{ID: newItemID(), ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 1, Price: 35}},
		Subtotal: 35, Total: 35, PaymentMethod: "wallet", DeliveredAt: time.Now().UTC().Format(time.RFC3339),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PlacedAt: time.Now().UTC(),
	}
	if err := w.svc.repo.insertOrder(ctx, o); err != nil {
		t.Fatalf("stage order: %v", err)
	}
	w.svc.emitCRMEvent(ctx, "order.delivered", cid, map[string]any{
		"order_id": o.OrderID, "offer_pack": int32(0), "promotional_only": false, "labelled_product": crmLabelledProductOf(o),
	})
	w.svc.crmProcessEventsAt(ctx, crmA02Tick)
	if got := crmInboxOrderIDs(t, w, cid, "A-02"); len(got) != 1 || got[0] != o.OrderID {
		t.Fatalf("A-02 from a pre-snapshot event: %v", got)
	}
}

package consumer

// D-09: the order.failed event the backend already emitted now reaches the
// member, from both product paths (a rider's not-delivered marking and a
// store cancel), once per order, with the picklist cause and no money claim
// that is untrue.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMD09 -v

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestCRMD09DeliveryFailedReachesTheMember(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007601", 1000)

	newInstant := func() (*order, delivery) {
		t.Helper()
		o, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
			Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
		})
		if err != nil {
			t.Fatalf("createOrder: %v", err)
		}
		var task delivery
		if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&task); err != nil {
			t.Fatalf("task: %v", err)
		}
		return o, task
	}

	// (1) The rider marks it not delivered.
	o1, t1 := newInstant()
	if _, err := w.svc.claimOfferedDelivery(ctx, w.rider, t1.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.svc.failDelivery(ctx, w.rider, t1.ID, "Society gate / lift closed | guard would not open | photo=nd/1.jpg | called=true"); err != nil {
		t.Fatalf("failDelivery: %v", err)
	}
	// (2) The store cancels another one before it leaves.
	o2, t2 := newInstant()
	if _, err := w.svc.storeCancelDelivery(ctx, w.mgr, w.storeID.Hex(), t2.ID, ""); err != nil {
		t.Fatalf("storeCancelDelivery: %v", err)
	}
	w.svc.crmProcessEvents(ctx)

	got := crmDispatchScopes(t, w.db, cid, "D-09")
	if len(got) != 2 || got[o1.OrderID] != "SENT" || got[o2.OrderID] != "SENT" {
		t.Fatalf("D-09 once per failed order: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "D-09"); n != 2 {
		t.Fatalf("D-09 inbox rows = %d", n)
	}
	var rows []struct {
		BodyEN string `bson:"body_en"`
		BodyHI string `bson:"body_hi"`
		CTA    string `bson:"cta"`
	}
	cur, err := w.db.Collection(collConsumerInbox).Find(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "D-09"}})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	bodies := map[string]bool{}
	for _, r := range rows {
		bodies[r.BodyEN] = true
		if !strings.Contains(r.BodyEN, "Milk gold-500ml 500ml"+crmLabelledSuffix) || !strings.Contains(r.BodyEN, "Nothing has been charged for it.") ||
			!strings.Contains(r.BodyEN, "96672 60050") || strings.Contains(r.BodyEN, "[") {
			t.Fatalf("D-09 body_en = %q", r.BodyEN)
		}
		if !strings.Contains(r.BodyHI, "koi shulk nahi laga") || strings.Contains(r.BodyHI, "[") {
			t.Fatalf("D-09 body_hi = %q", r.BodyHI)
		}
		if r.CTA != "reorder" {
			t.Fatalf("D-09 cta = %q", r.CTA)
		}
	}
	want := map[string]string{
		"(Society gate / lift closed)": "the rider's picklist cause, never the remark or the evidence",
		"(Cancelled by the store)":     "the store cancel's default reason",
	}
	for frag, why := range want {
		found := false
		for b := range bodies {
			if strings.Contains(b, frag) {
				found = true
			}
		}
		if !found {
			t.Fatalf("D-09 must carry %s: %v", why, bodies)
		}
	}
	for b := range bodies {
		if strings.Contains(b, "guard would not open") || strings.Contains(b, "photo=") {
			t.Fatalf("the rider's remark or evidence leaked into the message: %q", b)
		}
	}
	// The failed orders are cancelled for the member, and no delivered
	// message ever went out for them.
	for _, id := range []string{o1.OrderID, o2.OrderID} {
		if o := w.orderByID(t, id); o.Status != "cancelled" {
			t.Fatalf("order %s status = %q", id, o.Status)
		}
	}
	if got := crmDispatchScopes(t, w.db, cid, "D-06"); len(got) != 0 {
		t.Fatalf("D-06 must not fire for a failed order: %v", got)
	}
}

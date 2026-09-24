package consumer

// The generic event router: conditions fail closed, params carry the C6
// tokens, and the Mongo test proves the double-fire the conditions exist to
// prevent (D-06 on a Welcome Litre pack) does not happen.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMGenericRouter -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCRMConditionEvaluator(t *testing.T) {
	facts := func(m map[string]any) func(string) (any, error) {
		return func(k string) (any, error) {
			if v, ok := m[k]; ok {
				return v, nil
			}
			return nil, errBadRequest("unknown key " + k)
		}
	}
	cases := []struct {
		expr  string
		facts map[string]any
		want  bool
		fails bool
	}{
		{"order.is_promotional_only == false", map[string]any{"order.is_promotional_only": false}, true, false},
		{"order.is_promotional_only == false", map[string]any{"order.is_promotional_only": true}, false, false},
		{"order.is_promotional_only == true", map[string]any{"order.is_promotional_only": true}, true, false},
		{"product_line == 'quick_pyaas'", map[string]any{"product_line": "quick_pyaas"}, true, false},
		{"product_line == 'quick_pyaas'", map[string]any{"product_line": "morning_delivery"}, false, false},
		{"product_line != 'quick_pyaas'", map[string]any{"product_line": "morning_delivery"}, true, false},
		{`offer.pack1_state == "delivered"`, map[string]any{"offer.pack1_state": "delivered"}, true, false},
		{"orders_count == 1", map[string]any{"orders_count": float64(1)}, true, false},
		{"orders_count == 1", map[string]any{"orders_count": int32(2)}, false, false},
		{"rating <= 3", map[string]any{"rating": int32(2)}, true, false},
		{"rating <= 3", map[string]any{"rating": int64(4)}, false, false},
		{"rating == 5", map[string]any{"rating": float64(5)}, true, false},
		{"effective_in_days >= 2", map[string]any{"effective_in_days": 2.0}, true, false},
		{"complaint.type in ['wrong_item','damaged','missing']", map[string]any{"complaint.type": "missing"}, true, false},
		{"complaint.type in ['wrong_item','damaged','missing']", map[string]any{"complaint.type": "late"}, false, false},
		{"true", nil, true, false},
		{"false", nil, false, false},
		// Fail closed: unknown key, unparsable shapes, type mismatches.
		{"orders_count == 0", map[string]any{}, false, true},
		{"opt_out_at == null || days_since_opt_out > 90", map[string]any{"opt_out_at": nil}, false, true},
		{"(product_line == 'x' && days_inactive == 10)", map[string]any{"product_line": "x"}, false, true},
		{"cart.age >= PT30M", map[string]any{"cart.age": 1.0}, false, true},
		{"rating <= 'three'", map[string]any{"rating": 2.0}, false, true},
		{"rating == 5", map[string]any{"rating": "five"}, false, true},
		{"complaint.type in [wrong_item]", map[string]any{"complaint.type": "wrong_item"}, false, true},
		{"just words", map[string]any{}, false, true},
	}
	for _, c := range cases {
		got, err := crmEvalConditions([]string{c.expr}, facts(c.facts))
		if c.fails {
			if err == nil {
				t.Errorf("%q: want an error (fail closed), got ok=%v", c.expr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %v want %v", c.expr, got, c.want)
		}
	}
	// AND across lines: one false line is false, one broken line is an error.
	if ok, err := crmEvalConditions([]string{"true", "rating == 5"}, facts(map[string]any{"rating": 4.0})); err != nil || ok {
		t.Fatalf("AND with a false line: ok=%v err=%v", ok, err)
	}
	if _, err := crmEvalConditions([]string{"true", "nope == 1"}, facts(map[string]any{})); err == nil {
		t.Fatal("AND with an unknown key must fail closed")
	}
	if ok, err := crmEvalConditions(nil, facts(nil)); err != nil || !ok {
		t.Fatalf("empty conditions hold: ok=%v err=%v", ok, err)
	}
}

func TestCRMEventParamsShapes(t *testing.T) {
	p := crmEventParams("order.confirmed", map[string]any{
		"order_id": "ord_1", "labelled_product": "Parag Gold 500ml" + crmLabelledSuffix, "promotional_only": false, "eta": "today by 7 am",
	}, nil)
	if p["ORDER_ID"] != "ord_1" || p["ETA"] != "today by 7 am" || p["LABELLED_PRODUCT"] != "Parag Gold 500ml"+crmLabelledSuffix {
		t.Fatalf("order.confirmed params: %v", p)
	}
	p = crmEventParams("order.dispatched", map[string]any{"order_id": "ord_1", "labelled_product": "x", "partner": "Ravi", "eta_min": int32(12)}, nil)
	if p["PARTNER"] != "Ravi" || p["ETA_MIN"] != "12" {
		t.Fatalf("order.dispatched params: %v", p)
	}
	p = crmEventParams("order.failed", map[string]any{"order_id": "ord_1", "labelled_product": "x", "reason": "Customer not home"}, nil)
	if p["REASON"] != "Customer not home" {
		t.Fatalf("order.failed params: %v", p)
	}
	o := &order{Total: 70, Items: []orderItem{{Name: "Parag Gold", Variant: "500ml"}}}
	p = crmEventParams("complaint.created", map[string]any{"complaint_id": "cmp_1", "ref": "PYS-1", "category": "missing", "order_id": "ord_1"}, o)
	// [AMOUNT] is never the order's total (E2E-01): the event context adds
	// what the member paid for the goods, from the ledger.
	if p["REF"] != "PYS-1" || p["COMPLAINT_ID"] != "cmp_1" || p["SLA"] != crmComplaintSLA || p["AMOUNT"] != "" || p["LABELLED_PRODUCT"] != "Parag Gold 500ml"+crmLabelledSuffix {
		t.Fatalf("complaint.created params: %v", p)
	}
	p = crmEventParams("complaint.created", map[string]any{"complaint_id": "cmp_2", "ref": "PYS-2", "category": "app"}, nil)
	if _, has := p["AMOUNT"]; has || p["SLA"] == "" {
		t.Fatalf("complaint without an order: %v", p)
	}
	p = crmEventParams("complaint.resolved", map[string]any{"complaint_id": "cmp_1", "ref": "PYS-1", "resolution": "Refunded"}, nil)
	if p["RESOLUTION"] != "Refunded" {
		t.Fatalf("complaint.resolved params: %v", p)
	}
	p = crmEventParams("rating.submitted", map[string]any{"order_id": "ord_1", "rating": int64(4)}, nil)
	if p["RATING"] != "4" {
		t.Fatalf("rating.submitted params: %v", p)
	}
	if crmRupees(70) != "70" || crmRupees(70.5) != "70.50" {
		t.Fatalf("crmRupees: %q %q", crmRupees(70), crmRupees(70.5))
	}
	if tok, ok := crmTemplateResolvable(crmTemplate{EN: "Hi [ETA]", HI: "[ETA] [LINK]"}, map[string]string{"ETA": "7 am"}); ok || tok != "LINK" {
		t.Fatalf("resolvable: tok=%q ok=%v", tok, ok)
	}
	if _, ok := crmTemplateResolvable(crmTemplate{EN: "Hi [ETA]"}, map[string]string{"ETA": "7 am"}); !ok {
		t.Fatal("fully resolvable template refused")
	}
}

// crmDispatchStatuses lists a consumer's dispatch-log rows for one trigger,
// oldest first, as status strings.
func crmDispatchStatuses(t *testing.T, db *mongo.Database, cid primitive.ObjectID, trigger string) []string {
	t.Helper()
	cur, err := db.Collection(collCRMDispatch).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		t.Fatalf("dispatch rows: %v", err)
	}
	var rows []crmDispatchRow
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("dispatch rows decode: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Status)
	}
	return out
}

func TestCRMGenericRouterOrdinaryOrderVsPack(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	// (1) An ORDINARY morning order through the real rider flow.
	cid := w.customer(t, "9000007102", 500)
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Router Tester", Phone: "9000007102",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	queue, err := w.svc.storeOrders(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeOrders: %v", err)
	}
	var task *delivery
	for i := range queue {
		if queue[i].OrderID == ord.OrderID {
			task = &queue[i]
		}
	}
	if task == nil {
		t.Fatal("task missing from the store queue")
	}
	if _, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), task.ID, w.riderID.Hex()); err != nil {
		t.Fatalf("assignRider: %v", err)
	}
	if _, err := w.svc.acceptDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("acceptDelivery: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, task.ID); err != nil {
		t.Fatalf("pickupDelivery: %v", err)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{
		ProofPhoto: "https://example.test/proof.jpg",
		Geo:        &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliverDelivery: %v", err)
	}
	w.svc.crmProcessEvents(ctx)

	if got := crmDispatchStatuses(t, w.db, cid, "D-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("D-01 on order.confirmed: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, cid, "D-06"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("D-06 on an ordinary delivered order: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "D-06"); n != 1 {
		t.Fatalf("D-06 inbox rows = %d", n)
	}
	if got := crmDispatchStatuses(t, w.db, cid, "W-02"); len(got) != 0 {
		t.Fatalf("W-02 must not fire for an ordinary order: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, cid, "D-02"); len(got) != 0 {
		t.Fatalf("D-02 is quick_pyaas only; a morning order must not fire it: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, cid, "A-02"); len(got) != 0 {
		t.Fatalf("A-02 is quick_pyaas only: %v", got)
	}
	// The rendered inbox body carries the label, never a raw token.
	var row struct {
		BodyEN string `bson:"body_en"`
	}
	if err := w.db.Collection(collConsumerInbox).FindOne(ctx,
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "D-06"}}).Decode(&row); err != nil {
		t.Fatalf("D-06 inbox row: %v", err)
	}
	if want := "Delivered ✅ Milk gold-500ml 500ml x2" + crmLabelledSuffix + ". Enjoy! Tap to rate."; row.BodyEN != want {
		t.Fatalf("D-06 body_en = %q, want %q", row.BodyEN, want)
	}

	// (2) A Welcome Litre PACK: W-02 fires, D-06 and D-01 stay silent.
	res, err := w.svc.crmEnrol(ctx, "router-test-operator", crmEnrolInput{
		Phone: "9000007103", Name: "Pack Household", Line1: "Flat 7, Router Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	pcid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
	deliverOrder(t, ctx, w.svc, w.svc.repo, w.db, res.Pack1OrderID)
	w.svc.crmProcessEvents(ctx)

	if got := crmDispatchStatuses(t, w.db, pcid, "W-02"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("W-02 on the pack delivery: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, pcid, "D-06"); len(got) != 0 {
		t.Fatalf("D-06 must not fire on a promotional-only order: %v", got)
	}
	if got := crmDispatchStatuses(t, w.db, pcid, "D-01"); len(got) != 0 {
		t.Fatalf("D-01 must not fire on a promotional-only order: %v", got)
	}
	if off := mustOffer(t, ctx, w.svc, pcid); off.Pack1State != pack1Delivered {
		t.Fatalf("pack1 state: %+v", off)
	}
}

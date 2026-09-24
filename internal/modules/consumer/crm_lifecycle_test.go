package consumer

// Contract C6: the order, complaint and rating lifecycle lands in the
// crm_events outbox with the documented topics and payload keys, from the
// real choke points (task creation, pickup, delivery, the complaint register,
// the operator's resolve, the review). Mongo-backed because the outbox IS
// stored state.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMLifecycle -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// crmEventsOf lists a consumer's outbox rows for one topic, oldest first.
func crmEventsOf(t *testing.T, db *mongo.Database, cid primitive.ObjectID, topic string) []crmEvent {
	t.Helper()
	cur, err := db.Collection(collCRMEvents).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "topic", Value: topic}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []crmEvent
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("events decode: %v", err)
	}
	return out
}

// crmPayloadInt reads an integer the BSON decoder may hand back as int32,
// int64 or float64.
func crmPayloadInt(v any) int {
	switch n := v.(type) {
	case int32:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

func TestCRMLifecycleEmitsContractC6(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007001", 500)

	// (1) order.confirmed the moment the task exists.
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 2, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Lifecycle Tester", Phone: "9000007001",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	evs := crmEventsOf(t, w.db, cid, "order.confirmed")
	if len(evs) != 1 {
		t.Fatalf("order.confirmed events = %d, want 1", len(evs))
	}
	p := evs[0].Payload
	if p["order_id"] != ord.OrderID || p["promotional_only"] != false {
		t.Fatalf("order.confirmed payload: %+v", p)
	}
	if p["labelled_product"] != "Milk gold-500ml 500ml x2"+crmLabelledSuffix {
		t.Fatalf("labelled_product must carry name + variant: %v", p["labelled_product"])
	}
	// A morning order that names no day is dated with the first open morning
	// (orders.go, the noon cut-off), so the message names that day.
	if eta, _ := p["eta"].(string); ord.DeliveryDate == "" || !strings.HasSuffix(eta, " by "+crmDLTDeliveryBy) {
		t.Fatalf("eta for a morning order must name its day (%q): %v", ord.DeliveryDate, p["eta"])
	}

	// (2) order.dispatched when the rider leaves with it.
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
	evs = crmEventsOf(t, w.db, cid, "order.dispatched")
	if len(evs) != 1 {
		t.Fatalf("order.dispatched events = %d, want 1", len(evs))
	}
	p = evs[0].Payload
	if p["order_id"] != ord.OrderID || p["partner"] != "Ravi Rider" || crmPayloadInt(p["eta_min"]) < 5 {
		t.Fatalf("order.dispatched payload: %+v", p)
	}
	if lp, _ := p["labelled_product"].(string); !strings.HasSuffix(lp, crmLabelledSuffix) {
		t.Fatalf("order.dispatched labelled_product: %v", p["labelled_product"])
	}

	// (3) order.delivered for an ORDINARY order (no offer-pack gate any more).
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{
		ProofPhoto: "https://example.test/proof.jpg",
		Geo:        &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliverDelivery: %v", err)
	}
	evs = crmEventsOf(t, w.db, cid, "order.delivered")
	if len(evs) != 1 {
		t.Fatalf("order.delivered events = %d, want 1 (every order emits now)", len(evs))
	}
	p = evs[0].Payload
	if p["order_id"] != ord.OrderID || p["promotional_only"] != false || crmPayloadInt(p["offer_pack"]) != 0 {
		t.Fatalf("order.delivered payload: %+v", p)
	}
	if p["labelled_product"] != "Milk gold-500ml 500ml x2"+crmLabelledSuffix {
		t.Fatalf("order.delivered labelled_product: %v", p["labelled_product"])
	}

	// (4) complaint.created once: the app's retry of the same filing is the
	// duplicate path and must not emit a second time.
	c, err := w.svc.fileComplaint(ctx, cid, complaintInput{
		Ref: "PYS-C6TEST", Category: "missing", OrderID: ord.OrderID, Detail: "one pack short",
	})
	if err != nil {
		t.Fatalf("fileComplaint: %v", err)
	}
	if again, err := w.svc.fileComplaint(ctx, cid, complaintInput{
		Ref: "PYS-C6TEST", Category: "missing", OrderID: ord.OrderID, Detail: "one pack short",
	}); err != nil || again.ID != c.ID {
		t.Fatalf("retry must hand back the same row: %v %+v", err, again)
	}
	evs = crmEventsOf(t, w.db, cid, "complaint.created")
	if len(evs) != 1 {
		t.Fatalf("complaint.created events = %d, want 1", len(evs))
	}
	p = evs[0].Payload
	if p["complaint_id"] != c.ID || p["ref"] != "PYS-C6TEST" || p["category"] != "missing" || p["order_id"] != ord.OrderID {
		t.Fatalf("complaint.created payload: %+v", p)
	}

	// (5) complaint.resolved only when the operator's status IS resolved.
	h := &handler{svc: w.svc}
	resolve := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/admin/crm/complaints/"+c.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("complaintId", c.ID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.crmUpdateComplaint(rec, req)
		return rec.Code
	}
	if code := resolve(`{"status":"in_review"}`); code != http.StatusOK {
		t.Fatalf("in_review: %d", code)
	}
	if n := len(crmEventsOf(t, w.db, cid, "complaint.resolved")); n != 0 {
		t.Fatalf("in_review must not emit complaint.resolved: %d", n)
	}
	if code := resolve(`{"status":"resolved","resolution":"Refunded one pack to your Wallet"}`); code != http.StatusOK {
		t.Fatalf("resolved: %d", code)
	}
	evs = crmEventsOf(t, w.db, cid, "complaint.resolved")
	if len(evs) != 1 {
		t.Fatalf("complaint.resolved events = %d, want 1", len(evs))
	}
	p = evs[0].Payload
	if p["complaint_id"] != c.ID || p["ref"] != "PYS-C6TEST" || p["resolution"] != "Refunded one pack to your Wallet" {
		t.Fatalf("complaint.resolved payload: %+v", p)
	}

	// (6) rating.submitted from the review.
	if _, err := w.svc.reviewOrder(ctx, cid.Hex(), ord.OrderID, 4, "good"); err != nil {
		t.Fatalf("reviewOrder: %v", err)
	}
	evs = crmEventsOf(t, w.db, cid, "rating.submitted")
	if len(evs) != 1 {
		t.Fatalf("rating.submitted events = %d, want 1", len(evs))
	}
	if p = evs[0].Payload; p["order_id"] != ord.OrderID || crmPayloadInt(p["rating"]) != 4 {
		t.Fatalf("rating.submitted payload: %+v", p)
	}
}

// The token helpers behind the C6 payloads.
func TestCRMLifecycleTokenHelpers(t *testing.T) {
	if got := crmLabelledProductOf(&order{Items: []orderItem{{Name: "Parag Gold", Variant: "500ml"}}}); got != "Parag Gold 500ml"+crmLabelledSuffix {
		t.Fatalf("labelled with variant: %q", got)
	}
	if got := crmLabelledProductOf(&order{Items: []orderItem{{Name: "Parag Gold"}}}); got != "Parag Gold"+crmLabelledSuffix {
		t.Fatalf("labelled without variant: %q", got)
	}
	if got := crmLabelledProductOf(nil); got != "500 ml Parag Full Cream"+crmLabelledSuffix {
		t.Fatalf("labelled fallback: %q", got)
	}

	now := time.Date(2026, 9, 23, 9, 0, 0, 0, istZone)
	if got := crmOrderETA(&order{}, now.Add(20*time.Minute).Format(time.RFC3339), now); got != "by 9:20 am" {
		t.Fatalf("instant eta: %q", got)
	}
	if got := crmOrderETA(&order{DeliveryDate: istDay(now.Add(24 * time.Hour))}, "", now); got != "tomorrow by "+crmDLTDeliveryBy {
		t.Fatalf("tomorrow eta: %q", got)
	}
	if got := crmOrderETA(&order{DeliveryDate: istDay(now)}, "", now); got != "today by "+crmDLTDeliveryBy {
		t.Fatalf("today eta: %q", got)
	}
	if got := crmOrderETA(&order{DeliveryDate: "2026-09-27"}, "", now); got != "27 Sep by "+crmDLTDeliveryBy {
		t.Fatalf("dated eta: %q", got)
	}
	if got := crmOrderETA(&order{}, "", now); got != "by "+crmDLTDeliveryBy {
		t.Fatalf("undated eta: %q", got)
	}

	if got := crmDeliveryETAMinutes(&delivery{EtaAt: now.Add(10 * time.Minute).Format(time.RFC3339)}, now); got != 10 {
		t.Fatalf("eta_min ahead: %d", got)
	}
	if got := crmDeliveryETAMinutes(&delivery{EtaAt: now.Add(-10 * time.Minute).Format(time.RFC3339), DistanceKm: 2.2}, now); got != 9 {
		t.Fatalf("eta_min late, by distance: %d", got)
	}
	if got := crmDeliveryETAMinutes(&delivery{DistanceKm: 0.5}, now); got != 5 {
		t.Fatalf("eta_min floor: %d", got)
	}

	if got := crmPartnerName("", "abc"); got != "your PYAAS rider" {
		t.Fatalf("partner empty: %q", got)
	}
	if got := crmPartnerName("abc", "abc"); got != "your PYAAS rider" {
		t.Fatalf("partner raw id: %q", got)
	}
	if got := crmPartnerName(" Ravi ", "abc"); got != "Ravi" {
		t.Fatalf("partner name: %q", got)
	}
}

package consumer

// POST /orders/{id}/review: the first rating of a delivered order is the
// member's rating. The app retries after a timeout, so the SAME rating and
// comment again answers 200 with the stored review; a DIFFERENT second
// review answers 409 ALREADY_REVIEWED and changes nothing - no overwrite of
// the stored review, no second rating.submitted (E-07 / the CRM read it).
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run OrderReview -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestOrderReviewFirstWinsRetryIsIdempotent(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	first := time.Date(2026, 9, 24, 3, 30, 0, 0, time.UTC) // 09:00 IST
	w.svc.clock = func() time.Time { return first }
	cid := w.customer(t, "9000007861", 1000)
	o := instantOrderDelivered(t, w, cid)
	h := &handler{svc: w.svc}

	review := func(orderID, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/orders/"+orderID+"/review", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", orderID)
		req = req.WithContext(context.WithValue(context.WithValue(req.Context(), chi.RouteCtxKey, rctx),
			consumerCtxKey, consumerActor{ID: cid.Hex()}))
		rec := httptest.NewRecorder()
		h.reviewOrder(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	stored := func() *orderReview {
		t.Helper()
		return w.orderByID(t, o.OrderID).Review
	}
	ratings := func() int64 { return crmEventCount(t, w, "rating.submitted", cid) }

	// The first review is stored and announced once.
	if code, body := review(o.OrderID, `{"rating":4,"comment":"good"}`); code != http.StatusOK {
		t.Fatalf("first review: %d %v", code, body)
	}
	r := stored()
	if r == nil || r.Rating != 4 || r.Comment != "good" || !r.CreatedAt.Equal(first) {
		t.Fatalf("first review stored: %+v", r)
	}
	if n := ratings(); n != 1 {
		t.Fatalf("rating.submitted after the first review: %d", n)
	}

	// A retry of the same review (a timeout on the phone): 200 with the
	// stored review, the first time kept, nothing announced again.
	w.svc.clock = func() time.Time { return first.Add(30 * time.Second) }
	code, body := review(o.OrderID, `{"rating":4,"comment":"good"}`)
	if code != http.StatusOK {
		t.Fatalf("identical retry: %d %v", code, body)
	}
	if rv, _ := body["review"].(map[string]any); rv == nil || rv["rating"] != float64(4) || rv["comment"] != "good" {
		t.Fatalf("identical retry must answer the stored review: %v", body)
	}
	if r := stored(); r == nil || !r.CreatedAt.Equal(first) {
		t.Fatalf("identical retry rewrote the review: %+v", r)
	}
	if n := ratings(); n != 1 {
		t.Fatalf("identical retry emitted rating.submitted again: %d", n)
	}

	// A different second review: 409, the first review stands, no event.
	for _, second := range []string{`{"rating":1,"comment":"late"}`, `{"rating":4,"comment":"good, but late"}`, `{"rating":5,"comment":"good"}`} {
		code, body := review(o.OrderID, second)
		if code != http.StatusConflict || body["code"] != "ALREADY_REVIEWED" || body["message"] != "You have already rated this order." {
			t.Fatalf("second review %s: %d %v, want 409 ALREADY_REVIEWED", second, code, body)
		}
	}
	if r := stored(); r == nil || r.Rating != 4 || r.Comment != "good" || !r.CreatedAt.Equal(first) {
		t.Fatalf("a second review overwrote the first: %+v", r)
	}
	if n := ratings(); n != 1 {
		t.Fatalf("a refused second review emitted rating.submitted: %d", n)
	}

	// Unchanged: an order that is not delivered, or not the member's, is 404;
	// a rating outside 1-5 is 400.
	placed, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "instant", Priority: "normal", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
		ConsumerName: "Review Tester", Phone: "9000007861",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	if code, body := review(placed.OrderID, `{"rating":5,"comment":""}`); code != http.StatusNotFound {
		t.Fatalf("review of an undelivered order: %d %v, want 404", code, body)
	}
	if code, body := review("ord_"+primitive.NewObjectID().Hex(), `{"rating":5,"comment":""}`); code != http.StatusNotFound {
		t.Fatalf("review of an unknown order: %d %v, want 404", code, body)
	}
	if code, body := review(o.OrderID, `{"rating":6,"comment":"good"}`); code != http.StatusBadRequest {
		t.Fatalf("rating 6: %d %v, want 400", code, body)
	}
}

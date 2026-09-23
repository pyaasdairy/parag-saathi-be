package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
)

// GET /orders is polled every 15 s and used to sign every proof photo on it,
// serially. The list now drops the key; GET /orders/{id} still carries it.
func TestOrderListOmitsProofPhotoButDetailKeepsIt(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000002001", 200)
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Photo Tester", Phone: "9000002001",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	// A value without the private-bucket prefix is handed back as-is by the
	// signer, so the detail response is deterministic here.
	if _, err := w.db.Collection(collOrders).UpdateOne(ctx, bson.D{{Key: "order_id", Value: ord.OrderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "proof_photo_url", Value: "https://example.test/proof.jpg"}}}}); err != nil {
		t.Fatalf("stamp proof: %v", err)
	}

	h := &handler{svc: w.svc}
	call := func(method, target string, fn http.HandlerFunc, orderID string) string {
		t.Helper()
		req := httptest.NewRequest(method, target, nil)
		rctx := chi.NewRouteContext()
		if orderID != "" {
			rctx.URLParams.Add("id", orderID)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
		rec := httptest.NewRecorder()
		fn(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, target, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	list := call(http.MethodGet, "/orders", h.listOrders, "")
	if !strings.Contains(list, ord.OrderID) {
		t.Fatalf("list missing the order: %s", list)
	}
	if strings.Contains(list, "proof_photo_url") {
		t.Fatalf("the list must not carry proof_photo_url: %s", list)
	}
	detail := call(http.MethodGet, "/orders/"+ord.OrderID, h.getOrder, ord.OrderID)
	if !strings.Contains(detail, `"proof_photo_url":"https://example.test/proof.jpg"`) {
		t.Fatalf("detail lost the proof photo: %s", detail)
	}
}

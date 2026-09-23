package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// Contract C1: tomorrow's subscription previews and scheduled one-off orders
// with no task yet, scoped to the store the order would route to.
func TestStoreUpcomingListsTomorrowsPreviews(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	store := w.storeID.Hex()
	tomorrow := addDaysIST(istToday(time.Now()), 1)

	// (a) a subscription preview: the 13:00 scheduler's unlocked order.
	cid := w.customer(t, "9000006001", 500)
	addrs, err := w.svc.repo.listAddresses(ctx, cid)
	if err != nil || len(addrs) == 0 {
		t.Fatalf("addresses: %v", err)
	}
	if _, err := w.db.Collection(collAddresses).UpdateOne(ctx, bson.D{{Key: "_id", Value: addrs[0].ID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "society", Value: "Gomti Greens"}, {Key: "society_id", Value: "soc-gg"}, {Key: "tower", Value: "C"}, {Key: "unit", Value: "1201"}}}}); err != nil {
		t.Fatalf("stamp society: %v", err)
	}
	addrs[0].Society, addrs[0].SocietyID, addrs[0].Tower, addrs[0].Unit = "Gomti Greens", "soc-gg", "C", "1201"
	sub := &subscription{
		SubscriptionID: "sub_upcoming_1", ConsumerID: cid, ProductID: "gold-500ml", Name: "Milk gold-500ml",
		Variant: "500ml", Qty: 2, UnitPrice: 35, Frequency: "daily", Status: "active", StartDate: tomorrow,
	}
	preview, err := w.svc.insertSubscriptionOrder(ctx, sub, &addrs[0], tomorrow, false, time.Now())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	// (b) a locked subscription day already has a task: not upcoming.
	live, err := w.svc.insertSubscriptionOrder(ctx, sub, &addrs[0], tomorrow, true, time.Now())
	if err != nil {
		t.Fatalf("locked: %v", err)
	}
	// (c) a scheduled one-off order: with its task it belongs to the orders
	// console; without one (creation failed) it is upcoming.
	cid2 := w.customer(t, "9000006002", 500)
	oneOff, err := w.svc.createOrder(ctx, cid2.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "taaza-1l", Name: "Milk taaza-1l", Qty: 1, Price: 57}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Scheduled Tester", Phone: "9000006002", DeliveryDate: tomorrow,
	})
	if err != nil {
		t.Fatalf("scheduled order: %v", err)
	}

	rows, err := w.svc.storeUpcoming(ctx, w.mgr, store)
	if err != nil {
		t.Fatalf("storeUpcoming: %v", err)
	}
	ids := map[string]upcomingRow{}
	for _, r := range rows {
		ids[r.OrderID] = r
	}
	if len(rows) != 1 || ids[preview.OrderID].Source != upcomingSourceSubscription {
		t.Fatalf("rows: %+v want only the preview %s", rows, preview.OrderID)
	}
	pr := ids[preview.OrderID]
	if pr.DeliveryDate != tomorrow || pr.Lane != "morning" || pr.ConsumerName == "" && pr.Phone == "" ||
		pr.Society != "Gomti Greens" || pr.SocietyID != "soc-gg" || pr.Tower != "C" || pr.Unit != "1201" ||
		len(pr.Items) != 1 || pr.Items[0].ProductID != "gold-500ml" || pr.Items[0].Variant != "500ml" || pr.Items[0].Qty != 2 {
		t.Fatalf("preview row: %+v", pr)
	}
	if _, has := ids[live.OrderID]; has {
		t.Fatalf("a locked day with a task must not be upcoming")
	}
	if _, has := ids[oneOff.OrderID]; has {
		t.Fatalf("a scheduled order that already has a task must not be upcoming")
	}
	if _, err := w.db.Collection(collDeliveries).DeleteOne(ctx, bson.D{{Key: "order_id", Value: oneOff.OrderID}}); err != nil {
		t.Fatalf("drop task: %v", err)
	}
	rows, err = w.svc.storeUpcoming(ctx, w.mgr, store)
	if err != nil {
		t.Fatalf("storeUpcoming 2: %v", err)
	}
	var sched *upcomingRow
	for i := range rows {
		if rows[i].OrderID == oneOff.OrderID {
			sched = &rows[i]
		}
	}
	if len(rows) != 2 || sched == nil || sched.Source != upcomingSourceScheduled || sched.Items[0].ProductID != "taaza-1l" {
		t.Fatalf("rows after dropping the task: %+v", rows)
	}
	// Read-only: listing twice minted nothing.
	if n, _ := w.db.Collection(collDeliveries).CountDocuments(ctx, bson.D{{Key: "order_id", Value: oneOff.OrderID}}); n != 0 {
		t.Fatalf("upcoming created a task")
	}

	// (d) scoping: a second store far away sees neither row, and an order
	// pinned near it shows only there.
	store2 := primitive.NewObjectID()
	mgr2 := primitive.NewObjectID()
	if _, err := w.db.Collection("org_units").InsertOne(ctx, bson.D{
		{Key: "_id", Value: store2}, {Key: "type", Value: "STORE"}, {Key: "active", Value: true},
		{Key: "name", Value: "Far Store"}, {Key: "geo_lat", Value: 28.6100}, {Key: "geo_lng", Value: 77.2100},
	}); err != nil {
		t.Fatalf("store2: %v", err)
	}
	if _, err := w.db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: mgr2}, {Key: "role_code", Value: "STORE_MANAGER"},
		{Key: "status", Value: "ACTIVE"}, {Key: "org_unit_id", Value: store2},
	}); err != nil {
		t.Fatalf("mgr2 role: %v", err)
	}
	actor2 := auth.Actor{PartyID: mgr2.Hex(), Kind: "role", RoleCode: "STORE_MANAGER"}
	cid3 := w.customer(t, "9000006003", 500)
	farOrder, err := w.svc.createOrder(ctx, cid3.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-1l", Name: "Milk gold-1l", Qty: 1, Price: 69}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Delhi",
		Lane: "morning", ConsumerName: "Far Tester", Phone: "9000006003", DeliveryDate: tomorrow,
		Geo: &geoPoint{Lat: 28.6000, Lng: 77.2000},
	})
	if err != nil {
		t.Fatalf("far order: %v", err)
	}
	if _, err := w.db.Collection(collDeliveries).DeleteOne(ctx, bson.D{{Key: "order_id", Value: farOrder.OrderID}}); err != nil {
		t.Fatalf("drop far task: %v", err)
	}
	rows2, err := w.svc.storeUpcoming(ctx, actor2, store2.Hex())
	if err != nil {
		t.Fatalf("storeUpcoming store2: %v", err)
	}
	if len(rows2) != 1 || rows2[0].OrderID != farOrder.OrderID {
		t.Fatalf("store2 rows: %+v want only %s", rows2, farOrder.OrderID)
	}
	rows, _ = w.svc.storeUpcoming(ctx, w.mgr, store)
	for _, r := range rows {
		if r.OrderID == farOrder.OrderID {
			t.Fatalf("the far order leaked into store 1's upcoming")
		}
	}
	if _, err := w.svc.storeUpcoming(ctx, w.mgr, store2.Hex()); err == nil {
		t.Fatalf("a manager of store 1 read store 2's upcoming")
	}

	// (e) the handler: 200, the array in the operator group's standard data
	// envelope (exactly like /stores/{storeId}/orders), snake_case keys, floor
	// null when unknown.
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodGet, "/stores/"+store+"/upcoming", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("storeId", store)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req = req.WithContext(auth.WithActor(req.Context(), w.mgr))
	rec := httptest.NewRecorder()
	h.storeUpcoming(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler: %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || len(env.Data) != 2 {
		t.Fatalf("handler body: %v %s", err, rec.Body.String())
	}
	body := env.Data
	for _, k := range []string{"order_id", "delivery_date", "delivery_window", "lane", "consumer_name", "phone",
		"address_label", "address_text", "society", "society_id", "tower", "floor", "unit", "items", "source"} {
		if _, ok := body[0][k]; !ok {
			t.Fatalf("key %q missing: %s", k, rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), `"floor":null`) {
		t.Fatalf("floor should be null when unknown: %s", rec.Body.String())
	}
}

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
	// Before noon the upcoming window is exactly tomorrow (from noon it runs
	// to the day after); the test drives the endpoint, and places its
	// scheduled one-off orders, at 09:00 IST today (after noon tomorrow is
	// past the one-off cut-off).
	ist := time.Now().In(istZone)
	at := time.Date(ist.Year(), ist.Month(), ist.Day(), 9, 0, 0, 0, istZone)

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
	// (b) a locked subscription day already has a task: not upcoming. Its
	// own subscription: one live order per (subscription, day) is the
	// invariant the day index enforces, and a preview is locked in place,
	// never re-inserted beside itself.
	sub2 := *sub
	sub2.SubscriptionID = "sub_upcoming_2"
	live, err := w.svc.insertSubscriptionOrder(ctx, &sub2, &addrs[0], tomorrow, true, time.Now())
	if err != nil {
		t.Fatalf("locked: %v", err)
	}
	// (c) a scheduled one-off order: with its task it belongs to the orders
	// console; without one (creation failed) it is upcoming.
	cid2 := w.customer(t, "9000006002", 500)
	oneOff, err := w.svc.createOrderAt(ctx, cid2.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "taaza-1l", Name: "Milk taaza-1l", Qty: 1, Price: 57}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Scheduled Tester", Phone: "9000006002", DeliveryDate: tomorrow,
	}, at)
	if err != nil {
		t.Fatalf("scheduled order: %v", err)
	}

	rows, err := w.svc.storeUpcomingAt(ctx, w.mgr, store, at)
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
	rows, err = w.svc.storeUpcomingAt(ctx, w.mgr, store, at)
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
	farOrder, err := w.svc.createOrderAt(ctx, cid3.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-1l", Name: "Milk gold-1l", Qty: 1, Price: 69}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Delhi",
		Lane: "morning", ConsumerName: "Far Tester", Phone: "9000006003", DeliveryDate: tomorrow,
		Geo: &geoPoint{Lat: 28.6000, Lng: 77.2000},
	}, at)
	if err != nil {
		t.Fatalf("far order: %v", err)
	}
	if _, err := w.db.Collection(collDeliveries).DeleteOne(ctx, bson.D{{Key: "order_id", Value: farOrder.OrderID}}); err != nil {
		t.Fatalf("drop far task: %v", err)
	}
	rows2, err := w.svc.storeUpcomingAt(ctx, actor2, store2.Hex(), at)
	if err != nil {
		t.Fatalf("storeUpcoming store2: %v", err)
	}
	if len(rows2) != 1 || rows2[0].OrderID != farOrder.OrderID {
		t.Fatalf("store2 rows: %+v want only %s", rows2, farOrder.OrderID)
	}
	rows, _ = w.svc.storeUpcomingAt(ctx, w.mgr, store, at)
	for _, r := range rows {
		if r.OrderID == farOrder.OrderID {
			t.Fatalf("the far order leaked into store 1's upcoming")
		}
	}
	if _, err := w.svc.storeUpcomingAt(ctx, w.mgr, store2.Hex(), at); err == nil {
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

// Each upcoming row says whether its noon cut-off has passed without a lock
// deciding it yet (awaiting_funds) and when it locks (locks_at). Since the
// low-wallet rule (24 Sep) the lock decides a day once, on the wallet at
// 12:00: before the lock tick both members' tomorrow is flagged; after it
// the funded one has left for the task queue and the short one is skipped,
// gone from the list instead of lingering until its day, and only the day
// after tomorrow's previews remain, neither flagged.
func TestStoreUpcomingFlagsPreviewsAwaitingFunds(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	plan := func(phone string, fund float64) *subscription {
		cid := w.customer(t, phone, fund)
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: D})
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		chainBackdateSubscription(t, w, sub, istDayAt(D, 8, 0))
		return sub
	}
	funded := plan("9000006101", 500)
	short := plan("9000006102", 0)
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))

	type wantRow struct {
		awaiting bool
		locksAt  string
	}
	check := func(at time.Time, want map[string]wantRow) {
		t.Helper()
		rows, err := w.svc.storeUpcomingAt(ctx, w.mgr, w.storeID.Hex(), at)
		if err != nil {
			t.Fatalf("storeUpcoming: %v", err)
		}
		raw, _ := json.Marshal(rows)
		var wire []map[string]any
		_ = json.Unmarshal(raw, &wire)
		if len(wire) != len(want) {
			t.Fatalf("rows at %s: %s", at.In(istZone).Format("15:04"), raw)
		}
		for _, r := range wire {
			exp, ok := want[r["order_id"].(string)]
			if !ok {
				t.Fatalf("unexpected row %v", r)
			}
			if r["awaiting_funds"] != exp.awaiting || r["locks_at"] != exp.locksAt {
				t.Fatalf("row %s %s: awaiting_funds=%v locks_at=%v, want %v %s", r["order_id"], r["delivery_date"], r["awaiting_funds"], r["locks_at"], exp.awaiting, exp.locksAt)
			}
		}
	}
	noonD, noonD1 := istDayAt(D, 12, 0).UTC().Format(time.RFC3339), istDayAt(D1, 12, 0).UTC().Format(time.RFC3339)

	// 12:02, the lock tick not run yet: tomorrow is past its cut-off, undecided.
	check(istDayAt(D, 12, 2), map[string]wantRow{
		liveSubOrder(t, w, short.SubscriptionID, D1).OrderID:  {true, noonD},
		liveSubOrder(t, w, funded.SubscriptionID, D1).OrderID: {true, noonD},
	})

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	if liveSubOrder(t, w, short.SubscriptionID, D1) != nil {
		t.Fatalf("the short member's tomorrow must be skipped at the lock")
	}
	check(istDayAt(D, 12, 30), map[string]wantRow{
		liveSubOrder(t, w, short.SubscriptionID, D2).OrderID:  {false, noonD1},
		liveSubOrder(t, w, funded.SubscriptionID, D2).OrderID: {false, noonD1},
	})
}

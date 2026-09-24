package consumer

// The 12-noon cut-off for one-off MORNING orders (One Voice 1.2; the app's
// product page says "Order by 12 noon, delivery by 7 AM"): after 12:00 IST
// tomorrow's morning route is closed to new one-off orders, exactly as it is
// to subscription changes. A requested morning that is closed (tomorrow from
// noon, today, a past day) is not refused: the order is moved to the first
// morning still open and accepted (owner, 24 Sep, R2), and says so with
// requested_date and date_moved. The instant lane is untouched.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run 'CutOff|FirstOpenMorning|MovedDay' -v

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func morningOrderFor(day string) orderInput {
	return orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", DeliveryDate: day,
	}
}

func TestMorningOrderAfterNoonMovesToTheFirstOpenMorning(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000011001", 5000)
	at := func(day string, h, m, s int) time.Time {
		return istDayAt(day, h, m).Add(time.Duration(s) * time.Second)
	}
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	const M = "2026-10-31" // month end: the first open morning is in November
	for _, c := range []struct {
		label     string
		at        time.Time
		requested string
		want      string
		moved     bool
	}{
		{"11:59 tomorrow", at(D, 11, 59, 0), D1, D1, false},
		{"11:59 today", at(D, 11, 59, 0), D, D1, true},
		{"12:00 tomorrow", at(D, 12, 0, 0), D1, D2, true},
		{"12:00:01 tomorrow", at(D, 12, 0, 1), D1, D2, true},
		{"12:14:59 tomorrow", at(D, 12, 14, 59), D1, D2, true},
		{"12:00 today", at(D, 12, 0, 0), D, D2, true},
		{"12:00 yesterday", at(D, 12, 0, 0), addDaysIST(D, -1), D2, true},
		{"12:01 day after tomorrow", at(D, 12, 1, 0), D2, D2, false},
		{"12:01 +7", at(D, 12, 1, 0), addDaysIST(D, 7), addDaysIST(D, 7), false},
		{"23:59 tomorrow", at(D, 23, 59, 0), D1, D2, true},
		{"month end 12:00:01", at(M, 12, 0, 1), "2026-11-01", "2026-11-02", true},
		{"month end 23:59", at(M, 23, 59, 0), "2026-11-01", "2026-11-02", true},
		{"month end 11:59 +1", at(M, 11, 59, 0), "2026-11-01", "2026-11-01", false},
	} {
		o, err := w.svc.createOrderAt(ctx, cid.Hex(), morningOrderFor(c.requested), c.at)
		if err != nil {
			t.Fatalf("%s: a closed morning must be moved, not refused: %v", c.label, err)
		}
		if o.DeliveryDate != c.want || o.RequestedDate != c.requested || o.DateMoved != c.moved {
			t.Fatalf("%s: delivery_date %s requested_date %s date_moved %v, want %s %s %v",
				c.label, o.DeliveryDate, o.RequestedDate, o.DateMoved, c.want, c.requested, c.moved)
		}
		stored := w.orderByID(t, o.OrderID)
		task, _ := w.svc.repo.findDeliveryByOrder(ctx, o.OrderID)
		if stored.DeliveryDate != c.want || stored.RequestedDate != c.requested || stored.DateMoved != c.moved ||
			task == nil || task.DeliveryDate != c.want {
			t.Fatalf("%s: stored %+v task %+v, want the real day %s on both", c.label, stored, task, c.want)
		}
	}

	// The upper bound stays: more than 7 days ahead is refused, nothing stored.
	before, _ := w.db.Collection(collOrders).CountDocuments(ctx, map[string]any{"user_id": cid.Hex()})
	_, err := w.svc.createOrderAt(ctx, cid.Hex(), morningOrderFor(addDaysIST(D, 8)), at(D, 12, 1, 0))
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != 422 || ae.Code != "BAD_DELIVERY_DATE" {
		t.Fatalf("+8 days must be refused with BAD_DELIVERY_DATE: %v", err)
	}
	if n, _ := w.db.Collection(collOrders).CountDocuments(ctx, map[string]any{"user_id": cid.Hex()}); n != before {
		t.Fatalf("a refused order was stored: %d orders, want %d", n, before)
	}
	// A malformed date is still a bad request.
	if _, err := w.svc.createOrderAt(ctx, cid.Hex(), morningOrderFor("7 Oct"), at(D, 12, 1, 0)); !errors.As(err, &ae) || ae.status != 400 {
		t.Fatalf("a malformed date must be a 400: %v", err)
	}

	// The instant lane has no cut-off and carries none of the three keys (a
	// stale client's date is ignored).
	instant := morningOrderFor(D1)
	instant.Lane = "instant"
	o, err := w.svc.createOrderAt(ctx, cid.Hex(), instant, at(D, 12, 1, 0))
	if err != nil || o.Lane != "instant" || o.DeliveryDate != "" || o.RequestedDate != "" || o.DateMoved {
		t.Fatalf("instant after noon: %v %+v", err, o)
	}
	raw, _ := json.Marshal(o)
	for _, k := range []string{`"delivery_date"`, `"requested_date"`, `"date_moved"`} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("an instant order carries %s: %s", k, raw)
		}
	}
	// The refusal code old app builds still handle stays defined.
	if errCodeCutoffPassed != "CUTOFF_PASSED" {
		t.Fatalf("CUTOFF_PASSED renamed: %q", errCodeCutoffPassed)
	}
}

// On the wire (POST /orders through the handler, the real clock): a morning
// already past (a stale cart's date) is moved, answered 201 with the real
// delivery_date and the two additive keys, and GET /orders/{id} carries them.
func TestMovedMorningOrderOnTheWire(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	cid := w.customer(t, "9000011002", 5000)
	h := &handler{svc: w.svc}
	body, _ := json.Marshal(map[string]any{
		"order_items":    []map[string]any{{"product_id": "gold-500ml", "name": "Milk gold-500ml", "qty": 1, "price": 35}},
		"payment_method": "wallet", "address_label": "Home", "address_text": "Shop St 1, Lucknow",
		"lane": "morning", "delivery_date": "2020-01-01",
	})
	firstBefore := firstEditableDay(time.Now())
	req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
	rec := httptest.NewRecorder()
	h.createOrder(rec, req)
	firstAfter := firstEditableDay(time.Now())
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("POST /orders: %d %s", rec.Code, rec.Body.String())
	}
	if d := out["delivery_date"]; (d != firstBefore && d != firstAfter) || out["requested_date"] != "2020-01-01" || out["date_moved"] != true {
		t.Fatalf("wire keys: delivery_date=%v requested_date=%v date_moved=%v (first open morning %s)",
			out["delivery_date"], out["requested_date"], out["date_moved"], firstAfter)
	}
	got, err := w.svc.getOrder(context.Background(), cid.Hex(), out["id"].(string))
	if err != nil || got.RequestedDate != "2020-01-01" || !got.DateMoved || got.DeliveryDate != out["delivery_date"] {
		t.Fatalf("GET /orders/{id}: %v %+v", err, got)
	}
}

// D-01 names the day the order really arrives: an order for tomorrow placed
// at 12:30 is moved to the day after, and order.confirmed's [ETA] says so.
func TestD01NamesTheMovedDay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000011003", 5000)
	const D = "2026-10-06"
	placedAt := istDayAt(D, 12, 30)
	o, err := w.svc.createOrderAt(ctx, cid.Hex(), morningOrderFor(addDaysIST(D, 1)), placedAt)
	if err != nil || o.DeliveryDate != addDaysIST(D, 2) {
		t.Fatalf("createOrderAt: %v %+v", err, o)
	}
	evs := crmEventsOf(t, w.db, cid, "order.confirmed")
	if len(evs) != 1 {
		t.Fatalf("order.confirmed events: %d", len(evs))
	}
	want := crmOrderETA(&order{DeliveryDate: addDaysIST(D, 2)}, "", placedAt)
	if evs[0].Payload["eta"] != want || want != "8 Oct by "+crmDLTDeliveryBy {
		t.Fatalf("D-01 [ETA] = %v, want %q", evs[0].Payload["eta"], want)
	}
	// Before noon the same order is for tomorrow, and D-01 says tomorrow.
	cid2 := w.customer(t, "9000011004", 5000)
	if _, err := w.svc.createOrderAt(ctx, cid2.Hex(), morningOrderFor(addDaysIST(D, 1)), istDayAt(D, 11, 0)); err != nil {
		t.Fatalf("createOrderAt 11:00: %v", err)
	}
	if evs := crmEventsOf(t, w.db, cid2, "order.confirmed"); len(evs) != 1 || evs[0].Payload["eta"] != "tomorrow by "+crmDLTDeliveryBy {
		t.Fatalf("D-01 before noon: %+v", evs)
	}
}

// A morning order that names no day (an older client, a direct API call)
// is for the first morning still open to orders: tomorrow before noon, the
// day after tomorrow from noon. Without a date it bypassed the cut-off and
// its task went on the very next route, tomorrow's included.
func TestUndatedMorningOrderTakesTheFirstOpenMorning(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	cid := w.customer(t, "9000011101", 1000)
	undated := func(lane string) orderInput {
		return orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: lane,
		}
	}
	for _, c := range []struct {
		label string
		lane  string
		at    time.Time
		want  string
	}{
		{"09:00 morning", "morning", istDayAt(D, 9, 0), D1},
		{"11:59 morning", "morning", istDayAt(D, 11, 59), D1},
		{"12:00 morning", "morning", istDayAt(D, 12, 0), D2},
		{"15:00 morning", "morning", istDayAt(D, 15, 0), D2},
		{"23:00 no lane", "", istDayAt(D, 23, 0), D2},
		{"02:00 next day", "morning", istDayAt(D1, 2, 0), D2},
	} {
		o, err := w.svc.createOrderAt(ctx, cid.Hex(), undated(c.lane), c.at)
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		task, _ := w.svc.repo.findDeliveryByOrder(ctx, o.OrderID)
		if o.Lane != "morning" || o.DeliveryDate != c.want || task == nil || task.DeliveryDate != c.want {
			t.Fatalf("%s: delivery_date %q task %+v, want %s", c.label, o.DeliveryDate, task, c.want)
		}
		if o.RequestedDate != "" || o.DateMoved {
			t.Fatalf("%s: an undated order requested nothing and moved nothing: %+v", c.label, o)
		}
	}
	// The instant lane still carries no day.
	if o, err := w.svc.createOrderAt(ctx, cid.Hex(), undated("instant"), istDayAt(D, 15, 0)); err != nil || o.DeliveryDate != "" {
		t.Fatalf("instant: %v %+v", err, o)
	}
}

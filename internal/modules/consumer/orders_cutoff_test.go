package consumer

// The 12-noon cut-off for one-off MORNING orders (One Voice 1.2; the app's
// product page says "Order by 12 noon, delivery by 7 AM"): after 12:00 IST
// tomorrow's morning route is closed to new one-off orders, exactly as it is
// to subscription changes. The instant lane is untouched.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CutOff -v

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOneOffMorningOrderNoonCutOff(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	cid := w.customer(t, "9000011001", 1000)
	morning := func(day string) orderInput {
		return orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
			Lane: "morning", DeliveryDate: day,
		}
	}

	// 11:59: tomorrow is still open.
	o, err := w.svc.createOrderAt(ctx, cid.Hex(), morning(D1), istDayAt(D, 11, 59))
	if err != nil || o.DeliveryDate != D1 {
		t.Fatalf("11:59 for tomorrow must be accepted: %v %+v", err, o)
	}

	// 12:01: tomorrow is closed; the refusal names the next open morning.
	_, err = w.svc.createOrderAt(ctx, cid.Hex(), morning(D1), istDayAt(D, 12, 1))
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != 422 || ae.Code != "CUTOFF_PASSED" || ae.NextDeliveryDate != D2 ||
		!strings.HasPrefix(ae.Message, "Order by 12 noon for tomorrow; next available ") {
		t.Fatalf("12:01 for tomorrow must be refused with CUTOFF_PASSED: %v %+v", err, ae)
	}
	// On the wire: the flat {code, message} the app's apiClient reads, plus
	// the next open morning as a field.
	rec := httptest.NewRecorder()
	writeErr(rec, err)
	var body map[string]any
	if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil || rec.Code != 422 ||
		body["code"] != "CUTOFF_PASSED" || body["next_delivery_date"] != D2 || body["message"] != ae.Message {
		t.Fatalf("wire body: %d %s", rec.Code, rec.Body.String())
	}
	if n, _ := w.db.Collection(collOrders).CountDocuments(ctx, map[string]any{"user_id": cid.Hex()}); n != 1 {
		t.Fatalf("a refused order was stored: %d orders", n)
	}

	// 12:01 for the day after tomorrow: open.
	if o, err = w.svc.createOrderAt(ctx, cid.Hex(), morning(D2), istDayAt(D, 12, 1)); err != nil || o.DeliveryDate != D2 {
		t.Fatalf("12:01 for the day after tomorrow must be accepted: %v %+v", err, o)
	}
	// The instant lane has no cut-off (a stale client's date is ignored).
	instant := morning(D1)
	instant.Lane = "instant"
	if o, err = w.svc.createOrderAt(ctx, cid.Hex(), instant, istDayAt(D, 12, 1)); err != nil || o.Lane != "instant" {
		t.Fatalf("instant after noon: %v %+v", err, o)
	}
}

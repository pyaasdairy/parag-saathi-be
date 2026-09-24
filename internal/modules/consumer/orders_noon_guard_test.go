package consumer

// Where the noon rules meet the order guard and the instant hours (the merge
// of feature/noon-rules into integration/delivery). POST /orders decides the
// morning first - a morning already closed at 12 noon is moved to the first
// open one and accepted - and only then judges the order as it will be
// delivered: an address we do not serve is refused on either lane, moved or
// not; instant keeps its hours; instant being shut at night never refuses a
// morning order. The member's cancel reads the same service clock as the
// order. Fixed IST clocks throughout, never the wall clock.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 \
//	  go test ./internal/modules/consumer/ -run 'OrderGuardJudges|MemberCancelReads' -v

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestOrderGuardJudgesTheMorningAsItWillBeDelivered(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	t.Setenv("INSTANT_TEST_OPEN", "")
	// Instant hours saved as 07:00-22:00, the console's default.
	guardZone(t, w, zone{InstantRadiusM: 2500, StandardRadiusM: 8000, InstantOpenMin: 7 * 60, InstantCloseMin: 22 * 60})
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	night := istDayAt(D, 22, 30) // past noon (D+1 is closed) and past instant's close

	p1 := pointAtBearing(guardCenter, 1000, 90) // inside both circles
	p5 := pointAtBearing(guardCenter, 5000, 0)  // the standard circle only
	place := func(cid primitive.ObjectID, lane, day string, pin geoPt, at time.Time) (*order, error) {
		t.Helper()
		return w.svc.createOrderAt(ctx, cid.Hex(), orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
			PaymentMethod: "cod", AddressLabel: "Home", AddressText: "Guard St 1, Lucknow",
			Lane: lane, DeliveryDate: day, ConsumerName: "Noon Guard", Phone: "9000017000",
			Geo: &geoPoint{Lat: pin.Lat, Lng: pin.Lng},
		}, at)
	}
	if sv, err := w.svc.serviceabilityAt(ctx, p1.Lat, p1.Lng, "", night); err != nil || !sv.Serviceable || !sv.InstantClosed {
		t.Fatalf("precondition: at 22:30 the point is served with instant shut, got %+v %v", sv, err)
	}

	// 1) The shipped cart's "tomorrow" at 22:30: moved to the day after and
	//    accepted, with its store task on that day, inside the instant circle
	//    and outside it alike.
	cid := w.customer(t, "9000017001", 0)
	for name, pin := range map[string]geoPt{"1 km": p1, "5 km": p5} {
		o, err := place(cid, "morning", D1, pin, night)
		if err != nil {
			t.Fatalf("%s: a morning order for tomorrow at 22:30 must be moved and accepted, got %v", name, err)
		}
		if o.DeliveryDate != D2 || o.RequestedDate != D1 || !o.DateMoved {
			t.Fatalf("%s: delivery_date %q requested %q moved %v, want %s %s true", name, o.DeliveryDate, o.RequestedDate, o.DateMoved, D2, D1)
		}
		if task := chainTaskFor(t, w, o.OrderID); task.StoreID != w.storeID.Hex() || task.Status != "ASSIGNED" || task.DeliveryDate != D2 {
			t.Fatalf("%s: task store %s status %s day %q, want the store's ASSIGNED task on %s", name, task.StoreID, task.Status, task.DeliveryDate, D2)
		}
	}

	// 2) The instant lane keeps its hours and its circle at 22:30; its stale
	//    date is ignored, never moved.
	other := w.customer(t, "9000017002", 0)
	_, err := place(other, "instant", D1, p1, night)
	guardRefused(t, "instant at 22:30", err, "INSTANT_CLOSED")
	_, err = place(other, "instant", "", p5, night)
	guardRefused(t, "instant 5 km out at 22:30", err, "INSTANT_OUT_OF_RANGE")

	// 3) An address we do not serve is refused, never moved to a later morning
	//    and stored.
	for _, day := range []string{D1, D2, ""} {
		_, err = place(other, "morning", day, guardDelhi, night)
		guardRefused(t, "Delhi morning at 22:30 for "+day, err, "NOT_SERVICEABLE")
	}
	guardNothingFor(t, w, other)

	// 4) At 09:00 the next day (before noon) instant is open and carries no
	//    date, and the open morning named is kept.
	morning := istDayAt(D1, 9, 0)
	inst, err := place(other, "instant", D2, p1, morning)
	if err != nil || inst.DeliveryDate != "" || inst.RequestedDate != "" || inst.DateMoved {
		t.Fatalf("instant at 09:00: %v %+v", err, inst)
	}
	kept, err := place(other, "morning", D2, p5, morning)
	if err != nil || kept.DeliveryDate != D2 || kept.RequestedDate != D2 || kept.DateMoved {
		t.Fatalf("an open morning at 09:00 must be kept: %v %+v", err, kept)
	}

	// 5) The morning is decided before the guard judges it: a date the order
	//    cannot have is answered as such wherever the order goes.
	stranger := w.customer(t, "9000017004", 0)
	_, err = place(stranger, "morning", "6 Oct", guardDelhi, night)
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != http.StatusBadRequest {
		t.Fatalf("a malformed date is a 400 before the guard, got %v", err)
	}
	_, err = place(stranger, "morning", addDaysIST(D, 9), guardDelhi, night)
	guardRefused(t, "nine days ahead to Delhi", err, "BAD_DELIVERY_DATE")
	guardNothingFor(t, w, stranger)
}

// The member's cancel (POST /orders/{id}/cancel calls cancelOrder) judges the
// noon cut-off on the service clock, as createOrder places on it: a pinned
// clock drives both.
func TestMemberCancelReadsTheServiceClock(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	cid := w.customer(t, "9000017003", 500)

	ihClock(w, istDayAt(D, 9, 0))
	tomorrow, err := w.svc.createOrder(ctx, cid.Hex(), morningOrderFor(D1))
	if err != nil || tomorrow.DeliveryDate != D1 {
		t.Fatalf("order for tomorrow at 09:00: %v %+v", err, tomorrow)
	}
	later, err := w.svc.createOrder(ctx, cid.Hex(), morningOrderFor(D2))
	if err != nil || later.DeliveryDate != D2 {
		t.Fatalf("order for the day after at 09:00: %v %+v", err, later)
	}

	ihClock(w, istDayAt(D, 12, 30))
	_, err = w.svc.cancelOrder(ctx, cid.Hex(), tomorrow.OrderID)
	assertOrderLocked(t, "tomorrow's order at 12:30 on the service clock", err)
	if o := w.orderByID(t, tomorrow.OrderID); o.Status != "placed" {
		t.Fatalf("a refused cancel changed the order: %q", o.Status)
	}
	if o, err := w.svc.cancelOrder(ctx, cid.Hex(), later.OrderID); err != nil || o.Status != "cancelled" {
		t.Fatalf("the day after tomorrow still cancels at 12:30: %v %+v", err, o)
	}
}

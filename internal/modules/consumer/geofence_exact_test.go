package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// chainTaskFor finds the delivery task minted for an order.
func chainTaskFor(t *testing.T, w *chainWorld, orderID string) *delivery {
	t.Helper()
	d, err := w.svc.repo.findDeliveryByOrder(context.Background(), orderID)
	if err != nil || d == nil {
		t.Fatalf("task for %s: %v %v", orderID, d, err)
	}
	return d
}

// chainOutForDelivery walks a morning task to the door: assign, accept, pick up.
func chainOutForDelivery(t *testing.T, w *chainWorld, taskID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.svc.assignRider(ctx, w.mgr, w.storeID.Hex(), taskID, w.riderID.Hex()); err != nil {
		t.Fatalf("assignRider: %v", err)
	}
	if _, err := w.svc.acceptDelivery(ctx, w.rider, taskID); err != nil {
		t.Fatalf("acceptDelivery: %v", err)
	}
	if _, err := w.svc.pickupDelivery(ctx, w.rider, taskID); err != nil {
		t.Fatalf("pickupDelivery: %v", err)
	}
}

func chainMorningOrder(t *testing.T, w *chainWorld, phone string, geo *geoPoint) *order {
	t.Helper()
	cid := w.customer(t, phone, 500)
	ord, err := w.svc.createOrder(context.Background(), cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Fence Tester", Phone: phone, Geo: geo,
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	return ord
}

func geofenceCode(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// With the customer's own pin the fence is real: the phone's verdict counts
// and the server measures the 300 m itself.
func TestGeofenceEnforcedWhenTaskHasTheCustomersPin(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	pin := &geoPoint{Lat: 26.8000, Lng: 81.0500}
	ord := chainMorningOrder(t, w, "9000003001", pin)
	task := chainTaskFor(t, w, ord.OrderID)
	if !task.GeoExact || task.Geo.Lat != pin.Lat {
		t.Fatalf("task should carry the customer pin exactly: %+v exact=%v", task.Geo, task.GeoExact)
	}
	if b, _ := json.Marshal(task); !strings.Contains(string(b), `"geoExact":true`) {
		t.Fatalf("geoExact missing from the wire: %s", b)
	}
	chainOutForDelivery(t, w, task.ID)

	at := &geoPt{Lat: pin.Lat, Lng: pin.Lng}
	_, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: at, GeofenceOK: false})
	if geofenceCode(err) != "GEOFENCE_FAILED" {
		t.Fatalf("phone says out of fence on an exact pin: err %v want GEOFENCE_FAILED", err)
	}
	far := &geoPt{Lat: pin.Lat + 0.05, Lng: pin.Lng}
	_, err = w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: far, GeofenceOK: true})
	if geofenceCode(err) != "GEOFENCE_FAILED" {
		t.Fatalf("5 km from the pin with a lying flag: err %v want GEOFENCE_FAILED", err)
	}
	d, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: at, GeofenceOK: true})
	if err != nil || d.Status != "DELIVERED" {
		t.Fatalf("at the pin: %v %v", d, err)
	}
}

// Without a pin the task points at the store; the rider at the real door is
// legitimately far from it, so neither the phone's verdict nor the server's
// measurement may refuse the delivery.
func TestGeofenceNotEnforcedWhenTaskFallsBackToTheStore(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	ord := chainMorningOrder(t, w, "9000003002", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	if task.GeoExact {
		t.Fatalf("an order without coordinates must not claim an exact pin")
	}
	if b, _ := json.Marshal(task); !strings.Contains(string(b), `"geoExact":false`) {
		t.Fatalf("geoExact must be emitted even when false: %s", b)
	}
	chainOutForDelivery(t, w, task.ID)

	far := &geoPt{Lat: task.Geo.Lat + 0.03, Lng: task.Geo.Lng}
	d, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: far, GeofenceOK: false})
	if err != nil || d.Status != "DELIVERED" {
		t.Fatalf("3 km from the store fallback, phone says out of fence: %v %v", d, err)
	}
	if o := w.orderByID(t, ord.OrderID); o.Status != "delivered" {
		t.Fatalf("order status %q want delivered", o.Status)
	}
}

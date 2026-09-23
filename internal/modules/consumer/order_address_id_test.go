package consumer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Two saved rows can both be called "Home". When the app sends address_id the
// task is stamped from that row; without it (older builds) the label lookup
// stands; someone else's id or a garbage id falls back to the label.
func TestOrderAddressIDPicksTheDoor(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000007001", 500) // default "Home", no unit
	lat, lng := 26.7712, 81.0123
	second := &address{
		ID: primitive.NewObjectID(), ConsumerID: cid, Label: "Home",
		Line1: "Tower B", Pincode: "226030", City: "Lucknow",
		Society: "Gomti Greens", SocietyID: "soc-gg", Tower: "B", Unit: "202",
		Lat: &lat, Lng: &lng, CreatedAt: time.Now().UTC(),
	}
	if _, err := w.db.Collection(collAddresses).InsertOne(ctx, second); err != nil {
		t.Fatalf("second address: %v", err)
	}
	other := w.customer(t, "9000007002", 0)
	foreign := &address{
		ID: primitive.NewObjectID(), ConsumerID: other, Label: "Home",
		Line1: "Elsewhere", Pincode: "226030", City: "Lucknow", Unit: "999",
		Lat: &lat, Lng: &lng, CreatedAt: time.Now().UTC(),
	}
	if _, err := w.db.Collection(collAddresses).InsertOne(ctx, foreign); err != nil {
		t.Fatalf("foreign address: %v", err)
	}

	place := func(addressID string) *order {
		t.Helper()
		ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Home, Lucknow", AddressID: addressID,
			Lane: "morning", ConsumerName: "Door Tester", Phone: "9000007001",
		})
		if err != nil {
			t.Fatalf("createOrder(%q): %v", addressID, err)
		}
		return ord
	}

	byID := place(second.ID.Hex())
	if task := chainTaskFor(t, w, byID.OrderID); task.Unit != "202" || task.SocietyID != "soc-gg" || task.Tower != "B" {
		t.Fatalf("address_id ignored: unit %q society %q tower %q", task.Unit, task.SocietyID, task.Tower)
	}
	if b, _ := json.Marshal(byID); !strings.Contains(string(b), `"address_id":"`+second.ID.Hex()+`"`) {
		t.Fatalf("address_id missing from the order wire: %s", b)
	}

	byLabel := place("")
	if task := chainTaskFor(t, w, byLabel.OrderID); task.Unit != "" || task.SocietyID != "" {
		t.Fatalf("label lookup should pick the default Home row: unit %q society %q", task.Unit, task.SocietyID)
	}
	if b, _ := json.Marshal(byLabel); strings.Contains(string(b), "address_id") {
		t.Fatalf("address_id must be omitted when not sent: %s", b)
	}

	for _, bad := range []string{foreign.ID.Hex(), "not-an-object-id", primitive.NewObjectID().Hex()} {
		ord := place(bad)
		if task := chainTaskFor(t, w, ord.OrderID); task.Unit != "" {
			t.Fatalf("address_id %q must fall back to the label: unit %q", bad, task.Unit)
		}
	}
}

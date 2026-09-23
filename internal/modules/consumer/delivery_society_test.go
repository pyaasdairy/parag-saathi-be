package consumer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// The structured door is copied onto the task with its society id (contract
// C3, camelCase like the rest of the task), so a round can be grouped by the
// directory's key and not only by the society's display name.
func TestDeliveryTaskCarriesTheSocietyID(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	cid := w.customer(t, "9000004001", 500)
	floor := 7
	if _, err := w.db.Collection(collAddresses).UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "label", Value: "Home"}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "society", Value: "Gomti Greens"}, {Key: "society_id", Value: "soc-gomti-greens"},
			{Key: "tower", Value: "B"}, {Key: "floor", Value: floor}, {Key: "unit", Value: "703"},
		}}}); err != nil {
		t.Fatalf("stamp society: %v", err)
	}
	ord, err := w.svc.createOrder(ctx, cid.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "B-703, Gomti Greens",
		Lane: "morning", ConsumerName: "Society Tester", Phone: "9000004001",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	task := chainTaskFor(t, w, ord.OrderID)
	if task.Society != "Gomti Greens" || task.SocietyID != "soc-gomti-greens" || task.Tower != "B" || task.Unit != "703" || task.Floor == nil || *task.Floor != 7 {
		t.Fatalf("structured door not copied: society=%q id=%q tower=%q floor=%v unit=%q",
			task.Society, task.SocietyID, task.Tower, task.Floor, task.Unit)
	}
	b, _ := json.Marshal(task)
	if !strings.Contains(string(b), `"societyId":"soc-gomti-greens"`) || !strings.Contains(string(b), `"society":"Gomti Greens"`) {
		t.Fatalf("wire shape: %s", b)
	}

	// An address without a directory key emits no societyId at all.
	cid2 := w.customer(t, "9000004002", 500)
	ord2, err := w.svc.createOrder(ctx, cid2.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1",
		Lane: "morning", ConsumerName: "Plain Tester", Phone: "9000004002",
	})
	if err != nil {
		t.Fatalf("createOrder 2: %v", err)
	}
	if b, _ := json.Marshal(chainTaskFor(t, w, ord2.OrderID)); strings.Contains(string(b), "societyId") {
		t.Fatalf("societyId must be omitted when absent: %s", b)
	}
}

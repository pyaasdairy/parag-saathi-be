package consumer

// E2E-02 (the consumer E2E rehearsal, real app lib against the merged
// backend): D-01 and E-04 named only the order's first line, so an instant
// order of 500ml x2 + 1L x1 was confirmed as "Order confirmed ... Full Cream
// Milk - Parag Gold 500ml ... Arriving by 2:31 pm", with no 1L and no count.
// The label now covers every line with its pack size and count; the SMS
// variable alone is cut to DLT's 30 characters.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run 'CRMLabelled|CRMSMSClips|CRMOrderConfirmed' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const e2eGold = "Full Cream Milk - Parag Gold"

func TestCRMLabelledProductCoversEveryLine(t *testing.T) {
	cases := []struct {
		name  string
		items []orderItem
		want  string
	}{
		{"one pack", []orderItem{{Name: e2eGold, Variant: "500ml", Qty: 1}}, e2eGold + " 500ml"},
		{"one line of two packs", []orderItem{{Name: e2eGold, Variant: "500ml", Qty: 2}}, e2eGold + " 500ml x2"},
		{"two sizes of one product", []orderItem{{Name: e2eGold, Variant: "500ml", Qty: 2}, {Name: e2eGold, Variant: "1L", Qty: 1}},
			e2eGold + " 500ml x2 + 1L x1"},
		{"one size on two lines", []orderItem{{Name: e2eGold, Variant: "500ml", Qty: 1}, {Name: e2eGold, Variant: "1L", Qty: 1}, {Name: e2eGold, Variant: "500ml", Qty: 2}},
			e2eGold + " 500ml x3 + 1L x1"},
		{"two products", []orderItem{{Name: e2eGold, Variant: "500ml", Qty: 2}, {Name: e2eGold, Variant: "1L", Qty: 1}, {Name: "Toned Milk - PYAAS", Variant: "1L Carton", Qty: 1}},
			e2eGold + " 500ml x2 + 1L x1 + 1 more"},
		{"three products", []orderItem{{Name: e2eGold, Variant: "500ml", Qty: 1}, {Name: "Toned Milk - PYAAS", Variant: "1L Carton", Qty: 1}, {Name: "Paneer", Qty: 1}},
			e2eGold + " 500ml x1 + 2 more"},
		{"no pack size", []orderItem{{Name: "Paneer", Qty: 2}}, "Paneer x2"},
	}
	for _, c := range cases {
		if got := crmLabelledProductOf(&order{Items: c.items}); got != c.want+crmLabelledSuffix {
			t.Errorf("%s: %q, want %q", c.name, got, c.want+crmLabelledSuffix)
		}
	}
	// Unchanged: the fallback for an order with no named line.
	if got := crmLabelledProductOf(nil); got != "500 ml Parag Full Cream"+crmLabelledSuffix {
		t.Errorf("nil order: %q", got)
	}
}

// DLT caps a variable at 30 characters. The longer multi-line label is cut
// on a word, in the SMS variables only; the inbox and push keep it whole.
func TestCRMSMSClipsTheProductLabel(t *testing.T) {
	var got struct {
		Recipients []map[string]string `json:"recipients"`
	}
	ch, _ := testSMSChannel(t, map[string]string{"D-01": "1207160000000099999"}, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"type":"success","request_id":"x"}`))
	})
	label := e2eGold + " 500ml x2 + 1L x1" + crmLabelledSuffix
	tpl := crmTemplate{EN: "Order confirmed [LABELLED_PRODUCT]. Arriving [ETA].", HI: "Order confirm [LABELLED_PRODUCT]. [ETA] tak."}
	if err := ch.deliver(context.Background(), "919876543210", crmTrigger{ID: "D-01", Category: "service_implicit"}, tpl,
		map[string]string{"LABELLED_PRODUCT": label, "ETA": "by 2:31 pm", "LINK": "https://pyaasdairy.com/app"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(got.Recipients) != 1 {
		t.Fatalf("recipients: %+v", got.Recipients)
	}
	v := got.Recipients[0]["labelled_product"]
	if n := utf8.RuneCountInString(v); n == 0 || n > 30 || !strings.HasPrefix(label, v) || strings.HasSuffix(v, " ") {
		t.Fatalf("SMS labelled_product %q (%d chars): want a word-boundary prefix of %q of at most 30", v, n, label)
	}
	if got.Recipients[0]["eta"] != "by 2:31 pm" {
		t.Fatalf("other variables are untouched: %+v", got.Recipients[0])
	}
}

// e2eGoldCatalog names both gold sizes as the production catalog does (one
// product, the pack size in the variant); the test world names each SKU.
func e2eGoldCatalog(t *testing.T, w *chainWorld) {
	t.Helper()
	ctx := context.Background()
	for sku, v := range map[string]string{"gold-500ml": "500ml", "gold-1l": "1L"} {
		if _, err := w.db.Collection(collCatalog).UpdateOne(ctx, bson.D{{Key: "sku_id", Value: sku}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: e2eGold}, {Key: "variant", Value: v}}}}); err != nil {
			t.Fatalf("catalog %s: %v", sku, err)
		}
	}
}

// e2eRehearsalOrder is the rehearsal's instant order: 500ml x2 + 1L x1.
func e2eRehearsalOrder(t *testing.T, w *chainWorld, cid primitive.ObjectID, pay string) *order {
	t.Helper()
	o, err := w.svc.createOrder(context.Background(), cid.Hex(), orderInput{
		Items: []orderItem{
			{ProductID: "gold-500ml", Name: e2eGold, Variant: "500ml", Qty: 2, Price: 35},
			{ProductID: "gold-1l", Name: e2eGold, Variant: "1L", Qty: 1, Price: 69},
		},
		PaymentMethod: pay, AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "instant", Geo: &geoPoint{Lat: 26.7712, Lng: 81.0123},
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	return o
}

func TestCRMOrderConfirmedNamesEveryLine(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	e2eGoldCatalog(t, w)
	cid := w.customer(t, "9000012001", 500)
	w.svc.crmProcessEvents(ctx) // the funding top-up's receipt
	e2eRehearsalOrder(t, w, cid, "cod")
	w.svc.crmProcessEvents(ctx)
	body := inboxBodyEN(t, w.db, cid, "D-01")
	if !strings.Contains(body, e2eGold+" 500ml x2 + 1L x1"+crmLabelledSuffix) {
		t.Fatalf("D-01 must name every line with its pack size and count: %q", body)
	}
}

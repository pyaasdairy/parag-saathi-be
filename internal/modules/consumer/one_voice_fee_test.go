package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pyaas/saathi-backend/internal/platform/dolibarr"
)

// The One Voice delivery rule (pyaas-one-voice.md 1.2, the founder's call of
// 25 Sep): free above Rs 199, Rs 5 below it, Founding Family members never
// pay, Parag at printed MRP with nothing added. "Above" is judged on the goods
// the order bills (after member prices, Parag lines included, before the fee
// and the instant monsoon surcharge): Rs 199.00 is free, Rs 198.99 pays.
func TestOneVoiceDeliveryFeeRule(t *testing.T) {
	for _, c := range []struct {
		name     string
		subtotal float64
		member   bool
		want     float64
	}{
		{"nothing billed", 0, false, 0},
		{"one Parag pack", 35, false, 5},
		{"just under the line", 198.99, false, 5},
		{"exactly Rs 199", 199, false, 0},
		{"above the line", 250, false, 0},
		{"a member under the line", 35, true, 0},
		{"a member above the line", 250, true, 0},
	} {
		if got := oneVoiceDeliveryFee(c.subtotal, 5, c.member); got != c.want {
			t.Errorf("%s: oneVoiceDeliveryFee(%v, 5, %v) = %v want %v", c.name, c.subtotal, c.member, got, c.want)
		}
	}
}

// The rule on real orders: what createOrder bills, for a non-member and for
// an active member, with the fee read from DELIVERY-FEE (the ERP's service
// price once the sync has seen it, else FOUNDING_DELIVERY_FEE_PAISE, Rs 5).
// The spec 5.3 non-member PYAAS fee, if ever switched on, follows the same
// rule: it never adds a second fee and never charges an order over Rs 199.
func TestOneVoiceDeliveryFeeOnOrders(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	p := 85.0
	if _, err := w.db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
		SkuID: "pyaas-toned-1l", Kind: catalogKindProduct, Price: &p, Name: "Toned Milk - PYAAS", Category: "milk", Unit: "1 L", Variant: "1L Carton",
	}); err != nil {
		t.Fatalf("seed pyaas: %v", err)
	}
	place := func(cid string, items ...orderItem) *order {
		t.Helper()
		o, err := w.svc.createOrder(ctx, cid, orderInput{
			Items: items, PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning",
		})
		if err != nil {
			t.Fatalf("createOrder: %v", err)
		}
		return o
	}
	gold := func(n int) orderItem { return orderItem{ProductID: "gold-500ml", Qty: n, Price: 35} }
	pyaas := func(n int) orderItem { return orderItem{ProductID: "pyaas-toned-1l", Qty: n, Price: 85} }

	nm := w.customer(t, "9000019001", 1000)
	// Below Rs 199: Parag at MRP (Rs 35) plus Rs 5, never the old Rs 15.
	if o := place(nm.Hex(), gold(2)); o.Subtotal != 70 || o.DeliveryFee != 5 || o.Total != 75 || o.Items[0].Price != 35 {
		t.Fatalf("non-member under Rs 199: subtotal %v fee %v total %v line %v", o.Subtotal, o.DeliveryFee, o.Total, o.Items[0].Price)
	}
	// Rs 199 and above is free (6 x Rs 35 = Rs 210).
	if o := place(nm.Hex(), gold(6)); o.DeliveryFee != 0 || o.Total != 210 {
		t.Fatalf("non-member over Rs 199: fee %v total %v", o.DeliveryFee, o.Total)
	}

	// An active member never pays it, however small the order, and Parag
	// stays at MRP for them too.
	seedTestFarms(t, w, 1)
	member := w.customer(t, "9000019002", 1000)
	if _, err := w.svc.joinFoundingFamily(ctx, member, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	if !w.svc.foundingActive(ctx, member) {
		t.Fatalf("a one-seat farm unlocks on the first join")
	}
	if o := place(member.Hex(), gold(1)); o.DeliveryFee != 0 || o.Total != 35 || o.Items[0].Price != 35 {
		t.Fatalf("member Parag order: fee %v total %v line %v", o.DeliveryFee, o.Total, o.Items[0].Price)
	}
	if o := place(member.Hex(), pyaas(1)); o.DeliveryFee != 0 || o.Items[0].Price != 83 || o.Total != 83 {
		t.Fatalf("member PYAAS order: fee %v line %v total %v", o.DeliveryFee, o.Items[0].Price, o.Total)
	}

	// The amount is DELIVERY-FEE: the ERP's price wins once the sync has it.
	w.svc.repo.saveFoundingERPPrice(ctx, "delivery_fee", 6)
	if o := place(nm.Hex(), gold(1)); o.DeliveryFee != 6 || o.Total != 41 {
		t.Fatalf("ERP DELIVERY-FEE: fee %v total %v", o.DeliveryFee, o.Total)
	}
	w.svc.repo.saveFoundingERPPrice(ctx, "delivery_fee", 5)

	// FOUNDING_PYAAS_NONMEMBER_FEE switched on changes nothing One Voice does
	// not already say: a non-member's PYAAS order under Rs 199 pays Rs 5 once
	// (not twice), and one of Rs 199 or more stays free.
	w.svc.deps.Cfg.FoundingPyaasNonMemberFee = true
	defer func() { w.svc.deps.Cfg.FoundingPyaasNonMemberFee = false }()
	if o := place(nm.Hex(), pyaas(1)); o.DeliveryFee != 5 || o.Total != 90 {
		t.Fatalf("non-member fee on, PYAAS under Rs 199: fee %v total %v", o.DeliveryFee, o.Total)
	}
	if o := place(nm.Hex(), pyaas(3)); o.DeliveryFee != 0 || o.Total != 255 {
		t.Fatalf("non-member fee on, PYAAS over Rs 199: fee %v total %v", o.DeliveryFee, o.Total)
	}
	// The savings line keeps promising only what a daily subscriber really
	// saves: subscription mornings never carry the fee, so nothing is added.
	if sv := w.svc.foundingSavings(ctx); sv == nil || sv.DeliveryFee != 0 {
		t.Fatalf("savings delivery_fee with the non-member fee on: %+v", sv)
	}
}

// RV-PR-02: the server bills DELIVERY-FEE as the ERP carries it (its GST
// included, like every ERP row), which need not be the Rs 5 the app used to
// hard-code: Rs 5 entered ex-GST at 18% bills Rs 5.90. So the rule orders are
// billed by is on the wire, additively, where the app reads it before a cart
// (GET /serviceability) and on the Founding Family screen (GET
// /founding-family): delivery {fee, free_from}. What it says is what an order
// bills.
func TestOneVoiceRuleIsServedAsBilled(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 10)
	nm := w.customer(t, "9000019011", 500)
	w.svc.appKey = "test-app-key"
	h := &handler{svc: w.svc}
	served := func() (fee, freeFrom float64) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/serviceability?lat=26.7700&lng=81.0100", nil)
		req.Header.Set("X-Parag-App-Key", "test-app-key")
		rec := httptest.NewRecorder()
		h.serviceability(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /serviceability: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Serviceable bool `json:"serviceable"`
			Delivery    *struct {
				Fee      float64 `json:"fee"`
				FreeFrom float64 `json:"free_from"`
			} `json:"delivery"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Delivery == nil {
			t.Fatalf("serviceability delivery rule: %s (%v)", rec.Body.String(), err)
		}
		v, err := w.svc.foundingFamilyView(ctx, nm)
		if err != nil || v.Delivery == nil {
			t.Fatalf("founding-family delivery rule: %+v %v", v, err)
		}
		if v.Delivery.Fee != body.Delivery.Fee || v.Delivery.FreeFrom != body.Delivery.FreeFrom {
			t.Fatalf("the two routes disagree: %+v vs %+v", *v.Delivery, *body.Delivery)
		}
		return body.Delivery.Fee, body.Delivery.FreeFrom
	}
	bill := func() *order {
		t.Helper()
		o, err := w.svc.createOrder(ctx, nm.Hex(), orderInput{
			Items:         []orderItem{{ProductID: "gold-500ml", Qty: 1, Price: 35}},
			PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning",
		})
		if err != nil {
			t.Fatalf("createOrder: %v", err)
		}
		return o
	}

	// No ERP row yet: the published Rs 5 from Rs 199 (FOUNDING_DELIVERY_FEE_PAISE).
	if fee, from := served(); fee != 5 || from != 199 {
		t.Fatalf("served rule before the ERP: fee %v free_from %v", fee, from)
	}
	if o := bill(); o.DeliveryFee != 5 {
		t.Fatalf("billed before the ERP: %v", o.DeliveryFee)
	}
	// DELIVERY-FEE entered as Rs 5 ex-GST at 18%: Rs 5.90 is billed, and now
	// quoted too.
	w.svc.repo.saveFoundingERPPrice(ctx, "delivery_fee", 5.9)
	if fee, from := served(); fee != 5.9 || from != 199 {
		t.Fatalf("served rule with the ERP at Rs 5.90: fee %v free_from %v", fee, from)
	}
	if o := bill(); o.DeliveryFee != 5.9 || o.Total != 40.9 {
		t.Fatalf("billed with the ERP at Rs 5.90: fee %v total %v", o.DeliveryFee, o.Total)
	}
}

// The Dolibarr sync warns when DELIVERY-FEE with its GST is not the published
// One Voice fee (the founder should enter it so it comes to exactly Rs 5.00),
// and stays quiet when it is.
func TestDolibarrSyncWarnsWhenDeliveryFeeIsNotTheOneVoiceFee(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	var logs bytes.Buffer
	w.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	product := func(raw string) dolibarr.Product {
		t.Helper()
		var p dolibarr.Product
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("product: %v", err)
		}
		return p
	}
	// Rs 5 entered with 18% GST on top: Rs 5.90 is saved and warned about.
	w.svc.saveFoundingServicePrice(ctx, "delivery_fee", product(`{"ref":"DELIVERY-FEE","label":"Delivery","price_ttc":"5.00000000","tva_tx":"18.000"}`))
	if got := w.svc.foundingDeliveryFee(ctx); got != 5.9 {
		t.Fatalf("saved DELIVERY-FEE: %v want 5.9", got)
	}
	if !strings.Contains(logs.String(), "DELIVERY-FEE with its GST is not the published One Voice fee") || !strings.Contains(logs.String(), "erp_with_gst=5.9") {
		t.Fatalf("no drift warning for Rs 5.90: %q", logs.String())
	}
	// Entered so it comes to exactly Rs 5.00: saved, no warning.
	logs.Reset()
	w.svc.saveFoundingServicePrice(ctx, "delivery_fee", product(`{"ref":"DELIVERY-FEE","label":"Delivery","price_ttc":"5.00000000","tva_tx":"0"}`))
	if got := w.svc.foundingDeliveryFee(ctx); got != 5 {
		t.Fatalf("saved DELIVERY-FEE: %v want 5", got)
	}
	if strings.Contains(logs.String(), "DELIVERY-FEE") {
		t.Fatalf("a Rs 5.00 DELIVERY-FEE was warned about: %q", logs.String())
	}
}

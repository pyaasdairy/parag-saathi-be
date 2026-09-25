package consumer

import (
	"context"
	"testing"
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

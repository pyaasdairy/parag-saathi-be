package consumer

// Founding Family (founding.go): the view and its 404, joining (Rs 99 exactly
// once, WALLET_SHORT with the shortfall, FARM_UNLOCKED, ALREADY_MEMBER), the
// unlock that turns every waiting member active with the three notifications,
// stop with perks to the end of the paid month, member pricing on orders and
// subscriptions, and the monthly billing with its rollover and retry-then-stop.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run Founding -v

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

// The bill date rolls on the anchor day-of-month, clamped to the month's
// last day, never overflowing into the month after.
func TestFoundingNextBillDateRollover(t *testing.T) {
	cases := []struct {
		prev   string
		anchor int
		want   string
	}{
		{"2026-01-31", 31, "2026-02-28"},
		{"2026-02-28", 31, "2026-03-31"}, // the anchor survives the short month
		{"2026-08-31", 31, "2026-09-30"},
		{"2026-12-15", 15, "2027-01-15"}, // year roll
		{"2028-01-31", 31, "2028-02-29"}, // leap day
		{"2026-10-05", 0, "2026-11-05"},  // no anchor: the day of prev
		{"garbage", 3, "garbage"},
	}
	for _, c := range cases {
		if got := nextBillDate(c.prev, c.anchor); got != c.want {
			t.Errorf("nextBillDate(%s, %d) = %s want %s", c.prev, c.anchor, got, c.want)
		}
	}
}

// Member pricing on the catalog index: level 3 is the ERP's explicit price
// when present, else level 1 minus Rs 2 per litre on PYAAS milk only;
// Parag, ghee and 200 ml trial packs are never discounted.
func TestFoundingMemberPriceResolution(t *testing.T) {
	docs := []catalogDoc{
		{Kind: catalogKindProduct, SkuID: "pyaas-toned-1l", Name: "Toned Milk - PYAAS", Category: "milk", Variant: "1L Carton", Unit: "1 L", Price: fp(85)},
		{Kind: catalogKindProduct, SkuID: "pyaas-toned-pouch", Name: "Toned Milk - PYAAS", Category: "milk", Variant: "500ml Pouch", Unit: "500 ml", Price: fp(51)},
		{Kind: catalogKindProduct, SkuID: "pyaas-a2-1l", Name: "A2 Cow Milk - PYAAS", Category: "milk", Variant: "1L Carton", Unit: "1 L", Price: fp(139), MemberPrice: fp(137)},
		{Kind: catalogKindAddition, SkuID: "dol-pys-toned-450ml", BaseID: "PYS-TONED-450ML", Name: "Toned Milk", Category: "milk", Variant: "450 ML", Price: fp(48)},
		{Kind: catalogKindAddition, SkuID: "dol-pys-trial-200ml", BaseID: "PYS-TRIAL-200ML", Name: "Trial Pack", Category: "milk", Variant: "200 ML", Price: fp(20)},
		{Kind: catalogKindAddition, SkuID: "dol-pys-ghee-500ml", BaseID: "PYS-GHEE-500ML", Name: "Bilona Ghee", Category: "ghee", Variant: "500 ML", Price: fp(1499)},
		{Kind: catalogKindProduct, SkuID: "gold-1l", Name: "Full Cream - Parag Gold", Category: "milk", Variant: "1L", Price: fp(71)},
		{Kind: catalogKindAddition, SkuID: "dol-pys-wfm", BaseID: "PYS-WFM", Name: "Whole Farm Milk", Category: "milk", Price: fp(86), Variants: []variantDoc{
			{VariantID: "b1", Label: "1 L Bottle", Price: 85, VolumeMl: 1000},
			{VariantID: "p1", Label: "1 L Pouch", Price: 85, VolumeMl: 1000, MemberPrice: fp(82)},
		}},
	}
	ix := buildPriceIndex(docs)
	cases := []struct {
		sku, variant string
		member       bool
		want         float64
	}{
		{"pyaas-toned-1l", "", true, 83},        // 1 L: Rs 2 off
		{"pyaas-toned-1l", "", false, 85},       // non-member: level 1
		{"pyaas-toned-pouch", "", true, 50},     // 500 ml: Rs 1 off
		{"pyaas-a2-1l", "", true, 137},          // explicit ERP level 3 wins
		{"dol-pys-toned-450ml", "", true, 47},   // 450 ml: 0.9 rounds to Rs 1
		{"dol-pys-trial-200ml", "", true, 20},   // 200 ml: 0.4 rounds to 0
		{"dol-pys-ghee-500ml", "", true, 1499},  // ghee: never
		{"gold-1l", "", true, 71},               // Parag: never
		{"dol-pys-wfm", "1 L Bottle", true, 83}, // variant litres from volume
		{"dol-pys-wfm", "1 L Pouch", true, 82},  // variant explicit level 3
	}
	for _, c := range cases {
		got, ok := ix.priceForMember(c.sku, c.variant, c.member)
		if !ok || got != c.want {
			t.Errorf("priceForMember(%s,%q,%v) = (%v,%v) want %v", c.sku, c.variant, c.member, got, ok, c.want)
		}
	}
	if sku, p, ok := ix.cheapestPyaasLitre(); !ok || sku != "pyaas-toned-1l" || p != 85 {
		t.Fatalf("cheapest 1 L PYAAS line: %s %v %v", sku, p, ok)
	}
	if !isPyaasMilkSKU("pyaas-toned-1l", "", "milk") || isPyaasMilkSKU("taaza-1l", "PRG-TONED-1LTR", "milk") || isPyaasMilkSKU("dol-pys-ghee-500ml", "PYS-GHEE-500ML", "ghee") {
		t.Fatalf("PYAAS milk identification")
	}
	if l := litresOf(0, "1L Carton"); l != 1 {
		t.Fatalf("litresOf(1L Carton) = %v", l)
	}
}

func foundingAPI(t *testing.T, w *chainWorld, cid primitive.ObjectID, method, path, body string, fn http.HandlerFunc) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex(), Phone: "+919000000000"}))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec.Code, rec.Body.String()
}

func seedTestFarms(t *testing.T, w *chainWorld, unlocksAt int) {
	t.Helper()
	_, err := w.svc.upsertFoundingFarms(context.Background(), []foundingFarmInput{
		{ID: "gonard-dairy", Name: "Gonard Dairy", Farmer: "Harsh Singh", Place: "Paraspur", UnlocksAt: unlocksAt},
		{ID: "mishra-dairy", Name: "Mishra Dairy", Farmer: "Abhishek Mishra", Place: "Gonda", UnlocksAt: 80},
	}, "test")
	if err != nil {
		t.Fatalf("seed farms: %v", err)
	}
}

func crmEventCount(t *testing.T, w *chainWorld, topic string, cid primitive.ObjectID) int64 {
	t.Helper()
	filter := bson.D{{Key: "topic", Value: topic}}
	if !cid.IsZero() {
		filter = append(filter, bson.E{Key: "consumer_id", Value: cid})
	}
	n, err := w.db.Collection(collCRMEvents).CountDocuments(context.Background(), filter)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return n
}

func TestFoundingFamilyJoinUnlockStopAndPricing(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}
	// A PYAAS line on the catalog so member pricing has something to bite on.
	p := 85.0
	if _, err := w.db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
		SkuID: "pyaas-toned-1l", Kind: catalogKindProduct, Price: &p, Name: "Toned Milk - PYAAS", Category: "milk", Unit: "1 L", Variant: "1L Carton",
	}); err != nil {
		t.Fatalf("seed pyaas: %v", err)
	}

	// No farms yet: the programme is closed and the app says opening soon.
	a := w.customer(t, "9000009001", 500)
	code, body := foundingAPI(t, w, a, http.MethodGet, "/founding-family", "", h.foundingView)
	if code != 404 || !strings.Contains(body, "NOT_AVAILABLE") {
		t.Fatalf("closed programme: %d %s", code, body)
	}
	seedTestFarms(t, w, 2)

	// The view in the app's shape, savings from the 1 L PYAAS line.
	code, body = foundingAPI(t, w, a, http.MethodGet, "/founding-family", "", h.foundingView)
	if code != 200 {
		t.Fatalf("view: %d %s", code, body)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("view body: %v %s", err, body)
	}
	if view["price_month"] != 99.0 || view["member"] != nil {
		t.Fatalf("view: %v", view)
	}
	farms, _ := view["farms"].([]any)
	if len(farms) != 2 {
		t.Fatalf("farms: %v", view["farms"])
	}
	f0, _ := farms[0].(map[string]any)
	for _, k := range []string{"id", "name", "farmer", "place", "note", "photo_url", "unlocks_at", "claimed", "status", "unlocked_packs"} {
		if _, ok := f0[k]; !ok {
			t.Fatalf("farm key %q missing: %v", k, f0)
		}
	}
	if f0["id"] != "gonard-dairy" || f0["status"] != farmFilling || f0["claimed"] != 0.0 || f0["unlocks_at"] != 2.0 || f0["note"] != nil {
		t.Fatalf("farm row: %v", f0)
	}
	// delivery_fee is what a non-member really pays per morning delivery:
	// nothing while FOUNDING_PYAAS_NONMEMBER_FEE is off, so the app's line
	// (level1 + fee) * 30 - (level3 * 30 + 99) never promises a saving on a
	// Rs 5 fee nobody is charged (it hides the line when there is none).
	sav, _ := view["savings"].(map[string]any)
	if sav["level1_per_litre"] != 85.0 || sav["level3_per_litre"] != 83.0 || sav["delivery_fee"] != 0.0 {
		t.Fatalf("savings: %v", sav)
	}
	w.svc.deps.Cfg.FoundingPyaasNonMemberFee = true
	if sv := w.svc.foundingSavings(ctx); sv == nil || sv.DeliveryFee != 5 {
		t.Fatalf("savings with the non-member fee switched on: %+v", sv)
	}
	w.svc.deps.Cfg.FoundingPyaasNonMemberFee = false

	// A short wallet: WALLET_SHORT names the shortfall and carries it as a field.
	poor := w.customer(t, "9000009002", 60)
	code, body = foundingAPI(t, w, poor, http.MethodPost, "/founding-family/join", `{"farm_id":"gonard-dairy"}`, h.foundingJoin)
	var short struct {
		Code      string  `json:"code"`
		Message   string  `json:"message"`
		Shortfall float64 `json:"shortfall"`
	}
	_ = json.Unmarshal([]byte(body), &short)
	if code != 422 || short.Code != "WALLET_SHORT" || short.Shortfall != 39 || !strings.Contains(short.Message, "short by 39") {
		t.Fatalf("wallet short: %d %s", code, body)
	}
	if f, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy"); f.Claimed != 0 {
		t.Fatalf("a refused join took a seat: %d", f.Claimed)
	}
	if n, _ := w.db.Collection(collFoundingMembers).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: poor}}); n != 0 {
		t.Fatalf("a refused join left a member row")
	}

	// A joins: Rs 99 leaves the wallet once, labelled FOUNDING-99, seat #1, waiting.
	code, body = foundingAPI(t, w, a, http.MethodPost, "/founding-family/join", `{"farm_id":"gonard-dairy"}`, h.foundingJoin)
	if code != 200 {
		t.Fatalf("join: %d %s", code, body)
	}
	var joined struct {
		Member foundingMemberView `json:"member"`
	}
	_ = json.Unmarshal([]byte(body), &joined)
	m := joined.Member
	if m.Status != memberWaiting || m.FarmID != "gonard-dairy" || m.LineNumber == nil || *m.LineNumber != 1 || m.ReferralCode == nil || m.JoinedAt == nil || m.NextBillDate != nil {
		t.Fatalf("member after join: %+v", m)
	}
	wv, _ := w.svc.wallet(ctx, a)
	if wv.Available != 401 {
		t.Fatalf("wallet after join: %v want 401", wv.Available)
	}
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: a}, {Key: "remark", Value: foundingLedgerLabel}, {Key: "ref_type", Value: "founding"}}); n != 1 {
		t.Fatalf("FOUNDING-99 ledger rows: %d want 1", n)
	}
	// A second join is refused and takes nothing.
	if code, body = foundingAPI(t, w, a, http.MethodPost, "/founding-family/join", `{"farm_id":"mishra-dairy"}`, h.foundingJoin); code != 409 || !strings.Contains(body, "ALREADY_MEMBER") {
		t.Fatalf("second join: %d %s", code, body)
	}
	if wv, _ = w.svc.wallet(ctx, a); wv.Available != 401 {
		t.Fatalf("second join moved money: %v", wv.Available)
	}
	if crmEventCount(t, w, "founding.seat_waiting", a) != 1 {
		t.Fatalf("seat_waiting for A: %d", crmEventCount(t, w, "founding.seat_waiting", a))
	}
	// Waiting is not active: PYAAS milk still bills at level 1.
	if w.svc.foundingActive(ctx, a) {
		t.Fatalf("a waiting member must not have member pricing")
	}

	// B takes the last seat: the farm unlocks, both members turn active with
	// month one starting today, and the unlock + you-are-in messages go out.
	b := w.customer(t, "9000009003", 500)
	code, body = foundingAPI(t, w, b, http.MethodPost, "/founding-family/join", `{"farm_id":"gonard-dairy"}`, h.foundingJoin)
	if code != 200 {
		t.Fatalf("join B: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &joined)
	today := istToday(time.Now())
	if joined.Member.Status != memberActive || joined.Member.NextBillDate == nil || *joined.Member.NextBillDate != nextBillDate(today, 0) || *joined.Member.LineNumber != 2 {
		t.Fatalf("member B after the unlock: %+v", joined.Member)
	}
	farm, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy")
	if farm.Status != farmUnlocked || farm.Claimed != 2 || farm.UnlockedAt == nil {
		t.Fatalf("farm after the unlock: %+v", farm)
	}
	ma, _ := w.svc.repo.findFoundingMember(ctx, a)
	if ma.Status != memberActive || ma.NextBillDate != nextBillDate(today, 0) || ma.BillDay == 0 {
		t.Fatalf("member A after the unlock: %+v", ma)
	}
	for _, cid := range []primitive.ObjectID{a, b} {
		if crmEventCount(t, w, "founding.farm_unlocked", cid) != 1 || crmEventCount(t, w, "founding.member_active", cid) != 1 {
			t.Fatalf("unlock notifications for %s: unlocked=%d active=%d", cid.Hex(),
				crmEventCount(t, w, "founding.farm_unlocked", cid), crmEventCount(t, w, "founding.member_active", cid))
		}
	}
	if crmEventCount(t, w, "founding.seat_waiting", b) != 0 {
		t.Fatalf("the seat that unlocked the farm must not also say waiting")
	}
	// Claims are closed: a third home is refused.
	c := w.customer(t, "9000009004", 500)
	if code, body = foundingAPI(t, w, c, http.MethodPost, "/founding-family/join", `{"farm_id":"gonard-dairy"}`, h.foundingJoin); code != 409 || !strings.Contains(body, "FARM_UNLOCKED") {
		t.Fatalf("join an unlocked farm: %d %s", code, body)
	}

	// Pricing: A's order bills PYAAS at level 3 with no delivery fee; a
	// non-member pays level 1 and the usual fee; Parag is level 1 for both.
	ord, err := w.svc.createOrder(ctx, a.Hex(), orderInput{
		Items: []orderItem{
			{ProductID: "pyaas-toned-1l", Name: "Toned Milk - PYAAS", Qty: 1, Price: 85},
			{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35},
		},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning", ConsumerName: "A", Phone: "9000009001",
	})
	if err != nil {
		t.Fatalf("member order: %v", err)
	}
	if ord.Items[0].Price != 83 || ord.Items[1].Price != 35 || ord.DeliveryFee != 0 || ord.Total != 118 {
		t.Fatalf("member order pricing: %+v fee %v total %v", ord.Items, ord.DeliveryFee, ord.Total)
	}
	ord2, err := w.svc.createOrder(ctx, c.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "pyaas-toned-1l", Name: "Toned Milk - PYAAS", Qty: 1, Price: 85}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning", ConsumerName: "C", Phone: "9000009004",
	})
	if err != nil {
		t.Fatalf("non-member order: %v", err)
	}
	if ord2.Items[0].Price != 85 || ord2.DeliveryFee != deliveryFee {
		t.Fatalf("non-member order pricing: %+v fee %v", ord2.Items, ord2.DeliveryFee)
	}
	// The catalog carries both prices on the PYAAS line, none on Parag.
	cat, err := w.svc.repo.catalogView(ctx, true)
	if err != nil {
		t.Fatalf("catalogView: %v", err)
	}
	w.svc.decorateMemberPrices(ctx, cat)
	seen := map[string]*float64{}
	for _, ad := range cat.Additions {
		seen[ad.ID] = ad.MemberPrice
	}
	if seen["pyaas-toned-1l"] == nil || *seen["pyaas-toned-1l"] != 83 || seen["gold-500ml"] != nil {
		t.Fatalf("catalog member prices: %v", seen)
	}
	// A subscription created by A carries the member price; its morning
	// order re-reads the standing.
	sub, err := w.svc.createSubscription(ctx, a, subscriptionInput{ProductID: "pyaas-toned-1l", Qty: 1, Frequency: "daily", StartDate: today})
	if err != nil {
		t.Fatalf("member subscription: %v", err)
	}
	if sub.UnitPrice != 83 {
		t.Fatalf("member subscription price: %v", sub.UnitPrice)
	}
	if lp := w.svc.subscriptionLinePrice(ctx, sub); lp != 83 {
		t.Fatalf("line price for an active member: %v", lp)
	}

	// Stop: perks run to the end of the paid month, then end.
	code, body = foundingAPI(t, w, a, http.MethodPost, "/founding-family/stop", "", h.foundingStop)
	if code != 200 {
		t.Fatalf("stop: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &joined)
	if joined.Member.Status != memberStopped {
		t.Fatalf("member after stop: %+v", joined.Member)
	}
	ma, _ = w.svc.repo.findFoundingMember(ctx, a)
	if ma.PerksUntil != addDaysIST(nextBillDate(today, 0), -1) {
		t.Fatalf("perks_until after stop: %q", ma.PerksUntil)
	}
	if !w.svc.foundingActive(ctx, a) {
		t.Fatalf("perks must run to the end of the paid month")
	}
	if _, active := w.svc.foundingStanding(ctx, a, addDaysIST(ma.PerksUntil, 1)); active {
		t.Fatalf("perks must end after the paid month")
	}
	if lp := w.svc.subscriptionLinePrice(ctx, sub); lp != 83 {
		t.Fatalf("line price while perks still run: %v", lp)
	}
	// A stopped member is not billed again (B, still active, is), and can
	// re-join a filling farm.
	if billed, stopped := w.svc.billFoundingMembers(ctx, time.Now().AddDate(0, 2, 0)); billed != 1 || stopped != 0 {
		t.Fatalf("billing two months on: billed=%d stopped=%d want 1 (B) and 0", billed, stopped)
	}
	if ma, _ = w.svc.repo.findFoundingMember(ctx, a); ma.Status != memberStopped || ma.LastBillDate != "" {
		t.Fatalf("billing touched the stopped member: %+v", ma)
	}
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: a}, {Key: "remark", Value: foundingLedgerLabel}}); n != 1 {
		t.Fatalf("FOUNDING-99 rows for a stopped member: %d want 1", n)
	}
	code, body = foundingAPI(t, w, a, http.MethodPost, "/founding-family/join", `{"farm_id":"mishra-dairy"}`, h.foundingJoin)
	if code != 200 {
		t.Fatalf("re-join: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &joined)
	if joined.Member.Status != memberWaiting || joined.Member.FarmID != "mishra-dairy" || *joined.Member.LineNumber != 1 {
		t.Fatalf("member after re-join: %+v", joined.Member)
	}
	// Inside the paid month the re-join is free (owner rule, 24 Sep; see
	// TestFoundingRejoinInsidePaidMonth for the whole rule).
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: a}, {Key: "remark", Value: foundingLedgerLabel}}); n != 1 {
		t.Fatalf("FOUNDING-99 rows after a re-join inside the paid month: %d want 1", n)
	}
}

// Monthly billing: Rs 99 on the bill day exactly once, the date rolls a month
// on its anchor, a short wallet is retried for three days and then stops the
// membership with the perks ending the day before the missed bill.
func TestFoundingBillingRolloverAndRetryThenStop(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 1)
	a := w.customer(t, "9000009101", 300)
	if _, err := w.svc.joinFoundingFamily(ctx, a, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	m, _ := w.svc.repo.findFoundingMember(ctx, a)
	if m.Status != memberActive {
		t.Fatalf("a one-seat farm unlocks on the first join: %+v", m)
	}
	// Pin the bill date to a 31st so the rollover is exercised.
	if _, err := w.db.Collection(collFoundingMembers).UpdateByID(ctx, m.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "next_bill_date", Value: "2026-10-31"}, {Key: "bill_day", Value: 31}}}}); err != nil {
		t.Fatalf("pin bill date: %v", err)
	}
	at := func(day string, hour int) time.Time {
		d, _ := parseDay(day)
		return d.Add(time.Duration(hour) * time.Hour)
	}
	// Before the bill day: nothing.
	if billed, _ := w.svc.billFoundingMembers(ctx, at("2026-10-30", 9)); billed != 0 {
		t.Fatalf("billed before the bill day")
	}
	// On the bill day: Rs 99 once, however many ticks run; the date rolls to 30 Nov.
	for i := 0; i < 3; i++ {
		w.svc.billFoundingMembers(ctx, at("2026-10-31", 9+i))
	}
	wv, _ := w.svc.wallet(ctx, a)
	if wv.Available != 300-99-99 {
		t.Fatalf("wallet after the first month: %v want 102", wv.Available)
	}
	m, _ = w.svc.repo.findFoundingMember(ctx, a)
	if m.NextBillDate != "2026-11-30" || m.LastBillDate != "2026-10-31" || m.BillAttempts != 0 {
		t.Fatalf("after the first bill: %+v", m)
	}
	// Second month bills 30 Nov and rolls back onto the 31st.
	w.svc.billFoundingMembers(ctx, at("2026-11-30", 9))
	m, _ = w.svc.repo.findFoundingMember(ctx, a)
	if m.NextBillDate != "2026-12-31" {
		t.Fatalf("anchor day lost: %q", m.NextBillDate)
	}
	if wv, _ = w.svc.wallet(ctx, a); wv.Available != 3 {
		t.Fatalf("wallet after two months: %v", wv.Available)
	}
	// Third month: the wallet is short. Three billing days of retries, then stopped.
	for i, day := range []string{"2026-12-31", "2027-01-01", "2027-01-02"} {
		billed, stopped := w.svc.billFoundingMembers(ctx, at(day, 9))
		m, _ = w.svc.repo.findFoundingMember(ctx, a)
		if billed != 0 {
			t.Fatalf("day %d billed a short wallet", i)
		}
		if i < 2 && (stopped != 0 || m.Status != memberActive || m.BillAttempts != i+1) {
			t.Fatalf("retry day %d: stopped=%d %+v", i, stopped, m)
		}
		if i == 2 && (stopped != 1 || m.Status != memberStopped || m.StopReason != "wallet_short" || m.PerksUntil != "2026-12-30") {
			t.Fatalf("after three short days: stopped=%d %+v", stopped, m)
		}
	}
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: a}, {Key: "remark", Value: foundingLedgerLabel}}); n != 3 {
		t.Fatalf("FOUNDING-99 rows: %d want 3 (join + two months)", n)
	}
	// A wallet top-up after the stop bills nothing: the member must re-join.
	if _, err := w.svc.creditTopup(ctx, a, 500, "test", "topup-after-stop"); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if billed, _ := w.svc.billFoundingMembers(ctx, at("2027-01-03", 9)); billed != 0 {
		t.Fatalf("a stopped member was billed")
	}
	// The members-only gate, when the founder switches it on, refuses a
	// PYAAS line for a non-member, and says which farm a waiting member is
	// waiting on. Off by default: nothing above needed it.
	w.svc.deps.Cfg.FoundingPyaasMembersOnly = true
	defer func() { w.svc.deps.Cfg.FoundingPyaasMembersOnly = false }()
	p := 85.0
	if _, err := w.db.Collection(collCatalog).InsertOne(ctx, catalogDoc{
		SkuID: "pyaas-toned-1l", Kind: catalogKindProduct, Price: &p, Name: "Toned Milk - PYAAS", Category: "milk", Unit: "1 L",
	}); err != nil {
		t.Fatalf("seed pyaas: %v", err)
	}
	nonMember := w.customer(t, "9000009103", 300)
	_, err := w.svc.createOrder(ctx, nonMember.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "pyaas-toned-1l", Qty: 1, Price: 85}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1", Lane: "morning",
	})
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "FOUNDING_REQUIRED" || !strings.Contains(ae.Message, "Unlock it with Founding Family") {
		t.Fatalf("gate for a non-member: %v", err)
	}
	if _, err := w.svc.createOrder(ctx, nonMember.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1", Lane: "morning",
	}); err != nil {
		t.Fatalf("the gate must never touch Parag: %v", err)
	}
	waiting := w.customer(t, "9000009102", 300)
	if _, err := w.svc.joinFoundingFamily(ctx, waiting, "mishra-dairy"); err != nil {
		t.Fatalf("join mishra: %v", err)
	}
	_, err = w.svc.createSubscription(ctx, waiting, subscriptionInput{ProductID: "pyaas-toned-1l", Qty: 1, Frequency: "daily"})
	if !errors.As(err, &ae) || ae.Code != "FOUNDING_REQUIRED" || !strings.Contains(ae.Message, "Opens when Mishra Dairy unlocks") {
		t.Fatalf("gate for a waiting member: %v", err)
	}
}

// The admin farm endpoint: SUPER_ADMIN upserts the founder's records without
// touching seats; a farm set to unlocked by hand activates its members.
func TestFoundingAdminFarmsUpsert(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}
	put := func(body string) (int, string) {
		req := httptest.NewRequest(http.MethodPut, "/admin/founding/farms", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(auth.WithActor(req.Context(), auth.Actor{PartyID: adminKeyActorID, Kind: "service", RoleCode: "SUPER_ADMIN"}))
		rec := httptest.NewRecorder()
		h.adminPutFoundingFarms(rec, req)
		return rec.Code, rec.Body.String()
	}
	code, body := put(`{"farms":[{"name":"Gonard Dairy","farmer":"Harsh Singh","place":"Paraspur","unlocks_at":65,"photo_url":"https://x/g.jpg"}]}`)
	if code != 200 || !strings.Contains(body, `"id":"gonard-dairy"`) || !strings.Contains(body, `"claimed":0`) {
		t.Fatalf("put: %d %s", code, body)
	}
	if code, body = put(`{"farms":[{"name":"Nameless","farmer":"","unlocks_at":10}]}`); code != 400 {
		t.Fatalf("invalid farm accepted: %d %s", code, body)
	}
	a := w.customer(t, "9000009201", 300)
	if _, err := w.svc.joinFoundingFamily(ctx, a, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	// An edit keeps the seat; a hand unlock activates the waiting member.
	code, body = put(`{"farms":[{"id":"gonard-dairy","name":"Gonard Dairy","farmer":"Harsh Singh","unlocks_at":65,"unlocked_packs":"Whole Farm Milk 1 L bottle and pouch","status":"unlocked"}]}`)
	if code != 200 || !strings.Contains(body, `"claimed":1`) || !strings.Contains(body, `"status":"unlocked"`) {
		t.Fatalf("hand unlock: %d %s", code, body)
	}
	if m, _ := w.svc.repo.findFoundingMember(ctx, a); m.Status != memberActive {
		t.Fatalf("member after a hand unlock: %+v", m)
	}
	if crmEventCount(t, w, "founding.farm_unlocked", a) != 1 {
		t.Fatalf("unlock notification after a hand unlock")
	}
	// The seed path never touches an existing farm.
	added, err := SeedFoundingFarms(ctx, w.db, []byte(`{"farms":[{"id":"gonard-dairy","name":"Other","farmer":"X","unlocks_at":1},{"id":"new-farm","name":"New Farm","farmer":"Y","unlocks_at":10}]}`))
	if err != nil || added != 1 {
		t.Fatalf("seed: added=%d err=%v", added, err)
	}
	if f, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy"); f.Name != "Gonard Dairy" || f.Claimed != 1 || f.Status != farmUnlocked {
		t.Fatalf("seed overwrote a live farm: %+v", f)
	}
}

// The billing worker ticks hourly, but a short wallet is retried for three
// billing DAYS (spec 5.6, "we will try again tomorrow"): every failed tick on
// one IST day counts as that day's single attempt, and only the third
// distinct short day stops the membership. A top-up later the same day is
// still picked up by the next tick.
func TestFoundingBillingRetriesOncePerDay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 1)
	a := w.customer(t, "9000009111", 99)
	if _, err := w.svc.joinFoundingFamily(ctx, a, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	m, _ := w.svc.repo.findFoundingMember(ctx, a)
	if m.Status != memberActive {
		t.Fatalf("a one-seat farm unlocks on the first join: %+v", m)
	}
	if _, err := w.db.Collection(collFoundingMembers).UpdateByID(ctx, m.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "next_bill_date", Value: "2026-10-31"}, {Key: "bill_day", Value: 31}}}}); err != nil {
		t.Fatalf("pin bill date: %v", err)
	}
	at := func(day string, hour int) time.Time {
		d, _ := parseDay(day)
		return d.Add(time.Duration(hour) * time.Hour)
	}
	// Bill day, empty wallet, a full day of hourly ticks: one attempt, still active.
	for h := 0; h < 24; h++ {
		if _, stopped := w.svc.billFoundingMembers(ctx, at("2026-10-31", h)); stopped != 0 {
			t.Fatalf("stopped at %02d:00 on the first short day", h)
		}
	}
	m, _ = w.svc.repo.findFoundingMember(ctx, a)
	if m.Status != memberActive || m.BillAttempts != 1 {
		t.Fatalf("after 24 hourly ticks on one day: status=%s attempts=%d, want active and 1", m.Status, m.BillAttempts)
	}
	// Day two, hourly again: attempt two, still active.
	for h := 0; h < 24; h++ {
		w.svc.billFoundingMembers(ctx, at("2026-11-01", h))
	}
	m, _ = w.svc.repo.findFoundingMember(ctx, a)
	if m.Status != memberActive || m.BillAttempts != 2 {
		t.Fatalf("after the second short day: status=%s attempts=%d, want active and 2", m.Status, m.BillAttempts)
	}
	// Day three: the third distinct short day stops it on its first tick.
	if _, stopped := w.svc.billFoundingMembers(ctx, at("2026-11-02", 0)); stopped != 1 {
		t.Fatalf("the third short day must stop the membership")
	}
	m, _ = w.svc.repo.findFoundingMember(ctx, a)
	if m.Status != memberStopped || m.StopReason != "wallet_short" || m.PerksUntil != "2026-10-30" {
		t.Fatalf("after three short days: %+v", m)
	}

	// A top-up later on a short day is billed by the next tick, and the
	// retry count starts again.
	b := w.customer(t, "9000009112", 0)
	bm := &foundingMember{ID: primitive.NewObjectID(), ConsumerID: b, FarmID: "gonard-dairy", Status: memberActive,
		LineNumber: 2, NextBillDate: "2026-10-31", BillDay: 31, Joins: 1}
	if _, err := w.db.Collection(collFoundingMembers).InsertOne(ctx, bm); err != nil {
		t.Fatalf("seed member b: %v", err)
	}
	w.svc.billFoundingMembers(ctx, at("2026-10-31", 9))
	if _, err := w.svc.creditTopup(ctx, b, 150, "test", "topup-same-day"); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if billed, _ := w.svc.billFoundingMembers(ctx, at("2026-10-31", 15)); billed != 1 {
		t.Fatalf("a same-day top-up must be billed by the next tick")
	}
	if got, _ := w.svc.repo.findFoundingMember(ctx, b); got.Status != memberActive || got.NextBillDate != "2026-11-30" || got.BillAttempts != 0 || got.LastAttemptDate != "" {
		t.Fatalf("after the same-day top-up: %+v", got)
	}
}

// Stop, then re-join inside the paid month (owner rule, 24 Sep): the month is
// already paid, so the re-join charges nothing and keeps the perks. The farm
// the member held can be taken back even though it unlocked (claims are
// closed to newcomers, not to its own member); another farm still filling
// seats them as waiting with the paid month intact, and its unlock bills
// from the day after that month. A join after perks_until is a fresh Rs 99
// join, charged once.
func TestFoundingRejoinInsidePaidMonth(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}
	seedTestFarms(t, w, 1)
	rows := func(cid primitive.ObjectID) int64 {
		n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "remark", Value: foundingLedgerLabel}})
		return n
	}
	join := func(cid primitive.ObjectID, farm string) (int, foundingMemberView, string) {
		code, body := foundingAPI(t, w, cid, http.MethodPost, "/founding-family/join", `{"farm_id":"`+farm+`"}`, h.foundingJoin)
		var out struct {
			Member foundingMemberView `json:"member"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		return code, out.Member, body
	}
	stop := func(cid primitive.ObjectID) {
		if code, body := foundingAPI(t, w, cid, http.MethodPost, "/founding-family/stop", "", h.foundingStop); code != 200 {
			t.Fatalf("stop: %d %s", code, body)
		}
	}

	a := w.customer(t, "9000009301", 500)
	if code, m, body := join(a, "gonard-dairy"); code != 200 || m.Status != memberActive {
		t.Fatalf("first join: %d %s", code, body)
	}
	first, _ := w.svc.repo.findFoundingMember(ctx, a)
	paidBill := first.NextBillDate
	stop(a)

	// 1) Back to the farm A held, now unlocked: active again, same line, same
	//    bill date, no charge.
	code, m, body := join(a, "gonard-dairy")
	if code != 200 || m.Status != memberActive || m.LineNumber == nil || *m.LineNumber != 1 || m.NextBillDate == nil || *m.NextBillDate != paidBill {
		t.Fatalf("re-join the own unlocked farm inside the paid month: %d %s", code, body)
	}
	if wv, _ := w.svc.wallet(ctx, a); wv.Available != 401 || rows(a) != 1 {
		t.Fatalf("a re-join inside the paid month charged: wallet %v, FOUNDING-99 rows %d", wv.Available, rows(a))
	}
	if f, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy"); f.Claimed != 1 {
		t.Fatalf("taking back one's own seat must not add a seat: claimed %d", f.Claimed)
	}
	if !w.svc.foundingActive(ctx, a) {
		t.Fatalf("perks after taking the seat back")
	}

	// 2) Stop again, re-join a farm still filling: waiting there, perks kept
	//    through the paid month, still no charge.
	stop(a)
	perks := addDaysIST(paidBill, -1)
	code, m, body = join(a, "mishra-dairy")
	if code != 200 || m.Status != memberWaiting || m.FarmID != "mishra-dairy" || *m.LineNumber != 1 {
		t.Fatalf("re-join a filling farm inside the paid month: %d %s", code, body)
	}
	if wv, _ := w.svc.wallet(ctx, a); wv.Available != 401 || rows(a) != 1 {
		t.Fatalf("a re-join inside the paid month charged: wallet %v, rows %d", wv.Available, rows(a))
	}
	if got, _ := w.svc.repo.findFoundingMember(ctx, a); got.PerksUntil != perks {
		t.Fatalf("perks_until lost on re-join: %q want %q", got.PerksUntil, perks)
	}
	if !w.svc.foundingActive(ctx, a) {
		t.Fatalf("the paid month's perks must survive a re-join onto a filling farm")
	}
	if _, active := w.svc.foundingStanding(ctx, a, addDaysIST(perks, 1)); active {
		t.Fatalf("perks must end with the paid month while still waiting")
	}
	// Mishra unlocks: month two bills the day after the paid month, not a
	// month after the unlock.
	if _, err := w.svc.upsertFoundingFarms(ctx, []foundingFarmInput{
		{ID: "mishra-dairy", Name: "Mishra Dairy", Farmer: "Abhishek Mishra", Place: "Gonda", UnlocksAt: 80, Status: farmUnlocked},
	}, "test"); err != nil {
		t.Fatalf("hand unlock: %v", err)
	}
	got, _ := w.svc.repo.findFoundingMember(ctx, a)
	if got.Status != memberActive || got.NextBillDate != paidBill || got.PerksUntil != "" {
		t.Fatalf("activation of a paid-through member: %+v (want next bill %s)", got, paidBill)
	}

	// 3) After perks_until: a fresh Rs 99 join, charged exactly once; the
	//    unlocked farms (its own included) are closed to it.
	stop(a)
	if _, err := w.db.Collection(collFoundingMembers).UpdateOne(ctx, bson.D{{Key: "consumer_id", Value: a}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "perks_until", Value: addDaysIST(istToday(time.Now()), -1)}}}}); err != nil {
		t.Fatalf("age perks: %v", err)
	}
	if code, _, body = join(a, "mishra-dairy"); code != 409 || !strings.Contains(body, "FARM_UNLOCKED") {
		t.Fatalf("own unlocked farm after the paid month: %d %s", code, body)
	}
	if _, err := w.svc.upsertFoundingFarms(ctx, []foundingFarmInput{
		{ID: "third-farm", Name: "Third Farm", Farmer: "Ranjeet Singh", UnlocksAt: 10},
	}, "test"); err != nil {
		t.Fatalf("third farm: %v", err)
	}
	code, m, body = join(a, "third-farm")
	if code != 200 || m.Status != memberWaiting {
		t.Fatalf("fresh join after the paid month: %d %s", code, body)
	}
	if wv, _ := w.svc.wallet(ctx, a); wv.Available != 302 || rows(a) != 2 {
		t.Fatalf("a fresh join must charge Rs 99 once: wallet %v, rows %d", wv.Available, rows(a))
	}
	if got, _ = w.svc.repo.findFoundingMember(ctx, a); got.PerksUntil != "" || w.svc.foundingActive(ctx, a) {
		t.Fatalf("a fresh waiting join carries no perks: %+v", got)
	}
}

// The line number is the member's identity on the card, so it is never
// handed out twice on one farm: a waiting member who stops gives the SEAT
// back (claimed goes down, the farm needs one more home) but not the
// number. A#1 B#2 C#3, B stops, D joins -> D is #4, claimed is 3.
func TestFoundingLineNumbersNeverRepeat(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 10)
	join := func(phone string) (primitive.ObjectID, int) {
		cid := w.customer(t, phone, 300)
		m, err := w.svc.joinFoundingFamily(ctx, cid, "gonard-dairy")
		if err != nil || m.LineNumber == nil {
			t.Fatalf("join %s: %v %+v", phone, err, m)
		}
		return cid, *m.LineNumber
	}
	_, la := join("9000009401")
	b, lb := join("9000009402")
	_, lc := join("9000009403")
	if la != 1 || lb != 2 || lc != 3 {
		t.Fatalf("lines: %d %d %d want 1 2 3", la, lb, lc)
	}
	if _, err := w.svc.stopFoundingFamily(ctx, b); err != nil {
		t.Fatalf("stop B: %v", err)
	}
	if f, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy"); f.Claimed != 2 {
		t.Fatalf("claimed after B stops: %d want 2", f.Claimed)
	}
	_, ld := join("9000009404")
	if ld != 4 {
		t.Fatalf("D's line: %d want 4 (3 is still C's)", ld)
	}
	if f, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy"); f.Claimed != 3 {
		t.Fatalf("claimed after D joins: %d want 3 (the live seat count)", f.Claimed)
	}
	// A failed debit gives the seat back too, and its number is never reused.
	poor := w.customer(t, "9000009405", 0)
	if _, err := w.svc.joinFoundingFamily(ctx, poor, "gonard-dairy"); err == nil {
		t.Fatalf("an empty wallet joined")
	}
	_, le := join("9000009406")
	if le <= ld {
		t.Fatalf("E's line %d must follow D's %d", le, ld)
	}
}

// Five taps on "Join" at once (a double-tap, a retry racing the first
// request) make ONE member, take ONE seat and move Rs 99 ONCE. The service's
// pre-check cannot promise that on its own - all five read "not a member"
// before any of them writes - so the consumer_id unique index is the guard,
// and the test world builds it exactly as the boot does.
func TestFoundingConcurrentJoinIsOneMemberOneSeatOneDebit(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 10)
	a := w.customer(t, "9000009501", 500)
	const n = 5
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			_, err := w.svc.joinFoundingFamily(ctx, a, "gonard-dairy")
			errs <- err
		}()
	}
	close(start)
	ok := 0
	for i := 0; i < n; i++ {
		var ae *apiError
		if err := <-errs; err == nil {
			ok++
		} else if !errors.As(err, &ae) || ae.Code != "ALREADY_MEMBER" {
			t.Fatalf("a losing join must say ALREADY_MEMBER: %v", err)
		}
	}
	rows, _ := w.db.Collection(collFoundingMembers).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: a}})
	f, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy")
	debits, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "consumer_id", Value: a}, {Key: "remark", Value: foundingLedgerLabel}})
	wv, _ := w.svc.wallet(ctx, a)
	if ok != 1 || rows != 1 || f.Claimed != 1 || debits != 1 || wv.Available != 401 {
		t.Fatalf("5 concurrent joins: ok=%d member rows=%d seats=%d FOUNDING-99 debits=%d wallet=%v; want 1 1 1 1 401",
			ok, rows, f.Claimed, debits, wv.Available)
	}
}

// When the boot could not build the unique index that makes a join (or a
// referral apply) exactly-once, those two money paths refuse with a flat
// 503 {code, message} instead of trusting the racy pre-check, and move
// nothing. The guard retries the build, so once the index exists the paths
// open again without a restart. A 503 is what the app's referral outbox
// replays later; the join screen shows the message.
func TestFoundingAndReferralRefuseWithoutTheirUniqueIndex(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}
	seedTestFarms(t, w, 10)
	broken := func(context.Context) error { return errors.New("index build failed") }
	flat := func(code int, body, want string) {
		t.Helper()
		var e map[string]any
		if err := json.Unmarshal([]byte(body), &e); err != nil || code != http.StatusServiceUnavailable ||
			e["code"] != want || e["message"] == nil || e["message"] == "" || len(e) != 2 {
			t.Fatalf("want a flat 503 %s {code,message}, got %d %s", want, code, body)
		}
	}

	a := w.customer(t, "9000009601", 500)
	w.svc.foundingIdx.markMissing(broken)
	code, body := foundingAPI(t, w, a, http.MethodPost, "/founding-family/join", `{"farm_id":"gonard-dairy"}`, h.foundingJoin)
	flat(code, body, "FOUNDING_UNAVAILABLE")
	if f, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy"); f.Claimed != 0 {
		t.Fatalf("a refused join took a seat")
	}
	if wv, _ := w.svc.wallet(ctx, a); wv.Available != 500 {
		t.Fatalf("a refused join moved money: %v", wv.Available)
	}
	// The view and stop are not money-at-risk paths and keep answering.
	if code, body = foundingAPI(t, w, a, http.MethodGet, "/founding-family", "", h.foundingView); code != 200 {
		t.Fatalf("the view must not depend on the guard: %d %s", code, body)
	}
	// The index can be built again: the next join rebuilds it and goes through.
	w.svc.foundingIdx.markMissing(w.svc.repo.ensureFoundingIndexes)
	if code, body = foundingAPI(t, w, a, http.MethodPost, "/founding-family/join", `{"farm_id":"gonard-dairy"}`, h.foundingJoin); code != 200 {
		t.Fatalf("join once the index is back: %d %s", code, body)
	}

	referrer := w.customer(t, "9000009602", 0)
	refCode, err := w.svc.referralCode(ctx, referrer)
	if err != nil {
		t.Fatalf("referral code: %v", err)
	}
	b := w.customer(t, "9000009603", 0)
	w.svc.referralIdx.markMissing(broken)
	code, body = foundingAPI(t, w, b, http.MethodPost, "/referrals/apply", `{"code":"`+refCode+`"}`, h.applyReferral)
	flat(code, body, "REFERRAL_UNAVAILABLE")
	if n, _ := w.db.Collection(collReferrals).CountDocuments(ctx, bson.D{{Key: "referee_id", Value: b}}); n != 0 {
		t.Fatalf("a refused apply stored a referral")
	}
	w.svc.referralIdx.markMissing(w.svc.repo.ensureReferralIndexes)
	if code, body = foundingAPI(t, w, b, http.MethodPost, "/referrals/apply", `{"code":"`+refCode+`"}`, h.applyReferral); code != 200 {
		t.Fatalf("apply once the index is back: %d %s", code, body)
	}
}

// The billing worker bills at boot, like the subscription worker. It used to
// wait a full hour before its first tick, and every deploy (and a free-plan
// instance spinning down when idle) restarts the process, so an instance
// that never stayed up an hour never billed: members kept level-3 prices and
// free delivery without paying.
func TestFoundingBillingWorkerBillsAtBoot(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 1)
	a := w.customer(t, "9000009121", 198)
	if _, err := w.svc.joinFoundingFamily(ctx, a, "gonard-dairy"); err != nil {
		t.Fatalf("join: %v", err)
	}
	m, _ := w.svc.repo.findFoundingMember(ctx, a)
	// Due since yesterday, so the check holds across an IST midnight.
	due := addDaysIST(istToday(time.Now()), -1)
	if _, err := w.db.Collection(collFoundingMembers).UpdateByID(ctx, m.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "next_bill_date", Value: due}}}}); err != nil {
		t.Fatalf("due: %v", err)
	}
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	go w.svc.foundingBillingWorker(wctx)
	ref := "founding:bill:" + m.ID.Hex() + ":" + due
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "ref_id", Value: ref}})
		if n == 1 {
			if fresh, _ := w.svc.repo.findFoundingMember(ctx, a); fresh != nil && fresh.NextBillDate != due {
				break // billed and rolled to the next month
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker did not bill at boot: %d rows for %s", n, ref)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
}

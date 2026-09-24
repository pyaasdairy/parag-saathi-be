package consumer

// Referrals (referrals.go): the code is the app's own derivation so shared
// codes keep attributing, a referee links once, the reward pays both sides
// exactly once on the referee's first delivery, and the ledger reads in the
// shape lib/referrals.ts types.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run Referral -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The derivation must match lib/referrals.ts codeFromUid byte for byte:
// h = h*31 + charCode, unsigned 32-bit; ("PG" + base36 upper + "XXXX")[:6].
// Vectors computed with the JavaScript algorithm.
func TestReferralCodeMatchesTheAppDerivation(t *testing.T) {
	cases := map[string]string{
		"68d1c2f3a4b5c6d7e8f90123": "PG1W1A",
		"000000000000000000000000": "PG18H7",
		"ffffffffffffffffffffffff": "PG1VVN",
		"u_9876543210":             "PGUZXI",
		"":                         "PG0XXX",
	}
	for uid, want := range cases {
		if got := referralCodeFor(uid); got != want {
			t.Errorf("referralCodeFor(%q) = %q want %q", uid, got, want)
		}
	}
	// Stable: the same id always yields the same code, and it is six chars.
	for i := 0; i < 3; i++ {
		if c := referralCodeFor("68d1c2f3a4b5c6d7e8f90123"); c != "PG1W1A" || len(c) != 6 {
			t.Fatalf("unstable code %q", c)
		}
	}
	if normalizeReferralCode(" pg1w1a ") != "PG1W1A" {
		t.Fatalf("codes are matched case-insensitively and trimmed")
	}
}

func referralAPI(t *testing.T, w *chainWorld, cid primitive.ObjectID, method, path, body string, fn http.HandlerFunc) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex(), Phone: "+919000000000"}))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec.Code, rec.Body.String()
}

func TestReferralApplyLinksOnceAndRewardsExactlyOnce(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}

	referrer := w.customer(t, "9000007001", 0)
	referee := w.customer(t, "9000007002", 500)
	other := w.customer(t, "9000007003", 0)

	// GET /referrals/code mints the derived code into the account, once.
	code, body := referralAPI(t, w, referrer, http.MethodGet, "/referrals/code", "", h.referralCode)
	if code != 200 {
		t.Fatalf("code: %d %s", code, body)
	}
	var got struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(body), &got)
	if got.Code != referralCodeFor(referrer.Hex()) {
		t.Fatalf("code %q want the app derivation %q", got.Code, referralCodeFor(referrer.Hex()))
	}
	if acct, _ := w.svc.repo.findAccountByID(ctx, referrer); acct.ReferralCode == nil || *acct.ReferralCode != got.Code {
		t.Fatalf("code not stored on the account: %+v", acct.ReferralCode)
	}
	if again, _ := w.svc.referralCode(ctx, referrer); again != got.Code {
		t.Fatalf("code changed on a second read: %q", again)
	}

	// The referee applies it (lower case, padded - the app upper-cases but
	// a typed code may not be).
	code, body = referralAPI(t, w, referee, http.MethodPost, "/referrals/apply", `{"code":" `+strings.ToLower(got.Code)+` "}`, h.applyReferral)
	if code != 200 {
		t.Fatalf("apply: %d %s", code, body)
	}
	var applied applyReferralResult
	_ = json.Unmarshal([]byte(body), &applied)
	if applied.Status != referralPending || applied.RewardAmount != 100 || applied.Code != got.Code {
		t.Fatalf("apply body: %+v", applied)
	}
	// Idempotent: the same code again is the same link, not a second one.
	code, body = referralAPI(t, w, referee, http.MethodPost, "/referrals/apply", `{"code":"`+got.Code+`"}`, h.applyReferral)
	var again applyReferralResult
	_ = json.Unmarshal([]byte(body), &again)
	if code != 200 || again.ID != applied.ID {
		t.Fatalf("second apply: %d %s (want the same link %s)", code, body, applied.ID)
	}
	if n, _ := w.db.Collection(collReferrals).CountDocuments(ctx, bson.D{{Key: "referee_id", Value: referee}}); n != 1 {
		t.Fatalf("referral rows for the referee: %d want 1", n)
	}
	// A different friend's code after the link is refused. (Accounts minted
	// in the same second derive the SAME code - the app's hash keeps the top
	// base-36 digits and same-second ids differ only in the counter - so the
	// third account gets a stored code, which always wins over derivation.)
	if _, err := w.svc.repo.updateAccount(ctx, other, bson.D{{Key: "referral_code", Value: "PGOTHR"}}); err != nil {
		t.Fatalf("store other code: %v", err)
	}
	otherCode, _ := w.svc.referralCode(ctx, other)
	if otherCode != "PGOTHR" {
		t.Fatalf("a stored code must win over the derivation: %q", otherCode)
	}
	if code, body = referralAPI(t, w, referee, http.MethodPost, "/referrals/apply", `{"code":"`+otherCode+`"}`, h.applyReferral); code != 409 || !strings.Contains(body, "ALREADY_REFERRED") {
		t.Fatalf("second referrer: %d %s", code, body)
	}
	// Self-referral refused; unknown code is a 404 in the {code,message} envelope.
	if code, body = referralAPI(t, w, referrer, http.MethodPost, "/referrals/apply", `{"code":"`+got.Code+`"}`, h.applyReferral); code != 422 || !strings.Contains(body, "SELF_REFERRAL") {
		t.Fatalf("self referral: %d %s", code, body)
	}
	code, body = referralAPI(t, w, other, http.MethodPost, "/referrals/apply", `{"code":"PGNOPE"}`, h.applyReferral)
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &envelope)
	if code != 404 || envelope.Code != "REFERRAL_CODE_NOT_FOUND" || envelope.Message == "" {
		t.Fatalf("unknown code: %d %s", code, body)
	}
	if evs, _ := w.db.Collection(collCRMEvents).CountDocuments(ctx, bson.D{{Key: "topic", Value: "referral.applied"}}); evs != 1 {
		t.Fatalf("referral.applied events: %d want 1", evs)
	}

	// The referrer's ledger: one pending row in the app's shape.
	code, body = referralAPI(t, w, referrer, http.MethodGet, "/referrals", "", h.listReferrals)
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("list body: %v %s", err, body)
	}
	for _, k := range []string{"id", "name", "status", "reward_amount", "created_at"} {
		if _, ok := rows[0][k]; !ok {
			t.Fatalf("list row missing %q: %s", k, body)
		}
	}
	if rows[0]["status"] != referralPending || rows[0]["reward_amount"] != 100.0 || rows[0]["name"] != "Family ending 7002" {
		t.Fatalf("list row: %v", rows[0])
	}
	// The referee's own ledger is empty (they referred nobody).
	if code, body = referralAPI(t, w, referee, http.MethodGet, "/referrals", "", h.listReferrals); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("referee list: %d %s", code, body)
	}

	// The referee's first delivered order pays both sides Rs 100 in REWARDS,
	// once the rider can no longer undo it (the reward sweep after the hold).
	afterHold := time.Now().Add(referralRewardHold + time.Hour)
	ord, err := w.svc.createOrder(ctx, referee.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Referee", Phone: "9000007002",
	})
	if err != nil {
		t.Fatalf("createOrder: %v", err)
	}
	task := chainTaskFor(t, w, ord.OrderID)
	chainOutForDelivery(t, w, task.ID)
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task.ID, deliverInput{ProofPhoto: "p.jpg", Geo: &geoPt{Lat: task.Geo.Lat, Lng: task.Geo.Lng}, GeofenceOK: true}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n := w.svc.payDueReferralRewards(ctx, afterHold); n != 1 {
		t.Fatalf("the reward sweep credited %d referral(s), want 1", n)
	}
	rw, _ := w.svc.wallet(ctx, referrer)
	ew, _ := w.svc.wallet(ctx, referee)
	if rw.Rewards != 100 || ew.Rewards != 100 {
		t.Fatalf("rewards after the first delivery: referrer %v referee %v want 100 each", rw.Rewards, ew.Rewards)
	}
	if ew.Cash != 500-ord.Total {
		t.Fatalf("the delivery itself still settled from cash: %v want %v", ew.Cash, 500-ord.Total)
	}
	// Exactly once: a replayed delivered sync and another sweep move no more
	// money and emit no second referral.rewarded; the ledger row reads credited.
	w.svc.syncOrderDelivered(ctx, chainTaskFor(t, w, ord.OrderID))
	w.svc.payDueReferralRewards(ctx, afterHold)
	rw, _ = w.svc.wallet(ctx, referrer)
	ew, _ = w.svc.wallet(ctx, referee)
	if rw.Rewards != 100 || ew.Rewards != 100 {
		t.Fatalf("rewards after a replay: referrer %v referee %v", rw.Rewards, ew.Rewards)
	}
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "ref_id", Value: bson.D{{Key: "$regex", Value: "^referral:"}}}}); n != 2 {
		t.Fatalf("referral ledger rows: %d want 2", n)
	}
	if evs, _ := w.db.Collection(collCRMEvents).CountDocuments(ctx, bson.D{{Key: "topic", Value: "referral.rewarded"}}); evs != 2 {
		t.Fatalf("referral.rewarded events: %d want 2 (one per side)", evs)
	}
	_, body = referralAPI(t, w, referrer, http.MethodGet, "/referrals", "", h.listReferrals)
	rows = nil
	_ = json.Unmarshal([]byte(body), &rows)
	if len(rows) != 1 || rows[0]["status"] != referralCredited {
		t.Fatalf("ledger after the reward: %s", body)
	}
	// A second delivery for the referee pays nothing more.
	ord2, err := w.svc.createOrder(ctx, referee.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow",
		Lane: "morning", ConsumerName: "Referee", Phone: "9000007002",
	})
	if err != nil {
		t.Fatalf("createOrder 2: %v", err)
	}
	task2 := chainTaskFor(t, w, ord2.OrderID)
	chainOutForDelivery(t, w, task2.ID)
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task2.ID, deliverInput{ProofPhoto: "p.jpg", Geo: &geoPt{Lat: task2.Geo.Lat, Lng: task2.Geo.Lng}, GeofenceOK: true}); err != nil {
		t.Fatalf("deliver 2: %v", err)
	}
	w.svc.payDueReferralRewards(ctx, afterHold)
	if rw, _ = w.svc.wallet(ctx, referrer); rw.Rewards != 100 {
		t.Fatalf("a second delivery paid the referrer again: %v", rw.Rewards)
	}
}

// A code shared from the phone before the server ever minted it still
// resolves: the derivation is run over the accounts that hold no code, and
// the match is stored so the next lookup is indexed.
func TestReferralDerivedCodeResolvesWithoutAStoredOne(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	referrer := w.customer(t, "9000007101", 0)
	referee := w.customer(t, "9000007102", 0)
	if acct, _ := w.svc.repo.findAccountByID(ctx, referrer); acct.ReferralCode != nil {
		t.Fatalf("precondition: no stored code")
	}
	code := referralCodeFor(referrer.Hex())
	out, err := w.svc.applyReferral(ctx, referee, code)
	if err != nil || out.Code != code {
		t.Fatalf("apply by derived code: %v %+v", err, out)
	}
	acct, _ := w.svc.repo.findAccountByID(ctx, referrer)
	if acct.ReferralCode == nil || *acct.ReferralCode != code {
		t.Fatalf("the match was not stored: %+v", acct.ReferralCode)
	}
	ref, _ := w.svc.repo.findReferralByReferee(ctx, referee)
	if ref == nil || ref.ReferrerID != referrer {
		t.Fatalf("link: %+v", ref)
	}
}

// Two accounts created in the same millisecond with the same derived code
// (same-second ObjectIDs collide in the four base-36 characters): the
// OLDER account must win every time, whichever row Mongo returns first
// for the created_at tie. The ObjectID breaks the tie - its counter
// orders accounts created within one second - so the answer never
// depends on insertion or index order.
func TestReferralCodeCollisionTieGoesToTheOlderAccount(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	var older, younger primitive.ObjectID
	for i := 0; i < 1000 && older.IsZero(); i++ {
		a, b := primitive.NewObjectID(), primitive.NewObjectID()
		if referralCodeFor(a.Hex()) == referralCodeFor(b.Hex()) {
			older, younger = a, b
		}
	}
	if older.IsZero() {
		t.Skip("no colliding same-second pair found")
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	// The younger row goes in first, so natural order favours it.
	for _, id := range []primitive.ObjectID{younger, older} {
		if err := w.svc.repo.insertAccount(ctx, &account{ID: id, Phone: "+91" + id.Hex()[14:], Status: "ACTIVE", CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatalf("account: %v", err)
		}
	}
	code := referralCodeFor(older.Hex())
	referee := w.customer(t, "9000007301", 0)
	out, err := w.svc.applyReferral(ctx, referee, code)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	ref, _ := w.svc.repo.findReferralByReferee(ctx, referee)
	if ref == nil || ref.ReferrerID != older || out.Code != code {
		t.Fatalf("the derived-code tie went to %v, want the older account %s", ref, older.Hex())
	}
	// The match was stored on the older account; the stored-code lookup
	// breaks the same tie the same way once both hold the code.
	if _, err := w.svc.repo.mintReferralCode(ctx, younger); err != nil {
		t.Fatalf("mint younger: %v", err)
	}
	if got, _ := w.svc.repo.findAccountByReferralCode(ctx, code); got == nil || got.ID != older {
		t.Fatalf("the stored-code tie went to %+v, want %s", got, older.Hex())
	}
}

// chainDeliver takes a placed order through pickup and a proof-of-delivery
// drop, settling it exactly as the rider's route does.
func chainDeliver(t *testing.T, w *chainWorld, orderID string) {
	t.Helper()
	tk := chainTaskFor(t, w, orderID)
	chainOutForDelivery(t, w, tk.ID)
	if _, err := w.svc.deliverDelivery(context.Background(), w.rider, tk.ID, deliverInput{
		ProofPhoto: "p.jpg", Geo: &geoPt{Lat: tk.Geo.Lat, Lng: tk.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliver %s: %v", orderID, err)
	}
}

// Spec 5.8: a referral counts only when the friend pays. A Rs 0 Welcome
// Litre pack and an order paid entirely from promo (REWARDS) money are
// deliveries the referee never paid for, so they leave the referral pending;
// the first delivery that takes the referee's own money credits both sides.
func TestReferralRewardWaitsForAPaidDelivery(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	referrer := w.customer(t, "9000007101", 0)
	referee := w.customer(t, "9000007102", 0) // no cash yet
	code, err := w.svc.referralCode(ctx, referrer)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if _, err := w.svc.applyReferral(ctx, referee, code); err != nil {
		t.Fatalf("apply: %v", err)
	}
	afterHold := time.Now().Add(referralRewardHold + time.Hour)
	pending := func(step string) {
		t.Helper()
		// The reward sweep, run past every delivery's undo window, must still
		// find nothing the friend paid for.
		w.svc.payDueReferralRewards(ctx, afterHold)
		ref, _ := w.svc.repo.findReferralByReferee(ctx, referee)
		rw, _ := w.svc.wallet(ctx, referrer)
		if ref == nil || ref.Status != referralPending || rw.Rewards != 0 {
			t.Fatalf("%s: the referral paid out on a delivery the friend did not pay for: %+v referrer rewards %v", step, ref, rw.Rewards)
		}
	}

	// 1) The Rs 0 Welcome Litre pack.
	acct, _ := w.svc.repo.findAccountByID(ctx, referee)
	addrs, _ := w.svc.repo.listAddresses(ctx, referee)
	pack, err := w.svc.mintPromoPackOrder(ctx, acct, &addrs[0], addDaysIST(istToday(time.Now()), 1), 1)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	chainDeliver(t, w, pack.OrderID)
	pending("Rs 0 promo pack")

	// 2) An order paid in full from promo money.
	if _, err := w.svc.creditRewards(ctx, referee, 60, "test:promo:7102", "test promo"); err != nil {
		t.Fatalf("promo: %v", err)
	}
	morning := orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning",
	}
	promoPaid, err := w.svc.createOrder(ctx, referee.Hex(), morning)
	if err != nil {
		t.Fatalf("promo order: %v", err)
	}
	chainDeliver(t, w, promoPaid.OrderID)
	pending("order paid from REWARDS")

	// 3) The first delivery the referee pays for with their own money.
	if _, err := w.svc.creditTopup(ctx, referee, 500, "test", "test:topup:7102"); err != nil {
		t.Fatalf("topup: %v", err)
	}
	paid, err := w.svc.createOrder(ctx, referee.Hex(), morning)
	if err != nil {
		t.Fatalf("paid order: %v", err)
	}
	chainDeliver(t, w, paid.OrderID)
	w.svc.payDueReferralRewards(ctx, afterHold)
	ref, _ := w.svc.repo.findReferralByReferee(ctx, referee)
	rw, _ := w.svc.wallet(ctx, referrer)
	if ref == nil || ref.Status != referralCredited || ref.RewardOrderID != paid.OrderID || rw.Rewards != 100 {
		t.Fatalf("the first paid delivery must credit the referral: %+v referrer rewards %v", ref, rw.Rewards)
	}
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "ref_id", Value: "referral:" + ref.ID.Hex() + ":referee"}}); n != 1 {
		t.Fatalf("referee reward rows: %d want 1", n)
	}
}

// A friend's code is for a new family: an account that has already had a
// paid delivery cannot apply one (it would pay Rs 100 to both sides for a
// customer the programme did not bring), and two members cannot refer each
// other (each side collected Rs 200). A Rs 0 Welcome Litre pack does not make
// a family established, and re-applying the code already linked stays 200.
func TestReferralApplyRefusesEstablishedAndCircular(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	h := &handler{svc: w.svc}
	a := w.customer(t, "9000007301", 500)
	b := w.customer(t, "9000007302", 500)
	c := w.customer(t, "9000007303", 500)
	d := w.customer(t, "9000007304", 0)
	// Distinct stored codes: same-second accounts derive colliding ones.
	for cid, code := range map[primitive.ObjectID]string{a: "PGAAA1", b: "PGBBB2"} {
		if _, err := w.db.Collection(collAccounts).UpdateOne(ctx, bson.D{{Key: "_id", Value: cid}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "referral_code", Value: code}}}}); err != nil {
			t.Fatalf("code: %v", err)
		}
	}
	apply := func(cid primitive.ObjectID, code string) (int, string) {
		return referralAPI(t, w, cid, http.MethodPost, "/referrals/apply", `{"code":"`+code+`"}`, h.applyReferral)
	}
	notEligible := func(step string, status int, body string) {
		t.Helper()
		if status != 422 || !strings.Contains(body, `"code":"REFERRAL_NOT_ELIGIBLE"`) {
			t.Fatalf("%s: %d %s want 422 REFERRAL_NOT_ELIGIBLE", step, status, body)
		}
	}

	// B joins with A's code; A then tries B's: circular.
	if code, body := apply(b, "PGAAA1"); code != 200 {
		t.Fatalf("B applies A: %d %s", code, body)
	}
	code, body := apply(a, "PGBBB2")
	notEligible("A applies the code of the family A referred", code, body)

	// C already had a paid delivery: an existing customer, not a new family.
	ord, err := w.svc.createOrder(ctx, c.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning",
	})
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	chainDeliver(t, w, ord.OrderID)
	code, body = apply(c, "PGAAA1")
	notEligible("a customer with a paid delivery", code, body)

	// D only received the Rs 0 Welcome Litre pack: still a new family.
	acct, _ := w.svc.repo.findAccountByID(ctx, d)
	addrs, _ := w.svc.repo.listAddresses(ctx, d)
	pack, err := w.svc.mintPromoPackOrder(ctx, acct, &addrs[0], addDaysIST(istToday(time.Now()), 1), 1)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	chainDeliver(t, w, pack.OrderID)
	if code, body := apply(d, "PGAAA1"); code != 200 {
		t.Fatalf("a family that only had the free pack: %d %s", code, body)
	}

	// B's own link survives B becoming a paying customer: re-apply is 200.
	ordB, err := w.svc.createOrder(ctx, b.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning",
	})
	if err != nil {
		t.Fatalf("order B: %v", err)
	}
	chainDeliver(t, w, ordB.OrderID)
	if code, body := apply(b, "PGAAA1"); code != 200 {
		t.Fatalf("re-applying the linked code: %d %s", code, body)
	}
	if n, _ := w.db.Collection(collReferrals).CountDocuments(ctx, bson.D{}); n != 2 {
		t.Fatalf("referral links: %d want 2 (B<-A, D<-A)", n)
	}
}

// PM-01: the reward paid the moment the referee's first delivery was marked,
// but the rider may undo a DELIVERED marking for riderUndoWindow, which hands
// the referee's money back and puts the order back out. After an undo and a
// fail the friend had paid nothing and the order was cancelled, yet both
// wallets kept Rs 100 and the referral read credited.
func TestReferralRewardNotPaidForADeliveryTheRiderUndid(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	referrer := w.customer(t, "9000007401", 0)
	referee := w.customer(t, "9000007402", 500)
	code, err := w.svc.referralCode(ctx, referrer)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if _, err := w.svc.applyReferral(ctx, referee, code); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rewards := func(cid primitive.ObjectID) float64 {
		t.Helper()
		wv, err := w.svc.wallet(ctx, cid)
		if err != nil {
			t.Fatalf("wallet: %v", err)
		}
		return wv.Rewards
	}
	unpaid := func(step string) {
		t.Helper()
		ref, _ := w.svc.repo.findReferralByReferee(ctx, referee)
		if ref == nil || ref.Status != referralPending || rewards(referrer) != 0 || rewards(referee) != 0 {
			t.Fatalf("%s: the referral paid out for a delivery the friend did not pay for: %+v referrer %v referee %v",
				step, ref, rewards(referrer), rewards(referee))
		}
	}

	past := func() time.Time { return time.Now().Add(referralRewardHold + time.Minute) }

	// The first delivery: inside the undo window nothing is paid.
	o := instantOrderDelivered(t, w, referee)
	unpaid("right after the delivery")
	if n := w.svc.payDueReferralRewards(ctx, time.Now()); n != 0 {
		t.Fatalf("a sweep inside the undo window paid %d referral(s)", n)
	}
	unpaid("a sweep inside the undo window")

	// The rider undoes it and it then fails at the door: once the window
	// has passed the sweep still pays nothing, and the stamp is gone.
	task := chainTaskFor(t, w, o.OrderID)
	if code := riderUndo(t, w, task.ID); code != 200 {
		t.Fatalf("rider undo: %d", code)
	}
	if _, err := w.svc.failDelivery(ctx, w.rider, task.ID, "Customer not at home"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if st := w.orderByID(t, o.OrderID).Status; st != "cancelled" || w.cash(t, referee) != 500 {
		t.Fatalf("setup: order %s, referee cash %v (want cancelled, 500)", st, w.cash(t, referee))
	}
	unpaid("after an undo and a fail")
	if n := w.svc.payDueReferralRewards(ctx, past()); n != 0 {
		t.Fatalf("the sweep paid %d referral(s) for an undone delivery", n)
	}
	unpaid("a sweep after the window, the delivery undone")
	if ref, _ := w.svc.repo.findReferralByReferee(ctx, referee); ref.RewardDueAt != nil {
		t.Fatalf("the stamp of an undone delivery must be cleared: %v", ref.RewardDueAt)
	}

	// The next delivery is undone and delivered again: the one that stands
	// pays both sides once, when the window has passed.
	o2 := instantOrderDelivered(t, w, referee)
	task2 := chainTaskFor(t, w, o2.OrderID)
	if code := riderUndo(t, w, task2.ID); code != 200 {
		t.Fatalf("rider undo 2: %d", code)
	}
	if _, err := w.svc.deliverDelivery(ctx, w.rider, task2.ID, deliverInput{
		ProofPhoto: "https://example.test/proof2.jpg", Geo: &geoPt{Lat: task2.Geo.Lat, Lng: task2.Geo.Lng}, GeofenceOK: true,
	}); err != nil {
		t.Fatalf("deliver again: %v", err)
	}
	unpaid("the redelivery, inside its window")
	if n := w.svc.payDueReferralRewards(ctx, past()); n != 1 {
		t.Fatalf("the sweep after the window credited %d referral(s), want 1", n)
	}
	ref, _ := w.svc.repo.findReferralByReferee(ctx, referee)
	if ref == nil || ref.Status != referralCredited || ref.RewardOrderID != o2.OrderID || ref.RewardDueAt != nil ||
		rewards(referrer) != 100 || rewards(referee) != 100 {
		t.Fatalf("the standing delivery must credit the referral: %+v referrer %v referee %v", ref, rewards(referrer), rewards(referee))
	}
	if cash := w.cash(t, referee); cash != 500-o2.Total {
		t.Fatalf("referee cash %v want %v (the undone order refunded, the standing one paid once)", cash, 500-o2.Total)
	}
	// Exactly once: another sweep and a replayed delivered sync add nothing.
	w.svc.syncOrderDelivered(ctx, chainTaskFor(t, w, o2.OrderID))
	if n := w.svc.payDueReferralRewards(ctx, past()); n != 0 {
		t.Fatalf("a second sweep credited %d referral(s)", n)
	}
	if n, _ := w.db.Collection(collWalletTxns).CountDocuments(ctx, bson.D{{Key: "ref_id", Value: bson.D{{Key: "$regex", Value: "^referral:"}}}}); n != 2 {
		t.Fatalf("referral ledger rows: %d want 2", n)
	}
	if evs, _ := w.db.Collection(collCRMEvents).CountDocuments(ctx, bson.D{{Key: "topic", Value: "referral.rewarded"}}); evs != 2 {
		t.Fatalf("referral.rewarded events: %d want 2 (one per side)", evs)
	}
}

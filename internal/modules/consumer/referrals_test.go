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

	// The referee's first delivered order pays both sides Rs 100 in REWARDS.
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
	rw, _ := w.svc.wallet(ctx, referrer)
	ew, _ := w.svc.wallet(ctx, referee)
	if rw.Rewards != 100 || ew.Rewards != 100 {
		t.Fatalf("rewards after the first delivery: referrer %v referee %v want 100 each", rw.Rewards, ew.Rewards)
	}
	if ew.Cash != 500-ord.Total {
		t.Fatalf("the delivery itself still settled from cash: %v want %v", ew.Cash, 500-ord.Total)
	}
	// Exactly once: a replayed delivered sync moves no more money and emits
	// no second referral.rewarded; the ledger row reads credited.
	w.svc.syncOrderDelivered(ctx, chainTaskFor(t, w, ord.OrderID))
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

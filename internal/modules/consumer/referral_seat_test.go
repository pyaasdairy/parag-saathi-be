package consumer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The founder's 25 Sep call: referral credit can pay the Rs 99 Founding
// Family seat, which turns a referral straight into a member. The Rs 100
// each side gets (a REWARDS credit, once the friend's first paid delivery
// stands) is spent before cash: the join's balance check counts it, the
// FOUNDING-99 debit takes it first, and a wallet of promo money alone pays
// the seat. The monthly Rs 99 draws the same way.
func TestReferralCreditPaysTheFoundingSeat(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 10)

	referrer := w.customer(t, "9000019501", 0) // no cash at all
	friend := w.customer(t, "9000019502", 0)
	referFriend(t, w, referrer, friend)
	if _, err := w.svc.creditTopup(ctx, friend, 100, "razorpay", "seat-topup-9502"); err != nil {
		t.Fatalf("topup: %v", err)
	}
	o, err := w.svc.createOrder(ctx, friend.Hex(), orderInput{
		Items:         []orderItem{{ProductID: "gold-500ml", Qty: 1, Price: 35}},
		PaymentMethod: "wallet", AddressLabel: "Home", AddressText: "Shop St 1, Lucknow", Lane: "morning",
	})
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	chainDeliver(t, w, o.OrderID) // Rs 35 + Rs 5 from the friend's own cash
	if n := w.svc.payDueReferralRewards(ctx, time.Now().Add(referralRewardHold+time.Hour)); n != 1 {
		t.Fatalf("referral rewards paid: %d want 1", n)
	}
	rw, _ := w.svc.wallet(ctx, referrer)
	if rw.Cash != 0 || rw.Rewards != 100 {
		t.Fatalf("referrer wallet before the join: cash %v rewards %v", rw.Cash, rw.Rewards)
	}

	// The referrer holds only the Rs 100 credit: it pays the seat.
	if _, err := w.svc.joinFoundingFamily(ctx, referrer, "gonard-dairy"); err != nil {
		t.Fatalf("a seat paid from referral credit was refused: %v", err)
	}
	rw, _ = w.svc.wallet(ctx, referrer)
	if rw.Cash != 0 || rw.Rewards != 1 {
		t.Fatalf("referrer wallet after the join: cash %v rewards %v want 0 and 1", rw.Cash, rw.Rewards)
	}
	seatRow := func(cid primitive.ObjectID) walletTxn {
		t.Helper()
		var row walletTxn
		if err := w.db.Collection(collWalletTxns).FindOne(ctx, bson.D{
			{Key: "consumer_id", Value: cid}, {Key: "remark", Value: foundingLedgerLabel}, {Key: "type", Value: "DEBIT"},
		}).Decode(&row); err != nil {
			t.Fatalf("FOUNDING-99 row: %v", err)
		}
		return row
	}
	if row := seatRow(referrer); row.Amount != 99 || row.Bucket != "REWARDS" || row.RefType != "founding" {
		t.Fatalf("the seat's ledger row: %+v", row)
	}

	// The friend holds cash and the credit: the credit goes first, the cash
	// is untouched.
	fw, _ := w.svc.wallet(ctx, friend)
	if fw.Cash != 60 || fw.Rewards != 100 {
		t.Fatalf("friend wallet before the join: cash %v rewards %v", fw.Cash, fw.Rewards)
	}
	if _, err := w.svc.joinFoundingFamily(ctx, friend, "gonard-dairy"); err != nil {
		t.Fatalf("friend join: %v", err)
	}
	if fw, _ = w.svc.wallet(ctx, friend); fw.Cash != 60 || fw.Rewards != 1 {
		t.Fatalf("friend wallet after the join: cash %v rewards %v want 60 and 1", fw.Cash, fw.Rewards)
	}

	// Credit plus cash together: Rs 50 of credit and Rs 49 of cash make the
	// Rs 99; one rupee short is WALLET_SHORT by exactly that rupee.
	mixed := w.customer(t, "9000019503", 49)
	if _, err := w.svc.creditRewards(ctx, mixed, 50, "test:seat:9503", "test credit"); err != nil {
		t.Fatalf("credit: %v", err)
	}
	if _, err := w.svc.joinFoundingFamily(ctx, mixed, "gonard-dairy"); err != nil {
		t.Fatalf("a seat from credit plus cash was refused: %v", err)
	}
	if mw, _ := w.svc.wallet(ctx, mixed); mw.Cash != 0 || mw.Rewards != 0 {
		t.Fatalf("mixed wallet after the join: cash %v rewards %v", mw.Cash, mw.Rewards)
	}
	short := w.customer(t, "9000019504", 0)
	if _, err := w.svc.creditRewards(ctx, short, 98, "test:seat:9504", "test credit"); err != nil {
		t.Fatalf("credit: %v", err)
	}
	_, err = w.svc.joinFoundingFamily(ctx, short, "gonard-dairy")
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "WALLET_SHORT" || ae.Shortfall != 1 {
		t.Fatalf("Rs 98 of credit for a Rs 99 seat: %v", err)
	}
}

// referFriend links friend to referrer's code, as the app's sign-up does.
// Accounts a test mints in the same second derive the SAME code (the app's
// hash; the oldest account wins a collision) and a join stores it, so the
// referrer is first given a stored code of its own, which always wins.
func referFriend(t *testing.T, w *chainWorld, referrer, friend primitive.ObjectID) {
	t.Helper()
	ctx := context.Background()
	own := "T" + strings.ToUpper(referrer.Hex()[14:])
	if _, err := w.svc.repo.updateAccount(ctx, referrer, bson.D{{Key: "referral_code", Value: own}}); err != nil {
		t.Fatalf("store code: %v", err)
	}
	code, err := w.svc.referralCode(ctx, referrer)
	if err != nil || code != own {
		t.Fatalf("code: %q %v", code, err)
	}
	if _, err := w.svc.applyReferral(ctx, friend, code); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

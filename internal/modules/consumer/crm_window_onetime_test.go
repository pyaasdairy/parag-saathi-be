package consumer

// Two founder questions, answered by walking the real code day by day:
//
//	1. "the ₹500 recharge window — is it working correctly?"
//	2. "it is one time only, for new users only, right?"
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 CRM_ENABLED=true \
//	  go test ./internal/modules/consumer/ -run 'CRMWindow|CRMOneTime' -v

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// enrolAged enrols a household and back-dates its first delivery by `daysAgo`
// IST days, so the offer is exactly as old as we want to test.
func enrolAged(t *testing.T, svc *service, phone string, daysAgo int) primitive.ObjectID {
	t.Helper()
	ctx := context.Background()
	res, err := svc.crmEnrol(ctx, "window-test", crmEnrolInput{
		Phone: phone, Name: "Window " + phone, Line1: "Flat " + phone + ", Window Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("enrol %s: %v", phone, err)
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
	// Pack 1 delivered `daysAgo` days ago — the anchor the whole window hangs on.
	at := time.Now().AddDate(0, 0, -daysAgo)
	if _, err := svc.repo.offers().UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "offer_id", Value: offerWelcomeLitre}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "pack1_state", Value: pack1Delivered},
			{Key: "first_delivery_at", Value: at.UTC()},
		}}}); err != nil {
		t.Fatalf("age offer: %v", err)
	}
	return cid
}

func pack2State(t *testing.T, svc *service, cid primitive.ObjectID) string {
	t.Helper()
	var o consumerOffer
	if err := svc.repo.offers().FindOne(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}}).Decode(&o); err != nil {
		t.Fatalf("offer: %v", err)
	}
	return o.Pack2State
}

// THE WINDOW, day by day. The offer says "₹500 within 7 days of the first
// delivery". Day 0 is the delivery day itself, so days 0..7 inclusive must
// work — eight calendar days — and day 8 must not.
func TestCRMWindowRechargeBoundaryDayByDay(t *testing.T) {
	svc, _, done := crmP0Service(t)
	defer done()
	ctx := context.Background()

	for day := 0; day <= 9; day++ {
		phone := fmt.Sprintf("90000020%02d", day)
		cid := enrolAged(t, svc, phone, day)

		// A qualifying ₹500 recharge, settled the normal way.
		if _, err := svc.creditTopup(ctx, cid, 500, "razorpay", "win-"+phone); err != nil {
			t.Fatalf("day %d recharge: %v", day, err)
		}
		svc.crmProcessEvents(ctx) // the outbox turns the settle into the CRM event

		got := pack2State(t, svc, cid)
		unlocked := got == pack2Pending || got == pack2Delivered
		want := day <= 7 // the published 7-day window, day 0 counted

		status := "REFUSED "
		if unlocked {
			status = "UNLOCKED"
		}
		t.Logf("  day %d: ₹500 recharge → pack2=%-9s %s", day, got, status)

		if unlocked != want {
			t.Errorf("day %d: unlocked=%v want %v (pack2=%s)", day, unlocked, want, got)
		}
	}
}

// The customer-facing side of the same boundary: once the window has closed the
// app must stop advertising the pack, even before the 10:30 sweep runs.
func TestCRMWindowOfferViewClosesOnDay8(t *testing.T) {
	svc, _, done := crmP0Service(t)
	defer done()

	for _, c := range []struct {
		day        int
		wantClosed bool
	}{{5, false}, {7, false}, {8, true}, {12, true}} {
		cid := enrolAged(t, svc, fmt.Sprintf("90000021%02d", c.day), c.day)
		var o consumerOffer
		_ = svc.repo.offers().FindOne(context.Background(),
			bson.D{{Key: "consumer_id", Value: cid}}).Decode(&o)
		closed := o.Pack2State == pack2Locked && o.FirstDeliveryAt != nil &&
			daysSinceFirstDelivery(&o, time.Now()) > crmOfferConfig().Pack2GraceDays
		if closed != c.wantClosed {
			t.Errorf("day %d: window closed=%v want %v", c.day, closed, c.wantClosed)
		}
		t.Logf("  day %d: offer view says window closed=%v", c.day, closed)
	}
}

// ONE TIME ONLY, NEW HOUSEHOLDS ONLY — every way someone could try to take it
// twice.
func TestCRMOneTimeOnlyForNewHouseholds(t *testing.T) {
	svc, _, done := crmP0Service(t)
	defer done()
	ctx := context.Background()

	// (a) the same phone cannot enrol twice.
	cid := enrolAged(t, svc, "9000003001", 0)
	if _, err := svc.crmEnrol(ctx, "window-test", crmEnrolInput{
		Phone: "9000003001", Name: "Repeat", Line1: "Flat 1, Window Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	}); err == nil {
		t.Fatal("the same phone enrolled a SECOND time")
	}

	// (b) a second ₹500 recharge does not buy a third pack.
	if _, err := svc.creditTopup(ctx, cid, 500, "razorpay", "one-1"); err != nil {
		t.Fatalf("recharge: %v", err)
	}
	svc.crmProcessEvents(ctx)
	if got := pack2State(t, svc, cid); got != pack2Pending {
		t.Fatalf("first recharge should unlock pack 2, got %s", got)
	}
	if _, err := svc.creditTopup(ctx, cid, 500, "razorpay", "one-2"); err != nil {
		t.Fatalf("second recharge: %v", err)
	}
	svc.crmProcessEvents(ctx)
	n, _ := svc.repo.orders.CountDocuments(ctx, bson.D{
		{Key: "user_id", Value: cid.Hex()}, {Key: "offer_pack", Value: 2},
	})
	if n > 1 {
		t.Fatalf("a second recharge minted %d pack-2 orders — the offer is not one-time", n)
	}

	// (c) a household that has already PAID is not a new household.
	payer := &account{
		ID: primitive.NewObjectID(), Phone: "+919000003002", Status: "ACTIVE",
		HasPaidOrder: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := svc.repo.insertAccount(ctx, payer); err != nil {
		t.Fatalf("payer: %v", err)
	}
	lat, lng := 26.7725, 81.0150
	_, _ = svc.repo.accounts.Database().Collection(collAddresses).InsertOne(ctx, &address{
		ID: primitive.NewObjectID(), ConsumerID: payer.ID, Label: "Home", Line1: "Paid St",
		Pincode: "226030", City: "Lucknow", IsDefault: true, Lat: &lat, Lng: &lng,
		CreatedAt: time.Now().UTC(),
	})
	if st, _ := svc.crmEligibility(ctx, payer.ID); st != "not_eligible" {
		t.Fatalf("a paying household reads %q, want not_eligible", st)
	}
	if _, err := svc.crmSelfEnrol(ctx, payer.ID, crmEnrolInput{}); err == nil {
		t.Fatal("a paying household was allowed to take the free offer")
	}

	// (d) delete the account and sign up again on the same phone — the claim
	// registry deliberately SURVIVES erasure, so the offer cannot be re-armed.
	if _, err := svc.repo.accounts.DeleteOne(ctx, bson.D{{Key: "_id", Value: cid}}); err != nil {
		t.Fatalf("erase: %v", err)
	}
	reborn := &account{
		ID: primitive.NewObjectID(), Phone: "+919000003001", Status: "ACTIVE",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := svc.repo.insertAccount(ctx, reborn); err != nil {
		t.Fatalf("re-signup: %v", err)
	}
	_, _ = svc.repo.accounts.Database().Collection(collAddresses).InsertOne(ctx, &address{
		ID: primitive.NewObjectID(), ConsumerID: reborn.ID, Label: "Home", Line1: "Reborn St",
		Pincode: "226030", City: "Lucknow", IsDefault: true, Lat: &lat, Lng: &lng,
		CreatedAt: time.Now().UTC(),
	})
	if st, _ := svc.crmEligibility(ctx, reborn.ID); st != "not_eligible" {
		t.Fatalf("erase-and-resignup reads %q — the offer was re-armed", st)
	}
}

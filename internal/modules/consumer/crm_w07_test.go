package consumer

// W-07 must be on record as SENT before pack 2 expires. A suppressed W-07
// (here: the per-trigger kill switch) leaves the pack locked; the next day's
// claim sends it and only then does the pack expire.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMW07 -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMW07SuppressedLeavesPackLocked(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	res, err := w.svc.crmEnrol(ctx, "w07-test-operator", crmEnrolInput{
		Phone: "9000007104", Name: "Expiry Household", Line1: "Flat 9, Grace Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)

	// Day 9 since pack 1 landed: past the grace window, pack 2 still locked.
	nowIST := time.Now().In(istZone)
	sched := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 11, 0, 0, 0, istZone)
	forceFirstDelivery(t, w.db, cid, sched.AddDate(0, 0, -9))
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Locked {
		t.Fatalf("pack2 must start locked: %+v", off)
	}

	// Kill W-07 (G1) so the dispatch is SUPPRESSED, then run the sweep.
	if err := w.svc.deps.Flags.Set(ctx, crmFlagPrefix+"W-07", false, "w07-test"); err != nil {
		t.Fatalf("kill switch: %v", err)
	}
	w.svc.crmProcessSchedules(ctx, sched)
	if got := crmDispatchStatuses(t, w.db, cid, "W-07"); len(got) != 1 || got[0] != "SUPPRESSED" {
		t.Fatalf("W-07 under the kill switch: %v", got)
	}
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Locked {
		t.Fatalf("a suppressed W-07 must leave pack 2 LOCKED, got %q", off.Pack2State)
	}

	// Switch back on: the day's claim is already taken, so the same day
	// cannot send, and the pack must still not expire.
	if err := w.svc.deps.Flags.Set(ctx, crmFlagPrefix+"W-07", true, "w07-test"); err != nil {
		t.Fatalf("kill switch off: %v", err)
	}
	w.svc.crmProcessSchedules(ctx, sched.Add(time.Minute))
	if got := crmDispatchStatuses(t, w.db, cid, "W-07"); len(got) != 1 {
		t.Fatalf("same-day retry must not claim twice: %v", got)
	}
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Locked {
		t.Fatalf("pack 2 must stay LOCKED until W-07 is SENT, got %q", off.Pack2State)
	}

	// Next day: a fresh claim, the message goes out, and only then the CAS.
	w.svc.crmProcessSchedules(ctx, sched.AddDate(0, 0, 1))
	if got := crmDispatchStatuses(t, w.db, cid, "W-07"); len(got) != 2 || got[1] != "SENT" {
		t.Fatalf("W-07 the next day: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "W-07"); n != 1 {
		t.Fatalf("W-07 inbox rows = %d, want 1", n)
	}
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Expired {
		t.Fatalf("pack 2 must expire once W-07 is SENT, got %q", off.Pack2State)
	}

	// The wrapper reports what the log recorded.
	if st, g := w.svc.crmDispatch(ctx, "W-07", cid, nil); st != "" || g != "" {
		t.Fatalf("claim already taken today must report nothing: %q %q", st, g)
	}
	if st, _ := w.svc.crmDispatch(ctx, "NO-SUCH-TRIGGER", cid, nil); st != "" {
		t.Fatalf("unknown trigger must report nothing: %q", st)
	}
}

// Pack 2 rides a real morning (terms 4.4-4.5): it is minted for tomorrow
// only when the plan will actually deliver tomorrow under the noon rule. A
// plan created, resumed or edited after today's noon starts the day after
// tomorrow, and a day the member skipped (tomorrow's preview cancelled)
// delivers nothing; either way the free pack would arrive alone.
func TestCRMPack2AttachFollowsTheNoonRule(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	household := func(phone string, changedAt time.Time) (primitive.ObjectID, *subscription) {
		cid := w.customer(t, phone, 500)
		sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "gold-500ml", Qty: 1, Frequency: "daily", StartDate: D}, chainPlanMadeAt)
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		chainBackdateSubscription(t, w, sub, changedAt)
		if _, err := w.db.Collection(collConsumerOffers).InsertOne(ctx, bson.D{
			{Key: "consumer_id", Value: cid}, {Key: "offer_id", Value: offerWelcomeLitre},
			{Key: "enrolled_at", Value: istDayAt(D, 8, 0).UTC()}, {Key: "pack1_state", Value: pack1Delivered},
			{Key: "pack2_state", Value: pack2Pending}, {Key: "subscription_id", Value: sub.SubscriptionID},
			{Key: "pack2_unlocked_at", Value: istDayAt(D, 9, 0).UTC()},
		}); err != nil {
			t.Fatalf("offer: %v", err)
		}
		return cid, sub
	}
	packsFor := func(cid primitive.ObjectID, day string) int64 {
		n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
			{Key: "user_id", Value: cid.Hex()}, {Key: "offer_pack", Value: 2}, {Key: "delivery_date", Value: day},
		})
		return n
	}

	// (a) the plan changed at 12:30 today: tomorrow does not deliver, so the
	// pack waits and rides the day after tomorrow, attached next morning.
	lateCID, _ := household("9000007201", istDayAt(D, 12, 30))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 40))
	if ok, err := w.svc.crmTryAttachPack2At(ctx, lateCID, istDayAt(D, 12, 45)); err != nil || ok || packsFor(lateCID, D1) != 0 {
		t.Fatalf("(a) pack 2 attached to a tomorrow the plan does not deliver: ok=%v err=%v packs=%d", ok, err, packsFor(lateCID, D1))
	}
	if ok, err := w.svc.crmTryAttachPack2At(ctx, lateCID, istDayAt(D1, 9, 0)); err != nil || !ok || packsFor(lateCID, D2) != 1 {
		t.Fatalf("(a) next morning the pack rides the plan's first day %s: ok=%v err=%v", D2, ok, err)
	}

	// (b) the member skipped tomorrow (cancelled its preview): no attach.
	skipCID, skipSub := household("9000007202", istDayAt(addDaysIST(D, -2), 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 30))
	prev := liveSubOrder(t, w, skipSub.SubscriptionID, D1)
	if prev == nil {
		t.Fatalf("(b) no preview for tomorrow")
	}
	if _, err := w.svc.cancelOrder(ctx, skipCID.Hex(), prev.OrderID); err != nil {
		t.Fatalf("(b) cancel: %v", err)
	}
	if ok, err := w.svc.crmTryAttachPack2At(ctx, skipCID, istDayAt(D, 10, 0)); err != nil || ok || packsFor(skipCID, D1) != 0 {
		t.Fatalf("(b) pack 2 attached to a skipped tomorrow: ok=%v err=%v", ok, err)
	}

	// (c) control: a plan from before the cut-off with tomorrow previewed.
	okCID, _ := household("9000007203", istDayAt(addDaysIST(D, -2), 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 45))
	if ok, err := w.svc.crmTryAttachPack2At(ctx, okCID, istDayAt(D, 10, 0)); err != nil || !ok || packsFor(okCID, D1) != 1 {
		t.Fatalf("(c) pack 2 must ride tomorrow's delivery: ok=%v err=%v", ok, err)
	}
}

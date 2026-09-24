package consumer

// The member-facing copy of the noon rule (owner, 24 Sep, R3(c)):
//   - D-07 goes out when the noon lock skips tomorrow for want of funds:
//     "No delivery tomorrow - your wallet was short at 12 noon. Recharge by
//     12 noon tomorrow and your milk resumes [DATE]", [DATE] the next morning
//     the plan delivers after the skipped one. Not for a day a free Welcome
//     Litre pack still arrives, not for a day that is not tomorrow.
//   - B-02 ("recharge by 12 noon tomorrow") is not sent to a member D-07
//     reached at the same noon (one clear message), and never before the
//     lock has decided tomorrow.
//   - Copy that said "tomorrow" but meant another day names the real day:
//     A-05 (worded when it fires) and FF-01.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run CRMNoonCopy -v

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// inboxBodies is every EN body the member's inbox holds for a trigger.
func inboxBodies(t *testing.T, w *chainWorld, cid primitive.ObjectID, trigger string) []string {
	t.Helper()
	cur, err := w.db.Collection(collConsumerInbox).Find(context.Background(),
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: trigger}})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	var rows []struct {
		EN string `bson:"body_en"`
		HI string `bson:"body_hi"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	var out []string
	for _, r := range rows {
		if tok := crmTokenRe.FindString(r.EN + r.HI); tok != "" {
			t.Fatalf("%s: unresolved token %s in %q / %q", trigger, tok, r.EN, r.HI)
		}
		out = append(out, r.EN)
	}
	return out
}

func TestCRMNoonCopyD07ReplacesB02ForASkippedDay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -2), 9, 0)
	plan := func(phone string, fund float64, freq string) (primitive.ObjectID, *subscription) {
		t.Helper()
		cid := w.customer(t, phone, fund)
		sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 2, Frequency: freq, StartDate: D1}, chainPlanMadeAt)
		if err != nil {
			t.Fatalf("createSubscription: %v", err)
		}
		chainBackdateSubscription(t, w, sub, long)
		return cid, sub
	}
	short, _ := plan("9000013101", 0, "daily")        // D+1 skipped, short for D+2 too
	shortAlt, _ := plan("9000013102", 0, "alternate") // D+1 skipped; the plan's next morning is D+3
	covered, _ := plan("9000013103", 100, "daily")    // D+1 locked (100 >= 58); D+2 short once D+1 is paid
	freePack, _ := plan("9000013104", 0, "daily")     // D+1 skipped, but a free Welcome Litre pack arrives D+1
	if err := w.svc.repo.insertOrder(ctx, &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: freePack.Hex(), Status: "placed",
		PaymentMethod: "wallet", Lane: "morning", DeliveryDate: D1, ScheduledFor: D1, OfferID: offerWelcomeLitre, OfferPack: 1,
		Items:     []orderItem{{ID: newItemID(), ProductID: "gold-500ml", Name: "Milk gold-500ml", Qty: 1, IsPromotional: true}},
		CreatedAt: istDayAt(D, 8, 0).UTC(), UpdatedAt: istDayAt(D, 8, 0).UTC(), PlacedAt: istDayAt(D, 8, 0).UTC(),
	}); err != nil {
		t.Fatalf("free pack: %v", err)
	}

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))
	// 12:01, before the lock tick: B-02 must wait for the lock to decide.
	w.svc.crmProcessSchedules(ctx, istDayAt(D, 12, 1))
	for _, cid := range []primitive.ObjectID{short, shortAlt, covered, freePack} {
		if n := inboxCount(t, w.db, cid, "B-02"); n != 0 {
			t.Fatalf("B-02 went out before the noon lock decided tomorrow: %d rows", n)
		}
	}
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5)) // the lock
	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 12, 6))
	w.svc.crmProcessSchedules(ctx, istDayAt(D, 12, 20))

	want := map[primitive.ObjectID]struct {
		d07 string
		b02 int
	}{
		short:    {"No delivery tomorrow - your wallet was short at 12 noon. Recharge by 12 noon tomorrow and your milk resumes 8 Oct.", 0},
		shortAlt: {"No delivery tomorrow - your wallet was short at 12 noon. Recharge by 12 noon tomorrow and your milk resumes 9 Oct.", 0},
		covered:  {"", 1},
		freePack: {"", 1},
	}
	for cid, exp := range want {
		bodies := inboxBodies(t, w, cid, "D-07")
		switch {
		case exp.d07 == "" && len(bodies) != 0:
			t.Errorf("%s: D-07 sent: %q", cid.Hex(), bodies)
		case exp.d07 != "" && (len(bodies) != 1 || bodies[0] != exp.d07):
			t.Errorf("%s: D-07 bodies %q, want [%q] (dispatch %v)", cid.Hex(), bodies, exp.d07, crmDispatchStatuses(t, w.db, cid, "D-07"))
		}
		if n := inboxCount(t, w.db, cid, "B-02"); n != exp.b02 {
			t.Errorf("%s: %d B-02 rows, want %d", cid.Hex(), n, exp.b02)
		}
	}
	// A replayed event or a second drain sends nothing more.
	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 12, 30))
	if n := inboxCount(t, w.db, short, "D-07"); n != 1 {
		t.Fatalf("D-07 repeated: %d", n)
	}
}

// A day skipped by a catch-up in the small hours of the day itself (the
// server was down over the noon before) is not "tomorrow": no D-07.
func TestCRMNoonCopyNoD07ForADayThatIsNotTomorrow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	cid := w.customer(t, "9000013201", 0)
	sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 2, Frequency: "daily", StartDate: D}, chainPlanMadeAt)
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -3), 9, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 3, 0)) // catch-up of D itself: skipped
	assertSkipped(t, w, sub, D)
	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 3, 1))
	if n := inboxCount(t, w.db, cid, "D-07"); n != 0 {
		t.Fatalf("D-07 said \"no delivery tomorrow\" for today: %d", n)
	}
}

// A-05 fires two hours after an unpaid plan is created; it names the morning
// the milk really starts as seen when it is sent.
func TestCRMNoonCopyA05NamesTheDayTheMilkStarts(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	mk := func(phone string, at time.Time) (primitive.ObjectID, *subscription) {
		t.Helper()
		cid := w.customer(t, phone, 0)
		sub, err := w.svc.createSubscriptionAt(ctx, cid, subscriptionInput{ProductID: "taaza-500ml", Qty: 2, Frequency: "daily", StartDate: D1}, at)
		if err != nil {
			t.Fatalf("createSubscriptionAt: %v", err)
		}
		w.svc.sweepOneSubscription(ctx, sub, at) // the create handler's kick: previews the first morning
		// The outbox stamps the wall clock; the plan was made at `at`, and the
		// delay counts from there.
		if _, err := w.db.Collection(collCRMEvents).UpdateMany(ctx,
			bson.D{{Key: "consumer_id", Value: cid}, {Key: "topic", Value: "subscription.created_unpaid"}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "created_at", Value: at.UTC()}}}}); err != nil {
			t.Fatalf("stamp event: %v", err)
		}
		return cid, sub
	}
	morning, _ := mk("9000013301", istDayAt(D, 9, 0))       // A-05 at 11:00: tomorrow is still open
	lateMorning, _ := mk("9000013302", istDayAt(D, 10, 30)) // A-05 at 12:30: tomorrow was skipped at noon
	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 10, 31))

	w.svc.crmFireDueSchedules(ctx, istDayAt(D, 11, 5))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5)) // both short: D+1 skipped
	w.svc.crmFireDueSchedules(ctx, istDayAt(D, 12, 35))

	for cid, want := range map[primitive.ObjectID]string{
		morning:     "One step left — recharge your Wallet and your morning milk starts tomorrow.",
		lateMorning: "One step left — recharge your Wallet and your morning milk starts 8 Oct.",
	} {
		if got := inboxBodies(t, w, cid, "A-05"); len(got) != 1 || got[0] != want {
			t.Errorf("%s: A-05 %q, want [%q] (dispatch %v)", cid.Hex(), got, want, crmDispatchStatuses(t, w.db, cid, "A-05"))
		}
	}
}

// FF-01 promises the first delivery on the first morning still open to
// orders when the farm unlocks: tomorrow before noon, the day after from noon.
func TestCRMNoonCopyFF01NamesTheFirstOpenMorning(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	for _, c := range []struct {
		phone, farm string
		at          time.Time
		want        string
	}{
		{"9000013401", "ff-noon-am", istDayAt(D, 10, 0), "First delivery tomorrow by 7 AM."},
		{"9000013402", "ff-noon-pm", istDayAt(D, 14, 0), "First delivery 8 Oct by 7 AM."},
	} {
		cid := w.customer(t, c.phone, 0)
		if _, err := w.svc.repo.foundingFarms().InsertOne(ctx, foundingFarm{
			ID: c.farm, Name: "Noon Farm", Farmer: "Ram Singh", UnlocksAt: 1, Claimed: 1, Status: farmFilling,
			CreatedAt: c.at.Add(-24 * time.Hour).UTC(), UpdatedAt: c.at.Add(-time.Hour).UTC(),
		}); err != nil {
			t.Fatalf("farm: %v", err)
		}
		if _, err := w.svc.repo.foundingMembers().InsertOne(ctx, foundingMember{
			ID: primitive.NewObjectID(), ConsumerID: cid, FarmID: c.farm, Status: memberWaiting, LineNumber: 1, JoinedAt: c.at.Add(-time.Hour),
		}); err != nil {
			t.Fatalf("member: %v", err)
		}
		w.svc.unlockFoundingFarm(ctx, &foundingFarm{ID: c.farm, Name: "Noon Farm", Farmer: "Ram Singh"}, c.at)
		w.svc.crmProcessEventsAt(ctx, c.at.Add(time.Minute))
		got := inboxBodies(t, w, cid, "FF-01")
		if len(got) != 1 || !strings.HasSuffix(got[0], c.want) {
			t.Errorf("unlocked at %s: FF-01 %q, want it to end %q", c.at.In(istZone).Format("15:04"), got, c.want)
		}
	}
}

// nr-2: D-07 says "No delivery tomorrow", so it is only for a member who
// gets no morning delivery tomorrow at all. A member whose older plan is
// locked, whose one-off morning order is reserved first, or whose other
// plan a catch-up locks after this one was skipped still gets milk
// tomorrow: no D-07, and B-02 is judged for them as for anyone else.
func TestCRMNoonCopyNoD07WhenAnotherDeliveryArrivesTomorrow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1 := addDaysIST(D, 1)
	long := istDayAt(addDaysIST(D, -3), 9, 0)

	// A: Rs 60 covers the older plan (2 x Rs 29), not the newer (Rs 35).
	twoPlans := w.customer(t, "9000013501", 60)
	aOld := walletLockPlan(t, w, twoPlans, "taaza-500ml", 2, D1, long)
	aNew := walletLockPlan(t, w, twoPlans, "gold-500ml", 1, D1, long.Add(time.Hour))
	// B: a one-off morning order for tomorrow (Rs 35 + Rs 15), then a plan
	// the rest (Rs 20) cannot pay.
	oneOff := w.customer(t, "9000013502", 70)
	bSub := walletLockPlan(t, w, oneOff, "taaza-500ml", 2, D1, long)
	if _, err := w.svc.createOrderAt(ctx, oneOff.Hex(), morningOrderFor(D1), istDayAt(D, 11, 0)); err != nil {
		t.Fatalf("one-off order: %v", err)
	}
	// C (control): one plan, an empty wallet: nothing comes tomorrow.
	nothing := w.customer(t, "9000013503", 0)
	cSub := walletLockPlan(t, w, nothing, "taaza-500ml", 2, D1, long)

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 9, 0))

	// D: the server was down all morning, so the 12:05 tick catches this
	// member's day up one plan at a time: the older plan (Rs 57) is skipped
	// on Rs 30, then the newer one (Rs 29) is locked.
	catchUp := w.customer(t, "9000013504", 30)
	dOld := walletLockPlan(t, w, catchUp, "taaza-1l", 1, D1, long)
	dNew := walletLockPlan(t, w, catchUp, "taaza-500ml", 1, D1, long.Add(time.Hour))

	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	assertLocked(t, w, aOld, D1)
	assertSkipped(t, w, aNew, D1)
	assertSkipped(t, w, bSub, D1)
	assertSkipped(t, w, cSub, D1)
	assertSkipped(t, w, dOld, D1)
	assertLocked(t, w, dNew, D1)

	w.svc.crmProcessEventsAt(ctx, istDayAt(D, 12, 6))
	w.svc.crmProcessSchedules(ctx, istDayAt(D, 12, 20))

	for _, c := range []struct {
		name string
		cid  primitive.ObjectID
		d07  string
		b02  int
	}{
		{"older plan locked", twoPlans, "", 1},
		{"one-off order reserved", oneOff, "", 1},
		{"nothing comes", nothing, "No delivery tomorrow - your wallet was short at 12 noon. Recharge by 12 noon tomorrow and your milk resumes 8 Oct.", 0},
		{"a catch-up locked the other plan", catchUp, "", 1},
	} {
		bodies := inboxBodies(t, w, c.cid, "D-07")
		switch {
		case c.d07 == "" && len(bodies) != 0:
			t.Errorf("%s: told %q although a delivery still arrives tomorrow", c.name, bodies)
		case c.d07 != "" && (len(bodies) != 1 || bodies[0] != c.d07):
			t.Errorf("%s: D-07 bodies %q, want [%q]", c.name, bodies, c.d07)
		}
		if n := inboxCount(t, w.db, c.cid, "B-02"); n != c.b02 {
			t.Errorf("%s: %d B-02 rows, want %d (dispatch %v)", c.name, n, c.b02, crmDispatchStatuses(t, w.db, c.cid, "B-02"))
		}
	}
	// The lock itself says so for the plain cases.
	for _, cid := range []primitive.ObjectID{twoPlans, oneOff} {
		for _, e := range daySkippedEvents(t, w, cid) {
			p, _ := e["payload"].(bson.M)
			if p["tomorrow_blocked"] != false {
				t.Errorf("%s: day_skipped tomorrow_blocked = %v, want false", cid.Hex(), p["tomorrow_blocked"])
			}
		}
	}
}

// nr-3: after a boot or wake past noon (a deploy, a restart after an outage,
// or the free plan's instance woken by the first request), the CRM minute
// tick can run before the subscription worker's boot tick has locked
// tomorrow. B-02 waits for positive evidence that this noon's lock ran, so a
// member the lock then skips gets D-07 alone, never B-02 as well.
func TestCRMNoonCopyB02WaitsForTheLockAfterABootPastNoon(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	plan := func(phone string, fund float64, D string) primitive.ObjectID {
		t.Helper()
		cid := w.customer(t, phone, fund)
		walletLockPlan(t, w, cid, "taaza-500ml", 2, addDaysIST(D, 1), istDayAt(addDaysIST(D, -3), 9, 0)) // Rs 58 a morning
		return cid
	}
	counts := func(cid primitive.ObjectID) (int, int) {
		return inboxCount(t, w.db, cid, "D-07"), inboxCount(t, w.db, cid, "B-02")
	}

	// (a) Previews made at 09:00; the server is down from 11:00 to 13:30.
	const Da = "2026-10-06"
	short := plan("9000013601", 0, Da)   // D+1 skipped at the lock: D-07 only
	enough := plan("9000013602", 80, Da) // D+1 locked, Rs 22 left for D+2: B-02
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(Da, 9, 0))
	boot := istDayAt(Da, 13, 30)
	w.svc.crmProcessSchedules(ctx, boot) // the CRM tick gets there first
	for _, cid := range []primitive.ObjectID{short, enough} {
		if _, b02 := counts(cid); b02 != 0 {
			t.Fatalf("13:30 boot: B-02 went out before the noon lock ran (%s)", cid.Hex())
		}
	}
	w.svc.sweepSubscriptionOrders(ctx, boot) // the worker's boot tick: the lock
	w.svc.crmProcessEventsAt(ctx, boot.Add(time.Minute))
	w.svc.crmProcessSchedules(ctx, boot.Add(time.Minute))
	if d07, b02 := counts(short); d07 != 1 || b02 != 0 {
		t.Errorf("13:30 boot, skipped member: D-07 %d, B-02 %d, want 1 and 0", d07, b02)
	}
	if d07, b02 := counts(enough); d07 != 0 || b02 != 1 {
		t.Errorf("13:30 boot, funded member short for D+2: D-07 %d, B-02 %d, want 0 and 1", d07, b02)
	}

	// (b) Asleep since the day before (no preview at all); woken at 12:30.
	const Db = "2026-10-13"
	asleep := plan("9000013603", 0, Db)
	wake := istDayAt(Db, 12, 30)
	w.svc.crmProcessSchedules(ctx, wake)
	if _, b02 := counts(asleep); b02 != 0 {
		t.Fatalf("12:30 wake: B-02 went out before the noon lock ran")
	}
	w.svc.sweepSubscriptionOrders(ctx, wake) // catch-up: tomorrow skipped
	w.svc.crmProcessEventsAt(ctx, wake.Add(time.Minute))
	w.svc.crmProcessSchedules(ctx, wake.Add(time.Minute))
	if d07, b02 := counts(asleep); d07 != 1 || b02 != 0 {
		t.Errorf("12:30 wake: D-07 %d, B-02 %d, want 1 and 0", d07, b02)
	}
}

// noonLockRanAt records that the subscription worker ran the noon lock at
// at, the evidence B-02 waits for (nr-3), for a test that judges B-02 on
// its own without driving the lock.
func noonLockRanAt(t *testing.T, svc *service, at time.Time) {
	t.Helper()
	svc.markNoonLockRan(context.Background(), lockedThroughDay(at), at)
	if !svc.noonLockRan(context.Background(), lockedThroughDay(at)) {
		t.Fatalf("the noon lock for %s was not recorded", lockedThroughDay(at))
	}
}

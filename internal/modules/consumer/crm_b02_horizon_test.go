package consumer

// B-02 under the noon lock. The copy says "recharge by 12 noon tomorrow to
// receive your delivery"; the delivery whose order locks at 12:00 tomorrow is
// the day AFTER tomorrow (tomorrow's locked at 12:00 today). So the 12:00
// sweep on day D must judge D+2, with the wallet as the D+2 lock will see it:
// after D+1's locked delivery has been paid for.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMB02 -v

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMB02JudgesTheDayItsCopyNames(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)

	plan := func(phone string, fund float64, freq, start string) primitive.ObjectID {
		t.Helper()
		cid := w.customer(t, phone, fund)
		sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{
			ProductID: "gold-500ml", Variant: "500ml", Qty: 2, Frequency: freq, StartDate: start,
		})
		if err != nil {
			t.Fatalf("createSubscription %s: %v", phone, err)
		}
		chainBackdateSubscription(t, w, sub, istDayAt(addDaysIST(D, -2), 9, 0))
		return cid
	}
	// Rs 70 a morning (2 x Rs 35, no fee on a plan).
	shortAfterTomorrow := plan("9000011301", 100, "daily", D1) // D+1 locks (100 >= 70); after it, 30 < 70 for D+2
	covered := plan("9000011302", 150, "daily", D1)            // after D+1, 80 >= 70
	dueOnlyD2 := plan("9000011303", 0, "weekly", D2)           // nothing tomorrow, an empty wallet for D+2
	dueOnlyD1 := plan("9000011304", 0, "weekly", D1)           // D+1 was the 12:00 D-1 sweep's business; nothing on D+2

	// The day before the route: the 11:00 tick previews D+1, the 12:05 tick
	// locks it (funded ones only) and previews D+2.
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 11, 0))
	w.svc.sweepSubscriptionOrders(ctx, istDayAt(D, 12, 5))
	if o := liveSubOrderFor(t, w, shortAfterTomorrow, D1); o == nil || o.SubLockedAt == "" {
		t.Fatalf("setup: the short member's D+1 must be locked: %+v", o)
	}

	w.svc.crmProcessSchedules(ctx, istDayAt(D, 12, 20))

	for _, c := range []struct {
		name string
		cid  primitive.ObjectID
		want int
	}{
		{"D+1 locked, the wallet left after it cannot pay D+2", shortAfterTomorrow, 1},
		{"D+1 locked, the rest still pays D+2", covered, 0},
		{"a plan due on D+2 only, with an empty wallet", dueOnlyD2, 1},
		{"a plan due on D+1 only (its cut-off is today's noon)", dueOnlyD1, 0},
	} {
		if n := inboxCount(t, w.db, c.cid, "B-02"); n != c.want {
			t.Errorf("%s: %d B-02 rows, want %d (dispatch %v)", c.name, n, c.want, crmDispatchStatuses(t, w.db, c.cid, "B-02"))
		}
	}
}

// liveSubOrderFor finds a member's live subscription order for a day.
func liveSubOrderFor(t *testing.T, w *chainWorld, cid primitive.ObjectID, day string) *order {
	t.Helper()
	subs, err := w.svc.repo.listSubscriptions(context.Background(), cid)
	if err != nil || len(subs) == 0 {
		t.Fatalf("subscriptions of %s: %v", cid.Hex(), err)
	}
	return liveSubOrder(t, w, subs[0].SubscriptionID, day)
}

package consumer

// G9 (owner, 24 Sep): the Welcome Litre's free pack 1 follows the noon rule.
// It is minted for the first morning still open to orders - tomorrow before
// 12:00 IST, the day after tomorrow from noon - never for a tomorrow whose
// route the store has already locked. The campaign plan starts that same
// morning, so pack 1 and the plan's first morning coincide.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run WelcomeLitrePack1 -v

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestWelcomeLitrePack1FollowsTheNoonRule(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	const D = "2026-10-06"
	D1, D2 := addDaysIST(D, 1), addDaysIST(D, 2)
	const M = "2026-10-31"
	for i, c := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"09:00", istDayAt(D, 9, 0), D1},
		{"11:59", istDayAt(D, 11, 59), D1},
		{"12:00:01", istDayAt(D, 12, 0).Add(time.Second), D2},
		{"12:14:59", istDayAt(D, 12, 14).Add(59 * time.Second), D2},
		{"23:59", istDayAt(D, 23, 59), D2},
		{"month end 14:00", istDayAt(M, 14, 0), "2026-11-02"},
	} {
		res, err := w.svc.crmEnrolAt(ctx, "g9-operator", crmEnrolInput{
			Phone: "90000150" + strconv.Itoa(10+i), Name: "G9 Household", Line1: "Flat " + strconv.Itoa(i+1) + ", Noon Tower",
			Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
		}, c.at)
		if err != nil {
			t.Fatalf("%s: crmEnrolAt: %v", c.name, err)
		}
		if res.Pack1For != c.want {
			t.Fatalf("%s: pack1_scheduled_for %s, want %s", c.name, res.Pack1For, c.want)
		}
		pack := w.orderByID(t, res.Pack1OrderID)
		task, _ := w.svc.repo.findDeliveryByOrder(ctx, res.Pack1OrderID)
		if pack.DeliveryDate != c.want || task == nil || task.DeliveryDate != c.want {
			t.Fatalf("%s: pack order %s task %+v, want both on %s", c.name, pack.DeliveryDate, task, c.want)
		}
		cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
		if locked := lockedThroughDay(c.at); c.want <= locked {
			t.Fatalf("%s: pack 1 on %s, a morning already locked (through %s)", c.name, c.want, locked)
		}
		if n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
			{Key: "user_id", Value: cid.Hex()}, {Key: "offer_pack", Value: 1},
		}); n != 1 {
			t.Fatalf("%s: %d pack-1 orders, want 1", c.name, n)
		}
		sub, _ := w.svc.repo.findSubscriptionByID(ctx, res.SubscriptionID)
		if sub == nil || sub.StartDate != c.want {
			t.Fatalf("%s: the campaign plan starts %+v, want %s (pack 1's morning)", c.name, sub, c.want)
		}
		if nd := w.svc.nextDeliveryFor(ctx, sub, c.at); nd != c.want {
			t.Fatalf("%s: the plan's next_delivery_date %s, want %s", c.name, nd, c.want)
		}
	}
}

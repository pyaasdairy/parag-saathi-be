package consumer

// The subscription sweep must order each due day EXACTLY ONCE however many
// ticks run. Before the per-day claim (ordered_days), scheduling tomorrow
// overwrote today's claim, so the next tick re-claimed today and the one after
// re-claimed tomorrow — a duplicate order, delivery task and debit every other
// tick, for every daily subscriber, from 1 PM IST onwards.
//
// Run:
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run NeverDuplicates -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// The sweep must order each subscription day ONCE however many ticks run —
// before the per-day claim, scheduling tomorrow let the next tick re-claim today
// (and the one after re-claim tomorrow), minting a duplicate order every tick.
func TestSimpleDeliverySweepNeverDuplicatesADay(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000002004", 5000)
	sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{
		ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: istToday(time.Now()),
	})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	ist := time.Now().In(istZone)
	base := time.Date(ist.Year(), ist.Month(), ist.Day(), 14, 30, 0, 0, istZone)
	w.svc.sweepOneSubscription(ctx, sub, base)
	for i := 0; i < 8; i++ {
		w.svc.sweepSubscriptionOrders(ctx, base.Add(time.Duration(i)*15*time.Minute))
	}
	for _, day := range []string{base.Format("2006-01-02"), base.AddDate(0, 0, 1).Format("2006-01-02")} {
		n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
			{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: day},
		})
		if n != 1 {
			t.Fatalf("day %s has %d orders for the subscription, want exactly 1", day, n)
		}
	}
	// Pause before midnight cancels tomorrow's preview and releases the day;
	// resuming schedules it again — once.
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	w.svc.sweepSubscriptionOrders(ctx, base.Add(3*time.Hour))
	if _, err := w.svc.setSubscriptionStatus(ctx, cid, sub.SubscriptionID, "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	w.svc.sweepSubscriptionOrders(ctx, base.Add(3*time.Hour+15*time.Minute))
	w.svc.sweepSubscriptionOrders(ctx, base.Add(3*time.Hour+30*time.Minute))
	tomorrow := base.AddDate(0, 0, 1).Format("2006-01-02")
	live, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
		{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: tomorrow},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
	})
	if live != 1 {
		t.Fatalf("after pause+resume tomorrow should have exactly 1 live order, got %d", live)
	}
}

package consumer

// The (subscription_id, scheduled_for) unique index is the DB backstop under
// claimSubscriptionDay: two workers that both passed the claim still yield one
// order, a boot over legacy duplicates reports them instead of dying, and the
// partial filter leaves a cancelled preview out so pause-then-resume can
// schedule the day again.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run SubscriptionDay -v

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Workers that all believed they won the day (the claim bypassed, as a
// pre-check read racing another's insert would) place ONE order and ONE
// delivery task; every loser gets the winner's order back with no error.
func TestSubscriptionDayRacingWorkersYieldOneOrder(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000008201", 5000)
	sub, err := w.svc.createSubscription(ctx, cid, subscriptionInput{
		ProductID: "taaza-500ml", Qty: 1, Frequency: "daily", StartDate: istToday(time.Now()),
	})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	addr, err := w.svc.subscriptionAddress(ctx, sub.ConsumerID)
	if err != nil {
		t.Fatalf("subscriptionAddress: %v", err)
	}
	now := time.Now()
	day := istToday(now)

	const workers = 4
	results := make([]*order, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = w.svc.insertSubscriptionOrder(ctx, sub, addr, day, true, now)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: a duplicate day must read as already placed, not fail: %v", i, errs[i])
		}
		if results[i] == nil || results[i].OrderID != results[0].OrderID {
			t.Fatalf("worker %d returned %+v, want the day's single order %s", i, results[i], results[0].OrderID)
		}
	}
	n, _ := w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
		{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: day},
	})
	if n != 1 {
		t.Fatalf("orders for the day after %d racing inserts: %d want 1", workers, n)
	}
	tasks, _ := w.db.Collection(collDeliveries).CountDocuments(ctx, bson.D{{Key: "order_id", Value: results[0].OrderID}})
	if tasks != 1 {
		t.Fatalf("delivery tasks for the day's order: %d want 1", tasks)
	}

	// The whole sweep racing itself for tomorrow (claim + index): one order.
	// Swept after noon, a plan from before the cut-off is caught up for
	// tomorrow (locked) - the day the racing sweeps all reach for.
	ist := now.In(istZone)
	base := time.Date(ist.Year(), ist.Month(), ist.Day(), 14, 30, 0, 0, istZone)
	chainBackdateSubscription(t, w, sub, base.Add(-6*time.Hour))
	tomorrow := addDaysIST(istToday(base), 1)
	var sweeps sync.WaitGroup
	for i := 0; i < workers; i++ {
		sweeps.Add(1)
		go func() {
			defer sweeps.Done()
			w.svc.sweepOneSubscription(ctx, sub, base)
		}()
	}
	sweeps.Wait()
	n, _ = w.db.Collection(collOrders).CountDocuments(ctx, bson.D{
		{Key: "subscription_id", Value: sub.SubscriptionID}, {Key: "scheduled_for", Value: tomorrow},
	})
	if n != 1 {
		t.Fatalf("orders for tomorrow after %d racing sweeps: %d want 1", workers, n)
	}
}

// A boot over rows from before the index: the build is refused, the number of
// colliding (subscription, day) pairs is reported, and once they are cleaned
// up the index exists with its partial filter - a live second row is dropped
// as already placed, a cancelled sibling and one-off orders never collide.
func TestSubscriptionDayIndexReportsLegacyDuplicates(t *testing.T) {
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the subscription day index test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	db := client.Database("consumer_subday_index_test")
	_ = db.Drop(ctx)
	defer func() { _ = db.Drop(ctx); _ = client.Disconnect(ctx) }()
	repo := newRepository(db)

	row := func(sub, day, status string) *order {
		return &order{
			MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: "u1", Status: status,
			SubscriptionID: sub, ScheduledFor: day, PlacedAt: time.Now().UTC(),
		}
	}
	for _, o := range []*order{
		row("sub_a", "2026-09-20", "placed"), row("sub_a", "2026-09-20", "placed"), // the legacy pair
		row("sub_a", "2026-09-21", "delivered"), row("sub_a", "2026-09-21", "cancelled"), // not a collision
		row("sub_b", "2026-09-20", "placed"),
	} {
		if _, err := db.Collection(collOrders).InsertOne(ctx, o); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	dups, err := repo.ensureSubscriptionDayIndex(ctx)
	if err == nil || !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("build over duplicates: err=%v, want a duplicate-key refusal", err)
	}
	if dups != 1 {
		t.Fatalf("duplicate pairs reported: %d want 1", dups)
	}

	// Cleaned up, the build succeeds and is idempotent.
	if _, err := db.Collection(collOrders).DeleteOne(ctx, bson.D{
		{Key: "subscription_id", Value: "sub_a"}, {Key: "scheduled_for", Value: "2026-09-20"},
	}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for i := 0; i < 2; i++ {
		if dups, err := repo.ensureSubscriptionDayIndex(ctx); err != nil || dups != 0 {
			t.Fatalf("build %d after cleanup: dups=%d err=%v", i, dups, err)
		}
	}

	if placed, err := repo.insertSubscriptionOrderDoc(ctx, row("sub_a", "2026-09-20", "placed")); err != nil || placed {
		t.Fatalf("second live row for a day: placed=%v err=%v, want dropped as already placed", placed, err)
	}
	if placed, err := repo.insertSubscriptionOrderDoc(ctx, row("sub_a", "2026-09-20", "cancelled")); err != nil || !placed {
		t.Fatalf("cancelled sibling: placed=%v err=%v, want inserted", placed, err)
	}
	for i := 0; i < 2; i++ {
		if err := repo.insertOrder(ctx, row("", "", "placed")); err != nil {
			t.Fatalf("one-off order %d must never collide: %v", i, err)
		}
	}

	// Pause then resume: the preview is cancelled, the day is placed again - once.
	if _, err := db.Collection(collOrders).UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: "sub_b"}, {Key: "scheduled_for", Value: "2026-09-20"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}}}}); err != nil {
		t.Fatalf("cancel preview: %v", err)
	}
	if placed, err := repo.insertSubscriptionOrderDoc(ctx, row("sub_b", "2026-09-20", "placed")); err != nil || !placed {
		t.Fatalf("re-schedule after the cancel: placed=%v err=%v, want inserted", placed, err)
	}
	if placed, err := repo.insertSubscriptionOrderDoc(ctx, row("sub_b", "2026-09-20", "placed")); err != nil || placed {
		t.Fatalf("a third row for the re-scheduled day: placed=%v err=%v, want dropped", placed, err)
	}
	if live, _ := repo.findLiveSubscriptionOrder(ctx, "sub_b", "2026-09-20"); live == nil || live.Status != "placed" {
		t.Fatalf("live order after the re-schedule: %+v", live)
	}
}

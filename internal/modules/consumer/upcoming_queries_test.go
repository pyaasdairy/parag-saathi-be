package consumer

// R4-12: the store console calls /upcoming on every 12-second poll, and
// storeUpcoming ran, for EACH platform-wide preview, one task lookup and one
// nearestStore (which re-reads every active store): about two queries per
// plan per poll per open console. It now reads the stores once and the tasks
// with one $in query per call.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run UpcomingQueries -v

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/deps"
)

func TestUpcomingQueriesDoNotGrowWithThePreviews(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	tomorrow := addDaysIST(istToday(time.Now()), 1)

	// 30 previews routed to this store, one already minted as a task, and one
	// routed to a second, farther store.
	far := primitive.NewObjectID()
	if _, err := w.db.Collection("org_units").InsertOne(ctx, map[string]any{
		"_id": far, "type": "STORE", "active": true, "name": "Far Store", "geo_lat": 27.5, "geo_lng": 81.5,
	}); err != nil {
		t.Fatalf("far store: %v", err)
	}
	cid := w.customer(t, "9000005601", 0)
	var tasked string
	for i := 0; i < 31; i++ {
		geo := &geoPoint{Lat: 26.7712, Lng: 81.0123}
		if i == 30 {
			geo = &geoPoint{Lat: 27.5001, Lng: 81.5001}
		}
		o := &order{
			MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: cid.Hex(), Status: "placed", Lane: "morning",
			SubscriptionID: fmt.Sprintf("sub_up_%d", i), ScheduledFor: tomorrow, Geo: geo,
			Items:     []orderItem{{ID: newItemID(), ProductID: "gold-500ml", Name: "Milk gold-500ml", Variant: "500ml", Qty: 1, Price: 35}},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PlacedAt: time.Now().UTC(),
		}
		if err := w.svc.repo.insertOrder(ctx, o); err != nil {
			t.Fatalf("stage preview: %v", err)
		}
		if i == 0 {
			tasked = o.OrderID
			w.svc.createDeliveryForOrder(ctx, o)
		}
	}

	// The same database through a client that counts what it is asked.
	var mu sync.Mutex
	finds := map[string]int{}
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" || e.CommandName == "count" || e.CommandName == "aggregate" {
			mu.Lock()
			finds[e.Command.Lookup(e.CommandName).StringValue()]++
			mu.Unlock()
		}
	}}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(os.Getenv("CONSUMER_MONGO_TEST_URI")).SetMonitor(monitor))
	if err != nil {
		t.Fatalf("monitored client: %v", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()
	db := client.Database(w.db.Name())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := newService(&deps.Deps{Cfg: w.svc.deps.Cfg, Log: log, DB: db, Flags: w.svc.deps.Flags, Bus: w.svc.deps.Bus}, newRepository(db), log)

	rows, err := svc.storeUpcoming(ctx, w.mgr, w.storeID.Hex())
	if err != nil {
		t.Fatalf("storeUpcoming: %v", err)
	}
	if len(rows) != 29 {
		t.Fatalf("upcoming rows = %d, want 29 (30 routed here, one already a task)", len(rows))
	}
	for _, r := range rows {
		if r.OrderID == tasked {
			t.Fatal("a preview that already has a task was listed")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if finds["org_units"] > 2 || finds[collDeliveries] > 2 {
		t.Fatalf("queries for one /upcoming call: org_units=%d deliveries=%d (all: %v), want at most 2 each", finds["org_units"], finds[collDeliveries], finds)
	}
}

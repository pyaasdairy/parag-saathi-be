package consumer

// STORE_LOW_STOCK re-arms on purpose when the store's set of low SKUs (or
// their counts) changes: that raise after the admin read the alert is news,
// and the alert must read unread again with the new summary. The read itself
// (POST /notifications/{id}/read, platformops) treats this explicit
// read_at: null as unread. A repeat of the SAME set (Saathi posts it again on
// every launch) leaves the read alone. A recovered store clears it.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run LowStock -v

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestLowStockRaiseRearmsAReadAlert(t *testing.T) {
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the low-stock integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(ctx)
	db := client.Database("consumer_lowstock_rearm_test")
	_ = db.Drop(ctx)
	defer db.Drop(ctx)
	repo := newRepository(db)

	admin := primitive.NewObjectID()
	if _, err := db.Collection("parties").InsertOne(ctx, bson.D{{Key: "_id", Value: admin}, {Key: "phone", Value: "+919900000001"}}); err != nil {
		t.Fatalf("admin party: %v", err)
	}
	if _, err := db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: admin}, {Key: "role_code", Value: "SUPER_ADMIN"}, {Key: "status", Value: "ACTIVE"},
	}); err != nil {
		t.Fatalf("admin role: %v", err)
	}
	type alert struct {
		ID     primitive.ObjectID `bson:"_id"`
		Params map[string]string  `bson:"params"`
		ReadAt *time.Time         `bson:"read_at"`
	}
	alerts := func() []alert {
		t.Helper()
		cur, err := db.Collection("notifications").Find(ctx, bson.D{
			{Key: "party_id", Value: admin}, {Key: "template_key", Value: templateStoreLowStock},
		})
		if err != nil {
			t.Fatalf("alerts: %v", err)
		}
		var out []alert
		if err := cur.All(ctx, &out); err != nil {
			t.Fatalf("alerts decode: %v", err)
		}
		return out
	}

	if err := repo.raiseLowStock(ctx, "store-1", lowStockRequest{StoreName: "PYAAS Store", Summary: "Toned Milk 500ml (4)", ItemCount: 1}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	got := alerts()
	if len(got) != 1 || got[0].ReadAt != nil {
		t.Fatalf("first raise: %+v", got)
	}
	// The admin reads it (what POST /notifications/{id}/read stores).
	readAt := time.Date(2026, 9, 24, 5, 0, 0, 0, time.UTC)
	if _, err := db.Collection("notifications").UpdateOne(ctx, bson.D{{Key: "_id", Value: got[0].ID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "read_at", Value: readAt}}}}); err != nil {
		t.Fatalf("read: %v", err)
	}
	// A new low set: the same row, the new summary, unread again.
	if err := repo.raiseLowStock(ctx, "store-1", lowStockRequest{StoreName: "PYAAS Store", Summary: "Toned Milk 500ml (2), Chai Special (1)", ItemCount: 2}); err != nil {
		t.Fatalf("re-raise: %v", err)
	}
	got = alerts()
	if len(got) != 1 || got[0].ReadAt != nil || got[0].Params["summary"] != "Toned Milk 500ml (2), Chai Special (1)" {
		t.Fatalf("a new low set must re-arm the one alert: %+v", got)
	}
	// Stock recovered: the alert is cleared.
	if err := repo.raiseLowStock(ctx, "store-1", lowStockRequest{StoreName: "PYAAS Store"}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got = alerts(); len(got) != 0 {
		t.Fatalf("recovered stock must clear the alert: %+v", got)
	}
}

// Saathi's store home starts its last-posted key empty, so every app launch
// or re-login posts the SAME low set again. That repeat is not news: an alert
// the admin already read stays read, and keeps its place in the inbox (its
// queued_at). Only a set that really changed turns it unread again.
func TestLowStockIdenticalRaiseKeepsTheAdminsRead(t *testing.T) {
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the low-stock integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(ctx)
	db := client.Database("consumer_lowstock_repeat_test")
	_ = db.Drop(ctx)
	defer db.Drop(ctx)
	repo := newRepository(db)

	admin := primitive.NewObjectID()
	if _, err := db.Collection("parties").InsertOne(ctx, bson.D{{Key: "_id", Value: admin}, {Key: "phone", Value: "+919900000003"}}); err != nil {
		t.Fatalf("admin party: %v", err)
	}
	if _, err := db.Collection("role_assignments").InsertOne(ctx, bson.D{
		{Key: "party_id", Value: admin}, {Key: "role_code", Value: "PCDF_ADMIN"}, {Key: "status", Value: "ACTIVE"},
	}); err != nil {
		t.Fatalf("admin role: %v", err)
	}
	type alert struct {
		ID       primitive.ObjectID `bson:"_id"`
		Params   map[string]string  `bson:"params"`
		Status   string             `bson:"status"`
		QueuedAt time.Time          `bson:"queued_at"`
		ReadAt   *time.Time         `bson:"read_at"`
	}
	only := func(label string) alert {
		t.Helper()
		cur, err := db.Collection("notifications").Find(ctx, bson.D{
			{Key: "party_id", Value: admin}, {Key: "template_key", Value: templateStoreLowStock},
		})
		if err != nil {
			t.Fatalf("%s: alerts: %v", label, err)
		}
		var out []alert
		if err := cur.All(ctx, &out); err != nil {
			t.Fatalf("%s: alerts decode: %v", label, err)
		}
		if len(out) != 1 {
			t.Fatalf("%s: %d alerts, want the one row: %+v", label, len(out), out)
		}
		return out[0]
	}

	// A summary that starts with "$" must be stored as text, never read as a
	// field path by the update.
	set := lowStockRequest{StoreName: "PYAAS Store", Summary: "$Toned Milk 500ml (4)", ItemCount: 1}
	if err := repo.raiseLowStock(ctx, "store-7", set); err != nil {
		t.Fatalf("first raise: %v", err)
	}
	first := only("first raise")
	if first.ReadAt != nil || first.Status != "QUEUED" || first.QueuedAt.IsZero() ||
		first.Params["summary"] != set.Summary || first.Params["item_count"] != "1" ||
		first.Params["store"] != "PYAAS Store" || first.Params["store_id"] != "store-7" {
		t.Fatalf("a first raise is a new unread alert: %+v", first)
	}

	// The admin reads it (what POST /notifications/{id}/read stores).
	readAt := time.Date(2026, 9, 24, 5, 0, 0, 0, time.UTC)
	if _, err := db.Collection("notifications").UpdateOne(ctx, bson.D{{Key: "_id", Value: first.ID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "read_at", Value: readAt}}}}); err != nil {
		t.Fatalf("read: %v", err)
	}

	// The store manager reopens Saathi: the same set is posted again, twice.
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if err := repo.raiseLowStock(ctx, "store-7", set); err != nil {
			t.Fatalf("repeat raise %d: %v", i, err)
		}
	}
	again := only("repeat raise")
	if again.ID != first.ID || again.ReadAt == nil || !again.ReadAt.Equal(readAt) {
		t.Fatalf("the same low set un-read the admin's alert: %+v", again)
	}
	if !again.QueuedAt.Equal(first.QueuedAt) || again.Status != first.Status {
		t.Fatalf("the same low set moved the alert: queued_at %v -> %v, status %q -> %q",
			first.QueuedAt, again.QueuedAt, first.Status, again.Status)
	}

	// The count alone changes: that is a new set, the alert re-arms.
	changed := lowStockRequest{StoreName: "PYAAS Store", Summary: set.Summary, ItemCount: 2}
	if err := repo.raiseLowStock(ctx, "store-7", changed); err != nil {
		t.Fatalf("changed raise: %v", err)
	}
	rearmed := only("changed count")
	if rearmed.ReadAt != nil || !rearmed.QueuedAt.After(first.QueuedAt) || rearmed.Params["item_count"] != "2" {
		t.Fatalf("a changed low set must re-arm the alert: %+v", rearmed)
	}
}

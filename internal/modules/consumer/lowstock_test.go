package consumer

// STORE_LOW_STOCK re-arms on purpose: the store posts a raise only when its
// set of low SKUs (or their counts) changes, so a raise after the admin read
// the alert is news, and the alert must read unread again with the new
// summary. The read itself (POST /notifications/{id}/read, platformops)
// treats this explicit read_at: null as unread. A recovered store clears it.
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

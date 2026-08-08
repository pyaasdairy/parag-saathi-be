package consumer

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestSeedConsumerProductsIntegration exercises the whole phase-2 catalog path
// against a REAL Mongo: seed → serve (flag on/off) → store stock. It is gated on
// CONSUMER_MONGO_TEST_URI so a normal `go test` (and CI without Mongo) skips it.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 go test ./internal/modules/consumer/ -run Integration -v
func TestSeedConsumerProductsIntegration(t *testing.T) {
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the Mongo integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(ctx)
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("mongo ping: %v", err)
	}

	dbName := "consumer_phase2_it_test"
	db := client.Database(dbName)
	_ = db.Drop(ctx) // start clean
	defer db.Drop(ctx)

	repo := newRepository(db)

	// ── Seed (idempotent) ──
	if err := repo.seedConsumerProducts(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := repo.seedConsumerProducts(ctx); err != nil {
		t.Fatalf("re-seed (idempotency): %v", err)
	}

	// ── Serve WITH the flag on: seeded products surface as additions ──
	on, err := repo.catalogView(ctx, true)
	if err != nil {
		t.Fatalf("catalogView(true): %v", err)
	}
	if len(on.Additions) != 48 {
		t.Fatalf("served additions = %d, want 48 (re-seed must not duplicate)", len(on.Additions))
	}

	byID := map[string]additionView{}
	for _, a := range on.Additions {
		byID[a.ID] = a
	}

	// gold-500ml: image path + milk GST-exempt + FSSAI present.
	gold, ok := byID["gold-500ml"]
	if !ok {
		t.Fatal("gold-500ml not served")
	}
	if gold.PhotoURL != "catalog/img/gold.png" {
		t.Errorf("gold photo_url = %q, want catalog/img/gold.png", gold.PhotoURL)
	}
	if gold.Price <= 0 {
		t.Errorf("gold price = %v, want > 0", gold.Price)
	}
	if gold.Compliance == nil || gold.Compliance.GSTRate == nil || *gold.Compliance.GSTRate != 0 {
		t.Errorf("gold GST = %v, want 0 (fresh milk exempt)", gold.Compliance)
	}
	if gold.Compliance != nil && gold.Compliance.FSSAILicense == "" {
		t.Error("gold FSSAI license missing")
	}

	// taaza-500ml carries a back-of-pack image path.
	if taaza, ok := byID["taaza-500ml"]; !ok {
		t.Error("taaza-500ml not served")
	} else if taaza.BackPhotoURL != "catalog/img/taaza-back.png" {
		t.Errorf("taaza back_photo_url = %q, want catalog/img/taaza-back.png", taaza.BackPhotoURL)
	}

	// CONSUMER-HIDDEN STOCK: the served JSON must not leak stock_count anywhere.
	raw, _ := json.Marshal(on)
	if strings.Contains(string(raw), "stock_count") || strings.Contains(string(raw), "stockCount") {
		t.Error("consumer catalog JSON leaked stock_count — must be store-console-only")
	}

	// ── Serve WITH the flag off: response is the overlay-only baseline (no seeded
	//    products), so on a fresh DB the additions are empty and version is 0. ──
	off, err := repo.catalogView(ctx, false)
	if err != nil {
		t.Fatalf("catalogView(false): %v", err)
	}
	if len(off.Additions) != 0 {
		t.Errorf("flag-off additions = %d, want 0 (seeded products must be inert)", len(off.Additions))
	}
	if off.Version != 0 {
		t.Errorf("flag-off version = %d, want 0 (seeded stamp must not bump the cache version)", off.Version)
	}

	// ── Store-console stock: all 48 present with the default on-hand count ──
	stock, err := repo.seededStock(ctx)
	if err != nil {
		t.Fatalf("seededStock: %v", err)
	}
	if len(stock) != 48 {
		t.Fatalf("seeded stock rows = %d, want 48", len(stock))
	}
	for _, d := range stock {
		if d.StockCount == nil || *d.StockCount != defaultSeedStock {
			t.Errorf("%s stock_count = %v, want %d", d.SkuID, d.StockCount, defaultSeedStock)
			break
		}
	}
}

package consumer

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// products_seed.json is generated from the consumer app's constants/products.ts
// (via the TS compiler, so there is ONE source of truth and no hand-transcription
// drift). It carries every SKU's identity, price, compliance block and image
// asset names. Regenerate + bump seedProductVersion when the app catalog changes.
//
//go:embed products_seed.json
var embeddedProductsSeed []byte

// globalCatalogStore is the sentinel store the seeded baseline products live
// under. catalogView reads ACROSS all stores, so these surface to the consumer;
// the per-store console read filters by a real store id, so they never disturb it.
const globalCatalogStore = "_global"

// seedProductVersion bumps whenever products_seed.json changes. updated_at is
// stamped from seedProductStamp (a fixed time), not time.Now(), so re-seeding the
// SAME data never churns the catalog cache version.
const seedProductVersion = 1

var seedProductStamp = time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)

// defaultSeedStock is the placeholder on-hand count stamped on each seeded product
// FOR THE STORE CONSOLE ONLY (never projected to the consumer). The store manager
// edits the real number; a re-seed never overwrites it ($setOnInsert).
const defaultSeedStock = 50

// seedProduct mirrors one row of products_seed.json.
type seedProduct struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Variant        string        `json:"variant"`
	BaseID         string        `json:"base_id"`
	Category       string        `json:"category"`
	Price          float64       `json:"price"`
	MRP            *float64      `json:"mrp"`
	Unit           string        `json:"unit"`
	Tag            string        `json:"tag"`
	Description    string        `json:"description"`
	Subscribable   bool          `json:"subscribable"`
	Rating         *float64      `json:"rating"`
	RatingCount    *int          `json:"rating_count"`
	MostOrdered    bool          `json:"most_ordered"`
	PackCount      *int          `json:"pack_count"`
	OutOfStock     bool          `json:"out_of_stock"`
	ImageAsset     string        `json:"image_asset"`
	BackImageAsset string        `json:"back_image_asset"`
	Compliance     complianceDoc `json:"compliance"`
}

// complianceDoc is the per-SKU FSSAI / Legal Metrology / GST block, resolved from
// the app's category helpers + per-SKU overrides at extract time. It travels with
// the product and is surfaced on the product page + invoice.
type complianceDoc struct {
	HSN                 string   `bson:"hsn,omitempty"                  json:"hsn,omitempty"`
	GSTRate             *float64 `bson:"gst_rate,omitempty"             json:"gstRate,omitempty"`
	NetQuantity         string   `bson:"net_quantity,omitempty"         json:"netQuantity,omitempty"`
	Veg                 *bool    `bson:"veg,omitempty"                  json:"veg,omitempty"`
	Ingredients         string   `bson:"ingredients,omitempty"          json:"ingredients,omitempty"`
	Nutrition           string   `bson:"nutrition,omitempty"            json:"nutrition,omitempty"`
	Allergens           string   `bson:"allergens,omitempty"            json:"allergens,omitempty"`
	ShelfLife           string   `bson:"shelf_life,omitempty"           json:"shelfLife,omitempty"`
	Storage             string   `bson:"storage,omitempty"              json:"storage,omitempty"`
	CountryOfOrigin     string   `bson:"country_of_origin,omitempty"    json:"countryOfOrigin,omitempty"`
	FSSAILicense        string   `bson:"fssai_license,omitempty"        json:"fssaiLicense,omitempty"`
	Manufacturer        string   `bson:"manufacturer,omitempty"         json:"manufacturer,omitempty"`
	ManufacturerAddress string   `bson:"manufacturer_address,omitempty" json:"manufacturerAddress,omitempty"`
}

// catalogImagePath maps a bundled asset filename to the STABLE image path the
// consumer app resolves against its API base. It routes to the catalog image
// proxy (catalog_images.go → GET /consumer/catalog/img/<file>), which streams
// the bytes from B2 at catalog/<file>. Kept in ONE place so the URL scheme and
// the B2 upload prefix never drift.
func catalogImagePath(asset string) string { return "catalog/img/" + asset }

// seedConsumerProducts idempotently upserts the bundled baseline into
// consumer_catalog as kind="product" (global, store-agnostic). It NEVER touches
// per-store override/addition rows (different store_id) and never overwrites a
// store manager's edited stock ($setOnInsert). Whether the consumer is SERVED
// these is separately gated by CONSUMER_CATALOG_SEED_SERVE (see catalogView).
func (r *repository) seedConsumerProducts(ctx context.Context) error {
	var products []seedProduct
	if err := json.Unmarshal(embeddedProductsSeed, &products); err != nil {
		return fmt.Errorf("consumer product seed parse: %w", err)
	}
	models := make([]mongo.WriteModel, 0, len(products))
	for _, p := range products {
		set := bson.D{
			{Key: "name", Value: p.Name},
			{Key: "category", Value: p.Category},
			{Key: "variant", Value: p.Variant},
			{Key: "description", Value: p.Description},
			{Key: "subscribable", Value: p.Subscribable},
			{Key: "price", Value: p.Price},
			{Key: "in_stock", Value: !p.OutOfStock},
			{Key: "unit", Value: p.Unit},
			{Key: "tag", Value: p.Tag},
			{Key: "base_id", Value: p.BaseID},
			{Key: "most_ordered", Value: p.MostOrdered},
			{Key: "image_asset", Value: p.ImageAsset},
			{Key: "back_image_asset", Value: p.BackImageAsset},
			{Key: "compliance", Value: p.Compliance},
			{Key: "seed_version", Value: seedProductVersion},
			{Key: "updated_by", Value: "seed"},
			{Key: "updated_at", Value: seedProductStamp},
		}
		if p.MRP != nil {
			set = append(set, bson.E{Key: "mrp", Value: *p.MRP})
		}
		if p.Rating != nil {
			set = append(set, bson.E{Key: "rating", Value: *p.Rating})
		}
		if p.RatingCount != nil {
			set = append(set, bson.E{Key: "rating_count", Value: *p.RatingCount})
		}
		if p.PackCount != nil {
			set = append(set, bson.E{Key: "pack_count", Value: *p.PackCount})
		}
		// Stable image paths served by the catalog image proxy (catalog_images.go).
		// The FE resolves these against its API base; the bytes live in B2 under
		// catalog/<asset>. Front always present; back only where one exists.
		if p.ImageAsset != "" {
			set = append(set, bson.E{Key: "photo_url", Value: catalogImagePath(p.ImageAsset)})
		}
		if p.BackImageAsset != "" {
			set = append(set, bson.E{Key: "back_photo_url", Value: catalogImagePath(p.BackImageAsset)})
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.D{{Key: "store_id", Value: globalCatalogStore}, {Key: "sku_id", Value: p.ID}}).
			SetUpdate(bson.D{
				{Key: "$set", Value: set},
				{Key: "$setOnInsert", Value: bson.D{
					{Key: "kind", Value: catalogKindProduct},
					{Key: "stock_count", Value: defaultSeedStock}, // store manager edits; never re-seeded over
					{Key: "created_at", Value: seedProductStamp},
				}},
			}).
			SetUpsert(true))
	}
	if len(models) == 0 {
		return nil
	}
	// ONE round-trip instead of len(products) sequential upserts — the 48-upsert
	// loop blew the boot deadline against a remote Atlas cluster. Unordered so
	// every upsert applies independently (one bad row can't abort the rest).
	if _, err := r.catalog.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false)); err != nil {
		return fmt.Errorf("consumer product seed bulk upsert: %w", err)
	}
	return nil
}

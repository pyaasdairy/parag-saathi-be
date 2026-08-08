package consumer

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// CONSUMER CATALOG OVERLAY — a thin, store-manager-owned layer over the milk
// baseline the consumer app ships in constants/products.ts. The Store Manager
// console (Saathi operator side, STORE_MANAGER role) can, per store:
//
//   - OVERRIDE a baseline SKU: change its price, flip it in/out-of-stock, or
//     hide it entirely (kind="override" doc, keyed by the baseline sku id);
//   - ADD a new SKU the baseline doesn't carry (kind="addition" doc with its
//     own category / variant / photo / price).
//
// The consumer app reads GET /consumer/catalog (app-key gated, alongside
// /traceability) and merges this overlay onto its shipped baseline. The overlay
// NEVER replaces the baseline — an empty overlay leaves the app's own product
// list untouched, so a catalog read failure degrades gracefully.
//
// Everything here stays inside the consumer_* namespace (collCatalog) and never
// touches operator business state — store OWNERSHIP is the only operator read,
// reused via service.assertStore (the same guard the store order flow uses).

// ── Baseline (mirrors the ~8 core milk SKUs from the consumer app's
// constants/products.ts) so the store console can render the products it may
// override without shipping the whole app catalog to the backend. Kept
// deliberately small: id / name / category / variant / price only. ─────────────
type baselineSku struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Variant  string  `json:"variant"`
	Price    float64 `json:"price"`
}

var consumerBaseline = []baselineSku{
	{ID: "taaza-500ml", Name: "Toned Milk - Parag Taaza", Category: "milk", Variant: "500ml", Price: 29},
	{ID: "taaza-1l", Name: "Toned Milk - Parag Taaza", Category: "milk", Variant: "1L", Price: 57},
	{ID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Category: "milk", Variant: "500ml", Price: 35},
	{ID: "gold-1l", Name: "Full Cream Milk - Parag Gold", Category: "milk", Variant: "1L", Price: 69},
	{ID: "shakti-500ml", Name: "Standardised Milk - Parag Shakti", Category: "milk", Variant: "500ml", Price: 32},
	{ID: "chai-special-500ml", Name: "Chai Special - Parag", Category: "super_tea", Variant: "500ml", Price: 32},
	{ID: "dahi-sweet-200g", Name: "Sweet Dahi", Category: "dahi", Variant: "200g Cup", Price: 24},
	{ID: "paneer-1kg", Name: "Paneer", Category: "paneer", Variant: "1kg", Price: 410},
}

// baselineIDs is the set of baseline sku ids — an ADD must not shadow one (that
// would be an override, not a new SKU).
var baselineIDs = func() map[string]struct{} {
	m := make(map[string]struct{}, len(consumerBaseline))
	for _, b := range consumerBaseline {
		m[b.ID] = struct{}{}
	}
	return m
}()

// validCategories mirrors the consumer app's Category union (products.ts). An
// addition (and any category we surface) must be one of these.
var validCategories = map[string]struct{}{
	"milk": {}, "dahi": {}, "paneer": {}, "ghee": {}, "butter": {}, "chaach": {},
	"flavoured_milk": {}, "mattha": {}, "lassi": {}, "khoya": {}, "super_tea": {}, "sweets": {},
}

const (
	catalogKindOverride = "override"
	catalogKindAddition = "addition"
	// catalogKindProduct is a seeded, store-agnostic baseline product (store_id =
	// globalCatalogStore). It projects to the consumer exactly like an addition,
	// but only when CONSUMER_CATALOG_SEED_SERVE is on (see catalogView).
	catalogKindProduct = "product"

	minCatalogPrice = 1.0
	maxCatalogPrice = 100000.0
)

// variantDoc is one purchasable variant hanging off an addition (base) product:
// a size/pack of the same product (e.g. "500ml Pouch" vs "1L Pouch") carrying
// its own price, photo, stock flag and physical descriptors. The bson keys are
// snake_case (storage); the json keys are the camelCase the contract canonicalises
// on — so a variantDoc doubles as its own read-view (no separate projection type).
type variantDoc struct {
	VariantID  string            `bson:"variant_id"             json:"variantId"`
	Label      string            `bson:"label"                  json:"label"`
	Price      float64           `bson:"price"                  json:"price"`
	ImageURL   string            `bson:"image_url,omitempty"    json:"imageUrl,omitempty"`
	OutOfStock bool              `bson:"out_of_stock,omitempty" json:"outOfStock"`
	VolumeMl   float64           `bson:"volume_ml,omitempty"    json:"volumeMl,omitempty"`
	Unit       string            `bson:"unit,omitempty"         json:"unit,omitempty"`
	Attributes map[string]string `bson:"attributes,omitempty"   json:"attributes,omitempty"`
}

// physicalDoc is the base product's physical envelope (used by delivery/packing
// and, later, by the Dolibarr product sync). Same bson=storage / json=contract
// camelCase split as variantDoc, so it is its own read-view too.
type physicalDoc struct {
	VolumeMl   float64 `bson:"volume_ml,omitempty" json:"volumeMl,omitempty"`
	WeightG    float64 `bson:"weight_g,omitempty"  json:"weightG,omitempty"`
	Dimensions string  `bson:"dimensions,omitempty" json:"dimensions,omitempty"`
}

// catalogDoc is one overlay row: a per-store override on a baseline SKU, or a
// store-added SKU. Unified fields (price / in_stock / hidden) apply to both;
// the name/category/variant/photo_url fields carry an addition's identity. kind
// distinguishes the two so a baseline override never masquerades as a product.
//
// An addition is a BASE product that may carry many variants[] (sizes/packs) and
// a physical{} envelope. base_id is an optional external/base handle (empty for a
// natively-added product) that a future DOLIBARR-SYNC adapter can key on — see
// the DOLIBARR-SYNC seam note below.
type catalogDoc struct {
	ID           primitive.ObjectID `bson:"_id,omitempty"`
	StoreID      string             `bson:"store_id"`
	SkuID        string             `bson:"sku_id"`
	Kind         string             `bson:"kind"`
	Price        *float64           `bson:"price,omitempty"`
	InStock      *bool              `bson:"in_stock,omitempty"`
	Hidden       *bool              `bson:"hidden,omitempty"`
	Name         string             `bson:"name,omitempty"`
	Category     string             `bson:"category,omitempty"`
	Variant      string             `bson:"variant,omitempty"`
	Description  string             `bson:"description,omitempty"`
	Subscribable *bool              `bson:"subscribable,omitempty"`
	PhotoURL     string             `bson:"photo_url,omitempty"`
	BaseID       string             `bson:"base_id,omitempty"`
	Variants     []variantDoc       `bson:"variants,omitempty"`
	Physical     *physicalDoc       `bson:"physical,omitempty"`
	// Extended product fields — carried by seeded baseline products (kind=product)
	// and available to additions. All optional; the consumer view emits them when
	// present, and the app falls back to its own defaults when absent.
	MRP            *float64       `bson:"mrp,omitempty"`
	Unit           string         `bson:"unit,omitempty"`
	Tag            string         `bson:"tag,omitempty"`
	Rating         *float64       `bson:"rating,omitempty"`
	RatingCount    *int           `bson:"rating_count,omitempty"`
	MostOrdered    *bool          `bson:"most_ordered,omitempty"`
	PackCount      *int           `bson:"pack_count,omitempty"`
	BackPhotoURL   string         `bson:"back_photo_url,omitempty"`
	ImageAsset     string         `bson:"image_asset,omitempty"`
	BackImageAsset string         `bson:"back_image_asset,omitempty"`
	Compliance     *complianceDoc `bson:"compliance,omitempty"`
	// StockCount is the store console's on-hand number. NEVER projected to the
	// consumer (see additionViewFromDoc / catalogView).
	StockCount  *int      `bson:"stock_count,omitempty"`
	SeedVersion int       `bson:"seed_version,omitempty"`
	UpdatedBy   string    `bson:"updated_by"`
	UpdatedAt   time.Time `bson:"updated_at"`
	CreatedAt   time.Time `bson:"created_at"`
}

// DOLIBARR-SYNC seam (NOT integrated). Additions above are the exact shape a
// future Dolibarr → Saathi product adapter would feed: Dolibarr's product +
// variant (product_attribute) + weight/volume/dimension fields map 1:1 onto a
// catalogDoc addition (base_id ← Dolibarr product ref, variants[] ← its
// combinations, physical{} ← its weight/volume/length·width·height). When that
// adapter lands it will UPSERT additions through the same repo methods the store
// console uses (insertAddition / pushVariants / patchExisting), keyed by base_id,
// so Dolibarr-sourced and hand-added products coexist in one overlay. Nothing
// here calls Dolibarr today — this is a documented seam only.

// ── Consumer-facing view (GET /consumer/catalog) ────────────────────────────

// overrideView is the per-SKU overlay the consumer app merges onto its baseline
// product. Pointer fields so an unset override key is simply absent (the app
// keeps the baseline value).
type overrideView struct {
	Price   *float64 `json:"price,omitempty"`
	InStock *bool    `json:"in_stock,omitempty"`
	Hidden  *bool    `json:"hidden,omitempty"`
}

// additionView is a store-added SKU as the consumer app consumes it. A base
// product carries its variants[] and physical{} envelope (both omitted when
// empty, so a plain single-SKU addition still reads exactly as before).
type additionView struct {
	ID           string       `json:"id"`
	BaseID       string       `json:"baseId,omitempty"`
	Name         string       `json:"name"`
	Category     string       `json:"category"`
	Variant      string       `json:"variant,omitempty"`
	Description  string       `json:"description,omitempty"`
	Subscribable bool         `json:"subscribable"`
	Price        float64      `json:"price"`
	PhotoURL     string       `json:"photo_url,omitempty"`
	InStock      bool         `json:"in_stock"`
	Variants     []variantDoc `json:"variants,omitempty"`
	Physical     *physicalDoc `json:"physical,omitempty"`
	// Extended fields (present for seeded baseline products). Note: stock_count is
	// intentionally NOT here — on-hand stock is store-console-only, never shown to
	// shoppers.
	MRP          *float64       `json:"mrp,omitempty"`
	Unit         string         `json:"unit,omitempty"`
	Tag          string         `json:"tag,omitempty"`
	Rating       *float64       `json:"rating,omitempty"`
	RatingCount  *int           `json:"ratingCount,omitempty"`
	MostOrdered  bool           `json:"mostOrdered,omitempty"`
	PackCount    *int           `json:"packCount,omitempty"`
	BackPhotoURL string         `json:"back_photo_url,omitempty"`
	Compliance   *complianceDoc `json:"compliance,omitempty"`
}

// catalogResponse is the whole overlay: a map of baseline-SKU overrides keyed by
// sku id, the list of store additions, and a version (ms) the app can cache on.
type catalogResponse struct {
	Overrides map[string]overrideView `json:"overrides"`
	Additions []additionView          `json:"additions"`
	Version   int64                   `json:"version"`
}

// ── Store-console view (GET /consumer/stores/{storeId}/skus) ────────────────

type storeOverrideView struct {
	SkuID     string    `json:"sku_id"`
	Price     *float64  `json:"price,omitempty"`
	InStock   *bool     `json:"in_stock,omitempty"`
	Hidden    *bool     `json:"hidden,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storeAdditionView struct {
	ID           string       `json:"id"`
	BaseID       string       `json:"baseId,omitempty"`
	Name         string       `json:"name"`
	Category     string       `json:"category"`
	Variant      string       `json:"variant,omitempty"`
	Description  string       `json:"description,omitempty"`
	Subscribable bool         `json:"subscribable"`
	Price        float64      `json:"price"`
	PhotoURL     string       `json:"photo_url,omitempty"`
	InStock      bool         `json:"in_stock"`
	Hidden       bool         `json:"hidden,omitempty"`
	Variants     []variantDoc `json:"variants,omitempty"`
	Physical     *physicalDoc `json:"physical,omitempty"`
	UpdatedBy    string       `json:"updated_by,omitempty"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// additionViewFromDoc / storeAdditionViewFromDoc project an addition catalogDoc
// onto its consumer / store-console read shape (shared by the list + write paths
// so every response serialises baseId + variants[] + physical{} identically).
// subscribableOrDairyDefault reports a stored addition's subscribable flag,
// defaulting an unset flag to true for milk (so a hand-added milk SKU is
// subscribable by default, matching the consumer's bundled behaviour).
func subscribableOrDairyDefault(d catalogDoc) bool {
	if d.Subscribable != nil {
		return *d.Subscribable
	}
	return d.Category == "milk"
}

func additionViewFromDoc(d catalogDoc) additionView {
	return additionView{
		ID:           d.SkuID,
		BaseID:       d.BaseID,
		Name:         d.Name,
		Category:     d.Category,
		Variant:      d.Variant,
		Description:  d.Description,
		Subscribable: subscribableOrDairyDefault(d),
		Price:        valOrZero(d.Price),
		PhotoURL:     d.PhotoURL,
		InStock:      d.InStock == nil || *d.InStock, // default in-stock
		Variants:     d.Variants,
		Physical:     d.Physical,
		MRP:          d.MRP,
		Unit:         d.Unit,
		Tag:          d.Tag,
		Rating:       d.Rating,
		RatingCount:  d.RatingCount,
		MostOrdered:  d.MostOrdered != nil && *d.MostOrdered,
		PackCount:    d.PackCount,
		BackPhotoURL: d.BackPhotoURL,
		Compliance:   d.Compliance,
		// StockCount deliberately omitted — never projected to the consumer.
	}
}

func storeAdditionViewFromDoc(d catalogDoc) storeAdditionView {
	return storeAdditionView{
		ID:           d.SkuID,
		BaseID:       d.BaseID,
		Name:         d.Name,
		Category:     d.Category,
		Variant:      d.Variant,
		Description:  d.Description,
		Subscribable: subscribableOrDairyDefault(d),
		Price:        valOrZero(d.Price),
		PhotoURL:     d.PhotoURL,
		InStock:      d.InStock == nil || *d.InStock,
		Hidden:       d.Hidden != nil && *d.Hidden,
		Variants:     d.Variants,
		Physical:     d.Physical,
		UpdatedBy:    d.UpdatedBy,
		UpdatedAt:    d.UpdatedAt,
	}
}

type storeCatalogResponse struct {
	StoreID   string              `json:"store_id"`
	Baseline  []baselineSku       `json:"baseline"`
	Overrides []storeOverrideView `json:"overrides"`
	Additions []storeAdditionView `json:"additions"`
}

// ── Request bodies ──────────────────────────────────────────────────────────

// variantInput is a variant on the wire (POST base+variants, POST variant-to-base,
// or PATCH edit_variant). Following the km/unit tolerance style, every key is
// accepted in BOTH snake_case and camelCase; normalize() folds the aliases onto
// the canonical field. Price is a pointer so "absent" is distinguishable from 0
// (a variant PATCH must not zero a price the caller never sent).
type variantInput struct {
	VariantID   string            `json:"variant_id"`
	VariantIDC  string            `json:"variantId"`
	Label       string            `json:"label"`
	Price       *float64          `json:"price"`
	ImageURL    string            `json:"image_url"`
	ImageURLC   string            `json:"imageUrl"`
	OutOfStock  *bool             `json:"out_of_stock"`
	OutOfStockC *bool             `json:"outOfStock"`
	VolumeMl    float64           `json:"volume_ml"`
	VolumeMlC   float64           `json:"volumeMl"`
	Unit        string            `json:"unit"`
	Attributes  map[string]string `json:"attributes"`
}

func (v *variantInput) normalize() {
	if v.VariantID == "" && v.VariantIDC != "" {
		v.VariantID = v.VariantIDC
	}
	if v.ImageURL == "" && v.ImageURLC != "" {
		v.ImageURL = v.ImageURLC
	}
	if v.OutOfStock == nil && v.OutOfStockC != nil {
		v.OutOfStock = v.OutOfStockC
	}
	if v.VolumeMl == 0 && v.VolumeMlC != 0 {
		v.VolumeMl = v.VolumeMlC
	}
}

// physicalInput is the physical{} envelope on the wire; snake+camel tolerant.
type physicalInput struct {
	VolumeMl   float64 `json:"volume_ml"`
	VolumeMlC  float64 `json:"volumeMl"`
	WeightG    float64 `json:"weight_g"`
	WeightGC   float64 `json:"weightG"`
	Dimensions string  `json:"dimensions"`
}

func (p *physicalInput) normalize() {
	if p.VolumeMl == 0 && p.VolumeMlC != 0 {
		p.VolumeMl = p.VolumeMlC
	}
	if p.WeightG == 0 && p.WeightGC != 0 {
		p.WeightG = p.WeightGC
	}
}

func (p *physicalInput) toDoc() *physicalDoc {
	if p == nil {
		return nil
	}
	return &physicalDoc{VolumeMl: p.VolumeMl, WeightG: p.WeightG, Dimensions: strings.TrimSpace(p.Dimensions)}
}

// addSkuRequest is the POST body. Two shapes share it (km/unit tolerance style,
// snake+camel accepted throughout):
//
//   - ADD A BASE (base_id absent): name + category identify a new base product,
//     optionally carrying variants[] and a physical{} envelope. A legacy flat
//     single-SKU addition (top-level price/variant, no variants[]) still works.
//   - ADD A VARIANT TO A BASE (base_id set): variants[] are appended to the
//     existing addition whose id == base_id; the base's own fields are ignored.
type addSkuRequest struct {
	ID           string         `json:"id"`
	BaseID       string         `json:"base_id"`
	BaseIDC      string         `json:"baseId"`
	Name         string         `json:"name"`
	Category     string         `json:"category"`
	Variant      string         `json:"variant"`
	Description  string         `json:"description"`
	Subscribable *bool          `json:"subscribable"`
	Price        float64        `json:"price"`
	PhotoURL     string         `json:"photo_url"`
	PhotoURLC    string         `json:"photoUrl"`
	InStock      *bool          `json:"in_stock"`
	InStockC     *bool          `json:"inStock"`
	Variants     []variantInput `json:"variants"`
	Physical     *physicalInput `json:"physical"`
}

func (a *addSkuRequest) normalize() {
	if a.BaseID == "" && a.BaseIDC != "" {
		a.BaseID = a.BaseIDC
	}
	if a.PhotoURL == "" && a.PhotoURLC != "" {
		a.PhotoURL = a.PhotoURLC
	}
	if a.InStock == nil && a.InStockC != nil {
		a.InStock = a.InStockC
	}
	for i := range a.Variants {
		a.Variants[i].normalize()
	}
	if a.Physical != nil {
		a.Physical.normalize()
	}
}

// patchSkuRequest edits ANY field of a base OR a specific variant. Base-level
// fields are optional pointers (only sent keys are applied). When edit_variant
// is present it patches the single variant it names (variantId); otherwise the
// body patches the base / baseline override. snake+camel accepted throughout.
type patchSkuRequest struct {
	Price        *float64       `json:"price"`
	InStock      *bool          `json:"in_stock"`
	InStockC     *bool          `json:"inStock"`
	OutOfStock   *bool          `json:"out_of_stock"` // Saathi console sends this; folded to in_stock
	OutOfStockC  *bool          `json:"outOfStock"`
	Hidden       *bool          `json:"hidden"`
	Name         *string        `json:"name"`
	Category     *string        `json:"category"`
	Variant      *string        `json:"variant"`
	Description  *string        `json:"description"`
	Subscribable *bool          `json:"subscribable"`
	PhotoURL     *string        `json:"photo_url"`
	PhotoURLC    *string        `json:"photoUrl"`
	Physical     *physicalInput `json:"physical"`

	EditVariant  *variantInput `json:"edit_variant"`
	EditVariantC *variantInput `json:"editVariant"`
}

func (p *patchSkuRequest) normalize() {
	// out_of_stock (the console's field) is the inverse of the stored in_stock;
	// fold it so a base OOS toggle actually persists. An explicit in_stock wins.
	if p.OutOfStock == nil && p.OutOfStockC != nil {
		p.OutOfStock = p.OutOfStockC
	}
	if p.InStock == nil && p.OutOfStock != nil {
		v := !*p.OutOfStock
		p.InStock = &v
	}
	if p.InStock == nil && p.InStockC != nil {
		p.InStock = p.InStockC
	}
	if p.PhotoURL == nil && p.PhotoURLC != nil {
		p.PhotoURL = p.PhotoURLC
	}
	if p.Physical != nil {
		p.Physical.normalize()
	}
	if p.EditVariant == nil && p.EditVariantC != nil {
		p.EditVariant = p.EditVariantC
	}
	if p.EditVariant != nil {
		p.EditVariant.normalize()
	}
}

// ── Validation ──────────────────────────────────────────────────────────────

func validatePrice(p float64) *apiError {
	if p < minCatalogPrice || p > maxCatalogPrice {
		return errUnprocessable("INVALID_PRICE", "price must be between 1 and 100000")
	}
	return nil
}

func validateCategory(c string) *apiError {
	if _, ok := validCategories[c]; !ok {
		return errUnprocessable("INVALID_CATEGORY", "category is not a recognised product category")
	}
	return nil
}

// validateVariant enforces the variant contract: a label is required and a
// price is required and in the same 1..100000 band as any other SKU. variantId
// is optional on the way in (minted when absent).
func validateVariant(v variantInput) *apiError {
	if strings.TrimSpace(v.Label) == "" {
		return errUnprocessable("VARIANT_LABEL_REQUIRED", "each variant needs a label")
	}
	if v.Price == nil {
		return errUnprocessable("VARIANT_PRICE_REQUIRED", "each variant needs a price")
	}
	return validatePrice(*v.Price)
}

// validatePhysical rejects negative physical descriptors (0 = unset).
func validatePhysical(p *physicalInput) *apiError {
	if p == nil {
		return nil
	}
	if p.VolumeMl < 0 || p.WeightG < 0 {
		return errUnprocessable("INVALID_PHYSICAL", "physical volume/weight must not be negative")
	}
	return nil
}

// variantToDoc materialises a validated variantInput into a stored variantDoc,
// minting a variant id when the caller did not supply one.
func variantToDoc(v variantInput) variantDoc {
	id := strings.TrimSpace(v.VariantID)
	if id == "" {
		id = "var-" + primitive.NewObjectID().Hex()
	}
	out := variantDoc{
		VariantID:  id,
		Label:      strings.TrimSpace(v.Label),
		VolumeMl:   v.VolumeMl,
		Unit:       strings.TrimSpace(v.Unit),
		ImageURL:   strings.TrimSpace(v.ImageURL),
		Attributes: v.Attributes,
	}
	if v.Price != nil {
		out.Price = *v.Price
	}
	if v.OutOfStock != nil {
		out.OutOfStock = *v.OutOfStock
	}
	return out
}

// ── Repository ──────────────────────────────────────────────────────────────

// ensureCatalogIndexes owns the overlay's unique key: at most one overlay row
// per (store, sku), so an override upsert and an addition insert are both
// idempotent/collision-safe on that pair.
func (r *repository) ensureCatalogIndexes(ctx context.Context) error {
	_, err := r.catalog.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "store_id", Value: 1}, {Key: "sku_id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return err
}

// upsertOverride sets (or clears) the price/in_stock/hidden overlay on a SKU for
// a store. It only stamps kind=override ON INSERT, so patching a store ADDITION
// keeps its kind (and identity) intact while still updating its price/stock.
func (r *repository) upsertOverride(ctx context.Context, storeID, skuID string, set bson.D, updatedBy string) (*catalogDoc, error) {
	now := time.Now().UTC()
	set = append(set,
		bson.E{Key: "updated_by", Value: updatedBy},
		bson.E{Key: "updated_at", Value: now},
	)
	after := options.After
	var doc catalogDoc
	err := r.catalog.FindOneAndUpdate(ctx,
		bson.D{{Key: "store_id", Value: storeID}, {Key: "sku_id", Value: skuID}},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "kind", Value: catalogKindOverride},
				{Key: "created_at", Value: now},
			}},
		},
		options.FindOneAndUpdate().SetReturnDocument(after).SetUpsert(true),
	).Decode(&doc)
	if err != nil {
		return nil, errInternal("catalog override failed")
	}
	return &doc, nil
}

// insertAddition stores a store-added SKU. A duplicate (store, sku) is a
// conflict — the SKU already exists (use PATCH to edit it).
func (r *repository) insertAddition(ctx context.Context, doc *catalogDoc) error {
	if _, err := r.catalog.InsertOne(ctx, doc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("SKU_EXISTS", "a SKU with this id already exists for this store")
		}
		return errInternal("catalog addition failed")
	}
	return nil
}

// pushVariants appends variants to an existing addition (base) product,
// identified by (store, sku). Returns errNotFound if no such addition exists.
func (r *repository) pushVariants(ctx context.Context, storeID, baseID string, variants []variantDoc, updatedBy string) (*catalogDoc, error) {
	now := time.Now().UTC()
	after := options.After
	var doc catalogDoc
	err := r.catalog.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "store_id", Value: storeID},
			{Key: "sku_id", Value: baseID},
			{Key: "kind", Value: catalogKindAddition},
		},
		bson.D{
			{Key: "$push", Value: bson.D{{Key: "variants", Value: bson.D{{Key: "$each", Value: variants}}}}},
			{Key: "$set", Value: bson.D{
				{Key: "updated_by", Value: updatedBy},
				{Key: "updated_at", Value: now},
			}},
		},
		options.FindOneAndUpdate().SetReturnDocument(after),
	).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errNotFound("no base product with this id to add a variant to")
		}
		return nil, errInternal("catalog variant add failed")
	}
	return &doc, nil
}

// patchExisting applies a $set to an existing overlay row (used when the patch
// touches addition-only fields like name/category/photo/physical, which must not
// upsert a phantom doc onto a baseline sku). Returns errNotFound if absent.
func (r *repository) patchExisting(ctx context.Context, storeID, skuID string, set bson.D, updatedBy string) (*catalogDoc, error) {
	now := time.Now().UTC()
	set = append(set,
		bson.E{Key: "updated_by", Value: updatedBy},
		bson.E{Key: "updated_at", Value: now},
	)
	after := options.After
	var doc catalogDoc
	err := r.catalog.FindOneAndUpdate(ctx,
		bson.D{{Key: "store_id", Value: storeID}, {Key: "sku_id", Value: skuID}},
		bson.D{{Key: "$set", Value: set}},
		options.FindOneAndUpdate().SetReturnDocument(after),
	).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errNotFound("no catalog entry for this sku")
		}
		return nil, errInternal("catalog patch failed")
	}
	return &doc, nil
}

// patchVariant edits a single variant in place (positional arrayFilters on
// variant_id) inside an addition. set holds the per-variant field writes already
// keyed as variants.$[v].<field>. Returns errNotFound if the base or the named
// variant does not exist.
func (r *repository) patchVariant(ctx context.Context, storeID, skuID, variantID string, set bson.D, updatedBy string) (*catalogDoc, error) {
	now := time.Now().UTC()
	set = append(set,
		bson.E{Key: "updated_by", Value: updatedBy},
		bson.E{Key: "updated_at", Value: now},
	)
	after := options.After
	var doc catalogDoc
	err := r.catalog.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "store_id", Value: storeID},
			{Key: "sku_id", Value: skuID},
			{Key: "variants.variant_id", Value: variantID},
		},
		bson.D{{Key: "$set", Value: set}},
		options.FindOneAndUpdate().
			SetReturnDocument(after).
			SetArrayFilters(options.ArrayFilters{Filters: []interface{}{bson.M{"v.variant_id": variantID}}}),
	).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errNotFound("no variant with this id on this product")
		}
		return nil, errInternal("catalog variant patch failed")
	}
	return &doc, nil
}

// deleteVariant removes a single variant ($pull by variant_id) from an addition,
// leaving the base product intact. Returns errNotFound if nothing was removed.
func (r *repository) deleteVariant(ctx context.Context, storeID, skuID, variantID string) error {
	res, err := r.catalog.UpdateOne(ctx,
		bson.D{{Key: "store_id", Value: storeID}, {Key: "sku_id", Value: skuID}},
		bson.D{
			{Key: "$pull", Value: bson.D{{Key: "variants", Value: bson.D{{Key: "variant_id", Value: variantID}}}}},
			{Key: "$set", Value: bson.D{{Key: "updated_at", Value: time.Now().UTC()}}},
		},
	)
	if err != nil {
		return errInternal("catalog variant delete failed")
	}
	if res.ModifiedCount == 0 {
		return errNotFound("no variant with this id on this product")
	}
	return nil
}

// deleteSku removes a store's overlay row for a SKU — clears a baseline override
// (resetting it to the shipped default) or removes a store addition entirely.
func (r *repository) deleteSku(ctx context.Context, storeID, skuID string) (int64, error) {
	res, err := r.catalog.DeleteOne(ctx, bson.D{{Key: "store_id", Value: storeID}, {Key: "sku_id", Value: skuID}})
	if err != nil {
		return 0, errInternal("catalog delete failed")
	}
	return res.DeletedCount, nil
}

// listOverridesAndAdditions returns every overlay row for a store (both kinds),
// newest first — the store console's working set.
func (r *repository) listOverridesAndAdditions(ctx context.Context, storeID string) ([]catalogDoc, error) {
	cur, err := r.catalog.Find(ctx, bson.D{{Key: "store_id", Value: storeID}},
		options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}))
	if err != nil {
		return nil, errInternal("catalog list failed")
	}
	out := []catalogDoc{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("catalog decode failed")
	}
	return out, nil
}

// catalogView projects the whole overlay (across every store — the pilot is
// single-store, and the consumer app has no store context at read time) into
// the consumer-facing overrides map + additions list + version. Rows are read
// oldest→newest so a later write wins on any (rare) cross-store sku collision,
// and the max updated_at becomes the cache version. Hidden additions are omitted
// (a hidden store SKU should not surface to shoppers at all).
func (r *repository) catalogView(ctx context.Context, serveSeeded bool) (*catalogResponse, error) {
	cur, err := r.catalog.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "updated_at", Value: 1}}))
	if err != nil {
		return nil, errInternal("catalog view failed")
	}
	var docs []catalogDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, errInternal("catalog decode failed")
	}
	resp := &catalogResponse{Overrides: map[string]overrideView{}, Additions: []additionView{}}
	var maxT time.Time
	for _, d := range docs {
		// Seeded baseline products are inert until CONSUMER_CATALOG_SEED_SERVE is on:
		// skip them BEFORE the version stamp so the response stays byte-identical.
		if d.Kind == catalogKindProduct && !serveSeeded {
			continue
		}
		if d.UpdatedAt.After(maxT) {
			maxT = d.UpdatedAt
		}
		if d.Kind == catalogKindAddition || d.Kind == catalogKindProduct {
			if d.Hidden != nil && *d.Hidden {
				continue // a hidden addition/product is not shown to shoppers
			}
			resp.Additions = append(resp.Additions, additionViewFromDoc(d))
			continue
		}
		resp.Overrides[d.SkuID] = overrideView{Price: d.Price, InStock: d.InStock, Hidden: d.Hidden}
	}
	if !maxT.IsZero() {
		resp.Version = maxT.UnixMilli()
	}
	return resp, nil
}

func valOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// ── Service ─────────────────────────────────────────────────────────────────

// consumerCatalog builds the consumer-facing overlay (app-key gated at the
// handler).
func (s *service) consumerCatalog(ctx context.Context) (*catalogResponse, error) {
	return s.repo.catalogView(ctx, s.catalogServeSeeded)
}

// storeSkus returns the store console's view: baseline + this store's overrides
// and additions. Guarded to the store the actor actually manages.
func (s *service) storeSkus(ctx context.Context, actor auth.Actor, storeID string) (*storeCatalogResponse, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	docs, err := s.repo.listOverridesAndAdditions(ctx, storeID)
	if err != nil {
		return nil, err
	}
	resp := &storeCatalogResponse{
		StoreID:   storeID,
		Baseline:  consumerBaseline,
		Overrides: []storeOverrideView{},
		Additions: []storeAdditionView{},
	}
	for _, d := range docs {
		if d.Kind == catalogKindAddition {
			resp.Additions = append(resp.Additions, storeAdditionViewFromDoc(d))
			continue
		}
		resp.Overrides = append(resp.Overrides, storeOverrideView{
			SkuID:     d.SkuID,
			Price:     d.Price,
			InStock:   d.InStock,
			Hidden:    d.Hidden,
			UpdatedBy: d.UpdatedBy,
			UpdatedAt: d.UpdatedAt,
		})
	}
	return resp, nil
}

// addStoreSku handles both POST shapes (see addSkuRequest). With base_id set it
// APPENDS the request's variants[] to that existing base; otherwise it CREATES a
// new base product (name required, category in the fixed set, id must not shadow
// a baseline SKU) carrying its variants[] + physical{}. Every variant is
// validated (label + price) either way; base_id itself is optional.
func (s *service) addStoreSku(ctx context.Context, actor auth.Actor, storeID string, req addSkuRequest) (*storeAdditionView, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	req.normalize()

	// Validate any supplied variants up front (both shapes need this).
	for _, v := range req.Variants {
		if err := validateVariant(v); err != nil {
			return nil, err
		}
	}

	// Shape 2: add variant(s) to an existing base.
	if baseID := strings.TrimSpace(req.BaseID); baseID != "" {
		if len(req.Variants) == 0 {
			return nil, errUnprocessable("VARIANTS_REQUIRED", "adding to a base needs at least one variant")
		}
		vdocs := make([]variantDoc, 0, len(req.Variants))
		for _, v := range req.Variants {
			vdocs = append(vdocs, variantToDoc(v))
		}
		doc, err := s.repo.pushVariants(ctx, storeID, baseID, vdocs, actor.PartyID)
		if err != nil {
			return nil, err
		}
		view := storeAdditionViewFromDoc(*doc)
		return &view, nil
	}

	// Shape 1: create a new base product.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errUnprocessable("NAME_REQUIRED", "a product name is required")
	}
	if err := validateCategory(strings.TrimSpace(req.Category)); err != nil {
		return nil, err
	}
	if err := validatePhysical(req.Physical); err != nil {
		return nil, err
	}
	// A base is priced either by its own top-level price OR entirely by its
	// variants. Range-check the top-level price only when one is given.
	if req.Price != 0 || len(req.Variants) == 0 {
		if err := validatePrice(req.Price); err != nil {
			return nil, err
		}
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = "sku-" + primitive.NewObjectID().Hex()
	}
	if _, clash := baselineIDs[id]; clash {
		return nil, errConflict("BASELINE_SKU", "that id is a baseline SKU — override it instead of adding it")
	}
	now := time.Now().UTC()
	inStock := true
	if req.InStock != nil {
		inStock = *req.InStock
	}
	price := req.Price
	var vdocs []variantDoc
	for _, v := range req.Variants {
		vdocs = append(vdocs, variantToDoc(v))
	}
	doc := &catalogDoc{
		StoreID:      storeID,
		SkuID:        id,
		Kind:         catalogKindAddition,
		Price:        &price,
		InStock:      &inStock,
		Name:         name,
		Category:     strings.TrimSpace(req.Category),
		Variant:      strings.TrimSpace(req.Variant),
		Description:  strings.TrimSpace(req.Description),
		Subscribable: req.Subscribable,
		PhotoURL:     strings.TrimSpace(req.PhotoURL),
		BaseID:       strings.TrimSpace(req.BaseID),
		Variants:     vdocs,
		Physical:     req.Physical.toDoc(),
		UpdatedBy:    actor.PartyID,
		UpdatedAt:    now,
		CreatedAt:    now,
	}
	if err := s.repo.insertAddition(ctx, doc); err != nil {
		return nil, err
	}
	view := storeAdditionViewFromDoc(*doc)
	return &view, nil
}

// patchStoreSku edits ANY field of a base OR of a specific variant. When
// edit_variant is present it patches that one variant (price/stock/photo/volume/
// unit/attributes) in place. Otherwise it patches the base: price/in_stock/hidden
// keep upserting (so a baseline SKU override still works with no prior doc), while
// addition-only fields (name/category/variant/photo/physical) update an existing
// addition in place (never upsert a phantom onto a baseline sku).
func (s *service) patchStoreSku(ctx context.Context, actor auth.Actor, storeID, skuID string, req patchSkuRequest) (*catalogDoc, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	skuID = strings.TrimSpace(skuID)
	if skuID == "" {
		return nil, errBadRequest("a sku id is required")
	}
	req.normalize()

	// Variant-level edit: patch the single named variant in place.
	if ev := req.EditVariant; ev != nil {
		vid := strings.TrimSpace(ev.VariantID)
		if vid == "" {
			return nil, errBadRequest("edit_variant needs a variantId to identify the variant")
		}
		set := bson.D{}
		if ev.Price != nil {
			if err := validatePrice(*ev.Price); err != nil {
				return nil, err
			}
			set = append(set, bson.E{Key: "variants.$[v].price", Value: *ev.Price})
		}
		if strings.TrimSpace(ev.Label) != "" {
			set = append(set, bson.E{Key: "variants.$[v].label", Value: strings.TrimSpace(ev.Label)})
		}
		if strings.TrimSpace(ev.ImageURL) != "" {
			set = append(set, bson.E{Key: "variants.$[v].image_url", Value: strings.TrimSpace(ev.ImageURL)})
		}
		if ev.OutOfStock != nil {
			set = append(set, bson.E{Key: "variants.$[v].out_of_stock", Value: *ev.OutOfStock})
		}
		if ev.VolumeMl != 0 {
			set = append(set, bson.E{Key: "variants.$[v].volume_ml", Value: ev.VolumeMl})
		}
		if strings.TrimSpace(ev.Unit) != "" {
			set = append(set, bson.E{Key: "variants.$[v].unit", Value: strings.TrimSpace(ev.Unit)})
		}
		if ev.Attributes != nil {
			set = append(set, bson.E{Key: "variants.$[v].attributes", Value: ev.Attributes})
		}
		if len(set) == 0 {
			return nil, errBadRequest("nothing to update on the variant")
		}
		return s.repo.patchVariant(ctx, storeID, skuID, vid, set, actor.PartyID)
	}

	// Base-level edit. Split overlay-safe fields (upsertable on a baseline sku)
	// from addition-only fields (must target an existing addition doc).
	set := bson.D{}
	additionOnly := false
	if req.Price != nil {
		if err := validatePrice(*req.Price); err != nil {
			return nil, err
		}
		set = append(set, bson.E{Key: "price", Value: *req.Price})
	}
	if req.InStock != nil {
		set = append(set, bson.E{Key: "in_stock", Value: *req.InStock})
	}
	if req.Hidden != nil {
		set = append(set, bson.E{Key: "hidden", Value: *req.Hidden})
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, errUnprocessable("NAME_REQUIRED", "a product name cannot be blank")
		}
		set = append(set, bson.E{Key: "name", Value: name})
		additionOnly = true
	}
	if req.Category != nil {
		cat := strings.TrimSpace(*req.Category)
		if err := validateCategory(cat); err != nil {
			return nil, err
		}
		set = append(set, bson.E{Key: "category", Value: cat})
		additionOnly = true
	}
	if req.Variant != nil {
		set = append(set, bson.E{Key: "variant", Value: strings.TrimSpace(*req.Variant)})
		additionOnly = true
	}
	if req.Description != nil {
		set = append(set, bson.E{Key: "description", Value: strings.TrimSpace(*req.Description)})
		additionOnly = true
	}
	if req.Subscribable != nil {
		set = append(set, bson.E{Key: "subscribable", Value: *req.Subscribable})
		additionOnly = true
	}
	if req.PhotoURL != nil {
		set = append(set, bson.E{Key: "photo_url", Value: strings.TrimSpace(*req.PhotoURL)})
		additionOnly = true
	}
	if req.Physical != nil {
		if err := validatePhysical(req.Physical); err != nil {
			return nil, err
		}
		set = append(set, bson.E{Key: "physical", Value: req.Physical.toDoc()})
		additionOnly = true
	}
	if len(set) == 0 {
		return nil, errBadRequest("nothing to update: set price, stock, hidden, name, category, variant, photo, physical, or edit_variant")
	}
	if additionOnly {
		return s.repo.patchExisting(ctx, storeID, skuID, set, actor.PartyID)
	}
	return s.repo.upsertOverride(ctx, storeID, skuID, set, actor.PartyID)
}

// deleteStoreSku removes either a single VARIANT (variantID set → pull it, base
// stays) or the WHOLE base/override (variantID empty → drop the overlay row: a
// baseline override reset, or a store addition removed entirely).
func (s *service) deleteStoreSku(ctx context.Context, actor auth.Actor, storeID, skuID, variantID string) error {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return err
	}
	skuID = strings.TrimSpace(skuID)
	if skuID == "" {
		return errBadRequest("a sku id is required")
	}
	if variantID = strings.TrimSpace(variantID); variantID != "" {
		return s.repo.deleteVariant(ctx, storeID, skuID, variantID)
	}
	n, err := s.repo.deleteSku(ctx, storeID, skuID)
	if err != nil {
		return err
	}
	if n == 0 {
		return errNotFound("no catalog entry for this sku")
	}
	return nil
}

// ── Handlers ────────────────────────────────────────────────────────────────

// getCatalog — GET /consumer/catalog. Consumer-app-only (same X-Parag-App-Key
// gate as /traceability), raw-JSON. Returns the overlay the app merges onto its
// shipped baseline.
func (h *handler) getCatalog(w http.ResponseWriter, r *http.Request) {
	if !h.svc.appKeyOK(r) {
		writeErr(w, errForbidden("the catalog is available from the PARAG app only"))
		return
	}
	resp, err := h.svc.consumerCatalog(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// listSkus — GET /consumer/stores/{storeId}/skus (STORE_MANAGER). Operator wire
// format ({data} envelope), consumed by the Saathi store console.
func (h *handler) listSkus(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	resp, err := h.svc.storeSkus(r.Context(), actor, chi.URLParam(r, "storeId"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// addSku — POST /consumer/stores/{storeId}/skus (STORE_MANAGER).
func (h *handler) addSku(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body addSkuRequest
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	resp, err := h.svc.addStoreSku(r.Context(), actor, chi.URLParam(r, "storeId"), body)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusCreated, resp)
}

// patchSku — PATCH /consumer/stores/{storeId}/skus/{skuId} (STORE_MANAGER).
func (h *handler) patchSku(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body patchSkuRequest
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	doc, err := h.svc.patchStoreSku(r.Context(), actor, chi.URLParam(r, "storeId"), chi.URLParam(r, "skuId"), body)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	// An addition patch echoes the full product (variants/physical included); a
	// baseline override echoes the compact override row.
	if doc.Kind == catalogKindAddition {
		httpx.JSON(w, http.StatusOK, storeAdditionViewFromDoc(*doc))
		return
	}
	httpx.JSON(w, http.StatusOK, storeOverrideView{
		SkuID: doc.SkuID, Price: doc.Price, InStock: doc.InStock, Hidden: doc.Hidden,
		UpdatedBy: doc.UpdatedBy, UpdatedAt: doc.UpdatedAt,
	})
}

// deleteSkuHandler — DELETE /consumer/stores/{storeId}/skus/{skuId} (STORE_MANAGER).
// ?variantId=… (snake ?variant_id=… tolerated) deletes just that variant; absent,
// the whole base/override row is removed.
func (h *handler) deleteSkuHandler(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	variantID := r.URL.Query().Get("variantId")
	if variantID == "" {
		variantID = r.URL.Query().Get("variant_id")
	}
	if err := h.svc.deleteStoreSku(r.Context(), actor, chi.URLParam(r, "storeId"), chi.URLParam(r, "skuId"), variantID); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

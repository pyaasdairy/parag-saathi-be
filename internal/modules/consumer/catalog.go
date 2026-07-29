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
	{ID: "taaza-500ml", Name: "Toned Milk - PYAAS Taaza", Category: "milk", Variant: "500ml Pouch", Price: 29},
	{ID: "taaza-1l", Name: "Toned Milk - PYAAS Taaza", Category: "milk", Variant: "1L Pouch", Price: 57},
	{ID: "gold-500ml", Name: "Full Cream Milk - PYAAS Gold", Category: "milk", Variant: "500ml Pouch", Price: 35},
	{ID: "gold-1l", Name: "Full Cream Milk - PYAAS Gold", Category: "milk", Variant: "1L Pouch", Price: 69},
	{ID: "shakti-500ml", Name: "Standardised Milk - PYAAS Shakti", Category: "milk", Variant: "500ml Pouch", Price: 32},
	{ID: "chai-special-500ml", Name: "Chai Special - PYAAS", Category: "super_tea", Variant: "500ml Pouch", Price: 32},
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

	minCatalogPrice = 1.0
	maxCatalogPrice = 100000.0
)

// catalogDoc is one overlay row: a per-store override on a baseline SKU, or a
// store-added SKU. Unified fields (price / in_stock / hidden) apply to both;
// the name/category/variant/photo_url fields carry an addition's identity. kind
// distinguishes the two so a baseline override never masquerades as a product.
type catalogDoc struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	StoreID   string             `bson:"store_id"`
	SkuID     string             `bson:"sku_id"`
	Kind      string             `bson:"kind"`
	Price     *float64           `bson:"price,omitempty"`
	InStock   *bool              `bson:"in_stock,omitempty"`
	Hidden    *bool              `bson:"hidden,omitempty"`
	Name      string             `bson:"name,omitempty"`
	Category  string             `bson:"category,omitempty"`
	Variant   string             `bson:"variant,omitempty"`
	PhotoURL  string             `bson:"photo_url,omitempty"`
	UpdatedBy string             `bson:"updated_by"`
	UpdatedAt time.Time          `bson:"updated_at"`
	CreatedAt time.Time          `bson:"created_at"`
}

// ── Consumer-facing view (GET /consumer/catalog) ────────────────────────────

// overrideView is the per-SKU overlay the consumer app merges onto its baseline
// product. Pointer fields so an unset override key is simply absent (the app
// keeps the baseline value).
type overrideView struct {
	Price   *float64 `json:"price,omitempty"`
	InStock *bool    `json:"in_stock,omitempty"`
	Hidden  *bool    `json:"hidden,omitempty"`
}

// additionView is a store-added SKU as the consumer app consumes it.
type additionView struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Variant  string  `json:"variant,omitempty"`
	Price    float64 `json:"price"`
	PhotoURL string  `json:"photo_url,omitempty"`
	InStock  bool    `json:"in_stock"`
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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Category  string    `json:"category"`
	Variant   string    `json:"variant,omitempty"`
	Price     float64   `json:"price"`
	PhotoURL  string    `json:"photo_url,omitempty"`
	InStock   bool      `json:"in_stock"`
	Hidden    bool      `json:"hidden,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storeCatalogResponse struct {
	StoreID   string              `json:"store_id"`
	Baseline  []baselineSku       `json:"baseline"`
	Overrides []storeOverrideView `json:"overrides"`
	Additions []storeAdditionView `json:"additions"`
}

// ── Request bodies ──────────────────────────────────────────────────────────

type addSkuRequest struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Variant  string  `json:"variant"`
	Price    float64 `json:"price"`
	PhotoURL string  `json:"photo_url"`
	InStock  *bool   `json:"in_stock"`
}

type patchSkuRequest struct {
	Price   *float64 `json:"price"`
	InStock *bool    `json:"in_stock"`
	Hidden  *bool    `json:"hidden"`
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
func (r *repository) catalogView(ctx context.Context) (*catalogResponse, error) {
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
		if d.UpdatedAt.After(maxT) {
			maxT = d.UpdatedAt
		}
		if d.Kind == catalogKindAddition {
			if d.Hidden != nil && *d.Hidden {
				continue // a hidden addition is not shown to shoppers
			}
			resp.Additions = append(resp.Additions, additionView{
				ID:       d.SkuID,
				Name:     d.Name,
				Category: d.Category,
				Variant:  d.Variant,
				Price:    valOrZero(d.Price),
				PhotoURL: d.PhotoURL,
				InStock:  d.InStock == nil || *d.InStock, // default in-stock
			})
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
	return s.repo.catalogView(ctx)
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
			resp.Additions = append(resp.Additions, storeAdditionView{
				ID:        d.SkuID,
				Name:      d.Name,
				Category:  d.Category,
				Variant:   d.Variant,
				Price:     valOrZero(d.Price),
				PhotoURL:  d.PhotoURL,
				InStock:   d.InStock == nil || *d.InStock,
				Hidden:    d.Hidden != nil && *d.Hidden,
				UpdatedBy: d.UpdatedBy,
				UpdatedAt: d.UpdatedAt,
			})
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

// addStoreSku validates and inserts a store-added SKU. name required, category
// in the fixed set, price in 1..100000, and the id must not shadow a baseline
// SKU (that would be an override, not an addition).
func (s *service) addStoreSku(ctx context.Context, actor auth.Actor, storeID string, req addSkuRequest) (*storeAdditionView, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errUnprocessable("NAME_REQUIRED", "a product name is required")
	}
	if err := validateCategory(strings.TrimSpace(req.Category)); err != nil {
		return nil, err
	}
	if err := validatePrice(req.Price); err != nil {
		return nil, err
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
	doc := &catalogDoc{
		StoreID:   storeID,
		SkuID:     id,
		Kind:      catalogKindAddition,
		Price:     &price,
		InStock:   &inStock,
		Name:      name,
		Category:  strings.TrimSpace(req.Category),
		Variant:   strings.TrimSpace(req.Variant),
		PhotoURL:  strings.TrimSpace(req.PhotoURL),
		UpdatedBy: actor.PartyID,
		UpdatedAt: now,
		CreatedAt: now,
	}
	if err := s.repo.insertAddition(ctx, doc); err != nil {
		return nil, err
	}
	return &storeAdditionView{
		ID: id, Name: name, Category: doc.Category, Variant: doc.Variant,
		Price: price, PhotoURL: doc.PhotoURL, InStock: inStock,
		UpdatedBy: actor.PartyID, UpdatedAt: now,
	}, nil
}

// patchStoreSku applies a price/stock/visibility override to a SKU (baseline or
// a store addition). At least one field must be present; price is range-checked.
func (s *service) patchStoreSku(ctx context.Context, actor auth.Actor, storeID, skuID string, req patchSkuRequest) (*catalogDoc, error) {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return nil, err
	}
	skuID = strings.TrimSpace(skuID)
	if skuID == "" {
		return nil, errBadRequest("a sku id is required")
	}
	if req.Price == nil && req.InStock == nil && req.Hidden == nil {
		return nil, errBadRequest("nothing to update: set price, in_stock, or hidden")
	}
	set := bson.D{}
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
	return s.repo.upsertOverride(ctx, storeID, skuID, set, actor.PartyID)
}

// deleteStoreSku removes a store's overlay for a SKU (clears a baseline override
// or deletes a store addition).
func (s *service) deleteStoreSku(ctx context.Context, actor auth.Actor, storeID, skuID string) error {
	if err := s.assertStore(ctx, actor, storeID); err != nil {
		return err
	}
	skuID = strings.TrimSpace(skuID)
	if skuID == "" {
		return errBadRequest("a sku id is required")
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
	httpx.JSON(w, http.StatusOK, storeOverrideView{
		SkuID: doc.SkuID, Price: doc.Price, InStock: doc.InStock, Hidden: doc.Hidden,
		UpdatedBy: doc.UpdatedBy, UpdatedAt: doc.UpdatedAt,
	})
}

// deleteSkuHandler — DELETE /consumer/stores/{storeId}/skus/{skuId} (STORE_MANAGER).
func (h *handler) deleteSkuHandler(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	if err := h.svc.deleteStoreSku(r.Context(), actor, chi.URLParam(r, "storeId"), chi.URLParam(r, "skuId")); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

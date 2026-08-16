// Dolibarr ERP → consumer catalog sync + nightly stock-out (docs/dolibarr-sync.md,
// per the Dolibarr Write-API Enablement Brief + Tech Integration Framework I7-I10).
//
// ROLES. Dolibarr is the PRODUCT & PRICE MASTER (63-item PRG-* range) and the
// stock ledger; Saathi is the operational system the shopper sees. This file keeps
// the two in lock-step with ZERO store-manager work:
//
//	INBOUND (catalog):   every DolibarrSyncEvery (+once at boot) pull the sellable
//	  PRG-* products and mirror name/price/description/physical/image into
//	  consumer_catalog — as a price OVERRIDE for the 32 seeded baseline SKUs the
//	  ERP also carries (mapped by the curated table below, so a match can never be
//	  wrong), and as a store ADDITION (base_id = ERP ref) for everything else.
//	  Field ownership is strict: Dolibarr owns name/price/description/physical/
//	  stock; Saathi owns presentation (category defaults, seeded photos, ordering
//	  flags) — a sync can never blank out what the app curates.
//	INBOUND (images):    product photos attached in Dolibarr are served straight
//	  from our API via GET /consumer/catalog/dolimg/<folder>/<file> (cached,
//	  read-only proxy) — additions get their photo_url pointed here automatically.
//	OUTBOUND (stock):    once per day (DolibarrStockOutHourIST, IST) post ONE net
//	  stock-out movement per SKU for YESTERDAY's delivered quantities to warehouse
//	  DolibarrOutWarehouseID — idempotent on movementcode (query-before-post, the
//	  client enforces it), DRY-RUN unless DolibarrPostStockOut=true. Stock-IN /
//	  GRN is booked by a human in the Dolibarr UI, never by this API (one writer
//	  per direction — the brief's hard rule).
//
// SAFETY. Everything here is additive and flag-gated: with DOLIBARR_URL/API_KEY
// unset none of it runs. Writes go through the same repo methods the store
// console uses, are change-detected (no churn), never delete, and only hide
// what this sync itself created.
package consumer

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/dolibarr"
)

const collDolibarrSync = "dolibarr_sync"

// dolibarrUpdatedBy stamps every catalog row this sync writes, so the store
// console (and any audit) can tell ERP-mirrored values from human edits.
const dolibarrUpdatedBy = "dolibarr-sync"

// dolibarrRefToSeedSku is the CURATED map of ERP refs onto the 48-product seeded
// baseline — only pairs verified by hand (same product, same pack size; prices
// cross-checked against products_seed.json). A curated table cannot mis-match;
// anything not listed becomes a store addition instead. Seeded SKUs whose ERP
// pack size differs (e.g. Dahi Sada 80/180/380g vs seeded 90/200/400g) are
// deliberately NOT mapped.
var dolibarrRefToSeedSku = map[string]string{
	"PRG-TONED-500ML":      "taaza-500ml",
	"PRG-TONED-1LTR":       "taaza-1l",
	"PRG-FCM-500ML":        "gold-500ml",
	"PRG-FCM-1LTR":         "gold-1l",
	"PRG-STD-500ML":        "shakti-500ml",
	"PRG-DAHI-M-90GM":      "dahi-sweet-90g",
	"PRG-DAHI-M-200GM":     "dahi-sweet-200g",
	"PRG-DAHI-LF-5KG":      "dahi-plain-5kg",
	"PRG-DAHI-LF-15KG":     "dahi-plain-15kg",
	"PRG-PANEER-1KG":       "paneer-1kg",
	"PRG-PANEER-100GM":     "paneer-vac-100g",
	"PRG-PANEER-200GM":     "paneer-vac-200g",
	"PRG-GHEE-200ML":       "ghee-poly-200ml",
	"PRG-GHEE-500ML":       "ghee-poly-500ml",
	"PRG-GHEE-1LTR":        "ghee-poly-1l",
	"PRG-GHEE-SIKA-500ML":  "ghee-sika-500ml",
	"PRG-GHEE-SIKA-1LTR":   "ghee-sika-1l",
	"PRG-BUTTER-100GM":     "butter-100g",
	"PRG-BUTTER-500GM":     "butter-500g",
	"PRG-FLAVMILK-200ML":   "flavoured-milk-200ml",
	"PRG-KHOYA-500GM":      "khoya-500g",
	"PRG-KHOYA-1KG":        "khoya-1kg",
	"PRG-LADDOO-250GM":     "ladoo-besan-250g",
	"PRG-KHEER-C-100GM":    "kheer-chhena-100g",
	"PRG-KHEER-R-100GM":    "rice-kheer-100ml",
	"PRG-PEDA-250GM":       "peda-250g",
	"PRG-MILKCAKE-250GM":   "milk-cake-250g",
	"PRG-RASGOLLA-200GM":   "rasgolla-200g",
	"PRG-RASGOLLA-500GM":   "rasgolla-500g",
	"PRG-GULABJAMUN-200GM": "gulab-jamun-200g",
	"PRG-GULABJAMUN-500GM": "gulab-jamun-500g",
	"PRG-SHREEKHAND-100GM": "shree-khand-100g",
}

// dolibarrCategoryBySegment maps the <CAT> segment of a PRG-<CAT>-<SIZE> ref to
// the app's seeded category vocabulary (products_seed.json uses exactly these).
var dolibarrCategoryBySegment = map[string]string{
	"TONED": "milk", "FCM": "milk", "STD": "milk",
	"TEA": "super_tea", "FLAVMILK": "flavoured_milk",
	"GHEE": "ghee", "BUTTER": "butter", "DAHI": "dahi",
	"PANEER": "paneer", "KHOYA": "khoya", "LASSI": "lassi",
	"MATTHA": "mattha", "CHHACH": "chaach",
	"PEDA": "sweets", "MILKCAKE": "sweets", "KALAKAND": "sweets",
	"GULABJAMUN": "sweets", "RASGOLLA": "sweets", "LADDOO": "sweets",
	"KHEER": "sweets", "SHREEKHAND": "sweets",
}

// dolibarrRefPattern gates which ERP rows sync at all: only the PRG-* master
// range. Legacy/test rows (Parag_Gold, Full_Cream_Milk_FCM, …) never sync, so
// they can never duplicate a product in the app.
var dolibarrRefPattern = regexp.MustCompile(`^PRG-[A-Z0-9-]+$`)

// dolibarrFallbackSeedSku maps a PRG-<CAT> segment to the seeded product whose
// photos stand in for an ERP addition that has NO image of its own — image
// policy: an ERP-uploaded image always wins; otherwise the card reuses the art
// we already have for that family instead of rendering empty.
var dolibarrFallbackSeedSku = map[string]string{
	"TONED": "taaza-500ml", "FCM": "gold-500ml", "STD": "shakti-500ml",
	"TEA": "chai-special-500ml", "FLAVMILK": "flavoured-milk-200ml",
	"GHEE": "ghee-poly-500ml", "BUTTER": "butter-100g", "DAHI": "dahi-plain-200g",
	"PANEER": "paneer-vac-200g", "KHOYA": "khoya-500g", "LASSI": "lassi-200g",
	"MATTHA": "mattha-200ml", "CHHACH": "chaach-500ml",
	"PEDA": "peda-250g", "MILKCAKE": "milk-cake-250g", "KALAKAND": "milk-cake-250g",
	"GULABJAMUN": "gulab-jamun-200g", "RASGOLLA": "rasgolla-200g",
	"LADDOO": "ladoo-besan-250g", "KHEER": "kheer-chhena-100g", "SHREEKHAND": "shree-khand-100g",
}

// dolibarrVATSuffix extracts the GST %% from a Dolibarr vat code like "C+S-5"
// or "C+S-00" — the instance stores prices ex-GST (price_base_type=HT) with
// tva_tx unset, so the consumer price is price × (1 + rate/100).
var dolibarrVATSuffix = regexp.MustCompile(`-([0-9]+(?:\.[0-9]+)?)$`)

// dolibarrEffectivePrice converts an ERP product's stored price to the consumer
// price. Verified against live data: PRG-PEDA-250GM 119.04762 (C+S-5) → ₹125
// exactly (= the seeded price); milk (C+S-00) passes through unchanged. Prices
// that land within 2 paise of a whole rupee snap to it (float dust from the
// ERP's 8-decimal storage).
func dolibarrEffectivePrice(p dolibarr.Product) float64 {
	return dolibarrWithGST(p, p.PriceTTC.Float())
}

// dolibarrEffectiveMin is the MRP: the ERP's price_min under the same GST rule.
// 0 when the ERP doesn't declare one.
func dolibarrEffectiveMin(p dolibarr.Product) float64 {
	return dolibarrWithGST(p, p.PriceMin.Float())
}

func dolibarrWithGST(p dolibarr.Product, base float64) float64 {
	if base <= 0 {
		return 0
	}
	rate := p.TvaTx.Float()
	if rate == 0 {
		if m := dolibarrVATSuffix.FindStringSubmatch(p.DefaultVATCode); m != nil {
			rate, _ = strconv.ParseFloat(m[1], 64)
		}
	}
	v := base * (1 + rate/100)
	if r := math.Round(v); math.Abs(v-r) < 0.02 {
		return r
	}
	return math.Round(v*100) / 100
}

// dolibarrCategory derives the app category for an ERP-only product.
func dolibarrCategory(ref, label string) string {
	parts := strings.Split(ref, "-")
	if len(parts) >= 2 {
		if c, ok := dolibarrCategoryBySegment[parts[1]]; ok {
			return c
		}
	}
	if strings.Contains(strings.ToLower(label), "milk") {
		return "milk"
	}
	return "sweets"
}

// dolibarrAdditionSku is the deterministic sku_id for an ERP-only product.
func dolibarrAdditionSku(ref string) string { return "dol-" + strings.ToLower(ref) }

// dolibarrSizeSuffix matches a trailing pack size in an ERP label ("… 80 GM",
// "… 1 LTR", "… 500ML", "… 1.5 KG").
var dolibarrSizeSuffix = regexp.MustCompile(`(?i)\s+(\d+(?:\.\d+)?\s*(?:ml|ltr|l|gm|g|kg))\s*$`)

// dolibarrSplitLabel splits an ERP label into the app's (name, variant) pair —
// ERP labels embed the pack size ("Parag Dahi (Sada / Plain) 80 GM") while the
// app's cards group by NAME and select by VARIANT; without the split every size
// renders as a separate look-alike card (the "repeated 3-4 times" bug).
func dolibarrSplitLabel(label string) (name, variant string) {
	label = strings.TrimSpace(label)
	if m := dolibarrSizeSuffix.FindStringSubmatch(label); m != nil {
		return strings.TrimSpace(strings.TrimSuffix(label, m[0])), strings.TrimSpace(m[1])
	}
	return label, ""
}

// dolibarrImagePath builds the app-resolvable photo path for an ERP attachment
// (served by the /catalog/dolimg proxy below; same resolution scheme as
// catalogImagePath — the FE joins it onto its API base).
func dolibarrImagePath(folder, file string) string {
	return "catalog/dolimg/" + url.PathEscape(folder) + "/" + url.PathEscape(file)
}

// dolibarrSyncSchemaV is bumped when the shape the sync WRITES changes (e.g. the
// name/variant split, back covers). A state row with an older v forces one
// re-patch of its catalog row, so schema migrations ship with the code — no
// manual database edits ever.
const dolibarrSyncSchemaV = 4 // v4: ERP on-hand mirrored into inventory stock_count

// dolibarrSyncState is one ref's sync bookkeeping row (collection dolibarr_sync).
type dolibarrSyncState struct {
	Ref          string    `bson:"_id"`
	DolibarrID   int64     `bson:"dolibarr_id"`
	SkuID        string    `bson:"sku_id"`
	Mode         string    `bson:"mode"` // baseline | addition
	Price        float64   `bson:"price"`
	Name         string    `bson:"name"`
	PhotoURL     string    `bson:"photo_url,omitempty"`
	HiddenBySync bool      `bson:"hidden_by_sync,omitempty"`
	Stock        int64     `bson:"stock,omitempty"` // last ERP on-hand mirrored to inventory
	SchemaV      int       `bson:"schema_v,omitempty"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

// ── workers ──────────────────────────────────────────────────────────────────

// dolibarrWorkers is the single entry point wired from module.go — it only runs
// when the integration is configured, and everything inside is best-effort.
func (s *service) dolibarrWorkers(ctx context.Context) {
	cfg := s.deps.Cfg
	cli := dolibarr.New(cfg.DolibarrURL, cfg.DolibarrAPIKey)
	if cli == nil {
		return
	}
	s.log.Info("dolibarr integration ON",
		"store", cfg.DolibarrStoreID, "sync_every", cfg.DolibarrSyncEvery,
		"stockout_hour_ist", cfg.DolibarrStockOutHourIST, "post_stockout", cfg.DolibarrPostStockOut)
	go s.dolibarrCatalogLoop(ctx, cli)
	go s.dolibarrStockOutLoop(ctx, cli)
}

func (s *service) dolibarrCatalogLoop(ctx context.Context, cli *dolibarr.Client) {
	// small boot delay so a crash-looping dependency can't hammer the ERP
	select {
	case <-time.After(15 * time.Second):
	case <-ctx.Done():
		return
	}
	for {
		if err := s.runDolibarrCatalogSync(ctx, cli); err != nil {
			s.log.Warn("dolibarr catalog sync failed", "err", err)
		}
		select {
		case <-time.After(s.deps.Cfg.DolibarrSyncEvery):
		case <-ctx.Done():
			return
		}
	}
}

// runDolibarrCatalogSync executes one full inbound pass. Idempotent; safe to
// run at any frequency (change-detected against dolibarr_sync state).
func (s *service) runDolibarrCatalogSync(ctx context.Context, cli *dolibarr.Client) error {
	cfg := s.deps.Cfg
	products, err := cli.ListSellableProducts(ctx)
	if err != nil {
		return fmt.Errorf("list products: %w", err)
	}

	stateCol := s.deps.DB.Collection(collDolibarrSync)
	seen := map[string]bool{}
	var synced, skipped int
	for _, p := range products {
		ref := strings.ToUpper(strings.TrimSpace(p.Ref))
		if !dolibarrRefPattern.MatchString(ref) {
			skipped++ // legacy/test rows never sync
			continue
		}
		price := dolibarrEffectivePrice(p)
		if price <= 0 || p.Label == "" {
			s.log.Warn("dolibarr product skipped (bad data)", "ref", ref, "price", price)
			continue
		}
		seen[ref] = true

		var st dolibarrSyncState
		_ = stateCol.FindOne(ctx, bson.D{{Key: "_id", Value: ref}}).Decode(&st)

		if seedSku, ok := dolibarrRefToSeedSku[ref]; ok {
			s.syncBaselinePrice(ctx, cli, ref, seedSku, price, p, &st)
		} else {
			s.syncAddition(ctx, cli, ref, price, p, &st)
		}
		synced++
	}

	// ERP rows that vanished or went "Not for sale" (mode=1 pull excludes them):
	// the ERP is the master, so the app follows automatically —
	//   * additions this sync created → HIDDEN (gone from the consumer feed);
	//   * baseline-mapped seeded SKUs → in_stock=false override (auto out-of-
	//     stock; the seeded product itself stays, and comes back in stock the
	//     moment the ERP puts it back on sale — see syncBaselinePrice).
	cur, err := stateCol.Find(ctx, bson.D{})
	if err == nil {
		var all []dolibarrSyncState
		_ = cur.All(ctx, &all)
		for _, st := range all {
			if seen[st.Ref] || st.HiddenBySync {
				continue
			}
			switch st.Mode {
			case "addition":
				hidden := true
				if _, err := s.repo.patchExisting(ctx, cfg.DolibarrStoreID, st.SkuID,
					bson.D{{Key: "hidden", Value: &hidden}}, dolibarrUpdatedBy); err != nil {
					continue
				}
				s.log.Info("dolibarr addition hidden (off-sale/gone in ERP)", "ref", st.Ref, "sku", st.SkuID)
			case "baseline":
				off := false
				if _, err := s.repo.upsertOverride(ctx, cfg.DolibarrStoreID, st.SkuID,
					bson.D{{Key: "in_stock", Value: &off}}, dolibarrUpdatedBy); err != nil {
					continue
				}
				s.log.Info("dolibarr baseline auto out-of-stock (off-sale in ERP)", "ref", st.Ref, "sku", st.SkuID)
			default:
				continue
			}
			_, _ = stateCol.UpdateByID(ctx, st.Ref, bson.D{{Key: "$set", Value: bson.D{
				{Key: "hidden_by_sync", Value: true}, {Key: "updated_at", Value: time.Now().UTC()}}}})
		}
	}
	s.log.Info("dolibarr catalog sync done", "erp_products", len(products), "synced", synced, "legacy_skipped", skipped)
	return nil
}

// syncBaselinePrice mirrors the ERP price onto a seeded baseline SKU as a store
// override — price ONLY. Name/photo/category of seeded products belong to the
// app and are never touched (that is what "must not affect anything" means).
func (s *service) syncBaselinePrice(ctx context.Context, cli *dolibarr.Client, ref, seedSku string, price float64, p dolibarr.Product, st *dolibarrSyncState) {
	stock := p.StockReel.Int()
	if st.Mode == "baseline" && st.Price == price && !st.HiddenBySync &&
		st.Stock == stock && st.SchemaV >= dolibarrSyncSchemaV {
		return // unchanged
	}
	// REAL inventory: the ERP's on-hand (GRN-in via the Dolibarr UI, minus our
	// nightly delivery-outs) IS the store's stock number — mirror it onto the
	// seeded row so the store console and low-stock alerts run on ERP truth.
	// The manager keeps the in/out-of-stock SWITCH; the COUNT is the ledger's.
	s.setSeededStockCount(ctx, seedSku, stock)
	set := bson.D{{Key: "price", Value: &price}}
	if st.HiddenBySync { // ERP put it back ON sale → restore availability
		on := true
		set = append(set, bson.E{Key: "in_stock", Value: &on})
		s.log.Info("dolibarr baseline back in stock (on sale again in ERP)", "ref", ref, "sku", seedSku)
	}
	if _, err := s.repo.upsertOverride(ctx, s.deps.Cfg.DolibarrStoreID, seedSku, set, dolibarrUpdatedBy); err != nil {
		s.log.Warn("dolibarr baseline override failed", "ref", ref, "sku", seedSku, "err", err)
		return
	}
	s.saveDolibarrState(ctx, dolibarrSyncState{
		Ref: ref, DolibarrID: p.ID.Int(), SkuID: seedSku, Mode: "baseline",
		Price: price, Name: p.Label, Stock: stock,
	})
	s.log.Info("dolibarr → baseline", "ref", ref, "sku", seedSku, "price", price, "stock", stock)
}

// syncAddition upserts an ERP-only product as a store addition. On INSERT the
// Saathi-owned defaults are chosen once — category from the ref, subscribable
// false, and IN-STOCK FALSE: during the pilot a new ERP product must appear in
// the app immediately but stay unbuyable until the store manager confirms the
// dark store actually stocks it (one switch in the console). Afterwards the
// sync updates only the Dolibarr-owned fields (name/variant/unit/description/
// price/photos) — never in_stock — and photos only while they still point at
// the dolimg proxy (a store-manager-set photo wins forever).
//
// ERP labels embed the pack size, so they are split into name + variant
// (dolibarrSplitLabel) — that lets the app group sizes of one product onto one
// card instead of rendering a look-alike card per size.
func (s *service) syncAddition(ctx context.Context, cli *dolibarr.Client, ref string, price float64, p dolibarr.Product, st *dolibarrSyncState) {
	cfg := s.deps.Cfg
	sku := dolibarrAdditionSku(ref)
	name, variant := dolibarrSplitLabel(p.Label)
	stock := p.StockReel.Int()
	// Image policy: an ERP-uploaded image ALWAYS wins; a photo already on the
	// row (manager-set or earlier) is respected; only a photo-less card falls
	// back to our own family art so nothing ever renders empty.
	erpFront, erpBack := s.dolibarrPickPhotos(ctx, cli, ref, st)
	front, back := erpFront, erpBack
	if front == "" && s.currentAdditionPhoto(ctx, sku) == "" {
		front, back = s.dolibarrFallbackPhotos(ctx, ref)
	}

	if st.Mode != "addition" { // first sight → create
		subscribable := false
		inStock := false // pilot rule: visible immediately, sellable only after the manager flips it
		doc := &catalogDoc{
			StoreID: cfg.DolibarrStoreID, SkuID: sku, Kind: catalogKindAddition,
			BaseID: ref, Name: name, Variant: variant, Unit: variant,
			Category:    dolibarrCategory(ref, p.Label),
			Description: p.Description, Subscribable: &subscribable,
			Price: &price, InStock: &inStock, PhotoURL: front, BackPhotoURL: back,
			UpdatedBy: dolibarrUpdatedBy,
			UpdatedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		}
		// MRP: only when the ERP actually declares one (price_min); the same
		// GST rule as the selling price applies. FE shows the strikethrough
		// only when mrp is present — absent is fine.
		if mrp := dolibarrEffectiveMin(p); mrp > 0 && mrp >= price {
			doc.MRP = &mrp
		}
		if ml := p.VolumeMl(); ml > 0 {
			doc.Physical = &physicalDoc{VolumeMl: ml}
		}
		sc := int(stock)
		doc.StockCount = &sc
		if err := s.repo.insertAddition(ctx, doc); err != nil {
			// duplicate (row already there from a lost state db) → fall through to patch
			s.log.Debug("dolibarr insertAddition", "ref", ref, "err", err)
		} else {
			s.saveDolibarrState(ctx, dolibarrSyncState{
				Ref: ref, DolibarrID: p.ID.Int(), SkuID: sku, Mode: "addition",
				Price: price, Name: p.Label, PhotoURL: front,
			})
			s.log.Info("dolibarr product added (out of stock until enabled)", "ref", ref, "sku", sku, "price", price, "photo", front != "")
			return
		}
	}

	if st.Mode == "addition" && st.Price == price && st.Name == p.Label &&
		(erpFront == "" || erpFront == st.PhotoURL) && !st.HiddenBySync &&
		st.Stock == stock && st.SchemaV >= dolibarrSyncSchemaV {
		return // unchanged (and already written in the current schema shape)
	}
	sc := int(stock)
	set := bson.D{
		{Key: "name", Value: name},
		{Key: "variant", Value: variant},
		{Key: "unit", Value: variant},
		{Key: "description", Value: p.Description},
		{Key: "price", Value: &price},
		{Key: "stock_count", Value: &sc}, // ERP on-hand = inventory truth
	}
	if mrp := dolibarrEffectiveMin(p); mrp > 0 && mrp >= price {
		set = append(set, bson.E{Key: "mrp", Value: &mrp})
	}
	if front != "" {
		set = append(set, bson.E{Key: "photo_url", Value: front})
	}
	if back != "" {
		set = append(set, bson.E{Key: "back_photo_url", Value: back})
	}
	if st.HiddenBySync { // product is back on sale in the ERP → unhide
		hidden := false
		set = append(set, bson.E{Key: "hidden", Value: &hidden})
	}
	if _, err := s.repo.patchExisting(ctx, cfg.DolibarrStoreID, sku, set, dolibarrUpdatedBy); err != nil {
		s.log.Warn("dolibarr addition patch failed", "ref", ref, "sku", sku, "err", err)
		return
	}
	s.saveDolibarrState(ctx, dolibarrSyncState{
		Ref: ref, DolibarrID: p.ID.Int(), SkuID: sku, Mode: "addition",
		Price: price, Name: p.Label, PhotoURL: erpFront, Stock: stock,
	})
	s.log.Info("dolibarr product updated", "ref", ref, "sku", sku, "price", price, "stock", stock)
}

// currentAdditionPhoto reads the photo the addition row carries right now —
// a manager-set (or previously applied) photo must never be clobbered by a
// family fallback. "" when the row does not exist yet or has no photo.
func (s *service) currentAdditionPhoto(ctx context.Context, sku string) string {
	var row struct {
		PhotoURL string `bson:"photo_url"`
	}
	_ = s.deps.DB.Collection(collCatalog).FindOne(ctx, bson.D{
		{Key: "store_id", Value: s.deps.Cfg.DolibarrStoreID},
		{Key: "sku_id", Value: sku},
	}).Decode(&row)
	return row.PhotoURL
}

// dolibarrFallbackPhotos returns the seeded family art (front, back) for an ERP
// ref with no image of its own; ("","") when the family has no seeded stand-in.
func (s *service) dolibarrFallbackPhotos(ctx context.Context, ref string) (string, string) {
	parts := strings.Split(ref, "-")
	if len(parts) < 2 {
		return "", ""
	}
	seedSku, ok := dolibarrFallbackSeedSku[parts[1]]
	if !ok {
		return "", ""
	}
	var row struct {
		PhotoURL     string `bson:"photo_url"`
		BackPhotoURL string `bson:"back_photo_url"`
	}
	if err := s.deps.DB.Collection(collCatalog).FindOne(ctx, bson.D{
		{Key: "store_id", Value: globalCatalogStore},
		{Key: "sku_id", Value: seedSku},
		{Key: "kind", Value: catalogKindProduct},
	}).Decode(&row); err != nil {
		return "", ""
	}
	return row.PhotoURL, row.BackPhotoURL
}

// dolibarrPickPhotos returns proxy paths for the product's cover images: the
// FIRST image attachment is the front cover, the SECOND the back cover — the
// upload convention for the ERP team. ("","") keeps whatever we knew.
func (s *service) dolibarrPickPhotos(ctx context.Context, cli *dolibarr.Client, ref string, st *dolibarrSyncState) (front, back string) {
	docs, err := cli.ListProductDocuments(ctx, ref)
	if err != nil || len(docs) == 0 {
		return st.PhotoURL, ""
	}
	var imgs []string
	for _, d := range docs {
		if d.IsImage() && d.Level1Name != "" && d.RelativeName != "" {
			imgs = append(imgs, dolibarrImagePath(d.Level1Name, d.RelativeName))
		}
	}
	switch len(imgs) {
	case 0:
		return st.PhotoURL, ""
	case 1:
		return imgs[0], ""
	default:
		return imgs[0], imgs[1]
	}
}

// setSeededStockCount mirrors the ERP on-hand onto a seeded product's
// stock_count (the number the store console's inventory/low-stock run on).
// Count = ledger truth; the manager's in/out-of-stock switch stays untouched.
func (s *service) setSeededStockCount(ctx context.Context, sku string, stock int64) {
	_, err := s.deps.DB.Collection(collCatalog).UpdateOne(ctx,
		bson.D{{Key: "store_id", Value: globalCatalogStore}, {Key: "sku_id", Value: sku}, {Key: "kind", Value: catalogKindProduct}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "stock_count", Value: int(stock)},
			{Key: "updated_by", Value: dolibarrUpdatedBy},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
	if err != nil {
		s.log.Warn("dolibarr seeded stock mirror failed", "sku", sku, "err", err)
	}
}

func (s *service) saveDolibarrState(ctx context.Context, st dolibarrSyncState) {
	st.SchemaV = dolibarrSyncSchemaV
	st.UpdatedAt = time.Now().UTC()
	_, err := s.deps.DB.Collection(collDolibarrSync).UpdateByID(ctx, st.Ref,
		bson.D{{Key: "$set", Value: st}}, options.Update().SetUpsert(true))
	if err != nil {
		s.log.Warn("dolibarr state save failed", "ref", st.Ref, "err", err)
	}
}

// ── outbound: nightly NET stock-out ─────────────────────────────────────────
// (istZone — IST as a fixed offset — is shared from trial.go)

func (s *service) dolibarrStockOutLoop(ctx context.Context, cli *dolibarr.Client) {
	// boot delay: never race the DB/HTTP startup (same guard as the catalog loop)
	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return
	}
	var lastRun string // IST date the job last SUCCEEDED for
	for {
		now := time.Now().In(istZone)
		today := now.Format("2006-01-02")
		if now.Hour() == s.deps.Cfg.DolibarrStockOutHourIST && lastRun != today {
			if err := s.runDolibarrStockOut(ctx, cli); err != nil {
				// failure does NOT mark the day done — the 10-min tick retries
				// within the hour window (the job is idempotent end to end)
				s.log.Warn("dolibarr stock-out failed (will retry)", "err", err)
			} else {
				lastRun = today
			}
		}
		select {
		case <-time.After(10 * time.Minute):
		case <-ctx.Done():
			return
		}
	}
}

// runDolibarrStockOut aggregates YESTERDAY's (IST) delivered order quantities
// per SKU and posts one NET output movement each — the brief's contract: one
// movement per SKU per day, idempotent on movementcode, never per-drop.
func (s *service) runDolibarrStockOut(ctx context.Context, cli *dolibarr.Client) error {
	cfg := s.deps.Cfg
	now := time.Now().In(istZone)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, istZone).AddDate(0, 0, -1)
	dayEnd := dayStart.AddDate(0, 0, 1)
	date := dayStart.Format("2006-01-02")

	// delivered orders, grouped per item product_id (post-adjustment quantities)
	cur, err := s.repo.orders.Aggregate(ctx, []bson.D{
		{{Key: "$match", Value: bson.D{
			{Key: "status", Value: "delivered"},
			{Key: "updated_at", Value: bson.D{{Key: "$gte", Value: dayStart.UTC()}, {Key: "$lt", Value: dayEnd.UTC()}}},
		}}},
		{{Key: "$unwind", Value: "$order_items"}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$order_items.product_id"},
			{Key: "qty", Value: bson.D{{Key: "$sum", Value: "$order_items.qty"}}},
		}}},
	})
	if err != nil {
		return fmt.Errorf("aggregate delivered: %w", err)
	}
	var rows []struct {
		SkuID string `bson:"_id"`
		Qty   int64  `bson:"qty"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		s.log.Info("dolibarr stock-out: nothing delivered", "date", date)
		return nil
	}

	// sku → ERP mapping from the sync state
	stateCol := s.deps.DB.Collection(collDolibarrSync)
	sc, err := stateCol.Find(ctx, bson.D{})
	if err != nil {
		return err
	}
	var states []dolibarrSyncState
	_ = sc.All(ctx, &states)
	bySku := map[string]dolibarrSyncState{}
	for _, st := range states {
		bySku[st.SkuID] = st
	}

	for _, r := range rows {
		st, ok := bySku[r.SkuID]
		if !ok || st.DolibarrID <= 0 || r.Qty <= 0 {
			s.log.Info("dolibarr stock-out: sku not ERP-mapped, skipped", "sku", r.SkuID, "qty", r.Qty)
			continue
		}
		m := dolibarr.StockOut{
			ProductID:   st.DolibarrID,
			WarehouseID: int64(cfg.DolibarrOutWarehouseID),
			Qty:         r.Qty,
			Code:        fmt.Sprintf("SAATHI-STKOUT-%s-%s", date, st.Ref),
			Label:       "Daily net consumer delivery " + date,
			Datem:       dayEnd.Add(-30 * time.Minute).UTC().Format("2006-01-02 15:04:05"),
		}
		if !cfg.DolibarrPostStockOut {
			s.log.Info("dolibarr stock-out DRY-RUN", "code", m.Code, "product_id", m.ProductID, "qty", -m.Qty)
			continue
		}
		posted, err := cli.PostStockOut(ctx, m)
		switch {
		case err != nil:
			s.log.Warn("dolibarr stock-out post failed", "code", m.Code, "err", err)
		case posted:
			s.log.Info("dolibarr stock-out posted", "code", m.Code, "qty", -m.Qty)
		default:
			s.log.Info("dolibarr stock-out already posted, skipped", "code", m.Code)
		}
	}
	return nil
}

// ── image proxy: GET /consumer/catalog/dolimg/{folder}/{file} ────────────────

// dolibarrImgCache is a tiny bounded cache so repeated app loads don't hammer
// the ERP (entries expire after 24h; ~64 images max, evict-oldest).
type dolibarrImgCache struct {
	mu sync.Mutex
	m  map[string]dolibarrImgEntry
}
type dolibarrImgEntry struct {
	data []byte
	mime string
	at   time.Time
}

var dolibarrImages = &dolibarrImgCache{m: map[string]dolibarrImgEntry{}}

func (c *dolibarrImgCache) get(key string) (dolibarrImgEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) > 24*time.Hour {
		return dolibarrImgEntry{}, false
	}
	return e, true
}

func (c *dolibarrImgCache) put(key string, e dolibarrImgEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= 64 { // evict oldest
		var oldK string
		var oldT time.Time
		for k, v := range c.m {
			if oldK == "" || v.at.Before(oldT) {
				oldK, oldT = k, v.at
			}
		}
		delete(c.m, oldK)
	}
	c.m[key] = e
}

// dolibarrImage streams a product photo out of the Dolibarr document store —
// PUBLIC read-only, hard-scoped to modulepart=product paths, no traversal.
// 404 when the integration is off (route exists but serves nothing).
func (h *handler) dolibarrImage(w http.ResponseWriter, r *http.Request) {
	cfg := h.svc.deps.Cfg
	cli := dolibarr.New(cfg.DolibarrURL, cfg.DolibarrAPIKey)
	if cli == nil {
		http.NotFound(w, r)
		return
	}
	// path after /catalog/dolimg/ → "<folder>/<file>"
	rest := r.URL.Path
	if i := strings.Index(rest, "/catalog/dolimg/"); i >= 0 {
		rest = rest[i+len("/catalog/dolimg/"):]
	}
	rest = strings.TrimPrefix(rest, "/")
	folder, file, ok := strings.Cut(rest, "/")
	folder, _ = url.PathUnescape(folder)
	file, _ = url.PathUnescape(file)
	if !ok || folder == "" || file == "" ||
		strings.Contains(folder, "..") || strings.Contains(file, "..") || strings.Contains(file, "/") {
		http.NotFound(w, r)
		return
	}
	key := folder + "/" + file
	e, hit := dolibarrImages.get(key)
	if !hit {
		data, err := cli.DownloadProductDoc(r.Context(), folder, file)
		if err != nil {
			h.svc.log.Warn("dolibarr image fetch failed", "path", key, "err", err)
			http.NotFound(w, r)
			return
		}
		e = dolibarrImgEntry{data: data, mime: dolibarrMime(file), at: time.Now()}
		dolibarrImages.put(key, e)
	}
	w.Header().Set("Content-Type", e.mime)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(e.data)))
	_, _ = w.Write(e.data)
}

func dolibarrMime(file string) string {
	switch strings.ToLower(path.Ext(file)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

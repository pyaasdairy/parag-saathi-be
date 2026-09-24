// Server-side price authority — closes the client-priced-order hole ("₹0 milk").
//
// Until now orders and subscriptions billed the unit price THE CLIENT SENT
// (validated only against < 0), which let a tampered client buy at ₹0-or-any
// price. Money amounts must come from the server's own catalog: the same rows
// the consumer catalog serves (seeded baseline + store overrides + additions,
// later-write-wins across stores — identical semantics to catalogView), which
// is also where the Dolibarr ERP sync lands its authoritative prices.
//
// The index resolves (product_id, variant) → the price the SERVER will charge:
//   - a store override's price beats the seeded price (that is how ERP prices
//     reach seeded SKUs);
//   - an addition prices itself, and a named variant with its own price wins
//     over the base;
//   - a hidden SKU is not purchasable at all;
//   - an unknown product id resolves to nothing — the order is rejected, never
//     guessed.
package consumer

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type catalogPriceIndex struct {
	base     map[string]float64            // sku_id → effective unit price
	names    map[string]string             // sku_id → server-side display name
	variants map[string]map[string]float64 // sku_id → normalized variant key → price
	hidden   map[string]bool               // sku_id → not purchasable
	sizes    map[string]string             // sku_id → pack size as the catalogue writes it ("500ml", "1L")
	// The Founding Family dimension (founding.go). member / memberVariants
	// hold an EXPLICIT level-3 price (the ERP's multiprice level 3, mirrored
	// by the sync into member_price); pyaas marks the PYAAS milk lines and
	// litres their pack size, so a line without an explicit level 3 derives
	// one as level 1 minus memberOffPerLitre per litre. Parag never appears
	// in pyaas, so it is never discounted.
	member         map[string]float64
	memberVariants map[string]map[string]float64
	pyaas          map[string]bool
	litres         map[string]float64
	variantLitres  map[string]map[string]float64
	// memberOffPerLitre is FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE in rupees (2).
	memberOffPerLitre float64
}

// buildPriceIndex folds catalog rows (oldest→newest, so a later write wins on
// cross-store collisions — same rule as catalogView) into a price index. Pure:
// unit-tested without a database.
func buildPriceIndex(docs []catalogDoc) *catalogPriceIndex {
	ix := &catalogPriceIndex{
		base:              map[string]float64{},
		names:             map[string]string{},
		variants:          map[string]map[string]float64{},
		hidden:            map[string]bool{},
		sizes:             map[string]string{},
		member:            map[string]float64{},
		memberVariants:    map[string]map[string]float64{},
		pyaas:             map[string]bool{},
		litres:            map[string]float64{},
		variantLitres:     map[string]map[string]float64{},
		memberOffPerLitre: 2,
	}
	for _, d := range docs {
		switch d.Kind {
		case catalogKindProduct, catalogKindAddition:
			if d.Price != nil {
				ix.base[d.SkuID] = *d.Price
			}
			if v := strings.TrimSpace(d.Variant); v != "" {
				ix.sizes[d.SkuID] = v
			}
			if isPyaasMilkSKU(d.SkuID, d.BaseID, d.Category) {
				ix.pyaas[d.SkuID] = true
				ix.litres[d.SkuID] = packLitres(d)
			}
			if d.MemberPrice != nil && *d.MemberPrice > 0 {
				ix.member[d.SkuID] = *d.MemberPrice
			}
			if d.Name != "" {
				// EXACT catalog name, no variant concat: order-line names are the
				// store inventory's reconciliation key (deriveStock matches rows
				// by name); mutating them would orphan sold/held counts.
				ix.names[d.SkuID] = d.Name
			}
			if d.Hidden != nil {
				ix.hidden[d.SkuID] = *d.Hidden
			}
			for _, v := range d.Variants {
				if v.Price == 0 {
					continue
				}
				m := ix.variants[d.SkuID]
				if m == nil {
					m = map[string]float64{}
					ix.variants[d.SkuID] = m
				}
				if v.VariantID != "" {
					m[variantKey(v.VariantID)] = v.Price
				}
				if v.Label != "" {
					m[variantKey(v.Label)] = v.Price
				}
				for _, key := range []string{v.VariantID, v.Label} {
					if key == "" {
						continue
					}
					if v.MemberPrice != nil && *v.MemberPrice > 0 {
						mv := ix.memberVariants[d.SkuID]
						if mv == nil {
							mv = map[string]float64{}
							ix.memberVariants[d.SkuID] = mv
						}
						mv[variantKey(key)] = *v.MemberPrice
					}
					if l := litresOf(v.VolumeMl, v.Label, v.Unit); l > 0 {
						vl := ix.variantLitres[d.SkuID]
						if vl == nil {
							vl = map[string]float64{}
							ix.variantLitres[d.SkuID] = vl
						}
						vl[variantKey(key)] = l
					}
				}
			}
		case catalogKindOverride:
			if d.Price != nil {
				ix.base[d.SkuID] = *d.Price
			}
			if d.Hidden != nil {
				ix.hidden[d.SkuID] = *d.Hidden
			}
			if d.MemberPrice != nil {
				if *d.MemberPrice > 0 {
					ix.member[d.SkuID] = *d.MemberPrice
				} else {
					delete(ix.member, d.SkuID)
				}
			}
		}
	}
	return ix
}

// packLitres is a catalog row's pack size in litres: the physical envelope
// when the row carries one, else the size parsed from its unit / variant /
// name ("1 L", "500ml Pouch", "450 ML"). A milk row that says nothing is
// taken as one litre.
func packLitres(d catalogDoc) float64 {
	if d.Physical != nil && d.Physical.VolumeMl > 0 {
		return d.Physical.VolumeMl / 1000
	}
	if l := litresOf(0, d.Unit, d.Variant, d.Name); l > 0 {
		return l
	}
	return 1
}

var packSizeRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(ml|ltr|litre|liter|l)\b`)

// litresOf reads a pack size from a volume in ml or the first label that
// names one. 0 when none does.
func litresOf(volumeMl float64, labels ...string) float64 {
	if volumeMl > 0 {
		return volumeMl / 1000
	}
	for _, s := range labels {
		m := packSizeRe.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil || n <= 0 {
			continue
		}
		if strings.EqualFold(m[2], "ml") {
			return n / 1000
		}
		return n
	}
	return 0
}

// memberPriceFor resolves the Founding Family (level 3) unit price: the
// explicit level 3 when the catalog carries one for the sku or variant, else
// level 1 minus the per-litre discount for a PYAAS milk line, else level 1
// (Parag and everything else are never discounted). ok mirrors priceFor.
func (ix *catalogPriceIndex) memberPriceFor(productID, variant string) (price float64, ok bool) {
	base, ok := ix.priceFor(productID, variant)
	if !ok {
		return 0, false
	}
	// Only PYAAS milk has a member price: a member_price stored on any other
	// row (Parag included) is never applied, whoever wrote it.
	if !ix.pyaas[productID] {
		return base, true
	}
	if variant != "" {
		if mv := ix.memberVariants[productID]; mv != nil {
			if p, hit := mv[variantKey(variant)]; hit && p > 0 && p <= base {
				return p, true
			}
		}
	}
	explicitVariant := false
	if variant != "" {
		if m := ix.variants[productID]; m != nil {
			_, explicitVariant = m[variantKey(variant)]
		}
	}
	if p, hit := ix.member[productID]; hit && !explicitVariant && p > 0 && p <= base {
		return p, true
	}
	litres := ix.litres[productID]
	if variant != "" {
		if vl := ix.variantLitres[productID]; vl != nil {
			if l, hit := vl[variantKey(variant)]; hit {
				litres = l
			}
		}
	}
	p := base - memberDiscountFor(litres, ix.memberOffPerLitre)
	if p < 0 {
		p = 0
	}
	return p, true
}

// priceForMember is priceFor with the member dimension: level 3 for an
// active Founding Family member, level 1 for everyone else.
func (ix *catalogPriceIndex) priceForMember(productID, variant string, member bool) (float64, bool) {
	if member {
		return ix.memberPriceFor(productID, variant)
	}
	return ix.priceFor(productID, variant)
}

// isPyaasLine reports whether a sku is a PYAAS milk line (the members-only
// range and the delivery-fee rule key on it).
func (ix *catalogPriceIndex) isPyaasLine(productID string) bool { return ix.pyaas[productID] }

// cheapestPyaasLitre finds the lowest-priced purchasable one-litre PYAAS milk
// line - the savings line's reference when no SKU is configured.
func (ix *catalogPriceIndex) cheapestPyaasLitre() (sku string, price float64, ok bool) {
	for id := range ix.pyaas {
		if ix.hidden[id] || ix.litres[id] != 1 {
			continue
		}
		p, hit := ix.base[id]
		if !hit || p <= 0 {
			continue
		}
		if !ok || p < price || (p == price && id < sku) {
			sku, price, ok = id, p, true
		}
	}
	return sku, price, ok
}

func variantKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// priceFor resolves the authoritative unit price. ok=false → the product is
// unknown or hidden and must not be sold.
func (ix *catalogPriceIndex) priceFor(productID, variant string) (price float64, ok bool) {
	if ix.hidden[productID] {
		return 0, false
	}
	if variant != "" {
		if m := ix.variants[productID]; m != nil {
			if p, hit := m[variantKey(variant)]; hit {
				return p, true
			}
		}
		// a variant label with no own price falls through to the base price
	}
	p, hit := ix.base[productID]
	if !hit || p <= 0 {
		return 0, false
	}
	return p, true
}

// nameFor returns the server-side display name ("" when unknown) so a stored
// order line can never carry a spoofed product name into the store console.
func (ix *catalogPriceIndex) nameFor(productID string) string { return ix.names[productID] }

// variantFor returns the SKU's pack size ("500ml", "1L") for a line the
// server writes itself (a Welcome Litre plan, a promo pack). The size lives
// in variant, never inside the name (the stores match stock by the exact
// name). The catalogue row's own variant wins; a row that carries none (a
// store addition, an older seed) falls back to the bundled seed, which is
// generated from the consumer app's catalogue. "" when neither knows the SKU.
func (ix *catalogPriceIndex) variantFor(productID string) string {
	if v := ix.sizes[productID]; v != "" {
		return v
	}
	return seedVariantFor(productID)
}

// lineVariant is the pack size a stored order or plan line carries: the size
// that priceFor billed, never a label the client merely typed.
//   - A label that is one of the SKU's own priced variants chose the price:
//     kept as sent.
//   - Otherwise the SKU's known size (variantFor) wins over the client's
//     label, except that the same size in another spelling ("500 ML") is kept
//     as sent; an empty label gets the size.
//   - A SKU no catalogue can size keeps the client's label, as before.
func (ix *catalogPriceIndex) lineVariant(productID, clientVariant string) string {
	v := strings.TrimSpace(clientVariant)
	if v != "" {
		if _, priced := ix.variants[productID][variantKey(v)]; priced {
			return clientVariant
		}
	}
	size := ix.variantFor(productID)
	if size == "" {
		return clientVariant
	}
	if v != "" && strings.ReplaceAll(variantKey(v), " ", "") == strings.ReplaceAll(variantKey(size), " ", "") {
		return clientVariant
	}
	return size
}

var (
	seedVariantsOnce sync.Once
	seedVariants     map[string]string
)

// seedVariantFor reads the pack size of a seeded SKU from products_seed.json
// (parsed once; a malformed embed yields no sizes, never a panic here).
func seedVariantFor(productID string) string {
	seedVariantsOnce.Do(func() {
		seedVariants = map[string]string{}
		var rows []seedProduct
		if json.Unmarshal(embeddedProductsSeed, &rows) != nil {
			return
		}
		for _, p := range rows {
			if v := strings.TrimSpace(p.Variant); v != "" {
				seedVariants[p.ID] = v
			}
		}
	})
	return seedVariants[productID]
}

// ensureSubscriptionVariant fills a plan's pack size from the catalogue when
// the stored document has none: every Welcome Litre plan created before the
// plan recorded its size, and any plan an older app build sent without one.
// Without it the morning order, the store row and the rider row all show
// "1 x Full Cream Milk" with no volume. The size is written back once, so
// later mornings read it from the plan. Best-effort: an unknown SKU or a
// failed read leaves the plan as it was.
func (s *service) ensureSubscriptionVariant(ctx context.Context, sub *subscription) {
	if sub == nil || strings.TrimSpace(sub.Variant) != "" {
		return
	}
	ix, err := s.loadPriceIndex(ctx)
	if err != nil {
		return
	}
	v := ix.variantFor(sub.ProductID)
	if v == "" {
		return
	}
	sub.Variant = v
	_, _ = s.repo.subscriptions.UpdateOne(ctx,
		bson.D{
			{Key: "subscription_id", Value: sub.SubscriptionID},
			{Key: "variant", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "variant", Value: v}}}})
}

// loadPriceIndex reads the full catalog (109 rows today — one small query per
// order/subscription creation) and builds the index.
func (s *service) loadPriceIndex(ctx context.Context) (*catalogPriceIndex, error) {
	cur, err := s.repo.catalog.Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "updated_at", Value: 1}}))
	if err != nil {
		return nil, errInternal("catalog price lookup failed")
	}
	var docs []catalogDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, errInternal("catalog price decode failed")
	}
	ix := buildPriceIndex(docs)
	ix.memberOffPerLitre = s.deps.Cfg.FoundingLevel3OffPerLitre()
	return ix, nil
}

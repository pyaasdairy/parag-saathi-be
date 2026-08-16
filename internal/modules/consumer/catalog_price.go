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
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type catalogPriceIndex struct {
	base     map[string]float64            // sku_id → effective unit price
	names    map[string]string             // sku_id → server-side display name
	variants map[string]map[string]float64 // sku_id → normalized variant key → price
	hidden   map[string]bool               // sku_id → not purchasable
}

// buildPriceIndex folds catalog rows (oldest→newest, so a later write wins on
// cross-store collisions — same rule as catalogView) into a price index. Pure:
// unit-tested without a database.
func buildPriceIndex(docs []catalogDoc) *catalogPriceIndex {
	ix := &catalogPriceIndex{
		base:     map[string]float64{},
		names:    map[string]string{},
		variants: map[string]map[string]float64{},
		hidden:   map[string]bool{},
	}
	for _, d := range docs {
		switch d.Kind {
		case catalogKindProduct, catalogKindAddition:
			if d.Price != nil {
				ix.base[d.SkuID] = *d.Price
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
			}
		case catalogKindOverride:
			if d.Price != nil {
				ix.base[d.SkuID] = *d.Price
			}
			if d.Hidden != nil {
				ix.hidden[d.SkuID] = *d.Hidden
			}
		}
	}
	return ix
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
	return buildPriceIndex(docs), nil
}

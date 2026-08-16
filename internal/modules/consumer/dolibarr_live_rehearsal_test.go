package consumer

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/pyaas/saathi-backend/internal/platform/dolibarr"
)

// TestDolibarrLiveRehearsal is a READ-ONLY dry run of the inbound sync against
// the REAL ERP: it pulls the live product list and prints exactly what the sync
// would do — using the shipped mapping, gate and price rule — without writing to
// Mongo or Dolibarr. Skipped unless DOLIBARR_URL + DOLIBARR_API_KEY are set, so
// CI and normal test runs never touch the network.
//
//	DOLIBARR_URL=… DOLIBARR_API_KEY=… go test ./internal/modules/consumer/ \
//	  -run TestDolibarrLiveRehearsal -v -count=1
func TestDolibarrLiveRehearsal(t *testing.T) {
	url, key := os.Getenv("DOLIBARR_URL"), os.Getenv("DOLIBARR_API_KEY")
	if url == "" || key == "" {
		t.Skip("set DOLIBARR_URL + DOLIBARR_API_KEY for the live read-only rehearsal")
	}
	cli := dolibarr.New(url, key)
	ctx := context.Background()

	prods, err := cli.ListSellableProducts(ctx)
	if err != nil {
		t.Fatalf("live ERP list: %v", err)
	}
	sort.Slice(prods, func(i, j int) bool { return prods[i].Ref < prods[j].Ref })

	var overrides, additions, skipped, badPrice, withImg int
	for _, p := range prods {
		ref := strings.ToUpper(strings.TrimSpace(p.Ref))
		if !dolibarrRefPattern.MatchString(ref) {
			skipped++
			t.Logf("SKIP legacy       %-24s %q", p.Ref, p.Label)
			continue
		}
		price := dolibarrEffectivePrice(p)
		if price <= 0 {
			badPrice++
			t.Logf("SKIP bad price    %-24s %q", ref, p.Label)
			continue
		}
		img := ""
		if docs, err := cli.ListProductDocuments(ctx, ref); err == nil {
			for _, d := range docs {
				if d.IsImage() {
					img = dolibarrImagePath(d.Level1Name, d.RelativeName)
					withImg++
					break
				}
			}
		}
		if sku, ok := dolibarrRefToSeedSku[ref]; ok {
			overrides++
			t.Logf("OVERRIDE %-22s → %-22s ₹%-9.2f", ref, sku, price)
		} else {
			additions++
			t.Logf("ADDITION %-22s → %-22s ₹%-9.2f cat=%-14s img=%v",
				ref, dolibarrAdditionSku(ref), price, dolibarrCategory(ref, p.Label), img != "")
		}
	}
	t.Logf("PLAN: %d ERP products → %d seeded price-overrides, %d additions, %d legacy skipped, %d bad-price, %d with images",
		len(prods), overrides, additions, skipped, badPrice, withImg)
	if overrides == 0 && additions == 0 {
		t.Fatal("live rehearsal produced an empty plan — investigate before enabling the sync")
	}
}

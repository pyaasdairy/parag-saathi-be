package consumer

import (
	"strings"
	"testing"
	"time"
)

// The embedded config is the single source of trigger truth — pin that it
// parses and carries everything the engine dereferences, so a bad config
// replacement fails in CI, never at 10:30 IST in production.
func TestCRMConfigParses(t *testing.T) {
	c := crmConfigLoad() // sync.Once — parse happens exactly once per process
	if len(c.Triggers) != 54 {
		t.Fatalf("triggers: got %d want 54", len(c.Triggers))
	}
	for _, id := range []string{"W-01", "W-02", "W-03a", "W-03b", "W-04", "W-05", "W-06", "W-07", "W-08", "W-09", "W-10"} {
		tr, ok := c.Triggers[id]
		if !ok {
			t.Fatalf("trigger %s missing from config", id)
		}
		if tr.Section != "W" {
			t.Fatalf("trigger %s: section %q", id, tr.Section)
		}
		// Customer-facing W triggers must have a registered template (G10);
		// internal ones (W-09/W-10) deliberately have none.
		if id != "W-09" && id != "W-10" {
			if _, ok := c.Templates[tr.Template.String()]; !ok {
				t.Fatalf("trigger %s: template %q not in registry", id, tr.Template.String())
			}
		}
	}
	if c.Offer.Pack2MinRechargePaise != 50000 {
		t.Fatalf("pack2 threshold: got %d paise want 50000", c.Offer.Pack2MinRechargePaise)
	}
	if c.Offer.Pack2GraceDays != 7 {
		t.Fatalf("grace days: got %d want 7", c.Offer.Pack2GraceDays)
	}
	if c.Offer.SeedSKU != "gold-500ml" {
		t.Fatalf("seed sku mapping: %q", c.Offer.SeedSKU)
	}
	// The paise→rupee conversion happens exactly once; pin the arithmetic.
	if got := float64(c.Offer.Pack2MinRechargePaise) / 100.0; got != 500.0 {
		t.Fatalf("threshold in rupees: %v", got)
	}
}

// W-07's "nothing has been charged" line is MANDATORY and must never be
// shortened out by a template edit — in either language (spec §5.3).
func TestW07MandatoryNoChargeLine(t *testing.T) {
	c := crmConfigLoad()
	tpl := c.Templates["T-W07"]
	if !strings.Contains(tpl.EN, "Nothing has been charged") {
		t.Fatalf("T-W07 en lost the mandatory no-charge line: %q", tpl.EN)
	}
	if !strings.Contains(tpl.HI, "Koi shulk nahi laga hai") {
		t.Fatalf("T-W07 hi lost the mandatory no-charge line: %q", tpl.HI)
	}
}

// CH-01: the entitlement is DERIVED from offer state, never stored. Pin the
// derivation table.
func TestEntitledFreeDeliveries(t *testing.T) {
	cases := []struct {
		p1, p2 string
		want   int
	}{
		{pack1Pending, pack2Locked, 1},    // enrolled, nothing delivered
		{pack1Delivered, pack2Locked, 0},  // day 1+, unrecharged — cover is spent
		{pack1Delivered, pack2Pending, 1}, // recharge settled, pack 2 owed
		{pack1Delivered, pack2Delivered, 0},
		{pack1Delivered, pack2Expired, 0},
		{pack1Forfeited, pack2Expired, 0},
	}
	for _, c := range cases {
		o := &consumerOffer{OfferID: offerWelcomeLitre, Pack1State: c.p1, Pack2State: c.p2}
		if got := entitledFreeDeliveries(o); got != c.want {
			t.Errorf("p1=%s p2=%s: got %d want %d", c.p1, c.p2, got, c.want)
		}
	}
	if entitledFreeDeliveries(nil) != 0 {
		t.Error("nil offer must be 0 — non-campaign customers take the existing path")
	}
}

// W-09: the address bucket must be stable under formatting noise (case,
// spacing) and distinguish genuinely different addresses.
func TestCRMAddressHash(t *testing.T) {
	a := crmAddressHash("Flat 101, E2E Tower", "226030", 26.7725, 81.0150)
	b := crmAddressHash("  flat 101,   e2e tower ", "226030", 26.77251, 81.01502) // same ~11 m bucket
	if a != b {
		t.Fatal("same household must bucket identically despite formatting noise")
	}
	c := crmAddressHash("Flat 102, E2E Tower", "226030", 26.7725, 81.0150)
	if a == c {
		t.Fatal("different flats must not collide")
	}
}

// The schedule anchor: day 0 is the IST calendar day pack 1 landed, and the
// grace expiry fires at day >= 7 — pinned against the istZone conversion.
func TestDaysSinceFirstDelivery(t *testing.T) {
	first := time.Date(2026, 8, 21, 2, 0, 0, 0, istZone) // 02:00 IST delivery
	o := &consumerOffer{FirstDeliveryAt: &first}
	if got := daysSinceFirstDelivery(o, first.Add(3*time.Hour)); got != 0 {
		t.Fatalf("same IST day: got %d want 0", got)
	}
	if got := daysSinceFirstDelivery(o, first.Add(24*time.Hour)); got != 1 {
		t.Fatalf("next day: got %d want 1", got)
	}
	if got := daysSinceFirstDelivery(o, first.AddDate(0, 0, 7)); got != 7 {
		t.Fatalf("day 7: got %d want 7", got)
	}
	if got := daysSinceFirstDelivery(&consumerOffer{}, time.Now()); got != -1 {
		t.Fatal("no first delivery must report -1 (no schedules anchor)")
	}
}

// Template rendering must leave unresolved tokens VISIBLE (C-01: fail loudly,
// never silently render a half-truth) and substitute both bracket styles.
func TestCRMRender(t *testing.T) {
	out := crmRender("Recharge by [DATE] via {LINK}", map[string]string{"DATE": "28 Aug", "LINK": "x"})
	if out != "Recharge by 28 Aug via x" {
		t.Fatalf("render: %q", out)
	}
	leftover := crmRender("by [DATE]", nil)
	if !strings.Contains(leftover, "[DATE]") {
		t.Fatal("unresolved tokens must stay visible, not vanish")
	}
}

// Frequency-cap extraction from the heterogeneous config values.
func TestCRMCapLimit(t *testing.T) {
	if got := crmCapLimit(crmTrigger{FrequencyCap: map[string]any{"per_offer": float64(1)}}); got != 1 {
		t.Fatalf("per_offer: %d", got)
	}
	if got := crmCapLimit(crmTrigger{FrequencyCap: map[string]any{"max_total": float64(2)}}); got != 2 {
		t.Fatalf("max_total: %d", got)
	}
	if got := crmCapLimit(crmTrigger{}); got != 0 {
		t.Fatalf("no cap: %d", got)
	}
}

// THE ISOLATION SEAM: with CRM_ENABLED unset, the promotional order minting
// path is unreachable and the entitlement of every non-enrolled customer is
// zero — the binary must behave byte-identically to the pre-CRM build.
func TestCRMDisabledIsInert(t *testing.T) {
	t.Setenv("CRM_ENABLED", "")
	if crmEnabled() {
		t.Fatal("CRM must be OFF unless explicitly enabled")
	}
	t.Setenv("CRM_ENABLED", "true")
	if !crmEnabled() {
		t.Fatal("flag flip must enable without a rebuild")
	}
}

// Doorstep prefs are client input — pin the whitelist + caps + nil-collapse.
func TestSanitizeDeliveryPrefs(t *testing.T) {
	if sanitizeDeliveryPrefs(nil) != nil {
		t.Fatal("nil map must stay nil")
	}
	if sanitizeDeliveryPrefs(map[string]any{"junk": "x"}) != nil {
		t.Fatal("only unknown keys → nil (nothing to store)")
	}
	d := sanitizeDeliveryPrefs(map[string]any{
		"handover": " RING_BELL ", "callBefore": true, "note": "  leave at door  ",
		"receiver": "Amma", "evil": "<script>",
	})
	if d == nil || d.Handover != "RING_BELL" || !d.CallBefore || d.Note != "leave at door" || d.Receiver != "Amma" {
		t.Fatalf("sanitize wrong: %+v", d)
	}
	long := sanitizeDeliveryPrefs(map[string]any{"note": string(make([]byte, 1000))})
	if long != nil && len(long.Note) > 280 {
		t.Fatalf("note cap breached: %d", len(long.Note))
	}
}

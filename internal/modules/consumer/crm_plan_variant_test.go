package consumer

// F17: a Welcome Litre plan created server-side carried no pack size, so
// every morning order, store row and rider row it produced showed
// "1 x Full Cream Milk - Parag Gold" with no volume. The size lives in the
// catalogue (products_seed.json "variant"); the plan takes it at create, and
// a plan stored without one (legacy documents) gets it at order time.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMPlanVariant -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// crmTaskItemsForSub returns the delivery task items of the order the sweep
// placed for one subscription on one delivery day.
func crmTaskItemsForSub(t *testing.T, w *chainWorld, subID, day string) []deliveryItem {
	t.Helper()
	ctx := context.Background()
	var o order
	if err := w.db.Collection(collOrders).FindOne(ctx, bson.D{
		{Key: "subscription_id", Value: subID}, {Key: "scheduled_for", Value: day},
	}).Decode(&o); err != nil {
		t.Fatalf("subscription order for %s on %s: %v", subID, day, err)
	}
	var d delivery
	if err := w.db.Collection(collDeliveries).FindOne(ctx, bson.D{{Key: "order_id", Value: o.OrderID}}).Decode(&d); err != nil {
		t.Fatalf("task for %s: %v", o.OrderID, err)
	}
	if len(o.Items) != 1 || len(d.Items) != 1 {
		t.Fatalf("order / task lines: %+v / %+v", o.Items, d.Items)
	}
	if o.Items[0].Variant != d.Items[0].Variant {
		t.Fatalf("order line %q and task line %q disagree", o.Items[0].Variant, d.Items[0].Variant)
	}
	return d.Items
}

func TestCRMPlanVariantWelcomeLitreCarriesPackSize(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	enrol := func(phone, line, product string, qty int) (primitive.ObjectID, string) {
		t.Helper()
		res, err := w.svc.crmEnrol(ctx, "variant-test-operator", crmEnrolInput{
			Phone: phone, Name: "Variant Household", Line1: line, Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
			PlanProductID: product, PlanQty: qty, PlanFrequency: "daily",
		})
		if err != nil {
			t.Fatalf("crmEnrol %s: %v", product, err)
		}
		cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
		if _, err := w.svc.creditTopup(ctx, cid, 500, "razorpay", "fund-"+phone); err != nil {
			t.Fatalf("fund: %v", err)
		}
		// The noon lock reads the wallet as it stood at 12:00 (walletAsOf):
		// the top-up is dated long before, whatever the wall clock says.
		chainStampLedger(t, w, cid, "fund-"+phone, "TOPUP", chainLedgerEpoch)
		return cid, res.SubscriptionID
	}
	_, litre := enrol("9000008401", "Flat 11, Size Tower", "gold-1l", 1)
	_, half := enrol("9000008402", "Flat 12, Size Tower", "gold-500ml", 2)

	for sub, want := range map[string]string{litre: "1L", half: "500ml"} {
		var s subscription
		if err := w.db.Collection(collSubscriptions).FindOne(ctx, bson.D{{Key: "subscription_id", Value: sub}}).Decode(&s); err != nil {
			t.Fatalf("subscription %s: %v", sub, err)
		}
		if s.Variant != want {
			t.Fatalf("plan %s (%s) created with variant %q, want %q", sub, s.ProductID, s.Variant, want)
		}
	}

	// A plan written before this fix: no variant on the document.
	legacyCID := w.customer(t, "9000008403", 500)
	now := time.Now().UTC()
	legacy := &subscription{
		MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(), ConsumerID: legacyCID,
		ProductID: "gold-1l", Name: "Milk gold-1l", Qty: 1, UnitPrice: 69, Frequency: "daily",
		Status: "active", StartDate: istDay(now.Add(24 * time.Hour)), CreatedAt: now, UpdatedAt: now,
	}
	if _, err := w.db.Collection(collSubscriptions).InsertOne(ctx, legacy); err != nil {
		t.Fatalf("legacy subscription: %v", err)
	}

	// The plans' first morning under the noon lock: the 11:00 tick the day
	// before previews it, the 12:15 tick locks it and mints the store task.
	// The plans are dated before that cut-off, so the test reads the same
	// whatever the wall clock says (created "now" after noon, they would
	// start the day after tomorrow and no task would exist for start).
	start := istDay(now.Add(24 * time.Hour))
	cutoff := lockMomentFor(start)
	for _, id := range []string{litre, half, legacy.SubscriptionID} {
		chainBackdateSubscription(t, w, &subscription{SubscriptionID: id}, cutoff.Add(-2*time.Hour))
	}
	w.svc.sweepSubscriptionOrders(ctx, cutoff.Add(-time.Hour))
	w.svc.sweepSubscriptionOrders(ctx, cutoff.Add(15*time.Minute))

	for sub, want := range map[string]string{litre: "1L", half: "500ml", legacy.SubscriptionID: "1L"} {
		if got := crmTaskItemsForSub(t, w, sub, start)[0].Variant; got != want {
			t.Fatalf("task line for %s carries variant %q, want %q", sub, got, want)
		}
	}
	// The legacy plan keeps the size it was given, so later mornings do not
	// derive it again.
	var s subscription
	if err := w.db.Collection(collSubscriptions).FindOne(ctx, bson.D{{Key: "subscription_id", Value: legacy.SubscriptionID}}).Decode(&s); err != nil || s.Variant != "1L" {
		t.Fatalf("legacy plan variant after the sweep: %q %v", s.Variant, err)
	}
}

// variantFor: the catalogue row's own variant wins; a row without one falls
// back to the bundled seed; an SKU nobody knows has no size.
func TestCRMPlanVariantForPrefersTheCatalogueRow(t *testing.T) {
	p := 45.0
	ix := buildPriceIndex([]catalogDoc{
		{SkuID: "paneer-200g", Kind: catalogKindAddition, Price: &p, Name: "Paneer", Variant: " 200g "},
		{SkuID: "gold-1l", Kind: catalogKindProduct, Price: &p, Name: "Full Cream Milk - Parag Gold"},
	})
	for sku, want := range map[string]string{"paneer-200g": "200g", "gold-1l": "1L", "taaza-500ml": "500ml", "no-such-sku": ""} {
		if got := ix.variantFor(sku); got != want {
			t.Errorf("variantFor(%s) = %q, want %q", sku, got, want)
		}
	}
}

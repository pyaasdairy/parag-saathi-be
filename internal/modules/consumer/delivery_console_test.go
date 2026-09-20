package consumer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The store manager's and rider's consoles read one query. Two things had gone
// wrong with it in production and both are pinned here.
//
//  1. It returned the OLDEST 500 tasks. A pilot store crossed 500 lifetime
//     tasks on 9 Aug 2026 and from that moment every new order was missing
//     from the queue — 3,432 of them — while the screen said there was no work.
//  2. Task lines carried only {name, qty}, so "Parag Gold x2" could be two
//     half-litres or two litres and neither the packer nor the rider could tell.
func TestStoreConsoleShowsNewOrdersPast500(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	repo := w.svc.repo
	ctx := context.Background()
	store := primitive.NewObjectID().Hex()
	now := time.Now().UTC()

	// 600 delivered tasks from months ago: the history that used to fill the
	// whole window.
	old := make([]any, 0, 600)
	for i := 0; i < 600; i++ {
		at := now.AddDate(0, 0, -90).Add(time.Duration(i) * time.Minute)
		old = append(old, bson.D{
			{Key: "delivery_id", Value: fmt.Sprintf("old-%03d", i)},
			{Key: "order_id", Value: fmt.Sprintf("ord-old-%03d", i)},
			{Key: "store_id", Value: store},
			{Key: "status", Value: "DELIVERED"},
			{Key: "assigned_at", Value: at.Format(time.RFC3339)},
			{Key: "created_at", Value: at},
			{Key: "updated_at", Value: at},
		})
	}
	if _, err := repo.deliveries.InsertMany(ctx, old); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	// This morning's order, placed after the store passed 500.
	fresh := bson.D{
		{Key: "delivery_id", Value: "fresh-1"},
		{Key: "order_id", Value: "ord-fresh-1"},
		{Key: "store_id", Value: store},
		{Key: "status", Value: "ASSIGNED"},
		{Key: "assigned_at", Value: now.Format(time.RFC3339)},
		{Key: "created_at", Value: now},
		{Key: "updated_at", Value: now},
		{Key: "items", Value: []deliveryItem{{ProductID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Qty: 2}}},
	}
	if _, err := repo.deliveries.InsertOne(ctx, fresh); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	list, err := repo.listDeliveriesByStore(ctx, store)
	if err != nil {
		t.Fatalf("listDeliveriesByStore: %v", err)
	}

	var found *delivery
	for i := range list {
		if list[i].ID == "fresh-1" {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("the new order is missing from the store console: %d rows returned, none of them today's", len(list))
	}
	if list[0].ID != "fresh-1" {
		t.Errorf("newest task should sort first, got %q", list[0].ID)
	}
	// Old DELIVERED work is trimmed; open work never is.
	for _, d := range list {
		if d.Status == "DELIVERED" && d.UpdatedAt.Before(now.AddDate(0, 0, -deliveryHistoryDays)) {
			t.Errorf("stale delivered task %q should not be carried in the console list", d.ID)
			break
		}
	}
	if len(found.Items) != 1 || found.Items[0].Variant != "500ml" {
		t.Errorf("pack size missing from the task line: %+v", found.Items)
	}
}

// An open task is NEVER aged out, however old it is: somebody still has to act
// on it, so it cannot fall off the queue.
func TestOpenTasksNeverAgeOut(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	repo := w.svc.repo
	ctx := context.Background()
	store := primitive.NewObjectID().Hex()
	ancient := time.Now().UTC().AddDate(0, 0, -400)

	if _, err := repo.deliveries.InsertOne(ctx, bson.D{
		{Key: "delivery_id", Value: "stuck-1"},
		{Key: "order_id", Value: "ord-stuck-1"},
		{Key: "store_id", Value: store},
		{Key: "status", Value: "OFFERED"},
		{Key: "assigned_at", Value: ancient.Format(time.RFC3339)},
		{Key: "created_at", Value: ancient},
		{Key: "updated_at", Value: ancient},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	list, err := repo.listDeliveriesByStore(ctx, store)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "stuck-1" {
		t.Fatalf("an unclaimed task must stay visible however old it is, got %d rows", len(list))
	}
}

// The pack size reaches the task by BOTH writing paths — in its own field.
//
// The name must stay EXACTLY as the catalog wrote it: the Saathi store screen
// reconciles stock by exact name match (models/store.dart), so decorating the
// name with "(500ml)" would silently stop "held" / "delivered" / "sold today"
// from counting against any product row.
func TestDeliveryItemsKeepThePackSize(t *testing.T) {
	items := deliveryItemsFor([]orderItem{
		{ProductID: "gold-500ml", Name: "Full Cream Milk - Parag Gold", Variant: "500ml", Qty: 2},
		{ProductID: "taaza-1l", Name: "Toned Milk - Parag Taaza", Variant: "1L", Qty: 1},
	})
	if items[0].Name != "Full Cream Milk - Parag Gold" || items[1].Name != "Toned Milk - Parag Taaza" {
		t.Errorf("the wire name must stay verbatim, got %q / %q", items[0].Name, items[1].Name)
	}
	if items[0].Variant != "500ml" || items[0].ProductID != "gold-500ml" {
		t.Errorf("variant/productId must ride along: %+v", items[0])
	}
	if items[1].Variant != "1L" {
		t.Errorf("variant missing on line 2: %+v", items[1])
	}
	// Label() is the display form used server-side, and never repeats a size the
	// name already carries.
	if got := items[0].Label(); got != "Full Cream Milk - Parag Gold (500ml)" {
		t.Errorf("Label(): got %q", got)
	}
	already := deliveryItem{Name: "Paneer 200g", Variant: "200g"}
	if got := already.Label(); got != "Paneer 200g" {
		t.Errorf("Label() should not repeat a size the name already states: %q", got)
	}
}

// The bell rules, which are subtle enough to be worth pinning.
//
//   - the ACCOUNT's standing prefs hard-code ring_bell:false and post it on
//     every save, so a false THERE says nothing and is dropped;
//   - the ADDRESS capture shows a visibly preselected "Hang it outside", so a
//     false there is a real instruction and must reach the rider;
//   - "do not ring" next to an explicit HAND_TO_CUSTOMER is what the cart
//     produces from an untouched switch, so the rider's step text must not
//     print a contradiction — but with any other mode it must.
func TestBellInstructionRules(t *testing.T) {
	no, yes := false, true

	if got := bellSaid(&deliveryPrefsDoc{RingBell: &no}); got.RingBell != nil {
		t.Error("the account's default ring_bell:false is not an instruction")
	}
	if got := bellSaid(&deliveryPrefsDoc{RingBell: &yes}); got.RingBell == nil || !*got.RingBell {
		t.Error("ring_bell:true is a real instruction and must survive")
	}

	// Contradiction suppressed.
	if line := riderHandoverSubtitle(&deliveryPrefsDoc{Handover: "HAND_TO_CUSTOMER", RingBell: &no}); line != "Hand the order to the customer in person" {
		t.Errorf("an untouched bell must not argue with the handover mode: %q", line)
	}
	// A real "leave it, don't ring" still reaches the rider.
	if line := riderHandoverSubtitle(&deliveryPrefsDoc{Handover: "DROP", RingBell: &no}); !strings.Contains(line, "do NOT ring") {
		t.Errorf("a deliberate do-not-ring was dropped: %q", line)
	}
	if line := riderHandoverSubtitle(&deliveryPrefsDoc{RingBell: &no}); !strings.Contains(line, "Do NOT ring") {
		t.Errorf("a bare do-not-ring was dropped: %q", line)
	}
	// And a positive bell is always carried.
	if line := riderHandoverSubtitle(&deliveryPrefsDoc{Handover: "HAND_TO_CUSTOMER", RingBell: &yes}); !strings.Contains(line, "ring the bell") {
		t.Errorf("ring-the-bell was dropped: %q", line)
	}
}

// The customer's own words reach the rider whichever key the client used.
func TestDoorstepNoteSurvivesEveryFieldName(t *testing.T) {
	for _, key := range []string{"note", "notes", "instructions"} {
		p := sanitizeDeliveryPrefs(map[string]any{key: "Leave with the guard, flat 805"})
		if p == nil || p.Note != "Leave with the guard, flat 805" {
			t.Errorf("%q: the doorstep note was dropped (%+v)", key, p)
		}
	}
}

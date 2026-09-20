package consumer

import (
	"context"
	"testing"
)

// The consumer app files a complaint offline, shows the member a reference, and
// retries the SAME filing until it syncs. Two rules follow: the retry must not
// mint a second ticket, and the member must be able to read their own list back
// (before this existed the app posted into a 404 forever).
func TestComplaintFilingIsIdempotentPerMember(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000003001", 0)

	in := complaintInput{
		Ref: "PYS-4F21A", Category: "missing", OrderID: "ord_x",
		Detail: "Only one packet arrived, the order said two",
	}
	first, err := w.svc.fileComplaint(ctx, cid, in)
	if err != nil {
		t.Fatalf("fileComplaint: %v", err)
	}
	if first.Status != complaintOpen || first.Ref != "PYS-4F21A" {
		t.Fatalf("unexpected first filing: %+v", first)
	}

	// The offline queue retries it.
	again, err := w.svc.fileComplaint(ctx, cid, in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("a retried filing became a second ticket: %q then %q", first.ID, again.ID)
	}

	list, err := w.svc.listComplaints(ctx, cid)
	if err != nil {
		t.Fatalf("listComplaints: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("member should see exactly one complaint, saw %d", len(list))
	}

	// A different member may hold the same short code without being refused.
	other := w.customer(t, "9000003002", 0)
	if _, err := w.svc.fileComplaint(ctx, other, in); err != nil {
		t.Errorf("another member's identical ref was refused: %v", err)
	}

	// An unknown category is filed as "other" — never refused, because the
	// complaint matters more than its label.
	odd, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-ZZZ", Category: "nonsense", Detail: "x"})
	if err != nil || odd.Category != "other" {
		t.Errorf("odd category should file as other, got %+v (%v)", odd, err)
	}
	// An empty complaint is refused: there is nothing for an operator to answer.
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-EMPTY", Detail: "   "}); err == nil {
		t.Error("an empty complaint should be refused")
	}
}

// A push token belongs to the phone, not the account: when a handset changes
// hands the newest owner must win, or one member's order news lands on another
// member's screen.
func TestPushTokenFollowsTheNewestOwner(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	first := w.customer(t, "9000003003", 0)
	second := w.customer(t, "9000003004", 0)

	in := pushRegisterInput{Token: "ExponentPushToken[abc123]", Platform: "android", Provider: "expo"}
	if err := w.svc.registerPushDevice(ctx, first, in); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := w.svc.registerPushDevice(ctx, second, in); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	n, err := w.svc.repo.pushDevices().CountDocuments(ctx, map[string]any{"token": in.Token})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("one token, one row — got %d", n)
	}
	var row pushDevice
	if err := w.svc.repo.pushDevices().FindOne(ctx, map[string]any{"token": in.Token}).Decode(&row); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if row.ConsumerID != second {
		t.Error("the token should now belong to the member who registered it last")
	}

	if err := w.svc.registerPushDevice(ctx, second, pushRegisterInput{Token: "  "}); err == nil {
		t.Error("an empty token should be refused")
	}
}

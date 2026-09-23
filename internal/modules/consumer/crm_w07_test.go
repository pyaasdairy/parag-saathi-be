package consumer

// W-07 must be on record as SENT before pack 2 expires. A suppressed W-07
// (here: the per-trigger kill switch) leaves the pack locked; the next day's
// claim sends it and only then does the pack expire.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMW07 -v

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCRMW07SuppressedLeavesPackLocked(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	res, err := w.svc.crmEnrol(ctx, "w07-test-operator", crmEnrolInput{
		Phone: "9000007104", Name: "Expiry Household", Line1: "Flat 9, Grace Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)

	// Day 9 since pack 1 landed: past the grace window, pack 2 still locked.
	nowIST := time.Now().In(istZone)
	sched := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 11, 0, 0, 0, istZone)
	forceFirstDelivery(t, w.db, cid, sched.AddDate(0, 0, -9))
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Locked {
		t.Fatalf("pack2 must start locked: %+v", off)
	}

	// Kill W-07 (G1) so the dispatch is SUPPRESSED, then run the sweep.
	if err := w.svc.deps.Flags.Set(ctx, crmFlagPrefix+"W-07", false, "w07-test"); err != nil {
		t.Fatalf("kill switch: %v", err)
	}
	w.svc.crmProcessSchedules(ctx, sched)
	if got := crmDispatchStatuses(t, w.db, cid, "W-07"); len(got) != 1 || got[0] != "SUPPRESSED" {
		t.Fatalf("W-07 under the kill switch: %v", got)
	}
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Locked {
		t.Fatalf("a suppressed W-07 must leave pack 2 LOCKED, got %q", off.Pack2State)
	}

	// Switch back on: the day's claim is already taken, so the same day
	// cannot send, and the pack must still not expire.
	if err := w.svc.deps.Flags.Set(ctx, crmFlagPrefix+"W-07", true, "w07-test"); err != nil {
		t.Fatalf("kill switch off: %v", err)
	}
	w.svc.crmProcessSchedules(ctx, sched.Add(time.Minute))
	if got := crmDispatchStatuses(t, w.db, cid, "W-07"); len(got) != 1 {
		t.Fatalf("same-day retry must not claim twice: %v", got)
	}
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Locked {
		t.Fatalf("pack 2 must stay LOCKED until W-07 is SENT, got %q", off.Pack2State)
	}

	// Next day: a fresh claim, the message goes out, and only then the CAS.
	w.svc.crmProcessSchedules(ctx, sched.AddDate(0, 0, 1))
	if got := crmDispatchStatuses(t, w.db, cid, "W-07"); len(got) != 2 || got[1] != "SENT" {
		t.Fatalf("W-07 the next day: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "W-07"); n != 1 {
		t.Fatalf("W-07 inbox rows = %d, want 1", n)
	}
	if off := mustOffer(t, ctx, w.svc, cid); off.Pack2State != pack2Expired {
		t.Fatalf("pack 2 must expire once W-07 is SENT, got %q", off.Pack2State)
	}

	// The wrapper reports what the log recorded.
	if st, g := w.svc.crmDispatch(ctx, "W-07", cid, nil); st != "" || g != "" {
		t.Fatalf("claim already taken today must report nothing: %q %q", st, g)
	}
	if st, _ := w.svc.crmDispatch(ctx, "NO-SUCH-TRIGGER", cid, nil); st != "" {
		t.Fatalf("unknown trigger must report nothing: %q", st)
	}
}

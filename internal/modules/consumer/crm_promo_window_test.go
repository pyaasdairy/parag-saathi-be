package consumer

// R1-11: E-07 (the referral ask, promotional) comes due 4 h after a 5-star
// rating. A rating after 17:00 or before 06:00 IST put the due time outside
// the 10:00-21:00 promotional window, G5 suppressed it at fire time, and the
// schedule row was closed for good: evening instant orders and early morning
// drops never got the ask. A promotional schedule that comes due outside the
// window now waits for the window's next opening instead.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMPromoWindow -v

import (
	"context"
	"testing"
	"time"
)

func TestCRMPromoWindowDelayedAskWaitsForTheWindow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	nowIST := time.Now().In(istZone)
	at := func(day, h, m int) time.Time {
		return time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day()+day, h, m, 0, 0, istZone)
	}
	cid := w.customer(t, "9000007851", 1000)
	yes := true
	if aerr := w.svc.applyConsents(ctx, cid, []consentInput{{Type: "marketing_whatsapp", Granted: &yes, Version: "2026-07"}}); aerr != nil {
		t.Fatalf("consent: %v", aerr)
	}
	o := instantOrderDelivered(t, w, cid)
	if _, err := w.svc.reviewOrder(ctx, cid.Hex(), o.OrderID, 5, "lovely"); err != nil {
		t.Fatalf("reviewOrder: %v", err)
	}
	crmStampEvents(t, w, cid, at(0, 18, 0)) // rated at 18:00 IST
	w.svc.crmProcessEventsAt(ctx, at(0, 18, 0))

	w.svc.crmFireDueSchedules(ctx, at(0, 22, 1)) // due 22:00: outside 10:00-21:00
	if got := crmDispatchStatuses(t, w.db, cid, "E-07"); len(got) != 0 {
		t.Fatalf("E-07 outside the promotional window must not be dispatched (nor lost): %v", got)
	}
	rows := crmScheduleRows(t, w.db, cid, "E-07")
	if len(rows) != 1 || rows[0].Status != "NEW" || !rows[0].DueAt.Equal(at(1, 10, 0).UTC()) {
		t.Fatalf("E-07 must wait for 10:00 the next morning: %+v", rows)
	}
	w.svc.crmFireDueSchedules(ctx, at(1, 10, 1))
	if got := crmDispatchStatuses(t, w.db, cid, "E-07"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("E-07 at the next window: %v", got)
	}
	if n := inboxCount(t, w.db, cid, "E-07"); n != 1 {
		t.Fatalf("E-07 inbox rows = %d", n)
	}
}

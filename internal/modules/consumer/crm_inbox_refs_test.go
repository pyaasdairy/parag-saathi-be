package consumer

// F19: an inbox row names the order or complaint it is about, so the app's
// notification centre (lib/notificationCenter.ts dropNoticesTheServerSent)
// can pair its own local notice with the server's row exactly, instead of by
// event class within 24 hours.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMInboxRefs -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCRMInboxRefsNameTheOrderAndTheComplaint(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000008601", 1000)

	o := instantOrderDelivered(t, w, cid)
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{
		Ref: "PYS-R1", Category: "late", OrderID: o.OrderID, Detail: "It came an hour late",
	}); err != nil {
		t.Fatalf("fileComplaint: %v", err)
	}
	w.svc.crmProcessEvents(ctx)

	req := httptest.NewRequest(http.MethodGet, "/crm/inbox", nil)
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
	rec := httptest.NewRecorder()
	(&handler{svc: w.svc}).crmInbox(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /crm/inbox: %d %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byTrigger := map[string]map[string]any{}
	for _, r := range rows {
		byTrigger[r["trigger_id"].(string)] = r
	}
	for _, id := range []string{"D-01", "D-02", "D-06"} {
		r := byTrigger[id]
		if r == nil || r["order_id"] != o.OrderID {
			t.Fatalf("%s must name order %s: %v", id, o.OrderID, r)
		}
		if _, has := r["complaint_ref"]; has {
			t.Fatalf("%s is not about a complaint: %v", id, r)
		}
	}
	if r := byTrigger["E-02"]; r == nil || r["complaint_ref"] != "PYS-R1" || r["order_id"] != o.OrderID {
		t.Fatalf("E-02 must name the complaint ref and its order: %v", r)
	}
	// A wallet receipt carries a ledger ref, which is not a complaint ref,
	// and names no order: both keys are absent.
	if r := byTrigger["B-06"]; r == nil {
		t.Fatalf("B-06 row missing: %v", rows)
	} else if _, has := r["complaint_ref"]; has {
		t.Fatalf("B-06 must not carry a complaint_ref: %v", r)
	} else if _, has := r["order_id"]; has {
		t.Fatalf("B-06 must not carry an order_id: %v", r)
	}
}

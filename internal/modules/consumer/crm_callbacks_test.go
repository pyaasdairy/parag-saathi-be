package consumer

// E-05 (milk quality / safety) routes to a HUMAN: the human_call transport
// writes a crm_callbacks row an operator works from GET
// /consumer/admin/crm/callbacks. The config forbids an automatic reply for
// this trigger ("NEVER auto-respond"), so no E-05 inbox row is written.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMCallback -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func crmCallbackRows(t *testing.T, w *chainWorld, cid primitive.ObjectID) []crmCallback {
	t.Helper()
	cur, err := w.db.Collection(collCRMCallbacks).Find(context.Background(), bson.D{{Key: "consumer_id", Value: cid}})
	if err != nil {
		t.Fatalf("callbacks read: %v", err)
	}
	var rows []crmCallback
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatalf("callbacks decode: %v", err)
	}
	return rows
}

func TestCRMCallbackQualityComplaintReachesAHuman(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000008201", 0)

	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{
		Ref: "PYS-Q1", Category: "quality", Detail: "The milk smelled sour this morning",
	}); err != nil {
		t.Fatalf("fileComplaint: %v", err)
	}
	w.svc.crmProcessEvents(ctx)

	rows := crmCallbackRows(t, w, cid)
	if len(rows) != 1 {
		t.Fatalf("callback rows = %d, want 1: %+v", len(rows), rows)
	}
	cb := rows[0]
	if cb.TriggerID != "E-05" || cb.Status != "open" || !strings.Contains(cb.Reason, "QUALITY") {
		t.Fatalf("callback row: %+v", cb)
	}
	if cb.Payload["ref"] != "PYS-Q1" || cb.Payload["category"] != "quality" || cb.Phone != "919000008201" {
		t.Fatalf("callback payload / phone: %+v", cb)
	}
	var disp []crmDispatchRow
	cur, err := w.db.Collection(collCRMDispatch).Find(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "E-05"}})
	if err != nil {
		t.Fatalf("dispatch read: %v", err)
	}
	if err := cur.All(ctx, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	if len(disp) != 1 || disp[0].Status != "SENT" || disp[0].Intended != "human_call" || disp[0].Channel != "human_call" {
		t.Fatalf("E-05 dispatch rows: %+v", disp)
	}
	// The member is acknowledged by E-02 like every complaint; E-05 itself
	// never writes an automatic reply.
	if n := inboxCount(t, w.db, cid, "E-05"); n != 0 {
		t.Fatalf("E-05 must not auto-respond: %d inbox rows", n)
	}
	if n := inboxCount(t, w.db, cid, "E-02"); n != 1 {
		t.Fatalf("E-02 acknowledgement rows = %d, want 1", n)
	}

	// The app's offline retry of the same ref, and a second drain, change nothing.
	if _, err := w.svc.fileComplaint(ctx, cid, complaintInput{
		Ref: "PYS-Q1", Category: "quality", Detail: "The milk smelled sour this morning",
	}); err != nil {
		t.Fatalf("fileComplaint retry: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	if n := len(crmCallbackRows(t, w, cid)); n != 1 {
		t.Fatalf("a retried complaint queued %d callbacks, want 1", n)
	}

	// A "missing" complaint names human_call as E-04's parallel channel: the
	// member gets the E-04 message AND an operator gets a callback.
	o := instantOrderDelivered(t, w, w.customer(t, "9000008202", 500))
	owner, _ := primitive.ObjectIDFromHex(o.UserID)
	if _, err := w.svc.fileComplaint(ctx, owner, complaintInput{
		Ref: "PYS-M1", Category: "missing", OrderID: o.OrderID, Detail: "One pack was missing",
	}); err != nil {
		t.Fatalf("fileComplaint missing: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	if rows := crmCallbackRows(t, w, owner); len(rows) != 1 || rows[0].TriggerID != "E-04" {
		t.Fatalf("E-04 parallel human_call: %+v", rows)
	}
	if n := inboxCount(t, w.db, owner, "E-04"); n != 1 {
		t.Fatalf("E-04 inbox rows = %d, want 1", n)
	}

	// The operator reads the queue through the admin CRM (same key gate).
	t.Setenv("ADMIN_API_KEY", strings.Repeat("k", adminKeyMinLen))
	r := chi.NewRouter()
	registerAdminCRM(r, &handler{svc: w.svc}, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/crm/callbacks?status=open", nil)
	req.Header.Set("X-Admin-Key", strings.Repeat("k", adminKeyMinLen))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET callbacks: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
			Open  int              `json:"open"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Data.Total != 2 || out.Data.Open != 2 || out.Data.Items[0]["triggerId"] != "E-04" || out.Data.Items[1]["triggerId"] != "E-05" {
		t.Fatalf("callbacks view (newest first): %s", rec.Body.String())
	}
	// Without the key the route is closed.
	bad := httptest.NewRequest(http.MethodGet, "/admin/crm/callbacks", nil)
	bad.Header.Set("X-Admin-Key", "wrong")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("callbacks with a wrong key: %d", rec.Code)
	}

	// DPDP erasure takes the member's callback rows with the account.
	if err := w.svc.repo.deleteAccountCascade(ctx, cid); err != nil {
		t.Fatalf("erasure: %v", err)
	}
	if n := len(crmCallbackRows(t, w, cid)); n != 0 {
		t.Fatalf("erasure left %d callback rows", n)
	}
}

package consumer

// The Saathi operator queue for the complaint register:
// GET /consumer/ops/complaints and PATCH /consumer/ops/complaints/{ref}, in
// the STORE_MANAGER / SUPER_ADMIN operator group.
//
// R4-06: every route in that group answers in Saathi's wire format ({data}
// on success, {error:{code,message}} on failure; the group's own auth
// middleware already does), and the Saathi ApiClient reads only that shape.
// These two answered a bare body and a flat {code,message}, so a Saathi
// client lost the code and the message of every refusal.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run OpsComplaint -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func opsComplaintCall(t *testing.T, w *chainWorld, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	h := &handler{svc: w.svc}
	r.Get("/ops/complaints", h.opsListComplaints)
	r.Patch("/ops/complaints/{ref}", h.opsUpdateComplaint)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: body is not a JSON object: %s", method, path, rec.Body.String())
	}
	return rec.Code, out
}

func TestOpsComplaintRoutesSpeakTheOperatorEnvelope(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	cid := w.customer(t, "9000005501", 0)
	if _, err := w.svc.fileComplaint(context.Background(), cid, complaintInput{Ref: "PYS-OPS01", Category: "late", Detail: "late again"}); err != nil {
		t.Fatalf("file: %v", err)
	}

	code, body := opsComplaintCall(t, w, http.MethodGet, "/ops/complaints", "")
	rows, ok := body["data"].([]any)
	if code != http.StatusOK || !ok || len(rows) != 1 {
		t.Fatalf("list: %d %v, want 200 {data:[1 row]}", code, body)
	}
	code, body = opsComplaintCall(t, w, http.MethodPatch, "/ops/complaints/PYS-OPS01", `{"status":"resolved","resolution":"Sorry, we are on it."}`)
	if row, ok := body["data"].(map[string]any); code != http.StatusOK || !ok || row["status"] != "resolved" {
		t.Fatalf("update: %d %v, want 200 {data:{status:resolved}}", code, body)
	}
	for _, c := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{http.MethodGet, "/ops/complaints?status=banished", "", http.StatusBadRequest, "BAD_REQUEST"},
		{http.MethodPatch, "/ops/complaints/PYS-NOPE", `{"status":"closed"}`, http.StatusNotFound, "NOT_FOUND"},
		{http.MethodPatch, "/ops/complaints/PYS-OPS01", `{}`, http.StatusBadRequest, "BAD_REQUEST"},
	} {
		code, body := opsComplaintCall(t, w, c.method, c.path, c.body)
		e, ok := body["error"].(map[string]any)
		if code != c.status || !ok || e["code"] != c.code || e["message"] == "" || e["message"] == nil {
			t.Errorf("%s %s: %d %v, want %d {error:{code:%s,message}}", c.method, c.path, code, body, c.status, c.code)
		}
	}
}

// R4-05: a ref is unique PER MEMBER (the app mints PYS- plus five letters, so
// two members hold the same code after a few thousand complaints), but the
// operator PATCH resolved by ref alone and FindOneAndUpdate took whichever row
// came first: the resolution the member reads verbatim, and E-06, could reach
// the wrong customer. A ref two members share is now refused, and the
// globally unique complaint id (cmp_...) names the row exactly.
func TestOpsComplaintSharedRefIsRefusedAndTheIDIsExact(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	a := w.customer(t, "9000005502", 0)
	b := w.customer(t, "9000005503", 0)
	ca, err := w.svc.fileComplaint(ctx, a, complaintInput{Ref: "PYS-AAAAA", Category: "late", Detail: "a's complaint"})
	if err != nil {
		t.Fatalf("file a: %v", err)
	}
	cb, err := w.svc.fileComplaint(ctx, b, complaintInput{Ref: "PYS-AAAAA", Category: "missing", Detail: "b's complaint"})
	if err != nil {
		t.Fatalf("file b: %v", err)
	}

	code, body := opsComplaintCall(t, w, http.MethodPatch, "/ops/complaints/PYS-AAAAA", `{"status":"resolved","resolution":"Refunded to your wallet."}`)
	if e, _ := body["error"].(map[string]any); code != http.StatusConflict || e["code"] != "AMBIGUOUS_REF" {
		t.Fatalf("a ref two members share: %d %v, want 409 AMBIGUOUS_REF", code, body)
	}
	for _, cid := range []primitive.ObjectID{a, b} {
		mine, _ := w.svc.listComplaints(ctx, cid)
		if len(mine) != 1 || mine[0].Status != complaintOpen || mine[0].Resolution != "" {
			t.Fatalf("a refused update changed a member's complaint: %+v", mine)
		}
	}

	code, body = opsComplaintCall(t, w, http.MethodPatch, "/ops/complaints/"+cb.ID, `{"status":"resolved","resolution":"Refunded to your wallet."}`)
	if row, _ := body["data"].(map[string]any); code != http.StatusOK || row["id"] != cb.ID {
		t.Fatalf("update by complaint id: %d %v", code, body)
	}
	mineA, _ := w.svc.listComplaints(ctx, a)
	mineB, _ := w.svc.listComplaints(ctx, b)
	if mineA[0].Status != complaintOpen || mineA[0].Resolution != "" || mineB[0].Status != complaintResolved {
		t.Fatalf("the id must reach exactly its own row: a=%+v b=%+v", mineA[0], mineB[0])
	}
	if n := len(crmEventsOf(t, w.db, a, "complaint.resolved")); n != 0 {
		t.Fatalf("E-06 went to the wrong member: %d events", n)
	}
	if n := len(crmEventsOf(t, w.db, b, "complaint.resolved")); n != 1 {
		t.Fatalf("complaint.resolved for the member resolved: %d", n)
	}
	_ = ca
	// A ref only one member holds still resolves by ref, as before.
	if _, err := w.svc.fileComplaint(ctx, a, complaintInput{Ref: "PYS-SOLO1", Category: "late", Detail: "only mine"}); err != nil {
		t.Fatalf("file solo: %v", err)
	}
	if code, body := opsComplaintCall(t, w, http.MethodPatch, "/ops/complaints/pys-solo1", `{"status":"in_review"}`); code != http.StatusOK {
		t.Fatalf("a unique ref: %d %v", code, body)
	}
}

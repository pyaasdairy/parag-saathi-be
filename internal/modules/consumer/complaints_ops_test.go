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

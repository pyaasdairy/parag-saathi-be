package consumer

// Production (release/26.07.03) stored complaints with an ObjectID _id and NO
// complaint_id field, and served the _id as the row's "id". This release
// serialises "id" from complaint_id, so every row production already holds
// read "id":"" everywhere: duplicate React keys on the member's complaints
// screen, and nothing the Saathi queue or the website's admin CRM could
// address by id. A legacy row now answers to a stable id derived from its _id
// ("cmp_" + the _id hex) on every read and every by-id lookup, and a boot
// backfill stamps that same id on the row, so the id never changes.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run ComplaintLegacy -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// insertLegacyComplaint writes a row exactly as release/26.07.03 filed it: an
// ObjectID _id, no complaint_id and no phone. withEmptyID writes the field as
// "" instead of leaving it out.
func insertLegacyComplaint(t *testing.T, w *chainWorld, cid primitive.ObjectID, ref string, at time.Time, withEmptyID bool) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	doc := bson.D{
		{Key: "_id", Value: id}, {Key: "consumer_id", Value: cid}, {Key: "ref", Value: ref},
		{Key: "category", Value: "late"}, {Key: "detail", Value: "legacy " + ref},
		{Key: "status", Value: complaintOpen}, {Key: "created_at", Value: at}, {Key: "updated_at", Value: at},
	}
	if withEmptyID {
		doc = append(doc, bson.E{Key: "complaint_id", Value: ""})
	}
	if _, err := w.db.Collection(collComplaints).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("legacy complaint %s: %v", ref, err)
	}
	return id
}

// memberComplaintIDs drives GET /consumer/complaints for the member and
// returns the ids in list order.
func memberComplaintIDs(t *testing.T, w *chainWorld, cid primitive.ObjectID) []string {
	t.Helper()
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodGet, "/complaints", nil)
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
	rec := httptest.NewRecorder()
	h.listComplaints(rec, req)
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("member list: %d %s", rec.Code, rec.Body.String())
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		s, _ := r["id"].(string)
		out = append(out, s)
	}
	return out
}

// adminCRMComplaintIDs drives GET /consumer/admin/crm/complaints.
func adminCRMComplaintIDs(t *testing.T, w *chainWorld) []string {
	t.Helper()
	h := &handler{svc: w.svc}
	rec := httptest.NewRecorder()
	h.crmComplaints(rec, httptest.NewRequest(http.MethodGet, "/admin/crm/complaints", nil))
	var body struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("admin CRM list: %d %s", rec.Code, rec.Body.String())
	}
	out := []string{}
	for _, r := range body.Data.Items {
		s, _ := r["id"].(string)
		out = append(out, s)
	}
	return out
}

// adminCRMAnswer drives POST /consumer/admin/crm/complaints/{complaintId}.
func adminCRMAnswer(t *testing.T, w *chainWorld, complaintID, body string) (int, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	h := &handler{svc: w.svc}
	r.Post("/admin/crm/complaints/{complaintId}", h.crmUpdateComplaint)
	req := httptest.NewRequest(http.MethodPost, "/admin/crm/complaints/"+complaintID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func assertDistinctIDs(t *testing.T, label string, ids []string, want int) {
	t.Helper()
	if len(ids) != want {
		t.Fatalf("%s: %d rows, want %d: %v", label, len(ids), want, ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("%s: every row needs its own non-empty id: %v", label, ids)
		}
		seen[id] = true
	}
}

func storedComplaint(t *testing.T, w *chainWorld, id primitive.ObjectID) bson.M {
	t.Helper()
	var m bson.M
	if err := w.db.Collection(collComplaints).FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&m); err != nil {
		t.Fatalf("stored complaint %s: %v", id.Hex(), err)
	}
	return m
}

func TestComplaintLegacyRowsAnswerToAStableID(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000005601", 0)
	base := time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)
	l1 := insertLegacyComplaint(t, w, cid, "PYS-LEG01", base, false)
	l2 := insertLegacyComplaint(t, w, cid, "PYS-LEG02", base.Add(time.Hour), false)
	l3 := insertLegacyComplaint(t, w, cid, "PYS-LEG03", base.Add(2*time.Hour), true)
	fresh, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-NEW01", Category: "quality", Detail: "sour"})
	if err != nil {
		t.Fatalf("file a new complaint: %v", err)
	}
	id1, id2, id3 := "cmp_"+l1.Hex(), "cmp_"+l2.Hex(), "cmp_"+l3.Hex()

	// The member's list: every row has its own non-empty id, the legacy ones
	// derived from their _id, the new one its own.
	got := memberComplaintIDs(t, w, cid)
	assertDistinctIDs(t, "member list", got, 4)
	if want := []string{fresh.ID, id3, id2, id1}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("member list ids %v, want %v", got, want)
	}
	// The app's offline retry of a legacy ref hands back the same row, with
	// that id (an empty id left the shipped build's row queued forever).
	again, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "pys-leg01", Category: "late", Detail: "retry"})
	if err != nil || again.ID != id1 || again.MongoID != l1 {
		t.Fatalf("retry of a legacy ref: %v %+v, want the row with id %s", err, again, id1)
	}

	// The Saathi queue lists the same ids and answers a legacy row by its id.
	code, body := opsComplaintCall(t, w, http.MethodGet, "/ops/complaints", "")
	rows, _ := body["data"].([]any)
	opsIDs := []string{}
	for _, r := range rows {
		m, _ := r.(map[string]any)
		s, _ := m["id"].(string)
		opsIDs = append(opsIDs, s)
	}
	if code != http.StatusOK {
		t.Fatalf("ops list: %d %v", code, body)
	}
	assertDistinctIDs(t, "ops list", opsIDs, 4)
	code, body = opsComplaintCall(t, w, http.MethodPatch, "/ops/complaints/"+id1, `{"status":"in_review"}`)
	if row, _ := body["data"].(map[string]any); code != http.StatusOK || row["id"] != id1 || row["status"] != complaintInReview {
		t.Fatalf("ops answer by the legacy id: %d %v", code, body)
	}
	if m := storedComplaint(t, w, l1); m["status"] != complaintInReview {
		t.Fatalf("ops answer did not reach the legacy row: %v", m)
	}
	// ...and a legacy row whose complaint_id is an empty string.
	if code, body := opsComplaintCall(t, w, http.MethodPatch, "/ops/complaints/"+id3, `{"status":"closed"}`); code != http.StatusOK {
		t.Fatalf("ops answer by the id of a row with complaint_id \"\": %d %v", code, body)
	}

	// The website's admin CRM lists the same ids and answers by them.
	assertDistinctIDs(t, "admin CRM list", adminCRMComplaintIDs(t, w), 4)
	code, body = adminCRMAnswer(t, w, id2, `{"status":"resolved","resolution":"Refunded one pack to your Wallet"}`)
	if row, _ := body["data"].(map[string]any); code != http.StatusOK || row["id"] != id2 || row["status"] != complaintResolved {
		t.Fatalf("admin CRM answer by the legacy id: %d %v", code, body)
	}
	if m := storedComplaint(t, w, l2); m["status"] != complaintResolved || m["resolution"] != "Refunded one pack to your Wallet" {
		t.Fatalf("admin CRM answer did not reach the legacy row: %v", m)
	}
	if evs := crmEventsOf(t, w.db, cid, "complaint.resolved"); len(evs) != 1 || evs[0].Payload["complaint_id"] != id2 {
		t.Fatalf("complaint.resolved for the legacy row: %+v", evs)
	}
	// The other rows were not touched.
	if m := storedComplaint(t, w, fresh.MongoID); m["status"] != complaintOpen {
		t.Fatalf("the new row changed: %v", m)
	}

	// The derived form names only a row that has no id of its own: a new row
	// answers to its own cmp_ id, never to one built from its _id.
	if code, _ := adminCRMAnswer(t, w, "cmp_"+fresh.MongoID.Hex(), `{"status":"closed"}`); code != http.StatusNotFound {
		t.Fatalf("a new row by an _id-derived id: %d, want 404", code)
	}
	if code, body := adminCRMAnswer(t, w, fresh.ID, `{"status":"in_review"}`); code != http.StatusOK {
		t.Fatalf("a new row by its own id: %d %v", code, body)
	}
	if code, _ := adminCRMAnswer(t, w, "cmp_"+primitive.NewObjectID().Hex(), `{"status":"closed"}`); code != http.StatusNotFound {
		t.Fatalf("an unknown derived id: %d, want 404", code)
	}
}

func TestComplaintLegacyIDBackfillStampsTheSameID(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000005602", 0)
	other := w.customer(t, "9000005603", 0)
	base := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	l1 := insertLegacyComplaint(t, w, cid, "PYS-BKF01", base, false)
	l2 := insertLegacyComplaint(t, w, other, "PYS-BKF02", base.Add(time.Minute), true)
	fresh, err := w.svc.fileComplaint(ctx, cid, complaintInput{Ref: "PYS-BKF03", Category: "app", Detail: "crash"})
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	before := memberComplaintIDs(t, w, cid)

	n, err := w.svc.repo.backfillLegacyComplaintIDs(ctx)
	if err != nil || n != 2 {
		t.Fatalf("backfill: %d %v, want the two legacy rows", n, err)
	}
	if m := storedComplaint(t, w, l1); m["complaint_id"] != "cmp_"+l1.Hex() {
		t.Fatalf("backfilled id: %v", m)
	}
	if m := storedComplaint(t, w, l2); m["complaint_id"] != "cmp_"+l2.Hex() {
		t.Fatalf("backfilled id (was \"\"): %v", m)
	}
	if m := storedComplaint(t, w, fresh.MongoID); m["complaint_id"] != fresh.ID {
		t.Fatalf("a row with its own id was rewritten: %v", m)
	}
	// Idempotent: nothing left to stamp.
	if n, err := w.svc.repo.backfillLegacyComplaintIDs(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill: %d %v, want 0", n, err)
	}
	// The ids the member saw before the backfill are the ids after it, and
	// the operator paths still resolve them.
	if after := memberComplaintIDs(t, w, cid); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("ids changed across the backfill: %v -> %v", before, after)
	}
	if code, body := opsComplaintCall(t, w, http.MethodPatch, "/ops/complaints/cmp_"+l1.Hex(), `{"status":"resolved","resolution":"Sorted."}`); code != http.StatusOK {
		t.Fatalf("ops answer after the backfill: %d %v", code, body)
	}
	if code, body := adminCRMAnswer(t, w, "cmp_"+l2.Hex(), `{"status":"closed"}`); code != http.StatusOK {
		t.Fatalf("admin CRM answer after the backfill: %d %v", code, body)
	}
}

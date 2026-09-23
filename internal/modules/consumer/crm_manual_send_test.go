package consumer

// The operator's manual send derives its synthetic trigger id from the
// message CONTENT, so the same message to the same member on the same day
// meets the existing claim: one SENT row, the repeat refused as DUPLICATE.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMManualSend -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCRMManualSendDedupsByContent(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007107", 0)
	h := &handler{svc: w.svc}

	send := func(body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/crm/message", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.crmComposeMessage(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	const hello = `{"phone":"9000007107","body_en":"Your milk is on its way","category":"service_implicit"}`

	if code, out := send(hello); code != http.StatusOK || out["sent"] != true {
		t.Fatalf("first send: %d %v", code, out)
	}
	if code, out := send(hello); code != http.StatusConflict {
		t.Fatalf("identical send the same day must be refused: %d %v", code, out)
	}
	// Different content is a different synthetic trigger and goes out.
	if code, out := send(`{"phone":"9000007107","body_en":"Your milk has arrived","category":"service_implicit"}`); code != http.StatusOK || out["sent"] != true {
		t.Fatalf("different content: %d %v", code, out)
	}

	cur, err := w.db.Collection(collCRMDispatch).Find(ctx,
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: bson.D{{Key: "$regex", Value: "^MANUAL-"}}}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		t.Fatalf("dispatch rows: %v", err)
	}
	var rows []crmDispatchRow
	if err := cur.All(ctx, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 || rows[0].Status != "SENT" || rows[1].Status != "SENT" || rows[0].TriggerID == rows[1].TriggerID {
		t.Fatalf("want exactly two SENT rows with distinct content ids, got %+v", rows)
	}
	if strings.Contains(rows[0].TriggerID, ":") || len(rows[0].TriggerID) != len("MANUAL-")+16 {
		t.Fatalf("trigger id must be the content hash, got %q", rows[0].TriggerID)
	}
	if n := inboxCount(t, w.db, cid, rows[0].TriggerID); n != 1 {
		t.Fatalf("inbox rows for the first message = %d", n)
	}
}

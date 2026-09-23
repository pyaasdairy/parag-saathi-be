package consumer

// A channel the config names but nothing serves is logged, never skipped in
// silence, and the dispatch row records the intended primary beside the
// channel that actually carried the message.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMIntended -v

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
)

func TestCRMUnpluggedChannelIsLogged(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	sms, smsHits := testSMSChannel(t, map[string]string{"D-06": "1207d"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "D-06", Category: "service_implicit",
		Delivery: crmDelivery{Primary: "push", Fallback: []crmFallback{{Channel: "whatsapp"}, {Channel: "sms"}}}}
	delivered := crmDeliverExternal(context.Background(), log, "919876543210", tr,
		crmTemplate{HI: "Deliver ho gaya"}, nil, map[string]crmTransport{"sms": sms})
	if len(delivered) != 1 || delivered[0] != "sms" || *smsHits != 1 {
		t.Fatalf("the plugged fallback must still deliver: %v (hits %d)", delivered, *smsHits)
	}
	out := buf.String()
	if strings.Count(out, "channel not plugged in") != 2 {
		t.Fatalf("want one warning per unplugged channel (push, whatsapp), got:\n%s", out)
	}
	for _, want := range []string{"trigger=D-06", "channel=push", "role=primary", "channel=whatsapp", "role=fallback"} {
		if !strings.Contains(out, want) {
			t.Fatalf("warning lacks %q:\n%s", want, out)
		}
	}
	// A nil log is still tolerated (the transports tests pass nil).
	crmDeliverExternal(context.Background(), nil, "919876543210", tr, crmTemplate{HI: "x"}, nil, map[string]crmTransport{})
}

func TestCRMIntendedPrimaryOnDispatchRow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000007106", 0)

	if st, _ := w.svc.crmDispatch(ctx, "B-01", cid, nil); st != "SENT" {
		t.Fatalf("B-01 dispatch: %q", st)
	}
	var row crmDispatchRow
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx,
		bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "B-01"}}).Decode(&row); err != nil {
		t.Fatalf("dispatch row: %v", err)
	}
	if row.Intended != "whatsapp" || row.Channel != "inapp" || row.Status != "SENT" {
		t.Fatalf("row must record intended=whatsapp next to channel=inapp: %+v", row)
	}

	// The operator dispatch log carries it (additive key).
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodGet, "/crm/dispatch-log/9000007106", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("phone", "9000007106")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.crmDispatchLog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch log: %d %s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 1 {
		t.Fatalf("dispatch log body: %v %s", err, rec.Body.String())
	}
	if out[0]["intended"] != "whatsapp" || out[0]["channel"] != "inapp" || out[0]["trigger_id"] != "B-01" {
		t.Fatalf("dispatch log row: %v", out[0])
	}
}

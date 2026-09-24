package consumer

// CRM caveat 4 (owner, 24 Sep): a provider that refuses a message is loud.
// The owner saw "money added" never reach a phone and nothing said why: an
// SMS, WhatsApp or push rejection was a Warn line and the dispatch row read
// SENT via the inbox. A rejection (or a failure with an unknown outcome) is
// now logged at ERROR with the trigger and template, and recorded on the
// dispatch row as channel_errors (also on the operator's dispatch log); the
// row's channel still names only what really carried the message. A member
// with no device is not a provider failure and is not recorded.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run ChannelErrors -v

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
)

// syncBuffer is a log sink the worker's goroutines may share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestCRMChannelErrorsAreRecordedAndLoud(t *testing.T) {
	clearDryRunEnv(t)
	stub := newStubProvider(t, `{"type":"error","message":"template not approved"}`)
	t.Setenv("CRM_MSG91_AUTHKEY", "dry-run-key")
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"B-06":"1207160000000000006"}`)
	t.Setenv("CRM_MSG91_BASE_URL", stub.srv.URL)
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	sink := &syncBuffer{}
	w.svc.log = slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// Push is on, and this member has no device: not a provider failure.
	push := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		t.Errorf("no push call expected for a member without a device")
	}))
	defer push.Close()
	w.svc.crmPush = testPushChannel(push.URL, "", w)

	cid := w.customer(t, "9000015501", 0)
	if _, err := w.svc.creditTopup(ctx, cid, 200, "razorpay", "topup_err_1"); err != nil {
		t.Fatalf("creditTopup: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	stub.only(t) // the SMS was attempted, and MSG91 refused it

	var row struct {
		Status        string            `bson:"status"`
		Channel       string            `bson:"channel"`
		ChannelErrors []crmChannelError `bson:"channel_errors"`
	}
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "B-06"}}).Decode(&row); err != nil {
		t.Fatalf("dispatch row: %v", err)
	}
	if row.Status != "SENT" || row.Channel != "inapp" {
		t.Fatalf("dispatch status %s channel %q: the refused SMS must not be claimed", row.Status, row.Channel)
	}
	if len(row.ChannelErrors) != 1 {
		t.Fatalf("channel_errors %+v, want exactly the refused SMS", row.ChannelErrors)
	}
	e := row.ChannelErrors[0]
	if e.Channel != "sms" || e.Role != "fallback" || e.Transient || !strings.Contains(e.Error, "rejected") || e.Template != "T-B05-REFUND" {
		t.Fatalf("channel error %+v", e)
	}
	errorsLogged := 0
	for _, rec := range sink.records(t) {
		if rec["level"] == "ERROR" && rec["trigger"] == "B-06" && rec["channel"] == "sms" {
			errorsLogged++
		}
		if rec["level"] == "ERROR" && rec["channel"] == "push" {
			t.Fatalf("a member without a device was logged as a push failure: %v", rec)
		}
	}
	if errorsLogged != 1 {
		t.Fatalf("the SMS rejection must be logged once at ERROR with the trigger id; records: %v", sink.records(t))
	}

	// The operator's dispatch log shows it.
	h := &handler{svc: w.svc}
	req := httptest.NewRequest(http.MethodGet, "/crm/dispatch/9000015501", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("phone", "9000015501")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.crmDispatchLog(rec, req)
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("dispatch log: %d %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, r := range out {
		if r["trigger_id"] == "B-06" {
			errs, _ := r["channel_errors"].([]any)
			found = len(errs) == 1
		}
	}
	if !found {
		t.Fatalf("the operator's dispatch log does not show the refused SMS: %s", rec.Body.String())
	}
}

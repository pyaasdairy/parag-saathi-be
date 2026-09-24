package consumer

// CRM caveat 3 (owner, 24 Sep): "money added" (B-06) reaches the phone as a
// push as soon as push is enabled, in parallel with its WhatsApp/SMS chain,
// like the other service messages that name push. It named no push at all,
// so with the MSG91/WhatsApp keys still pending a top-up was announced in
// the in-app inbox only.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run B06Push -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestCRMB06PushIsParallel(t *testing.T) {
	d := crmConfigLoad().Triggers["B-06"].Delivery
	hasPush := false
	for _, p := range d.Parallel {
		hasPush = hasPush || p == "push"
	}
	if !hasPush || d.Primary != "whatsapp" || len(d.Fallback) != 1 || d.Fallback[0].Channel != "sms" {
		t.Fatalf("B-06 delivery %+v: want whatsapp, fallback sms, and push in parallel", d)
	}
}

func TestCRMB06PushReachesThePhone(t *testing.T) {
	clearDryRunEnv(t)
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var msgs []map[string]any
		_ = json.Unmarshal(b, &msgs)
		mu.Lock()
		for _, m := range msgs {
			body, _ := m["body"].(string)
			bodies = append(bodies, body)
		}
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"data":[{"status":"ok","id":"b06"}]}`))
	}))
	defer srv.Close()
	w.svc.crmPush = testPushChannel(srv.URL, "", w)

	cid := w.customer(t, "9000015401", 0)
	if err := w.svc.registerPushDevice(ctx, cid, pushRegisterInput{Token: "ExponentPushToken[b06b06b06b06b06b06b06b]", Platform: "android", Provider: "expo"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := w.svc.creditTopup(ctx, cid, 300, "razorpay", "topup_b06_push"); err != nil {
		t.Fatalf("creditTopup: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "₹300 added to your Wallet") {
		t.Fatalf("B-06 push bodies %q, want the money-added message once", got)
	}
	var row crmDispatchRow
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "B-06"}}).Decode(&row); err != nil {
		t.Fatalf("dispatch row: %v", err)
	}
	if row.Status != "SENT" || row.Channel != "inapp+push" {
		t.Fatalf("B-06 dispatch: status %s channel %q, want SENT inapp+push", row.Status, row.Channel)
	}
}

package consumer

// Contract C5: the Expo push transport is the config's "push" channel. It
// sends the rendered body to the member's registered devices, deep-links the
// tap, falls through at once when the member has no device, drops a token
// Expo no longer knows, and stays inert without EXPO_PUSH_ENABLED.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run CRMPush -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestCRMPushHelpers(t *testing.T) {
	if crmPushHref(map[string]string{"ORDER_ID": "ord_9"}) != "/order/ord_9" ||
		crmPushHref(map[string]string{"COMPLAINT_ID": "cmp_1"}) != "/complaints" ||
		crmPushHref(nil) != "/inbox" {
		t.Fatal("crmPushHref routes")
	}
	if crmPushBody(crmTemplate{EN: "en", HI: "hi"}) != "en" || crmPushBody(crmTemplate{HI: "hi"}) != "hi" {
		t.Fatal("crmPushBody picks EN then HI")
	}
	if crmPushAndroidChannel(crmTrigger{Category: "promotional"}) != "offers" ||
		crmPushAndroidChannel(crmTrigger{Section: "B"}) != "wallet" ||
		crmPushAndroidChannel(crmTrigger{Section: "D"}) != "delivery" ||
		crmPushAndroidChannel(crmTrigger{Section: "E"}) != "orders" {
		t.Fatal("crmPushAndroidChannel mapping")
	}
	// Inert without the env, nil-safe, and only plugged in when enabled.
	t.Setenv("EXPO_PUSH_ENABLED", "")
	if newPushChannel(nil, nil).Enabled() {
		t.Fatal("push must be disabled without EXPO_PUSH_ENABLED")
	}
	var nilPush *pushChannel
	if nilPush.Enabled() {
		t.Fatal("nil receiver must report disabled")
	}
	if got := (&service{}).crmTransports(); got != nil {
		t.Fatalf("no transports without keys, got %v", got)
	}
	t.Setenv("EXPO_PUSH_ENABLED", "true")
	if !newPushChannel(nil, nil).Enabled() {
		t.Fatal("EXPO_PUSH_ENABLED=true must enable push")
	}
	if got := (&service{crmPush: &pushChannel{enabled: true}}).crmTransports(); got == nil || got["push"] == nil {
		t.Fatalf("enabled push must be a transport, got %v", got)
	}
	if err := (&pushChannel{enabled: true}).available(crmTrigger{}, crmTemplate{EN: "x"}); err == nil {
		t.Fatal("an unbound push transport must report unavailable")
	}
}

func TestCRMPushDeliversAndFallsThrough(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()

	type captured struct {
		auth string
		msgs []map[string]any
	}
	var got []captured
	answer := `{"data":[{"status":"ok","id":"r1"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var msgs []map[string]any
		_ = json.Unmarshal(b, &msgs)
		got = append(got, captured{auth: r.Header.Get("Authorization"), msgs: msgs})
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(answer))
	}))
	defer srv.Close()
	w.svc.crmPush = &pushChannel{enabled: true, baseURL: srv.URL, client: srv.Client(), repo: w.svc.repo, log: w.svc.log}

	cid := w.customer(t, "9000007110", 0)
	const tok = "ExponentPushToken[cccccccccccccccccccccc]"
	if err := w.svc.registerPushDevice(ctx, cid, pushRegisterInput{Token: tok, Platform: "android", Provider: "expo"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	label := "Milk gold-500ml 500ml" + crmLabelledSuffix
	params := map[string]string{"LABELLED_PRODUCT": label, "ORDER_ID": "ord_push_1"}

	// (1) D-06 (primary push): the device gets the rendered body and a deep link.
	if st, g := w.svc.crmDispatch(ctx, "D-06", cid, params); st != "SENT" {
		t.Fatalf("D-06: %q %q", st, g)
	}
	var row crmDispatchRow
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "D-06"}}).Decode(&row); err != nil {
		t.Fatalf("row: %v", err)
	}
	if row.Channel != "inapp+push" || row.Intended != "push" {
		t.Fatalf("D-06 row: channel=%q intended=%q", row.Channel, row.Intended)
	}
	if len(got) != 1 || len(got[0].msgs) != 1 || got[0].auth != "" {
		t.Fatalf("expo calls: %+v", got)
	}
	m := got[0].msgs[0]
	if m["to"] != tok || m["title"] != "PYAAS" || m["channelId"] != "delivery" ||
		m["body"] != "Delivered ✅ "+label+". Enjoy! Tap to rate." {
		t.Fatalf("expo message: %v", m)
	}
	data, _ := m["data"].(map[string]any)
	if data["href"] != "/order/ord_push_1" || data["trigger_id"] != "D-06" || data["order_id"] != "ord_push_1" || data["cta"] != "rate" {
		t.Fatalf("expo data: %v", data)
	}

	// (2) A dead device: Expo says DeviceNotRegistered, the token is dropped,
	// the row shows the inbox only (whatsapp/sms fallbacks are unplugged).
	answer = `{"data":[{"status":"error","message":"gone","details":{"error":"DeviceNotRegistered"}}]}`
	params["ETA"] = "today by 7 am"
	if st, _ := w.svc.crmDispatch(ctx, "D-01", cid, params); st != "SENT" {
		t.Fatalf("D-01: %q", st)
	}
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "D-01"}}).Decode(&row); err != nil {
		t.Fatalf("row: %v", err)
	}
	if row.Channel != "inapp" {
		t.Fatalf("D-01 after a dead device must be inbox only, got %q", row.Channel)
	}
	if n, _ := w.db.Collection(collPushDevices).CountDocuments(ctx, bson.D{{Key: "token", Value: tok}}); n != 0 {
		t.Fatal("DeviceNotRegistered must drop the token")
	}

	// (3) No device at all: no call leaves the building, the chain falls
	// through immediately, the inbox still carries the message.
	calls := len(got)
	if st, _ := w.svc.crmDispatch(ctx, "B-01", cid, nil); st != "SENT" {
		t.Fatalf("B-01: %q", st)
	}
	if len(got) != calls {
		t.Fatal("a member without a device must not produce an Expo call")
	}
	if err := w.db.Collection(collCRMDispatch).FindOne(ctx, bson.D{{Key: "consumer_id", Value: cid}, {Key: "trigger_id", Value: "B-01"}}).Decode(&row); err != nil {
		t.Fatalf("row: %v", err)
	}
	if row.Channel != "inapp" {
		t.Fatalf("B-01 without a device: %q", row.Channel)
	}

	// (4) The access token rides as a Bearer header; a provider 5xx is
	// transient (no fallback, inbox only); a non-expo token is never sent.
	w.svc.crmPush.accessToken = "expo-secret"
	answer = `{"data":[{"status":"ok","id":"r2"}]}`
	other := w.customer(t, "9000007111", 0)
	if err := w.svc.registerPushDevice(ctx, other, pushRegisterInput{Token: "ExpoPushToken[dddddddddddddddddddddd]", Platform: "ios", Provider: "expo"}); err != nil {
		t.Fatalf("register other: %v", err)
	}
	if err := w.svc.registerPushDevice(ctx, other, pushRegisterInput{Token: "fcm-raw-token", Platform: "android", Provider: "fcm"}); err != nil {
		t.Fatalf("register fcm: %v", err)
	}
	if st, _ := w.svc.crmDispatch(ctx, "D-06", other, params); st != "SENT" {
		t.Fatalf("D-06 other: %q", st)
	}
	last := got[len(got)-1]
	if last.auth != "Bearer expo-secret" || len(last.msgs) != 1 || last.msgs[0]["to"] != "ExpoPushToken[dddddddddddddddddddddd]" {
		t.Fatalf("bearer/token filtering: %+v", last)
	}
	tr := crmTrigger{ID: "D-01", Category: "service_implicit", Section: "D", Delivery: crmDelivery{Primary: "push", Fallback: []crmFallback{{Channel: "sms"}}}}
	sms, smsHits := testSMSChannel(t, map[string]string{"D-01": "1207x"}, func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(`{"type":"success"}`))
	})
	dead := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(503) }))
	defer dead.Close()
	bound := (&pushChannel{enabled: true, baseURL: dead.URL, client: dead.Client(), repo: w.svc.repo}).bind(other)
	delivered := crmDeliverExternal(ctx, testQuietLog, "919000007111", tr, crmTemplate{EN: "Order confirmed", HI: "Order confirm"},
		map[string]string{}, map[string]crmTransport{"push": bound, "sms": sms})
	if len(delivered) != 0 || *smsHits != 0 {
		t.Fatalf("a transient push failure must not fall back to sms: %v hits=%d", delivered, *smsHits)
	}
}

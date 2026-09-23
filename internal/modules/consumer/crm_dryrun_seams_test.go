package consumer

// The dry-run seams: CRM_MSG91_BASE_URL, CRM_WA_BASE_URL and EXPO_PUSH_BASE_URL
// each replace one provider's origin and keep its path, so a local stub can
// capture exactly what production would send. Unset, every transport still
// points at the real provider.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run DryRun -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/push"
)

// stubProvider is one local capture server: every request's method, path,
// headers and body, answered with whatever the test says the provider says.
type stubProvider struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []stubHit
}

type stubHit struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

func newStubProvider(t *testing.T, answer string) *stubProvider {
	t.Helper()
	p := &stubProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			buf := make([]byte, 64<<10)
			n, _ := r.Body.Read(buf)
			body = buf[:n]
		}
		p.mu.Lock()
		p.hits = append(p.hits, stubHit{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *stubProvider) only(t *testing.T) stubHit {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.hits) != 1 {
		t.Fatalf("stub hits: %d want exactly 1: %+v", len(p.hits), p.hits)
	}
	return p.hits[0]
}

func clearDryRunEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CRM_MSG91_AUTHKEY", "CRM_MSG91_SENDER", "CRM_DLT_TEMPLATE_IDS", "CRM_MSG91_BASE_URL",
		"CRM_WA_TOKEN", "CRM_WA_PHONE_ID", "CRM_WA_TEMPLATE_NAMES", "CRM_WA_BASE_URL",
		"EXPO_PUSH_ENABLED", "EXPO_ACCESS_TOKEN", "EXPO_PUSH_BASE_URL",
	} {
		t.Setenv(k, "")
	}
}

// With the overrides unset every transport points at the real provider: the
// keys-unset build is byte-identical to before the seams existed.
func TestDryRunOverridesUnsetKeepRealOrigins(t *testing.T) {
	clearDryRunEnv(t)
	if got := newSMSChannel(testQuietLog).baseURL; got != "https://control.msg91.com/api/v5/flow/" {
		t.Fatalf("sms baseURL: %q", got)
	}
	if got := newWhatsAppChannel(testQuietLog).baseURL; got != "https://graph.facebook.com/v19.0" {
		t.Fatalf("whatsapp baseURL: %q", got)
	}
	if got := crmProviderOrigin("CRM_MSG91_BASE_URL", crmMSG91Origin); got != crmMSG91Origin {
		t.Fatalf("origin with the key unset: %q", got)
	}
	t.Setenv("CRM_MSG91_BASE_URL", "  http://127.0.0.1:9009/  ")
	if got := crmProviderOrigin("CRM_MSG91_BASE_URL", crmMSG91Origin); got != "http://127.0.0.1:9009" {
		t.Fatalf("origin is trimmed of space and trailing slash: %q", got)
	}
}

// The W-01 flow payload, built by the real channel constructor from env alone,
// lands on the stub at MSG91's flow path with the production headers and body.
func TestDryRunMSG91BaseURLCapturesW01Flow(t *testing.T) {
	clearDryRunEnv(t)
	stub := newStubProvider(t, `{"type":"success","request_id":"dry-run"}`)
	t.Setenv("CRM_MSG91_AUTHKEY", "dry-run-key")
	t.Setenv("CRM_MSG91_SENDER", "PYAASD")
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-01":"1207160000000000001"}`)
	t.Setenv("CRM_MSG91_BASE_URL", stub.srv.URL+"/")

	ch := newSMSChannel(testQuietLog)
	if !ch.Enabled() {
		t.Fatal("sms channel must be enabled from env")
	}
	tr := crmTrigger{ID: "W-01", Category: "service_implicit"}
	tpl := crmTemplate{HI: "Ho gaya. Kal subah 7 baje tak 500 ml Parag Full Cream - muft."}
	if err := ch.deliver(context.Background(), "919000007106", tr, tpl, map[string]string{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	hit := stub.only(t)
	if hit.Method != http.MethodPost || hit.Path != "/api/v5/flow/" {
		t.Fatalf("stub saw %s %s, want POST /api/v5/flow/", hit.Method, hit.Path)
	}
	if hit.Header.Get("authkey") != "dry-run-key" || hit.Header.Get("content-type") != "application/json" {
		t.Fatalf("headers: %v", hit.Header)
	}
	var got struct {
		TemplateID string              `json:"template_id"`
		Sender     string              `json:"sender"`
		Recipients []map[string]string `json:"recipients"`
	}
	if err := json.Unmarshal(hit.Body, &got); err != nil {
		t.Fatalf("body %s: %v", hit.Body, err)
	}
	if got.TemplateID != "1207160000000000001" || got.Sender != "PYAASD" || len(got.Recipients) != 1 ||
		got.Recipients[0]["mobiles"] != "919000007106" || got.Recipients[0]["time"] != crmDLTDeliveryBy {
		t.Fatalf("flow payload: %s", hit.Body)
	}
}

// End to end: an enrolment whose W-01 the worker routes to SMS (WhatsApp and
// push unplugged, so the fallback runs at once) reaches the stub through the
// service exactly as production would send it, and the row reads SENT.
func TestDryRunW01ReachesStubThroughTheWorker(t *testing.T) {
	clearDryRunEnv(t)
	stub := newStubProvider(t, `{"type":"success","request_id":"dry-run"}`)
	t.Setenv("CRM_MSG91_AUTHKEY", "dry-run-key")
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-01":"1207160000000000001"}`)
	t.Setenv("CRM_MSG91_BASE_URL", stub.srv.URL)
	w, done := newChainWorld(t) // newService reads the env above
	defer done()
	ctx := context.Background()

	res, err := w.svc.crmEnrol(ctx, "dry-run-operator", crmEnrolInput{
		Phone: "9000007106", Name: "Dry Run Household", Line1: "Flat 2, Stub Tower",
		Pincode: "226030", Lat: 26.7725, Lng: 81.0150,
	})
	if err != nil {
		t.Fatalf("crmEnrol: %v", err)
	}
	cid, _ := primitive.ObjectIDFromHex(res.ConsumerID)
	w.svc.crmProcessEvents(ctx)
	if got := crmDispatchStatuses(t, w.db, cid, "W-01"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("W-01 after the worker: %v", got)
	}
	hit := stub.only(t)
	if hit.Method != http.MethodPost || hit.Path != "/api/v5/flow/" || hit.Header.Get("authkey") != "dry-run-key" {
		t.Fatalf("stub saw %s %s (authkey %q)", hit.Method, hit.Path, hit.Header.Get("authkey"))
	}
	var got struct {
		TemplateID string              `json:"template_id"`
		Recipients []map[string]string `json:"recipients"`
	}
	if err := json.Unmarshal(hit.Body, &got); err != nil {
		t.Fatalf("body %s: %v", hit.Body, err)
	}
	if got.TemplateID != "1207160000000000001" || len(got.Recipients) != 1 || got.Recipients[0]["mobiles"] != "919000007106" {
		t.Fatalf("W-01 flow payload: %s", hit.Body)
	}
	// A second worker turn sends nothing more.
	w.svc.crmProcessEvents(ctx)
	stub.only(t)
}

// The WhatsApp channel built from env posts to the stub at the Graph path,
// version and phone-number id included.
func TestDryRunWABaseURLKeepsGraphPath(t *testing.T) {
	clearDryRunEnv(t)
	stub := newStubProvider(t, `{"messages":[{"id":"wamid.dry"}]}`)
	t.Setenv("CRM_WA_TOKEN", "wa-token")
	t.Setenv("CRM_WA_PHONE_ID", "PHONE42")
	t.Setenv("CRM_WA_TEMPLATE_NAMES", `{"W-01":"pyaas_w01_welcome"}`)
	t.Setenv("CRM_WA_BASE_URL", stub.srv.URL)

	ch := newWhatsAppChannel(testQuietLog)
	if !ch.Enabled() {
		t.Fatal("whatsapp channel must be enabled from env")
	}
	tr := crmTrigger{ID: "W-01", Category: "service_implicit"}
	if err := ch.deliver(context.Background(), "919000007106", tr, crmTemplate{EN: "You're set."}, nil); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	hit := stub.only(t)
	if hit.Method != http.MethodPost || hit.Path != "/v19.0/PHONE42/messages" || hit.Header.Get("Authorization") != "Bearer wa-token" {
		t.Fatalf("stub saw %s %s (auth %q)", hit.Method, hit.Path, hit.Header.Get("Authorization"))
	}
}

// The push transport built from env posts to the stub at Expo's push path.
func TestDryRunExpoBaseURLKeepsPushPath(t *testing.T) {
	clearDryRunEnv(t)
	stub := newStubProvider(t, `{"data":[{"status":"ok","id":"r1"}]}`)
	t.Setenv("EXPO_PUSH_ENABLED", "true")
	t.Setenv("EXPO_PUSH_BASE_URL", stub.srv.URL)

	ch := newPushChannel(testQuietLog, nil)
	if !ch.Enabled() {
		t.Fatal("push channel must be enabled from env")
	}
	tickets, err := ch.expo.Send(context.Background(), []push.Message{{To: "ExponentPushToken[dry]", Body: "hi"}})
	// The stub first: a broken seam would have sent this to the real exp.host.
	hit := stub.only(t)
	if hit.Method != http.MethodPost || hit.Path != "/--/api/v2/push/send" {
		t.Fatalf("stub saw %s %s", hit.Method, hit.Path)
	}
	if err != nil || len(tickets) != 1 || !tickets[0].OK {
		t.Fatalf("send: %v %+v", err, tickets)
	}
}

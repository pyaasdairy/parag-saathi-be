package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Test transports NEVER touch the real providers — every channel points its
// baseURL at httptest (the field exists precisely so tests can, unlike the
// const-endpoint legacy msg91 client).

func testSMSChannel(t *testing.T, dlt map[string]string, h http.HandlerFunc) (*smsChannel, *int) {
	t.Helper()
	m := make(map[string]crmDLTTemplateID, len(dlt))
	for k, v := range dlt {
		m[k] = crmDLTTemplateID{any: v} // plain-string form
	}
	return testSMSChannelDLT(t, m, h)
}

func testSMSChannelDLT(t *testing.T, dlt map[string]crmDLTTemplateID, h http.HandlerFunc) (*smsChannel, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return &smsChannel{authKey: "sms-key", sender: "PYAASD", baseURL: srv.URL, client: srv.Client(), dlt: dlt}, &hits
}

func testWAChannel(t *testing.T, names map[string]string, h http.HandlerFunc) (*whatsappChannel, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return &whatsappChannel{token: "wa-token", phoneID: "PHONE42", baseURL: srv.URL, client: srv.Client(), names: names}, &hits
}

var testQuietLog = slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// The delivery routing block must parse for every trigger — it is Phase B's
// channel-priority source of truth (was silently dropped before this field).
func TestCRMDeliveryConfigParses(t *testing.T) {
	c := crmConfigLoad()
	w01 := c.Triggers["W-01"]
	if w01.Delivery.Primary != "whatsapp" {
		t.Fatalf("W-01 primary: %q", w01.Delivery.Primary)
	}
	if len(w01.Delivery.Parallel) != 1 || w01.Delivery.Parallel[0] != "push" {
		t.Fatalf("W-01 parallel: %v", w01.Delivery.Parallel)
	}
	if len(w01.Delivery.Fallback) != 1 || w01.Delivery.Fallback[0].Channel != "sms" {
		t.Fatalf("W-01 fallback: %+v", w01.Delivery.Fallback)
	}
	if fb := c.Triggers["W-07"].Delivery.Fallback; len(fb) != 1 || fb[0].Channel != "sms" || fb[0].If != "not_read" {
		t.Fatalf("W-07 fallback: %+v", fb)
	}
	// Every customer-facing W trigger routes over a known primary channel.
	for id, tr := range c.Triggers {
		if tr.Section != "W" || tr.Category == "internal" {
			continue
		}
		switch tr.Delivery.Primary {
		case "whatsapp", "push":
		default:
			t.Fatalf("trigger %s: unexpected primary channel %q", id, tr.Delivery.Primary)
		}
	}
}

// MSG91 Flow payload: authkey header, DLT template id, sender header, msisdn,
// and the ROMAN body's tokens as lowercase named variables.
func TestCRMSMSFlowPayload(t *testing.T) {
	var got struct {
		TemplateID string              `json:"template_id"`
		Sender     string              `json:"sender"`
		ShortURL   string              `json:"short_url"`
		Recipients []map[string]string `json:"recipients"`
	}
	var gotAuth string
	ch, _ := testSMSChannel(t, map[string]string{"W-06": "1207160000000012345"}, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authkey")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"type":"success","request_id":"x"}`))
	})
	tr := crmTrigger{ID: "W-06", Category: "service_implicit"}
	tpl := crmTemplate{
		EN:    "Recharge Rs500 by [DATE] — [LINK]",
		HI:    "[DATE] tak Rs500 recharge karein — {LINK}",
		HIDev: "देवनागरी [DATE] — [LINK]", // present, but SMS must never route it
	}
	err := ch.deliver(context.Background(), "919876543210", tr, tpl, map[string]string{"DATE": "28 Aug", "LINK": "https://pyaas.app"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if gotAuth != "sms-key" {
		t.Fatalf("authkey header: %q", gotAuth)
	}
	if got.TemplateID != "1207160000000012345" || got.Sender != "PYAASD" {
		t.Fatalf("template/sender: %q %q", got.TemplateID, got.Sender)
	}
	if len(got.Recipients) != 1 {
		t.Fatalf("recipients: %+v", got.Recipients)
	}
	rec := got.Recipients[0]
	if rec["mobiles"] != "919876543210" || rec["date"] != "28 Aug" || rec["link"] != "https://pyaas.app" {
		t.Fatalf("recipient variables: %+v", rec)
	}
	for k, v := range rec {
		if crmHasDevanagari(v) {
			t.Fatalf("devanagari leaked into SMS variable %s: %q", k, v)
		}
	}
}

// A trigger with no mapped DLT template id must never attempt SMS — sending
// unregistered content is a TRAI violation, so the channel reports itself
// unavailable and the provider is never contacted.
func TestCRMSMSUnmappedTemplateSuppressed(t *testing.T) {
	ch, hits := testSMSChannel(t, map[string]string{"W-06": "1207x"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "W-07", Category: "service_implicit", Delivery: crmDelivery{Primary: "sms"}}
	tpl := crmTemplate{EN: "expired", HI: "khatam"}
	if err := ch.available(tr, tpl); err == nil || !strings.Contains(err.Error(), "no DLT template id mapped") {
		t.Fatalf("unmapped trigger must be unavailable with a clear reason, got %v", err)
	}
	delivered := crmDeliverExternal(context.Background(), testQuietLog, "919876543210", tr, tpl,
		map[string]string{}, map[string]crmTransport{"sms": ch})
	if len(delivered) != 0 {
		t.Fatalf("delivered over unmapped sms: %v", delivered)
	}
	if *hits != 0 {
		t.Fatalf("provider contacted %d times for unmapped template", *hits)
	}
}

// Promotional SMS fails CLOSED: the G4 DND scrub has no infrastructure, and a
// telecom channel may not skip it.
func TestCRMSMSPromotionalFailsClosed(t *testing.T) {
	ch, hits := testSMSChannel(t, map[string]string{"W-03b": "1207y"}, func(w http.ResponseWriter, r *http.Request) {})
	tr := crmTrigger{ID: "W-03b", Category: "promotional"}
	if err := ch.available(tr, crmTemplate{EN: "offer"}); err == nil || !strings.Contains(err.Error(), "DND") {
		t.Fatalf("promotional sms must fail closed on the DND scrub, got %v", err)
	}
	if *hits != 0 {
		t.Fatal("provider contacted for a promotional SMS")
	}
}

// An SMS variable carrying Devanagari aborts before the provider is reached —
// the roman-only rule is structural, not advisory.
func TestCRMSMSRejectsDevanagariVariable(t *testing.T) {
	ch, hits := testSMSChannel(t, map[string]string{"W-06": "1207z"}, func(w http.ResponseWriter, r *http.Request) {})
	tr := crmTrigger{ID: "W-06", Category: "service_implicit"}
	tpl := crmTemplate{HI: "[NAME] ji, recharge karein"}
	err := ch.deliver(context.Background(), "919876543210", tr, tpl, map[string]string{"NAME": "अनुराग"})
	if err == nil || !strings.Contains(err.Error(), "roman-only") {
		t.Fatalf("devanagari variable must abort the SMS, got %v", err)
	}
	if *hits != 0 {
		t.Fatal("provider contacted despite devanagari variable")
	}
}

// WhatsApp Cloud payload: Bearer auth, /{phone_id}/messages path, template
// name from the mapping, language "hi" when a Devanagari variant exists, and
// positional body parameters in order of appearance.
func TestCRMWhatsAppTemplatePayload(t *testing.T) {
	var gotPath, gotAuth string
	var got struct {
		Product  string `json:"messaging_product"`
		To       string `json:"to"`
		Type     string `json:"type"`
		Template struct {
			Name     string `json:"name"`
			Language struct {
				Code string `json:"code"`
			} `json:"language"`
			Components []struct {
				Type       string `json:"type"`
				Parameters []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parameters"`
			} `json:"components"`
		} `json:"template"`
	}
	ch, _ := testWAChannel(t, map[string]string{"W-06": "pyaas_w06_recharge"}, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"messaging_product":"whatsapp","messages":[{"id":"wamid.X"}]}`))
	})
	tr := crmTrigger{ID: "W-06", Category: "service_implicit"}
	tpl := crmTemplate{
		EN:    "Recharge by [DATE] — [LINK]",
		HI:    "[DATE] tak recharge — [LINK]",
		HIDev: "[DATE] तक Rs500 रिचार्ज करें — [LINK]",
	}
	if err := ch.deliver(context.Background(), "919876543210", tr, tpl, map[string]string{"DATE": "28 Aug", "LINK": "https://pyaas.app"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if gotPath != "/PHONE42/messages" {
		t.Fatalf("path: %q", gotPath)
	}
	if gotAuth != "Bearer wa-token" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if got.Product != "whatsapp" || got.To != "919876543210" || got.Type != "template" {
		t.Fatalf("envelope: %+v", got)
	}
	if got.Template.Name != "pyaas_w06_recharge" {
		t.Fatalf("template name: %q", got.Template.Name)
	}
	if got.Template.Language.Code != "hi" {
		t.Fatalf("devanagari variant must ride language hi, got %q", got.Template.Language.Code)
	}
	if len(got.Template.Components) != 1 || got.Template.Components[0].Type != "body" {
		t.Fatalf("components: %+v", got.Template.Components)
	}
	ps := got.Template.Components[0].Parameters
	if len(ps) != 2 || ps[0].Text != "28 Aug" || ps[1].Text != "https://pyaas.app" {
		t.Fatalf("positional params wrong: %+v", ps)
	}
}

// Business-initiated WhatsApp REQUIRES a pre-approved template — an unmapped
// trigger never reaches Meta.
func TestCRMWhatsAppUnmappedTemplateSuppressed(t *testing.T) {
	ch, hits := testWAChannel(t, nil, func(w http.ResponseWriter, r *http.Request) {})
	tr := crmTrigger{ID: "W-01", Category: "service_implicit", Delivery: crmDelivery{Primary: "whatsapp"}}
	if err := ch.available(tr, crmTemplate{EN: "hi"}); err == nil || !strings.Contains(err.Error(), "pre-approved") {
		t.Fatalf("unmapped WA trigger must be unavailable, got %v", err)
	}
	delivered := crmDeliverExternal(context.Background(), testQuietLog, "919876543210", tr, crmTemplate{EN: "hi"},
		nil, map[string]crmTransport{"whatsapp": ch})
	if len(delivered) != 0 || *hits != 0 {
		t.Fatalf("unmapped WA template reached the provider: delivered=%v hits=%d", delivered, *hits)
	}
}

// Fallback order: a DEFINITIVE primary rejection (4xx) falls back to the next
// configured channel; the row records what actually delivered.
func TestCRMFallbackOnDefinitiveFailure(t *testing.T) {
	wa, waHits := testWAChannel(t, map[string]string{"W-07": "pyaas_w07"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"template paused"}}`))
	})
	sms, smsHits := testSMSChannel(t, map[string]string{"W-07": "1207a"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "W-07", Category: "service_implicit",
		Delivery: crmDelivery{Primary: "whatsapp", Fallback: []crmFallback{{Channel: "sms", At: "PT4H", If: "not_read"}}}}
	tpl := crmTemplate{EN: "Your pack expired. Nothing has been charged.", HI: "Koi shulk nahi laga hai."}
	delivered := crmDeliverExternal(context.Background(), testQuietLog, "919876543210", tr, tpl,
		nil, map[string]crmTransport{"whatsapp": wa, "sms": sms})
	if len(delivered) != 1 || delivered[0] != "sms" {
		t.Fatalf("fallback must deliver over sms, got %v", delivered)
	}
	if *waHits != 1 || *smsHits != 1 {
		t.Fatalf("hit counts: wa=%d sms=%d", *waHits, *smsHits)
	}
}

// A primary whose channel is UNAVAILABLE (keys absent → not in the transports
// map) skips straight to the fallback.
func TestCRMFallbackOnUnavailablePrimary(t *testing.T) {
	sms, smsHits := testSMSChannel(t, map[string]string{"W-07": "1207a"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "W-07", Category: "service_implicit",
		Delivery: crmDelivery{Primary: "whatsapp", Fallback: []crmFallback{{Channel: "sms"}}}}
	delivered := crmDeliverExternal(context.Background(), testQuietLog, "919876543210", tr,
		crmTemplate{HI: "roman"}, nil, map[string]crmTransport{"sms": sms})
	if len(delivered) != 1 || delivered[0] != "sms" || *smsHits != 1 {
		t.Fatalf("unavailable primary must fall back to sms: %v (hits %d)", delivered, *smsHits)
	}
}

// A TRANSIENT primary failure (network error — outcome unknown) must NOT fall
// back: the provider may have accepted the message, and a fallback would risk
// double-delivery. The claim's next-IST-day granularity is the retry.
func TestCRMTransientFailureNoFallback(t *testing.T) {
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close() // connection refused → client.Do error → transient
	wa := &whatsappChannel{token: "t", phoneID: "P", baseURL: deadURL, client: http.DefaultClient,
		names: map[string]string{"W-07": "pyaas_w07"}}
	sms, smsHits := testSMSChannel(t, map[string]string{"W-07": "1207a"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "W-07", Category: "service_implicit",
		Delivery: crmDelivery{Primary: "whatsapp", Fallback: []crmFallback{{Channel: "sms"}}}}
	delivered := crmDeliverExternal(context.Background(), testQuietLog, "919876543210", tr,
		crmTemplate{EN: "body"}, nil, map[string]crmTransport{"whatsapp": wa, "sms": sms})
	if len(delivered) != 0 {
		t.Fatalf("transient failure must not fall back, delivered %v", delivered)
	}
	if *smsHits != 0 {
		t.Fatal("sms fallback attempted after a transient whatsapp failure")
	}
	// And the classification itself is pinned:
	err := wa.deliver(context.Background(), "919876543210", tr, crmTemplate{EN: "body"}, nil)
	if !crmIsTransient(err) {
		t.Fatalf("network error must classify transient, got %v", err)
	}
	// A provider 5xx is transient too.
	wa5, _ := testWAChannel(t, map[string]string{"W-07": "x"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	})
	if err := wa5.deliver(context.Background(), "919876543210", tr, crmTemplate{EN: "body"}, nil); !crmIsTransient(err) {
		t.Fatalf("5xx must classify transient, got %v", err)
	}
	// MSG91's "type":"error" with HTTP 200 is a DEFINITIVE rejection.
	smsErr, _ := testSMSChannel(t, map[string]string{"W-07": "x"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"error","message":"invalid template"}`))
	})
	if err := smsErr.deliver(context.Background(), "919876543210", tr, crmTemplate{EN: "body"}, nil); err == nil || crmIsTransient(err) {
		t.Fatalf("msg91 body error must be definitive, got %v", err)
	}
}

// Parallel channels are best-effort extras: they ride alongside the primary
// and never trigger fallback (B-02: primary sms, parallel whatsapp).
func TestCRMParallelChannels(t *testing.T) {
	sms, _ := testSMSChannel(t, map[string]string{"B-02": "1207b"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"success"}`))
	})
	wa, _ := testWAChannel(t, map[string]string{"B-02": "pyaas_b02"}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"messages":[{"id":"wamid.Y"}]}`))
	})
	tr := crmTrigger{ID: "B-02", Category: "service_implicit",
		Delivery: crmDelivery{Primary: "sms", Parallel: []string{"whatsapp"}}}
	delivered := crmDeliverExternal(context.Background(), testQuietLog, "919876543210", tr,
		crmTemplate{EN: "Low balance — recharge by 12 noon tomorrow."}, nil,
		map[string]crmTransport{"sms": sms, "whatsapp": wa})
	if len(delivered) != 2 || delivered[0] != "sms" || delivered[1] != "whatsapp" {
		t.Fatalf("parallel delivery: %v", delivered)
	}
}

// An unresolved token is a send error on BOTH channels — C-01's fail-loudly
// rule (crmRender leaves tokens visible; the channels refuse to ship them).
func TestCRMUnresolvedTokenAborts(t *testing.T) {
	sms, hits := testSMSChannel(t, map[string]string{"W-06": "x"}, func(w http.ResponseWriter, r *http.Request) {})
	tr := crmTrigger{ID: "W-06", Category: "service_implicit"}
	if err := sms.deliver(context.Background(), "919876543210", tr, crmTemplate{HI: "recharge by [DATE]"}, nil); err == nil ||
		!strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("unresolved token must abort, got %v", err)
	}
	if *hits != 0 {
		t.Fatal("provider contacted with an unresolved token")
	}
}

// The token class is case-insensitive (the shipped config uses lowercase
// tokens like [labelled_product] in T-W05 et al.), but the params lookup
// stays case-sensitive — matching crmRender's exact-key substitution.
func TestCRMLowercaseTokenExtracts(t *testing.T) {
	names, values, err := crmTokenValues("Delivered. [labelled_product] by [DATE].",
		map[string]string{"labelled_product": "Pyaas 20L Jar", "DATE": "28 Aug"})
	if err != nil || len(names) != 2 || names[0] != "labelled_product" || values[0] != "Pyaas 20L Jar" {
		t.Fatalf("lowercase token extraction: names=%v values=%v err=%v", names, values, err)
	}
	if _, _, err := crmTokenValues("[labelled_product]", map[string]string{"LABELLED_PRODUCT": "x"}); err == nil {
		t.Fatal("params lookup must stay case-sensitive (C-01 fail-loudly)")
	}
}

// CRM_DLT_TEMPLATE_IDS value forms. DLT approves the EN and Hindi bodies of a
// trigger as SEPARATE registrations, so a value is either a plain string (one
// id for whichever language is sent — the original format) or a per-language
// object {"en":…,"hi":…}. The tests below pin: plain strings keep working for
// both languages, objects resolve by the body language crmSMSBody picks, a
// missing language key suppresses like an unmapped trigger, and malformed
// values degrade exactly like malformed JSON always has.

// A MIXED map parses, and the plain-string form resolves the same id no matter
// which language body ends up being sent (today's behavior, unchanged).
func TestCRMDLTPlainStringResolvesBothLanguages(t *testing.T) {
	t.Setenv("CRM_DLT_TEMPLATE_IDS",
		`{"W-06":"1207plain","W-07":{"en":"1207en","hi":"1207hi"}}`)
	m := crmParseDLTMap(testQuietLog, "CRM_DLT_TEMPLATE_IDS")
	if m == nil {
		t.Fatal("mixed plain+object map must parse")
	}
	if got := m["W-06"].forLang("hi"); got != "1207plain" {
		t.Fatalf("plain string must cover hi, got %q", got)
	}
	if got := m["W-06"].forLang("en"); got != "1207plain" {
		t.Fatalf("plain string must cover en, got %q", got)
	}
	// And it rides all the way into the wire payload for both body languages.
	var gotID string
	ch, _ := testSMSChannelDLT(t, m, func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			TemplateID string `json:"template_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		gotID = p.TemplateID
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "W-06", Category: "service_implicit"}
	for _, tpl := range []crmTemplate{
		{HI: "recharge karein"}, // hi_roman body
		{EN: "please recharge"}, // en body
	} {
		if err := ch.available(tr, tpl); err != nil {
			t.Fatalf("plain-string mapping must be available (%+v): %v", tpl, err)
		}
		if err := ch.deliver(context.Background(), "919876543210", tr, tpl, nil); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		if gotID != "1207plain" {
			t.Fatalf("template_id: %q", gotID)
		}
	}
}

// The object form resolves the id registered for the language of the body the
// EXISTING selection picks: hi_roman body → "hi" id, en body → "en" id.
func TestCRMDLTObjectFormResolvesPerLanguage(t *testing.T) {
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-07":{"en":"1207en","hi":"1207hi"}}`)
	m := crmParseDLTMap(testQuietLog, "CRM_DLT_TEMPLATE_IDS")
	var gotID string
	ch, _ := testSMSChannelDLT(t, m, func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			TemplateID string `json:"template_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		gotID = p.TemplateID
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "W-07", Category: "service_implicit"}
	// hi_roman present → hi body → hi id (even though en is also mapped).
	hiTpl := crmTemplate{EN: "expired", HI: "khatam ho gaya"}
	if err := ch.deliver(context.Background(), "919876543210", tr, hiTpl, nil); err != nil {
		t.Fatalf("deliver hi: %v", err)
	}
	if gotID != "1207hi" {
		t.Fatalf("hi body must ride the hi id, got %q", gotID)
	}
	// en-only template → en body → en id.
	enTpl := crmTemplate{EN: "your pack expired"}
	if err := ch.deliver(context.Background(), "919876543210", tr, enTpl, nil); err != nil {
		t.Fatalf("deliver en: %v", err)
	}
	if gotID != "1207en" {
		t.Fatalf("en body must ride the en id, got %q", gotID)
	}
	// Unknown language keys in the object are tolerated and ignored.
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-07":{"en":"1207en","mr":"1207mr"}}`)
	m2 := crmParseDLTMap(testQuietLog, "CRM_DLT_TEMPLATE_IDS")
	if m2 == nil || m2["W-07"].forLang("en") != "1207en" {
		t.Fatalf("unknown keys must be ignored, not fatal: %+v", m2)
	}
}

// An object mapping that lacks the SELECTED language is unmapped for that
// send: same refusal, same logged reason, provider never contacted — exactly
// like a trigger absent from the map.
func TestCRMDLTObjectMissingLanguageSuppressed(t *testing.T) {
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-07":{"en":"1207en"}}`)
	m := crmParseDLTMap(testQuietLog, "CRM_DLT_TEMPLATE_IDS")
	ch, hits := testSMSChannelDLT(t, m, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"success"}`))
	})
	tr := crmTrigger{ID: "W-07", Category: "service_implicit", Delivery: crmDelivery{Primary: "sms"}}
	tpl := crmTemplate{EN: "expired", HI: "khatam"} // hi_roman body selected → needs the "hi" id
	if err := ch.available(tr, tpl); err == nil || !strings.Contains(err.Error(), "no DLT template id mapped") {
		t.Fatalf("missing language key must suppress with the unmapped reason, got %v", err)
	}
	delivered := crmDeliverExternal(context.Background(), testQuietLog, "919876543210", tr, tpl,
		map[string]string{}, map[string]crmTransport{"sms": ch})
	if len(delivered) != 0 {
		t.Fatalf("delivered despite missing hi registration: %v", delivered)
	}
	if *hits != 0 {
		t.Fatalf("provider contacted %d times without a hi-language DLT id", *hits)
	}
	// The en body WOULD be available — the suppression is per-language, not
	// per-trigger.
	if err := ch.available(tr, crmTemplate{EN: "expired"}); err != nil {
		t.Fatalf("en body with an en id must stay available: %v", err)
	}
}

// A malformed VALUE (wrong JSON type) degrades exactly like malformed JSON
// always has: the whole map collapses to nil — log once, channel mappings
// disabled, never a panic or an unregistered send.
func TestCRMDLTMalformedValueDegrades(t *testing.T) {
	for _, raw := range []string{
		`{"W-06":123}`,                              // number, not string/object
		`{"W-06":["1207a"]}`,                        // array
		`{"W-06":{"en":123}}`,                       // object with non-string id
		`{"W-06":"ok","W-07":true}`,                 // one bad value poisons the map
		`{"W-06":{"en":"x","hi":{"nested":"bad"}}}`, // nested object id
	} {
		t.Setenv("CRM_DLT_TEMPLATE_IDS", raw)
		if m := crmParseDLTMap(testQuietLog, "CRM_DLT_TEMPLATE_IDS"); m != nil {
			t.Fatalf("malformed value %s must collapse the map to nil, got %v", raw, m)
		}
	}
	// A JSON null value was a tolerated no-op in the old map[string]string
	// format (entry resolves to "" → unmapped); it stays that way, never an
	// error and never an id.
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-06":null,"W-07":"1207a"}`)
	if m := crmParseDLTMap(testQuietLog, "CRM_DLT_TEMPLATE_IDS"); m == nil ||
		m["W-06"].forLang("en") != "" || m["W-06"].forLang("hi") != "" || m["W-07"].forLang("hi") != "1207a" {
		t.Fatalf("null value must stay a tolerated unmapped entry: %+v", m)
	}
	// And the channel built on top of it simply reports every trigger unmapped.
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"W-06":123}`)
	t.Setenv("CRM_MSG91_AUTHKEY", "k")
	ch := newSMSChannel(testQuietLog)
	err := ch.available(crmTrigger{ID: "W-06", Category: "service_implicit"}, crmTemplate{EN: "body"})
	if err == nil || !strings.Contains(err.Error(), "no DLT template id mapped") {
		t.Fatalf("malformed map must leave every trigger unmapped, got %v", err)
	}
}

// THE INERTNESS SEAM: with none of the Phase B env vars set, both channels are
// disabled (nil-receiver-safe), the service resolves zero transports, and the
// dispatcher's external block is unreachable — byte-identical to Phase A.
func TestCRMChannelsInertWithoutKeys(t *testing.T) {
	for _, k := range []string{"CRM_MSG91_AUTHKEY", "CRM_MSG91_SENDER", "CRM_DLT_TEMPLATE_IDS",
		"CRM_WA_TOKEN", "CRM_WA_PHONE_ID", "CRM_WA_TEMPLATE_NAMES"} {
		t.Setenv(k, "")
	}
	if newSMSChannel(nil).Enabled() {
		t.Fatal("sms channel must be disabled without CRM_MSG91_AUTHKEY")
	}
	if newWhatsAppChannel(nil).Enabled() {
		t.Fatal("whatsapp channel must be disabled without CRM_WA_TOKEN/CRM_WA_PHONE_ID")
	}
	var nilSMS *smsChannel
	var nilWA *whatsappChannel
	if nilSMS.Enabled() || nilWA.Enabled() {
		t.Fatal("nil receivers must report disabled, never panic")
	}
	s := &service{}
	if got := s.crmTransports(); got != nil {
		t.Fatalf("no keys → no transports, got %v", got)
	}
	// WA token alone is not enough — both keys or nothing.
	t.Setenv("CRM_WA_TOKEN", "half")
	if newWhatsAppChannel(nil).Enabled() {
		t.Fatal("whatsapp must stay disabled with only a token")
	}
	// A malformed mapping JSON degrades to no mappings, never a panic/send.
	t.Setenv("CRM_DLT_TEMPLATE_IDS", "{not json")
	if m := crmParseTemplateMap(testQuietLog, "CRM_DLT_TEMPLATE_IDS"); m != nil {
		t.Fatalf("malformed map must collapse to nil, got %v", m)
	}
}

package consumer

// CRM SMS caveat 1 (owner, 24 Sep): a provider registration belongs to ONE
// message body. B-06 ("money added") picks its body by the credited account:
// T-B05-REFUND for a top-up or refund, T-B05-PROMO for Pyaas credit. The
// DLT (and WhatsApp) mappings were keyed by trigger id alone, so a promo
// credit went out under the refund body's registration - the wrong words,
// and a DLT template mismatch. The mapping is now looked up by the template
// id first, then by the trigger id, and the trigger id only covers the
// trigger's own (primary) template: a variant body with no mapping of its
// own sends no SMS or WhatsApp (inbox, and push, only).
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27018 \
//	  go test ./internal/modules/consumer/ -run TemplateRegistration -v

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCRMTemplateRegistrationPerTemplate(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"type":"success"}`)) }
	b06 := func(tpl string) crmTrigger {
		return crmTrigger{ID: "B-06", Category: "transactional", Template: crmTemplateRef{Ref: tpl}}
	}
	body := crmTemplate{EN: "₹[X] added ([REASON])."}
	params := map[string]string{"X": "75", "REASON": "Welcome credit"}

	// Mapped by the trigger id only: the refund body (the trigger's own
	// template) sends; the promo body does not borrow the refund's id.
	sms, hits := testSMSChannel(t, map[string]string{"B-06": "flow-refund"}, ok)
	if err := sms.available(b06("T-B05-REFUND"), body); err != nil {
		t.Fatalf("the trigger's own template must use the trigger's id: %v", err)
	}
	if err := sms.available(b06("T-B05-PROMO"), body); err == nil {
		t.Fatalf("the promo body must not go out under the refund body's DLT id")
	}
	// Mapped by the template id: each body its own registration.
	sms2, _ := testSMSChannel(t, map[string]string{"B-06": "flow-refund", "T-B05-PROMO": "flow-promo"}, func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			TemplateID string `json:"template_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got.TemplateID != "flow-promo" {
			t.Errorf("promo SMS sent under %q, want flow-promo", got.TemplateID)
		}
		_, _ = w.Write([]byte(`{"type":"success"}`))
	})
	if err := sms2.available(b06("T-B05-PROMO"), body); err != nil {
		t.Fatalf("a promo mapping of its own: %v", err)
	}
	if err := sms2.deliver(context.Background(), "919000000001", b06("T-B05-PROMO"), body, params); err != nil {
		t.Fatalf("deliver promo: %v", err)
	}
	// A template-id key covers the template under any trigger id spelling.
	sms3, _ := testSMSChannel(t, map[string]string{"T-B05-REFUND": "flow-refund-by-template"}, ok)
	if err := sms3.available(b06("T-B05-REFUND"), body); err != nil {
		t.Fatalf("a mapping keyed by the template id: %v", err)
	}
	if got := sms3.dltFor(b06("T-B05-REFUND")).forLang("en"); got != "flow-refund-by-template" {
		t.Fatalf("template-keyed id %q", got)
	}
	// A plain trigger (one template) is unchanged: keyed by trigger id.
	if err := sms.available(crmTrigger{ID: "B-06"}, body); err != nil {
		t.Fatalf("an ad hoc trigger with no template keeps the trigger id: %v", err)
	}
	if *hits != 0 {
		t.Fatalf("availability checks sent %d SMS", *hits)
	}

	// WhatsApp's approved templates are per body too.
	wa, _ := testWAChannel(t, map[string]string{"B-06": "pyaas_money_added_refund"}, ok)
	if err := wa.available(b06("T-B05-REFUND"), body); err != nil {
		t.Fatalf("whatsapp, the trigger's own template: %v", err)
	}
	if err := wa.available(b06("T-B05-PROMO"), body); err == nil {
		t.Fatalf("whatsapp must not send the promo body under the refund template")
	}
	wa2, _ := testWAChannel(t, map[string]string{"T-B05-PROMO": "pyaas_money_added_promo"}, ok)
	if err := wa2.available(b06("T-B05-PROMO"), body); err != nil {
		t.Fatalf("whatsapp, a promo template of its own: %v", err)
	}
}

// Through the worker: a Pyaas credit reaches the inbox with the promo body
// and no SMS, while a top-up sends its SMS under the B-06 mapping.
func TestCRMTemplateRegistrationPromoCreditSendsNoSMS(t *testing.T) {
	clearDryRunEnv(t)
	stub := newStubProvider(t, `{"type":"success","request_id":"dry-run"}`)
	t.Setenv("CRM_MSG91_AUTHKEY", "dry-run-key")
	t.Setenv("CRM_DLT_TEMPLATE_IDS", `{"B-06":"flow-refund"}`)
	t.Setenv("CRM_MSG91_BASE_URL", stub.srv.URL)
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000015301", 0)
	if _, err := w.svc.promoCredit(ctx, cid, 75, "promo_reg_1", "Welcome credit"); err != nil {
		t.Fatalf("promoCredit: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	rows := crmB06Rows(t, w, cid)
	if len(rows) != 1 || rows[0].Template != "T-B05-PROMO" {
		t.Fatalf("promo B-06 inbox: %+v", rows)
	}
	stub.mu.Lock()
	n := len(stub.hits)
	stub.mu.Unlock()
	if n != 0 {
		t.Fatalf("a promo credit went out by SMS under the refund body's id: %+v", stub.hits)
	}
	if got := crmDispatchStatuses(t, w.db, cid, "B-06"); len(got) != 1 || got[0] != "SENT" {
		t.Fatalf("promo B-06 dispatch: %v", got)
	}

	if _, err := w.svc.creditTopup(ctx, cid, 200, "razorpay", "topup_reg_1"); err != nil {
		t.Fatalf("creditTopup: %v", err)
	}
	w.svc.crmProcessEvents(ctx)
	hit := stub.only(t)
	var got struct {
		TemplateID string              `json:"template_id"`
		Recipients []map[string]string `json:"recipients"`
	}
	if err := json.Unmarshal(hit.Body, &got); err != nil || got.TemplateID != "flow-refund" ||
		len(got.Recipients) != 1 || got.Recipients[0]["x"] != "200" || got.Recipients[0]["reason"] != "recharge" {
		t.Fatalf("top-up B-06 SMS: %s", hit.Body)
	}
}

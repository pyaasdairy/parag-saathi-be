// CRM Phase B outbound transports — MSG91 DLT SMS and WhatsApp Cloud API.
//
// Both implement the crmChannel seam promised in crm_engine.go and stay INERT
// until their env keys exist: with CRM_MSG91_AUTHKEY / CRM_WA_TOKEN+CRM_WA_PHONE_ID
// unset the dispatcher resolves zero transports and behaves byte-identically to
// Phase A (in-app inbox only). The inbox channel always runs regardless.
//
// COMPLIANCE INVARIANTS (TRAI / Meta — non-negotiable):
//   - SMS content is the DLT-REGISTERED template held at MSG91; we only supply
//     variables. A trigger with no mapped DLT template id (CRM_DLT_TEMPLATE_IDS)
//     never attempts SMS — unregistered content is a TRAI violation, so the
//     channel reports itself unavailable with a logged reason instead. DLT
//     approves each language body as a SEPARATE registration, so a mapping may
//     be per-language ({"en":…,"hi":…}); the id is resolved for the language of
//     the body actually sent, and a missing language key is equally unmapped.
//   - SMS is ROMAN-ONLY: variables are drawn from the roman body (hi_roman,
//     else en) and any Devanagari in a variable value aborts the send.
//     WhatsApp carries the Devanagari rendering (hi_devanagari preferred).
//   - Promotional SMS FAILS CLOSED: the G4 DND scrub has no infrastructure yet
//     (crm_engine.go guard chain passes it structurally for the inbox), and a
//     telecom channel may not skip it — so category=="promotional" makes the
//     SMS transport unavailable. WhatsApp is an OTT channel outside the TRAI
//     DND registry; its promotional gates are the G2 explicit-consent guard
//     (enforced before any channel is reached) AND the G2b channel-granular
//     check (crmHasChannelConsent): the dispatcher hands a promotional trigger
//     to a transport only when the member's marketing_<channel> consent is
//     itself active and unexpired — an email-only opt-in never yields a promo
//     SMS or WhatsApp.
//   - Business-initiated WhatsApp requires a PRE-APPROVED template: a trigger
//     with no mapped template name (CRM_WA_TEMPLATE_NAMES) never attempts it.
//
// ERROR CONTRACT (inherits crm_engine.go §dispatch): deliver returns nil ONLY
// when the provider accepted the message. Failures split two ways:
//   - definitive (provider rejected: 4xx, or MSG91's "type":"error" body) →
//     the delivery chain may FALL BACK to the next configured channel;
//   - transient (transport error / 5xx — outcome unknown, wrapped with
//     errCRMTransient) → NO fallback (it could double-deliver a message the
//     provider actually accepted); the claim's next-IST-day granularity is the
//     retry, exactly as for every other send failure.
//
// Neither path panics, writes CRM state, or blocks the in-app channel.
package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	// MSG91 Flow API — templated (DLT) SMS with named variables. NOT the /otp
	// endpoint the login path uses: flow renders a registered campaign template.
	crmMSG91FlowEndpoint = "https://control.msg91.com/api/v5/flow/"
	// Meta Graph API base for the WhatsApp Cloud API (POST {phone_id}/messages).
	crmWAGraphBase = "https://graph.facebook.com/v19.0"
)

// errCRMTransient marks a send failure whose outcome is UNKNOWN (network error,
// provider 5xx). The delivery chain must not fall back past it — the provider
// may have accepted the message despite the error.
var errCRMTransient = errors.New("transient transport failure")

func crmIsTransient(err error) bool { return errors.Is(err, errCRMTransient) }

// ── Delivery routing config (the trigger JSON's `delivery` block) ───────────

// crmDelivery is a trigger's channel routing: one primary (with ordered
// fallbacks taken only on unavailability or definitive rejection) plus
// best-effort parallel channels. Channels Phase B does not ship (push, rcs,
// ai_call, human_call, admin_console, email) resolve to nothing and are
// skipped — the config keeps naming them so later phases need no config edit.
type crmDelivery struct {
	Primary  string        `json:"primary"`
	Parallel []string      `json:"parallel"`
	Fallback []crmFallback `json:"fallback"`
}

// crmFallback carries the config's timing/condition hints (after/at/if —
// "PT2H", "not_delivered", …). Phase B has no delivery-receipt store, so the
// dispatcher falls back IMMEDIATELY on a definitive primary failure instead of
// scheduling a conditional retry; the fields are parsed so the intent survives.
type crmFallback struct {
	Channel string `json:"channel"`
	After   string `json:"after"`
	At      string `json:"at"`
	If      string `json:"if"`
}

// ── Transport plumbing shared by both channels ──────────────────────────────

// crmTransport is a phone-number transport. Narrower than crmChannel: the
// dispatcher resolves the account's phone ONCE and shares it across the
// delivery chain (both channels also satisfy crmChannel for the seam's sake).
type crmTransport interface {
	Name() string
	// available reports why this transport cannot carry this trigger (keys,
	// mapping, category, body) — nil means deliver may be attempted.
	available(t crmTrigger, tpl crmTemplate) error
	deliver(ctx context.Context, phone string, t crmTrigger, tpl crmTemplate, params map[string]string) error
}

// crmTokenRe matches the config's runtime placeholders in both bracket styles
// ([DATE] and {DATE}) — same token grammar crmRender substitutes. The class is
// case-insensitive because the shipped config also uses lowercase tokens
// ([labelled_product]); lookup against params stays case-sensitive, matching
// crmRender's exact-key substitution.
var crmTokenRe = regexp.MustCompile(`[\[{]([A-Za-z][A-Za-z0-9_]*)[\]}]`)

// crmTokenValues extracts the template body's placeholders IN ORDER OF
// APPEARANCE (WhatsApp body params are positional) and resolves each against
// params. An unresolved token is a send error — C-01: a message that cannot
// resolve its values fails loudly, never half-renders (crmRender's contract).
func crmTokenValues(body string, params map[string]string) (names, values []string, err error) {
	seen := map[string]bool{}
	for _, m := range crmTokenRe.FindAllStringSubmatch(body, -1) {
		tok := m[1]
		if seen[tok] {
			continue
		}
		seen[tok] = true
		v, ok := params[tok]
		if !ok || v == "" {
			return nil, nil, fmt.Errorf("unresolved template token [%s]", tok)
		}
		names, values = append(names, tok), append(values, v)
	}
	return names, values, nil
}

func crmHasDevanagari(s string) bool {
	for _, r := range s {
		if r >= 0x0900 && r <= 0x097F {
			return true
		}
	}
	return false
}

// crmDLTTemplateID is one CRM_DLT_TEMPLATE_IDS value. DLT approves the EN and
// Hindi bodies of a trigger as SEPARATE template registrations, so a value is
// EITHER a plain string (one id covering whichever language body is sent —
// the original format, unchanged) OR an object {"en":"<id>","hi":"<id>"}
// carrying one id per registered language. Any other JSON type for the value
// is malformed and fails the WHOLE map's unmarshal — degrading exactly like
// malformed JSON always has (crmParseDLTMap: log once, channel mappings
// disabled). Unknown language keys inside the object form are tolerated and
// ignored, matching encoding/json's usual tolerance for extra keys.
type crmDLTTemplateID struct {
	any    string            // plain-string form
	byLang map[string]string // object form, keyed "en" / "hi"
}

func (d *crmDLTTemplateID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		d.any = s
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf(`DLT template id must be a string or {"en":"…","hi":"…"} object: %w`, err)
	}
	d.byLang = m
	return nil
}

// forLang resolves the DLT id registered for the body language SMS routes
// ("hi" for hi_roman, "en" for en). A plain-string mapping covers either
// language; an object mapping missing the selected language resolves to "" —
// unmapped, so available() suppresses the send exactly like an absent trigger.
func (d crmDLTTemplateID) forLang(lang string) string {
	if d.any != "" {
		return d.any
	}
	return d.byLang[lang]
}

// crmParseDLTMap reads CRM_DLT_TEMPLATE_IDS: {"<trigger-id>": <string-or-
// per-language-object>}. Same safety contract as crmParseTemplateMap —
// malformed JSON (including a value of the wrong type) degrades to a nil map:
// log once, every trigger reports "no template mapped", never a crash or an
// unregistered send.
func crmParseDLTMap(log *slog.Logger, env string) map[string]crmDLTTemplateID {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return nil
	}
	m := map[string]crmDLTTemplateID{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		if log != nil {
			log.Warn("crm: malformed template map env — channel mappings disabled", "env", env, "err", err)
		}
		return nil
	}
	return m
}

// crmParseTemplateMap reads a {"<trigger-id>":"<provider-template>"} JSON map
// from env. Malformed JSON degrades SAFELY: the map comes back empty, so every
// trigger reports "no template mapped" and delivery stays on the inbox —
// misconfiguration can never cause an unregistered send.
func crmParseTemplateMap(log *slog.Logger, env string) map[string]string {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return nil
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		if log != nil {
			log.Warn("crm: malformed template map env — channel mappings disabled", "env", env, "err", err)
		}
		return nil
	}
	return m
}

// crmDeliveryPhone resolves the MSISDN both providers want ("91"+10 digits,
// no plus) from the account's canonical "+91…" phone. The Play-review account
// is excluded — its fixed number must never receive live traffic.
func (s *service) crmDeliveryPhone(ctx context.Context, consumerID primitive.ObjectID) (string, error) {
	var a struct {
		Phone string `bson:"phone"`
	}
	if err := s.repo.accounts.FindOne(ctx, bson.D{{Key: "_id", Value: consumerID}},
		options.FindOne().SetProjection(bson.D{{Key: "phone", Value: 1}})).Decode(&a); err != nil {
		return "", fmt.Errorf("account phone lookup: %w", err)
	}
	digits := normalizePhone(a.Phone)
	if len(digits) != 10 {
		return "", fmt.Errorf("account has no deliverable 10-digit phone (%q)", a.Phone)
	}
	if digits == reviewPhone {
		return "", fmt.Errorf("play-review account — external sends disabled")
	}
	return "91" + digits, nil
}

// crmTransports resolves the ENABLED Phase B transports. With no keys set it
// returns nil, the dispatcher's external block short-circuits, and the binary
// behaves byte-identically to Phase A — the hard no-op invariant.
func (s *service) crmTransports() map[string]crmTransport {
	m := map[string]crmTransport{}
	if s.crmSMS.Enabled() {
		m["sms"] = s.crmSMS
	}
	if s.crmWA.Enabled() {
		m["whatsapp"] = s.crmWA
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// crmDeliverExternal runs one dispatch's telecom delivery chain and returns
// the names of the transports that ACCEPTED the message. Routing honours the
// trigger's delivery block: primary first; fallbacks only when the channel
// before them was unavailable or definitively rejected (never after a
// transient failure — unknown outcome, falling back could double-deliver);
// parallel channels best-effort, never triggering fallback. Channels not in
// the transports map (unshipped kinds, disabled keys) are skipped silently.
func crmDeliverExternal(ctx context.Context, log *slog.Logger, phone string, t crmTrigger, tpl crmTemplate, params map[string]string, transports map[string]crmTransport) []string {
	var delivered []string
	done := map[string]bool{}
	attempt := func(name, role string) (ok, transient bool) {
		if done[name] {
			return false, false
		}
		tr := transports[name]
		if tr == nil {
			return false, false
		}
		// Mark attempted regardless of outcome: a channel that failed in one
		// role (including a transient failure with an unknown outcome) must
		// never be re-attempted in another role — that could double-deliver.
		done[name] = true
		if err := tr.available(t, tpl); err != nil {
			if log != nil {
				log.Warn("crm: channel unavailable", "trigger", t.ID, "channel", name, "role", role, "reason", err.Error())
			}
			return false, false
		}
		if err := tr.deliver(ctx, phone, t, tpl, params); err != nil {
			if log != nil {
				log.Warn("crm: channel send failed", "trigger", t.ID, "channel", name, "role", role, "transient", crmIsTransient(err), "err", err)
			}
			return false, crmIsTransient(err)
		}
		delivered = append(delivered, name)
		return true, false
	}
	if p := t.Delivery.Primary; p != "" {
		ok, transient := attempt(p, "primary")
		if !ok && !transient {
			for _, fb := range t.Delivery.Fallback {
				fbOK, fbTransient := attempt(fb.Channel, "fallback")
				if fbOK || fbTransient {
					break
				}
			}
		}
	}
	for _, p := range t.Delivery.Parallel {
		attempt(p, "parallel")
	}
	return delivered
}

// ── SMS — MSG91 Flow API (DLT-registered campaign templates) ────────────────

type smsChannel struct {
	authKey string
	sender  string // 6-char DLT header (CRM_MSG91_SENDER, e.g. PYAASD)
	baseURL string // crmMSG91FlowEndpoint; a struct field so tests point it at httptest
	client  *http.Client
	dlt     map[string]crmDLTTemplateID // trigger id → DLT flow/template id(s) (CRM_DLT_TEMPLATE_IDS)
	log     *slog.Logger
}

var _ crmChannel = (*smsChannel)(nil)
var _ crmTransport = (*smsChannel)(nil)

// newSMSChannel builds the CRM SMS transport from env. Missing auth key →
// disabled (Enabled() false) and the dispatcher never resolves it.
func newSMSChannel(log *slog.Logger) *smsChannel {
	return &smsChannel{
		authKey: strings.TrimSpace(os.Getenv("CRM_MSG91_AUTHKEY")),
		sender:  strings.TrimSpace(os.Getenv("CRM_MSG91_SENDER")),
		baseURL: crmMSG91FlowEndpoint,
		client:  &http.Client{Timeout: 10 * time.Second},
		dlt:     crmParseDLTMap(log, "CRM_DLT_TEMPLATE_IDS"),
		log:     log,
	}
}

func (c *smsChannel) Name() string { return "sms" }

func (c *smsChannel) Enabled() bool { return c != nil && c.authKey != "" }

// crmSMSBody picks the ROMAN body SMS is allowed to carry: hi_roman first
// (the codebase's Hindi-first default — crmNotifyAdmins, lowstock), else en.
// hi_devanagari is NEVER a candidate here.
func crmSMSBody(tpl crmTemplate) string {
	if tpl.HI != "" {
		return tpl.HI
	}
	return tpl.EN
}

// crmSMSLang is the DLT-registration language key for the body crmSMSBody
// picks: hi_roman routes as "hi", the en fallback as "en". It mirrors — never
// alters — crmSMSBody's selection, so the two can never disagree.
func crmSMSLang(tpl crmTemplate) string {
	if tpl.HI != "" {
		return "hi"
	}
	return "en"
}

func (c *smsChannel) available(t crmTrigger, tpl crmTemplate) error {
	if !c.Enabled() {
		return fmt.Errorf("sms: channel not configured (CRM_MSG91_AUTHKEY unset)")
	}
	if t.Category == "promotional" {
		return fmt.Errorf("sms: G4 DND scrub not implemented — promotional SMS fails closed (TRAI)")
	}
	// Resolve the id for the LANGUAGE of the body we would send (hi_roman →
	// "hi", en → "en"): DLT registers each language body separately, so an id
	// approved for one language never covers the other. Missing = unmapped =
	// the same refusal as a trigger absent from the map entirely.
	if c.dlt[t.ID].forLang(crmSMSLang(tpl)) == "" {
		return fmt.Errorf("sms: no DLT template id mapped for trigger %s (CRM_DLT_TEMPLATE_IDS) — refusing unregistered content", t.ID)
	}
	if crmSMSBody(tpl) == "" {
		return fmt.Errorf("sms: template %s has no roman body", t.Template.String())
	}
	return nil
}

// deliver posts one flow message. The SMS text itself is the DLT template
// registered at MSG91 — we send ONLY variables, named after the CRM tokens
// lowercased (register flow variables as ##date##, ##link##, …).
func (c *smsChannel) deliver(ctx context.Context, phone string, t crmTrigger, tpl crmTemplate, params map[string]string) error {
	names, values, err := crmTokenValues(crmSMSBody(tpl), params)
	if err != nil {
		return fmt.Errorf("sms: %w", err)
	}
	rec := map[string]string{"mobiles": phone}
	for i, n := range names {
		if crmHasDevanagari(values[i]) {
			return fmt.Errorf("sms: variable %s carries Devanagari — SMS is roman-only", n)
		}
		rec[strings.ToLower(n)] = values[i]
	}
	payload := map[string]any{
		"template_id": c.dlt[t.ID].forLang(crmSMSLang(tpl)),
		"short_url":   "0",
		"recipients":  []any{rec},
	}
	if c.sender != "" {
		payload["sender"] = c.sender
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sms: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sms: build request: %w", err)
	}
	req.Header.Set("authkey", c.authKey)
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("sms: request failed: %v: %w", err, errCRMTransient)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("sms: provider unavailable (status %d): %s: %w", resp.StatusCode, rb, errCRMTransient)
	}
	// MSG91 answers {"type":"error"} with HTTP 200 — sniff the body, never
	// trust the status alone (same discipline as sms.MSG91.SendOTP).
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(rb), `"success"`) {
		return fmt.Errorf("sms: send rejected (status %d): %s", resp.StatusCode, rb)
	}
	return nil
}

// Send satisfies crmChannel (the engine seam): resolve the phone, then deliver.
func (c *smsChannel) Send(ctx context.Context, s *service, consumerID primitive.ObjectID, t crmTrigger, tpl crmTemplate, params map[string]string) error {
	if err := c.available(t, tpl); err != nil {
		return err
	}
	phone, err := s.crmDeliveryPhone(ctx, consumerID)
	if err != nil {
		return fmt.Errorf("sms: %w", err)
	}
	return c.deliver(ctx, phone, t, tpl, params)
}

// ── WhatsApp — Meta Cloud API (pre-approved business templates) ─────────────

type whatsappChannel struct {
	token   string
	phoneID string
	baseURL string // crmWAGraphBase; a struct field so tests point it at httptest
	client  *http.Client
	names   map[string]string // trigger id → approved template name (CRM_WA_TEMPLATE_NAMES)
	log     *slog.Logger
}

var _ crmChannel = (*whatsappChannel)(nil)
var _ crmTransport = (*whatsappChannel)(nil)

// newWhatsAppChannel builds the CRM WhatsApp transport from env. Missing token
// or phone-number id → disabled.
func newWhatsAppChannel(log *slog.Logger) *whatsappChannel {
	c := &whatsappChannel{
		token:   strings.TrimSpace(os.Getenv("CRM_WA_TOKEN")),
		phoneID: strings.TrimSpace(os.Getenv("CRM_WA_PHONE_ID")),
		baseURL: crmWAGraphBase,
		client:  &http.Client{Timeout: 10 * time.Second},
		names:   crmParseTemplateMap(log, "CRM_WA_TEMPLATE_NAMES"),
		log:     log,
	}
	// Half-configured (exactly one of token/phone-id set) degrades safely to
	// disabled, but say so once at startup so the operator gets a diagnostic
	// instead of silence. Fires only when a NEW env var is partially set, so
	// the keys-unset byte-identity invariant is untouched.
	if (c.token == "") != (c.phoneID == "") && log != nil {
		log.Warn("crm: whatsapp partially configured, channel disabled",
			"has_token", c.token != "", "has_phone_id", c.phoneID != "")
	}
	return c
}

func (c *whatsappChannel) Name() string { return "whatsapp" }

func (c *whatsappChannel) Enabled() bool { return c != nil && c.token != "" && c.phoneID != "" }

// crmWABody picks WhatsApp's body + language code: hi_devanagari preferred
// (same preference inappChannel encodes), else hi_roman, else en.
func crmWABody(tpl crmTemplate) (body, lang string) {
	switch {
	case tpl.HIDev != "":
		return tpl.HIDev, "hi"
	case tpl.HI != "":
		return tpl.HI, "hi"
	default:
		return tpl.EN, "en"
	}
}

func (c *whatsappChannel) available(t crmTrigger, tpl crmTemplate) error {
	if !c.Enabled() {
		return fmt.Errorf("whatsapp: channel not configured (CRM_WA_TOKEN / CRM_WA_PHONE_ID unset)")
	}
	if c.names[t.ID] == "" {
		return fmt.Errorf("whatsapp: no approved template name mapped for trigger %s (CRM_WA_TEMPLATE_NAMES) — business-initiated WhatsApp requires a pre-approved template", t.ID)
	}
	if body, _ := crmWABody(tpl); body == "" {
		return fmt.Errorf("whatsapp: template %s has no body", t.Template.String())
	}
	return nil
}

// deliver posts one template message to POST {base}/{phone_id}/messages. The
// approved template's {{1}},{{2}},… slots are filled positionally with the CRM
// tokens in order of appearance in the body variant we route (register the
// Meta template with its variables in that same order).
func (c *whatsappChannel) deliver(ctx context.Context, phone string, t crmTrigger, tpl crmTemplate, params map[string]string) error {
	body, lang := crmWABody(tpl)
	_, values, err := crmTokenValues(body, params)
	if err != nil {
		return fmt.Errorf("whatsapp: %w", err)
	}
	template := map[string]any{
		"name":     c.names[t.ID],
		"language": map[string]string{"code": lang},
	}
	if len(values) > 0 { // Meta rejects an empty parameters array
		ps := make([]map[string]string, 0, len(values))
		for _, v := range values {
			ps = append(ps, map[string]string{"type": "text", "text": v})
		}
		template["components"] = []map[string]any{{"type": "body", "parameters": ps}}
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                phone,
		"type":              "template",
		"template":          template,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("whatsapp: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+c.phoneID+"/messages", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("whatsapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("content-type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp: request failed: %v: %w", err, errCRMTransient)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("whatsapp: provider unavailable (status %d): %s: %w", resp.StatusCode, rb, errCRMTransient)
	}
	// Graph acknowledges an accepted send with a "messages":[{"id":…}] block.
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(rb), `"messages"`) {
		return fmt.Errorf("whatsapp: send rejected (status %d): %s", resp.StatusCode, rb)
	}
	return nil
}

// Send satisfies crmChannel (the engine seam): resolve the phone, then deliver.
func (c *whatsappChannel) Send(ctx context.Context, s *service, consumerID primitive.ObjectID, t crmTrigger, tpl crmTemplate, params map[string]string) error {
	if err := c.available(t, tpl); err != nil {
		return err
	}
	phone, err := s.crmDeliveryPhone(ctx, consumerID)
	if err != nil {
		return fmt.Errorf("whatsapp: %w", err)
	}
	return c.deliver(ctx, phone, t, tpl, params)
}

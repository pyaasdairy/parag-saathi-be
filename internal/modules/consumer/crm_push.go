// CRM push transport: the Expo push service behind the config's "push"
// channel (contract C5).
//
// Inert until EXPO_PUSH_ENABLED=true: crmTransports resolves no push
// transport and the dispatcher behaves exactly as before. Tokens come from
// consumer_push_devices, the registry POST /consumer/push/register fills; a
// member with no device is a DEFINITIVE unavailability, so the trigger's
// fallback runs at once (G9: "if absent, fall through immediately"). A token
// Expo reports as DeviceNotRegistered is dropped from the registry.
//
// Push is not a telecom channel: DND does not apply, and a promotional push
// is gated by the same G2/G2b consent checks as every transport
// (marketing_push). The message body is the rendered EN template (the app's
// inbox holds both languages); data.href is the in-app route the app's tap
// handler opens (lib/notifications.ts hrefFromResponse).
package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const crmExpoPushEndpoint = "https://exp.host/--/api/v2/push/send"

type pushChannel struct {
	enabled     bool
	accessToken string // EXPO_ACCESS_TOKEN, optional (Expo push security)
	baseURL     string // crmExpoPushEndpoint; a struct field so tests point it at httptest
	client      *http.Client
	repo        *repository
	log         *slog.Logger
	// consumerID binds one dispatch. The crmTransport seam hands a transport
	// the member's phone; push needs the member's device rows instead, so
	// crmDispatchAt binds a per-dispatch copy (bind) and the shared instance
	// on the service stays unbound and stateless.
	consumerID primitive.ObjectID
}

var _ crmChannel = (*pushChannel)(nil)
var _ crmTransport = (*pushChannel)(nil)

// newPushChannel builds the push transport from env. EXPO_PUSH_ENABLED not
// exactly "true" -> disabled, and the dispatcher never resolves it.
func newPushChannel(log *slog.Logger, repo *repository) *pushChannel {
	return &pushChannel{
		enabled:     os.Getenv("EXPO_PUSH_ENABLED") == "true",
		accessToken: strings.TrimSpace(os.Getenv("EXPO_ACCESS_TOKEN")),
		baseURL:     crmExpoPushEndpoint,
		client:      &http.Client{Timeout: 10 * time.Second},
		repo:        repo,
		log:         log,
	}
}

func (c *pushChannel) Name() string { return "push" }

func (c *pushChannel) Enabled() bool { return c != nil && c.enabled }

// bind returns a copy of the transport that delivers to one member's devices.
func (c *pushChannel) bind(consumerID primitive.ObjectID) *pushChannel {
	cp := *c
	cp.consumerID = consumerID
	return &cp
}

func (c *pushChannel) available(t crmTrigger, tpl crmTemplate) error {
	if !c.Enabled() {
		return fmt.Errorf("push: channel not configured (EXPO_PUSH_ENABLED unset)")
	}
	if c.consumerID.IsZero() {
		return fmt.Errorf("push: transport not bound to a member")
	}
	if crmPushBody(tpl) == "" {
		return fmt.Errorf("push: template %s has no body", t.Template.String())
	}
	return nil
}

// crmPushBody is the roman English body (every handset renders it), else the
// roman Hindi one. Never Devanagari: the notification tray is not the inbox.
func crmPushBody(tpl crmTemplate) string {
	if tpl.EN != "" {
		return tpl.EN
	}
	return tpl.HI
}

// crmPushHref is the in-app route the tap opens: the order for an order
// event, the complaints screen for a complaint, else the inbox where every
// CRM message also lands.
func crmPushHref(params map[string]string) string {
	if id := params["ORDER_ID"]; id != "" {
		return "/order/" + id
	}
	if params["COMPLAINT_ID"] != "" {
		return "/complaints"
	}
	return "/inbox"
}

// crmPushAndroidChannel names the Android channel the app registers
// (lib/notifications.ts CHANNELS): offers for promotional content, wallet
// for the B ladder, delivery for the order lifecycle, orders otherwise.
func crmPushAndroidChannel(t crmTrigger) string {
	switch {
	case t.Category == "promotional":
		return "offers"
	case t.Section == "B":
		return "wallet"
	case t.Section == "D":
		return "delivery"
	}
	return "orders"
}

// expoPushTokens lists a member's Expo tokens, newest first (a handset that
// changed hands re-registers under its new owner, so the row's owner is the
// truth). Only expo-provider rows in Expo's token format are sent.
func (r *repository) expoPushTokens(ctx context.Context, consumerID primitive.ObjectID) ([]string, error) {
	cur, err := r.pushDevices().Find(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}, {Key: "provider", Value: "expo"}},
		options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}).SetLimit(5))
	if err != nil {
		return nil, err
	}
	var rows []pushDevice
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, d := range rows {
		if strings.HasPrefix(d.Token, "ExponentPushToken[") || strings.HasPrefix(d.Token, "ExpoPushToken[") {
			out = append(out, d.Token)
		}
	}
	return out, nil
}

// dropPushToken forgets a token Expo says no device holds any more.
func (r *repository) dropPushToken(ctx context.Context, token string) {
	_, _ = r.pushDevices().DeleteOne(ctx, bson.D{{Key: "token", Value: token}})
}

// expoPushReceipt is one entry of the push service's per-message answer.
type expoPushReceipt struct {
	Status  string `json:"status"` // ok | error
	Message string `json:"message"`
	Details struct {
		Error string `json:"error"` // DeviceNotRegistered, MessageTooBig, ...
	} `json:"details"`
}

// deliver posts one message per registered device. The phone argument is
// unused: push addresses devices, not numbers. Definitive failures (no
// device, every device refused, request rejected) let the chain fall back;
// a network error or 5xx is transient and, as for every transport, stops it.
func (c *pushChannel) deliver(ctx context.Context, _ string, t crmTrigger, tpl crmTemplate, params map[string]string) error {
	tokens, err := c.repo.expoPushTokens(ctx, c.consumerID)
	if err != nil {
		return fmt.Errorf("push: device lookup: %v: %w", err, errCRMTransient)
	}
	if len(tokens) == 0 {
		return fmt.Errorf("push: no registered device for the member")
	}
	body := crmPushBody(tpl)
	if _, _, err := crmTokenValues(body, params); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	body = crmRender(body, params)
	data := map[string]string{"href": crmPushHref(params), "trigger_id": t.ID, "category": t.Category}
	if tpl.CTA != "" {
		data["cta"] = tpl.CTA
	}
	if v := params["ORDER_ID"]; v != "" {
		data["order_id"] = v
	}
	if v := params["COMPLAINT_ID"]; v != "" {
		data["complaint_id"] = v
	}
	msgs := make([]map[string]any, 0, len(tokens))
	for _, tok := range tokens {
		msgs = append(msgs, map[string]any{
			"to": tok, "title": "PYAAS", "body": body, "sound": "default",
			"channelId": crmPushAndroidChannel(t), "data": data,
		})
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("push: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("push: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("push: request failed: %v: %w", err, errCRMTransient)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("push: provider unavailable (status %d): %s: %w", resp.StatusCode, rb, errCRMTransient)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("push: send rejected (status %d): %s", resp.StatusCode, rb)
	}
	var out struct {
		Data   []expoPushReceipt `json:"data"`
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return fmt.Errorf("push: unreadable answer: %s", rb)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("push: send rejected: %s %s", out.Errors[0].Code, out.Errors[0].Message)
	}
	accepted := 0
	for i, r := range out.Data {
		if r.Status == "ok" {
			accepted++
			continue
		}
		if r.Details.Error == "DeviceNotRegistered" && i < len(tokens) {
			c.repo.dropPushToken(ctx, tokens[i])
			if c.log != nil {
				c.log.Info("crm: push token dropped, device no longer registered", "trigger", t.ID)
			}
			continue
		}
		if c.log != nil {
			c.log.Warn("crm: push device refused", "trigger", t.ID, "error", r.Details.Error, "message", r.Message)
		}
	}
	if accepted == 0 {
		return fmt.Errorf("push: no device accepted the message")
	}
	return nil
}

// Send satisfies crmChannel (the engine seam): bind the member, then deliver.
func (c *pushChannel) Send(ctx context.Context, s *service, consumerID primitive.ObjectID, t crmTrigger, tpl crmTemplate, params map[string]string) error {
	b := c.bind(consumerID)
	if err := b.available(t, tpl); err != nil {
		return err
	}
	return b.deliver(ctx, "", t, tpl, params)
}

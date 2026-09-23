// CRM human_call transport: a trigger that names "human_call" (E-05 milk
// quality / safety as its primary, E-04 missing items in parallel) puts a
// row on the crm_callbacks queue that an operator works from the admin CRM
// (GET /consumer/admin/crm/callbacks). Before this, human_call resolved to
// nothing and a safety complaint reached nobody.
//
// It is not a telecom channel: no provider, no env key, nothing leaves the
// building. It is plugged for exactly the triggers whose delivery block names
// it, so every other dispatch is byte-identical to before. The dispatch claim
// (trigger, consumer, IST day, scope) makes the row exactly-once per event.
//
// A template-less trigger whose primary is human_call is routed without the
// template gate and writes NO inbox row: the config's rule for E-05 is
// "NEVER auto-respond"; a human answers it.
package consumer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

const (
	collCRMCallbacks  = "crm_callbacks"
	crmChanHumanCall  = "human_call"
	crmCallbackOpen   = "open"
	crmCallbacksLimit = 500
)

// crmCallback is one queued call-back. Payload is the product event the
// trigger fired on (complaint id, ref, category, order id), so the operator
// has what they need without a second lookup.
type crmCallback struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"`
	TriggerID  string             `bson:"trigger_id"`
	Reason     string             `bson:"reason"`
	Phone      string             `bson:"phone,omitempty"`
	ScopeKey   string             `bson:"scope_key,omitempty"`
	Payload    map[string]any     `bson:"payload,omitempty"`
	Status     string             `bson:"status"` // open (an operator workflow may add more later)
	CreatedAt  time.Time          `bson:"created_at"`
}

func (r *repository) crmCallbacksCol() *mongo.Collection {
	return r.accounts.Database().Collection(collCRMCallbacks)
}

// crmRoutesChannel reports whether a trigger's delivery block names channel
// as its primary, a parallel or a fallback.
func crmRoutesChannel(t crmTrigger, channel string) bool {
	if t.Delivery.Primary == channel {
		return true
	}
	for _, p := range t.Delivery.Parallel {
		if p == channel {
			return true
		}
	}
	for _, fb := range t.Delivery.Fallback {
		if fb.Channel == channel {
			return true
		}
	}
	return false
}

// crmHumanOnly is a trigger a person answers and no template speaks for:
// primary human_call and no template (E-05). The router lets it through the
// template gate and the dispatcher skips the inbox for it.
func crmHumanOnly(t crmTrigger) bool {
	return t.Delivery.Primary == crmChanHumanCall && t.Template.String() == ""
}

// callbackChannel is the human_call transport, bound per dispatch to the
// member and the event (like pushChannel.bind): the crmTransport seam hands a
// transport only the phone.
type callbackChannel struct {
	repo       *repository
	consumerID primitive.ObjectID
	scope      string
	payload    map[string]any
}

var _ crmTransport = (*callbackChannel)(nil)

func (c *callbackChannel) Name() string { return crmChanHumanCall }

// available: a queue row needs nothing a provider could refuse.
func (c *callbackChannel) available(crmTrigger, crmTemplate) error { return nil }

func (c *callbackChannel) deliver(ctx context.Context, phone string, t crmTrigger, _ crmTemplate, _ map[string]string) error {
	reason := strings.TrimSpace(t.Name)
	if reason == "" {
		reason = t.ID
	}
	row := crmCallback{
		ConsumerID: c.consumerID, TriggerID: t.ID, Reason: reason, Phone: phone,
		ScopeKey: c.scope, Payload: c.payload, Status: crmCallbackOpen, CreatedAt: time.Now().UTC(),
	}
	if _, err := c.repo.crmCallbacksCol().InsertOne(ctx, row); err != nil {
		return fmt.Errorf("queue callback: %w", err)
	}
	return nil
}

// ── Admin: GET /consumer/admin/crm/callbacks ────────────────────────────────

// crmCallbacks lists the call-back queue, newest first; ?status=open narrows
// it. Read-only, behind the admin CRM gate (SUPER_ADMIN token or the
// website's X-Admin-Key) and its read audit, like the complaints queue.
func (h *handler) crmCallbacks(w http.ResponseWriter, r *http.Request) {
	filter := bson.D{}
	if st := strings.TrimSpace(r.URL.Query().Get("status")); st != "" {
		filter = append(filter, bson.E{Key: "status", Value: st})
	}
	ctx := r.Context()
	cur, err := h.svc.repo.crmCallbacksCol().Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(crmCallbacksLimit))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("callbacks read failed")))
		return
	}
	var rows []crmCallback
	if err := cur.All(ctx, &rows); err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("callbacks decode failed")))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	open := 0
	for _, c := range rows {
		if c.Status == crmCallbackOpen {
			open++
		}
		out = append(out, map[string]any{
			"id": c.ID.Hex(), "consumerId": c.ConsumerID.Hex(), "phone": c.Phone,
			"triggerId": c.TriggerID, "reason": c.Reason, "payload": c.Payload,
			"status": c.Status, "createdAt": c.CreatedAt,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out), "open": open})
}

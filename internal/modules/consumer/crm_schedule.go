// CRM delayed dispatch: a trigger whose config carries a non-zero "delay"
// (A-01 PT2H, C-03 PT1H, E-07 PT4H, ...) is not sent on the worker tick
// that drains its event. The router evaluates the conditions once at enqueue
// and writes a crm_schedules row due at event-time + delay; the scheduler
// tick (crmProcessSchedules) fires every due row through the same
// crmFireTrigger path, re-evaluating the conditions and refusing an event
// whose order has since been cancelled. Replays are harmless: the row is
// unique per (trigger, event), and the dispatch claim still applies at fire.
package consumer

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collCRMSchedules = "crm_schedules"

// crmSchedule is one deferred dispatch: the event as it was, and when to
// fire it. Status NEW -> PROCESSING (a lease, like the outbox) -> DONE, or
// SKIPPED with the reason when the fire-time re-check refused it.
type crmSchedule struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	TriggerID  string             `bson:"trigger_id"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"`
	EventID    primitive.ObjectID `bson:"event_id"`
	Topic      string             `bson:"topic"`
	Payload    map[string]any     `bson:"payload,omitempty"`
	DueAt      time.Time          `bson:"due_at"`
	Status     string             `bson:"status"` // NEW | PROCESSING | DONE | SKIPPED
	Reason     string             `bson:"reason,omitempty"`
	ClaimedAt  *time.Time         `bson:"claimed_at,omitempty"`
	CreatedAt  time.Time          `bson:"created_at"`
}

func (r *repository) crmSchedulesCol() *mongo.Collection {
	return r.accounts.Database().Collection(collCRMSchedules)
}

// crmTriggerDelay is the trigger's configured delay as a duration; zero for
// PT0S, an absent field, or a shape crmParseISODuration does not read.
func crmTriggerDelay(t crmTrigger) time.Duration {
	d, _ := crmParseISODuration(t.Delay)
	return d
}

// crmParseISODuration reads the config's ISO-8601 durations: PT2H, PT30M,
// PT0S, PT1H30M, P1D, P1DT2H. Anything else is (0, false).
func crmParseISODuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if !strings.HasPrefix(s, "P") || s == "P" || s == "PT" {
		return 0, false
	}
	var total time.Duration
	inTime := false
	num := ""
	for _, c := range s[1:] {
		switch {
		case c >= '0' && c <= '9':
			num += string(c)
		case c == 'T':
			if inTime || num != "" {
				return 0, false
			}
			inTime = true
		default:
			if num == "" {
				return 0, false
			}
			n, err := strconv.Atoi(num)
			if err != nil {
				return 0, false
			}
			num = ""
			switch {
			case c == 'D' && !inTime:
				total += time.Duration(n) * 24 * time.Hour
			case c == 'H' && inTime:
				total += time.Duration(n) * time.Hour
			case c == 'M' && inTime:
				total += time.Duration(n) * time.Minute
			case c == 'S' && inTime:
				total += time.Duration(n) * time.Second
			default:
				return 0, false
			}
		}
	}
	if num != "" {
		return 0, false
	}
	return total, true
}

// crmEnqueueSchedule writes the deferred dispatch. A replayed event (the
// outbox is at-least-once) hits the unique (trigger, event) index and is
// dropped: the first row already carries the same payload and due time.
func (s *service) crmEnqueueSchedule(ctx context.Context, t crmTrigger, ev crmEvent, due time.Time) {
	row := crmSchedule{
		TriggerID: t.ID, ConsumerID: ev.ConsumerID, EventID: ev.ID, Topic: ev.Topic,
		Payload: ev.Payload, DueAt: due.UTC(), Status: "NEW", CreatedAt: time.Now().UTC(),
	}
	if _, err := s.repo.crmSchedulesCol().InsertOne(ctx, row); err != nil && !mongo.IsDuplicateKeyError(err) {
		s.log.Warn("crm: schedule enqueue failed", "trigger", t.ID, "topic", ev.Topic, "err", err)
	}
}

// crmFireDueSchedules fires every schedule row due at `now`, at most 200 per
// tick. Each row is leased (NEW -> PROCESSING) so a second replica never
// fires it too; a lease older than ten minutes belongs to a dead worker and
// goes back to NEW, exactly as the outbox does.
func (s *service) crmFireDueSchedules(ctx context.Context, now time.Time) {
	col := s.repo.crmSchedulesCol()
	_, _ = col.UpdateMany(ctx,
		bson.D{{Key: "status", Value: "PROCESSING"}, {Key: "claimed_at", Value: bson.D{{Key: "$lt", Value: now.Add(-10 * time.Minute)}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "NEW"}}}})
	for i := 0; i < 200; i++ {
		var row crmSchedule
		err := col.FindOneAndUpdate(ctx,
			bson.D{{Key: "status", Value: "NEW"}, {Key: "due_at", Value: bson.D{{Key: "$lte", Value: now.UTC()}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "PROCESSING"}, {Key: "claimed_at", Value: now.UTC()}}}},
			options.FindOneAndUpdate().SetSort(bson.D{{Key: "due_at", Value: 1}})).Decode(&row)
		if err != nil {
			return // nothing due (or transient: the next tick retries)
		}
		status, reason := s.crmFireSchedule(ctx, row, now)
		_, _ = col.UpdateOne(ctx, bson.D{{Key: "_id", Value: row.ID}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: status}, {Key: "reason", Value: reason}}}})
	}
}

// crmFireSchedule is the fire-time half of a delayed trigger: the same
// condition evaluation the enqueue ran, plus the stale-event check, then the
// ordinary dispatch. Returns the schedule row's final status and reason.
func (s *service) crmFireSchedule(ctx context.Context, row crmSchedule, now time.Time) (status, reason string) {
	t, ok := crmConfigLoad().Triggers[row.TriggerID]
	if !ok {
		return "SKIPPED", "trigger no longer in the config"
	}
	ev := crmEvent{ID: row.EventID, Topic: row.Topic, ConsumerID: row.ConsumerID, Payload: row.Payload}
	e := &crmEventCtx{s: s, ctx: ctx, ev: ev}
	if stale, why := crmEventStale(e); stale {
		return "SKIPPED", why
	}
	hold, err := crmEvalConditions(t.Conditions, e.fact)
	if err != nil {
		s.crmCondFailClosed(t, err)
		return "SKIPPED", "condition failed closed at fire: " + err.Error()
	}
	if !hold {
		return "SKIPPED", "conditions no longer hold at fire"
	}
	if fired, why := s.crmFireTrigger(ctx, t, e, now); !fired {
		return "SKIPPED", why
	}
	return "DONE", ""
}

// crmEventStale reports whether the product event a delayed trigger was
// queued for has been undone meanwhile: the order it names is cancelled (or
// gone). The failure topics are exempt from that rule, since a cancelled
// order is exactly what they announce; order.failed is the reverse (below).
func crmEventStale(e *crmEventCtx) (bool, string) {
	switch e.ev.Topic {
	case "order.line_cancelled":
		return false, ""
	case "order.failed":
		// D-09 is held through the rider's undo window. The failure it
		// announces is stale once the order is no longer cancelled: the rider
		// undid the marking or the store reassigned the task (both walk the
		// order back), and it may already be delivered.
		o := e.order()
		if o == nil {
			return true, "order no longer exists"
		}
		if o.Status != "cancelled" {
			return true, fmt.Sprintf("order %s is %s again: the failure was undone", o.OrderID, o.Status)
		}
		return false, ""
	case "payment.failed":
		return crmPaymentFailureStale(e)
	}
	if id, _ := e.ev.Payload["order_id"].(string); strings.TrimSpace(id) == "" {
		return false, ""
	}
	o := e.order()
	if o == nil {
		return true, "order no longer exists"
	}
	if o.Status == "cancelled" {
		return true, fmt.Sprintf("order %s cancelled before the delay elapsed", o.OrderID)
	}
	return false, ""
}

// crmPaymentFailureStale: B-03 says "no money was deducted", so it waits
// out a checkout retry. A Razorpay checkout that fails one method often
// succeeds with another on the SAME order; once that top-up order is paid (or
// its ledger credit exists) the failure is not news any more. A mandate
// shortfall names no payment order and is never stale here.
func crmPaymentFailureStale(e *crmEventCtx) (bool, string) {
	orderID, _ := e.ev.Payload["payment_order_id"].(string)
	if orderID = strings.TrimSpace(orderID); orderID == "" {
		return false, ""
	}
	if po, err := e.s.repo.findPaymentOrderByID(e.ctx, orderID); err == nil && po.Status == "PAID" {
		return true, fmt.Sprintf("payment order %s was paid by a later attempt", orderID)
	}
	n, err := e.s.repo.walletTxns.CountDocuments(e.ctx, bson.D{
		{Key: "consumer_id", Value: e.ev.ConsumerID}, {Key: "ref_id", Value: orderID}, {Key: "type", Value: "TOPUP"},
	})
	if err == nil && n > 0 {
		return true, fmt.Sprintf("payment order %s was credited by a later attempt", orderID)
	}
	return false, ""
}

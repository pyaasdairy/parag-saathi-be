// CRM delayed dispatch: a trigger whose config carries a non-zero "delay"
// (A-01 PT2H, C-03 PT1H, E-07 PT4H, ...) is not sent on the worker tick
// that drains its event. The router evaluates the conditions once at enqueue
// and writes a crm_schedules row due at event-time + delay; the scheduler
// tick (crmProcessSchedules) fires every due row through the same
// crmFireTrigger path, re-evaluating the conditions and refusing an event
// whose order has since been cancelled. Replays are harmless: the row is
// unique per (trigger, event), and the dispatch claim still applies at fire.
//
// The same rows carry a message the quiet hours defer (G5, founder decision
// 6): one that may not go out when its event is drained, or when its delay
// elapses, waits until it may (crmSendWindow) and is re-checked then.
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
		set := bson.D{{Key: "status", Value: status}, {Key: "reason", Value: reason}}
		if status == "NEW" { // a row that may not go out now waits for the moment it may
			next := now.Add(time.Hour)
			if t, ok := crmConfigLoad().Triggers[row.TriggerID]; ok {
				next, _ = crmSendWindow(t, now)
			}
			set = append(set, bson.E{Key: "due_at", Value: next.UTC()})
		}
		_, _ = col.UpdateOne(ctx, bson.D{{Key: "_id", Value: row.ID}}, bson.D{{Key: "$set", Value: set}})
	}
}

// crmPromoWindow reports whether now is inside the promotional window of G5
// (guards.G5_quiet_hours.windows.promotional_message, IST) and, when it is
// not, when the window next opens.
func crmPromoWindow(now time.Time) (next time.Time, inside bool) {
	g := crmConfigLoad().Guards
	ist := now.In(istZone)
	hm := ist.Format("15:04")
	if hm >= g.QuietPromoStart && hm < g.QuietPromoEnd {
		return now, true
	}
	open, err := time.ParseInLocation("15:04", g.QuietPromoStart, istZone)
	if err != nil {
		return now.Add(time.Hour), false // unreadable config: look again in an hour
	}
	next = time.Date(ist.Year(), ist.Month(), ist.Day(), open.Hour(), open.Minute(), 0, 0, istZone)
	if hm >= g.QuietPromoEnd {
		next = next.AddDate(0, 0, 1)
	}
	return next, false
}

// crmQuietWindow reports whether now is outside the member quiet hours of G5
// (windows.service_transactional.avoid, IST: 22:00-07:00, wrapping midnight)
// and, when it is inside them, when they end.
func crmQuietWindow(now time.Time) (next time.Time, open bool) {
	g := crmConfigLoad().Guards
	ist := now.In(istZone)
	hm := ist.Format("15:04")
	quiet := hm >= g.QuietStart && hm < g.QuietEnd
	if g.QuietStart > g.QuietEnd { // the usual case: the hours wrap midnight
		quiet = hm >= g.QuietStart || hm < g.QuietEnd
	}
	if !quiet {
		return now, true
	}
	end, err := time.ParseInLocation("15:04", g.QuietEnd, istZone)
	if err != nil {
		return now.Add(time.Hour), false // unreadable config: look again in an hour
	}
	next = time.Date(ist.Year(), ist.Month(), ist.Day(), end.Hour(), end.Minute(), 0, 0, istZone)
	if hm >= g.QuietEnd { // 22:00-23:59: the hours end tomorrow morning
		next = next.AddDate(0, 0, 1)
	}
	return next, false
}

// crmAlwaysSends reports a trigger that goes out the moment it is due, day or
// night (G5 quiet_hours.always_send; founder decision 6, 25 Sep 2026: order,
// delivery and money-added messages always send). It qualifies by category
// (transactional, internal), by the trigger's own critical flag, by section
// (D the order and delivery lifecycle, E a complaint or rating and its
// answer, FF a Founding Family seat) or by name. A promotional trigger never
// does: it waits for its own window and the end of the quiet hours.
func crmAlwaysSends(t crmTrigger) bool {
	if t.Category == "promotional" {
		return false
	}
	a := crmConfigLoad().Guards.AlwaysSend
	return a.Categories[t.Category] || (a.Critical && t.Critical) || a.Sections[t.Section] || a.Triggers[t.ID]
}

// crmSendWindow is when t may go out: (now, true) when it may now, else
// (the first moment it may, false). An always_send trigger goes at once; any
// other member message waits out the quiet hours; a promotional one must also
// be inside the promotional window. Every caller DEFERS on false (the router
// and the schedule fire re-queue the event, a scheduled sweep tries again on
// its next run) except the promotional G5 guard, which suppresses and logs.
func crmSendWindow(t crmTrigger, now time.Time) (time.Time, bool) {
	if crmAlwaysSends(t) {
		return now, true
	}
	at := now
	for i := 0; i < 4; i++ { // two windows: settles in at most two moves
		moved := false
		if next, open := crmQuietWindow(at); !open {
			at, moved = next, true
		}
		if t.Category == "promotional" {
			if next, open := crmPromoWindow(at); !open {
				at, moved = next, true
			}
		}
		if !moved {
			return at, at.Equal(now)
		}
	}
	return at, false
}

// crmDueAt is when an event trigger whose conditions hold is sent: now
// (fireNow), or the moment its crm_schedules row comes due, which is the
// event time plus its delay, or, for a message that may not go out now
// (crmSendWindow: the quiet hours, the promotional window), the moment it
// may. The row is re-checked when it fires, so the wait never sends a message
// whose event was undone meanwhile.
func crmDueAt(t crmTrigger, ev crmEvent, now time.Time) (due time.Time, fireNow bool) {
	if d := crmTriggerDelay(t); d > 0 {
		return crmDelayBase(ev, now).Add(d), false
	}
	if next, open := crmSendWindow(t, now); !open {
		return next, false
	}
	return now, true
}

// crmScheduledOpen reports whether the scheduled trigger id may go out at
// now (crmSendWindow). The scheduled sweeps run from 09:00 and reach the
// quiet hours only when the worker was down until after 22:00; such a run
// sends nothing that night and the next run decides again (B-01 and W-07
// re-check every morning; W-06's day-3 nudge gives way to its day-5 one).
// An unknown id is open: the dispatch itself reports it.
func crmScheduledOpen(id string, now time.Time) bool {
	t, ok := crmConfigLoad().Triggers[id]
	if !ok {
		return true
	}
	_, open := crmSendWindow(t, now)
	return open
}

// crmFireSchedule is the fire-time half of a delayed trigger: the same
// condition evaluation the enqueue ran, plus the stale-event check, then the
// ordinary dispatch. Returns the schedule row's final status and reason.
func (s *service) crmFireSchedule(ctx context.Context, row crmSchedule, now time.Time) (status, reason string) {
	t, ok := crmConfigLoad().Triggers[row.TriggerID]
	if !ok {
		return "SKIPPED", "trigger no longer in the config"
	}
	// Erasure deletes the member's pending rows; one queued by a tick racing
	// the erasure must still never write an inbox row for an erased id.
	if acct, err := s.repo.findAccountByID(ctx, row.ConsumerID); err != nil || acct == nil {
		if err == nil || crmErrCode(err) == "NOT_FOUND" {
			return "SKIPPED", "account erased"
		}
		return "SKIPPED", "account lookup failed"
	}
	// A promotional message that comes due outside 10:00-21:00 (E-07, 4 h after
	// an evening rating) would be suppressed by G5 and lost; it waits for the
	// window instead, and everything below is re-checked when it opens. Any
	// other member message that comes due inside the quiet hours (A-01 two
	// hours after a 21:00 sign-up) waits for them to end the same way.
	if _, open := crmSendWindow(t, now); !open {
		if t.Category == "promotional" {
			return "NEW", "waiting for the promotional window"
		}
		return "NEW", "waiting for the quiet hours to end"
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
	case "subscription.modified", "subscription.created_unpaid":
		return crmPlanChangeStale(e)
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

// crmPlanChangeStale reloads the plan a delayed plan message is about. The
// event's payload records the change as it happened; the member may have
// changed their mind within the hour (C-03) or the two hours (A-05), and the
// message must describe the plan as it is when it is sent:
//   - A-05 (subscription.created_unpaid) only while the plan is still active;
//   - C-03 on a pause only while the plan is still paused, on a quantity
//     reduction only while the quantity is still below what it was.
//
// A plan that no longer exists (or cannot be read) is stale: fail closed.
func crmPlanChangeStale(e *crmEventCtx) (bool, string) {
	subID, _ := e.ev.Payload["subscription_id"].(string)
	if subID = strings.TrimSpace(subID); subID == "" {
		return false, ""
	}
	sub, err := e.s.repo.findSubscriptionByID(e.ctx, subID)
	if err != nil {
		return true, "plan lookup failed: " + err.Error()
	}
	if sub == nil || sub.ConsumerID != e.ev.ConsumerID {
		return true, fmt.Sprintf("plan %s no longer exists", subID)
	}
	if e.ev.Topic == "subscription.created_unpaid" {
		if sub.Status != "active" {
			return true, fmt.Sprintf("plan %s is %s", subID, sub.Status)
		}
		return false, ""
	}
	switch change, _ := e.ev.Payload["change"].(string); change {
	case "paused":
		if sub.Status != "paused" {
			return true, fmt.Sprintf("plan %s is %s again", subID, sub.Status)
		}
	case "quantity_reduced":
		if prev, ok := crmPayloadNumber(e.ev.Payload["previous_qty"]); ok && float64(sub.Qty) >= prev {
			return true, fmt.Sprintf("plan %s is back to %d", subID, sub.Qty)
		}
	}
	return false, ""
}

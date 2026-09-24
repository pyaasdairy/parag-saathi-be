// CRM generic event router: the outbox topic and the trigger's conditions
// decide what fires, so wiring a new event trigger is a config change again.
//
// crmRouteEvent (crm_engine.go) keeps its explicit cases for the Welcome Litre
// state machine and calls crmRouteGeneric after them. Everything here is
// inert unless CRM_ENABLED (the worker never runs otherwise).
package consumer

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// crmLifecycleTopics are the outbox topics the lifecycle emits (contract C6).
// crmRouteGeneric serves every "event" trigger whose event names one of
// them; the token-coverage test walks this list.
var crmLifecycleTopics = []string{
	"order.confirmed", "order.dispatched", "order.delivered", "order.failed",
	"complaint.created", "complaint.resolved", "rating.submitted",
	"user.registered", "wallet.credited", "payment.failed",
	"subscription.activated", "subscription.created_unpaid", "subscription.modified",
	// The noon lock skipped a member's day for want of funds (D-07).
	"subscription.day_skipped",
	"order.line_cancelled", "delivery.delayed", "serviceability.checked",
	// Growth programmes: Founding Family seats (founding.go) and referrals
	// (referrals.go). The referral topics carry no trigger yet; they are in
	// the outbox for the day one is added.
	"founding.farm_unlocked", "founding.member_active", "founding.seat_waiting",
	"referral.applied", "referral.rewarded",
}

// crmComplaintSLA fills [SLA] in T-E02: the config's support.sla_resolve
// (PT24H, measured in support hours).
const crmComplaintSLA = "24 hours"

// crmRouteGeneric fires every event trigger in the config whose event is the
// outbox topic and whose conditions all hold. Skipped by design: internal
// triggers (operator-side, handled explicitly), triggers with no customer
// template, aliases and schedules. A trigger with a non-zero "delay" is
// queued instead (crm_schedule.go) and fired by the scheduler tick after
// re-checking its conditions. The dispatch claim is (trigger, consumer, IST
// day, scope): scoped to the order or complaint the event is about
// (crmEventScopeKey), so a per_order trigger fires once per order, not once
// per day.
func (s *service) crmRouteGeneric(ctx context.Context, ev crmEvent) {
	s.crmRouteGenericAt(ctx, ev, time.Now().UTC())
}

func (s *service) crmRouteGenericAt(ctx context.Context, ev crmEvent, now time.Time) {
	if ev.ConsumerID.IsZero() {
		return
	}
	cfg := crmConfigLoad()
	var ids []string
	for id, t := range cfg.Triggers {
		if _, waiting := cfg.AwaitingEvent[id]; waiting {
			continue // no product event exists for it yet (meta.awaiting_event)
		}
		// A trigger needs a template to speak, except one a person answers
		// (primary human_call, no template: E-05), which queues a call-back.
		if t.Kind == "event" && t.Event == ev.Topic && t.Category != "internal" && (t.Template.String() != "" || crmHumanOnly(t)) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids) // deterministic dispatch order
	e := &crmEventCtx{s: s, ctx: ctx, ev: ev}
	for _, id := range ids {
		t := cfg.Triggers[id]
		ok, err := crmEvalConditions(t.Conditions, e.fact)
		if err != nil {
			s.crmCondFailClosed(t, err)
			continue
		}
		if !ok {
			continue
		}
		if d := crmTriggerDelay(t); d > 0 {
			s.crmEnqueueSchedule(ctx, t, ev, crmDelayBase(ev, now).Add(d))
			continue
		}
		s.crmFireTrigger(ctx, t, e, now)
	}
}

// crmDelayBase is the instant a trigger's delay counts from: when the event
// happened, not the tick that drained it, so a stalled or spun-down worker does
// not push every delayed message back by its backlog. An event with no time,
// or one stamped after the tick's clock (a test driving its own clock), counts
// from the tick.
func crmDelayBase(ev crmEvent, now time.Time) time.Time {
	if ev.CreatedAt.IsZero() || ev.CreatedAt.After(now) {
		return now
	}
	return ev.CreatedAt
}

// crmFireTrigger renders and dispatches one trigger for one event whose
// conditions hold: the shared tail of the immediate route and the delayed
// fire. Reports whether a dispatch was attempted and, when not, why.
func (s *service) crmFireTrigger(ctx context.Context, t crmTrigger, e *crmEventCtx, now time.Time) (bool, string) {
	cfg := crmConfigLoad()
	if e.now.IsZero() {
		e.now = now // a token worded against the clock reads the moment it is sent
	}
	params := e.params()
	// A conditional template ({"if": ..., "then": ..., "else": ...}, B-06)
	// picks its branch on the same condition grammar and facts; an
	// unevaluable "if" fails closed like any other condition.
	tplID := t.Template.String()
	if t.Template.If != "" {
		then, err := crmEvalConditions([]string{t.Template.If}, e.fact)
		if err != nil {
			s.crmCondFailClosed(t, err)
			return false, "template condition failed closed: " + err.Error()
		}
		if !then {
			tplID = t.Template.Else
		}
	}
	// C-01: a message that cannot resolve its values is refused, never
	// half-rendered. Checked before the claim so a later tick with the value
	// present can still fire.
	if tok, ok := crmTemplateResolvable(cfg.Templates[tplID], params); !ok {
		s.log.Warn("crm: unresolved template token, trigger not fired", "trigger", t.ID, "token", tok)
		return false, "unresolved template token " + tok
	}
	s.crmDispatchWith(ctx, t.ID, e.ev.ConsumerID, params, now, crmDispatchOpts{Scope: crmEventScopeKey(e.ev.Payload), Template: tplID, Payload: e.ev.Payload})
	return true, ""
}

// crmEventScopeKey is the claim scope of one outbox event: what the emitter
// named explicitly (scope_key), else the complaint, else the order the event
// is about, else "" (a per-day claim). Two orders on one day are two scopes,
// so each gets its own D-01 and D-06; two complaints on one order are two
// scopes too, so the complaint wins over the order it names; a scheduled
// trigger has no event and keeps the per-day claim.
func crmEventScopeKey(payload map[string]any) string {
	for _, k := range []string{"scope_key", "complaint_id", "order_id"} {
		if v, _ := payload[k].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// crmCondWarned keys (trigger, error) pairs already logged: a condition that
// fails closed is reported once per process, not once per event.
var crmCondWarned sync.Map

func (s *service) crmCondFailClosed(t crmTrigger, err error) {
	if _, dup := crmCondWarned.LoadOrStore(t.ID+"|"+err.Error(), true); dup {
		return
	}
	s.log.Warn("crm: condition fails closed, trigger not fired", "trigger", t.ID, "err", err)
}

// crmEventCtx resolves the condition keys of one outbox event. The order
// behind an order_id is looked up at most once, lazily.
type crmEventCtx struct {
	s   *service
	ctx context.Context
	ev  crmEvent
	// now is the moment the message is sent (set by crmFireTrigger): a
	// delayed trigger words its day tokens against it, not the event's.
	now     time.Time
	ord     *order
	ordDone bool
	off     *consumerOffer
	offDone bool
	prm     map[string]string
}

func (e *crmEventCtx) order() *order {
	if !e.ordDone {
		e.ordDone = true
		if id, _ := e.ev.Payload["order_id"].(string); id != "" {
			if o, err := e.s.repo.findOrderAnyUser(e.ctx, id); err == nil {
				e.ord = o
			}
		}
	}
	return e.ord
}

// params builds the template params for this event once: the topic's own
// tokens (crmEventParams) over the standard ones (crmStandardParams).
func (e *crmEventCtx) params() map[string]string {
	if e.prm != nil {
		return e.prm
	}
	var o *order
	if e.ev.Topic == "complaint.created" { // [AMOUNT] and the label come from the order
		o = e.order()
	}
	p := crmEventParams(e.ev.Topic, e.ev.Payload, o)
	switch e.ev.Topic {
	case "subscription.created_unpaid":
		// A-05 fires two hours after the plan was made: [DATE] is the morning
		// the plan starts as seen NOW (a tomorrow the noon lock has skipped
		// meanwhile is not offered), not the label the emitter wrote.
		if lbl := e.planStartLabel(); lbl != "" {
			p["DATE"] = lbl
		}
	case "offer.finalized":
		// T-W01-LATER's [DATE]: the morning pack 1 comes, worded when W-01
		// is sent.
		if day, _ := e.ev.Payload["pack1_day"].(string); strings.TrimSpace(day) != "" {
			p["DATE"] = crmDayLabel(day, e.sentAt())
		}
	case "founding.farm_unlocked":
		// An event written before the emitter carried the label: the first
		// morning still open to orders when the farm unlocked.
		if _, ok := p["DATE"]; !ok && !e.ev.CreatedAt.IsZero() {
			p["DATE"] = crmDayLabel(firstEditableDay(e.ev.CreatedAt), e.sentAt())
		}
	}
	for k, v := range e.s.crmStandardParams(e.ctx, e.ev.ConsumerID) {
		if _, taken := p[k]; !taken {
			p[k] = v
		}
	}
	e.prm = p
	return p
}

// sentAt is the moment the message goes out: the fire clock, else the wall.
func (e *crmEventCtx) sentAt() time.Time {
	if !e.now.IsZero() {
		return e.now
	}
	return time.Now()
}

// planStartLabel words the first morning the event's plan still delivers,
// as seen when the message is sent; "" when the plan cannot be read or
// delivers nothing in the next three weeks.
func (e *crmEventCtx) planStartLabel() string {
	subID, _ := e.ev.Payload["subscription_id"].(string)
	if subID = strings.TrimSpace(subID); subID == "" {
		return ""
	}
	sub, err := e.s.repo.findSubscriptionByID(e.ctx, subID)
	if err != nil || sub == nil || sub.ConsumerID != e.ev.ConsumerID {
		return ""
	}
	at := e.sentAt()
	if day := e.s.nextDeliveryFor(e.ctx, sub, at); day != "" {
		return crmDayLabel(day, at)
	}
	return ""
}

// fact resolves one condition key. Only the keys listed here exist; any
// other key is unknown and the caller fails closed.
func (e *crmEventCtx) fact(key string) (any, error) {
	p := e.ev.Payload
	switch key {
	case "order.is_promotional_only":
		if v, ok := p["promotional_only"].(bool); ok {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no promotional_only")
	case "product_line":
		o := e.order()
		if o == nil {
			return nil, fmt.Errorf("order not found for product_line")
		}
		if o.Lane == "instant" {
			return "quick_pyaas", nil
		}
		return "morning_delivery", nil
	case "orders_count":
		// The consumer's completed orders, this one included once delivered.
		n, err := e.s.repo.orders.CountDocuments(e.ctx, bson.D{
			{Key: "user_id", Value: e.ev.ConsumerID.Hex()}, {Key: "status", Value: "delivered"},
		})
		if err != nil {
			return nil, err
		}
		return float64(n), nil
	case "member.started":
		// The member has begun: an order that is not cancelled (placed, on its
		// way or delivered), a recharge, or a plan that is not cancelled.
		// orders_count counts DELIVERED orders only, so on its own it let A-01
		// ("recharge your Wallet & place your first order") reach a member who
		// had recharged and whose first order was still on its way.
		return e.s.crmMemberStarted(e.ctx, e.ev.ConsumerID)
	case "complaint.type":
		if v, ok := p["category"].(string); ok && v != "" {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no category")
	case "new_eta_known":
		if v, ok := p["new_eta_known"].(bool); ok {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no new_eta_known")
	case "rating":
		if f, ok := crmPayloadNumber(p["rating"]); ok {
			return f, nil
		}
		return nil, fmt.Errorf("payload carries no rating")
	case "offer.id":
		// The consumer's campaign offer id; "" when they have none, so a
		// W trigger's "offer.id == 'welcome_litre'" reads false, never errors.
		if o := e.offer(); o != nil {
			return o.OfferID, nil
		}
		return "", nil
	case "offer.entitled_free_deliveries_remaining":
		return float64(entitledFreeDeliveries(e.offer())), nil
	case "wallet.topup_balance":
		wv, err := e.s.wallet(e.ctx, e.ev.ConsumerID)
		if err != nil {
			return nil, err
		}
		return wv.Cash, nil
	case "wallet.covers_first_cycle":
		// The live wallet against the cycle cost the emitter recorded, so a
		// top-up made during the delay is seen at fire time.
		amt, ok := crmPayloadNumber(p["first_cycle_amount"])
		if !ok {
			return nil, fmt.Errorf("payload carries no first_cycle_amount")
		}
		wv, err := e.s.wallet(e.ctx, e.ev.ConsumerID)
		if err != nil {
			return nil, err
		}
		return wv.Available >= amt, nil
	case "change":
		if v, ok := p["change"].(string); ok && v != "" {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no change")
	case "complaint_open":
		n, err := e.s.repo.complaints().CountDocuments(e.ctx, bson.D{
			{Key: "consumer_id", Value: e.ev.ConsumerID},
			{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{complaintOpen, complaintInReview}}}},
		})
		if err != nil {
			return nil, err
		}
		return n > 0, nil
	case "before_delivery":
		if v, ok := p["before_delivery"].(bool); ok {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no before_delivery")
	case "serviceability.in_zone":
		if v, ok := p["in_zone"].(bool); ok {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no in_zone")
	case "credit.account":
		if v, ok := p["account"].(string); ok && v != "" {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no account")
	case "order.contains_promotional_line":
		// The shipped model mints each free pack as its own order, so an
		// order carries a promotional line iff it is a pack order.
		n, _ := crmPayloadNumber(p["offer_pack"])
		return n > 0, nil
	case "offer.pack1_tomorrow":
		// W-01 (offer.finalized): pack 1 comes tomorrow as seen when the
		// message goes out. From 12:00 IST it comes the day after tomorrow
		// (G9), and T-W01's registered "Tomorrow by 7 am" would be untrue.
		// An event from before the payload carried the day was always for
		// tomorrow.
		day, _ := p["pack1_day"].(string)
		if strings.TrimSpace(day) == "" {
			return true, nil
		}
		return day == istDay(e.sentAt().Add(24*time.Hour)), nil
	case "tomorrow.delivery_blocked":
		// subscription.day_skipped (D-07): the skipped day is tomorrow as the
		// lock saw it and no free Welcome Litre pack still arrives that
		// morning; the emitter decides it (skipSubPreview).
		if v, ok := p["tomorrow_blocked"].(bool); ok {
			return v, nil
		}
		return nil, fmt.Errorf("payload carries no tomorrow_blocked")
	}
	return nil, fmt.Errorf("unknown condition key %q", key)
}

// crmMemberStarted answers the member.started condition key: any order that
// is not cancelled, any TOPUP ledger row, or any plan that is not cancelled.
func (s *service) crmMemberStarted(ctx context.Context, cid primitive.ObjectID) (any, error) {
	one := options.Count().SetLimit(1)
	notCancelled := bson.D{{Key: "$ne", Value: "cancelled"}}
	for _, q := range []struct {
		col    *mongo.Collection
		filter bson.D
	}{
		{s.repo.orders, bson.D{{Key: "user_id", Value: cid.Hex()}, {Key: "status", Value: notCancelled}}},
		{s.repo.walletTxns, bson.D{{Key: "consumer_id", Value: cid}, {Key: "type", Value: "TOPUP"}}},
		{s.repo.subscriptions, bson.D{{Key: "consumer_id", Value: cid}, {Key: "status", Value: notCancelled}}},
	} {
		n, err := q.col.CountDocuments(ctx, q.filter, one)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// offer resolves the consumer's campaign offer at most once, lazily.
func (e *crmEventCtx) offer() *consumerOffer {
	if !e.offDone {
		e.offDone = true
		if o, err := e.s.repo.findOffer(e.ctx, e.ev.ConsumerID); err == nil {
			e.off = o
		}
	}
	return e.off
}

// crmCondition is one parsed line of a trigger's conditions. Shapes: a
// boolean literal, "a == b", "a != b", "a < b" (and <=, >, >=, numbers
// only), and "a in ['x','y']". Anything else is unparsable and fails closed.
type crmCondition struct {
	key string
	op  string
	val any      // string, float64, bool or nil
	set []string // op "in"
}

func crmParseCondition(expr string) (crmCondition, error) {
	s := strings.TrimSpace(expr)
	switch s {
	case "true", "false":
		return crmCondition{op: "literal", val: s == "true"}, nil
	}
	if strings.Contains(s, "||") || strings.Contains(s, "&&") || strings.Contains(s, "(") {
		return crmCondition{}, fmt.Errorf("unsupported condition %q", expr)
	}
	if i := strings.Index(s, " in ["); i > 0 && strings.HasSuffix(s, "]") {
		c := crmCondition{key: strings.TrimSpace(s[:i]), op: "in"}
		for _, item := range strings.Split(s[i+len(" in ["):len(s)-1], ",") {
			v, err := crmParseOperand(strings.TrimSpace(item))
			if err != nil {
				return crmCondition{}, fmt.Errorf("condition %q: %w", expr, err)
			}
			str, ok := v.(string)
			if !ok {
				return crmCondition{}, fmt.Errorf("condition %q: list items must be quoted", expr)
			}
			c.set = append(c.set, str)
		}
		if len(c.set) == 0 {
			return crmCondition{}, fmt.Errorf("condition %q: empty list", expr)
		}
		return c, nil
	}
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if i := strings.Index(s, " "+op+" "); i > 0 {
			v, err := crmParseOperand(strings.TrimSpace(s[i+len(op)+2:]))
			if err != nil {
				return crmCondition{}, fmt.Errorf("condition %q: %w", expr, err)
			}
			return crmCondition{key: strings.TrimSpace(s[:i]), op: op, val: v}, nil
		}
	}
	return crmCondition{}, fmt.Errorf("unparsable condition %q", expr)
}

func crmParseOperand(s string) (any, error) {
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if n := len(s); n >= 2 && ((s[0] == 0x27 && s[n-1] == 0x27) || (s[0] == '"' && s[n-1] == '"')) {
		return s[1 : n-1], nil // 0x27 is the single quote
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("unparsable operand %q", s)
}

// eval resolves the key through facts and compares. A type mismatch between
// the fact and the operand is an error (fails closed), never a silent false.
func (c crmCondition) eval(facts func(string) (any, error)) (bool, error) {
	if c.op == "literal" {
		return c.val.(bool), nil
	}
	v, err := facts(c.key)
	if err != nil {
		return false, err
	}
	switch c.op {
	case "in":
		s, ok := v.(string)
		if !ok {
			return false, fmt.Errorf("%s is not a string", c.key)
		}
		for _, x := range c.set {
			if x == s {
				return true, nil
			}
		}
		return false, nil
	case "==", "!=":
		eq, err := crmOperandEqual(v, c.val)
		if err != nil {
			return false, fmt.Errorf("%s: %w", c.key, err)
		}
		return eq == (c.op == "=="), nil
	}
	a, okA := crmPayloadNumber(v)
	b, okB := crmPayloadNumber(c.val)
	if !okA || !okB {
		return false, fmt.Errorf("%s: %s needs numbers", c.key, c.op)
	}
	switch c.op {
	case "<":
		return a < b, nil
	case "<=":
		return a <= b, nil
	case ">":
		return a > b, nil
	case ">=":
		return a >= b, nil
	}
	return false, fmt.Errorf("unknown operator %q", c.op)
}

func crmOperandEqual(v, want any) (bool, error) {
	switch w := want.(type) {
	case nil:
		return v == nil, nil
	case bool:
		b, ok := v.(bool)
		if !ok {
			return false, fmt.Errorf("expected a boolean, got %T", v)
		}
		return b == w, nil
	case string:
		s, ok := v.(string)
		if !ok {
			return false, fmt.Errorf("expected a string, got %T", v)
		}
		return s == w, nil
	case float64:
		f, ok := crmPayloadNumber(v)
		if !ok {
			return false, fmt.Errorf("expected a number, got %T", v)
		}
		return f == w, nil
	}
	return false, fmt.Errorf("unsupported operand %T", want)
}

// crmEvalConditions is the AND of every line; an empty list holds.
func crmEvalConditions(conds []string, facts func(string) (any, error)) (bool, error) {
	for _, expr := range conds {
		c, err := crmParseCondition(expr)
		if err != nil {
			return false, err
		}
		ok, err := c.eval(facts)
		if err != nil {
			return false, fmt.Errorf("condition %q: %w", expr, err)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// crmPayloadNumber reads a number however the BSON decoder handed it back.
func crmPayloadNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// crmRupees renders a rupee amount for a template: whole rupees plain,
// otherwise two decimals.
func crmRupees(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// crmEventParams builds the UPPERCASE template params for one lifecycle
// topic from its payload (contract C6). o is the order behind order_id when
// the caller looked it up (complaint.created needs it for [AMOUNT] and the
// label); nil otherwise. ORDER_ID and COMPLAINT_ID ride along for push data.
func crmEventParams(topic string, payload map[string]any, o *order) map[string]string {
	str := func(k string) string {
		v, _ := payload[k].(string)
		return strings.TrimSpace(v)
	}
	p := map[string]string{}
	if v := str("order_id"); v != "" {
		p["ORDER_ID"] = v
	}
	if v := str("labelled_product"); v != "" {
		p["LABELLED_PRODUCT"] = v
	} else if o != nil {
		p["LABELLED_PRODUCT"] = crmLabelledProductOf(o)
	}
	switch topic {
	case "order.confirmed":
		if v := str("eta"); v != "" {
			p["ETA"] = v
		}
	case "order.dispatched":
		if v := str("partner"); v != "" {
			p["PARTNER"] = v
		}
		if n, ok := crmPayloadNumber(payload["eta_min"]); ok {
			p["ETA_MIN"] = strconv.Itoa(int(n))
		}
	case "order.failed":
		if v := str("reason"); v != "" {
			p["REASON"] = v
		}
	case "complaint.created", "complaint.resolved":
		if v := str("ref"); v != "" {
			p["REF"] = v
		}
		if v := str("complaint_id"); v != "" {
			p["COMPLAINT_ID"] = v
		}
		if v := str("resolution"); v != "" {
			p["RESOLUTION"] = v
		}
		if topic == "complaint.created" {
			p["SLA"] = crmComplaintSLA
			if o != nil {
				p["AMOUNT"] = crmRupees(o.Total)
			}
		}
	case "rating.submitted":
		if n, ok := crmPayloadNumber(payload["rating"]); ok {
			p["RATING"] = strconv.Itoa(int(n))
		}
	case "wallet.credited", "order.line_cancelled":
		// [X] is the rupee amount credited (B-06) or taken off the bill (D-05).
		if n, ok := crmPayloadNumber(payload["amount"]); ok {
			p["X"] = crmRupees(n)
		}
		if v := str("reason"); v != "" {
			p["REASON"] = v
		}
	case "subscription.activated", "subscription.created_unpaid":
		// [DATE] in T-A03 / T-A05 is the first delivery morning, worded by the
		// emitter ("today", "tomorrow", "2 Jan"); it overrides the Welcome
		// Litre DATE. A-05 re-words it when it fires (crmEventCtx.params).
		if v := str("start_label"); v != "" {
			p["DATE"] = v
		}
	case "subscription.day_skipped":
		// [DATE] in T-D07: the next morning the plan delivers after the
		// skipped one, worded by the lock that skipped it.
		if v := str("resume_label"); v != "" {
			p["DATE"] = v
		}
	case "delivery.delayed":
		if v := str("eta"); v != "" {
			p["ETA"] = v
		}
	case "payment.failed":
		if v := str("reason"); v != "" {
			p["REASON"] = v
		}
	case "founding.farm_unlocked", "founding.member_active", "founding.seat_waiting":
		// Spec section 6: the farm and farmer by name, the member's place in
		// line and the homes still to go.
		if v := str("farm"); v != "" {
			p["FARM"] = v
		}
		if v := str("farmer"); v != "" {
			p["FARMER"] = v
		}
		if n, ok := crmPayloadNumber(payload["line"]); ok && n > 0 {
			p["LINE"] = strconv.Itoa(int(n))
		}
		if n, ok := crmPayloadNumber(payload["togo"]); ok {
			p["TOGO"] = strconv.Itoa(int(n))
		}
		// [DATE] in T-FF01: the first morning still open to orders at the
		// unlock (tomorrow before 12 noon, the day after from noon).
		if v := str("first_delivery_label"); v != "" {
			p["DATE"] = v
		}
	case "referral.applied", "referral.rewarded":
		if n, ok := crmPayloadNumber(payload["reward_amount"]); ok {
			p["AMOUNT"] = crmRupees(n)
		}
	}
	return p
}

// crmTemplateResolvable reports the first token of any body variant that
// params cannot fill, or ok when every token resolves.
func crmTemplateResolvable(tpl crmTemplate, params map[string]string) (string, bool) {
	for _, body := range []string{tpl.EN, tpl.HI, tpl.HIDev} {
		for _, m := range crmTokenRe.FindAllStringSubmatch(body, -1) {
			if v, ok := params[m[1]]; !ok || v == "" {
				return m[1], false
			}
		}
	}
	return "", true
}

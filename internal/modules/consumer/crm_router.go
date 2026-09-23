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

	"go.mongodb.org/mongo-driver/bson"
)

// crmLifecycleTopics are the outbox topics the lifecycle emits (contract C6).
// crmRouteGeneric serves every "event" trigger whose event names one of
// them; the token-coverage test walks this list.
var crmLifecycleTopics = []string{
	"order.confirmed", "order.dispatched", "order.delivered", "order.failed",
	"complaint.created", "complaint.resolved", "rating.submitted",
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
// template (E-05 pages a human), aliases and schedules. A trigger's "delay"
// is not honoured; it fires on the worker tick that drains the event. The
// (trigger, consumer, IST day) dispatch claim still applies, so a per_order
// trigger fires at most once per consumer per day.
func (s *service) crmRouteGeneric(ctx context.Context, ev crmEvent) {
	if ev.ConsumerID.IsZero() {
		return
	}
	cfg := crmConfigLoad()
	var ids []string
	for id, t := range cfg.Triggers {
		if t.Kind == "event" && t.Event == ev.Topic && t.Category != "internal" && t.Template.String() != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids) // deterministic dispatch order
	e := &crmEventCtx{s: s, ctx: ctx, ev: ev}
	var params map[string]string
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
		if params == nil {
			var o *order
			if ev.Topic == "complaint.created" { // [AMOUNT] and the label come from the order
				o = e.order()
			}
			params = crmEventParams(ev.Topic, ev.Payload, o)
			for k, v := range s.crmStandardParams(ctx, ev.ConsumerID) {
				if _, taken := params[k]; !taken {
					params[k] = v
				}
			}
		}
		// C-01: a message that cannot resolve its values is refused, never
		// half-rendered. Checked before the day claim so a later tick with
		// the value present can still fire.
		if tok, ok := crmTemplateResolvable(cfg.Templates[t.Template.String()], params); !ok {
			s.log.Warn("crm: unresolved template token, trigger not fired", "trigger", t.ID, "token", tok)
			continue
		}
		s.crmDispatch(ctx, t.ID, ev.ConsumerID, params)
	}
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
	s       *service
	ctx     context.Context
	ev      crmEvent
	ord     *order
	ordDone bool
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
	}
	return nil, fmt.Errorf("unknown condition key %q", key)
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

package consumer

// Every event trigger the backend can reach carries only tokens the router
// supplies for its topic: this is the "unresolved template token" refusal
// the audit found, pinned as a test against the embedded config.

import (
	"encoding/json"
	"testing"
)

// crmFactTokens are the tokens crmEventCtx.params fills from a fact, not the
// payload, keyed by the template branch whose condition guarantees them:
// T-E04 is chosen only when complaint.refundable_amount > 0, and exactly
// then [AMOUNT] is set (crmComplaintRefundable).
var crmFactTokens = map[string]map[string]string{
	"T-E04": {"AMOUNT": "139"},
}

func TestCRMEventTriggerTokensSupplied(t *testing.T) {
	cfg := crmConfigLoad()
	label := "Parag Gold Full Cream 500ml" + crmLabelledSuffix
	// One representative payload per lifecycle topic, shaped as the emitters
	// write it (contract C6) and as BSON hands it back.
	sample := map[string]map[string]any{
		"order.confirmed":    {"order_id": "ord_1", "labelled_product": label, "promotional_only": false, "eta": "today by 7 am"},
		"order.dispatched":   {"order_id": "ord_1", "labelled_product": label, "partner": "Ravi", "eta_min": int32(12)},
		"order.delivered":    {"order_id": "ord_1", "offer_pack": int32(0), "promotional_only": false, "labelled_product": label, "orders_count": int32(1)},
		"order.failed":       {"order_id": "ord_1", "labelled_product": label, "reason": "Customer not home"},
		"complaint.created":  {"complaint_id": "cmp_1", "ref": "PYS-1", "category": "missing", "order_id": "ord_1"},
		"complaint.resolved": {"complaint_id": "cmp_1", "ref": "PYS-1", "resolution": "Refunded one pack"},
		"rating.submitted":   {"order_id": "ord_1", "rating": int32(4)},
		"user.registered":    {"source": "otp"},
		"wallet.credited":    {"amount": float64(500), "account": "topup", "reason": "recharge", "ref": "order_1", "scope_key": "order_1"},
		"payment.failed":     {"payment_order_id": "order_1", "payment_id": "pay_1", "amount": float64(500), "reason": "declined", "source": "razorpay"},
		"subscription.activated": {"subscription_id": "sub_1", "product_id": "gold-500ml", "qty": int32(2),
			"frequency": "daily", "start_date": "2026-09-25", "start_label": "tomorrow"},
		"subscription.created_unpaid": {"subscription_id": "sub_1", "first_cycle_amount": float64(70), "start_label": "tomorrow"},
		"subscription.modified":       {"subscription_id": "sub_1", "change": "paused"},
		"subscription.day_skipped": {"subscription_id": "sub_1", "day": "2026-10-07", "reason": "wallet_short", "shortfall": float64(58),
			"resume_label": "8 Oct", "tomorrow_blocked": true, "scope_key": "day_skipped:2026-10-07"},
		"order.line_cancelled": {"order_id": "ord_1", "line_id": "item_1", "labelled_product": label,
			"amount": float64(35), "before_delivery": true},
		"delivery.delayed":       {"order_id": "ord_1", "labelled_product": label, "new_eta_known": true, "eta": "about 7:52 am"},
		"serviceability.checked": {"in_zone": false, "pincode": "226030", "source": "waitlist", "has_orders": false, "has_serviceable_address": false},
		// Founding Family (founding.go) and referrals (referrals.go), as the
		// emitters write them.
		"founding.farm_unlocked": {"farm_id": "gonard-dairy", "farm": "Gonard Dairy", "farmer": "Harsh Singh", "line": int32(12), "togo": int32(0), "unlocked_packs": "", "first_delivery_label": "tomorrow"},
		"founding.member_active": {"farm_id": "gonard-dairy", "farm": "Gonard Dairy", "farmer": "Harsh Singh", "line": int32(12), "togo": int32(0), "unlocked_packs": "", "first_delivery_label": "tomorrow"},
		"founding.seat_waiting":  {"farm_id": "gonard-dairy", "farm": "Gonard Dairy", "farmer": "Harsh Singh", "line": int32(12), "togo": int32(53)},
		"referral.applied":       {"referral_id": "r1", "referee_id": "u2", "code": "PGHZU4", "reward_amount": 100.0},
		"referral.rewarded":      {"referral_id": "r1", "order_id": "ord_1", "reward_amount": 100.0, "referrer_id": "u1", "referee_id": "u2"},
	}
	for _, topic := range crmLifecycleTopics {
		if _, ok := sample[topic]; !ok {
			t.Errorf("lifecycle topic %s has no sample payload in this test", topic)
		}
	}
	o := &order{Total: 70, Items: []orderItem{{Name: "Parag Gold Full Cream", Variant: "500ml"}}}
	// What crmStandardParams supplies unconditionally. DATE is deliberately
	// absent: it exists only for a Welcome Litre household.
	std := map[string]string{"LINK": "https://pyaasdairy.com/app", "SUPPORT_NUMBER": "96672 60050"}

	routed := map[string]bool{}
	for id, tr := range cfg.Triggers {
		if tr.Kind != "event" || tr.Category == "internal" || tr.Template.String() == "" {
			continue
		}
		payload, emitted := sample[tr.Event]
		if !emitted {
			continue // dead config: the backend emits no such topic
		}
		// A conditional template ({"if","then","else"}) must resolve on both
		// branches; the branch its condition selects may add a token the
		// router fills from a fact rather than the payload.
		branches := []string{tr.Template.String()}
		if tr.Template.If != "" {
			branches = append(branches, tr.Template.Else)
		}
		for _, tplID := range branches {
			params := crmEventParams(tr.Event, payload, o)
			for k, v := range std {
				if _, taken := params[k]; !taken {
					params[k] = v
				}
			}
			for k, v := range crmFactTokens[tplID] {
				params[k] = v
			}
			tpl, ok := cfg.Templates[tplID]
			if !ok {
				t.Errorf("%s names template %s which does not exist", id, tplID)
				continue
			}
			if tok, ok := crmTemplateResolvable(tpl, params); !ok {
				t.Errorf("%s (%s) template %s carries [%s], which the router cannot fill", id, tr.Event, tplID, tok)
			}
		}
		routed[id] = true
	}
	for _, id := range []string{"A-02", "D-01", "D-02", "D-06", "D-07", "D-09", "E-01", "E-02", "E-04", "E-06", "E-07", "W-02", "W-05", "FF-01", "FF-02", "FF-03"} {
		if !routed[id] {
			t.Errorf("%s should be reachable from a lifecycle topic", id)
		}
	}

	// The routing fixes: sms is the LAST fallback where the audit asked for
	// it, rcs (unshipped) is gone from D-02, and nothing lost an entry.
	last := func(id string) string {
		fb := cfg.Triggers[id].Delivery.Fallback
		if len(fb) == 0 {
			return ""
		}
		return fb[len(fb)-1].Channel
	}
	for _, id := range []string{"D-01", "D-02", "D-06", "D-09", "E-06", "B-01", "E-05", "FF-01", "FF-02", "FF-03"} {
		if last(id) != "sms" {
			t.Errorf("%s: last fallback = %q, want sms (%+v)", id, last(id), cfg.Triggers[id].Delivery)
		}
	}
	if d09 := cfg.Triggers["D-09"]; d09.Delivery.Primary != "push" || len(d09.Delivery.Fallback) != 2 || d09.Delivery.Fallback[0].Channel != "whatsapp" || d09.Event != "order.failed" {
		t.Errorf("D-09 routing: %+v", d09.Delivery)
	}
	if fb := cfg.Triggers["D-02"].Delivery.Fallback; len(fb) != 2 || fb[0].Channel != "whatsapp" || fb[0].After != "PT5M" {
		t.Errorf("D-02 fallback chain: %+v", fb)
	}
	if fb := cfg.Triggers["D-01"].Delivery.Fallback; len(fb) != 2 || fb[0].Channel != "whatsapp" || fb[0].After != "PT15M" {
		t.Errorf("D-01 kept its whatsapp entry: %+v", fb)
	}
	if fb := cfg.Triggers["D-06"].Delivery.Fallback; len(fb) != 2 || fb[0].Channel != "whatsapp" || fb[0].After != "PT30M" {
		t.Errorf("D-06 kept its whatsapp entry: %+v", fb)
	}
	if p := cfg.Triggers["B-01"].Delivery.Parallel; len(p) != 1 || p[0] != "push" {
		t.Errorf("B-01 kept its parallel push: %v", p)
	}

	// meta.trigger_count is the real count, and the template registry is intact.
	var meta struct {
		Meta struct {
			TriggerCount int `json:"trigger_count"`
		} `json:"meta"`
		Triggers  []json.RawMessage          `json:"triggers"`
		Templates map[string]json.RawMessage `json:"templates"`
	}
	if err := json.Unmarshal(embeddedCRMConfig, &meta); err != nil {
		t.Fatalf("embedded config: %v", err)
	}
	// 52 templates: the 50 of the founding merge, T-E04-REDELIVER and T-W01-LATER.
	if meta.Meta.TriggerCount != len(meta.Triggers) || len(meta.Triggers) != 58 || len(meta.Templates) != 52 {
		t.Fatalf("meta.trigger_count=%d triggers=%d templates=%d", meta.Meta.TriggerCount, len(meta.Triggers), len(meta.Templates))
	}
}

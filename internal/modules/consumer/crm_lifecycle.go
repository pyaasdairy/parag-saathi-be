// CRM lifecycle events (contract C6) and the generic event router's helpers.
//
// The order, complaint and rating flows emit into the crm_events outbox from
// their own choke points (delivery_svc.go, complaints.go, orders.go); the
// worker routes each topic to every event trigger in crm_triggers.json whose
// conditions hold (crm_engine.go crmRouteEvent -> crmRouteGeneric). Everything
// here is inert unless CRM_ENABLED, and a CRM failure never fails an order.
package consumer

import (
	"math"
	"strings"
	"time"
)

// crmLabelledProductOf renders the C-01 supply-source label from an order's
// first line: name, then the variant when the line carries one ("Parag Gold
// Full Cream Milk 500ml"). The order's own name field is never changed; the
// Saathi store screen matches stock by that exact name.
func crmLabelledProductOf(o *order) string {
	name := "500 ml Parag Full Cream"
	if o != nil && len(o.Items) > 0 && o.Items[0].Name != "" {
		name = o.Items[0].Name
		if v := strings.TrimSpace(o.Items[0].Variant); v != "" {
			name += " " + v
		}
	}
	return name + crmLabelledSuffix
}

// crmOrderETA words the [ETA] token for order.confirmed: an instant order
// carries the task's hard ETA ("by 3:25 pm"); a morning order names its day
// against the published delivery promise (crmDLTDeliveryBy).
func crmOrderETA(o *order, etaAt string, now time.Time) string {
	if etaAt != "" {
		if t, err := time.Parse(time.RFC3339, etaAt); err == nil {
			return "by " + strings.ToLower(t.In(istZone).Format("3:04 PM"))
		}
	}
	day := orderDeliveryDate(o)
	switch day {
	case "":
		return "by " + crmDLTDeliveryBy // as soon as possible: no day is promised
	case istDay(now):
		return "today by " + crmDLTDeliveryBy
	case istDay(now.Add(24 * time.Hour)):
		return "tomorrow by " + crmDLTDeliveryBy
	}
	if t, err := time.ParseInLocation("2006-01-02", day, istZone); err == nil {
		return t.Format("2 Jan") + " by " + crmDLTDeliveryBy
	}
	return "by " + crmDLTDeliveryBy
}

// crmDeliveryETAMinutes is the [ETA_MIN] token for order.dispatched: minutes
// to the task's hard ETA when it is still ahead, else a distance estimate
// (about 4 min per km, never under 5) so a late instant order still gets a
// number rather than a blank token.
func crmDeliveryETAMinutes(d *delivery, now time.Time) int {
	if d.EtaAt != "" {
		if t, err := time.Parse(time.RFC3339, d.EtaAt); err == nil && t.After(now) {
			if m := int(math.Ceil(t.Sub(now).Minutes())); m >= 1 {
				return m
			}
		}
	}
	m := int(math.Ceil(d.DistanceKm * 4))
	if m < 5 {
		m = 5
	}
	return m
}

// crmPartnerName is the [PARTNER] token: the rider's name, or a neutral
// stand-in when the party record is missing (riderName then echoes the raw
// party id, which must never reach a customer).
func crmPartnerName(name, partyID string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == partyID {
		return "your PYAAS rider"
	}
	return name
}

// crmFailureReasonForCustomer words a task's failure reason for the member.
// The rider app posts "<picklist label> | <free remark> | photo=... |
// geo=... | called=true": only the first segment is the customer-readable
// cause, the rest is the rider's evidence and must never reach a message.
// A bare picklist code is mapped through the server's own catalog; anything
// empty or unreadable becomes a neutral phrase.
func crmFailureReasonForCustomer(reason string) string {
	first := strings.TrimSpace(strings.SplitN(reason, "|", 2)[0])
	for _, r := range riderNDReasonCatalog {
		if first == r.Code {
			first = r.Label
			break
		}
	}
	if first == "" || strings.ContainsAny(first, "=\n") {
		return "the delivery could not be completed"
	}
	if len(first) > 80 {
		first = first[:80]
	}
	return first
}

// crmSubscriptionStartLabel is the [DATE] token of T-A03: the first morning
// the plan delivers on or after its start date, worded against `now`
// ("today", "tomorrow", else "2 Jan"). A plan that never delivers inside a
// fortnight reports its start date as written.
func crmSubscriptionStartLabel(sub *subscription, now time.Time) string {
	day := sub.StartDate
	for i := 0; i < 14; i++ {
		d := addDaysIST(sub.StartDate, i)
		if subscriptionDeliversOn(sub.Frequency, sub.StartDate, d) {
			day = d
			break
		}
	}
	return crmDayLabel(day, now)
}

// crmDayLabel words an IST day relative to now.
func crmDayLabel(day string, now time.Time) string {
	switch day {
	case istDay(now):
		return "today"
	case istDay(now.Add(24 * time.Hour)):
		return "tomorrow"
	}
	if t, err := time.ParseInLocation("2006-01-02", day, istZone); err == nil {
		return t.Format("2 Jan")
	}
	return day
}

// crmClockLabel is the [ETA] token of T-D03: a clock time worded as an
// estimate ("about 7:52 am").
func crmClockLabel(t time.Time) string {
	return "about " + strings.ToLower(t.In(istZone).Format("3:04 PM"))
}

// crmCreditAccount maps a wallet bucket onto the config's two ledger
// accounts (C-02): CASH is the refundable top-up balance, REWARDS the
// non-refundable Pyaas credit.
func crmCreditAccount(bucket string) string {
	if bucket == "REWARDS" {
		return "promo_credit"
	}
	return "topup"
}

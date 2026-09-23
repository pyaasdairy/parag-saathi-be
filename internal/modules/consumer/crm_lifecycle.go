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

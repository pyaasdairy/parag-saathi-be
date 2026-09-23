// CRM detectors: product events nothing in the request path emits because
// nobody is acting when they happen. The scheduler tick runs them.
//
// delivery.delayed - a task that is out for delivery past the end of its
// promised window (an instant task's eta_at; a morning task's slot end) and
// not yet delivered. Emitted once per task, with the new ETA the router
// needs for D-03 ("Running a little late - new ETA [ETA]"). A task that has
// not left the store yet has no honest ETA to give, so it is left alone and
// looked at again on the next tick.
package consumer

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// crmDelayGrace is how far past the window end a task must be before it is
// called late: the promise is a minute, not a second.
const crmDelayGrace = 5 * time.Minute

// crmMorningWindowEnd is the end of the subscription route ("05:00 - 07:30
// AM") when a slot cannot be parsed.
const crmMorningWindowEnd = "07:30 AM"

// crmDeliveryWindowEnd is the instant a task's delivery promise expires:
// eta_at for an instant task, else the slot's end time on the task's
// delivery day (IST). ok=false when the task carries no window at all.
func crmDeliveryWindowEnd(d *delivery) (time.Time, bool) {
	if d.EtaAt != "" {
		if t, err := time.Parse(time.RFC3339, d.EtaAt); err == nil {
			return t, true
		}
	}
	day := d.DeliveryDate
	if day == "" {
		if i := strings.Index(d.Slot, " "); i > 0 {
			if _, ok := parseDay(d.Slot[:i]); ok {
				day = d.Slot[:i]
			}
		}
	}
	if day == "" {
		return time.Time{}, false
	}
	end := crmMorningWindowEnd
	if i := strings.LastIndex(d.Slot, " - "); i >= 0 {
		end = strings.TrimSpace(d.Slot[i+3:])
	}
	t, err := time.ParseInLocation("2006-01-02 3:04 PM", day+" "+end, istZone)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02 3:04 PM", day+" "+crmMorningWindowEnd, istZone)
		if err != nil {
			return time.Time{}, false
		}
	}
	return t, true
}

// crmSweepDelayedDeliveries emits delivery.delayed for every open task whose
// window ended before now-grace and which is on the road. Once per task: the
// delay_notified_at stamp is a compare-and-set, so two replicas cannot both
// emit. Bounded to tasks created in the last two days - older open tasks are
// a console problem, not a customer message.
func (s *service) crmSweepDelayedDeliveries(ctx context.Context, now time.Time) {
	cur, err := s.repo.deliveries.Find(ctx, bson.D{
		{Key: "status", Value: "OUT_FOR_DELIVERY"},
		{Key: "delay_notified_at", Value: bson.D{{Key: "$exists", Value: false}}},
		{Key: "created_at", Value: bson.D{{Key: "$gte", Value: now.Add(-48 * time.Hour).UTC()}}},
	})
	if err != nil {
		return
	}
	var tasks []delivery
	if err := cur.All(ctx, &tasks); err != nil {
		return
	}
	for i := range tasks {
		d := &tasks[i]
		end, ok := crmDeliveryWindowEnd(d)
		if !ok || now.Before(end.Add(crmDelayGrace)) {
			continue
		}
		cid, cerr := primitive.ObjectIDFromHex(d.ConsumerID)
		if cerr != nil {
			continue
		}
		res, uerr := s.repo.deliveries.UpdateOne(ctx,
			bson.D{{Key: "delivery_id", Value: d.ID}, {Key: "delay_notified_at", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "delay_notified_at", Value: now.UTC()}}}})
		if uerr != nil || res.ModifiedCount == 0 {
			continue // another replica stamped it first
		}
		eta := now.Add(time.Duration(crmDeliveryETAMinutes(d, now)) * time.Minute)
		s.emitCRMEvent(ctx, "delivery.delayed", cid, map[string]any{
			"order_id": d.OrderID, "labelled_product": s.crmLabelledProduct(ctx, d.OrderID),
			"new_eta_known": true, "eta": crmClockLabel(eta), "window_end": end.UTC().Format(time.RFC3339),
		})
	}
}

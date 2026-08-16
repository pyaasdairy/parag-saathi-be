// Mandate auto-renewal worker — the scheduler that turns the mandate machinery
// (mandate.go) into REAL subscription auto-renewal.
//
// Everything else already existed: create → Razorpay-verify → active mandate
// with a next_charge timestamp, and runMandateCharge — the idempotent charge
// (one debit per (mandate, day), advance-at-most-once bookkeeping). What was
// missing was the tick: charges only fired from a dev-only manual endpoint.
//
// This worker closes the loop: every tick it finds ACTIVE mandates whose
// next_charge is due and charges each one through the SAME runMandateCharge
// path the manual endpoint uses — so scheduled, manual and retried charges all
// share one money gate and can never double-debit.
//
// Failure model: a charge that fails (typically INSUFFICIENT_FUNDS) is logged
// and left due — the next tick retries. The mandate's schedule only advances on
// success, so a shopper who tops up later the same day is charged exactly once
// for that day; a shopper who never tops up simply accrues no debits.
package consumer

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mandateTickEvery is how often due mandates are looked for. Charges are due at
// most once per day per mandate, so a coarse tick is plenty — the per-day ref
// makes any overlap harmless.
const mandateTickEvery = 10 * time.Minute

// dueMandates returns ACTIVE mandates whose next_charge has passed (oldest
// first). next_charge is always set at activation (verifyMandate), but a nil is
// tolerated as "due now" so a legacy row can never be stranded unchargeable.
func (r *repository) dueMandates(ctx context.Context, now time.Time, limit int64) ([]mandate, error) {
	cur, err := r.mandates.Find(ctx, bson.D{
		{Key: "status", Value: "active"},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "next_charge", Value: bson.D{{Key: "$lte", Value: now}}}},
			bson.D{{Key: "next_charge", Value: nil}},
		}},
	}, options.Find().SetSort(bson.D{{Key: "next_charge", Value: 1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	var due []mandate
	if err := cur.All(ctx, &due); err != nil {
		return nil, err
	}
	return due, nil
}

// mandateAutoRenewalWorker runs for the lifetime of the process. Always on —
// auto-renewal is core product behaviour, not an integration flag.
func (s *service) mandateAutoRenewalWorker(ctx context.Context) {
	// boot delay: let mongo/indexes settle before the first sweep
	select {
	case <-time.After(45 * time.Second):
	case <-ctx.Done():
		return
	}
	s.log.Info("mandate auto-renewal worker ON", "tick", mandateTickEvery)
	for {
		s.sweepDueMandates(ctx)
		select {
		case <-time.After(mandateTickEvery):
		case <-ctx.Done():
			return
		}
	}
}

// sweepDueMandates charges every due mandate once. Safe to run concurrently
// with the dev endpoint or a second replica: the per-(mandate, day) ref means
// only one debit can ever land per day.
func (s *service) sweepDueMandates(ctx context.Context) {
	now := time.Now().UTC()
	due, err := s.repo.dueMandates(ctx, now, 200)
	if err != nil {
		s.log.Warn("mandate sweep query failed", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	var charged, failed int
	for _, m := range due {
		if _, err := s.runMandateCharge(ctx, m.ConsumerID, m.MandateID, now); err != nil {
			failed++ // typically INSUFFICIENT_FUNDS — stays due, next tick retries
			s.log.Info("mandate auto-renewal charge deferred", "mandate", m.MandateID, "plan", m.Plan,
				"amount", m.Amount, "reason", err.Error())
			continue
		}
		charged++
		s.log.Info("mandate auto-renewal charged", "mandate", m.MandateID, "plan", m.Plan, "amount", m.Amount)
	}
	s.log.Info("mandate auto-renewal sweep done", "due", len(due), "charged", charged, "deferred", failed)
}

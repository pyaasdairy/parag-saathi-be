package consumer

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SUBSCRIPTION TRIAL ENGINE — the "2+2" welcome trial (2 PAID then 2 FREE).
//
// The free-pack funnel: a new shopper's gold-500ml (Gold full-cream) daily
// subscription pays for its FIRST 2 delivered days at full price, then the NEXT
// 2 delivered days are on us (charged 0), and from the 5th delivered day onward
// it charges normally.
//
// The window is measured in DELIVERED DAYS, never calendar days. A pause, a
// skipped day, or an out-of-stock morning does NOT burn a free day — the free
// window only opens after 2 REAL paid deliveries land, and only advances when a
// delivery actually completes. This is what makes the promise honest: the shopper
// always gets 2 paid + 2 free real deliveries regardless of how the calendar falls.
//
// Money is still gated by the wallet's exactly-once ref (delivery:<orderId>); the
// trial ledger is a SEPARATE per-shopper counter, independently idempotent by the
// delivery key so a retried/duplicated settle can never double-advance the window.

const collConsumerTrials = "consumer_trials"

const (
	trialPaidDays = 2 // full-price delivered days at the start of the trial (gold-500ml)
	trialFreeDays = 2 // free (charged 0) delivered days that follow

	trialPhasePaid = "paid" // still inside the first 2 full-price days
	trialPhaseFree = "free" // inside the 2 free days (charge 0)
	trialPhaseDone = "done" // trial exhausted — charge normally
)

// trialDeliveryKey is the per-shopper, per-DELIVERED-DAY idempotency key. Stable
// for a given (consumer, UTC day), so a re-run of the same day's settle reuses the
// same ledger entry and never double-counts: "sub:<consumerId>:<YYYY-MM-DD>".
func trialDeliveryKey(consumerID primitive.ObjectID, day string) string {
	return "sub:" + consumerID.Hex() + ":" + day
}

// istZone is India Standard Time (UTC+5:30) as a fixed offset (no tzdata needed).
var istZone = time.FixedZone("IST", 5*3600+1800)

// trialDay is the IST calendar day (YYYY-MM-DD) a delivery counts toward. The
// morning run is an IST-morning event, so dating in IST (not UTC) keeps one IST
// morning = exactly one trial day, regardless of the 05:30-IST UTC-midnight seam.
func trialDay(t time.Time) string { return t.In(istZone).Format("2006-01-02") }

// isTrialProduct reports whether a product id is covered by the 2-paid/2-free
// welcome trial. THE OFFER APPLIES ONLY TO THE FULL-CREAM SKU the funnel
// subscribes (PYAAS Gold, gold-*): a Taaza/Shakti/Chai subscription neither
// consumes trial days nor earns the free ones — the member stays a 2+2
// candidate until they complete the paid days on the full cream itself.
func isTrialProduct(productID string) bool {
	return strings.HasPrefix(productID, "gold-")
}

// ── Documents + wire shapes ─────────────────────────────────────────────────

// trialCharge is one recorded delivered-day decision — the per-key idempotency
// log entry. Storing the effective amount + phase lets a replay of the SAME key
// return the SAME result instead of recomputing against advanced state.
type trialCharge struct {
	Key       string  `bson:"key"       json:"key"`
	Effective float64 `bson:"effective" json:"effective"`
	Phase     string  `bson:"phase"     json:"phase"`
}

// consumerTrial is the per-shopper welcome-trial ledger (one row per consumer;
// keyed by consumerId, extendable to per-subscription later by adding a scope
// field to the unique key). deliveredPaid/deliveredFree count REAL delivered days.
type consumerTrial struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"    json:"-"`
	ConsumerID    primitive.ObjectID `bson:"consumer_id"      json:"-"`
	DeliveredPaid int                `bson:"delivered_paid"   json:"deliveredPaid"`
	DeliveredFree int                `bson:"delivered_free"   json:"deliveredFree"`
	Phase         string             `bson:"phase"            json:"phase"`
	// Charges is the per-charge idempotency log (a set of charged delivery keys +
	// the decision each produced) so a re-run never double-advances the window.
	Charges   []trialCharge `bson:"charges"    json:"-"`
	Seq       int64         `bson:"seq"        json:"-"` // optimistic-concurrency guard
	CreatedAt time.Time     `bson:"created_at" json:"-"`
	UpdatedAt time.Time     `bson:"updated_at" json:"-"`
}

// trialView is GET /consumer/trial/me.
type trialView struct {
	Phase         string `json:"phase"`
	DeliveredPaid int    `json:"deliveredPaid"`
	DeliveredFree int    `json:"deliveredFree"`
	PaidRemaining int    `json:"paidRemaining"`
	FreeRemaining int    `json:"freeRemaining"`
	FreeActive    bool   `json:"freeActive"`
}

// ── Pure trial logic (no I/O — fully unit-testable) ─────────────────────────

// trialPhaseFor is the phase the shopper is CURRENTLY in (i.e. what the NEXT
// delivered day will be), given how many paid/free days have already landed.
func trialPhaseFor(deliveredPaid, deliveredFree int) string {
	switch {
	case deliveredPaid < trialPaidDays:
		return trialPhasePaid
	case deliveredFree < trialFreeDays:
		return trialPhaseFree
	default:
		return trialPhaseDone
	}
}

// trialApply computes the effective amount + phase for ONE new distinct delivered
// day and returns the advanced counts. FIRST 3 → full (paid++); NEXT 3 → 0
// (free++); after 6 → full ("done"). Pure: the caller owns persistence.
func trialApply(deliveredPaid, deliveredFree int, fullAmount float64) (effective float64, phase string, newPaid, newFree int) {
	switch {
	case deliveredPaid < trialPaidDays:
		return fullAmount, trialPhasePaid, deliveredPaid + 1, deliveredFree
	case deliveredFree < trialFreeDays:
		return 0, trialPhaseFree, deliveredPaid, deliveredFree + 1
	default:
		return fullAmount, trialPhaseDone, deliveredPaid, deliveredFree
	}
}

// findCharge returns the recorded decision for a delivery key, if it was already
// charged (the idempotency lookup).
func (t *consumerTrial) findCharge(key string) (trialCharge, bool) {
	for _, c := range t.Charges {
		if c.Key == key {
			return c, true
		}
	}
	return trialCharge{}, false
}

// effectiveForPhase applies a day's already-decided trial phase to a delivery's
// OWN full amount: a FREE day charges 0, any other phase charges in full. Keeping
// this split from the counter advance means a SECOND delivery on the same trial
// day (e.g. milk + curd in one morning) is charged its OWN amount — not a replay
// of the first delivery's amount — while the day still counts once toward the
// window. The recorded trialCharge.Effective stays as an audit of the first
// delivery that opened the day; the money charged is always per-delivery.
func effectiveForPhase(phase string, fullAmount float64) float64 {
	if phase == trialPhaseFree {
		return 0
	}
	return fullAmount
}

// charge applies ONE delivered day for `key`, mutating the ledger in place, and
// returns the effective amount + the phase THIS charge fell in. Idempotent by
// key: a replay returns the recorded result and does NOT advance the window. Pure
// (no I/O) so the whole progression is unit-testable without a database.
func (t *consumerTrial) charge(key string, fullAmount float64) (effective float64, phase string) {
	if rec, ok := t.findCharge(key); ok {
		// Replay: no double count. Apply the day's decided phase to THIS delivery's
		// amount so a same-day second delivery pays its own price (0 on a free day).
		return effectiveForPhase(rec.Phase, fullAmount), rec.Phase
	}
	eff, ph, np, nf := trialApply(t.DeliveredPaid, t.DeliveredFree, fullAmount)
	t.DeliveredPaid, t.DeliveredFree = np, nf
	t.Phase = trialPhaseFor(np, nf)
	t.Charges = append(t.Charges, trialCharge{Key: key, Effective: eff, Phase: ph})
	return eff, ph
}

// view derives the GET /trial/me projection from the current ledger.
func (t *consumerTrial) view() trialView {
	return trialView{
		Phase:         trialPhaseFor(t.DeliveredPaid, t.DeliveredFree),
		DeliveredPaid: t.DeliveredPaid,
		DeliveredFree: t.DeliveredFree,
		PaidRemaining: max(0, trialPaidDays-t.DeliveredPaid),
		FreeRemaining: max(0, trialFreeDays-t.DeliveredFree),
		// Free is "active" only once the paid quota is met and free days remain.
		FreeActive: t.DeliveredPaid >= trialPaidDays && t.DeliveredFree < trialFreeDays,
	}
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) ensureTrialIndexes(ctx context.Context) error {
	_, err := r.trials.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "consumer_id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return err
}

// getOrCreateTrial returns the shopper's trial ledger, upserting a fresh one on
// first use (atomic — a concurrent first-touch collapses to one row via the
// unique consumer_id index).
func (r *repository) getOrCreateTrial(ctx context.Context, consumerID primitive.ObjectID) (*consumerTrial, error) {
	now := time.Now().UTC()
	after := options.After
	var t consumerTrial
	err := r.trials.FindOneAndUpdate(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}},
		bson.D{{Key: "$setOnInsert", Value: bson.D{
			{Key: "consumer_id", Value: consumerID},
			{Key: "delivered_paid", Value: 0},
			{Key: "delivered_free", Value: 0},
			{Key: "phase", Value: trialPhasePaid},
			{Key: "charges", Value: []trialCharge{}},
			{Key: "seq", Value: int64(0)},
			{Key: "created_at", Value: now},
			{Key: "updated_at", Value: now},
		}}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(after),
	).Decode(&t)
	if err != nil {
		return nil, errInternal("trial lookup failed")
	}
	return &t, nil
}

// saveTrial persists an advanced ledger, GUARDED on the seq it was loaded at, so a
// concurrent advance can't clobber this one (lost update). Reports whether THIS
// call won the write; a false means someone else advanced first — reload + retry.
func (r *repository) saveTrial(ctx context.Context, t *consumerTrial) (bool, error) {
	res, err := r.trials.UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: t.ConsumerID}, {Key: "seq", Value: t.Seq}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "delivered_paid", Value: t.DeliveredPaid},
			{Key: "delivered_free", Value: t.DeliveredFree},
			{Key: "phase", Value: t.Phase},
			{Key: "charges", Value: t.Charges},
			{Key: "seq", Value: t.Seq + 1},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
	if err != nil {
		return false, errInternal("trial save failed")
	}
	return res.ModifiedCount == 1, nil
}

// ── Service ─────────────────────────────────────────────────────────────────

// trialChargeFor runs one subscription delivery's amount through the welcome
// trial and returns the EFFECTIVE amount to debit (0 on a free day) plus the
// phase THIS delivery fell in. Idempotent by deliveryKey: replaying the same key
// returns the same result and never double-advances the window. The window counts
// DELIVERED days — so a paused/skipped day is simply a key that was never charged
// and thus never burns a free day.
func (s *service) trialChargeFor(ctx context.Context, consumerID primitive.ObjectID, deliveryKey string, fullAmount float64) (float64, string, error) {
	full := round2(fullAmount)
	// A handful of attempts absorbs the rare optimistic-concurrency race (two
	// settles for the same shopper landing at once); each retry re-reads fresh.
	for attempt := 0; attempt < 5; attempt++ {
		t, err := s.repo.getOrCreateTrial(ctx, consumerID)
		if err != nil {
			return 0, "", err
		}
		if rec, ok := t.findCharge(deliveryKey); ok {
			return effectiveForPhase(rec.Phase, full), rec.Phase, nil // idempotent replay
		}
		eff, phase := t.charge(deliveryKey, full)
		ok, err := s.repo.saveTrial(ctx, t) // guarded on the loaded seq
		if err != nil {
			return 0, "", err
		}
		if ok {
			return eff, phase, nil
		}
		// Lost the race — the winner may have taken this very key; loop re-reads.
	}
	// Exhausted retries: return the recorded decision if the key landed, else fail
	// closed to the full amount (never accidentally give a free charge).
	t, err := s.repo.getOrCreateTrial(ctx, consumerID)
	if err != nil {
		return 0, "", err
	}
	if rec, ok := t.findCharge(deliveryKey); ok {
		return effectiveForPhase(rec.Phase, full), rec.Phase, nil
	}
	return full, trialPhaseFor(t.DeliveredPaid, t.DeliveredFree), nil
}

func (s *service) trialFor(ctx context.Context, consumerID primitive.ObjectID) (trialView, error) {
	t, err := s.repo.getOrCreateTrial(ctx, consumerID)
	if err != nil {
		return trialView{}, err
	}
	return t.view(), nil
}

// ── Handler ─────────────────────────────────────────────────────────────────

func (h *handler) trialMe(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	v, err := h.svc.trialFor(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

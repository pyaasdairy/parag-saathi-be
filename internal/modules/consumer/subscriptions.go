package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SERVER-OWNED SUBSCRIPTIONS — the backend twin of the consumer app's local
// subscription rows (lib/subscriptions.ts), so the DAILY MORNING ORDER exists
// even when nobody opens the app.
//
// Model (mirrors the FE exactly):
//   one subscription = ONE product line (product, variant, qty, unit price) +
//   a cadence (daily | alternate | weekly) anchored at start_date, minus any
//   vacation ranges (a skip is a one-day vacation, start == end).
//
// MONEY MODEL — no charge at creation. The worker only CREATES the order
// (payment_method "wallet"); money moves exactly once ON DELIVERY through the
// existing settle path (deliverDelivery → debit "delivery:<orderID>"), which
// also applies the Taaza 2-paid-2-free welcome trial. Charging here as well
// would double-debit — the AutoPay mandate (mandate.go) remains the wallet
// top-up backstop, not the per-delivery charge.
//
// ADDRESS MODEL — a subscription NEVER carries its own address. Every order the
// worker creates resolves the consumer's CURRENT default saved address (with
// lat/lng, so geofence routes it to the serving store). One saved address feeds
// every subscription — new subscriptions never re-ask for a location, and an
// address edit re-points every future delivery automatically.
//
// EXACTLY-ONCE — at most one order per (subscription, IST day), enforced by an
// atomic per-day claim (ordered_days, UpdateOne guarded on $ne day) plus a
// no-live-order-for-that-day check (claimSubscriptionDay). A crashed tick after
// the claim skips that day rather than ever double-ordering milk.

const collSubscriptions = "consumer_subscriptions"

// subscriptionDeliveryFee is the delivery fee on a SUBSCRIPTION morning delivery:
// always 0. The consumer app quotes and debits the bare subtotal on the Subscribe
// button, so the server-side recurring debit must match (never subtotal + ₹15).
const subscriptionDeliveryFee = 0.0

// NOTE: cadence math and day-claims use IST (istZone, shared with trial.go),
// matching the FE's local calendar dates, never UTC (UTC is "yesterday" until
// 05:30 IST).

// subscriptionFrequencies is the supported cadence set (FE Frequency minus the
// purely-local one_time/custom modes, which the app fulfils immediately itself).
var subscriptionFrequencies = map[string]bool{"daily": true, "alternate": true, "weekly": true}

// subscriptionTransitions — same shape as the mandate state machine.
var subscriptionTransitions = map[string]map[string]bool{
	"active":    {"paused": true, "cancelled": true},
	"paused":    {"active": true, "cancelled": true},
	"cancelled": {},
}

func subscriptionActionTarget(action string) (string, bool) {
	switch action {
	case "pause":
		return "paused", true
	case "resume":
		return "active", true
	case "cancel":
		return "cancelled", true
	default:
		return "", false
	}
}

// ── Documents + wire shapes (json mirrors the FE Subscription fields) ────────

type vacationRange struct {
	Start string `bson:"start" json:"start"` // YYYY-MM-DD inclusive
	End   string `bson:"end"   json:"end"`   // YYYY-MM-DD inclusive
}

type subscription struct {
	MongoID        primitive.ObjectID `bson:"_id,omitempty"            json:"-"`
	SubscriptionID string             `bson:"subscription_id"          json:"id"` // public "sub_…"
	ConsumerID     primitive.ObjectID `bson:"consumer_id"              json:"-"`
	ProductID      string             `bson:"product_id"               json:"product_id"`
	Name           string             `bson:"name"                     json:"name"`
	Variant        string             `bson:"variant,omitempty"        json:"variant"`
	Qty            int                `bson:"qty"                      json:"qty"`
	UnitPrice      float64            `bson:"unit_price"               json:"unit_price"`
	Frequency      string             `bson:"frequency"                json:"frequency"` // daily|alternate|weekly
	DeliverySlot   string             `bson:"delivery_slot,omitempty"  json:"delivery_slot,omitempty"`
	Status         string             `bson:"status"                   json:"status"`     // active|paused|cancelled
	StartDate      string             `bson:"start_date"               json:"start_date"` // YYYY-MM-DD (IST)
	Vacations      []vacationRange    `bson:"vacations,omitempty"      json:"vacations,omitempty"`
	// LastOrderDate is the exactly-once day claim (YYYY-MM-DD IST) — the worker
	// creates at most one order per subscription per day.
	LastOrderDate string `bson:"last_order_date,omitempty" json:"last_order_date,omitempty"`
	LastOrderID   string `bson:"last_order_id,omitempty"   json:"last_order_id,omitempty"`
	// OrderedDays lists every IST day already claimed (claimSubscriptionDay).
	OrderedDays []string  `bson:"ordered_days,omitempty" json:"-"`
	CreatedAt   time.Time `bson:"created_at"                json:"created_at"`
	UpdatedAt   time.Time `bson:"updated_at"                json:"updated_at"`
	// ChangedAt is the last MEMBER change (create, edit, pause, resume): the
	// noon rule compares it with a day's lock moment to decide whether that
	// day still belongs to the plan (subChangedBefore). updated_at cannot
	// serve: the worker's own claims stamp it too.
	ChangedAt time.Time `bson:"changed_at,omitempty" json:"-"`
	// NextDeliveryDate is derived on the wire (subscriptionNextDelivery): the
	// first day this plan still delivers, honouring the noon cut-off, so the
	// app can show when a plan created after noon really starts.
	NextDeliveryDate string `bson:"-" json:"next_delivery_date,omitempty"`
}

// subChangedBefore reports whether the plan's last member change predates
// t. A row from before changed_at existed (no stamp at all) is taken as
// unchanged - the worker's catch-up still serves it.
func (sub *subscription) subChangedBefore(t time.Time) bool {
	c := sub.ChangedAt
	if c.IsZero() {
		c = sub.CreatedAt
	}
	if c.IsZero() {
		return true
	}
	return c.Before(t)
}

// claimed reports whether this in-memory snapshot already holds day's claim.
func (sub *subscription) claimed(day string) bool {
	if sub.LastOrderDate == day {
		return true
	}
	for _, d := range sub.OrderedDays {
		if d == day {
			return true
		}
	}
	return false
}

func newSubscriptionID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "sub_" + hex.EncodeToString(b)
}

// istToday returns the current IST calendar day as YYYY-MM-DD.
func istToday(now time.Time) string { return now.In(istZone).Format("2006-01-02") }

// parseDay parses a YYYY-MM-DD into an IST-midnight time (cadence anchor).
func parseDay(iso string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", iso, istZone)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// subscriptionDeliversOn — the cadence law, IDENTICAL to the FE
// (lib/subscriptions.ts): daily every day, alternate every 2nd day, weekly
// every 7th, all counted from start_date. Days before the start never deliver.
func subscriptionDeliversOn(frequency, startDate, dayISO string) bool {
	from, ok1 := parseDay(startDate)
	to, ok2 := parseDay(dayISO)
	if !ok1 || !ok2 {
		return false
	}
	d := int(to.Sub(from).Hours() / 24)
	if d < 0 {
		return false
	}
	switch frequency {
	case "daily":
		return true
	case "alternate":
		return d%2 == 0
	case "weekly":
		return d%7 == 0
	default:
		return false
	}
}

// subscriptionDueOn — cadence AND not inside any vacation/skip range (dates are
// YYYY-MM-DD so string compare is chronological; the FE rule verbatim).
func subscriptionDueOn(sub *subscription, dayISO string) bool {
	if sub.Status != "active" || !subscriptionDeliversOn(sub.Frequency, sub.StartDate, dayISO) {
		return false
	}
	for _, v := range sub.Vacations {
		if dayISO >= v.Start && dayISO <= v.End {
			return false
		}
	}
	return true
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) ensureSubscriptionIndexes(ctx context.Context) error {
	specs := []mongo.IndexModel{
		{Keys: bson.D{{Key: "subscription_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}}}, // the worker scans active subs
	}
	_, err := r.subscriptions.Indexes().CreateMany(ctx, specs)
	return err
}

// subscriptionDayIndexName is the DB backstop behind claimSubscriptionDay: at
// most ONE live order per (subscription, day). The claim is what stops a
// second order on the happy path; the index is what stops it when two workers
// both believed they won (a pre-check read racing the other's insert, a
// replica whose claim update was lost). Partial on two counts: it applies only
// to rows that carry a subscription_id (one-off orders never collide), and
// only to LIVE statuses. The status clause is load-bearing: a pause before
// midnight cancels tomorrow's preview and releases the day, and the resume
// schedules that day again - a second row with the same key beside the
// cancelled one, which a full-key index would refuse and the resumed member
// would get no milk. ($in inside a partial filter needs MongoDB 6.0+; an older
// server refuses the build, the boot logs it and the claim stays the guard.)
const subscriptionDayIndexName = "subscription_day_unique"

// subscriptionLiveStatuses are the statuses the day index covers: everything
// an order holds between placement and delivery. "cancelled" is the one
// status left out, on purpose (see subscriptionDayIndexName).
var subscriptionLiveStatuses = bson.A{"placed", "confirmed", "assigned", "out_for_delivery", "delivered"}

// ensureSubscriptionDayIndex builds the (subscription_id, scheduled_for)
// unique index. When the build is refused because rows from before the index
// already collide, dups is how many (subscription, day) pairs hold more than
// one live order - what an operator has to clean up before the backstop can
// exist - and err is the refusal. Never fatal for the caller: the insert path
// stays protected by claimSubscriptionDay either way.
func (r *repository) ensureSubscriptionDayIndex(ctx context.Context) (dups int64, err error) {
	_, err = r.orders.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "subscription_id", Value: 1}, {Key: "scheduled_for", Value: 1}},
		Options: options.Index().SetName(subscriptionDayIndexName).SetUnique(true).
			SetPartialFilterExpression(bson.D{
				{Key: "subscription_id", Value: bson.D{{Key: "$exists", Value: true}}},
				{Key: "status", Value: bson.D{{Key: "$in", Value: subscriptionLiveStatuses}}},
			}),
	})
	if err == nil {
		return 0, nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return 0, err
	}
	dups, cerr := r.countSubscriptionDayDuplicates(ctx)
	if cerr != nil {
		return 0, fmt.Errorf("%w (duplicate count failed: %v)", err, cerr)
	}
	return dups, err
}

// countSubscriptionDayDuplicates counts the (subscription, day) pairs holding
// more than one live order - the rows the day index refuses to be built over.
func (r *repository) countSubscriptionDayDuplicates(ctx context.Context) (int64, error) {
	cur, err := r.orders.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "subscription_id", Value: bson.D{{Key: "$exists", Value: true}}},
			{Key: "status", Value: bson.D{{Key: "$in", Value: subscriptionLiveStatuses}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "sub", Value: "$subscription_id"}, {Key: "day", Value: "$scheduled_for"}}},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$match", Value: bson.D{{Key: "n", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
		{{Key: "$count", Value: "pairs"}},
	})
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)
	var out []struct {
		Pairs int64 `bson:"pairs"`
	}
	if err := cur.All(ctx, &out); err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, nil
	}
	return out[0].Pairs, nil
}

// insertSubscriptionOrderDoc inserts one subscription day's order. placed is
// false, with a nil error, when the day index already holds a live order for
// this (subscription, day): another worker placed it first, and "already
// placed today" is the whole outcome - no error, no second order. Any other
// duplicate (the order_id index) or failure is reported as insertOrder does.
func (r *repository) insertSubscriptionOrderDoc(ctx context.Context, o *order) (placed bool, err error) {
	if _, err := r.orders.InsertOne(ctx, o); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			if strings.Contains(err.Error(), "index: "+subscriptionDayIndexName) {
				return false, nil
			}
			return false, errConflict("ORDER_EXISTS", "order already exists")
		}
		return false, errInternal("order create failed")
	}
	return true, nil
}

// findLiveSubscriptionOrder returns the one live order for (subscription,
// day), or (nil, nil) when there is none.
func (r *repository) findLiveSubscriptionOrder(ctx context.Context, subID, day string) (*order, error) {
	var o order
	err := r.orders.FindOne(ctx, bson.D{
		{Key: "subscription_id", Value: subID},
		{Key: "scheduled_for", Value: day},
		{Key: "status", Value: bson.D{{Key: "$in", Value: subscriptionLiveStatuses}}},
	}).Decode(&o)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("subscription order lookup failed")
	}
	return &o, nil
}

func (r *repository) insertSubscription(ctx context.Context, s *subscription) error {
	if _, err := r.subscriptions.InsertOne(ctx, s); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("SUBSCRIPTION_EXISTS", "subscription already exists")
		}
		return errInternal("subscription create failed")
	}
	return nil
}

func (r *repository) findSubscription(ctx context.Context, subID string, consumerID primitive.ObjectID) (*subscription, error) {
	var s subscription
	err := r.subscriptions.FindOne(ctx, bson.D{{Key: "subscription_id", Value: subID}, {Key: "consumer_id", Value: consumerID}}).Decode(&s)
	if isNoDocs(err) {
		return nil, errNotFound("subscription not found")
	}
	if err != nil {
		return nil, errInternal("subscription lookup failed")
	}
	return &s, nil
}

func (r *repository) listSubscriptions(ctx context.Context, consumerID primitive.ObjectID) ([]subscription, error) {
	cur, err := r.subscriptions.Find(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}, {Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100))
	if err != nil {
		return nil, errInternal("subscriptions lookup failed")
	}
	out := []subscription{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("subscriptions decode failed")
	}
	return out, nil
}

// listActiveSubscriptions — the worker's scan (all consumers, active only),
// oldest plan first: the catch-up funds a member's plans in the same order
// the noon lock does (lockConsumerDay), whichever replica runs it.
func (r *repository) listActiveSubscriptions(ctx context.Context) ([]subscription, error) {
	cur, err := r.subscriptions.Find(ctx, bson.D{{Key: "status", Value: "active"}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "subscription_id", Value: 1}}).SetLimit(5000))
	if err != nil {
		return nil, errInternal("subscriptions scan failed")
	}
	out := []subscription{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("subscriptions scan decode failed")
	}
	return out, nil
}

func (r *repository) updateSubscription(ctx context.Context, subID string, consumerID primitive.ObjectID, set bson.D, guard bson.D) (*subscription, error) {
	filter := bson.D{{Key: "subscription_id", Value: subID}, {Key: "consumer_id", Value: consumerID}}
	filter = append(filter, guard...)
	set = append(set, bson.E{Key: "updated_at", Value: time.Now().UTC()})
	after := options.After
	var s subscription
	err := r.subscriptions.FindOneAndUpdate(ctx, filter,
		bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&s)
	if isNoDocs(err) {
		return nil, errConflict("SUBSCRIPTION_STATE", "subscription is not in the expected state")
	}
	if err != nil {
		return nil, errInternal("subscription update failed")
	}
	return &s, nil
}

// claimSubscriptionDay atomically claims (subscription, day) for order creation:
// the guard last_order_date != day admits exactly ONE winner per IST day —
// the same exactly-once shape as advanceMandateCharge. Returns whether THIS
// call won the claim.
//
// The claim is PER DAY (ordered_days), not just the single last_order_date:
// with only the latter, scheduling tomorrow overwrote today's claim, so the
// next tick claimed today again and the one after claimed tomorrow again — a
// fresh duplicate order (and delivery task, and debit) every other tick. As a
// second guard, a day that already has a live (non-cancelled) order is never
// claimed, which also covers subscriptions claimed before ordered_days existed
// and heals their claim list.
func (r *repository) claimSubscriptionDay(ctx context.Context, subID, day string) (bool, error) {
	live, err := r.orders.CountDocuments(ctx, bson.D{
		{Key: "subscription_id", Value: subID},
		{Key: "scheduled_for", Value: day},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
	}, options.Count().SetLimit(1))
	if err != nil {
		return false, errInternal("subscription day check failed")
	}
	if live > 0 {
		_, _ = r.subscriptions.UpdateOne(ctx,
			bson.D{{Key: "subscription_id", Value: subID}, {Key: "ordered_days", Value: bson.D{{Key: "$ne", Value: day}}}},
			bson.D{{Key: "$push", Value: bson.D{{Key: "ordered_days", Value: bson.D{
				{Key: "$each", Value: bson.A{day}}, {Key: "$slice", Value: -orderedDaysKept},
			}}}}})
		return false, nil
	}
	res, err := r.subscriptions.UpdateOne(ctx,
		bson.D{
			{Key: "subscription_id", Value: subID},
			{Key: "status", Value: "active"},
			{Key: "ordered_days", Value: bson.D{{Key: "$ne", Value: day}}},
		},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "last_order_date", Value: day},
				{Key: "updated_at", Value: time.Now().UTC()},
			}},
			{Key: "$push", Value: bson.D{{Key: "ordered_days", Value: bson.D{
				{Key: "$each", Value: bson.A{day}}, {Key: "$slice", Value: -orderedDaysKept},
			}}}},
		})
	if err != nil {
		return false, errInternal("subscription day claim failed")
	}
	return res.ModifiedCount == 1, nil
}

// orderedDaysKept bounds the per-subscription claim list (claims are made in
// day order, so the newest survive the slice).
const orderedDaysKept = 60

func (r *repository) setSubscriptionOrder(ctx context.Context, subID, orderID string) {
	_, _ = r.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: subID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "last_order_id", Value: orderID}}}})
}

// unclaimSubscriptionDay releases a day claim (guarded on the exact day) — used
// when the RECONCILER cancels a scheduled upcoming order because the plan
// changed (pause/vacation), so a resume before the midnight cutoff can schedule
// that day again. A shopper's DIRECT order cancel deliberately keeps the claim:
// that day stays skipped.
func (r *repository) unclaimSubscriptionDay(ctx context.Context, subID, day string) {
	_, _ = r.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: subID}},
		bson.D{{Key: "$pull", Value: bson.D{{Key: "ordered_days", Value: day}}}})
	_, _ = r.subscriptions.UpdateOne(ctx,
		bson.D{{Key: "subscription_id", Value: subID}, {Key: "last_order_date", Value: day}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "last_order_date", Value: ""},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
}

// findSubscriptionByID — UNSCOPED lookup for the worker (any consumer's sub).
// Returns (nil, nil) when absent. Never exposed on a route.
func (r *repository) findSubscriptionByID(ctx context.Context, subID string) (*subscription, error) {
	var s subscription
	err := r.subscriptions.FindOne(ctx, bson.D{{Key: "subscription_id", Value: subID}}).Decode(&s)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("subscription lookup failed")
	}
	return &s, nil
}

// listUnlockedSubOrders — SCHEDULED (pre-lock) subscription orders matching a
// scheduled_for filter (an exact day, or a range like {$lt: today}). Locked or
// cancelled orders never match.
func (r *repository) listUnlockedSubOrders(ctx context.Context, dayFilter any) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "subscription_id", Value: bson.D{{Key: "$gt", Value: ""}}},
		{Key: "scheduled_for", Value: dayFilter},
		{Key: "status", Value: "placed"},
		{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}, options.Find().SetLimit(5000))
	if err != nil {
		return nil, errInternal("scheduled orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("scheduled orders decode failed")
	}
	return out, nil
}

// listUnlockedSubOrdersFor is listUnlockedSubOrders for ONE subscription and
// every day up to and including throughDay.
func (r *repository) listUnlockedSubOrdersFor(ctx context.Context, subID, throughDay string) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "subscription_id", Value: subID},
		{Key: "scheduled_for", Value: bson.D{{Key: "$lte", Value: throughDay}}},
		{Key: "status", Value: "placed"},
		{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}, options.Find().SetLimit(100))
	if err != nil {
		return nil, errInternal("scheduled orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("scheduled orders decode failed")
	}
	return out, nil
}

// listMemberDayPreviews is listUnlockedSubOrders for ONE member's day: the
// previews the noon lock decides together (lockConsumerDay).
func (r *repository) listMemberDayPreviews(ctx context.Context, userID, day string) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "user_id", Value: userID},
		{Key: "subscription_id", Value: bson.D{{Key: "$gt", Value: ""}}},
		{Key: "scheduled_for", Value: day},
		{Key: "status", Value: "placed"},
		{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}, options.Find().SetLimit(100))
	if err != nil {
		return nil, errInternal("scheduled orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("scheduled orders decode failed")
	}
	return out, nil
}

// memberDayCommittedStatuses are the order statuses whose money the wallet
// still owes (or, delivered on the day itself, already paid after the lock
// moment - walletAsOf adds that debit back, so it is reserved here again).
var memberDayCommittedStatuses = bson.A{"placed", "confirmed", "preparing", "assigned", "out_for_delivery", "delivered"}

// listMemberDayCommitted is what a member's wallet already owes for day
// before any preview is funded: the subscription orders already locked for
// that day, and the wallet-paid one-off morning orders due on it (a one-off
// order dated for a day is fixed once that day's cut-off has passed).
func (r *repository) listMemberDayCommitted(ctx context.Context, userID, day string) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "user_id", Value: userID},
		{Key: "status", Value: bson.D{{Key: "$in", Value: memberDayCommittedStatuses}}},
		{Key: "payment_method", Value: bson.D{{Key: "$in", Value: bson.A{"wallet", "prepaid"}}}},
		{Key: "lane", Value: bson.D{{Key: "$ne", Value: "instant"}}},
		{Key: "$and", Value: bson.A{
			bson.D{{Key: "$or", Value: bson.A{
				bson.D{{Key: "delivery_date", Value: day}},
				bson.D{{Key: "scheduled_for", Value: day}},
			}}},
			bson.D{{Key: "$or", Value: bson.A{
				bson.D{{Key: "sub_locked_at", Value: bson.D{{Key: "$gt", Value: ""}}}},
				bson.D{{Key: "subscription_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}}},
			}}},
		}},
	}, options.Find().SetLimit(200))
	if err != nil {
		return nil, errInternal("committed orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("committed orders decode failed")
	}
	return out, nil
}

// ── Service ─────────────────────────────────────────────────────────────────

type subscriptionInput struct {
	ProductID    string          `json:"product_id"`
	Name         string          `json:"name"`
	Variant      string          `json:"variant"`
	Qty          int             `json:"qty"`
	UnitPrice    float64         `json:"unit_price"`
	Frequency    string          `json:"frequency"`
	DeliverySlot string          `json:"delivery_slot"`
	StartDate    string          `json:"start_date"` // YYYY-MM-DD (IST)
	Vacations    []vacationRange `json:"vacations"`
}

func (s *service) createSubscription(ctx context.Context, consumerID primitive.ObjectID, in subscriptionInput) (*subscription, error) {
	return s.createSubscriptionAt(ctx, consumerID, in, time.Now())
}

// createSubscriptionAt is createSubscription made at an explicit moment: the
// noon lock decides which morning a new plan first reaches, so tests drive it
// at a fixed IST time like the sweep's.
func (s *service) createSubscriptionAt(ctx context.Context, consumerID primitive.ObjectID, in subscriptionInput, at time.Time) (*subscription, error) {
	if in.ProductID == "" {
		return nil, errBadRequest("product_id is required")
	}
	if in.Qty <= 0 || in.Qty > maxQtyPerProduct {
		return nil, errBadRequest("qty must be between 1 and 10")
	}
	if !subscriptionFrequencies[in.Frequency] {
		return nil, errBadRequest("frequency must be one of: daily, alternate, weekly")
	}
	// The unit price is resolved SERVER-SIDE from the catalog (catalog_price.go)
	// — the client-sent unit_price is ignored, so a tampered app cannot start a
	// ₹0 (or inflated) daily plan. Unknown/hidden products are rejected.
	priceIx, err := s.loadPriceIndex(ctx)
	if err != nil {
		return nil, err
	}
	// FOUNDING FAMILY (founding.go): an active member's PYAAS milk plan is
	// priced at level 3; the worker re-reads the standing on every morning
	// order (subscriptionLinePrice), so this is the price shown at creation.
	memberActive := s.foundingActive(ctx, consumerID)
	unitPrice, ok := priceIx.priceForMember(in.ProductID, in.Variant, memberActive)
	if !ok {
		return nil, errBadRequest("unknown product: " + in.ProductID)
	}
	if gerr := s.foundingGate(ctx, consumerID, priceIx.isPyaasLine(in.ProductID), memberActive); gerr != nil {
		return nil, gerr
	}
	// ONE LIVE PLAN PER PRODUCT LINE (the app's DUPLICATE_SUBSCRIPTION guard,
	// lib/subscriptions.ts): the worker mints a morning order for EVERY active
	// plan, so a second plan on the same milk silently doubled the member's
	// daily order and charge. Change the existing plan instead. The line is
	// the one the plan will be STORED as (lineVariant): a label that is not
	// one of the SKU's own priced variants is stored as the SKU's size, so it
	// must be compared as that size too, or "1L" on a 500ml SKU made a second
	// 500ml plan.
	lineVar := priceIx.lineVariant(in.ProductID, in.Variant)
	if dup, derr := s.repo.findLiveSubscriptionForProduct(ctx, consumerID, in.ProductID, lineVar); derr != nil {
		return nil, derr
	} else if dup != nil {
		return nil, errConflict("DUPLICATE_SUBSCRIPTION", fmt.Sprintf(
			"You already have a %s plan for %s%s. Change its quantity or days in My subscriptions.",
			dup.Frequency, dup.Name, subscriptionVariantSuffix(dup.Variant)))
	}
	now := at.UTC()
	start := in.StartDate
	if start == "" {
		start = istToday(now)
	}
	if _, ok := parseDay(start); !ok {
		return nil, errBadRequest("start_date must be YYYY-MM-DD")
	}
	// HARD BACKSTOP (mirrors the FE's NEEDS_EXACT_LOCATION): a subscription may
	// never exist without a saved delivery point with coordinates — the morning
	// order must always route to a real door and its serving store.
	if _, aerr := s.subscriptionAddress(ctx, consumerID); aerr != nil {
		return nil, aerr
	}
	// The catalogue's name first, the client's only as a fallback (the
	// createOrder rule): the app posts its product id as the name while its
	// catalogue is still loading, and that id then printed on every morning
	// order, task and rider row. The size is the one billed (lineVariant).
	name := priceIx.nameFor(in.ProductID)
	if name == "" {
		name = in.Name
	}
	if name == "" {
		name = in.ProductID
	}
	sub := &subscription{
		MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(), ConsumerID: consumerID,
		ProductID: in.ProductID, Name: name, Variant: lineVar, Qty: in.Qty, UnitPrice: round2(unitPrice),
		Frequency: in.Frequency, DeliverySlot: in.DeliverySlot, Status: "active", StartDate: start,
		Vacations: in.Vacations, CreatedAt: now, UpdatedAt: now, ChangedAt: now,
	}
	if err := s.repo.insertSubscription(ctx, sub); err != nil {
		return nil, err
	}
	sub.NextDeliveryDate = s.nextDeliveryFor(ctx, sub, now)
	// CRM (inert unless CRM_ENABLED): subscription.activated (A-03) for
	// every plan the app creates, and subscription.created_unpaid (A-05)
	// when the wallet cannot cover its first delivery day. The campaign's
	// own plan (crmCreateSubscription) is announced by W-01 instead.
	if crmEnabled() {
		s.emitCRMEvent(ctx, "subscription.activated", consumerID, map[string]any{
			"subscription_id": sub.SubscriptionID, "product_id": sub.ProductID, "qty": sub.Qty,
			"frequency": sub.Frequency, "start_date": sub.StartDate,
			"start_label": crmSubscriptionStartLabel(sub, now),
			// A-03 is per_subscription: each plan is its own claim scope, so
			// two plans started on one day are both announced.
			"scope_key": sub.SubscriptionID,
		})
		cycle := round2(sub.UnitPrice*float64(sub.Qty)) + subscriptionDeliveryFee
		if wv, werr := s.wallet(ctx, consumerID); werr == nil && wv.Available < cycle {
			s.emitCRMEvent(ctx, "subscription.created_unpaid", consumerID, map[string]any{
				"subscription_id": sub.SubscriptionID, "first_cycle_amount": cycle, "scope_key": sub.SubscriptionID,
			})
		}
	}
	return sub, nil
}

func subscriptionVariantSuffix(variant string) string {
	if strings.TrimSpace(variant) == "" {
		return ""
	}
	return " " + strings.TrimSpace(variant)
}

// findLiveSubscriptionForProduct: the consumer's non-cancelled plan on the
// same product line (product id + normalised variant), or nil. A plan with
// no variant (the Welcome Litre plan is stored that way) is the whole
// product, so it matches any variant of it, and a request with no variant
// matches any plan on the product.
func (r *repository) findLiveSubscriptionForProduct(ctx context.Context, consumerID primitive.ObjectID, productID, variant string) (*subscription, error) {
	cur, err := r.subscriptions.Find(ctx, bson.D{
		{Key: "consumer_id", Value: consumerID},
		{Key: "product_id", Value: productID},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
	}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}).SetLimit(50))
	if err != nil {
		return nil, errInternal("subscriptions lookup failed")
	}
	var rows []subscription
	if err := cur.All(ctx, &rows); err != nil {
		return nil, errInternal("subscriptions decode failed")
	}
	// Spacing aside: lineVariant keeps "500 ML" as sent when it is the SKU's
	// own size, so "500 ML" and "500ml" are one line.
	key := func(v string) string { return strings.ReplaceAll(variantKey(v), " ", "") }
	want := key(variant)
	for i := range rows {
		if have := key(rows[i].Variant); have == want || have == "" || want == "" {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// subscriptionLinePrice is the unit price a morning order bills for this
// plan today: PYAAS milk lines follow the member's standing (level 3 while
// the Founding Family perks apply, level 1 otherwise) from the live catalog;
// every other line keeps the plan's own price.
func (s *service) subscriptionLinePrice(ctx context.Context, sub *subscription) float64 {
	if !isPyaasMilkSKU(sub.ProductID, "", "") {
		return round2(sub.UnitPrice)
	}
	ix, err := s.loadPriceIndex(ctx)
	if err != nil || !ix.isPyaasLine(sub.ProductID) {
		return round2(sub.UnitPrice)
	}
	if p, ok := ix.priceForMember(sub.ProductID, sub.Variant, s.foundingActive(ctx, sub.ConsumerID)); ok {
		return round2(p)
	}
	return round2(sub.UnitPrice)
}

// subscriptionAddress resolves the consumer's delivery point for subscription
// orders: the DEFAULT saved address (else the first) that carries lat/lng. One
// address serves every subscription — the app never asks again per subscription.
func (s *service) subscriptionAddress(ctx context.Context, consumerID primitive.ObjectID) (*address, error) {
	addrs, err := s.repo.listAddresses(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	var pick *address
	for i := range addrs {
		a := &addrs[i]
		if a.Lat == nil || a.Lng == nil {
			continue
		}
		if a.IsDefault {
			pick = a
			break
		}
		if pick == nil {
			pick = a
		}
	}
	if pick == nil {
		return nil, errUnprocessable("ADDRESS_REQUIRED", "save a delivery address (with its map location) before subscribing")
	}
	return pick, nil
}

func (s *service) listSubscriptionsFor(ctx context.Context, consumerID primitive.ObjectID) ([]subscription, error) {
	return s.repo.listSubscriptions(ctx, consumerID)
}

// setSubscriptionStatus — pause / resume / cancel with the state machine.
func (s *service) setSubscriptionStatus(ctx context.Context, consumerID primitive.ObjectID, subID, action string) (*subscription, error) {
	return s.setSubscriptionStatusAt(ctx, consumerID, subID, action, time.Now())
}

// setSubscriptionStatusAt is setSubscriptionStatus with the change made at now.
func (s *service) setSubscriptionStatusAt(ctx context.Context, consumerID primitive.ObjectID, subID, action string, now time.Time) (*subscription, error) {
	target, ok := subscriptionActionTarget(action)
	if !ok {
		return nil, errBadRequest("unknown subscription action")
	}
	sub, err := s.repo.findSubscription(ctx, subID, consumerID)
	if err != nil {
		return nil, err
	}
	if sub.Status == target {
		return sub, nil // idempotent
	}
	if !subscriptionTransitions[sub.Status][target] {
		return nil, errConflict("SUBSCRIPTION_STATE", fmt.Sprintf("cannot %s a %s subscription", action, sub.Status))
	}
	// A day already past its cut-off keeps the plan as it stood then.
	s.lockPreviewsBeforeChange(ctx, sub, now)
	updated, err := s.repo.updateSubscription(ctx, subID, consumerID,
		bson.D{{Key: "status", Value: target}, {Key: "changed_at", Value: now.UTC()}},
		bson.D{{Key: "status", Value: sub.Status}})
	if err != nil {
		return nil, err
	}
	// CRM subscription.modified (C-03 reads change in paused/quantity_reduced).
	s.emitCRMEvent(ctx, "subscription.modified", consumerID, map[string]any{
		"subscription_id": subID, "change": crmSubscriptionChange(action), "scope_key": subID + ":" + action,
	})
	return updated, nil
}

// crmSubscriptionChange names a status action for the CRM (C-03 conditions).
func crmSubscriptionChange(action string) string {
	switch action {
	case "pause":
		return "paused"
	case "resume":
		return "resumed"
	case "cancel":
		return "cancelled"
	}
	return action
}

// subscriptionPatch is PATCH /subscriptions/{id}'s body: every field optional.
type subscriptionPatch struct {
	Qty          *int             `json:"qty"`
	Frequency    *string          `json:"frequency"`
	DeliverySlot *string          `json:"delivery_slot"`
	StartDate    *string          `json:"start_date"`
	Vacations    *[]vacationRange `json:"vacations"`
}

// patchSubscription edits the live plan (qty / frequency / slot / re-anchored
// start date / vacation ranges) — the FE's updateSubscription + reactivate +
// vacation mirror rides through here.
func (s *service) patchSubscription(ctx context.Context, consumerID primitive.ObjectID, subID string, in subscriptionPatch) (*subscription, error) {
	return s.patchSubscriptionAt(ctx, consumerID, subID, in, time.Now())
}

// patchSubscriptionAt is patchSubscription with the change made at now.
func (s *service) patchSubscriptionAt(ctx context.Context, consumerID primitive.ObjectID, subID string, in subscriptionPatch, now time.Time) (*subscription, error) {
	set := bson.D{}
	if in.Qty != nil {
		if *in.Qty <= 0 || *in.Qty > maxQtyPerProduct {
			return nil, errBadRequest("qty must be between 1 and 10")
		}
		set = append(set, bson.E{Key: "qty", Value: *in.Qty})
	}
	if in.Frequency != nil {
		if !subscriptionFrequencies[*in.Frequency] {
			return nil, errBadRequest("frequency must be one of: daily, alternate, weekly")
		}
		set = append(set, bson.E{Key: "frequency", Value: *in.Frequency})
	}
	if in.DeliverySlot != nil {
		set = append(set, bson.E{Key: "delivery_slot", Value: *in.DeliverySlot})
	}
	if in.StartDate != nil {
		if _, ok := parseDay(*in.StartDate); !ok {
			return nil, errBadRequest("start_date must be YYYY-MM-DD")
		}
		set = append(set, bson.E{Key: "start_date", Value: *in.StartDate})
	}
	if in.Vacations != nil {
		for _, v := range *in.Vacations {
			if _, ok1 := parseDay(v.Start); !ok1 {
				return nil, errBadRequest("vacation start must be YYYY-MM-DD")
			}
			if _, ok2 := parseDay(v.End); !ok2 || v.End < v.Start {
				return nil, errBadRequest("vacation end must be YYYY-MM-DD on/after its start")
			}
		}
		set = append(set, bson.E{Key: "vacations", Value: *in.Vacations})
	}
	cur, err := s.repo.findSubscription(ctx, subID, consumerID)
	if err != nil || len(set) == 0 {
		return cur, err
	}
	// A day already past its cut-off keeps the plan as it stood then.
	s.lockPreviewsBeforeChange(ctx, cur, now)
	set = append(set, bson.E{Key: "changed_at", Value: now.UTC()})
	updated, err := s.repo.updateSubscription(ctx, subID, consumerID, set, bson.D{})
	if err != nil {
		return nil, err
	}
	// CRM: the quantity before the edit (cur), so C-03 can tell a reduction
	// from an increase.
	if in.Qty != nil && crmEnabled() && cur.Qty != updated.Qty {
		change := "quantity_increased"
		if updated.Qty < cur.Qty {
			change = "quantity_reduced"
		}
		s.emitCRMEvent(ctx, "subscription.modified", consumerID, map[string]any{
			"subscription_id": subID, "change": change, "qty": updated.Qty, "previous_qty": cur.Qty,
			"scope_key": subID + ":qty",
		})
	}
	return updated, nil
}

// ── The morning-order lifecycle (server twin of lib/subscriptionSweep.ts) ───
//
// THE CUT-OFF IS 12 NOON THE DAY BEFORE (One Voice 1.2, the app's copy, the
// owner's rule of 21 Sep): for everything - new plans, changes, pauses and
// skips. In sweep terms:
//
//   any tick     PREVIEW - the first day still open to changes (tomorrow
//                before noon, the day after tomorrow from noon) materialises
//                as a VISIBLE upcoming order (status placed, delivery_date,
//                NO delivery task, NO money). The shopper sees it in Orders
//                and can still change everything: subscription edits
//                reconcile it, pause/vacation cancel it (claim released), a
//                direct order cancel skips the day.
//   12:00 IST    LOCK - tomorrow's preview is re-checked against the LIVE
//                subscription, its line refreshed, funded from the wallet as
//                it stood at 12:00:00 (the member's plans oldest first), then
//                the store delivery task is created. From here edits no
//                longer touch it: a change made after noon applies to the day
//                after tomorrow. A preview the noon wallet does not cover is
//                closed at once as a skipped day (wallet_short, the claim
//                kept, subscription.day_skipped for D-07): a top-up after
//                noon reaches the day after tomorrow, never a late task
//                (lockConsumerDay).
//   05:00-07:30  The morning route delivers; money settles on delivery.
//   later        MISSED - a locked order whose day passed with no delivery is
//                closed (closeMissedSubscriptionOrders) so it never reads as
//                live again.

// lockHourIST is the hour (IST) on the day BEFORE delivery at which that
// day's previews lock.
const lockHourIST = 12

// lockedThroughDay is the latest IST day whose previews are locked at now:
// tomorrow once today's noon has passed, else today.
func lockedThroughDay(now time.Time) string {
	ist := now.In(istZone)
	today := ist.Format("2006-01-02")
	if ist.Hour() >= lockHourIST {
		return addDaysIST(today, 1)
	}
	return today
}

// routeStartHourIST is the hour (IST) a delivery day's morning route leaves
// the store (the 05:00-07:30 window). From it the day is past locking.
const routeStartHourIST = 5

// routeStartFor is the instant a delivery day's morning route leaves.
func routeStartFor(dayISO string) time.Time {
	d, ok := parseDay(dayISO)
	if !ok {
		return time.Time{}
	}
	return d.Add(routeStartHourIST * time.Hour)
}

// firstEditableDay is the first delivery day still open to changes at now.
func firstEditableDay(now time.Time) string { return addDaysIST(lockedThroughDay(now), 1) }

// lockMomentFor is the instant a delivery day's previews lock: noon IST on
// the day before it.
func lockMomentFor(dayISO string) time.Time {
	d, ok := parseDay(dayISO)
	if !ok {
		return time.Time{}
	}
	return d.AddDate(0, 0, -1).Add(lockHourIST * time.Hour)
}

// subscriptionNextDelivery is the first day the plan really delivers as seen
// at now, derived from the orders (live returns a day's live order, or nil):
//
//   - a day already past its cut-off (today, and tomorrow from noon) delivers
//     only through the live order it holds, whatever the plan's status now:
//     a pause or cancel after noon applies from the day after tomorrow, so a
//     locked tomorrow is still delivered and billed. A preview no tick has
//     locked yet goes the LOCK step's way (a plan changed before the cut-off
//     decides it; one changed after keeps it).
//   - an open day counts when the plan is due on it and the member has not
//     skipped it (a claimed day whose order was cancelled).
//
// "" when nothing is due in the next three weeks.
func subscriptionNextDelivery(sub *subscription, now time.Time, live func(day string) *order) string {
	today := istToday(now)
	lockedThrough := lockedThroughDay(now)
	from := 0
	if now.In(istZone).Hour() >= 8 { // the 05:00-07:30 route has run: today is behind us
		from = 1
	}
	for i := from; i < 21; i++ {
		day := addDaysIST(today, i)
		if day <= lockedThrough {
			o := live(day)
			if o != nil && (o.SubLockedAt != "" || !sub.subChangedBefore(lockMomentFor(day)) || subscriptionDueOn(sub, day)) {
				return day
			}
			continue
		}
		if !subscriptionDueOn(sub, day) {
			continue
		}
		if sub.claimed(day) && live(day) == nil {
			continue // the member skipped this day
		}
		return day
	}
	return ""
}

// nextDeliveryFor is subscriptionNextDelivery over this plan's live orders.
func (s *service) nextDeliveryFor(ctx context.Context, sub *subscription, now time.Time) string {
	return subscriptionNextDelivery(sub, now, func(day string) *order {
		o, _ := s.repo.findLiveSubscriptionOrder(ctx, sub.SubscriptionID, day)
		return o
	})
}

// addDaysIST offsets a YYYY-MM-DD day on the IST calendar.
func addDaysIST(dayISO string, n int) string {
	t, ok := parseDay(dayISO)
	if !ok {
		return dayISO
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// insertSubscriptionOrder materialises one subscription day as a consumer
// order. locked=false → a SCHEDULED preview (no delivery task yet, cancellable,
// reconciled until the midnight lock); locked=true → immediately live (delivery
// task created, store queue). Money never moves here — settle-on-delivery.
func (s *service) insertSubscriptionOrder(ctx context.Context, sub *subscription, addr *address, day string, locked bool, at time.Time) (*order, error) {
	s.ensureSubscriptionVariant(ctx, sub) // a plan stored without its pack size (F17)
	acct, _ := s.repo.findAccountByID(ctx, sub.ConsumerID)
	name, phone := "", ""
	if acct != nil {
		phone = acct.Phone
		if acct.FullName != nil {
			name = *acct.FullName
		}
	}
	unitPrice := s.subscriptionLinePrice(ctx, sub)
	subtotal := round2(unitPrice * float64(sub.Qty))
	// SUBSCRIPTION deliveries carry NO delivery fee — the consumer app quotes and
	// charges the bare subtotal on the Subscribe button (lib/api.ts), so the
	// server must debit the same. A ₹15 fee here over-charged every subscription
	// delivery vs the price the member agreed to.
	fee := subscriptionDeliveryFee
	now := at.UTC()
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: sub.ConsumerID.Hex(), Status: "placed",
		Subtotal: subtotal, DeliveryFee: fee, Total: round2(subtotal + fee), PaymentMethod: "wallet",
		AddressLabel: addr.Label, AddressText: joinAddress(addr), AddressID: addr.ID.Hex(), RiderID: nil,
		PlacedAt: now, Priority: "normal",
		// The 05:00–07:30 morning route — same window the FE sweep prints.
		DeliveryWindow: "05:00 - 07:30 AM", Lane: "morning",
		Items: []orderItem{{
			ID: newItemID(), ProductID: sub.ProductID, Name: sub.Name,
			Variant: sub.Variant, Price: unitPrice, Qty: sub.Qty,
		}},
		ConsumerName: name, Phone: phone, Geo: &geoPoint{Lat: *addr.Lat, Lng: *addr.Lng},
		CreatedAt: now, UpdatedAt: now,
		SubscriptionID: sub.SubscriptionID, ScheduledFor: day,
		// The wire date the app displays. ScheduledFor used to alias
		// json:"delivery_date" until a tag collision silenced both — setting
		// DeliveryDate restores the date on every subscription order response.
		DeliveryDate: day,
	}
	// Flag a free-day trial delivery for DISPLAY + ANALYTICS (badge "FREE" in the
	// app, count free milk in reporting). Peeking the trial ledger never advances
	// it and never gates the wallet/hold — the order is still placed only when
	// funded (unchanged); the actual 0 charge is applied at delivery.
	if isTrialProduct(sub.ProductID) {
		if t, terr := s.repo.getOrCreateTrial(ctx, sub.ConsumerID); terr == nil && trialPhaseFor(t.DeliveredPaid, t.DeliveredFree) == trialPhaseFree {
			o.TrialFree = true
		}
	}
	if locked {
		o.SubLockedAt = now.Format(time.RFC3339)
	}
	placed, err := s.repo.insertSubscriptionOrderDoc(ctx, o)
	if err != nil {
		return nil, err
	}
	if !placed {
		// The day index refused a second live order for this day: another
		// worker's insert won after both passed the claim. Its order IS the
		// day's order - nothing else (task, last_order_id) is repeated here.
		existing, ferr := s.repo.findLiveSubscriptionOrder(ctx, sub.SubscriptionID, day)
		if ferr != nil {
			return nil, ferr
		}
		s.log.InfoContext(ctx, "subscription day already placed - duplicate insert dropped",
			"subscription", sub.SubscriptionID, "day", day)
		return existing, nil
	}
	if locked {
		s.createDeliveryForOrder(ctx, o)
	}
	s.repo.setSubscriptionOrder(ctx, sub.SubscriptionID, o.OrderID)
	return o, nil
}

// cancelScheduledSubOrder cancels a still-unlocked preview (worker path only —
// guarded so a locked or already-cancelled order is never touched).
func (s *service) cancelScheduledSubOrder(ctx context.Context, o *order) {
	_, _ = s.repo.updateOrder(ctx, o.OrderID, o.UserID,
		bson.D{{Key: "status", Value: "cancelled"}},
		bson.D{
			{Key: "status", Value: "placed"},
			{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		})
}

// refreshSubOrder re-derives a preview's line from the LIVE subscription, so a
// qty / price / variant edit made before the midnight lock shows on the
// upcoming order (and bills correctly at lock).
func (s *service) refreshSubOrder(ctx context.Context, o *order, sub *subscription) *order {
	s.ensureSubscriptionVariant(ctx, sub) // a plan stored without its pack size (F17)
	unitPrice := s.subscriptionLinePrice(ctx, sub)
	if len(o.Items) == 1 &&
		o.Items[0].Qty == sub.Qty &&
		o.Items[0].Price == unitPrice &&
		o.Items[0].Variant == sub.Variant {
		return o
	}
	subtotal := round2(unitPrice * float64(sub.Qty))
	fee := subscriptionDeliveryFee // subscriptions never carry the ₹15 fee (see insertSubscriptionOrder)
	updated, err := s.repo.updateOrder(ctx, o.OrderID, o.UserID, bson.D{
		{Key: "order_items", Value: []orderItem{{
			ID: newItemID(), ProductID: sub.ProductID, Name: sub.Name,
			Variant: sub.Variant, Price: unitPrice, Qty: sub.Qty,
		}}},
		{Key: "subtotal", Value: subtotal},
		{Key: "delivery_fee", Value: fee},
		{Key: "total", Value: round2(subtotal + fee)},
	}, bson.D{{Key: "status", Value: "placed"}})
	if err != nil {
		return o
	}
	return updated
}

// sweepSubscriptionOrders runs the full lifecycle for one tick: expire stale
// previews, close missed orders, LOCK every day past its noon, catch up and
// PREVIEW per subscription, and reconcile the still-editable previews against
// subscription edits. Exactly-once per (subscription, day) via the claim;
// returns how many orders went LIVE (locked) this tick.
func (s *service) sweepSubscriptionOrders(ctx context.Context, now time.Time) int {
	today := istToday(now)
	lockedThrough := lockedThroughDay(now)
	editable := firstEditableDay(now)
	placed := 0

	// 1) EXPIRE — previews whose day passed without ever locking (server down
	//    over the cut-off, wallet never funded): cancel; stale milk never ships.
	if stale, err := s.repo.listUnlockedSubOrders(ctx, bson.D{{Key: "$lt", Value: today}}); err == nil {
		for i := range stale {
			s.cancelScheduledSubOrder(ctx, &stale[i])
		}
	}
	// 1b) MISSED — locked orders whose delivery day passed with no delivery.
	s.closeMissedSubscriptionOrders(ctx, now)

	// 2) LOCK — every preview whose day's noon has passed (today's, and from
	//    noon tomorrow's), decided once per member and day (lockConsumerDay):
	//    re-checked against the live subscription, funded from the wallet as
	//    it stood at 12:00:00 oldest plan first, then locked with its store
	//    task, or closed as a skipped day when the wallet did not cover it.
	if due, err := s.repo.listUnlockedSubOrders(ctx, bson.D{{Key: "$lte", Value: lockedThrough}}); err == nil {
		for _, md := range subMemberDays(due) {
			placed += s.lockConsumerDay(ctx, md.userID, md.day, now)
		}
	}

	subs, err := s.repo.listActiveSubscriptions(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "subscription sweep: scan failed")
		return placed
	}
	for i := range subs {
		placed += s.sweepOneSubscription(ctx, &subs[i], now)
	}

	// 5) RECONCILE — the still-editable previews (every day from the first
	//    editable one) against their live subscriptions: pause/cancel/vacation
	//    → cancel the upcoming order AND release the day claim (a resume
	//    before the cut-off re-schedules it); qty/price edits → refresh the
	//    shown line. A shopper's direct cancel is untouched here (its claim
	//    stays, so the day stays skipped). Locked days are never touched: a
	//    change made after noon applies to the day after tomorrow.
	if upcoming, err := s.repo.listUnlockedSubOrders(ctx, bson.D{{Key: "$gte", Value: editable}}); err == nil {
		for i := range upcoming {
			o := &upcoming[i]
			sub, _ := s.repo.findSubscriptionByID(ctx, o.SubscriptionID)
			if sub == nil || !subscriptionDueOn(sub, o.ScheduledFor) {
				s.cancelScheduledSubOrder(ctx, o)
				s.repo.unclaimSubscriptionDay(ctx, o.SubscriptionID, o.ScheduledFor)
				continue
			}
			s.refreshSubOrder(ctx, o, sub)
		}
	}

	if placed > 0 {
		s.log.InfoContext(ctx, "subscription sweep placed morning orders", "day", today, "orders", placed)
	}
	return placed
}

// subMemberDay is one member's delivery day, the unit the noon lock decides.
type subMemberDay struct{ userID, day string }

// subMemberDays is the distinct (member, day) pairs of a set of previews, in
// a stable order.
func subMemberDays(previews []order) []subMemberDay {
	seen := map[subMemberDay]bool{}
	var out []subMemberDay
	for i := range previews {
		md := subMemberDay{userID: previews[i].UserID, day: previews[i].ScheduledFor}
		if md.userID == "" || md.day == "" || seen[md] {
			continue
		}
		seen[md] = true
		out = append(out, md)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].day != out[j].day {
			return out[i].day < out[j].day
		}
		return out[i].userID < out[j].userID
	})
	return out
}

// lockConsumerDay is the noon LOCK for one member's delivery day, once its
// cut-off (lockMomentFor) has passed. Every preview the member still holds
// for that day is decided exactly once, the same way whichever tick,
// replica or member change gets there first:
//
//  1. Re-checked against its plan: a plan changed before the cut-off
//     decides the day as it is now (a pause, cancel or vacation cancels the
//     preview; an edit refreshes its line); a plan changed after the cut-off
//     keeps the day as previewed (the change belongs to the next editable
//     day).
//  2. Funded, oldest plan first, from the wallet as it stood at 12:00:00
//     (walletAsOf: identical at 12:00:05 and 12:14:59), less what the member
//     already owes that day (orders already locked, one-off morning orders),
//     each at what the door will take (lockCharge: a 2+2 free trial day is
//     Rs 0, so it never needs funds).
//  3. Covered: locked, with its store task. Not covered: closed at once as a
//     skipped day (skipSubPreview), never retried on later ticks, so a
//     top-up after noon cannot mint a late task; the plan stays active and
//     the next editable day is previewed as usual.
//
// A day whose morning route has left is past locking: its previews expire
// (cancelled, the day stays skipped, no task, no message) instead of
// minting a task for a round already gone. A read failure decides nothing;
// the next tick decides the same way. Returns how many previews this call
// locked.
func (s *service) lockConsumerDay(ctx context.Context, userID, day string, now time.Time) int {
	lockAt := lockMomentFor(day)
	if lockAt.IsZero() || now.Before(lockAt) {
		return 0 // before its cut-off the day is still an editable preview
	}
	previews, err := s.repo.listMemberDayPreviews(ctx, userID, day)
	if err != nil || len(previews) == 0 {
		return 0
	}
	if rs := routeStartFor(day); !rs.IsZero() && !now.Before(rs) {
		for i := range previews {
			s.cancelScheduledSubOrder(ctx, &previews[i])
		}
		return 0
	}
	type candidate struct {
		o   *order
		sub *subscription
	}
	var cands []candidate
	for i := range previews {
		o := &previews[i]
		sub, serr := s.repo.findSubscriptionByID(ctx, o.SubscriptionID)
		if serr != nil {
			return 0
		}
		// The tick runs every 15 minutes, so it can reach a day after its lock
		// moment has passed. A member change stamped after that moment belongs
		// to the next editable day (the noon rule), so the preview is decided
		// as it stands: no cancel, no refresh.
		asPreviewed := sub != nil && !sub.subChangedBefore(lockAt)
		if !asPreviewed {
			if sub == nil || !subscriptionDueOn(sub, day) {
				s.cancelScheduledSubOrder(ctx, o)
				continue
			}
			o = s.refreshSubOrder(ctx, o, sub)
		}
		cands = append(cands, candidate{o: o, sub: sub})
	}
	if len(cands) == 0 {
		return 0
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].sub, cands[j].sub
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.SubscriptionID < b.SubscriptionID
	})
	cid, cerr := primitive.ObjectIDFromHex(userID)
	if cerr != nil {
		return 0
	}
	avail, werr := s.walletAsOf(ctx, cid, lockAt, now)
	if werr != nil {
		s.log.WarnContext(ctx, "noon lock: wallet read failed - the day is decided on a later tick",
			"consumer", userID, "day", day, "err", werr)
		return 0
	}
	committed, cmerr := s.repo.listMemberDayCommitted(ctx, userID, day)
	if cmerr != nil {
		return 0
	}
	for i := range committed {
		avail -= lockCharge(&committed[i], committed[i].TrialFree)
	}
	avail = round2(avail)
	freeDay := false // only a trial line reads the trial (and only then creates its row)
	for _, c := range cands {
		if orderIsTrialLine(c.o) {
			freeDay = s.trialFreeDayNow(ctx, cid)
			break
		}
	}
	locked := 0
	for _, c := range cands {
		free := freeDay && orderIsTrialLine(c.o)
		cost := round2(lockCharge(c.o, free))
		if avail >= cost {
			if s.lockSubOrder(ctx, c.o, free, now) {
				locked++
			}
			// Reserved whether or not this call won the lock: a replica that
			// won it made the same decision on the same wallet.
			avail = round2(avail - cost)
			continue
		}
		s.skipSubPreview(ctx, c.o, round2(cost-avail), now)
	}
	return locked
}

// lockCharge is what the lock asks the wallet for one morning order: what
// the door will really take (decision 9 of 24 Sep, the expected charge, not
// the sticker). A 2+2 free trial day takes Rs 0 at delivery
// (trialChargeFor), so an empty wallet never skips it; anything else takes
// its total.
func lockCharge(o *order, trialFree bool) float64 {
	if trialFree {
		return 0
	}
	return o.Total
}

// orderIsTrialLine reports a subscription morning order on the trial SKU:
// the only order the 2+2 trial prices at delivery (a one-off order of the
// same milk never is).
func orderIsTrialLine(o *order) bool {
	if o.SubscriptionID == "" {
		return false
	}
	for _, it := range o.Items {
		if isTrialProduct(it.ProductID) {
			return true
		}
	}
	return false
}

// trialFreeDayNow reports whether the member's next delivered trial day is a
// free one, read at the lock (the day before's delivery may have opened the
// window since the preview was made). Peeking never advances the trial.
func (s *service) trialFreeDayNow(ctx context.Context, consumerID primitive.ObjectID) bool {
	t, err := s.repo.getOrCreateTrial(ctx, consumerID)
	return err == nil && trialPhaseFor(t.DeliveredPaid, t.DeliveredFree) == trialPhaseFree
}

// lockSubOrder locks one funded preview: the guarded stamp (placed, never
// locked - a replica that got there first owns it), then the store task.
// trialFree is the display flag as the lock read the trial (a preview made
// before the free window opened reads free once it is).
func (s *service) lockSubOrder(ctx context.Context, o *order, trialFree bool, now time.Time) bool {
	set := bson.D{{Key: "sub_locked_at", Value: now.UTC().Format(time.RFC3339)}}
	if orderIsTrialLine(o) {
		set = append(set, bson.E{Key: "trial_free", Value: trialFree})
	}
	upd, uerr := s.repo.updateOrder(ctx, o.OrderID, o.UserID,
		set,
		bson.D{
			{Key: "status", Value: "placed"},
			{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		})
	if uerr != nil {
		return false // raced with another replica — it owns the lock
	}
	s.createDeliveryForOrder(ctx, upd)
	return true
}

// skipSubPreview closes a preview the member's noon wallet could not cover:
// cancelled by "wallet_short" (guarded on a still-unlocked, placed preview,
// so it can never race a lock into two outcomes), the day claim KEPT so the
// day is never previewed again, no task, no money. subscription.day_skipped
// tells the CRM (D-07). Returns whether this call closed it.
func (s *service) skipSubPreview(ctx context.Context, o *order, shortfall float64, now time.Time) bool {
	if _, err := s.repo.updateOrder(ctx, o.OrderID, o.UserID,
		bson.D{
			{Key: "status", Value: "cancelled"},
			{Key: "cancelled_by", Value: orderCancelledByWalletShort},
			{Key: "skipped_at", Value: now.UTC()},
		},
		bson.D{
			{Key: "status", Value: "placed"},
			{Key: "sub_locked_at", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		}); err != nil {
		return false // locked, cancelled or skipped by another path first
	}
	s.log.InfoContext(ctx, "noon lock: day skipped - the wallet at 12:00 did not cover it",
		"subscription", o.SubscriptionID, "day", o.ScheduledFor, "shortfall", shortfall)
	if cid, err := primitive.ObjectIDFromHex(o.UserID); err == nil {
		s.emitCRMEvent(ctx, "subscription.day_skipped", cid, map[string]any{
			"subscription_id": o.SubscriptionID, "day": o.ScheduledFor,
			"reason": orderCancelledByWalletShort, "shortfall": shortfall,
		})
	}
	return true
}

// lockPreviewsBeforeChange runs the LOCK for every day of this plan whose
// cut-off has passed but that no tick has decided yet, against the plans AS
// THEY STOOD, before a member change made at now is written. changed_at
// keeps only the LAST change and the tick runs every 15 minutes, so without
// this a change made before noon followed by another before the first tick
// after noon was lost: the tick saw a plan changed after the lock moment and
// locked tomorrow as previewed (a pause at 11:50 and a resume at 12:05 still
// delivered and billed tomorrow; a qty edit at 11:50 never reached it). The
// whole member-day is decided, so the member's other plans are funded in the
// same order the tick would have used.
func (s *service) lockPreviewsBeforeChange(ctx context.Context, sub *subscription, now time.Time) {
	due, err := s.repo.listUnlockedSubOrdersFor(ctx, sub.SubscriptionID, lockedThroughDay(now))
	if err != nil {
		return
	}
	for _, md := range subMemberDays(due) {
		s.lockConsumerDay(ctx, md.userID, md.day, now)
	}
}

// sweepOneSubscription runs the per-subscription half of the sweep (steps 3
// and 4) for ONE subscription. The worker calls it for every active
// subscription; createSubscription's handler calls it right after the insert,
// so a new subscriber's preview exists at once instead of on the next
// 15-minute tick. Same claims, same noon lock — so the two callers can
// never double-order a day. Returns how many orders went LIVE.
func (s *service) sweepOneSubscription(ctx context.Context, sub *subscription, now time.Time) int {
	today := istToday(now)
	lockedThrough := lockedThroughDay(now)
	editable := firstEditableDay(now)
	placed := 0
	// 3) CATCH-UP — a day already past its noon (today, and from noon
	//    tomorrow) that is due but was never previewed still delivers, IF the
	//    plan predates that day's cut-off: a server that was down when the
	//    preview and the lock should have run still puts the milk on the
	//    route. A plan created, resumed or edited after the cut-off does not
	//    reach that day - it starts on the first editable one (the noon rule
	//    for new orders and changes). Claim, preview, then the same LOCK as
	//    every other day (lockConsumerDay): the wallet as it stood at that
	//    day's 12:00, the member's plans funded oldest first, a short day
	//    skipped once. A day whose route has already left is not caught up.
	for day := today; day <= lockedThrough; day = addDaysIST(day, 1) {
		if sub.claimed(day) || !subscriptionDueOn(sub, day) || !sub.subChangedBefore(lockMomentFor(day)) {
			continue
		}
		if rs := routeStartFor(day); !rs.IsZero() && !now.Before(rs) {
			continue
		}
		addr, aerr := s.subscriptionAddress(ctx, sub.ConsumerID)
		if aerr != nil {
			continue
		}
		if won, _ := s.repo.claimSubscriptionDay(ctx, sub.SubscriptionID, day); won {
			if _, oerr := s.insertSubscriptionOrder(ctx, sub, addr, day, false, now); oerr != nil {
				// The claim is what stops a second order for this day, so a
				// claim with no order behind it means the day is now skipped
				// FOREVER: the next tick sees it claimed and moves on, and
				// nobody is told. Release it so the next tick retries, and log
				// loudly enough that a repeated failure is visible rather than
				// a customer simply not getting milk.
				s.repo.unclaimSubscriptionDay(ctx, sub.SubscriptionID, day)
				s.log.ErrorContext(ctx, "subscription sweep: catch-up order failed — day released for retry",
					"subscription", sub.SubscriptionID, "day", day, "err", oerr)
				continue
			}
			placed += s.lockConsumerDay(ctx, sub.ConsumerID.Hex(), day, now)
		}
	}
	// 4) PREVIEW — materialise the first still-editable day as a visible,
	//    modifiable upcoming order (no delivery task, no money, no wallet
	//    gate — the member can top up until that day's noon cut-off).
	if !sub.claimed(editable) && subscriptionDueOn(sub, editable) {
		if addr, aerr := s.subscriptionAddress(ctx, sub.ConsumerID); aerr == nil {
			if won, _ := s.repo.claimSubscriptionDay(ctx, sub.SubscriptionID, editable); won {
				if _, oerr := s.insertSubscriptionOrder(ctx, sub, addr, editable, false, now); oerr != nil {
					s.repo.unclaimSubscriptionDay(ctx, sub.SubscriptionID, editable) // retry next tick
					s.log.ErrorContext(ctx, "subscription sweep: preview failed — day released for retry",
						"subscription", sub.SubscriptionID, "day", editable, "err", oerr)
				}
			}
		}
	}
	return placed
}

// orderCancelledByMissed marks an order the sweep closed because its
// delivery day passed with no delivery. Distinct from a task's cancel
// (orderCancelledByDelivery) so a rider's undo can never resurrect it.
const orderCancelledByMissed = "missed"

// orderCancelledByWalletShort marks a preview the noon lock closed because
// the member's wallet, as it stood at 12:00:00 the day before, could not
// cover it: a skipped day (the claim is kept, so it is never re-previewed).
const orderCancelledByWalletShort = "wallet_short"

// The words a missed close writes where people read them: the task's
// failure reason (the rider and store consoles print it after "Reported:")
// and the order.failed reason D-09 renders as [REASON] ("could not reach you
// (...)"). cancelled_by keeps the machine value above for the code.
const (
	missedTaskFailureReason = "Missed: the delivery day passed without a delivery"
	missedCustomerReason    = "the delivery day passed without a delivery"
)

// subscriptionOpenStatuses are the order statuses between placement and a
// delivery outcome - what a missed order can still be sitting at.
var subscriptionOpenStatuses = bson.A{"placed", "confirmed", "preparing", "assigned", "out_for_delivery"}

// listStaleLockedSubOrders: locked subscription orders whose delivery day is
// before cutoff and that never reached a delivery outcome.
func (r *repository) listStaleLockedSubOrders(ctx context.Context, cutoff string) ([]order, error) {
	cur, err := r.orders.Find(ctx, bson.D{
		{Key: "subscription_id", Value: bson.D{{Key: "$gt", Value: ""}}},
		{Key: "sub_locked_at", Value: bson.D{{Key: "$gt", Value: ""}}},
		{Key: "scheduled_for", Value: bson.D{{Key: "$gt", Value: ""}, {Key: "$lt", Value: cutoff}}},
		{Key: "status", Value: bson.D{{Key: "$in", Value: subscriptionOpenStatuses}}},
	}, options.Find().SetLimit(5000))
	if err != nil {
		return nil, errInternal("stale orders scan failed")
	}
	out := []order{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("stale orders decode failed")
	}
	return out, nil
}

// closeMissedSubscriptionOrders closes locked morning orders whose delivery
// day passed with no delivery (no task, or a task never completed). The
// EXPIRE step only ever cancelled unlocked previews, so such an order stayed
// "placed" forever: the app's active filter hid it while the server, the
// day index and the store queue all treated it as live. It becomes
// cancelled (the one terminal status the app draws besides delivered), its
// task fails with missedTaskFailureReason, order.failed is emitted with
// missedCustomerReason so the member is told in words, and no money moves. Yesterday's orders
// are closed from noon (the morning is left for a late delivered mark);
// older ones on any tick. An order a rider is still out with gets until the
// day after: failing the task under the rider refused their late delivered
// mark. Returns how many orders were closed.
func (s *service) closeMissedSubscriptionOrders(ctx context.Context, now time.Time) int {
	ist := now.In(istZone)
	cutoff := istToday(now)
	if ist.Hour() < lockHourIST {
		cutoff = addDaysIST(cutoff, -1)
	}
	inFlightCutoff := addDaysIST(istToday(now), -1)
	stale, err := s.repo.listStaleLockedSubOrders(ctx, cutoff)
	if err != nil {
		return 0
	}
	closed := 0
	for i := range stale {
		o := &stale[i]
		task, _ := s.repo.findDeliveryByOrder(ctx, o.OrderID)
		if task != nil && task.Status == "DELIVERED" {
			continue // the delivery happened; the order sync owns the rest
		}
		if task != nil && task.Status == "OUT_FOR_DELIVERY" && o.ScheduledFor >= inFlightCutoff {
			continue // the rider still has it: left for a late delivered mark
		}
		res, err := s.repo.orders.UpdateOne(ctx,
			bson.D{{Key: "order_id", Value: o.OrderID}, {Key: "status", Value: bson.D{{Key: "$in", Value: subscriptionOpenStatuses}}}},
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "status", Value: "cancelled"}, {Key: "cancelled_by", Value: orderCancelledByMissed},
				{Key: "missed_at", Value: now.UTC()}, {Key: "updated_at", Value: now.UTC()},
			}}})
		if err != nil || res.ModifiedCount == 0 {
			continue
		}
		closed++
		if task != nil && task.Status != "FAILED" {
			_, _ = s.repo.updateDelivery(ctx, task.ID,
				bson.D{{Key: "status", Value: "FAILED"}, {Key: "failure_reason", Value: missedTaskFailureReason}},
				bson.D{{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED", "FAILED"}}}}})
		}
		if crmEnabled() {
			if cid, cerr := primitive.ObjectIDFromHex(o.UserID); cerr == nil {
				s.emitCRMEvent(ctx, "order.failed", cid, map[string]any{
					"order_id": o.OrderID, "labelled_product": crmLabelledProductOf(o), "reason": missedCustomerReason,
				})
			}
		}
	}
	if closed > 0 {
		s.log.InfoContext(ctx, "subscription sweep closed missed orders", "before", cutoff, "orders", closed)
	}
	return closed
}

func joinAddress(a *address) string {
	parts := []string{}
	for _, p := range []string{a.Line1, a.Line2, a.City, a.Pincode} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// subscriptionOrderWorker — the background scheduler (no external cron). Ticks
// every 15 minutes; each tick runs the full lifecycle: it PREVIEWS the first
// still-editable day as a visible upcoming order, keeps previews reconciled
// with subscription edits until their 12:00 IST cut-off the day before, and
// at the first tick after noon LOCKS tomorrow's (delivery tasks created) — so
// the store manager's queue is filled by noon the day before the 05:00 route
// WITHOUT any consumer opening the app. The day-claim + lock guard make every
// duplicate tick (or replica) a no-op.
func (s *service) subscriptionOrderWorker(ctx context.Context) {
	const tick = 15 * time.Minute
	// Immediate first sweep on boot (covers a server restart mid-morning).
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	s.sweepSubscriptionOrders(runCtx, time.Now())
	cancel()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			s.sweepSubscriptionOrders(runCtx, now)
			cancel()
		}
	}
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *handler) createSubscription(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in subscriptionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	sub, err := h.svc.createSubscription(r.Context(), id, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Place today's / schedule tomorrow's order NOW, so it reaches the riders
	// without waiting for the next worker tick. Best-effort: the worker still
	// covers anything this misses, and the day claims make a repeat a no-op.
	kickCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	h.svc.sweepOneSubscription(kickCtx, sub, time.Now())
	cancel()
	sub.NextDeliveryDate = h.svc.nextDeliveryFor(r.Context(), sub, time.Now())
	writeJSON(w, http.StatusCreated, sub)
}

func (h *handler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.svc.listSubscriptionsFor(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now()
	for i := range list {
		list[i].NextDeliveryDate = h.svc.nextDeliveryFor(r.Context(), &list[i], now)
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) subscriptionAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, aerr := actorID(r)
		if aerr != nil {
			writeErr(w, aerr)
			return
		}
		sub, err := h.svc.setSubscriptionStatus(r.Context(), id, chi.URLParam(r, "id"), action)
		if err != nil {
			writeErr(w, err)
			return
		}
		sub.NextDeliveryDate = h.svc.nextDeliveryFor(r.Context(), sub, time.Now())
		writeJSON(w, http.StatusOK, sub)
	}
}

func (h *handler) patchSubscription(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Qty          *int             `json:"qty"`
		Frequency    *string          `json:"frequency"`
		DeliverySlot *string          `json:"delivery_slot"`
		StartDate    *string          `json:"start_date"`
		Vacations    *[]vacationRange `json:"vacations"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	sub, err := h.svc.patchSubscription(r.Context(), id, chi.URLParam(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	sub.NextDeliveryDate = h.svc.nextDeliveryFor(r.Context(), sub, time.Now())
	writeJSON(w, http.StatusOK, sub)
}

// sweepSubscriptions — DEV-only manual tick (mirrors /mandate/{id}/execute), so
// the subscribe→morning-order→store-queue flow is testable without waiting for
// the 15-minute worker tick.
func (h *handler) sweepSubscriptions(w http.ResponseWriter, r *http.Request) {
	if _, aerr := actorID(r); aerr != nil {
		writeErr(w, aerr)
		return
	}
	if !h.svc.deps.Cfg.OTPDevMode {
		writeErr(w, errForbidden("not available"))
		return
	}
	placed := h.svc.sweepSubscriptionOrders(r.Context(), time.Now())
	writeJSON(w, http.StatusOK, map[string]int{"placed": placed})
}

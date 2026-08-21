// CRM — Welcome Litre offer domain (spec: CRM/PYAAS_CRM_App_Developer_Spec_Rev3.md,
// config: crm_triggers.json, embedded — the single source of trigger truth).
//
// THE CAMPAIGN IN ONE LINE. A household enrols with a deliberately EMPTY wallet,
// receives a free 500 ml pack the next morning (pack 1), and is asked for money
// only afterwards; a SETTLED recharge >= the configured threshold within the
// grace window releases pack 2 free with a later delivery; otherwise the offer
// expires at day 7 with a mandatory "nothing has been charged" message.
//
// DESIGN DECISIONS THAT KEEP EXISTING FLOWS UNTOUCHED (each one deliberate —
// see the red-teamed architecture doc before "simplifying" any of them):
//
//   - The free pack is a STANDALONE ₹0 order with NO subscription link. The
//     subscription sweep selects on subscription_id, so its expiry/lock/refresh
//     steps can never see, cancel, re-price or line-rewrite the promo order;
//     the trial settle gate requires a subscription-linked parent, so the 2+2
//     ledger can never advance on it; and every wallet floor already passes at
//     a total of 0.0 — the correct number of floor edits is ZERO.
//   - The campaign SUBSCRIPTION is a completely normal daily plan. It ships
//     only when funded (existing floors, unchanged) — which IS the campaign:
//     free pack on day 1, recharge to continue.
//   - Offer state is STORED and transitioned with compare-and-set filters,
//     never inferred at read time; every transition appends to an embedded log
//     and emits offer_pack_state_change. A retried event cannot double-move.
//   - 2+2 EXCLUSIVITY (founder decision, option B): enrolment refuses anyone
//     with trial activity, and marks the enrollee's 2+2 ledger exhausted so
//     the two welcome offers can never stack. Organic (non-campaign) shoppers
//     keep the 2+2 exactly as today.
//   - Money units: config speaks PAISE (pack2_min_recharge_paise); this
//     codebase is float rupees. The threshold is converted ONCE, here, and the
//     release decision is made against the settled payment amount — never
//     against a float wallet delta.
//
// Everything is inert unless CRM_ENABLED=true AND the per-trigger flags allow.
package consumer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	collConsumerOffers = "consumer_offers"

	offerWelcomeLitre = "welcome_litre"

	pack1Pending   = "pending"
	pack1Delivered = "delivered"
	pack1Forfeited = "forfeited"

	pack2Locked    = "locked"
	pack2Pending   = "pending"
	pack2Delivered = "delivered"
	pack2Expired   = "expired"
)

// crmEnabled is the master isolation seam (layer 0). Off → every CRM entry
// point returns immediately and the binary behaves byte-identically to today.
func crmEnabled() bool { return os.Getenv("CRM_ENABLED") == "true" }

// offerTransition is one recorded state change — the audit trail the spec
// requires ("state is a column, never inferred at read time").
type offerTransition struct {
	PackNo int       `bson:"pack_no" json:"pack_no"`
	From   string    `bson:"from"    json:"from"`
	To     string    `bson:"to"      json:"to"`
	Reason string    `bson:"reason"  json:"reason"`
	At     time.Time `bson:"at"      json:"at"`
}

// consumerOffer is one (consumer, offer) enrolment — the CH-19 data model,
// translated from the spec's SQL columns to a self-contained document.
type consumerOffer struct {
	MongoID    primitive.ObjectID `bson:"_id,omitempty"      json:"-"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"        json:"-"`
	OfferID    string             `bson:"offer_id"           json:"offer_id"`
	EnrolledAt time.Time          `bson:"enrolled_at"        json:"enrolled_at"`

	Pack1State string `bson:"pack1_state" json:"pack1_state"`
	Pack2State string `bson:"pack2_state" json:"pack2_state"`

	// Enrolment provenance (analytics + reconciliation dimensions).
	SocietyID  string `bson:"society_id,omitempty"  json:"society_id,omitempty"`
	PromoterID string `bson:"promoter_id,omitempty" json:"promoter_id,omitempty"`
	AssetType  string `bson:"asset_type,omitempty"  json:"asset_type,omitempty"`

	// Abuse SIGNALS — flag for human review, never a hard reject (W-09).
	AddressHash     string `bson:"address_hash,omitempty"      json:"-"`
	DeviceFirstSeen string `bson:"device_first_seen,omitempty" json:"-"`
	AbuseFlagged    bool   `bson:"abuse_flagged,omitempty"     json:"abuse_flagged,omitempty"`

	// FirstDeliveryAt anchors every W schedule ("day 0" = the IST day pack 1
	// landed). Zero until pack 1 is delivered.
	FirstDeliveryAt *time.Time `bson:"first_delivery_at,omitempty" json:"first_delivery_at,omitempty"`

	// The subscription and promo orders this enrolment created — for support,
	// reconciliation and idempotent re-entry.
	SubscriptionID string `bson:"subscription_id,omitempty" json:"subscription_id,omitempty"`
	Pack1OrderID   string `bson:"pack1_order_id,omitempty"  json:"pack1_order_id,omitempty"`
	Pack2OrderID   string `bson:"pack2_order_id,omitempty"  json:"pack2_order_id,omitempty"`

	Transitions []offerTransition `bson:"transitions,omitempty" json:"transitions,omitempty"`
	CreatedAt   time.Time         `bson:"created_at" json:"-"`
	UpdatedAt   time.Time         `bson:"updated_at" json:"-"`
}

// entitledFreeDeliveries derives the CH-01 entitlement from offer state —
// an integer derived on demand, NEVER a stored counter that can drift.
func entitledFreeDeliveries(o *consumerOffer) int {
	if o == nil || o.OfferID == "" {
		return 0
	}
	n := 0
	if o.Pack1State == pack1Pending {
		n++
	}
	if o.Pack2State == pack2Pending {
		n++
	}
	return n
}

// crmAddressHash normalises an address into the W-09 duplicate-signal bucket:
// lowercased line + pincode, plus the geocode rounded to ~11 m (4 decimals).
// A hash match FLAGS for review — it never rejects (multi-family households
// are real, especially in older Lucknow properties).
func crmAddressHash(line1, pincode string, lat, lng float64) string {
	norm := strings.ToLower(strings.Join(strings.Fields(line1), " ")) + "|" + strings.TrimSpace(pincode)
	if lat != 0 || lng != 0 {
		norm += fmt.Sprintf("|%.4f,%.4f", lat, lng)
	}
	sum := sha1.Sum([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// ── Repository ──────────────────────────────────────────────────────────────

func (r *repository) offers() *mongo.Collection {
	return r.accounts.Database().Collection(collConsumerOffers)
}

func (r *repository) findOffer(ctx context.Context, consumerID primitive.ObjectID) (*consumerOffer, error) {
	var o consumerOffer
	err := r.offers().FindOne(ctx, bson.D{{Key: "consumer_id", Value: consumerID}}).Decode(&o)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("offer lookup failed")
	}
	return &o, nil
}

func (r *repository) findOffersByAddressHash(ctx context.Context, hash string) ([]consumerOffer, error) {
	cur, err := r.offers().Find(ctx, bson.D{{Key: "address_hash", Value: hash}}, options.Find().SetLimit(10))
	if err != nil {
		return nil, errInternal("offer hash lookup failed")
	}
	var out []consumerOffer
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("offer hash decode failed")
	}
	return out, nil
}

func (r *repository) insertOffer(ctx context.Context, o *consumerOffer) error {
	if _, err := r.offers().InsertOne(ctx, o); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("ALREADY_ENROLLED", "already enrolled")
		}
		return errInternal("offer store failed")
	}
	return nil
}

// transitionPack is THE state mutator: a compare-and-set on the expected `from`
// state, so a duplicate event, a retry or a second replica can never
// double-advance the machine. Returns (moved, error) — moved=false with a nil
// error means the CAS lost (someone already moved it), which callers treat as
// idempotent success.
func (r *repository) transitionPack(ctx context.Context, consumerID primitive.ObjectID, packNo int, from, to, reason string, extra bson.D) (bool, error) {
	field := "pack1_state"
	if packNo == 2 {
		field = "pack2_state"
	}
	now := time.Now().UTC()
	set := bson.D{{Key: field, Value: to}, {Key: "updated_at", Value: now}}
	set = append(set, extra...)
	res, err := r.offers().UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}, {Key: field, Value: from}},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$push", Value: bson.D{{Key: "transitions", Value: offerTransition{
				PackNo: packNo, From: from, To: to, Reason: reason, At: now,
			}}}},
		})
	if err != nil {
		return false, errInternal("offer transition failed")
	}
	return res.ModifiedCount == 1, nil
}

// ensureCRMIndexes creates the CRM collections' indexes OUTSIDE the fatal boot
// path — a failed campaign index must never take the platform down (the main
// ensureIndexes panics on failure by design; this one logs and continues).
func (s *service) ensureCRMIndexes(ctx context.Context) bool {
	ok := true
	offers := s.repo.offers()
	_, err := offers.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "offer_id", Value: 1}},
			Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "address_hash", Value: 1}},
			Options: options.Index().SetPartialFilterExpression(bson.D{{Key: "address_hash", Value: bson.D{{Key: "$exists", Value: true}}}})},
		{Keys: bson.D{{Key: "offer_id", Value: 1}, {Key: "pack2_state", Value: 1}}},
	})
	if err != nil {
		s.log.Warn("crm: offers index setup failed (continuing)", "err", err)
		ok = false
	}
	disp := s.repo.accounts.Database().Collection(collCRMDispatch)
	if _, err := disp.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// The exactly-once claim: one row per (trigger, consumer, IST day).
		{Keys: bson.D{{Key: "trigger_id", Value: 1}, {Key: "consumer_id", Value: 1}, {Key: "ist_day", Value: 1}},
			Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}}},
	}); err != nil {
		s.log.Warn("crm: dispatch index setup failed (continuing)", "err", err)
		ok = false
	}
	ev := s.repo.accounts.Database().Collection(collCRMEvents)
	if _, err := ev.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: 1}},
	}); err != nil {
		s.log.Warn("crm: events index setup failed (continuing)", "err", err)
		ok = false
	}
	inbox := s.repo.accounts.Database().Collection(collConsumerInbox)
	if _, err := inbox.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}},
	}); err != nil {
		s.log.Warn("crm: inbox index setup failed (continuing)", "err", err)
		ok = false
	}
	return ok
}

// ── Enrolment ───────────────────────────────────────────────────────────────

type crmEnrolInput struct {
	Phone      string  `json:"phone"`
	Name       string  `json:"name"`
	Line1      string  `json:"line1"`
	Pincode    string  `json:"pincode"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	SocietyID  string  `json:"society_id"`
	PromoterID string  `json:"promoter_id"`
	AssetType  string  `json:"asset_type"` // poster|standee|hanger|whatsapp|promoter
}

type crmEnrolResult struct {
	ConsumerID     string `json:"consumer_id"`
	OfferID        string `json:"offer_id"`
	SubscriptionID string `json:"subscription_id"`
	Pack1OrderID   string `json:"pack1_order_id"`
	Pack1For       string `json:"pack1_scheduled_for"` // IST day the free pack ships
	AbuseFlagged   bool   `json:"abuse_flagged"`
}

// crmEnrol is the server-side enrolment the promoter/ops route drives — the
// campaign's founding act. It is deliberately the ONLY writer of offer docs.
//
// Sequence (each step idempotent or compare-and-set):
//  1. find-or-create the account by phone (promoter-asserted identity; the
//     customer still OTP-verifies the same account when they install the app);
//  2. eligibility: never paid, no existing offer, no 2+2 trial activity;
//  3. abuse signals: same address hash elsewhere → FLAG, never reject;
//  4. mark the 2+2 ledger exhausted (offer exclusivity, founder option B);
//  5. create the default address if none, then a NORMAL daily subscription
//     (ships only when funded — the existing floors are the campaign design);
//  6. mint the standalone ₹0 pack-1 order + its delivery task for the next
//     morning, and record the offer with pack1=pending, pack2=locked;
//  7. emit offer_enrolled + the W-01 welcome dispatch.
func (s *service) crmEnrol(ctx context.Context, actor string, in crmEnrolInput) (*crmEnrolResult, error) {
	if !crmEnabled() {
		return nil, errForbidden("CRM is not enabled")
	}
	phone := normalizePhone(in.Phone)
	if len(phone) != 10 {
		return nil, errBadRequest("a valid 10-digit phone is required")
	}
	if strings.TrimSpace(in.Line1) == "" {
		return nil, errBadRequest("the delivery address line is required")
	}
	cfg := crmOfferConfig()

	// 1) account by CANONICAL phone (+91…) — the OTP login path keys accounts
	// by this exact form, so the household's later app login MUST land on the
	// same document. A bare-10-digit key here would orphan the whole offer
	// behind a duplicate account the app can never see.
	canonical := crmCanonicalPhone(phone)
	acct, err := s.repo.findAccountByPhone(ctx, canonical)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		now := time.Now().UTC()
		acct = &account{ID: primitive.NewObjectID(), Phone: canonical, Status: "ACTIVE", CreatedAt: now, UpdatedAt: now}
		if err := s.repo.insertAccount(ctx, acct); err != nil {
			return nil, err
		}
		// Empty wallet, verifyOTP parity — money only enters via a real top-up.
		_ = s.repo.insertWallet(ctx, &wallet{
			ID: primitive.NewObjectID(), ConsumerID: acct.ID, Currency: "INR", Seq: 0,
		})
		if strings.TrimSpace(in.Name) != "" {
			name := strings.TrimSpace(in.Name)
			if upd, uerr := s.repo.updateAccount(ctx, acct.ID, bson.D{{Key: "full_name", Value: &name}}); uerr == nil {
				acct = upd
			}
		}
	}

	// 2) eligibility — plain reads first; the unique (consumer, offer) index on
	// the offer INSERT below is the race-proof arbiter (nothing is minted until
	// this consumer owns the offer doc).
	if acct.HasPaidOrder {
		return nil, errUnprocessable("NOT_ELIGIBLE", "this customer has already paid for an order")
	}
	// has_paid_order only exists from this release forward — a PRE-CRM paying
	// customer carries none, so also refuse anyone with real order history.
	// (Campaign spec: the Welcome Litre is for households that have never paid.)
	if n, cerr := s.repo.orders.CountDocuments(ctx, bson.D{
		{Key: "user_id", Value: acct.ID.Hex()},
		{Key: "total", Value: bson.D{{Key: "$gt", Value: 0}}},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
	}); cerr != nil {
		return nil, errInternal("order history check failed")
	} else if n > 0 {
		return nil, errUnprocessable("NOT_ELIGIBLE", "this customer already has paid order history")
	}
	if t, terr := s.repo.getOrCreateTrial(ctx, acct.ID); terr == nil && (t.DeliveredPaid > 0 || t.DeliveredFree > 0) {
		return nil, errUnprocessable("NOT_ELIGIBLE", "this customer already has welcome-trial activity")
	}

	// 3) abuse signals — flag, never reject.
	hash := crmAddressHash(in.Line1, in.Pincode, in.Lat, in.Lng)
	flagged := false
	if dupes, derr := s.repo.findOffersByAddressHash(ctx, hash); derr == nil && len(dupes) > 0 {
		flagged = true
	}

	// 4) THE ARBITRATION POINT — insert the offer FIRST, before any minting.
	// Two concurrent enrols both reach here; the unique (consumer_id, offer_id)
	// index lets exactly one through, and the loser has created NOTHING yet —
	// no orphan subscription, no orphan ₹0 order, no double free milk.
	// A crash between here and the finalize below leaves an INCOMPLETE offer
	// (pack1_order_id "") which the next enrol call RESUMES instead of refusing.
	now := time.Now().UTC()
	offer := &consumerOffer{
		ConsumerID: acct.ID, OfferID: offerWelcomeLitre, EnrolledAt: now,
		Pack1State: pack1Pending, Pack2State: pack2Locked,
		SocietyID: in.SocietyID, PromoterID: in.PromoterID, AssetType: in.AssetType,
		AddressHash: hash, AbuseFlagged: flagged,
		Transitions: []offerTransition{{PackNo: 1, From: "", To: pack1Pending, Reason: "enrolled by " + actor, At: now}},
		CreatedAt:   now, UpdatedAt: now,
	}
	freshOffer := true
	if err := s.repo.insertOffer(ctx, offer); err != nil {
		existing, e2 := s.repo.findOffer(ctx, acct.ID)
		if e2 != nil || existing == nil {
			return nil, err // real conflict surfaced as-is
		}
		if existing.Pack1OrderID != "" {
			return nil, errConflict("ALREADY_ENROLLED", "already enrolled in "+existing.OfferID)
		}
		// Incomplete twin (crashed or racing mid-mint) — resume it.
		offer, freshOffer = existing, false
		flagged = existing.AbuseFlagged
	}

	// 5) offer exclusivity: exhaust the 2+2 so the offers can never stack.
	// (Campaign-only blast radius: touches ONLY this consumer's trial doc.)
	if _, err := s.repo.trials.UpdateOne(ctx,
		bson.D{{Key: "consumer_id", Value: acct.ID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "delivered_paid", Value: trialPaidDays},
			{Key: "delivered_free", Value: trialFreeDays},
			{Key: "phase", Value: trialPhaseDone},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}},
	); err != nil {
		return nil, errInternal("trial exclusivity mark failed")
	}

	// 6) address (only if the account has none), then the normal subscription
	// and the standalone ₹0 pack-1 order for the next IST morning.
	addr, aerr := s.subscriptionAddress(ctx, acct.ID)
	if aerr != nil || addr == nil || addr.Lat == nil || addr.Lng == nil {
		lat, lng := in.Lat, in.Lng
		na := &address{
			ID: primitive.NewObjectID(), ConsumerID: acct.ID, Label: "Home",
			Line1: strings.TrimSpace(in.Line1), Pincode: strings.TrimSpace(in.Pincode),
			City: "Lucknow", IsDefault: true, Lat: &lat, Lng: &lng, CreatedAt: time.Now().UTC(),
		}
		if _, ierr := s.repo.addresses.InsertOne(ctx, na); ierr != nil {
			return nil, errInternal("address store failed")
		}
		addr = na
	}
	sub, serr := s.crmCreateSubscription(ctx, acct.ID, cfg)
	if serr != nil {
		return nil, serr // offer stays incomplete — a retry resumes right here
	}
	packDay := istDay(time.Now().Add(24 * time.Hour))
	pack1, perr := s.mintPromoPackOrder(ctx, acct, addr, packDay, 1)
	if perr != nil {
		s.crmDeleteSubscription(ctx, sub.SubscriptionID) // never shipped — safe to retract
		return nil, perr
	}

	// 7) FINALIZE — exactly one resumer/creator wins the empty-ids slot; a
	// loser retracts its own scaffolding so no duplicate free milk can ship.
	res, uerr := s.repo.offers().UpdateOne(ctx,
		bson.D{
			{Key: "consumer_id", Value: acct.ID}, {Key: "offer_id", Value: offerWelcomeLitre},
			{Key: "pack1_order_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "subscription_id", Value: sub.SubscriptionID},
			{Key: "pack1_order_id", Value: pack1.OrderID},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
	if uerr != nil {
		return nil, errInternal("offer finalize failed")
	}
	if res.ModifiedCount == 0 {
		// A concurrent resume finalized first — retract our duplicates and
		// report the winner's result.
		s.crmDeleteSubscription(ctx, sub.SubscriptionID)
		s.crmRetractPromoOrder(ctx, pack1.OrderID)
		winner, werr := s.repo.findOffer(ctx, acct.ID)
		if werr != nil || winner == nil || winner.Pack1OrderID == "" {
			return nil, errConflict("ALREADY_ENROLLED", "enrolment finished on a concurrent request")
		}
		return &crmEnrolResult{
			ConsumerID: acct.ID.Hex(), OfferID: offerWelcomeLitre,
			SubscriptionID: winner.SubscriptionID, Pack1OrderID: winner.Pack1OrderID,
			Pack1For: packDay, AbuseFlagged: winner.AbuseFlagged,
		}, nil
	}

	if freshOffer {
		s.emitCRMEvent(ctx, "offer_enrolled", acct.ID, map[string]any{
			"offer_id": offerWelcomeLitre, "society_id": in.SocietyID,
			"promoter_id": in.PromoterID, "asset_type": in.AssetType,
		})
		if flagged {
			s.crmNotifyAdmins(ctx, "CRM_ABUSE_FLAG", map[string]string{
				"phone": phone, "reason": "address_hash match — second offer at the same address (review, do not auto-reject)",
			})
			s.emitCRMEvent(ctx, "abuse_flag_raised", acct.ID, map[string]any{"rule": "address_match", "entity": "offer"})
		}
	}
	// W-01 — welcome confirmation; the dispatch-log claim dedupes a resume.
	s.crmDispatch(ctx, "W-01", acct.ID, map[string]string{})

	return &crmEnrolResult{
		ConsumerID: acct.ID.Hex(), OfferID: offerWelcomeLitre,
		SubscriptionID: sub.SubscriptionID, Pack1OrderID: pack1.OrderID,
		Pack1For: packDay, AbuseFlagged: flagged,
	}, nil
}

// crmDeleteSubscription retracts a campaign subscription that lost the enrol
// finalize race — it has never shipped (start date is tomorrow) and nothing
// references it, so a hard delete is the honest cleanup.
func (s *service) crmDeleteSubscription(ctx context.Context, subscriptionID string) {
	if subscriptionID == "" {
		return
	}
	if _, err := s.repo.subscriptions.DeleteOne(ctx, bson.D{{Key: "subscription_id", Value: subscriptionID}}); err != nil {
		s.log.Warn("crm: losing-subscription retract failed", "subscription", subscriptionID, "err", err)
	}
}

// crmRetractPromoOrder cancels a ₹0 promo order (and fails its delivery task)
// that lost the enrol/unlock finalize race, so the store never ships two free
// packs to one household.
func (s *service) crmRetractPromoOrder(ctx context.Context, orderID string) {
	if orderID == "" {
		return
	}
	if _, err := s.repo.orders.UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: orderID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "cancelled"}, {Key: "updated_at", Value: time.Now().UTC()}}}},
	); err != nil {
		s.log.Warn("crm: losing-order retract failed", "order", orderID, "err", err)
	}
	if _, err := s.repo.deliveries.UpdateMany(ctx,
		bson.D{{Key: "order_id", Value: orderID}, {Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{"DELIVERED", "FAILED"}}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "FAILED"}, {Key: "failure_reason", Value: "duplicate promo pack retracted"}, {Key: "updated_at", Value: time.Now().UTC()}}}},
	); err != nil {
		s.log.Warn("crm: losing-delivery retract failed", "order", orderID, "err", err)
	}
}

// crmCreateSubscription creates the campaign's NORMAL daily plan: the offer
// SKU at the server-authoritative price, quantity honouring the app's
// 1 L/day milk floor (2 × 500 ml). It intentionally reuses the plain
// subscription document — the sweep treats it identically to any other plan.
func (s *service) crmCreateSubscription(ctx context.Context, consumerID primitive.ObjectID, cfg crmOffer) (*subscription, error) {
	ix, err := s.loadPriceIndex(ctx)
	if err != nil {
		return nil, err
	}
	unit, ok := ix.priceFor(cfg.SeedSKU, "")
	if !ok {
		return nil, errUnprocessable("SKU_UNAVAILABLE", "the campaign product is not sellable right now")
	}
	now := time.Now().UTC()
	sub := &subscription{
		MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(),
		ConsumerID: consumerID, ProductID: cfg.SeedSKU, Name: ix.nameFor(cfg.SeedSKU),
		Qty: cfg.SubscriptionQty, UnitPrice: round2(unit), Frequency: "daily",
		Status: "active", StartDate: istDay(time.Now().Add(24 * time.Hour)),
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.repo.subscriptions.InsertOne(ctx, sub); err != nil {
		return nil, errInternal("subscription store failed")
	}
	return sub, nil
}

// mintPromoPackOrder mints the STANDALONE zero-value promotional order —
// CH-04's ledger line, and the load-bearing isolation trick. Server-only:
// nothing here ever reads a client-supplied price or flag, so the price
// authority's ₹0 rejection for client input stays fully intact.
func (s *service) mintPromoPackOrder(ctx context.Context, acct *account, addr *address, day string, packNo int) (*order, error) {
	cfg := crmOfferConfig()
	ix, err := s.loadPriceIndex(ctx)
	if err != nil {
		return nil, err
	}
	// Promotional value = today's real catalog price (MRP-equivalent for the
	// disclosure), price on the line = 0 by construction.
	value, _ := ix.priceFor(cfg.SeedSKU, "")
	name := ix.nameFor(cfg.SeedSKU)
	if name == "" {
		name = "Parag Full Cream 500 ml"
	}
	fullName := ""
	if acct.FullName != nil {
		fullName = *acct.FullName
	}
	now := time.Now().UTC()
	o := &order{
		MongoID: primitive.NewObjectID(), OrderID: newOrderID(), UserID: acct.ID.Hex(), Status: "placed",
		Subtotal: 0, DeliveryFee: 0, Total: 0, PaymentMethod: "wallet",
		AddressLabel: addr.Label, AddressText: joinAddress(addr), RiderID: nil,
		PlacedAt: now, Priority: "normal",
		DeliveryWindow: "05:00 - 07:30 AM", Lane: "morning",
		DeliveryDate: day,
		OfferID:      offerWelcomeLitre, OfferPack: packNo,
		Items: []orderItem{{
			ID: newItemID(), ProductID: cfg.SeedSKU, Name: name, Variant: "500ml",
			Price: 0, Qty: 1,
			IsPromotional: true, PromotionalValue: round2(value), SupplySource: "parag",
		}},
		ConsumerName: fullName, Phone: acct.Phone,
		CreatedAt: now, UpdatedAt: now,
		ScheduledFor: day, // display only — no subscription_id, so the sweep never selects it
	}
	if addr.Lat != nil && addr.Lng != nil {
		o.Geo = &geoPoint{Lat: *addr.Lat, Lng: *addr.Lng}
	}
	if err := s.repo.insertOrder(ctx, o); err != nil {
		return nil, err
	}
	s.createDeliveryForOrder(ctx, o) // amount 0 → PREPAID task; settle writes the ₹0 gate row
	return o, nil
}

// crmOnPackDelivered advances the state machine when a promotional order
// settles as delivered. Compare-and-set: replays are no-ops. A transient error
// returns non-nil so the outbox retries the event (the CAS makes that safe).
func (s *service) crmOnPackDelivered(ctx context.Context, consumerID primitive.ObjectID, packNo int, orderID string) error {
	now := time.Now().UTC()
	switch packNo {
	case 1:
		moved, err := s.repo.transitionPack(ctx, consumerID, 1, pack1Pending, pack1Delivered,
			"promotional order "+orderID+" delivered",
			bson.D{{Key: "first_delivery_at", Value: now}})
		if err != nil {
			return err
		}
		if !moved {
			return nil // replay — already delivered
		}
		s.emitCRMEvent(ctx, "offer_pack_state_change", consumerID, map[string]any{"pack_no": 1, "from": pack1Pending, "to": pack1Delivered})
		s.crmDispatch(ctx, "W-02", consumerID, map[string]string{})
	case 2:
		moved, err := s.repo.transitionPack(ctx, consumerID, 2, pack2Pending, pack2Delivered,
			"promotional order "+orderID+" delivered", nil)
		if err != nil {
			return err
		}
		if !moved {
			return nil
		}
		s.emitCRMEvent(ctx, "offer_pack_state_change", consumerID, map[string]any{"pack_no": 2, "from": pack2Pending, "to": pack2Delivered})
		// C-01 product labelling: the token resolves from the ORDER LINE, never
		// the customer record — "{product_name} — delivered by PYAAS".
		s.crmDispatch(ctx, "W-05", consumerID, map[string]string{
			"labelled_product": s.crmLabelledProduct(ctx, orderID),
		})
	}
	return nil
}

// crmLabelledProduct renders the C-01 supply-source label from the order line.
func (s *service) crmLabelledProduct(ctx context.Context, orderID string) string {
	name := "500 ml Parag Full Cream"
	if o, err := s.repo.findOrderAnyUser(ctx, orderID); err == nil && o != nil && len(o.Items) > 0 && o.Items[0].Name != "" {
		name = o.Items[0].Name
	}
	return name + " — delivered by PYAAS"
}

// crmOnRechargeSettled is the W-04 release: SETTLED funds only (the caller sits
// past the wallet's exactly-once gate), threshold from config in paise
// converted here once. The grace window uses the SAME IST-day arithmetic as
// the expiry sweep (one predicate family — no wall-clock/day-boundary gap):
//   - recharge BEFORE pack 1 lands (the excited signup) → unlocks;
//   - recharge any time up to and including day <grace> (the very date the
//     W-06 nudge advertises) → unlocks;
//   - the sweep expires only from day grace+1, so no settled recharge inside
//     the advertised window can ever be discarded.
//
// A transient error returns non-nil so the outbox retries; a pack stuck
// PENDING with no order (mint failed mid-flight) is RESUMED on the retry.
func (s *service) crmOnRechargeSettled(ctx context.Context, consumerID primitive.ObjectID, amountRupees float64) error {
	cfg := crmOfferConfig()
	threshold := float64(cfg.Pack2MinRechargePaise) / 100.0
	if amountRupees+1e-9 < threshold {
		return nil
	}
	o, err := s.repo.findOffer(ctx, consumerID)
	if err != nil {
		return err
	}
	if o == nil || o.OfferID != offerWelcomeLitre {
		return nil
	}
	if cfg.PacksInEntitlement < 2 {
		return nil // single-pack configuration: W-04..W-07 no-op by design
	}
	if o.FirstDeliveryAt != nil && daysSinceFirstDelivery(o, time.Now()) > cfg.Pack2GraceDays {
		return nil // grace passed; the sweep owns the expiry + message
	}
	switch o.Pack2State {
	case pack2Locked:
		moved, terr := s.repo.transitionPack(ctx, consumerID, 2, pack2Locked, pack2Pending,
			fmt.Sprintf("settled recharge ₹%.2f >= ₹%.2f", amountRupees, threshold), nil)
		if terr != nil {
			return terr
		}
		if !moved {
			return nil // raced with a twin event — that one owns the mint
		}
		s.emitCRMEvent(ctx, "offer_pack_state_change", consumerID, map[string]any{"pack_no": 2, "from": pack2Locked, "to": pack2Pending})
	case pack2Pending:
		if o.Pack2OrderID != "" {
			return nil // fully unlocked already — pure replay
		}
		// pending-without-order: a previous attempt died after the CAS — resume.
	default:
		return nil // delivered/expired — nothing to do
	}

	// Schedule pack 2 as its own ₹0 order for the next morning ("scheduled onto
	// the next delivery" — the rider carries it with that morning's paid order).
	acct, aerr := s.repo.findAccountByID(ctx, consumerID)
	if aerr != nil || acct == nil {
		return errInternal("crm: pack2 account lookup failed")
	}
	addr, derr := s.subscriptionAddress(ctx, consumerID)
	if derr != nil || addr == nil {
		return errInternal("crm: pack2 address lookup failed")
	}
	day := istDay(time.Now().Add(24 * time.Hour))
	p2, perr := s.mintPromoPackOrder(ctx, acct, addr, day, 2)
	if perr != nil {
		return perr // event retries; state=pending resumes here
	}
	res, uerr := s.repo.offers().UpdateOne(ctx,
		bson.D{
			{Key: "consumer_id", Value: consumerID},
			{Key: "pack2_order_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "pack2_order_id", Value: p2.OrderID}, {Key: "updated_at", Value: time.Now().UTC()}}}})
	if uerr != nil || res.ModifiedCount == 0 {
		// Lost to a concurrent resume — retract our duplicate pack.
		s.crmRetractPromoOrder(ctx, p2.OrderID)
		return nil
	}
	// W-04 only AFTER the pack is really minted (never promise milk that has no
	// order behind it); the dispatch-log claim dedupes retries.
	s.crmDispatch(ctx, "W-04", consumerID, map[string]string{})
	return nil
}

// markHasPaidOrder sets the CH-19 fact on the first order whose SETTLED value
// is greater than zero — never on a promotional-only order (the caller
// guarantees amount > 0). Fire-and-forget; a miss self-heals on the next paid
// settle.
func (s *service) markHasPaidOrder(ctx context.Context, consumerID primitive.ObjectID) {
	if !crmEnabled() {
		return // CRM off → zero extra writes on the settle path
	}
	_, _ = s.repo.accounts.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: consumerID}, {Key: "has_paid_order", Value: bson.D{{Key: "$ne", Value: true}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "has_paid_order", Value: true}, {Key: "updated_at", Value: time.Now().UTC()}}}})
}

// crmCanonicalPhone maps a normalized 10-digit phone onto the account
// collection's canonical key ("+91" + digits) — the SAME form verifyOTP
// stores, so CRM and login always resolve to one account.
func crmCanonicalPhone(digits string) string { return "+91" + digits }

// istDay formats a time as the IST calendar day the campaign schedules on.
func istDay(t time.Time) string { return t.In(istZone).Format("2006-01-02") }

// daysSinceFirstDelivery is the W-schedule anchor: 0 on the delivery day.
func daysSinceFirstDelivery(o *consumerOffer, now time.Time) int {
	if o == nil || o.FirstDeliveryAt == nil {
		return -1
	}
	first, _ := time.ParseInLocation("2006-01-02", istDay(*o.FirstDeliveryAt), istZone)
	cur, _ := time.ParseInLocation("2006-01-02", istDay(now), istZone)
	return int(math.Round(cur.Sub(first).Hours() / 24))
}

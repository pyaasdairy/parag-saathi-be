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
	"errors"
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
	// Source — who founded the enrolment: "promoter" (ops console) or "self"
	// (the app's own funnel). Analytics + promoter-quality tracking only.
	Source string `bson:"source,omitempty" json:"source,omitempty"`
	// Pack2UnlockedAt — when the qualifying recharge SETTLED. Anchors the
	// terms' 14-day attach window ("delivered with your next delivery,
	// provided that takes place within 14 days of the recharge").
	Pack2UnlockedAt *time.Time `bson:"pack2_unlocked_at,omitempty" json:"-"`
	Pack1OrderID    string     `bson:"pack1_order_id,omitempty"  json:"pack1_order_id,omitempty"`
	Pack2OrderID    string     `bson:"pack2_order_id,omitempty"  json:"pack2_order_id,omitempty"`

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
	if _, err := disp.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}},
	}); err != nil {
		s.log.Warn("crm: dispatch index setup failed (continuing)", "err", err)
		ok = false
	}
	// The exactly-once claim: one row per (trigger, consumer, IST day, scope).
	if !s.ensureCRMClaimIndex(ctx, disp) {
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
	if _, err := s.repo.crmSchedulesCol().Indexes().CreateMany(ctx, []mongo.IndexModel{
		// One deferred dispatch per (trigger, event): an outbox replay is dropped.
		{Keys: bson.D{{Key: "trigger_id", Value: 1}, {Key: "event_id", Value: 1}},
			Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "due_at", Value: 1}}},
	}); err != nil {
		s.log.Warn("crm: schedules index setup failed (continuing)", "err", err)
		ok = false
	}
	return ok
}

const (
	// crmClaimIndexLegacy is the auto-named (trigger, consumer, day) unique
	// index every deployment before the scoped claim built.
	crmClaimIndexLegacy = "trigger_id_1_consumer_id_1_ist_day_1"
	// crmClaimIndexScoped is its replacement, with the event scope in the key.
	crmClaimIndexScoped = "crm_claim_scoped"
)

// ensureCRMClaimIndex migrates the exactly-once claim from (trigger,
// consumer, day) to (trigger, consumer, day, scope_key). Rows written before
// the scope existed are stamped scope_key "" first, so the new key covers
// them and a per-day claim taken under the old index stays taken. The legacy
// index is dropped only once the scoped one exists: a refused build (the
// same guard ensureSubscriptionDayIndex applies) logs the colliding row
// count and leaves the old index in force, so no claim is ever unprotected.
func (s *service) ensureCRMClaimIndex(ctx context.Context, disp *mongo.Collection) bool {
	if _, err := disp.UpdateMany(ctx,
		bson.D{{Key: "scope_key", Value: bson.D{{Key: "$exists", Value: false}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "scope_key", Value: ""}}}}); err != nil {
		s.log.Warn("crm: dispatch scope backfill failed (continuing)", "err", err)
		return false
	}
	if _, err := disp.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "trigger_id", Value: 1}, {Key: "consumer_id", Value: 1}, {Key: "ist_day", Value: 1}, {Key: "scope_key", Value: 1}},
		Options: options.Index().SetName(crmClaimIndexScoped).SetUnique(true),
	}); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			s.log.Error("crm: scoped claim index refused, legacy claim index kept",
				"duplicate_claims", s.crmCountClaimDuplicates(ctx, disp), "err", err)
		} else {
			s.log.Warn("crm: scoped claim index setup failed (continuing)", "err", err)
		}
		return false
	}
	if _, err := disp.Indexes().DropOne(ctx, crmClaimIndexLegacy); err != nil && !crmIsIndexNotFound(err) {
		s.log.Warn("crm: legacy claim index drop failed (continuing)", "err", err)
		return false
	}
	return true
}

// crmCountClaimDuplicates counts the (trigger, consumer, day, scope) keys
// held by more than one dispatch row - the rows a scoped index build refuses.
func (s *service) crmCountClaimDuplicates(ctx context.Context, disp *mongo.Collection) int64 {
	cur, err := disp.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "t", Value: "$trigger_id"}, {Key: "c", Value: "$consumer_id"},
				{Key: "d", Value: "$ist_day"}, {Key: "s", Value: "$scope_key"},
			}},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$match", Value: bson.D{{Key: "n", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
		{{Key: "$count", Value: "keys"}},
	})
	if err != nil {
		return -1
	}
	defer cur.Close(ctx)
	var out []struct {
		Keys int64 `bson:"keys"`
	}
	if err := cur.All(ctx, &out); err != nil || len(out) == 0 {
		return 0
	}
	return out[0].Keys
}

// crmIsIndexNotFound reports the server's IndexNotFound (27) answer to a drop
// of an index that is already gone - the expected reply on every boot after
// the migration ran once.
func crmIsIndexNotFound(err error) bool {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code == 27 || ce.Name == "IndexNotFound"
	}
	return false
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
	AssetType  string  `json:"asset_type"` // poster|standee|hanger|whatsapp|promoter|self
	// Plan choice (offer terms §6.3: daily or alternate-day, Toned or Full
	// Cream, 500 ml or 1 L). Empty → the campaign default (2 × FCM 500 ml
	// daily). The FREE packs are always the seed SKU regardless of plan.
	PlanProductID string `json:"plan_product_id"`
	PlanQty       int    `json:"plan_qty"`
	PlanFrequency string `json:"plan_frequency"` // daily | alternate
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

	// Address: create the promoter-captured one only if the account has none
	// with coordinates (the pin is what store routing + the sweep read).
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
	return s.crmEnrolCore(ctx, actor, acct, addr, in, "promoter")
}

// crmSelfEnrol — THE APP'S OWN FUNNEL (offer terms §3.1: "Start a subscription
// in the app. No payment and no wallet balance is required."). The signed-in
// customer enrols themselves: identity from the JWT, address from their saved
// default, zone check from the same serviceability engine checkout uses.
// Everything else — eligibility, arbitration, resume, minting — is
// byte-identical to the promoter path: ONE enrolment state machine, forever.
func (s *service) crmSelfEnrol(ctx context.Context, consumerID primitive.ObjectID, in crmEnrolInput) (*crmEnrolResult, error) {
	if !crmEnabled() {
		return nil, errForbidden("CRM is not enabled")
	}
	acct, err := s.repo.findAccountByID(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, errUnauthorized("account not found")
	}
	// The saved address IS the enrolment address (terms §1.2: the app checks
	// the area the moment the address is entered — both exist by subscribe).
	addr, aerr := s.subscriptionAddress(ctx, consumerID)
	if aerr != nil {
		return nil, aerr
	}
	if addr == nil || addr.Lat == nil || addr.Lng == nil {
		return nil, errUnprocessable("ADDRESS_REQUIRED", "save your delivery address (with the map pin) before starting the offer")
	}
	sv, serr := s.serviceability(ctx, *addr.Lat, *addr.Lng, addr.Pincode)
	if serr != nil {
		return nil, serr
	}
	if !sv.Serviceable {
		// W-08 (serviceability.checked): the signed-in member asked and the
		// answer is no; the trigger's per_customer cap makes it one message ever.
		s.emitCRMEvent(ctx, "serviceability.checked", consumerID, map[string]any{
			"in_zone": false, "pincode": addr.Pincode, "source": "enrol",
		})
		return nil, errUnprocessable("NOT_SERVICEABLE", "we don't deliver to this address yet — join the waitlist and the offer stays available for you")
	}
	// Abuse hash + audit fields derive from the STORED address so a self and a
	// promoter enrolment at the same door collide on the same household key.
	in.Line1, in.Pincode = addr.Line1, addr.Pincode
	in.Lat, in.Lng = *addr.Lat, *addr.Lng
	in.Phone = acct.Phone
	if in.AssetType == "" {
		in.AssetType = "self"
	}
	return s.crmEnrolCore(ctx, "self:"+consumerID.Hex(), acct, addr, in, "self")
}

// crmEnrolCore is the single enrolment state machine both entries share.
func (s *service) crmEnrolCore(ctx context.Context, actor string, acct *account, addr *address, in crmEnrolInput, source string) (*crmEnrolResult, error) {
	cfg := crmOfferConfig()
	phone := normalizePhone(acct.Phone)

	// Plan choice (validated) — defaults to the campaign plan when absent.
	planProduct, planQty, planFreq, perr := s.crmValidatePlan(in, cfg)
	if perr != nil {
		return nil, perr
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
	// ONE WELCOME PER HOUSEHOLD, FOREVER: the free_pack_claims registry is
	// keyed by PHONE and deliberately SURVIVES account erasure, so
	// delete-account → re-signup cannot re-arm this offer (or the 2+2 — both
	// welcome offers share the registry). Read-only here; the registry is
	// WRITTEN only after the offer doc wins arbitration below.
	if claimed, ok := s.crmHouseholdClaimed(ctx, acct.ID, acct.Phone); ok && claimed {
		return nil, errUnprocessable("NOT_ELIGIBLE", "this household has already used a welcome offer")
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
		Pack1State: pack1Pending, Pack2State: pack2Locked, Source: source,
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

	// 6) the customer's chosen plan (a NORMAL subscription — ships only when
	// funded) and the standalone ₹0 pack-1 order for the next IST morning.
	sub, minted, serr := s.crmEnsureSubscription(ctx, acct.ID, planProduct, planQty, planFreq)
	if serr != nil {
		return nil, serr // offer stays incomplete — a retry resumes right here
	}
	// retractSub undoes ONLY a plan this call minted. A plan the household
	// already had is theirs — deleting it on a failed enrolment would cancel
	// the milk they were already expecting.
	retractSub := func() {
		if minted {
			s.crmDeleteSubscription(ctx, sub.SubscriptionID)
		}
	}
	packDay := istDay(time.Now().Add(24 * time.Hour))
	pack1, perr := s.mintPromoPackOrder(ctx, acct, addr, packDay, 1)
	if perr != nil {
		retractSub() // never shipped — safe to retract
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
		retractSub()
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

	// Register the household claim NOW that this consumer owns the offer.
	// Idempotent (same consumer re-claims fine); the impossible different-
	// consumer case (phone is unique on accounts) logs and never unwinds.
	s.crmRegisterHouseholdClaim(ctx, acct.ID, acct.Phone)

	if freshOffer {
		s.emitCRMEvent(ctx, "offer_enrolled", acct.ID, map[string]any{
			"offer_id": offerWelcomeLitre, "source": source, "society_id": in.SocietyID,
			"promoter_id": in.PromoterID, "asset_type": in.AssetType,
		})
		if flagged {
			s.crmNotifyAdmins(ctx, "CRM_ABUSE_FLAG", map[string]string{
				"phone": phone, "reason": "address_hash match — second offer at the same address (review, do not auto-reject)",
			})
			s.emitCRMEvent(ctx, "abuse_flag_raised", acct.ID, map[string]any{"rule": "address_match", "entity": "offer"})
		}
	}
	// W-01 — welcome confirmation. Dispatched by the WORKER (crmRouteEvent),
	// never on the request: the SMS leg is a 10 s provider call, and an app
	// that backgrounded mid-enrol used to strand the row at CLAIMED with the
	// day's claim burned. Emitted on EVERY won finalize (a resume too); the
	// dispatch-log claim dedupes.
	s.emitCRMEvent(ctx, "offer.finalized", acct.ID, map[string]any{
		"offer_id": offerWelcomeLitre, "source": source,
	})

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

// crmEnsureSubscription returns the plan the offer should ride, ADOPTING one
// the household already has instead of minting a second.
//
// THE BUG THIS CLOSES: enrolment used to call crmCreateSubscription
// unconditionally. Eligibility only refuses on PAID order history, and a fresh
// subscriber has none — createSubscription writes no order, and the sweep only
// materialises tomorrow's preview from 13:00 IST. So a household that
// subscribed this morning with an unfunded wallet still read as `eligible`,
// was shown the funnel by the app, and on enrolling ended up with TWO active
// daily plans. Both then locked every morning once funded: two deliveries
// billed for milk they ordered once.
//
// Adopting (rather than refusing enrolment) is deliberate: such a household has
// never PAID, so the campaign is genuinely theirs — they simply must not be
// given a second plan to pay for. Their own choice of milk wins; the free packs
// are the campaign SKU regardless of what they subscribed to.
//
// `minted` reports whether this call created the plan, so a failed enrolment
// retracts only its own scaffolding and never cancels the customer's plan.
func (s *service) crmEnsureSubscription(ctx context.Context, consumerID primitive.ObjectID, productID string, qty int, frequency string) (sub *subscription, minted bool, err error) {
	existing, lerr := s.repo.listSubscriptions(ctx, consumerID)
	if lerr != nil {
		return nil, false, lerr
	}
	for i := range existing {
		// listSubscriptions already excludes cancelled; a paused plan still
		// belongs to the household and resumes on its own terms.
		if existing[i].Status == "active" || existing[i].Status == "paused" {
			s.log.Info("crm: adopting the household's existing plan instead of minting a second",
				"consumer", consumerID.Hex(), "subscription", existing[i].SubscriptionID, "status", existing[i].Status)
			return &existing[i], false, nil
		}
	}
	created, cerr := s.crmCreateSubscription(ctx, consumerID, productID, qty, frequency)
	if cerr != nil {
		return nil, false, cerr
	}
	return created, true, nil
}

// crmCreateSubscription creates the campaign's NORMAL daily plan: the offer
// SKU at the server-authoritative price with its catalogue pack size (every
// morning order, store row and rider row inherits it), quantity honouring
// the app's 1 L/day milk floor (2 × 500 ml). It intentionally reuses the plain
// subscription document — the sweep treats it identically to any other plan.
func (s *service) crmCreateSubscription(ctx context.Context, consumerID primitive.ObjectID, productID string, qty int, frequency string) (*subscription, error) {
	ix, err := s.loadPriceIndex(ctx)
	if err != nil {
		return nil, err
	}
	unit, ok := ix.priceFor(productID, "")
	if !ok {
		return nil, errUnprocessable("SKU_UNAVAILABLE", "the chosen milk is not sellable right now")
	}
	now := time.Now().UTC()
	sub := &subscription{
		MongoID: primitive.NewObjectID(), SubscriptionID: newSubscriptionID(),
		ConsumerID: consumerID, ProductID: productID, Name: ix.nameFor(productID),
		Variant: ix.variantFor(productID), Qty: qty, UnitPrice: round2(unit), Frequency: frequency,
		Status: "active", StartDate: istDay(time.Now().Add(24 * time.Hour)),
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.repo.subscriptions.InsertOne(ctx, sub); err != nil {
		return nil, errInternal("subscription store failed")
	}
	return sub, nil
}

// crmPlanSKUs — the offer terms' plan menu (§6.3: Toned or Full Cream, 500 ml
// or 1 L), with each pack's millilitres so the app's 1 L/day milk floor holds
// per DELIVERY DAY. Config-shaped next to SeedSKU; extend here when the menu
// grows — nothing else changes.
var crmPlanSKUs = map[string]int{
	"gold-500ml":  500,
	"gold-1l":     1000,
	"taaza-500ml": 500,
	"taaza-1l":    1000,
}

// crmValidatePlan resolves the enrolment's subscription plan. Empty input →
// the campaign default. Anything supplied is validated CLOSED: menu SKU,
// sane quantity, a cadence the backend worker actually runs, and at least
// one litre per delivery day (the app-wide milk floor).
func (s *service) crmValidatePlan(in crmEnrolInput, cfg crmOffer) (string, int, string, error) {
	product := strings.TrimSpace(in.PlanProductID)
	freq := strings.TrimSpace(in.PlanFrequency)
	qty := in.PlanQty
	if product == "" && qty == 0 && freq == "" {
		return cfg.SeedSKU, cfg.SubscriptionQty, "daily", nil
	}
	if product == "" {
		product = cfg.SeedSKU
	}
	ml, ok := crmPlanSKUs[product]
	if !ok {
		return "", 0, "", errBadRequest("plan_product_id must be one of the offer's milk options")
	}
	if freq == "" {
		freq = "daily"
	}
	if freq != "daily" && freq != "alternate" {
		return "", 0, "", errBadRequest("plan_frequency must be daily or alternate")
	}
	if qty == 0 {
		qty = 1
		if ml < 1000 {
			qty = 2
		}
	}
	if qty < 1 || qty > 8 {
		return "", 0, "", errBadRequest("plan_qty must be between 1 and 8")
	}
	if qty*ml < 1000 {
		return "", 0, "", errUnprocessable("BELOW_MILK_FLOOR", "a delivery day must total at least 1 litre")
	}
	return product, qty, freq, nil
}

// ── One welcome per household (free_pack_claims, shared with the 2+2 gate) ──

// crmHouseholdClaimed reports whether this phone's welcome entitlement is
// already held by a DIFFERENT consumer (erase-and-resignup, or the 2+2 was
// used on a previous account). ok=false → the read failed; callers FAIL OPEN
// on the read (never block a signup on a transient) — the write-side unique
// index remains the hard guarantee.
func (s *service) crmHouseholdClaimed(ctx context.Context, consumerID primitive.ObjectID, canonicalPhone string) (claimed bool, ok bool) {
	if canonicalPhone == "" {
		return false, true
	}
	var prior struct {
		ConsumerID primitive.ObjectID `bson:"consumer_id"`
	}
	err := s.repo.accounts.Database().Collection("free_pack_claims").
		FindOne(ctx, bson.D{{Key: "phone", Value: canonicalPhone}}).Decode(&prior)
	if err != nil {
		if isNoDocs(err) {
			return false, true
		}
		return false, false
	}
	return prior.ConsumerID != consumerID, true
}

// crmEligibility is the SERVER-TRUTH answer the app's funnel renders from —
// no AsyncStorage gate, no local guess. Read-only, ordered so the FIRST
// failing gate names the state the FE should show:
//
//	eligible          → show the Welcome Litre funnel
//	already_enrolled  → show the offer progress card instead
//	not_eligible      → existing/paying household: no funnel, normal app
//	address_required  → funnel visible, CTA routes to address capture first
//	not_serviceable   → funnel visible, CTA routes to the waitlist
func (s *service) crmEligibility(ctx context.Context, consumerID primitive.ObjectID) (status string, err error) {
	if !crmEnabled() {
		// DISABLED must be indistinguishable from a backend that has no CRM at
		// all: the app treats a 404 as "legacy rules apply" (the 2+2 pitch
		// stays). Answering not_eligible here would hide BOTH offers — no
		// acquisition pitch anywhere. The funnel switchover is therefore
		// exactly one dashboard action: set CRM_ENABLED=true.
		return "", errNotFound("route not found")
	}
	if o, oerr := s.repo.findOffer(ctx, consumerID); oerr != nil {
		return "", oerr
	} else if o != nil {
		return "already_enrolled", nil
	}
	acct, aerr := s.repo.findAccountByID(ctx, consumerID)
	if aerr != nil || acct == nil {
		return "", errUnauthorized("account not found")
	}
	if acct.HasPaidOrder {
		return "not_eligible", nil
	}
	if n, cerr := s.repo.orders.CountDocuments(ctx, bson.D{
		{Key: "user_id", Value: consumerID.Hex()},
		{Key: "total", Value: bson.D{{Key: "$gt", Value: 0}}},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: "cancelled"}}},
	}); cerr != nil {
		return "", errInternal("order history check failed")
	} else if n > 0 {
		return "not_eligible", nil
	}
	if t, terr := s.repo.getOrCreateTrial(ctx, consumerID); terr == nil && (t.DeliveredPaid > 0 || t.DeliveredFree > 0) {
		return "not_eligible", nil
	}
	if claimed, ok := s.crmHouseholdClaimed(ctx, consumerID, acct.Phone); ok && claimed {
		return "not_eligible", nil
	}
	addr, derr := s.subscriptionAddress(ctx, consumerID)
	if derr != nil || addr == nil || addr.Lat == nil || addr.Lng == nil {
		// subscriptionAddress reports "no usable address" as ADDRESS_REQUIRED —
		// for the funnel that IS a state, not a failure.
		if derr == nil || crmErrCode(derr) == "ADDRESS_REQUIRED" {
			return "address_required", nil
		}
		return "", derr
	}
	if sv, verr := s.serviceability(ctx, *addr.Lat, *addr.Lng, addr.Pincode); verr != nil {
		return "", verr
	} else if !sv.Serviceable {
		return "not_serviceable", nil
	}
	return "eligible", nil
}

// crmErrCode extracts the API error code ("" for foreign errors).
func crmErrCode(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// crmRegisterHouseholdClaim writes the phone's claim after the offer doc won
// arbitration. Idempotent; a dup by the SAME consumer is fine, and a dup by a
// different consumer is impossible while accounts are unique per phone.
func (s *service) crmRegisterHouseholdClaim(ctx context.Context, consumerID primitive.ObjectID, canonicalPhone string) {
	if canonicalPhone == "" {
		return
	}
	_, err := s.repo.accounts.Database().Collection("free_pack_claims").InsertOne(ctx, bson.D{
		{Key: "phone", Value: canonicalPhone}, {Key: "consumer_id", Value: consumerID},
		{Key: "claimed_at", Value: time.Now().UTC()},
	})
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		s.log.Warn("crm: household claim write failed (registry catches up on trial advancement)", "err", err)
	}
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
			ID: newItemID(), ProductID: cfg.SeedSKU, Name: name, Variant: ix.variantFor(cfg.SeedSKU),
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
			"LABELLED_PRODUCT": s.crmLabelledProduct(ctx, orderID),
		})
	}
	return nil
}

// crmLabelledSuffix is the C-01 rendering for the parag supply source.
const crmLabelledSuffix = " — delivered by PYAAS"

// crmLabelledProduct renders the C-01 supply-source label from the order line
// (crmLabelledProductOf, crm_lifecycle.go), looked up by order id.
func (s *service) crmLabelledProduct(ctx context.Context, orderID string) string {
	o, err := s.repo.findOrderAnyUser(ctx, orderID)
	if err != nil {
		o = nil
	}
	return crmLabelledProductOf(o)
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
		unlockedAt := time.Now().UTC()
		moved, terr := s.repo.transitionPack(ctx, consumerID, 2, pack2Locked, pack2Pending,
			fmt.Sprintf("settled recharge ₹%.2f >= ₹%.2f", amountRupees, threshold),
			bson.D{{Key: "pack2_unlocked_at", Value: unlockedAt}})
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
		// pending-without-order: a previous attempt died after the CAS, or the
		// attach conditions weren't met yet — try again now.
	default:
		return nil // delivered/expired — nothing to do
	}

	// TERMS §4.4–4.5: the free pack is "delivered free with your NEXT DELIVERY
	// after that recharge" — it RIDES a real morning, it never makes a lone
	// ₹15 drop. The attach helper mints only when tomorrow actually delivers;
	// for a paused/not-due subscription the pending pack waits (14-day cap,
	// enforced by the sweep) and the sweep re-attempts every tick.
	_, aerr := s.crmTryAttachPack2(ctx, consumerID)
	return aerr
}

// crmTryAttachPack2 mints the pack-2 ₹0 order for TOMORROW iff tomorrow is a
// real delivery morning for this household: subscription active, due
// tomorrow, not changed after tomorrow's noon cut-off, tomorrow not skipped,
// and the wallet covers that day (the noon rule would skip an unfunded day,
// and a free pack alone at the door is exactly the lone drop the campaign
// economics forbid). Idempotent: the pack2_order_id
// empty-slot guard makes concurrent attempts converge on one order.
// Returns (attached, error); a nil error with attached=false just means
// "conditions not met yet — the sweep keeps trying".
func (s *service) crmTryAttachPack2(ctx context.Context, consumerID primitive.ObjectID) (bool, error) {
	return s.crmTryAttachPack2At(ctx, consumerID, time.Now())
}

// crmTryAttachPack2At is crmTryAttachPack2 as seen at now.
func (s *service) crmTryAttachPack2At(ctx context.Context, consumerID primitive.ObjectID, now time.Time) (bool, error) {
	o, err := s.repo.findOffer(ctx, consumerID)
	if err != nil {
		return false, err
	}
	if o == nil || o.Pack2State != pack2Pending || o.Pack2OrderID != "" {
		return false, nil
	}
	// 14-day window (terms §4.5): past it the sweep expires the pack; never
	// attach after the cap even if the sweep hasn't swept yet.
	if o.Pack2UnlockedAt != nil && now.Sub(*o.Pack2UnlockedAt) > 14*24*time.Hour {
		return false, nil
	}
	var sub subscription
	if err := s.repo.subscriptions.FindOne(ctx,
		bson.D{{Key: "subscription_id", Value: o.SubscriptionID}}).Decode(&sub); err != nil {
		if isNoDocs(err) {
			return false, nil // subscription cancelled — §7.4: offer ends via the sweep
		}
		return false, errInternal("crm: pack2 subscription lookup failed")
	}
	tomorrow := istDay(now.Add(24 * time.Hour))
	if sub.Status != "active" || !subscriptionDueOn(&sub, tomorrow) {
		return false, nil // paused or off-cadence — wait for a real morning
	}
	// The noon rule: a plan created, resumed or edited after tomorrow's
	// cut-off starts the day after (the sweep never delivers tomorrow for
	// it), and a claimed tomorrow with no live order is a day the member
	// skipped. Either way tomorrow is not a delivery morning.
	if !sub.subChangedBefore(lockMomentFor(tomorrow)) {
		return false, nil
	}
	if sub.claimed(tomorrow) {
		if live, lerr := s.repo.findLiveSubscriptionOrder(ctx, sub.SubscriptionID, tomorrow); lerr != nil || live == nil {
			return false, nil
		}
	}
	dayCost := round2(sub.UnitPrice*float64(sub.Qty)) + subscriptionDeliveryFee
	if wv, werr := s.wallet(ctx, consumerID); werr != nil || wv.Available < dayCost {
		return false, nil // unfunded tomorrow — the noon rule would skip it
	}

	acct, aerr := s.repo.findAccountByID(ctx, consumerID)
	if aerr != nil || acct == nil {
		return false, errInternal("crm: pack2 account lookup failed")
	}
	addr, derr := s.subscriptionAddress(ctx, consumerID)
	if derr != nil || addr == nil {
		return false, errInternal("crm: pack2 address lookup failed")
	}
	p2, perr := s.mintPromoPackOrder(ctx, acct, addr, tomorrow, 2)
	if perr != nil {
		return false, perr // outbox/sweep retries; state=pending resumes here
	}
	res, uerr := s.repo.offers().UpdateOne(ctx,
		bson.D{
			{Key: "consumer_id", Value: consumerID},
			{Key: "pack2_order_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "pack2_order_id", Value: p2.OrderID}, {Key: "updated_at", Value: time.Now().UTC()}}}})
	if uerr != nil || res.ModifiedCount == 0 {
		// Lost to a concurrent attach — retract our duplicate pack.
		s.crmRetractPromoOrder(ctx, p2.OrderID)
		return false, nil
	}
	// W-04 only AFTER the pack is really minted and riding tomorrow's delivery
	// ("scheduled free with tomorrow's delivery" is now literally true).
	s.crmDispatch(ctx, "W-04", consumerID, map[string]string{})
	return true, nil
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

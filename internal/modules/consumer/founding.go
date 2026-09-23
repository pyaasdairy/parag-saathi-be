package consumer

// FOUNDING FAMILY - the membership that replaces PYAAS Plus (pyaas-app-spec.md,
// 21 Sep 2026; lib/foundingFamily.ts is the contract the app reads).
//
// THE MODEL. A home picks one of the PYAAS farms and pays Rs 99 from the
// wallet to hold a seat there (status waiting, a line number on that farm).
// When a farm's claims reach "unlocks at" it flips to unlocked, claims close,
// every waiting member becomes active, and the Rs 99 is month one. From then
// PYAAS milk lines bill at price level 3 for the member, delivery is free,
// and Rs 99 leaves the wallet every month on the same date; a short wallet is
// retried for three days and then the membership stops. Stop is allowed any
// month and perks run to the end of the paid month.
//
// DATA. Spec rule one says prices and statuses come from the ERP through the
// backend. What the ERP integration already provides is used as-is: the
// product master and its level-1 price (catalog_price.go), the level-3 price
// when the ERP carries one (dolibarr_sync.go mirrors multiprices level 3 into
// member_price) and, once the FOUNDING-99 / DELIVERY-FEE services exist in
// the ERP, their prices (foundingERPPrices). What the ERP does not hold today
// lives here: farm records, seats and member records, in
// consumer_founding_farms / consumer_founding_members, with an admin-group
// endpoint to upsert farms and a JSON seed under cmd/seed.
//
// PRICING. Parag (PRG-*) is never discounted. PYAAS milk lines (the seeded
// pyaas-* cards and the ERP's PYS-* additions, identified the way the
// catalog sync maps them) carry a member price: the ERP's level 3 when set,
// else level 1 minus FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE per litre (the spec's
// "level 1 minus Rs 2/L"). catalogPriceIndex.priceForMember applies it in
// createOrder, createSubscription and the subscription worker.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

const (
	collFoundingFarms   = "consumer_founding_farms"
	collFoundingMembers = "consumer_founding_members"
	collFoundingPrices  = "consumer_founding_prices"

	farmFilling  = "filling"
	farmUnlocked = "unlocked"

	memberWaiting = "waiting"
	memberActive  = "active"
	memberStopped = "stopped"

	// foundingLedgerLabel is the remark on every Rs 99 ledger row - the ERP
	// service the money is booked against.
	foundingLedgerLabel = "FOUNDING-99"
)

// ── Documents ───────────────────────────────────────────────────────────────

type foundingFarm struct {
	ID            string     `bson:"_id"`
	Name          string     `bson:"name"`
	Farmer        string     `bson:"farmer"`
	Place         string     `bson:"place,omitempty"`
	Note          string     `bson:"note,omitempty"`
	PhotoURL      string     `bson:"photo_url,omitempty"`
	UnlocksAt     int        `bson:"unlocks_at"`
	Claimed       int        `bson:"claimed"`
	Status        string     `bson:"status"`
	UnlockedPacks string     `bson:"unlocked_packs,omitempty"`
	UnlockedAt    *time.Time `bson:"unlocked_at,omitempty"`
	Sort          int        `bson:"sort,omitempty"`
	UpdatedBy     string     `bson:"updated_by,omitempty"`
	UpdatedAt     time.Time  `bson:"updated_at"`
	CreatedAt     time.Time  `bson:"created_at"`
}

type foundingMember struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"`
	ConsumerID  primitive.ObjectID `bson:"consumer_id"`
	FarmID      string             `bson:"farm_id"`
	Status      string             `bson:"status"`
	LineNumber  int                `bson:"line_number"`
	JoinedAt    time.Time          `bson:"joined_at"`
	ActivatedAt *time.Time         `bson:"activated_at,omitempty"`
	// NextBillDate is the IST day the next Rs 99 leaves the wallet; BillDay
	// is the anchor day-of-month it rolls on (the 31st stays the 31st where
	// the month has one, else the month's last day).
	NextBillDate string `bson:"next_bill_date,omitempty"`
	BillDay      int    `bson:"bill_day,omitempty"`
	LastBillDate string `bson:"last_bill_date,omitempty"`
	BillAttempts int    `bson:"bill_attempts,omitempty"`
	// PerksUntil: after a stop, the last IST day the paid month still covers.
	PerksUntil string     `bson:"perks_until,omitempty"`
	StoppedAt  *time.Time `bson:"stopped_at,omitempty"`
	StopReason string     `bson:"stop_reason,omitempty"`
	Joins      int        `bson:"joins"`
	UpdatedAt  time.Time  `bson:"updated_at"`
}

// foundingERPPrices holds the FOUNDING-99 / DELIVERY-FEE prices the Dolibarr
// sync mirrors once those services exist in the ERP (rupees). Zero means the
// ERP has not provided one and the config fallback applies.
type foundingERPPrices struct {
	ID          string    `bson:"_id"`
	PriceMonth  float64   `bson:"price_month,omitempty"`
	DeliveryFee float64   `bson:"delivery_fee,omitempty"`
	UpdatedAt   time.Time `bson:"updated_at"`
}

const foundingERPPricesID = "founding_erp_prices"

// ── Wire shapes (lib/foundingFamily.ts) ─────────────────────────────────────

type foundingFarmView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Farmer        string  `json:"farmer"`
	Place         *string `json:"place"`
	Note          *string `json:"note"`
	PhotoURL      *string `json:"photo_url"`
	UnlocksAt     int     `json:"unlocks_at"`
	Claimed       int     `json:"claimed"`
	Status        string  `json:"status"`
	UnlockedPacks *string `json:"unlocked_packs"`
}

type foundingMemberView struct {
	Status       string  `json:"status"`
	FarmID       string  `json:"farm_id"`
	LineNumber   *int    `json:"line_number"`
	ReferralCode *string `json:"referral_code"`
	JoinedAt     *string `json:"joined_at"`
	NextBillDate *string `json:"next_bill_date"`
}

type foundingSavingsView struct {
	Level1PerLitre float64 `json:"level1_per_litre"`
	Level3PerLitre float64 `json:"level3_per_litre"`
	DeliveryFee    float64 `json:"delivery_fee"`
}

type foundingView struct {
	PriceMonth float64              `json:"price_month"`
	Member     *foundingMemberView  `json:"member"`
	Farms      []foundingFarmView   `json:"farms"`
	Savings    *foundingSavingsView `json:"savings"`
}

func strOrNil(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func farmView(f foundingFarm) foundingFarmView {
	return foundingFarmView{
		ID: f.ID, Name: f.Name, Farmer: f.Farmer,
		Place: strOrNil(f.Place), Note: strOrNil(f.Note), PhotoURL: strOrNil(f.PhotoURL),
		UnlocksAt: f.UnlocksAt, Claimed: f.Claimed, Status: f.Status,
		UnlockedPacks: strOrNil(f.UnlockedPacks),
	}
}

func memberView(m *foundingMember, referralCode string) *foundingMemberView {
	if m == nil {
		return nil
	}
	v := &foundingMemberView{Status: m.Status, FarmID: m.FarmID, ReferralCode: strOrNil(referralCode)}
	if m.LineNumber > 0 {
		n := m.LineNumber
		v.LineNumber = &n
	}
	if !m.JoinedAt.IsZero() {
		j := m.JoinedAt.UTC().Format(time.RFC3339)
		v.JoinedAt = &j
	}
	if m.Status == memberActive && m.NextBillDate != "" {
		v.NextBillDate = strOrNil(m.NextBillDate)
	}
	return v
}

// ── Month arithmetic ────────────────────────────────────────────────────────

// nextBillDate rolls a YYYY-MM-DD (IST) one month forward on the anchor
// day-of-month: 31 Jan -> 28 Feb -> 31 Mar, never Go's overflow into the
// month after. anchorDay <= 0 means the day of prev.
func nextBillDate(prev string, anchorDay int) string {
	t, ok := parseDay(prev)
	if !ok {
		return prev
	}
	if anchorDay <= 0 {
		anchorDay = t.Day()
	}
	first := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, istZone)
	last := first.AddDate(0, 1, -1).Day()
	day := anchorDay
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, istZone).Format("2006-01-02")
}

// ── PYAAS milk identification (the catalog sync's own mapping) ─────────────

// isPyaasMilkSKU reports whether a catalog id is a PYAAS own-brand milk line:
// the seeded pyaas-* cards, the ERP's PYS-* additions (dol-pys-*), or a row
// whose base_id is a PYS-* ref. Ghee and every PRG-* (Parag) line are not.
func isPyaasMilkSKU(skuID, baseID, category string) bool {
	if category != "" && category != "milk" {
		return false
	}
	id := strings.ToLower(strings.TrimSpace(skuID))
	if strings.HasPrefix(id, "pyaas-") || strings.HasPrefix(id, "dol-pys-") {
		return !strings.Contains(id, "ghee")
	}
	base := strings.ToUpper(strings.TrimSpace(baseID))
	return strings.HasPrefix(base, "PYS-") && !strings.Contains(base, "GHEE")
}

// memberDiscountFor is the per-pack rupee discount at off rupees per litre,
// rounded to the rupee: 1 L -> 2, 500 ml -> 1, 450 ml -> 1, 200 ml -> 0 (the
// spec's trial packs carry no discount).
func memberDiscountFor(litres, offPerLitre float64) float64 {
	if litres <= 0 || offPerLitre <= 0 {
		return 0
	}
	return math.Round(litres * offPerLitre)
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) foundingFarms() *mongo.Collection {
	return r.accounts.Database().Collection(collFoundingFarms)
}

func (r *repository) foundingMembers() *mongo.Collection {
	return r.accounts.Database().Collection(collFoundingMembers)
}

func (r *repository) ensureFoundingIndexes(ctx context.Context) error {
	if _, err := r.foundingMembers().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "consumer_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "farm_id", Value: 1}, {Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "next_bill_date", Value: 1}}},
	}); err != nil {
		return err
	}
	return nil
}

func (r *repository) listFoundingFarms(ctx context.Context) ([]foundingFarm, error) {
	cur, err := r.foundingFarms().Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "name", Value: 1}}).SetLimit(100))
	if err != nil {
		return nil, errInternal("farms lookup failed")
	}
	out := []foundingFarm{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("farms decode failed")
	}
	return out, nil
}

func (r *repository) findFoundingFarm(ctx context.Context, id string) (*foundingFarm, error) {
	var f foundingFarm
	err := r.foundingFarms().FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&f)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("farm lookup failed")
	}
	return &f, nil
}

// upsertFoundingFarm writes the founder-owned fields of a farm. claimed and
// unlocked_at are never written here: seats are only moved by joins and the
// unlock, so a re-seed or an admin edit can never reset a farm's line.
func (r *repository) upsertFoundingFarm(ctx context.Context, f foundingFarm, updatedBy string) (*foundingFarm, error) {
	now := time.Now().UTC()
	set := bson.D{
		{Key: "name", Value: f.Name}, {Key: "farmer", Value: f.Farmer},
		{Key: "place", Value: f.Place}, {Key: "note", Value: f.Note}, {Key: "photo_url", Value: f.PhotoURL},
		{Key: "unlocks_at", Value: f.UnlocksAt}, {Key: "unlocked_packs", Value: f.UnlockedPacks},
		{Key: "sort", Value: f.Sort}, {Key: "updated_by", Value: updatedBy}, {Key: "updated_at", Value: now},
	}
	if f.Status != "" {
		set = append(set, bson.E{Key: "status", Value: f.Status})
	}
	after := options.After
	var out foundingFarm
	err := r.foundingFarms().FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: f.ID}},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$setOnInsert", Value: bson.D{{Key: "claimed", Value: 0}, {Key: "created_at", Value: now}}},
		},
		options.FindOneAndUpdate().SetReturnDocument(after).SetUpsert(true)).Decode(&out)
	if err != nil {
		return nil, errInternal("farm store failed")
	}
	if out.Status == "" {
		_, _ = r.foundingFarms().UpdateByID(ctx, f.ID, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: farmFilling}}}})
		out.Status = farmFilling
	}
	return &out, nil
}

// claimFarmSeat takes the next seat on a FILLING farm with room left and
// returns the farm as it stands after the claim (claimed is the new line
// number). (nil, nil) when the farm is unlocked or full - claims are closed.
func (r *repository) claimFarmSeat(ctx context.Context, farmID string) (*foundingFarm, error) {
	after := options.After
	var f foundingFarm
	err := r.foundingFarms().FindOneAndUpdate(ctx,
		bson.D{
			{Key: "_id", Value: farmID}, {Key: "status", Value: farmFilling},
			{Key: "$expr", Value: bson.D{{Key: "$lt", Value: bson.A{"$claimed", "$unlocks_at"}}}},
		},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "claimed", Value: 1}}}, {Key: "$set", Value: bson.D{{Key: "updated_at", Value: time.Now().UTC()}}}},
		options.FindOneAndUpdate().SetReturnDocument(after)).Decode(&f)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("farm claim failed")
	}
	return &f, nil
}

// releaseFarmSeat gives a seat back (a join whose debit failed, or a waiting
// member who stops). Never below zero, never on an unlocked farm.
func (r *repository) releaseFarmSeat(ctx context.Context, farmID string) {
	_, _ = r.foundingFarms().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: farmID}, {Key: "status", Value: farmFilling}, {Key: "claimed", Value: bson.D{{Key: "$gt", Value: 0}}}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "claimed", Value: -1}}}, {Key: "$set", Value: bson.D{{Key: "updated_at", Value: time.Now().UTC()}}}})
}

// unlockFarm flips filling -> unlocked exactly once and reports whether THIS
// call did it.
func (r *repository) unlockFarm(ctx context.Context, farmID string, at time.Time) (bool, error) {
	res, err := r.foundingFarms().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: farmID}, {Key: "status", Value: farmFilling}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: farmUnlocked}, {Key: "unlocked_at", Value: at}, {Key: "updated_at", Value: at}}}})
	if err != nil {
		return false, errInternal("farm unlock failed")
	}
	return res.ModifiedCount == 1, nil
}

func (r *repository) findFoundingMember(ctx context.Context, consumerID primitive.ObjectID) (*foundingMember, error) {
	var m foundingMember
	err := r.foundingMembers().FindOne(ctx, bson.D{{Key: "consumer_id", Value: consumerID}}).Decode(&m)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("member lookup failed")
	}
	return &m, nil
}

func (r *repository) listFarmMembers(ctx context.Context, farmID, status string) ([]foundingMember, error) {
	cur, err := r.foundingMembers().Find(ctx, bson.D{{Key: "farm_id", Value: farmID}, {Key: "status", Value: status}},
		options.Find().SetSort(bson.D{{Key: "line_number", Value: 1}}).SetLimit(5000))
	if err != nil {
		return nil, errInternal("members lookup failed")
	}
	out := []foundingMember{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("members decode failed")
	}
	return out, nil
}

// listMembersDueForBilling: active members whose next bill day is on or
// before day (YYYY-MM-DD, string order is chronological).
func (r *repository) listMembersDueForBilling(ctx context.Context, day string) ([]foundingMember, error) {
	cur, err := r.foundingMembers().Find(ctx, bson.D{
		{Key: "status", Value: memberActive},
		{Key: "next_bill_date", Value: bson.D{{Key: "$gt", Value: ""}, {Key: "$lte", Value: day}}},
	}, options.Find().SetLimit(5000))
	if err != nil {
		return nil, errInternal("billing scan failed")
	}
	out := []foundingMember{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("billing decode failed")
	}
	return out, nil
}

// updateFoundingMember applies a guarded $set and returns the fresh row;
// (nil, nil) when the guard did not match.
func (r *repository) updateFoundingMember(ctx context.Context, id primitive.ObjectID, set bson.D, unset bson.D, guard bson.D) (*foundingMember, error) {
	filter := append(bson.D{{Key: "_id", Value: id}}, guard...)
	set = append(set, bson.E{Key: "updated_at", Value: time.Now().UTC()})
	upd := bson.D{{Key: "$set", Value: set}}
	if len(unset) > 0 {
		upd = append(upd, bson.E{Key: "$unset", Value: unset})
	}
	after := options.After
	var m foundingMember
	err := r.foundingMembers().FindOneAndUpdate(ctx, filter, upd,
		options.FindOneAndUpdate().SetReturnDocument(after)).Decode(&m)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("member update failed")
	}
	return &m, nil
}

func (r *repository) foundingERPPrices(ctx context.Context) foundingERPPrices {
	var p foundingERPPrices
	_ = r.accounts.Database().Collection(collFoundingPrices).FindOne(ctx, bson.D{{Key: "_id", Value: foundingERPPricesID}}).Decode(&p)
	return p
}

// saveFoundingERPPrice records an ERP service price (FOUNDING-99 ->
// price_month, DELIVERY-FEE -> delivery_fee), called by the Dolibarr sync.
func (r *repository) saveFoundingERPPrice(ctx context.Context, field string, rupees float64) {
	if rupees <= 0 {
		return
	}
	_, _ = r.accounts.Database().Collection(collFoundingPrices).UpdateOne(ctx,
		bson.D{{Key: "_id", Value: foundingERPPricesID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: field, Value: rupees}, {Key: "updated_at", Value: time.Now().UTC()}}}},
		options.Update().SetUpsert(true))
}

// ── Service: prices and membership standing ─────────────────────────────────

// foundingPriceMonth is FOUNDING-99 in rupees: the ERP's price when the sync
// has seen the service, else the configured fallback.
func (s *service) foundingPriceMonth(ctx context.Context) float64 {
	if p := s.repo.foundingERPPrices(ctx); p.PriceMonth > 0 {
		return round2(p.PriceMonth)
	}
	return round2(s.deps.Cfg.FoundingPriceMonth())
}

// foundingDeliveryFee is DELIVERY-FEE in rupees, same precedence.
func (s *service) foundingDeliveryFee(ctx context.Context) float64 {
	if p := s.repo.foundingERPPrices(ctx); p.DeliveryFee > 0 {
		return round2(p.DeliveryFee)
	}
	return round2(s.deps.Cfg.FoundingDeliveryFee())
}

// foundingOpen reports whether the programme answers at all: not closed by
// config and at least one farm on record. Closed -> the app says opening soon.
func (s *service) foundingOpen(ctx context.Context) (bool, []foundingFarm, error) {
	if s.deps.Cfg.FoundingClosed {
		return false, nil, nil
	}
	farms, err := s.repo.listFoundingFarms(ctx)
	if err != nil {
		return false, nil, err
	}
	return len(farms) > 0, farms, nil
}

// foundingStanding is the member row plus whether the perks apply on day:
// active members, and stopped members until the end of their paid month.
func (s *service) foundingStanding(ctx context.Context, consumerID primitive.ObjectID, day string) (*foundingMember, bool) {
	m, err := s.repo.findFoundingMember(ctx, consumerID)
	if err != nil || m == nil {
		return nil, false
	}
	switch m.Status {
	case memberActive:
		return m, true
	case memberStopped:
		return m, m.PerksUntil != "" && day <= m.PerksUntil
	}
	return m, false
}

// foundingActive: does member pricing apply to this consumer today?
func (s *service) foundingActive(ctx context.Context, consumerID primitive.ObjectID) bool {
	_, active := s.foundingStanding(ctx, consumerID, istToday(time.Now()))
	return active
}

// foundingActiveHex is foundingActive for the hex consumer id orders carry.
func (s *service) foundingActiveHex(ctx context.Context, userID string) bool {
	cid, err := primitive.ObjectIDFromHex(userID)
	if err != nil {
		return false
	}
	return s.foundingActive(ctx, cid)
}

// foundingGate applies spec rule 5.1 when FOUNDING_PYAAS_MEMBERS_ONLY is on:
// a PYAAS milk line needs an active member whose farm has unlocked. Off by
// default until the app ships the shop lock states, so nothing existing
// changes. Returns the refusal, or nil.
func (s *service) foundingGate(ctx context.Context, consumerID primitive.ObjectID, pyaasLine bool, active bool) error {
	if !pyaasLine || active || !s.deps.Cfg.FoundingPyaasMembersOnly {
		return nil
	}
	m, _ := s.repo.findFoundingMember(ctx, consumerID)
	if m != nil && m.Status == memberWaiting {
		name := "your farm"
		if f, _ := s.repo.findFoundingFarm(ctx, m.FarmID); f != nil {
			name = f.Name
		}
		return errUnprocessable("FOUNDING_REQUIRED", "Opens when "+name+" unlocks.")
	}
	return errUnprocessable("FOUNDING_REQUIRED", "PYAAS milk is for Founding Family homes. Unlock it with Founding Family.")
}

// ── Service: the view ───────────────────────────────────────────────────────

var errFoundingClosed = &apiError{status: http.StatusNotFound, Code: "NOT_AVAILABLE", Message: "Founding Family opens in the app soon."}

func (s *service) foundingFamilyView(ctx context.Context, consumerID primitive.ObjectID) (*foundingView, error) {
	open, farms, err := s.foundingOpen(ctx)
	if err != nil {
		return nil, err
	}
	if !open {
		return nil, errFoundingClosed
	}
	v := &foundingView{PriceMonth: s.foundingPriceMonth(ctx), Farms: make([]foundingFarmView, 0, len(farms))}
	for _, f := range farms {
		v.Farms = append(v.Farms, farmView(f))
	}
	if m, err := s.repo.findFoundingMember(ctx, consumerID); err == nil && m != nil {
		code, _ := s.repo.mintReferralCode(ctx, consumerID)
		v.Member = memberView(m, code)
	}
	v.Savings = s.foundingSavings(ctx)
	return v, nil
}

// foundingSavings feeds the app's "1 L a day saves Rs X a month" line from
// the catalog's level-1 and level-3 prices of the reference 1 L PYAAS line
// (the configured SKU, else the cheapest 1 L PYAAS milk on the catalog).
// nil when no PYAAS 1 L line is priced yet - the app then shows no line.
func (s *service) foundingSavings(ctx context.Context) *foundingSavingsView {
	ix, err := s.loadPriceIndex(ctx)
	if err != nil {
		return nil
	}
	sku := s.deps.Cfg.FoundingSavingsReferenceSKU()
	l1, ok := ix.priceFor(sku, "")
	if !ok {
		sku, l1, ok = ix.cheapestPyaasLitre()
		if !ok {
			return nil
		}
	}
	l3, _ := ix.memberPriceFor(sku, "")
	return &foundingSavingsView{
		Level1PerLitre: round2(l1), Level3PerLitre: round2(l3), DeliveryFee: s.foundingDeliveryFee(ctx),
	}
}

// ── Service: join / stop / unlock ───────────────────────────────────────────

var errAlreadyMember = errConflict("ALREADY_MEMBER", "You are already in the Founding Family.")
var errFarmUnlocked = errConflict("FARM_UNLOCKED", "This farm has already unlocked, so claims are closed. Pick another farm.")

func walletShortError(short float64) *apiError {
	n := int(math.Ceil(short))
	if n < 1 {
		n = 1
	}
	return &apiError{status: http.StatusUnprocessableEntity, Code: "WALLET_SHORT",
		Message: "Your wallet is short by " + strconv.Itoa(n) + " rupees for the Rs 99 seat. Add money and try again.", Shortfall: float64(n)}
}

// joinFoundingFamily claims a seat on farm for the caller and takes the Rs 99
// from the wallet, exactly once (the ledger ref is the seat itself). Order of
// operations, so a failure at any step leaves nothing dangling:
//
//  1. checks (open, farm, standing, wallet balance);
//  2. the seat (atomic $inc guarded on filling and room left) - the line number;
//  3. the member row (insert, or a stopped member re-joins in place);
//  4. the debit; a short wallet here releases the seat and the row;
//  5. the unlock when the seat was the last one, else the seat notification.
func (s *service) joinFoundingFamily(ctx context.Context, consumerID primitive.ObjectID, farmID string) (*foundingMemberView, error) {
	open, _, err := s.foundingOpen(ctx)
	if err != nil {
		return nil, err
	}
	if !open {
		return nil, errFoundingClosed
	}
	farmID = strings.TrimSpace(farmID)
	if farmID == "" {
		return nil, errBadRequest("farm_id is required")
	}
	farm, err := s.repo.findFoundingFarm(ctx, farmID)
	if err != nil {
		return nil, err
	}
	if farm == nil {
		return nil, errNotFound("Pick a farm from the list.")
	}
	existing, err := s.repo.findFoundingMember(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Status != memberStopped {
		return nil, errAlreadyMember
	}
	if farm.Status != farmFilling || farm.Claimed >= farm.UnlocksAt {
		return nil, errFarmUnlocked
	}
	price := s.foundingPriceMonth(ctx)
	wv, err := s.wallet(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	if wv.Available < price {
		return nil, walletShortError(price - wv.Available)
	}

	now := time.Now().UTC()
	seat, err := s.repo.claimFarmSeat(ctx, farmID)
	if err != nil {
		return nil, err
	}
	if seat == nil {
		return nil, errFarmUnlocked
	}
	line := seat.Claimed

	var m *foundingMember
	inserted := false
	if existing == nil {
		m = &foundingMember{
			ID: primitive.NewObjectID(), ConsumerID: consumerID, FarmID: farmID, Status: memberWaiting,
			LineNumber: line, JoinedAt: now, Joins: 1, UpdatedAt: now,
		}
		if _, ierr := s.repo.foundingMembers().InsertOne(ctx, m); ierr != nil {
			s.repo.releaseFarmSeat(ctx, farmID)
			if mongo.IsDuplicateKeyError(ierr) {
				return nil, errAlreadyMember
			}
			return nil, errInternal("member store failed")
		}
		inserted = true
	} else {
		m, err = s.repo.updateFoundingMember(ctx, existing.ID,
			bson.D{
				{Key: "status", Value: memberWaiting}, {Key: "farm_id", Value: farmID},
				{Key: "line_number", Value: line}, {Key: "joined_at", Value: now}, {Key: "joins", Value: existing.Joins + 1},
			},
			bson.D{
				{Key: "activated_at", Value: ""}, {Key: "next_bill_date", Value: ""}, {Key: "bill_day", Value: ""},
				{Key: "last_bill_date", Value: ""}, {Key: "bill_attempts", Value: ""}, {Key: "perks_until", Value: ""},
				{Key: "stopped_at", Value: ""}, {Key: "stop_reason", Value: ""},
			},
			bson.D{{Key: "status", Value: memberStopped}})
		if err != nil {
			s.repo.releaseFarmSeat(ctx, farmID)
			return nil, err
		}
		if m == nil { // re-joined concurrently
			s.repo.releaseFarmSeat(ctx, farmID)
			return nil, errAlreadyMember
		}
	}

	ref := "founding:join:" + m.ID.Hex() + ":" + strconv.Itoa(m.Joins)
	if _, derr := s.debitAs(ctx, consumerID, price, "founding", ref, foundingLedgerLabel); derr != nil {
		s.repo.releaseFarmSeat(ctx, farmID)
		if inserted {
			_, _ = s.repo.foundingMembers().DeleteOne(ctx, bson.D{{Key: "_id", Value: m.ID}})
		} else {
			_, _ = s.repo.updateFoundingMember(ctx, m.ID,
				bson.D{{Key: "status", Value: memberStopped}, {Key: "farm_id", Value: existing.FarmID},
					{Key: "line_number", Value: existing.LineNumber}, {Key: "joined_at", Value: existing.JoinedAt}, {Key: "joins", Value: existing.Joins}},
				nil, bson.D{{Key: "status", Value: memberWaiting}})
		}
		var ae *apiError
		if errors.As(derr, &ae) && ae.Code == "INSUFFICIENT_FUNDS" {
			wv2, _ := s.wallet(ctx, consumerID)
			return nil, walletShortError(price - wv2.Available)
		}
		return nil, derr
	}

	if seat.Claimed >= seat.UnlocksAt {
		s.unlockFoundingFarm(ctx, seat, now)
	} else {
		s.emitCRMEvent(ctx, "founding.seat_waiting", consumerID, map[string]any{
			"farm_id": seat.ID, "farm": seat.Name, "farmer": seat.Farmer,
			"line": m.LineNumber, "togo": seat.UnlocksAt - seat.Claimed,
		})
	}
	fresh, _ := s.repo.findFoundingMember(ctx, consumerID)
	if fresh != nil {
		m = fresh
	}
	code, _ := s.repo.mintReferralCode(ctx, consumerID)
	return memberView(m, code), nil
}

// unlockFoundingFarm flips the farm (exactly once), activates every waiting
// member with month one starting today, and sends the unlock and you-are-in
// notifications. Safe to call again: a farm already unlocked does nothing.
func (s *service) unlockFoundingFarm(ctx context.Context, farm *foundingFarm, at time.Time) {
	flipped, err := s.repo.unlockFarm(ctx, farm.ID, at)
	if err != nil || !flipped {
		return
	}
	waiting, err := s.repo.listFarmMembers(ctx, farm.ID, memberWaiting)
	if err != nil {
		return
	}
	today := istToday(at)
	bill := nextBillDate(today, 0)
	anchor, _ := parseDay(today)
	for i := range waiting {
		w := &waiting[i]
		upd, err := s.repo.updateFoundingMember(ctx, w.ID,
			bson.D{
				{Key: "status", Value: memberActive}, {Key: "activated_at", Value: at},
				{Key: "next_bill_date", Value: bill}, {Key: "bill_day", Value: anchor.Day()}, {Key: "bill_attempts", Value: 0},
			}, nil, bson.D{{Key: "status", Value: memberWaiting}})
		if err != nil || upd == nil {
			continue
		}
		payload := map[string]any{
			"farm_id": farm.ID, "farm": farm.Name, "farmer": farm.Farmer, "line": w.LineNumber, "togo": 0,
			"unlocked_packs": farm.UnlockedPacks,
		}
		s.emitCRMEvent(ctx, "founding.farm_unlocked", w.ConsumerID, payload)
		s.emitCRMEvent(ctx, "founding.member_active", w.ConsumerID, payload)
	}
	s.log.InfoContext(ctx, "founding family: farm unlocked", "farm", farm.ID, "members", len(waiting))
}

// stopFoundingFamily ends the membership. An active member keeps the perks to
// the end of the paid month (perks_until = the day before the next bill); a
// waiting member's seat goes back to the farm.
func (s *service) stopFoundingFamily(ctx context.Context, consumerID primitive.ObjectID) (*foundingMemberView, error) {
	m, err := s.repo.findFoundingMember(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errNotFound("You are not in the Founding Family yet.")
	}
	code, _ := s.repo.mintReferralCode(ctx, consumerID)
	if m.Status == memberStopped {
		return memberView(m, code), nil // idempotent
	}
	now := time.Now().UTC()
	set := bson.D{{Key: "status", Value: memberStopped}, {Key: "stopped_at", Value: now}, {Key: "stop_reason", Value: "member"}}
	if m.Status == memberActive && m.NextBillDate != "" {
		set = append(set, bson.E{Key: "perks_until", Value: addDaysIST(m.NextBillDate, -1)})
	}
	upd, err := s.repo.updateFoundingMember(ctx, m.ID, set, nil, bson.D{{Key: "status", Value: m.Status}})
	if err != nil {
		return nil, err
	}
	if upd == nil {
		fresh, _ := s.repo.findFoundingMember(ctx, consumerID)
		return memberView(fresh, code), nil
	}
	if m.Status == memberWaiting {
		s.repo.releaseFarmSeat(ctx, m.FarmID)
	}
	return memberView(upd, code), nil
}

// ── Service: monthly billing ────────────────────────────────────────────────

// billFoundingMembers charges every active member whose bill day has come.
// Each (member, bill day) is one ledger ref, so a repeated tick or a second
// replica can never charge a month twice. A short wallet is retried on the
// following ticks for FoundingBillRetries days, then the membership stops
// with the perks ending the day before the missed bill.
func (s *service) billFoundingMembers(ctx context.Context, now time.Time) (billed, stopped int) {
	today := istToday(now)
	due, err := s.repo.listMembersDueForBilling(ctx, today)
	if err != nil {
		return 0, 0
	}
	price := s.foundingPriceMonth(ctx)
	for i := range due {
		m := &due[i]
		ref := "founding:bill:" + m.ID.Hex() + ":" + m.NextBillDate
		if _, derr := s.debitAs(ctx, m.ConsumerID, price, "founding", ref, foundingLedgerLabel); derr != nil {
			var ae *apiError
			if !errors.As(derr, &ae) || ae.Code != "INSUFFICIENT_FUNDS" {
				continue // transient: try again next tick
			}
			attempts := m.BillAttempts + 1
			if attempts >= s.deps.Cfg.FoundingBillRetries() {
				if upd, _ := s.repo.updateFoundingMember(ctx, m.ID,
					bson.D{{Key: "status", Value: memberStopped}, {Key: "stopped_at", Value: now.UTC()},
						{Key: "stop_reason", Value: "wallet_short"}, {Key: "perks_until", Value: addDaysIST(m.NextBillDate, -1)},
						{Key: "bill_attempts", Value: attempts}},
					nil, bson.D{{Key: "status", Value: memberActive}}); upd != nil {
					stopped++
				}
				continue
			}
			_, _ = s.repo.updateFoundingMember(ctx, m.ID, bson.D{{Key: "bill_attempts", Value: attempts}}, nil,
				bson.D{{Key: "status", Value: memberActive}, {Key: "next_bill_date", Value: m.NextBillDate}})
			continue
		}
		next := nextBillDate(m.NextBillDate, m.BillDay)
		if upd, _ := s.repo.updateFoundingMember(ctx, m.ID,
			bson.D{{Key: "last_bill_date", Value: m.NextBillDate}, {Key: "next_bill_date", Value: next}, {Key: "bill_attempts", Value: 0}},
			nil, bson.D{{Key: "status", Value: memberActive}, {Key: "next_bill_date", Value: m.NextBillDate}}); upd != nil {
			billed++
		}
	}
	if billed > 0 || stopped > 0 {
		s.log.InfoContext(ctx, "founding family billing", "day", today, "billed", billed, "stopped", stopped)
	}
	return billed, stopped
}

// foundingBillingWorker ticks hourly; the ledger refs make every tick after
// the first on a bill day a no-op.
func (s *service) foundingBillingWorker(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			s.billFoundingMembers(runCtx, now)
			cancel()
		}
	}
}

// ── Admin: farms ────────────────────────────────────────────────────────────

type foundingFarmInput struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Farmer        string `json:"farmer"`
	Place         string `json:"place"`
	Note          string `json:"note"`
	PhotoURL      string `json:"photo_url"`
	UnlocksAt     int    `json:"unlocks_at"`
	UnlockedPacks string `json:"unlocked_packs"`
	Status        string `json:"status"`
	Sort          int    `json:"sort"`
}

func farmSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// upsertFoundingFarms writes the farms a founder (or the seed) supplies and
// returns the full list. A farm set to unlocked by hand activates its
// waiting members the same way a full farm does.
func (s *service) upsertFoundingFarms(ctx context.Context, in []foundingFarmInput, updatedBy string) ([]foundingFarm, error) {
	for i, f := range in {
		id := farmSlug(f.ID)
		if id == "" {
			id = farmSlug(f.Name)
		}
		if id == "" || strings.TrimSpace(f.Name) == "" || strings.TrimSpace(f.Farmer) == "" {
			return nil, errBadRequest("every farm needs an id (or name), a name and a farmer")
		}
		if f.UnlocksAt <= 0 {
			return nil, errBadRequest("unlocks_at must be a positive number of homes")
		}
		status := strings.ToLower(strings.TrimSpace(f.Status))
		if status != "" && status != farmFilling && status != farmUnlocked {
			return nil, errBadRequest("status must be filling or unlocked")
		}
		sortKey := f.Sort
		if sortKey == 0 {
			sortKey = i + 1
		}
		doc := foundingFarm{
			ID: id, Name: strings.TrimSpace(f.Name), Farmer: strings.TrimSpace(f.Farmer),
			Place: strings.TrimSpace(f.Place), Note: strings.TrimSpace(f.Note), PhotoURL: strings.TrimSpace(f.PhotoURL),
			UnlocksAt: f.UnlocksAt, UnlockedPacks: strings.TrimSpace(f.UnlockedPacks), Sort: sortKey,
		}
		before, _ := s.repo.findFoundingFarm(ctx, id)
		if status == farmUnlocked && (before == nil || before.Status != farmUnlocked) {
			doc.Status = farmFilling // unlock through the one path that activates members
		} else {
			doc.Status = status
		}
		stored, err := s.repo.upsertFoundingFarm(ctx, doc, updatedBy)
		if err != nil {
			return nil, err
		}
		if status == farmUnlocked && stored.Status == farmFilling {
			s.unlockFoundingFarm(ctx, stored, time.Now().UTC())
		} else if stored.Status == farmFilling && stored.Claimed >= stored.UnlocksAt {
			s.unlockFoundingFarm(ctx, stored, time.Now().UTC()) // a lowered threshold already met
		}
	}
	return s.repo.listFoundingFarms(ctx)
}

// seedFoundingFarms is the idempotent seed path (cmd/seed): insert farms
// that do not exist yet, never touch one that does.
func (r *repository) seedFoundingFarms(ctx context.Context, in []foundingFarmInput) (int, error) {
	added := 0
	for i, f := range in {
		id := farmSlug(f.ID)
		if id == "" {
			id = farmSlug(f.Name)
		}
		if id == "" || f.Name == "" {
			continue
		}
		status := f.Status
		if status == "" {
			status = farmFilling
		}
		now := time.Now().UTC()
		doc := foundingFarm{
			ID: id, Name: f.Name, Farmer: f.Farmer, Place: f.Place, Note: f.Note, PhotoURL: f.PhotoURL,
			UnlocksAt: f.UnlocksAt, Claimed: 0, Status: status, UnlockedPacks: f.UnlockedPacks,
			Sort: i + 1, UpdatedBy: "seed", UpdatedAt: now, CreatedAt: now,
		}
		res, err := r.foundingFarms().UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
			bson.D{{Key: "$setOnInsert", Value: doc}}, options.Update().SetUpsert(true))
		if err != nil {
			return added, err
		}
		if res.UpsertedCount > 0 {
			added++
		}
	}
	return added, nil
}

// ── Handlers (consumer) ─────────────────────────────────────────────────────

func (h *handler) foundingView(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	v, err := h.svc.foundingFamilyView(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handler) foundingJoin(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		FarmID string `json:"farm_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	m, err := h.svc.joinFoundingFamily(r.Context(), id, body.FarmID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"member": m})
}

func (h *handler) foundingStop(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	m, err := h.svc.stopFoundingFamily(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"member": m})
}

// ── Handlers (admin group, operator envelope) ───────────────────────────────

func adminFarmRows(farms []foundingFarm) []map[string]any {
	rows := make([]map[string]any, 0, len(farms))
	for _, f := range farms {
		row := map[string]any{
			"id": f.ID, "name": f.Name, "farmer": f.Farmer, "place": f.Place, "note": f.Note, "photoUrl": f.PhotoURL,
			"unlocksAt": f.UnlocksAt, "claimed": f.Claimed, "status": f.Status, "unlockedPacks": f.UnlockedPacks,
			"sort": f.Sort, "updatedAt": f.UpdatedAt,
		}
		if f.UnlockedAt != nil {
			row["unlockedAt"] = f.UnlockedAt
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return farms[i].Sort < farms[j].Sort })
	return rows
}

func (h *handler) adminFoundingFarms(w http.ResponseWriter, r *http.Request) {
	farms, err := h.svc.repo.listFoundingFarms(r.Context())
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, adminFarmRows(farms))
}

func (h *handler) adminPutFoundingFarms(w http.ResponseWriter, r *http.Request) {
	actor, _ := operatorActor(r)
	var body struct {
		Farms []foundingFarmInput `json:"farms"`
	}
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	if len(body.Farms) == 0 {
		httpx.Error(w, r, toHTTPErr(errBadRequest("farms is required")))
		return
	}
	farms, err := h.svc.upsertFoundingFarms(r.Context(), body.Farms, actor.PartyID)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, adminFarmRows(farms))
}

// SeedFoundingFarms loads the farm records from a cmd/seed JSON document
// ({"farms": [...]}) into the database, inserting only farms that do not
// exist yet: a re-run never resets a farm's seats, status or edits. Returns
// how many farms were added.
func SeedFoundingFarms(ctx context.Context, db *mongo.Database, raw []byte) (int, error) {
	var doc struct {
		Farms []foundingFarmInput `json:"farms"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, err
	}
	return newRepository(db).seedFoundingFarms(ctx, doc.Farms)
}

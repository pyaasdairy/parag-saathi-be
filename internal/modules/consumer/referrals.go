package consumer

// REFERRALS - the server half of the app's Refer screen (lib/referrals.ts).
//
// The app derived a member's code ON THE PHONE for months, so codes are
// already out in the world. The server therefore mints the very same code
// (referralCodeFor mirrors codeFromUid byte for byte) into the account's
// referral_code field, lazily, the first time anything needs it. A code that
// no account has stored yet still resolves: applyReferral falls back to
// deriving the code for every account that has none and stores the match.
//
// Contract the app reads:
//
//	GET  /consumer/referrals/code   -> {code}
//	GET  /consumer/referrals        -> [{id, name, status, reward_amount, created_at}]
//	POST /consumer/referrals/apply  {code}
//
// status is "pending" until the referee's first delivered order that they paid
// for (spec 5.8: a referral counts only when the friend pays; a Rs 0 promo
// pack or an order paid from promo money does not), then
// "credited" (the app's ReferralStatus union: 'pending' | 'credited'; it counts
// and sums "credited" rows). The reward is REFERRAL_REWARD_PAISE (default the
// Refer screen's Rs 100) credited as a promo (REWARDS) wallet credit to BOTH
// sides, exactly once per referral, from the delivered sync.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collReferrals = "consumer_referrals"

const (
	referralPending  = "pending"
	referralCredited = "credited"
)

// referralCodeFor is the app's codeFromUid: h = h*31 + charCode over the
// consumer id, unsigned 32-bit, then ("PG" + base36(h).toUpperCase() +
// "XXXX").slice(0, 6). Consumer ids are hex strings, so bytes are char codes.
func referralCodeFor(uid string) string {
	var h uint32
	for i := 0; i < len(uid); i++ {
		h = h*31 + uint32(uid[i])
	}
	body := strings.ToUpper(strconv.FormatUint(uint64(h), 36))
	code := "PG" + body + "XXXX"
	return code[:6]
}

func normalizeReferralCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

type referral struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"`
	ReferrerID    primitive.ObjectID `bson:"referrer_id"`
	RefereeID     primitive.ObjectID `bson:"referee_id"`
	Code          string             `bson:"code"`
	Status        string             `bson:"status"`
	RewardPaise   int64              `bson:"reward_paise"`
	CreatedAt     time.Time          `bson:"created_at"`
	RewardedAt    *time.Time         `bson:"rewarded_at,omitempty"`
	RewardOrderID string             `bson:"reward_order_id,omitempty"`
}

// referralView is one row of GET /referrals, exactly as lib/referrals.ts
// types it.
type referralView struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	RewardAmount float64 `json:"reward_amount"`
	CreatedAt    string  `json:"created_at"`
}

// ── Repo ────────────────────────────────────────────────────────────────────

func (r *repository) referrals() *mongo.Collection {
	return r.accounts.Database().Collection(collReferrals)
}

func (r *repository) ensureReferralIndexes(ctx context.Context) error {
	if _, err := r.referrals().Indexes().CreateMany(ctx, []mongo.IndexModel{
		// One referrer per referee: the link is made once.
		{Keys: bson.D{{Key: "referee_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "referrer_id", Value: 1}, {Key: "created_at", Value: -1}}},
	}); err != nil {
		return err
	}
	// Codes can collide (four base-36 characters), so the lookup index is not
	// unique; applyReferral picks the oldest account deterministically.
	_, err := r.accounts.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "referral_code", Value: 1}, {Key: "created_at", Value: 1}},
	})
	return err
}

// mintReferralCode returns the account's code, deriving and storing it on
// first use. A code already on the row (carried over, or minted earlier)
// always wins so a shared code never changes under a member.
func (r *repository) mintReferralCode(ctx context.Context, consumerID primitive.ObjectID) (string, error) {
	acct, err := r.findAccountByID(ctx, consumerID)
	if err != nil {
		return "", err
	}
	if acct.ReferralCode != nil && strings.TrimSpace(*acct.ReferralCode) != "" {
		return normalizeReferralCode(*acct.ReferralCode), nil
	}
	code := referralCodeFor(consumerID.Hex())
	if _, err := r.accounts.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: consumerID}, {Key: "$or", Value: bson.A{
			bson.D{{Key: "referral_code", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "referral_code", Value: nil}},
			bson.D{{Key: "referral_code", Value: ""}},
		}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "referral_code", Value: code}, {Key: "updated_at", Value: time.Now().UTC()}}}}); err != nil {
		return "", errInternal("referral code store failed")
	}
	return code, nil
}

// oldestAccountFirst orders code owners oldest first. created_at alone ties
// for accounts created in the same millisecond, and same-second accounts are
// exactly the ones whose derived codes collide; the ObjectID breaks the tie
// (its counter orders ids minted within one second), so "the oldest account
// wins" never depends on which row Mongo happens to return first.
var oldestAccountFirst = bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}

// referralScanCap bounds the derivation fallback: the pilot book is a few
// thousand accounts, and one pass per unknown code is cheap at that size.
const referralScanCap = 50000

// findAccountByReferralCode resolves a code to its owner: a stored code
// first (oldest account wins on a collision), else the derivation over the
// accounts that hold no code yet, storing the match so the next lookup is
// indexed. (nil, nil) when nothing matches.
func (r *repository) findAccountByReferralCode(ctx context.Context, code string) (*account, error) {
	var a account
	err := r.accounts.FindOne(ctx, bson.D{{Key: "referral_code", Value: code}},
		options.FindOne().SetSort(oldestAccountFirst)).Decode(&a)
	if err == nil {
		return &a, nil
	}
	if !isNoDocs(err) {
		return nil, errInternal("referral lookup failed")
	}
	cur, err := r.accounts.Find(ctx,
		bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "referral_code", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "referral_code", Value: nil}},
			bson.D{{Key: "referral_code", Value: ""}},
		}}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}).
			SetSort(oldestAccountFirst).SetLimit(referralScanCap))
	if err != nil {
		return nil, errInternal("referral scan failed")
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var row struct {
			ID primitive.ObjectID `bson:"_id"`
		}
		if cur.Decode(&row) != nil {
			continue
		}
		if referralCodeFor(row.ID.Hex()) != code {
			continue
		}
		if _, err := r.mintReferralCode(ctx, row.ID); err != nil {
			return nil, err
		}
		return r.findAccountByID(ctx, row.ID)
	}
	return nil, nil
}

func (r *repository) findReferralByReferee(ctx context.Context, refereeID primitive.ObjectID) (*referral, error) {
	var ref referral
	err := r.referrals().FindOne(ctx, bson.D{{Key: "referee_id", Value: refereeID}}).Decode(&ref)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("referral lookup failed")
	}
	return &ref, nil
}

// insertReferral links the referee once. dup=true means a link already
// existed (the unique referee index refused a second one).
func (r *repository) insertReferral(ctx context.Context, ref *referral) (dup bool, err error) {
	if _, err := r.referrals().InsertOne(ctx, ref); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return true, nil
		}
		return false, errInternal("referral store failed")
	}
	return false, nil
}

func (r *repository) listReferralsByReferrer(ctx context.Context, referrerID primitive.ObjectID) ([]referral, error) {
	cur, err := r.referrals().Find(ctx, bson.D{{Key: "referrer_id", Value: referrerID}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500))
	if err != nil {
		return nil, errInternal("referrals lookup failed")
	}
	out := []referral{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("referrals decode failed")
	}
	return out, nil
}

// markReferralCredited flips pending -> credited exactly once and reports
// whether THIS call did the flip.
func (r *repository) markReferralCredited(ctx context.Context, id primitive.ObjectID, orderID string, at time.Time) (bool, error) {
	res, err := r.referrals().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: referralPending}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: referralCredited},
			{Key: "rewarded_at", Value: at},
			{Key: "reward_order_id", Value: orderID},
		}}})
	if err != nil {
		return false, errInternal("referral update failed")
	}
	return res.ModifiedCount == 1, nil
}

// ── Service ─────────────────────────────────────────────────────────────────

func (s *service) referralCode(ctx context.Context, consumerID primitive.ObjectID) (string, error) {
	return s.repo.mintReferralCode(ctx, consumerID)
}

// referralDisplayName is the joined family's name on the referrer's ledger:
// the profile name, else the phone with its middle hidden.
func referralDisplayName(a *account) string {
	if a == nil {
		return "A family"
	}
	if a.FullName != nil && strings.TrimSpace(*a.FullName) != "" {
		return strings.TrimSpace(*a.FullName)
	}
	digits := normalizePhone(a.Phone)
	if len(digits) >= 4 {
		return "Family ending " + digits[len(digits)-4:]
	}
	return "A family"
}

func (s *service) listReferrals(ctx context.Context, consumerID primitive.ObjectID) ([]referralView, error) {
	rows, err := s.repo.listReferralsByReferrer(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	out := make([]referralView, 0, len(rows))
	for _, ref := range rows {
		acct, _ := s.repo.findAccountByID(ctx, ref.RefereeID)
		out = append(out, referralView{
			ID:           ref.ID.Hex(),
			Name:         referralDisplayName(acct),
			Status:       ref.Status,
			RewardAmount: round2(float64(ref.RewardPaise) / 100),
			CreatedAt:    ref.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// applyReferralResult is POST /referrals/apply's body: the link as stored.
type applyReferralResult struct {
	ID           string  `json:"id"`
	Code         string  `json:"code"`
	Status       string  `json:"status"`
	RewardAmount float64 `json:"reward_amount"`
	CreatedAt    string  `json:"created_at"`
}

func referralResult(ref *referral) applyReferralResult {
	return applyReferralResult{
		ID: ref.ID.Hex(), Code: ref.Code, Status: ref.Status,
		RewardAmount: round2(float64(ref.RewardPaise) / 100),
		CreatedAt:    ref.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// applyReferral links the caller (the referee) to the code's owner once.
// Idempotent on the same code; a different code after a link is refused; a
// member cannot use their own code; an unknown code is a 404.
func (s *service) applyReferral(ctx context.Context, refereeID primitive.ObjectID, rawCode string) (*applyReferralResult, error) {
	code := normalizeReferralCode(rawCode)
	if code == "" {
		return nil, errBadRequest("a referral code is required")
	}
	// The referee-unique index is what stops two concurrent applies making
	// two links (two rewards); without it, refuse with a 503 the app's
	// outbox replays later rather than trust the pre-check below.
	if !s.referralIdx.ready(ctx) {
		return nil, errReferralUnavailable
	}
	referrer, err := s.repo.findAccountByReferralCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if referrer == nil {
		return nil, &apiError{status: http.StatusNotFound, Code: "REFERRAL_CODE_NOT_FOUND", Message: "We could not find a family with the code " + code + ". Check it with your friend."}
	}
	if referrer.ID == refereeID {
		return nil, errUnprocessable("SELF_REFERRAL", "That is your own code. Share it with a friend instead.")
	}
	if existing, err := s.repo.findReferralByReferee(ctx, refereeID); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.ReferrerID == referrer.ID {
			out := referralResult(existing)
			return &out, nil
		}
		return nil, errConflict("ALREADY_REFERRED", "A friend's code is already on this account.")
	}
	ref := &referral{
		ID: primitive.NewObjectID(), ReferrerID: referrer.ID, RefereeID: refereeID, Code: code,
		Status: referralPending, RewardPaise: int64(s.deps.Cfg.ReferralReward() * 100), CreatedAt: time.Now().UTC(),
	}
	dup, err := s.repo.insertReferral(ctx, ref)
	if err != nil {
		return nil, err
	}
	if dup { // raced with another apply for the same referee
		existing, err := s.repo.findReferralByReferee(ctx, refereeID)
		if err != nil {
			return nil, err
		}
		if existing == nil || existing.ReferrerID != referrer.ID {
			return nil, errConflict("ALREADY_REFERRED", "A friend's code is already on this account.")
		}
		out := referralResult(existing)
		return &out, nil
	}
	s.emitCRMEvent(ctx, "referral.applied", referrer.ID, map[string]any{
		"referral_id": ref.ID.Hex(), "referee_id": refereeID.Hex(), "code": code,
		"reward_amount": round2(float64(ref.RewardPaise) / 100),
	})
	out := referralResult(ref)
	return &out, nil
}

// rewardReferralOnDelivery runs from the delivered sync: the referee's first
// delivered order THEY PAID FOR after the link credits BOTH wallets and flips
// the referral to credited. Exactly once per referral: each credit is gated
// by its own ledger ref, and the status flip is guarded on pending, so a
// repeated sync (or two racing ones) can neither pay twice nor emit twice.
// Best-effort by contract: nothing here may fail a delivery.
func (s *service) rewardReferralOnDelivery(ctx context.Context, o *order) {
	if o == nil {
		return
	}
	refereeID, err := primitive.ObjectIDFromHex(o.UserID)
	if err != nil {
		return
	}
	ref, err := s.repo.findReferralByReferee(ctx, refereeID)
	if err != nil || ref == nil || ref.Status != referralPending {
		return
	}
	// Spec 5.8: a referral counts only when the friend pays. A delivery that
	// took none of the referee's own money leaves it pending for the next.
	if !s.referralPaidDelivery(ctx, refereeID, o) {
		return
	}
	amount := round2(float64(ref.RewardPaise) / 100)
	if amount <= 0 {
		return
	}
	key := "referral:" + ref.ID.Hex()
	if _, err := s.creditRewards(ctx, ref.ReferrerID, amount, key+":referrer", "Referral reward: a family you invited took their first delivery"); err != nil {
		s.log.Warn("referral: referrer credit failed", "referral", ref.ID.Hex(), "err", err)
		return
	}
	if _, err := s.creditRewards(ctx, ref.RefereeID, amount, key+":referee", "Referral reward: welcome to PYAAS"); err != nil {
		s.log.Warn("referral: referee credit failed", "referral", ref.ID.Hex(), "err", err)
		return
	}
	now := time.Now().UTC()
	flipped, err := s.repo.markReferralCredited(ctx, ref.ID, o.OrderID, now)
	if err != nil || !flipped {
		return
	}
	payload := map[string]any{
		"referral_id": ref.ID.Hex(), "order_id": o.OrderID, "reward_amount": amount,
		"referrer_id": ref.ReferrerID.Hex(), "referee_id": ref.RefereeID.Hex(),
	}
	s.emitCRMEvent(ctx, "referral.rewarded", ref.ReferrerID, payload)
	s.emitCRMEvent(ctx, "referral.rewarded", ref.RefereeID, payload)
}

// referralPaidDelivery reports whether the referee paid for this delivered
// order: a wallet settle that debited money not covered in full by promo
// (REWARDS) money, or a cash-on-delivery order with a positive total. A Rs 0
// Welcome Litre pack, a free trial day (its settle row is Rs 0) and an order
// paid entirely from REWARDS never count; otherwise every throwaway account
// that applied a code turned free milk into Rs 100 on each side.
func (s *service) referralPaidDelivery(ctx context.Context, refereeID primitive.ObjectID, o *order) bool {
	if o.Total <= 0 || o.OfferPack > 0 {
		return false
	}
	if o.PaymentMethod != "wallet" && o.PaymentMethod != "prepaid" {
		return true // cash on delivery: the rider collected the total at the door
	}
	var row walletTxn
	if err := s.repo.walletTxns.FindOne(ctx, bson.D{
		{Key: "consumer_id", Value: refereeID},
		{Key: "ref_id", Value: "delivery:" + o.OrderID},
		{Key: "type", Value: "DEBIT"},
	}).Decode(&row); err != nil {
		return false
	}
	return row.Amount > 0 && row.Bucket != "REWARDS"
}

// creditRewards is the server-authorised promo credit (REWARDS bucket) behind
// programme payouts. Same exactly-once ledger gate as promoCredit, without
// its dev-only guard: the caller is the backend itself, bounded to one
// configured amount per ref.
func (s *service) creditRewards(ctx context.Context, consumerID primitive.ObjectID, amount float64, ref, remark string) (walletView, error) {
	amount = round2(amount)
	if amount <= 0 || ref == "" {
		return walletView{}, errBadRequest("invalid reward")
	}
	row := walletTxn{
		ID: primitive.NewObjectID(), ConsumerID: consumerID,
		Type: "BONUS", Bucket: "REWARDS", Amount: amount, RefType: "reward", RefID: ref,
		Status: "SUCCESS", Remark: remark, CreatedAt: time.Now().UTC(),
	}
	dup, err := s.repo.insertWalletTxnGate(ctx, row)
	if err != nil {
		return walletView{}, err
	}
	if dup {
		wl, gerr := s.getOrCreateWallet(ctx, consumerID)
		if gerr != nil {
			return walletView{}, gerr
		}
		return walletToView(wl), nil
	}
	updated, err := s.repo.incWallet(ctx, consumerID, 0, amount, 0, 1)
	if err != nil {
		return walletView{}, err
	}
	s.repo.updateWalletTxnBalances(ctx, row.ID, updated.ID, updated.Seq, updated.CashBalance, updated.RewardsBalance)
	return walletToView(updated), nil
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *handler) referralCode(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	code, err := h.svc.referralCode(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

func (h *handler) listReferrals(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	rows, err := h.svc.listReferrals(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *handler) applyReferral(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	out, err := h.svc.applyReferral(r.Context(), id, body.Code)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

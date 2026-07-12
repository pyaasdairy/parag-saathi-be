package consumer

import (
	"context"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
)

const (
	otpLength        = 6
	refreshHashLabel = "consumer_refresh"
	refreshBytes     = 32
	maxOTPAttempts   = 5 // wrong-code attempts before a challenge is burned
)

var phoneRe = regexp.MustCompile(`^[6-9]\d{9}$`)

type service struct {
	deps *deps.Deps
	repo *repository
	log  *slog.Logger
	// consumerKey is DERIVED from the shared JWT secret via HMAC domain
	// separation, so consumer tokens are cryptographically DISJOINT from
	// operator tokens: an operator token fails signature validation on a
	// consumer route and vice-versa, even though both live in one backend.
	consumerKey []byte
	// Razorpay top-up seam — key id is public, secret is server-only (env). See
	// razorpay.go. Empty secret → offline dev seam (gated by OTP dev mode).
	rzpKeyID     string
	rzpKeySecret string
}

func newService(d *deps.Deps, repo *repository, log *slog.Logger) *service {
	return &service{
		deps:         d,
		repo:         repo,
		log:          log,
		consumerKey:  []byte(auth.HMACHash(d.Cfg.JWTSecret, "consumer-jwt-v1")),
		rzpKeyID:     os.Getenv("RAZORPAY_KEY_ID"),
		rzpKeySecret: os.Getenv("RAZORPAY_KEY_SECRET"),
	}
}

func normalizePhone(p string) string {
	digits := regexp.MustCompile(`\D`).ReplaceAllString(p, "")
	if len(digits) > 10 {
		digits = digits[len(digits)-10:]
	}
	return digits
}

// ── Auth ────────────────────────────────────────────────────────────────────

// requestOTP mints and (dev) echoes an OTP. Consumers self-register, so there
// is NO registration gate here (unlike the operator side): any valid mobile
// may receive a code and become a shopper on verify.
func (s *service) requestOTP(ctx context.Context, phone string) (string, time.Time, error) {
	phone = normalizePhone(phone)
	if !phoneRe.MatchString(phone) {
		return "", time.Time{}, errBadRequest("phone must be a 10-digit Indian mobile number")
	}
	now := time.Now().UTC()
	code, err := auth.GenerateNumericOTP(otpLength)
	if err != nil {
		return "", time.Time{}, errInternal("otp generation failed")
	}
	expires := now.Add(s.deps.Cfg.OTPTTL)
	ch := otpChallenge{
		ID:        primitive.NewObjectID().Hex(),
		Phone:     phone,
		CodeHash:  auth.HMACHash(s.deps.Cfg.OTPHashSecret, "consumer", phone, code),
		ExpiresAt: expires,
		CreatedAt: now,
	}
	if err := s.repo.insertOTP(ctx, ch); err != nil {
		return "", time.Time{}, err
	}
	devOTP := ""
	if s.deps.Cfg.OTPDevMode {
		devOTP = code
	}
	s.log.InfoContext(ctx, "consumer otp requested", slog.String("phone", phone))
	return devOTP, expires, nil
}

// verifyOTP checks the code, FINDS-OR-CREATES the shopper account, seeds an
// empty dual-bucket wallet on first sight, and issues the JWT pair.
func (s *service) verifyOTP(ctx context.Context, phone, code string) (*tokenPair, error) {
	phone = normalizePhone(phone)
	if !phoneRe.MatchString(phone) {
		return nil, errBadRequest("phone must be a 10-digit Indian mobile number")
	}
	ch, err := s.repo.latestOTP(ctx, phone)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if ch == nil || now.After(ch.ExpiresAt) {
		return nil, errUnauthorized("no active code for this number — request a new one")
	}
	// Brute-force guard: a wrong code burns an attempt; after maxOTPAttempts the
	// challenge is invalidated so the 6-digit space can't be walked.
	if ch.CodeHash != auth.HMACHash(s.deps.Cfg.OTPHashSecret, "consumer", phone, code) {
		if s.repo.bumpOTPAttempt(ctx, ch.ID) >= maxOTPAttempts {
			s.repo.deleteOTP(ctx, phone)
			return nil, errUnauthorized("too many incorrect attempts — request a new code")
		}
		return nil, errUnauthorized("incorrect code")
	}
	s.repo.deleteOTP(ctx, phone) // single-use: burn on success

	// The account is keyed by the CANONICAL phone (+91…) — store and look up by
	// the same form so a re-verify of an existing shopper finds them (never a
	// spurious duplicate insert). The FE also displays the +91 form.
	canonical := "+91" + phone
	acct, err := s.repo.findAccountByPhone(ctx, canonical)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		acct = &account{
			ID:        primitive.NewObjectID(),
			Phone:     canonical,
			FullName:  nil,
			Status:    "ACTIVE",
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.repo.insertAccount(ctx, acct); err != nil {
			// A concurrent verify created it — re-read.
			if existing, e := s.repo.findAccountByPhone(ctx, canonical); e == nil && existing != nil {
				acct = existing
			} else {
				return nil, err
			}
		}
		// Empty wallet — money only ever enters via a real top-up.
		_ = s.repo.insertWallet(ctx, &wallet{
			ID: primitive.NewObjectID(), ConsumerID: acct.ID, Currency: "INR", Seq: 0,
		})
	}
	return s.issueTokens(ctx, acct, now)
}

func (s *service) issueTokens(ctx context.Context, acct *account, now time.Time) (*tokenPair, error) {
	access, err := s.signAccessToken(acct.ID.Hex(), acct.Phone, now)
	if err != nil {
		return nil, errInternal("token sign failed")
	}
	rawRefresh, err := auth.RandomToken(refreshBytes)
	if err != nil {
		return nil, errInternal("refresh gen failed")
	}
	if err := s.repo.insertRefresh(ctx, refreshToken{
		TokenHash:  auth.HMACHash(s.deps.Cfg.OTPHashSecret, refreshHashLabel, rawRefresh),
		ConsumerID: acct.ID,
		ExpiresAt:  now.Add(s.deps.Cfg.RefreshTokenTTL),
		CreatedAt:  now,
	}); err != nil {
		return nil, err
	}
	return &tokenPair{AccessToken: access, RefreshToken: rawRefresh, Profile: acct}, nil
}

// refresh rotates the refresh token and re-issues the pair.
func (s *service) refresh(ctx context.Context, rawRefresh string) (*tokenPair, error) {
	if rawRefresh == "" {
		return nil, errUnauthorized("refresh token required")
	}
	hash := auth.HMACHash(s.deps.Cfg.OTPHashSecret, refreshHashLabel, rawRefresh)
	doc, err := s.repo.findRefresh(ctx, hash)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if doc == nil || now.After(doc.ExpiresAt) {
		return nil, errUnauthorized("invalid or expired refresh token")
	}
	// Rotate: consume the old token first (a replay finds nothing).
	if deleted, e := s.repo.deleteRefresh(ctx, hash); e != nil {
		return nil, e
	} else if deleted == 0 {
		return nil, errUnauthorized("refresh token already used")
	}
	acct, err := s.repo.findAccountByID(ctx, doc.ConsumerID)
	if err != nil {
		return nil, errUnauthorized("account no longer exists")
	}
	return s.issueTokens(ctx, acct, now)
}

func (s *service) logout(ctx context.Context, rawRefresh string) error {
	if rawRefresh == "" {
		return nil
	}
	_, _ = s.repo.deleteRefresh(ctx, auth.HMACHash(s.deps.Cfg.OTPHashSecret, refreshHashLabel, rawRefresh))
	return nil
}

// ── Profile ─────────────────────────────────────────────────────────────────

func (s *service) me(ctx context.Context, consumerID primitive.ObjectID) (*account, error) {
	return s.repo.findAccountByID(ctx, consumerID)
}

func (s *service) updateMe(ctx context.Context, consumerID primitive.ObjectID, patch map[string]any) (*account, error) {
	// Only whitelisted, shopper-owned fields are patchable — id, phone,
	// referral_code, status, membership_tier are server-controlled.
	set := bson.D{}
	str := func(k, col string) {
		if v, ok := patch[k]; ok {
			if s, ok := v.(string); ok {
				set = append(set, bson.E{Key: col, Value: strings.TrimSpace(s)})
			}
		}
	}
	str("full_name", "full_name")
	str("email", "email")
	str("alternate_phone", "alternate_phone")
	str("milk_preference", "milk_preference")
	str("avatar_url", "avatar_url")
	str("delivery_slot", "delivery_slot")
	if v, ok := patch["family_member_count"]; ok {
		if f, ok := v.(float64); ok {
			set = append(set, bson.E{Key: "family_member_count", Value: int(f)})
		}
	}
	if len(set) == 0 {
		return s.repo.findAccountByID(ctx, consumerID)
	}
	return s.repo.updateAccount(ctx, consumerID, set)
}

func (s *service) erase(ctx context.Context, consumerID primitive.ObjectID) error {
	return s.repo.deleteAccountCascade(ctx, consumerID)
}

// ── Wallet ──────────────────────────────────────────────────────────────────

const lowBalanceThreshold = 100.0

func (s *service) getOrCreateWallet(ctx context.Context, consumerID primitive.ObjectID) (*wallet, error) {
	wl, err := s.repo.findWallet(ctx, consumerID)
	if err != nil {
		return nil, err
	}
	if wl == nil {
		wl = &wallet{ID: primitive.NewObjectID(), ConsumerID: consumerID, Currency: "INR", Seq: 0}
		if err := s.repo.insertWallet(ctx, wl); err != nil {
			return nil, err
		}
	}
	return wl, nil
}

func walletToView(wl *wallet) walletView {
	available := wl.CashBalance + wl.RewardsBalance - wl.HeldAmount
	return walletView{
		Cash: wl.CashBalance, Rewards: wl.RewardsBalance, Held: wl.HeldAmount,
		Available: available, Currency: wl.Currency, LowBalance: available < lowBalanceThreshold,
		AutoRecharge: wl.AutoRecharge,
	}
}

func (s *service) wallet(ctx context.Context, consumerID primitive.ObjectID) (walletView, error) {
	wl, err := s.getOrCreateWallet(ctx, consumerID)
	if err != nil {
		return walletView{}, err
	}
	return walletToView(wl), nil
}

// bonusFor returns the promotional Rewards bonus for a Cash top-up (§17: bonus
// lands in Rewards, never Cash). Simple tiered rule for the pilot.
// bonusFor mirrors the FE's RECHARGE_TIERS (lib/pricing.ts) EXACTLY — the tier
// the customer was shown at checkout must be the tier the wallet actually
// credits. Picks the highest tier whose threshold the amount meets.
func bonusFor(amount float64) float64 {
	switch {
	case amount >= 10000:
		return 1000
	case amount >= 1000:
		return 250
	case amount >= 500:
		return 100
	case amount >= 200:
		return 50
	default:
		return 0
	}
}

// round2 bounds float rupee rounding to paise (pilot; production would use
// integer paise end-to-end).
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// creditTopup credits Cash (real money via the aggregator) plus the promotional
// Rewards bonus. EXACTLY-ONCE by ref: the TOPUP ledger row is inserted FIRST
// under a unique (consumer, ref, type) index as the money gate — a
// duplicate/concurrent same-ref call gets a dup-key and returns the current
// balance WITHOUT a second $inc, so a retried/duplicated gateway webhook can
// never mint money. (Residual: a crash after the gate insert but before the
// $inc under-credits — recoverable by reconciliation; it never OVER-credits.)
// Shared by the dev /wallet/topup path and the real Razorpay /wallet/verify path.
func (s *service) creditTopup(ctx context.Context, consumerID primitive.ObjectID, amount float64, method, ref string) (*wallet, error) {
	now := time.Now().UTC()
	bonus := round2(bonusFor(amount))

	cashRow := walletTxn{
		ID: primitive.NewObjectID(), ConsumerID: consumerID,
		Type: "TOPUP", Bucket: "CASH", Amount: amount, RefType: method, RefID: ref, Status: "SUCCESS", CreatedAt: now,
	}
	dup, err := s.repo.insertWalletTxnGate(ctx, cashRow)
	if err != nil {
		return nil, err
	}
	if dup {
		return s.getOrCreateWallet(ctx, consumerID) // already credited
	}

	// We own this movement — apply it atomically ($inc of cash + bonus).
	seqDelta := int64(1)
	if bonus > 0 {
		seqDelta = 2
	}
	updated, err := s.repo.incWallet(ctx, consumerID, amount, bonus, 0, seqDelta)
	if err != nil {
		return nil, err
	}
	// Backfill the running balances on the gate row now that we know them.
	topupSeq := updated.Seq - (seqDelta - 1)
	s.repo.updateWalletTxnBalances(ctx, cashRow.ID, updated.ID, topupSeq, updated.CashBalance, round2(updated.RewardsBalance-bonus))
	if bonus > 0 {
		_, _ = s.repo.insertWalletTxnGate(ctx, walletTxn{
			ID: primitive.NewObjectID(), WalletID: updated.ID, ConsumerID: consumerID, Seq: updated.Seq,
			Type: "BONUS", Bucket: "REWARDS", Amount: bonus, CashAfter: updated.CashBalance, RewardsAfter: updated.RewardsBalance,
			RefType: "promo", RefID: ref, Status: "SUCCESS", Remark: "recharge bonus", CreatedAt: now,
		})
	}
	return updated, nil
}

// topup is the DEV direct-credit path (POST /wallet/topup). The FE's real
// money-in path is createTopupOrder + verifyPayment (Razorpay); this stays for
// offline/testing and requires an explicit ref.
func (s *service) topup(ctx context.Context, consumerID primitive.ObjectID, amount float64, method, ref string) (walletView, error) {
	amount = round2(amount)
	if amount <= 0 || amount > 100000 {
		return walletView{}, errBadRequest("amount must be between 1 and 100000")
	}
	// A payment reference is REQUIRED — money never moves without an idempotency key.
	if ref == "" {
		return walletView{}, errBadRequest("a payment reference (ref) is required")
	}
	wl, err := s.creditTopup(ctx, consumerID, amount, method, ref)
	if err != nil {
		return walletView{}, err
	}
	return walletToView(wl), nil
}

// createTopupOrder mints an amount-bound Razorpay order (POST /wallet/order).
// The amount is persisted server-side so verifyPayment credits exactly what was
// ordered — a tampered client cannot pay ₹1 for a ₹500 top-up.
func (s *service) createTopupOrder(ctx context.Context, consumerID primitive.ObjectID, amountPaise int64) (topupOrderView, error) {
	if amountPaise < 100 || amountPaise > 100000_00 {
		return topupOrderView{}, errBadRequest("amount must be between ₹1 and ₹1,00,000")
	}
	receipt := "wtu_" + consumerID.Hex()
	orderID, err := s.createRzpOrder(ctx, amountPaise, receipt)
	if err != nil {
		return topupOrderView{}, err
	}
	if err := s.repo.insertPaymentOrder(ctx, &paymentOrder{
		ID: primitive.NewObjectID(), OrderID: orderID, ConsumerID: consumerID,
		AmountPaise: amountPaise, Receipt: receipt, Status: "CREATED", CreatedAt: time.Now().UTC(),
	}); err != nil {
		return topupOrderView{}, err
	}
	return topupOrderView{OrderID: orderID, KeyID: s.rzpKeyID}, nil
}

// verifyPayment authoritatively verifies a completed Razorpay payment and
// credits the wallet EXACTLY ONCE (POST /wallet/verify). Verification order:
// signature (crypto) → order ownership → CREATED→PAID transition (the settle
// gate) → ledger credit (the money gate). A replay after a successful credit is
// idempotent; a bad signature never credits and never 500s.
func (s *service) verifyPayment(ctx context.Context, consumerID primitive.ObjectID, paymentID, orderID, signature string) (verifyView, error) {
	if !s.verifyRzpSignature(orderID, paymentID, signature) {
		return verifyView{Verified: false}, nil
	}
	ord, err := s.repo.findPaymentOrder(ctx, orderID, consumerID)
	if err != nil {
		return verifyView{Verified: false}, nil // unknown/foreign order — not verifiable
	}
	amount := round2(float64(ord.AmountPaise) / 100)

	// Settle gate: only the CREATED→PAID transition proceeds to credit. A replay
	// finds it already PAID and returns the current balance (idempotent).
	transitioned, err := s.repo.markPaymentOrderPaid(ctx, orderID, paymentID, time.Now().UTC())
	if err != nil {
		return verifyView{}, err
	}
	if !transitioned {
		wl, _ := s.getOrCreateWallet(ctx, consumerID)
		return verifyView{Verified: true, Balance: round2(wl.CashBalance)}, nil
	}
	// Ledger money gate keyed by the order id (belt-and-braces with the settle gate).
	wl, err := s.creditTopup(ctx, consumerID, amount, "razorpay", orderID)
	if err != nil {
		return verifyView{}, err
	}
	return verifyView{Verified: true, Balance: round2(wl.CashBalance)}, nil
}

func (s *service) walletTxns(ctx context.Context, consumerID primitive.ObjectID, limit int64) ([]walletTxn, error) {
	return s.repo.listWalletTxns(ctx, consumerID, limit)
}

// ── Addresses ───────────────────────────────────────────────────────────────

func (s *service) listAddresses(ctx context.Context, consumerID primitive.ObjectID) ([]address, error) {
	return s.repo.listAddresses(ctx, consumerID)
}

func (s *service) createAddress(ctx context.Context, consumerID primitive.ObjectID, in addressInput) (*address, error) {
	now := time.Now().UTC()
	a := &address{
		ID: primitive.NewObjectID(), ConsumerID: consumerID,
		Label: in.Label, Line1: in.Line1, Line2: in.Line2, City: in.City, Pincode: in.Pincode,
		IsDefault: in.IsDefault, Lat: in.Lat, Lng: in.Lng, CreatedAt: now,
	}
	// Serving-store resolution seam (§6): resolve address geo → store polygon.
	// Kept as a pilot stub — the store registry lands in Phase 2.
	if in.IsDefault {
		if err := s.repo.clearDefaults(ctx, consumerID); err != nil {
			return nil, err
		}
	} else {
		// First address is default by default.
		if existing, err := s.repo.listAddresses(ctx, consumerID); err == nil && len(existing) == 0 {
			a.IsDefault = true
		}
	}
	if err := s.repo.insertAddress(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *service) makeDefault(ctx context.Context, consumerID, addrID primitive.ObjectID) (*address, error) {
	// Verify the target exists AND belongs to this consumer BEFORE clearing the
	// existing defaults — otherwise a bad/foreign id would wipe every default and
	// leave the shopper with ZERO defaults (breaking the single-default invariant).
	if _, err := s.repo.findAddress(ctx, addrID, consumerID); err != nil {
		return nil, err
	}
	if err := s.repo.clearDefaults(ctx, consumerID); err != nil {
		return nil, err
	}
	return s.repo.updateAddress(ctx, addrID, consumerID, bson.D{{Key: "is_default", Value: true}})
}

func (s *service) setAddressGeo(ctx context.Context, consumerID, addrID primitive.ObjectID, lat, lng float64) (*address, error) {
	return s.repo.updateAddress(ctx, addrID, consumerID, bson.D{{Key: "lat", Value: lat}, {Key: "lng", Value: lng}})
}

func (s *service) deleteAddress(ctx context.Context, consumerID, addrID primitive.ObjectID) error {
	// Preserve the single-default invariant: if the deleted one was default,
	// promote the newest remaining address.
	target, err := s.repo.findAddress(ctx, addrID, consumerID)
	if err != nil {
		return err
	}
	if _, err := s.repo.deleteAddress(ctx, addrID, consumerID); err != nil {
		return err
	}
	if target.IsDefault {
		if rest, e := s.repo.listAddresses(ctx, consumerID); e == nil && len(rest) > 0 {
			_, _ = s.repo.updateAddress(ctx, rest[0].ID, consumerID, bson.D{{Key: "is_default", Value: true}})
		}
	}
	return nil
}

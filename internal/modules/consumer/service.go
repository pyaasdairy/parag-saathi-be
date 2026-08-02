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

	"github.com/pyaas/saathi-backend/internal/modules/publictrace"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/sms"
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
	// trace is the operator's public QR resolver, reused READ-ONLY for the
	// consumer traceability bridge (tracebridge.go). Never mutates operator state.
	trace *publictrace.Service
	// appKey gates the traceability bridge to the consumer app only: the app
	// ships EXPO_PUBLIC_CONSUMER_APP_KEY and sends it as X-Parag-App-Key; the
	// backend requires a match. Empty → gate disabled (local dev without a key).
	appKey string
	// sms delivers the login OTP over MSG91 (env MSG91_AUTHKEY + MSG91_TEMPLATE_ID).
	// Disabled → dev echo only (see requestOTP).
	sms *sms.MSG91
}

func newService(d *deps.Deps, repo *repository, log *slog.Logger) *service {
	return &service{
		deps:         d,
		repo:         repo,
		log:          log,
		consumerKey:  []byte(auth.HMACHash(d.Cfg.JWTSecret, "consumer-jwt-v1")),
		rzpKeyID:     os.Getenv("RAZORPAY_KEY_ID"),
		rzpKeySecret: os.Getenv("RAZORPAY_KEY_SECRET"),
		trace:        publictrace.NewService(d, log),
		appKey:       os.Getenv("CONSUMER_APP_KEY"),
		sms:          sms.NewMSG91(os.Getenv("MSG91_AUTHKEY"), os.Getenv("MSG91_TEMPLATE_ID")),
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
	// Deliver the code over SMS (MSG91) SYNCHRONOUSLY — a login OTP must not wait
	// on the outbox worker. The code is still generated + verified by us; MSG91 is
	// only the transport. If it isn't configured we fall through to dev echo.
	if s.sms.Enabled() {
		if err := s.sms.SendOTP(ctx, phone, code); err != nil {
			s.log.ErrorContext(ctx, "consumer otp sms send failed", slog.String("phone", phone), slog.Any("err", err))
			return "", time.Time{}, errInternal("could not send the OTP right now — please try again")
		}
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
// offline/testing only. GATED to dev mode — in production it must be impossible
// to mint wallet money without a signature-verified payment (the shipped app
// never calls this in backend mode; recharge always goes through order+verify).
func (s *service) topup(ctx context.Context, consumerID primitive.ObjectID, amount float64, method, ref string) (walletView, error) {
	if !s.deps.Cfg.OTPDevMode {
		return walletView{}, errForbidden("not available")
	}
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

// promoCredit credits the REWARDS bucket only (POST /wallet/promo) — the seam
// for marketing grants like the free-pack "2 mornings on us" funnel. Same
// exactly-once gate as creditTopup (unique (consumer, ref, type) ledger row
// inserted FIRST), so a re-claimed pack can never double-credit. Dev-gated like
// /wallet/topup until a server-side campaign registry decides eligibility.
func (s *service) promoCredit(ctx context.Context, consumerID primitive.ObjectID, amount float64, ref, remark string) (walletView, error) {
	if !s.deps.Cfg.OTPDevMode {
		return walletView{}, errForbidden("not available")
	}
	amount = round2(amount)
	if amount <= 0 || amount > 5000 {
		return walletView{}, errBadRequest("promo amount must be between 1 and 5000")
	}
	if ref == "" {
		return walletView{}, errBadRequest("a promo reference (ref) is required")
	}
	now := time.Now().UTC()
	row := walletTxn{
		ID: primitive.NewObjectID(), ConsumerID: consumerID,
		Type: "BONUS", Bucket: "REWARDS", Amount: amount, RefType: "promo", RefID: ref,
		Status: "SUCCESS", Remark: remark, CreatedAt: now,
	}
	dup, err := s.repo.insertWalletTxnGate(ctx, row)
	if err != nil {
		return walletView{}, err
	}
	if dup { // already granted — return the current balance, no second $inc
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
		AmountPaise: amountPaise, Receipt: receipt, Purpose: "topup", Status: "CREATED", CreatedAt: time.Now().UTC(),
	}); err != nil {
		return topupOrderView{}, err
	}
	return topupOrderView{OrderID: orderID, KeyID: s.rzpKeyID}, nil
}

// createOrderPayment mints an amount-bound Razorpay order for an EXISTING
// consumer order's total (POST /orders/{id}/pay) — the seam for paying an order
// directly via the gateway instead of the wallet. The order total is read
// SERVER-SIDE (never a client amount), and the payment order is tagged
// purpose="order" so the wallet/verify credit path refuses it (an order payment
// must never top up the wallet). DEV-gated until the order-pay verify/capture
// flow lands.
func (s *service) createOrderPayment(ctx context.Context, consumerID primitive.ObjectID, orderID string) (topupOrderView, error) {
	if !s.deps.Cfg.OTPDevMode {
		return topupOrderView{}, errForbidden("not available")
	}
	ord, err := s.repo.findOrder(ctx, orderID, consumerID.Hex())
	if err != nil {
		return topupOrderView{}, err
	}
	amountPaise := int64(round2(ord.Total) * 100)
	if amountPaise < 100 {
		return topupOrderView{}, errBadRequest("order total is too low to pay via the gateway")
	}
	receipt := "ord_" + ord.OrderID
	rzpOrderID, err := s.createRzpOrder(ctx, amountPaise, receipt)
	if err != nil {
		return topupOrderView{}, err
	}
	if err := s.repo.insertPaymentOrder(ctx, &paymentOrder{
		ID: primitive.NewObjectID(), OrderID: rzpOrderID, ConsumerID: consumerID,
		AmountPaise: amountPaise, Receipt: receipt, Purpose: "order", RefID: ord.OrderID,
		Status: "CREATED", CreatedAt: time.Now().UTC(),
	}); err != nil {
		return topupOrderView{}, err
	}
	return topupOrderView{OrderID: rzpOrderID, KeyID: s.rzpKeyID, AmountPaise: amountPaise}, nil
}

// verifyPayment authoritatively verifies a completed Razorpay payment and
// credits the wallet EXACTLY ONCE (POST /wallet/verify). Verification order:
// signature (crypto) → order ownership → ledger credit (the money gate, keyed by
// the order id) → mark the order PAID (pure bookkeeping).
//
// The CREDIT is the money gate and runs FIRST: creditTopup is idempotent by
// (consumer, orderID, TOPUP), so a retry/replay credits exactly once and is
// self-healing — a transient failure on a prior attempt is repaired by the next
// retry (the credit had not committed, so it runs now). markPaymentOrderPaid is
// downstream bookkeeping only, so a failure there can never suppress the credit.
// A bad signature never credits and never 500s.
func (s *service) verifyPayment(ctx context.Context, consumerID primitive.ObjectID, paymentID, orderID, signature string) (verifyView, error) {
	if !s.verifyRzpSignature(orderID, paymentID, signature) {
		return verifyView{Verified: false}, nil
	}
	ord, err := s.repo.findPaymentOrder(ctx, orderID, consumerID)
	if err != nil {
		return verifyView{Verified: false}, nil // unknown/foreign order — not verifiable
	}
	// wallet/verify credits the WALLET — it must only ever act on a top-up order.
	// An order-pay order (purpose="order") is not a wallet credit and is refused
	// here so it can never mint wallet money. Empty purpose = legacy top-up.
	if ord.Purpose != "" && ord.Purpose != "topup" {
		return verifyView{Verified: false}, nil
	}
	amount := round2(float64(ord.AmountPaise) / 100)

	// Money gate FIRST — idempotent by the order id, so it credits exactly once
	// across retries and heals a prior partial attempt.
	wl, err := s.creditTopup(ctx, consumerID, amount, "razorpay", orderID)
	if err != nil {
		return verifyView{}, err
	}
	// Bookkeeping — record the order PAID (best-effort; never gates the credit).
	_, _ = s.repo.markPaymentOrderPaid(ctx, orderID, paymentID, time.Now().UTC())
	return verifyView{Verified: true, Balance: round2(wl.CashBalance)}, nil
}

func (s *service) walletTxns(ctx context.Context, consumerID primitive.ObjectID, limit int64) ([]walletTxn, error) {
	return s.repo.listWalletTxns(ctx, consumerID, limit)
}

// debit spends `amount` from the wallet (promo-first) for a purchase/delivery.
// EXACTLY-ONCE by ref: a DEBIT gate row is inserted first (unique consumer+ref+
// type), so a retried settle sweep for the same order can never double-charge.
// The balance move is a single atomic guarded update that fails closed on
// insufficient funds. If the move does NOT happen (insufficient funds or a
// transient error) the gate row is rolled back on a DETACHED context — a
// cancelled request can't orphan it — so a later top-up + retry still charges.
func (s *service) debit(ctx context.Context, consumerID primitive.ObjectID, amount float64, ref, remark string) (walletView, error) {
	amount = round2(amount)
	if amount <= 0 {
		return walletView{}, errBadRequest("amount must be positive")
	}
	if ref == "" {
		return walletView{}, errBadRequest("a debit reference (ref) is required")
	}
	now := time.Now().UTC()
	// Pre-read the balances so the ledger row records the bucket the debit
	// actually drew from (promo-first). Money correctness does NOT depend on this
	// read — the atomic guarded update below is the authority; this only labels
	// the ledger row for display.
	pre, _ := s.getOrCreateWallet(ctx, consumerID)
	bucket := "CASH"
	if pre != nil && pre.RewardsBalance >= amount {
		bucket = "REWARDS" // fully covered by promo
	}
	gate := walletTxn{
		ID: primitive.NewObjectID(), ConsumerID: consumerID,
		Type: "DEBIT", Bucket: bucket, Amount: amount, RefType: "order", RefID: ref, Status: "SUCCESS", Remark: remark, CreatedAt: now,
	}
	dup, err := s.repo.insertWalletTxnGate(ctx, gate)
	if err != nil {
		return walletView{}, err
	}
	if dup {
		wl, e := s.getOrCreateWallet(ctx, consumerID)
		if e != nil {
			return walletView{}, e
		}
		return walletToView(wl), nil // already debited — idempotent
	}
	updated, ok, err := s.repo.debitWalletAtomic(ctx, consumerID, amount, 1)
	if err != nil || !ok {
		// No money moved — roll back the gate on a detached context so a
		// cancelled/timed-out request context cannot orphan it (an orphan would
		// permanently short-circuit a future retry as "already debited").
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.repo.deleteWalletTxn(rbCtx, gate.ID)
		cancel()
		if err != nil {
			return walletView{}, err
		}
		return walletView{}, errUnprocessable("INSUFFICIENT_FUNDS", "wallet balance is too low for this order")
	}
	s.repo.updateWalletTxnBalances(ctx, gate.ID, updated.ID, updated.Seq, updated.CashBalance, updated.RewardsBalance)
	return walletToView(updated), nil
}

// refund credits money back to Cash. EXACTLY-ONCE by ref (a retried refund
// credits at most once). GATED to dev mode — a shopper-initiated arbitrary
// refund would mint spendable Cash with no entitlement. In production, refunds
// are server-authorised and bounded to a prior charge (driven from the order
// cancellation/return flow, a later phase); the shipped app never calls this.
func (s *service) refund(ctx context.Context, consumerID primitive.ObjectID, amount float64, ref, remark string) (walletView, error) {
	if !s.deps.Cfg.OTPDevMode {
		return walletView{}, errForbidden("not available")
	}
	amount = round2(amount)
	if amount <= 0 || amount > 100000 {
		return walletView{}, errBadRequest("amount must be between 1 and 100000")
	}
	if ref == "" {
		return walletView{}, errBadRequest("a refund reference (ref) is required")
	}
	now := time.Now().UTC()
	if remark == "" {
		remark = "refund"
	}
	gate := walletTxn{
		ID: primitive.NewObjectID(), ConsumerID: consumerID,
		Type: "REFUND", Bucket: "CASH", Amount: amount, RefType: "refund", RefID: ref, Status: "SUCCESS", Remark: remark, CreatedAt: now,
	}
	dup, err := s.repo.insertWalletTxnGate(ctx, gate)
	if err != nil {
		return walletView{}, err
	}
	if dup {
		wl, e := s.getOrCreateWallet(ctx, consumerID)
		if e != nil {
			return walletView{}, e
		}
		return walletToView(wl), nil
	}
	updated, err := s.repo.incWallet(ctx, consumerID, amount, 0, 0, 1)
	if err != nil {
		return walletView{}, err
	}
	s.repo.updateWalletTxnBalances(ctx, gate.ID, updated.ID, updated.Seq, updated.CashBalance, updated.RewardsBalance)
	return walletToView(updated), nil
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

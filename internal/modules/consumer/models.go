package consumer

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ── Mongo collections (all consumer_*; never touch operator collections) ────
const (
	collAccounts   = "consumer_accounts"
	collOTP        = "consumer_otp_challenges"
	collRefresh    = "consumer_refresh_tokens"
	collAddresses  = "consumer_addresses"
	collWallets    = "consumer_wallets"
	collWalletTxns = "consumer_wallet_txns"
	collConsents   = "consumer_consents"
	collPayOrders  = "consumer_payment_orders"
	// collCatalog holds the store-manager-owned consumer catalog OVERLAY:
	// per-SKU price/stock/visibility overrides on the baseline milk products,
	// plus store-added SKUs. A thin layer the consumer app reads (never a
	// replacement for the baseline shipped in the app).
	collCatalog = "consumer_catalog"
)

// ── Domain documents ────────────────────────────────────────────────────────

// account is a SHOPPER (never a Party). Flat profile the consumer app reads at
// GET /users/me.
type account struct {
	ID                primitive.ObjectID `bson:"_id,omitempty"           json:"id"`
	Phone             string             `bson:"phone"                  json:"phone"`
	FullName          *string            `bson:"full_name"              json:"full_name"`
	Email             *string            `bson:"email,omitempty"        json:"email"`
	AlternatePhone    *string            `bson:"alternate_phone,omitempty" json:"alternate_phone,omitempty"`
	FamilyMemberCount *int               `bson:"family_member_count,omitempty" json:"family_member_count,omitempty"`
	MilkPreference    *string            `bson:"milk_preference,omitempty" json:"milk_preference,omitempty"`
	AvatarURL         *string            `bson:"avatar_url,omitempty"   json:"avatar_url,omitempty"`
	ReferralCode      *string            `bson:"referral_code,omitempty" json:"referral_code,omitempty"`
	DeliverySlot      *string            `bson:"delivery_slot,omitempty" json:"delivery_slot,omitempty"`
	MembershipTier    string             `bson:"membership_tier,omitempty" json:"membership_tier,omitempty"`
	Status            string             `bson:"status"                 json:"status"`
	CreatedAt         time.Time          `bson:"created_at"             json:"created_at"`
	UpdatedAt         time.Time          `bson:"updated_at"             json:"updated_at"`
}

// otpChallenge mirrors the operator pattern but in the consumer namespace.
type otpChallenge struct {
	ID        string    `bson:"_id"`
	Phone     string    `bson:"phone"`
	CodeHash  string    `bson:"code_hash"`
	ExpiresAt time.Time `bson:"expires_at"`
	Attempts  int       `bson:"attempts"`
	CreatedAt time.Time `bson:"created_at"`
}

// refreshToken — rotating, hashed at rest.
type refreshToken struct {
	TokenHash  string             `bson:"token_hash"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"`
	ExpiresAt  time.Time          `bson:"expires_at"`
	CreatedAt  time.Time          `bson:"created_at"`
}

// address — delivery address; resolves to a serving store + cluster (§6).
type address struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"        json:"id"`
	ConsumerID  primitive.ObjectID `bson:"consumer_id"          json:"user_id"`
	Label       string             `bson:"label,omitempty"      json:"label"`
	Line1       string             `bson:"line1,omitempty"      json:"line1"`
	Line2       string             `bson:"line2,omitempty"      json:"line2"`
	City        string             `bson:"city,omitempty"       json:"city"`
	Pincode     string             `bson:"pincode,omitempty"    json:"pincode"`
	IsDefault   bool               `bson:"is_default"           json:"is_default"`
	Lat         *float64           `bson:"lat,omitempty"        json:"lat,omitempty"`
	Lng         *float64           `bson:"lng,omitempty"        json:"lng,omitempty"`
	StoreID     string             `bson:"store_id,omitempty"   json:"store_id,omitempty"`
	Preferences map[string]any     `bson:"preferences,omitempty" json:"-"`
	CreatedAt   time.Time          `bson:"created_at"           json:"created_at"`
}

// wallet — dual-bucket (Cash real + Rewards promotional), append-only ledger.
type wallet struct {
	ID             primitive.ObjectID `bson:"_id,omitempty"     json:"-"`
	ConsumerID     primitive.ObjectID `bson:"consumer_id"       json:"-"`
	CashBalance    float64            `bson:"cash_balance"      json:"cash"`
	RewardsBalance float64            `bson:"rewards_balance"   json:"rewards"`
	HeldAmount     float64            `bson:"held_amount"       json:"held"`
	Currency       string             `bson:"currency"          json:"currency"`
	AutoRecharge   autoRecharge       `bson:"autorecharge"      json:"autorecharge"`
	Seq            int64              `bson:"seq"               json:"-"`
}

type autoRecharge struct {
	Enabled     bool    `bson:"enabled"      json:"enabled"`
	Threshold   float64 `bson:"threshold"    json:"threshold,omitempty"`
	Amount      float64 `bson:"amount"       json:"amount,omitempty"`
	MandateRef  string  `bson:"mandate_ref"  json:"mandate_ref,omitempty"`
	PerDebitCap float64 `bson:"per_debit_cap" json:"per_debit_cap,omitempty"`
}

// walletTxn — one append-only ledger row (event-sourced money).
type walletTxn struct {
	ID           primitive.ObjectID `bson:"_id,omitempty"     json:"id"`
	WalletID     primitive.ObjectID `bson:"wallet_id"         json:"-"`
	ConsumerID   primitive.ObjectID `bson:"consumer_id"       json:"-"`
	Seq          int64              `bson:"seq"               json:"seq"`
	Type         string             `bson:"type"              json:"type"`   // TOPUP|BONUS|HOLD|CAPTURE|RELEASE|REFUND|ADJUST
	Bucket       string             `bson:"bucket"            json:"bucket"` // CASH|REWARDS
	Amount       float64            `bson:"amount"            json:"amount"`
	CashAfter    float64            `bson:"cash_after"        json:"cash_after"`
	RewardsAfter float64            `bson:"rewards_after"     json:"rewards_after"`
	RefType      string             `bson:"ref_type,omitempty" json:"ref_type,omitempty"`
	RefID        string             `bson:"ref_id,omitempty"  json:"ref_id,omitempty"`
	Status       string             `bson:"status,omitempty"  json:"status,omitempty"`
	Remark       string             `bson:"remark,omitempty"  json:"remark,omitempty"`
	CreatedAt    time.Time          `bson:"created_at"        json:"created_at"`
}

// paymentOrder is a wallet top-up order whose amount is BOUND server-side at
// creation, so a tampered client cannot pay ₹1 for a ₹500 top-up (§ razorpay.ts
// security model). Verified before the wallet is credited.
type paymentOrder struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"`
	OrderID     string             `bson:"order_id"`    // Razorpay order id (or dev pseudo id) — unique
	ConsumerID  primitive.ObjectID `bson:"consumer_id"` // owner
	AmountPaise int64              `bson:"amount_paise"`
	Receipt     string             `bson:"receipt,omitempty"`
	// Purpose separates wallet top-ups from direct order payments so the
	// wallet/verify credit path can NEVER be tricked into crediting the wallet for
	// an order-pay order. Empty is treated as "topup" (backward compat).
	Purpose   string     `bson:"purpose,omitempty"` // topup | order
	RefID     string     `bson:"ref_id,omitempty"`  // linked consumer order id when purpose=order
	Status    string     `bson:"status"`            // CREATED | PAID
	PaymentID string     `bson:"payment_id,omitempty"`
	CreatedAt time.Time  `bson:"created_at"`
	PaidAt    *time.Time `bson:"paid_at,omitempty"`
}

// ── Wire response shapes the FE reads (raw, not enveloped) ──────────────────

// tokenPair is what /auth/otp/verify and /auth/refresh return.
type tokenPair struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	Profile      *account `json:"profile,omitempty"`
}

// topupOrderView is POST /wallet/order — the amount-bound order the FE hands to
// Razorpay checkout. Reused by POST /orders/{id}/pay (with amountPaise set).
type topupOrderView struct {
	OrderID     string `json:"orderId"`
	KeyID       string `json:"keyId"`
	AmountPaise int64  `json:"amountPaise,omitempty"`
}

// verifyView is POST /wallet/verify — authoritative credit result.
type verifyView struct {
	Verified bool    `json:"verified"`
	Balance  float64 `json:"balance,omitempty"`
}

// walletView is GET /wallet — dual bucket + derived available.
type walletView struct {
	Cash         float64      `json:"cash"`
	Rewards      float64      `json:"rewards"`
	Held         float64      `json:"held"`
	Available    float64      `json:"available"`
	Currency     string       `json:"currency"`
	LowBalance   bool         `json:"lowBalance"`
	AutoRecharge autoRecharge `json:"autorecharge"`
}

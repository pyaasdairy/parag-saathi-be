// Package config loads all runtime configuration from the environment.
// A local `.env` file is read first (dev convenience, never committed);
// real environment variables always win. There are NO in-code connection
// strings: the server refuses to boot without MONGO_URI.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Env      string // dev | prod
	Port     int
	LogLevel string

	MongoURI string
	MongoDB  string

	JWTSecret       string
	QRSigningSecret string
	OTPHashSecret   string

	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	OTPTTL          time.Duration
	OTPDevMode      bool // true → OTP echoed in API response instead of SMS (dev only)

	RateLimitRPS   float64
	RateLimitBurst int

	// RedisURL, when set, switches rate limiting to a shared Redis token bucket
	// for global fairness across replicas. Empty → in-process limiting (dev,
	// single instance). Format: redis://[:password@]host:port/db
	RedisURL string

	SeedAdminPhone string

	// ── Dolibarr ERP integration (docs/dolibarr-sync.md) ────────────────────
	// ALL optional: when DolibarrURL or DolibarrAPIKey is empty the integration
	// is fully OFF — no workers start, no behaviour changes, zero impact.
	//
	// INBOUND  — catalog sync mirrors the ERP product master (name/price/stock/
	//            images) into consumer_catalog; Dolibarr owns the master, Saathi
	//            owns presentation (seeded photos/categories are never touched).
	// OUTBOUND — nightly job posts one NET stock-out per SKU per day to
	//            warehouse DolibarrOutWarehouseID. DRY-RUN (log only) unless
	//            DolibarrPostStockOut is explicitly true.
	DolibarrURL             string        // e.g. https://pyaas.with10.dolicloud.com/api/index.php
	DolibarrAPIKey          string        // DOLAPIKEY of the scoped saathi.sync user
	DolibarrStoreID         string        // store whose overlay receives overrides/additions
	DolibarrSyncEvery       time.Duration // catalog pull cadence (also runs once at boot)
	DolibarrWebhookToken    string        // shared secret for POST /consumer/dolibarr/webhook (instant sync kick)
	DolibarrStockOutHourIST int           // IST hour to post YESTERDAY's net stock-out
	DolibarrOutWarehouseID  int           // 2 = MOBILE HUB (id 1 AT PLANT is UI-only)
	DolibarrPostStockOut    bool          // false → compute + log only (dry-run)

	// ── Consumer growth programmes (referrals.go, founding.go) ──────────────
	// Every value is optional and has an in-code default (see the accessors
	// below), so a zero Config still prices the programmes the way the app
	// copy promises them. The two *int64 amounts treat 0 as a real setting
	// (nil = not configured -> default).
	ReferralRewardPaise *int64 // REFERRAL_REWARD_PAISE: promo credit to BOTH sides on the referee's first paid delivery; 0 stops paying
	// Founding Family. Prices are read from the ERP when the sync has seen the
	// FOUNDING-99 / DELIVERY-FEE services; these are the fallbacks until then.
	FoundingPriceMonthPaise        int64  // FOUNDING_PRICE_MONTH_PAISE (FOUNDING-99)
	FoundingDeliveryFeePaise       int64  // FOUNDING_DELIVERY_FEE_PAISE (DELIVERY-FEE)
	FoundingLevel3OffPaisePerLitre *int64 // FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE: level 3 = level 1 minus this per litre on PYAAS milk when the ERP carries no level 3; 0 = no derived discount
	FoundingSavingsSKU             string // FOUNDING_SAVINGS_SKU: the 1 L PYAAS line behind the "1 L a day saves" line
	FoundingBillRetryDays          int    // FOUNDING_BILL_RETRY_DAYS: retries before a short wallet stops the membership
	FoundingClosed                 bool   // FOUNDING_FAMILY_CLOSED=true: GET /founding-family answers 404 (the app says opening soon)
	FoundingPyaasMembersOnly       bool   // FOUNDING_PYAAS_MEMBERS_ONLY=true: PYAAS milk lines refuse non-active members (spec rule 5.1)
	FoundingPyaasNonMemberFee      bool   // FOUNDING_PYAAS_NONMEMBER_FEE=true: DELIVERY-FEE on PYAAS-milk orders by non-members (spec rule 5.3, needs the founder's yes)
}

// ReferralReward is the promo credit, in rupees, each side receives on the
// referee's first paid delivery (the Refer screen's "Gift Rs 100, get
// Rs 100"). REFERRAL_REWARD_PAISE=0 stops paying; unset keeps Rs 100.
func (c *Config) ReferralReward() float64 {
	if c.ReferralRewardPaise == nil {
		return 100
	}
	if *c.ReferralRewardPaise <= 0 {
		return 0
	}
	return float64(*c.ReferralRewardPaise) / 100
}

// FoundingPriceMonth is the FOUNDING-99 fallback in rupees.
func (c *Config) FoundingPriceMonth() float64 {
	if c.FoundingPriceMonthPaise > 0 {
		return float64(c.FoundingPriceMonthPaise) / 100
	}
	return 99
}

// FoundingDeliveryFee is the DELIVERY-FEE fallback in rupees.
func (c *Config) FoundingDeliveryFee() float64 {
	if c.FoundingDeliveryFeePaise > 0 {
		return float64(c.FoundingDeliveryFeePaise) / 100
	}
	return 5
}

// FoundingLevel3OffPerLitre is the member discount per litre in rupees, used
// only for PYAAS milk lines the ERP has not priced at level 3 yet. 0 means
// no derived discount; unset keeps Rs 2.
func (c *Config) FoundingLevel3OffPerLitre() float64 {
	if c.FoundingLevel3OffPaisePerLitre == nil {
		return 2
	}
	if *c.FoundingLevel3OffPaisePerLitre <= 0 {
		return 0
	}
	return float64(*c.FoundingLevel3OffPaisePerLitre) / 100
}

// FoundingSavingsReferenceSKU is the catalog id behind the savings line.
func (c *Config) FoundingSavingsReferenceSKU() string {
	if c.FoundingSavingsSKU != "" {
		return c.FoundingSavingsSKU
	}
	return "pyaas-toned-1l"
}

// FoundingBillRetries is how many billing days a short wallet is retried
// before the membership is set to stopped (spec rule 5.6: three days).
func (c *Config) FoundingBillRetries() int {
	if c.FoundingBillRetryDays > 0 {
		return c.FoundingBillRetryDays
	}
	return 3
}

// DolibarrEnabled reports whether the ERP integration is configured at all.
func (c *Config) DolibarrEnabled() bool { return c.DolibarrURL != "" && c.DolibarrAPIKey != "" }

// IsProd reports whether the server runs with production hardening.
func (c *Config) IsProd() bool { return c.Env == "prod" }

// Load reads `.env` (if present) then the process environment, validates the
// result, and returns the Config. Missing MONGO_URI is a hard error.
func Load() (*Config, error) {
	loadDotEnv(".env")

	cfg := &Config{
		Env:             envStr("ENV", "dev"),
		Port:            envInt("PORT", 8080),
		LogLevel:        envStr("LOG_LEVEL", "info"),
		MongoURI:        os.Getenv("MONGO_URI"),
		MongoDB:         envStr("MONGO_DB", "saathi"),
		JWTSecret:       envStr("JWT_SECRET", ""),
		QRSigningSecret: envStr("QR_SIGNING_SECRET", ""),
		OTPHashSecret:   envStr("OTP_HASH_SECRET", ""),
		AccessTokenTTL:  time.Duration(envInt("ACCESS_TOKEN_TTL_MINUTES", 15)) * time.Minute,
		RefreshTokenTTL: time.Duration(envInt("REFRESH_TOKEN_TTL_DAYS", 30)) * 24 * time.Hour,
		OTPTTL:          time.Duration(envInt("OTP_TTL_MINUTES", 5)) * time.Minute,
		OTPDevMode:      envBool("OTP_DEV_MODE", false),
		RateLimitRPS:    envFloat("RATE_LIMIT_RPS", 50),
		RateLimitBurst:  envInt("RATE_LIMIT_BURST", 100),
		RedisURL:        os.Getenv("REDIS_URL"),
		SeedAdminPhone:  envStr("SEED_ADMIN_PHONE", "9999999999"),

		DolibarrURL:             strings.TrimRight(envStr("DOLIBARR_URL", ""), "/"),
		DolibarrAPIKey:          envStr("DOLIBARR_API_KEY", ""),
		DolibarrStoreID:         envStr("DOLIBARR_STORE_ID", "6a53fb242b4fd88066524d41"),
		DolibarrSyncEvery:       dolibarrSyncCadence(),
		DolibarrWebhookToken:    envStr("DOLIBARR_WEBHOOK_TOKEN", ""),
		DolibarrStockOutHourIST: envInt("DOLIBARR_STOCKOUT_HOUR_IST", 1),
		DolibarrOutWarehouseID:  envInt("DOLIBARR_OUT_WAREHOUSE_ID", 2),
		DolibarrPostStockOut:    envBool("DOLIBARR_POST_STOCKOUT", false),

		ReferralRewardPaise:            envPaise("REFERRAL_REWARD_PAISE", 10000),
		FoundingPriceMonthPaise:        int64(envInt("FOUNDING_PRICE_MONTH_PAISE", 9900)),
		FoundingDeliveryFeePaise:       int64(envInt("FOUNDING_DELIVERY_FEE_PAISE", 500)),
		FoundingLevel3OffPaisePerLitre: envPaise("FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE", 200),
		FoundingSavingsSKU:             envStr("FOUNDING_SAVINGS_SKU", "pyaas-toned-1l"),
		FoundingBillRetryDays:          envInt("FOUNDING_BILL_RETRY_DAYS", 3),
		FoundingClosed:                 envBool("FOUNDING_FAMILY_CLOSED", false),
		FoundingPyaasMembersOnly:       envBool("FOUNDING_PYAAS_MEMBERS_ONLY", false),
		FoundingPyaasNonMemberFee:      envBool("FOUNDING_PYAAS_NONMEMBER_FEE", false),
	}

	if cfg.MongoURI == "" {
		return nil, fmt.Errorf("MONGO_URI is required and must be set via environment (no in-code default); see .env.example")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required; see .env.example")
	}
	if cfg.QRSigningSecret == "" {
		cfg.QRSigningSecret = cfg.JWTSecret + ":qr" // derived fallback for dev only
	}
	if cfg.OTPHashSecret == "" {
		cfg.OTPHashSecret = cfg.JWTSecret + ":otp"
	}

	if cfg.IsProd() {
		if strings.Contains(cfg.JWTSecret, "dev-only") {
			return nil, fmt.Errorf("refusing to start in prod with a dev-only JWT_SECRET")
		}
		if cfg.OTPDevMode {
			return nil, fmt.Errorf("refusing to start in prod with OTP_DEV_MODE=true (OTPs would leak in API responses)")
		}
		// With OTP_DEV_MODE off, MSG91 is the ONLY way an OTP reaches a human.
		// Without these two values the service booted happily, every
		// /auth/otp/request returned 200, and no SMS was ever sent — so nobody
		// could log in and nothing said why. Fail at boot instead, where it is
		// one clear line in the deploy log.
		if os.Getenv("MSG91_AUTHKEY") == "" || os.Getenv("MSG91_TEMPLATE_ID") == "" {
			return nil, fmt.Errorf(
				"refusing to start in prod without MSG91_AUTHKEY and MSG91_TEMPLATE_ID: " +
					"OTP_DEV_MODE is off, so SMS is the only OTP delivery path and login would " +
					"silently fail for every user")
		}
	}
	return cfg, nil
}

// loadDotEnv parses KEY=VALUE lines from path into the process environment,
// without overriding variables that are already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // optional file
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key != "" && os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envPaise reads a paise amount whose 0 is a real setting ("stop paying"):
// the default applies only when the key is unset or not a number.
func envPaise(key string, def int64) *int64 {
	v := int64(envInt(key, int(def)))
	return &v
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// dolibarrSyncCadence — how often the ERP catalog poll runs. Seconds-first:
// DOLIBARR_SYNC_EVERY_SECONDS (default 20 — each pass is a single /products
// call, so a sub-minute cadence is trivially light on DoliCloud). The legacy
// DOLIBARR_SYNC_EVERY_MINUTES is honoured when explicitly set. The webhook
// kick still short-circuits both when configured — this poll is the floor,
// not the ceiling.
func dolibarrSyncCadence() time.Duration {
	if v := os.Getenv("DOLIBARR_SYNC_EVERY_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return time.Duration(envInt("DOLIBARR_SYNC_EVERY_SECONDS", 20)) * time.Second
}

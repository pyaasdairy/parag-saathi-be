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
		DolibarrSyncEvery:       time.Duration(envInt("DOLIBARR_SYNC_EVERY_MINUTES", 1)) * time.Minute,
		DolibarrWebhookToken:    envStr("DOLIBARR_WEBHOOK_TOKEN", ""),
		DolibarrStockOutHourIST: envInt("DOLIBARR_STOCKOUT_HOUR_IST", 1),
		DolibarrOutWarehouseID:  envInt("DOLIBARR_OUT_WAREHOUSE_ID", 2),
		DolibarrPostStockOut:    envBool("DOLIBARR_POST_STOCKOUT", false),
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

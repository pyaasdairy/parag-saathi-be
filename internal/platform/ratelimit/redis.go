package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketLua is an atomic token-bucket evaluated entirely inside Redis, so
// concurrent requests from many API replicas can never over-admit. State per
// key is a hash {tokens, ts}; each call refills by elapsed time, then spends
// one token if available. The key auto-expires once idle long enough to have
// refilled a full burst, so buckets never leak.
//
//	KEYS[1] = bucket key
//	ARGV[1] = rate (tokens/sec)  ARGV[2] = burst
//	ARGV[3] = now (unix seconds, float)  ARGV[4] = requested (1)
//	returns 1 = allowed, 0 = denied
const tokenBucketLua = `
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local req   = tonumber(ARGV[4])

local h = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(h[1])
local ts = tonumber(h[2])
if tokens == nil then tokens = burst; ts = now end

local delta = now - ts
if delta < 0 then delta = 0 end
tokens = math.min(burst, tokens + delta * rate)

local allowed = 0
if tokens >= req then tokens = tokens - req; allowed = 1 end

redis.call('HSET', key, 'tokens', tokens, 'ts', now)
local ttl = math.ceil(burst / rate) + 1
redis.call('EXPIRE', key, ttl)
return allowed
`

// Redis is a shared token-bucket limiter backed by Redis — global fairness
// across every API replica. Fails OPEN on any Redis error so a cache outage
// degrades to "no limiting" rather than an outage of the API itself.
type Redis struct {
	client *redis.Client
	script *redis.Script
	rps    float64
	burst  int
	prefix string
	log    *slog.Logger
}

// NewRedis builds a Redis-backed limiter. The caller owns the *redis.Client
// lifecycle (Ping on boot, Close on shutdown).
func NewRedis(client *redis.Client, rps float64, burst int, log *slog.Logger) *Redis {
	return &Redis{
		client: client,
		script: redis.NewScript(tokenBucketLua),
		rps:    rps,
		burst:  burst,
		prefix: "saathi:rl:",
		log:    log,
	}
}

// Allow consumes one token for key across the shared bucket.
func (r *Redis) Allow(ctx context.Context, key string) bool {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	now := float64(time.Now().UnixNano()) / 1e9
	res, err := r.script.Run(ctx, r.client,
		[]string{r.prefix + key},
		r.rps, r.burst, now, 1,
	).Int()
	if err != nil {
		// Fail open: never let a limiter outage become an API outage.
		r.log.Warn("redis rate limiter error — failing open", slog.String("key", key), slog.Any("err", err))
		return true
	}
	return res == 1
}

// Kind identifies the backend.
func (r *Redis) Kind() string { return "redis" }

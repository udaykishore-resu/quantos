package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Cache is the Redis-backed hot path: latest quote, latest regime, hot feature
// snapshots, rate-limit buckets and the idempotency dedup set.
//
// It is never a source of truth (ADR-003). Every read falls through to the
// authoritative store on a miss or an error, and the two effects that must
// never duplicate — alerts and paper orders — additionally carry unique indexes
// in PostgreSQL, so a full Redis wipe cannot cause a duplicate.
type Cache struct {
	client *redis.Client
	ttl    time.Duration
	prefix string
}

// CacheConfig parameterises the connection.
type CacheConfig struct {
	Addr     string
	Password string
	DB       int
	TTL      time.Duration
	Prefix   string
	Timeout  time.Duration
}

// OpenCache connects and verifies the connection.
func OpenCache(ctx context.Context, cfg CacheConfig) (*Cache, error) {
	if cfg.Addr == "" {
		return nil, errors.New("store: redis address is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "quantos"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	c := redis.NewClient(&redis.Options{
		Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB,
		DialTimeout: cfg.Timeout, ReadTimeout: cfg.Timeout, WriteTimeout: cfg.Timeout,
	})
	pctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := c.Ping(pctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("%w: redis ping: %v", ErrUnavailable, err)
	}
	return &Cache{client: c, ttl: cfg.TTL, prefix: cfg.Prefix}, nil
}

func (c *Cache) key(parts ...string) string {
	k := c.prefix
	for _, p := range parts {
		k += ":" + p
	}
	return k
}

// SetQuote caches the latest quote for a symbol.
func (c *Cache) SetQuote(ctx context.Context, q domain.Quote) error {
	data, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, c.key("quote", string(q.Ticker)), data, c.ttl).Err()
}

// GetQuote reads the cached quote.
func (c *Cache) GetQuote(ctx context.Context, t domain.Ticker) (domain.Quote, bool) {
	data, err := c.client.Get(ctx, c.key("quote", string(t))).Bytes()
	if err != nil {
		return domain.Quote{}, false
	}
	var q domain.Quote
	if json.Unmarshal(data, &q) != nil {
		return domain.Quote{}, false
	}
	return q, true
}

// SetRegime caches the current market regime.
func (c *Cache) SetRegime(ctx context.Context, r domain.MarketRegime) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, c.key("regime"), data, c.ttl).Err()
}

// GetRegime reads the cached regime.
func (c *Cache) GetRegime(ctx context.Context) (domain.MarketRegime, bool) {
	data, err := c.client.Get(ctx, c.key("regime")).Bytes()
	if err != nil {
		return domain.MarketRegime{}, false
	}
	var r domain.MarketRegime
	if json.Unmarshal(data, &r) != nil {
		return domain.MarketRegime{}, false
	}
	return r, true
}

// SetSnapshot caches a feature snapshot by ticker and by hash, so both the
// "latest" and the "reproduce this decision" lookups hit the cache.
func (c *Cache) SetSnapshot(ctx context.Context, s domain.FeatureSnapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	pipe := c.client.Pipeline()
	pipe.Set(ctx, c.key("snap", string(s.Ticker)), data, c.ttl)
	pipe.Set(ctx, c.key("snaph", string(s.Ticker), s.Hash), data, c.ttl)
	_, err = pipe.Exec(ctx)
	return err
}

// GetSnapshot reads a cached snapshot by hash.
func (c *Cache) GetSnapshot(ctx context.Context, t domain.Ticker, hash string) (domain.FeatureSnapshot, bool) {
	data, err := c.client.Get(ctx, c.key("snaph", string(t), hash)).Bytes()
	if err != nil {
		return domain.FeatureSnapshot{}, false
	}
	var s domain.FeatureSnapshot
	if json.Unmarshal(data, &s) != nil {
		return domain.FeatureSnapshot{}, false
	}
	return s, true
}

// Mark implements bus.Deduper: it claims a key, returning true only for the
// first caller. SET NX is atomic, which is exactly the check-and-set the
// idempotency contract needs.
func (c *Cache) Mark(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = 48 * time.Hour
	}
	ok, err := c.client.SetNX(ctx, c.key("dedup", key), "1", ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// Seen implements bus.Deduper.
func (c *Cache) Seen(ctx context.Context, key string) (bool, error) {
	n, err := c.client.Exists(ctx, c.key("dedup", key)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Release removes a dedup claim, used when a handler fails after claiming.
func (c *Cache) Release(ctx context.Context, key string) error {
	return c.client.Del(ctx, c.key("dedup", key)).Err()
}

// rateLimitScript implements a token bucket atomically in Lua, so concurrent
// requests across replicas share one bucket rather than each getting their own.
const rateLimitScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local state = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil then
  tokens = burst
  ts = now
end
local delta = math.max(0, now - ts)
tokens = math.min(burst, tokens + delta * rate)
local allowed = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
end
redis.call('HMSET', key, 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', key, math.ceil(burst / rate) + 60)
return {allowed, tostring(tokens)}
`

var rateLimiter = redis.NewScript(rateLimitScript)

// Allow implements a distributed token bucket. On a Redis error it returns the
// supplied failOpen value: reads fail open, writes fail closed.
func (c *Cache) Allow(ctx context.Context, principal string, rate float64, burst int, now time.Time, failOpen bool) (bool, error) {
	res, err := rateLimiter.Run(ctx, c.client,
		[]string{c.key("rl", principal)},
		rate, burst, float64(now.UnixNano())/1e9, 1).Result()
	if err != nil {
		return failOpen, err
	}
	vals, ok := res.([]any)
	if !ok || len(vals) == 0 {
		return failOpen, fmt.Errorf("store: unexpected rate limiter reply")
	}
	allowed, _ := vals[0].(int64)
	return allowed == 1, nil
}

// Ping verifies the connection.
func (c *Cache) Ping(ctx context.Context) error { return c.client.Ping(ctx).Err() }

// Close releases the connection pool.
func (c *Cache) Close() error { return c.client.Close() }

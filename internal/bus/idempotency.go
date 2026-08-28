package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Deduper records whether an effect key has already been applied.
//
// Mark must be atomic: it returns false if the key was already present, and
// true if this caller is the first to claim it. That check-and-set is what
// turns at-least-once delivery into exactly-once effects (ADR-010, §36).
type Deduper interface {
	Mark(ctx context.Context, key string, ttl time.Duration) (first bool, err error)
	Seen(ctx context.Context, key string) (bool, error)
}

// MemoryDeduper is an in-process Deduper with TTL eviction. It is the default
// in embedded mode and the fallback when Redis is unavailable — note that the
// two effects that must never duplicate (alerts, paper orders) additionally
// carry a unique index in PostgreSQL, so a Redis wipe cannot cause a duplicate.
type MemoryDeduper struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	clock obs.Clock
	// maxEntries bounds memory; eviction is oldest-first when exceeded.
	maxEntries int
}

// NewMemoryDeduper returns an in-process deduper.
func NewMemoryDeduper(clock obs.Clock, maxEntries int) *MemoryDeduper {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	if maxEntries <= 0 {
		maxEntries = 1 << 20
	}
	return &MemoryDeduper{seen: map[string]time.Time{}, clock: clock, maxEntries: maxEntries}
}

// Mark claims a key, returning true only for the first caller.
func (d *MemoryDeduper) Mark(_ context.Context, key string, ttl time.Duration) (bool, error) {
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if exp, ok := d.seen[key]; ok && exp.After(now) {
		return false, nil
	}
	if len(d.seen) >= d.maxEntries {
		d.evictLocked(now)
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	d.seen[key] = now.Add(ttl)
	return true, nil
}

// Seen reports whether a key is currently claimed.
func (d *MemoryDeduper) Seen(_ context.Context, key string) (bool, error) {
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	exp, ok := d.seen[key]
	return ok && exp.After(now), nil
}

func (d *MemoryDeduper) evictLocked(now time.Time) {
	for k, exp := range d.seen {
		if !exp.After(now) {
			delete(d.seen, k)
		}
	}
	if len(d.seen) < d.maxEntries {
		return
	}
	// Still full after expiry sweep: drop an arbitrary tenth. Bounded memory
	// beats perfect dedup history, and the durable unique indexes remain.
	drop := len(d.seen) / 10
	for k := range d.seen {
		if drop <= 0 {
			break
		}
		delete(d.seen, k)
		drop--
	}
}

// EffectKey builds a deterministic idempotency key from ordered parts.
// Callers must include everything that distinguishes one effect from another
// and nothing that varies between redeliveries of the same effect.
func EffectKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:16])
}

// BucketTime rounds a timestamp down to a bucket, so that "the same alert
// condition within the same minute" collapses to one effect.
func BucketTime(t time.Time, bucket time.Duration) string {
	if bucket <= 0 {
		bucket = time.Minute
	}
	return t.UTC().Truncate(bucket).Format(time.RFC3339)
}

// IdempotentOptions parameterise the wrapper.
type IdempotentOptions struct {
	Group   string
	TTL     time.Duration
	Metrics *obs.Metrics
	Topic   string
}

// Idempotent wraps a handler so that an event is processed at most once per
// consumer group, keyed by event id.
//
// The claim happens before the handler runs and is released on handler failure,
// so a transient error does not permanently swallow an event.
func Idempotent(d Deduper, opts IdempotentOptions, h Handler) Handler {
	if d == nil {
		return h
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 48 * time.Hour
	}
	return func(ctx context.Context, e Envelope) error {
		key := EffectKey("consume", opts.Group, e.EventID)
		first, err := d.Mark(ctx, key, ttl)
		if err != nil {
			// A dedup-store failure must not stop processing: at-least-once is
			// the safe direction, and durable unique indexes protect the two
			// effects that truly cannot duplicate.
			return h(ctx, e)
		}
		if !first {
			if opts.Metrics != nil {
				opts.Metrics.BusDuplicates.Inc(opts.Topic, opts.Group)
			}
			return nil
		}
		if err := h(ctx, e); err != nil {
			if md, ok := d.(*MemoryDeduper); ok {
				md.release(key)
			}
			return err
		}
		return nil
	}
}

func (d *MemoryDeduper) release(key string) {
	d.mu.Lock()
	delete(d.seen, key)
	d.mu.Unlock()
}

// MaxAgeFilter drops events older than max, converting consumer lag into
// silence rather than into stale action (ADR-002 failure modes). Dropped events
// are still passed to onDropped for evaluation purposes.
func MaxAgeFilter(clock obs.Clock, max time.Duration, onDropped func(Envelope), h Handler) Handler {
	if max <= 0 {
		return h
	}
	if clock == nil {
		clock = obs.SystemClock{}
	}
	return func(ctx context.Context, e Envelope) error {
		if e.Age(clock.Now()) > max {
			if onDropped != nil {
				onDropped(e)
			}
			return nil
		}
		return h(ctx, e)
	}
}

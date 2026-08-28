package marketdata

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Rejection records why a datum was quarantined.
type Rejection struct {
	Ticker   domain.Ticker `json:"ticker"`
	Kind     string        `json:"kind"` // quote|candle|trade
	Reason   string        `json:"reason"`
	Detail   string        `json:"detail"`
	At       time.Time     `json:"at"`
	Provider string        `json:"provider"`
}

// ValidatorConfig parameterises semantic validation.
type ValidatorConfig struct {
	// MaxGapPct rejects a print that moves more than this fraction from the
	// last accepted price. A genuine limit-up move is ~0.20; the default 0.25
	// therefore catches bad ticks without discarding real extremes.
	MaxGapPct float64
	// MaxSpreadBps rejects an implausibly wide book.
	MaxSpreadBps float64
	// StalenessSoft/Hard drive the freshness gate.
	StalenessSoft time.Duration
	StalenessHard time.Duration
	// FutureTolerance rejects timestamps too far ahead of the clock, which
	// usually indicates a vendor timezone bug.
	FutureTolerance time.Duration
	Clock           obs.Clock
	Metrics         *obs.Metrics
	OnReject        func(Rejection)
}

// Validator wraps a Provider and enforces semantic validation, which the
// provider adapters are deliberately not responsible for (ADR-008).
//
// It also owns the freshness state that gates signal generation: governance
// rule G-5 says stale data suspends new signals, and this is where "stale" is
// decided.
type Validator struct {
	Provider
	cfg ValidatorConfig

	mu    sync.RWMutex
	last  map[domain.Ticker]domain.Quote
	seq   map[domain.Ticker]uint64
	stale map[domain.Ticker]bool
}

// NewValidator wraps p.
func NewValidator(p Provider, cfg ValidatorConfig) *Validator {
	if cfg.MaxGapPct <= 0 {
		cfg.MaxGapPct = 0.25
	}
	if cfg.MaxSpreadBps <= 0 {
		cfg.MaxSpreadBps = 500
	}
	if cfg.StalenessSoft <= 0 {
		cfg.StalenessSoft = 30 * time.Second
	}
	if cfg.StalenessHard <= 0 {
		cfg.StalenessHard = 2 * time.Minute
	}
	if cfg.FutureTolerance <= 0 {
		cfg.FutureTolerance = 5 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	return &Validator{
		Provider: p,
		cfg:      cfg,
		last:     map[domain.Ticker]domain.Quote{},
		seq:      map[domain.Ticker]uint64{},
		stale:    map[domain.Ticker]bool{},
	}
}

// ValidateQuote applies structural and semantic checks. Accepted quotes update
// the freshness state.
func (v *Validator) ValidateQuote(q domain.Quote) error {
	if err := q.Valid(); err != nil {
		return v.reject(q.Ticker, "quote", "structural", err.Error(), q.Timestamp)
	}
	now := v.cfg.Clock.Now()
	if q.Timestamp.After(now.Add(v.cfg.FutureTolerance)) {
		return v.reject(q.Ticker, "quote", "future_timestamp",
			fmt.Sprintf("ts=%s now=%s", q.Timestamp.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)), q.Timestamp)
	}
	if s := q.SpreadBps(); !math.IsNaN(s) && s > v.cfg.MaxSpreadBps {
		return v.reject(q.Ticker, "quote", "spread_too_wide",
			fmt.Sprintf("%.1f bps > %.1f", s, v.cfg.MaxSpreadBps), q.Timestamp)
	}

	v.mu.Lock()
	prev, hadPrev := v.last[q.Ticker]
	prevSeq, hadSeq := v.seq[q.Ticker]
	v.mu.Unlock()

	if hadSeq && q.Seq != 0 && q.Seq <= prevSeq {
		return v.reject(q.Ticker, "quote", "duplicate_or_reordered",
			fmt.Sprintf("seq=%d <= last=%d", q.Seq, prevSeq), q.Timestamp)
	}
	if hadPrev && !q.Timestamp.After(prev.Timestamp) && q.Seq == 0 {
		return v.reject(q.Ticker, "quote", "non_monotonic_timestamp",
			fmt.Sprintf("ts=%s last=%s", q.Timestamp.Format(time.RFC3339Nano), prev.Timestamp.Format(time.RFC3339Nano)), q.Timestamp)
	}
	if hadPrev && prev.Mid() > 0 {
		gap := math.Abs(q.Mid()-prev.Mid()) / prev.Mid()
		if gap > v.cfg.MaxGapPct {
			return v.reject(q.Ticker, "quote", "price_gap",
				fmt.Sprintf("%.2f%% > %.2f%%", gap*100, v.cfg.MaxGapPct*100), q.Timestamp)
		}
	}

	v.mu.Lock()
	v.last[q.Ticker] = q
	if q.Seq != 0 {
		v.seq[q.Ticker] = q.Seq
	}
	if v.stale[q.Ticker] {
		v.stale[q.Ticker] = false
		if v.cfg.Metrics != nil {
			v.cfg.Metrics.MarketDataStale.Set(0, string(q.Ticker))
		}
	}
	v.mu.Unlock()

	if v.cfg.Metrics != nil {
		v.cfg.Metrics.MarketEventsProcessed.Inc("quote", v.Provider.Name())
	}
	return nil
}

// ValidateCandle applies structural and semantic checks to a bar.
func (v *Validator) ValidateCandle(c domain.Candle) error {
	if err := c.Valid(); err != nil {
		return v.reject(c.Ticker, "candle", "structural", err.Error(), c.Start)
	}
	v.mu.RLock()
	prev, ok := v.last[c.Ticker]
	v.mu.RUnlock()
	if ok && prev.Mid() > 0 {
		gap := math.Abs(c.Close-prev.Mid()) / prev.Mid()
		if gap > v.cfg.MaxGapPct*2 {
			return v.reject(c.Ticker, "candle", "price_gap",
				fmt.Sprintf("%.2f%% from last quote", gap*100), c.Start)
		}
	}
	if v.cfg.Metrics != nil {
		v.cfg.Metrics.MarketEventsProcessed.Inc("candle", v.Provider.Name())
	}
	return nil
}

func (v *Validator) reject(t domain.Ticker, kind, reason, detail string, at time.Time) error {
	if v.cfg.Metrics != nil {
		v.cfg.Metrics.MarketEventsRejected.Inc(kind, reason)
	}
	if v.cfg.OnReject != nil {
		v.cfg.OnReject(Rejection{
			Ticker: t, Kind: kind, Reason: reason, Detail: detail,
			At: at, Provider: v.Provider.Name(),
		})
	}
	return fmt.Errorf("%w: %s %s: %s", ErrInvalidData, kind, reason, detail)
}

// Freshness describes how current a symbol's data is.
type Freshness struct {
	Ticker     domain.Ticker
	LastTickAt time.Time
	Age        time.Duration
	Soft       bool // degrade: signals may not be created, existing ones stand
	Hard       bool // suspend: treat the symbol as offline
	Reason     string
}

// Stale reports whether the symbol has crossed the soft threshold.
func (f Freshness) Stale() bool { return f.Soft || f.Hard }

// Freshness evaluates the staleness gate for one symbol.
func (v *Validator) Freshness(t domain.Ticker) Freshness {
	now := v.cfg.Clock.Now()
	v.mu.RLock()
	q, ok := v.last[t]
	v.mu.RUnlock()
	if !ok {
		return Freshness{Ticker: t, Age: 0, Hard: true, Reason: "no data received"}
	}
	age := now.Sub(q.Timestamp)
	f := Freshness{Ticker: t, LastTickAt: q.Timestamp, Age: age}
	switch {
	case age >= v.cfg.StalenessHard:
		f.Hard, f.Soft = true, true
		f.Reason = fmt.Sprintf("last tick %s ago exceeds hard threshold %s", age.Truncate(time.Second), v.cfg.StalenessHard)
	case age >= v.cfg.StalenessSoft:
		f.Soft = true
		f.Reason = fmt.Sprintf("last tick %s ago exceeds soft threshold %s", age.Truncate(time.Second), v.cfg.StalenessSoft)
	}
	if f.Stale() {
		v.mu.Lock()
		if !v.stale[t] {
			v.stale[t] = true
			if v.cfg.Metrics != nil {
				v.cfg.Metrics.MarketDataStale.Set(1, string(t))
			}
		}
		v.mu.Unlock()
	}
	return f
}

// StaleSymbols returns every symbol currently marked stale.
func (v *Validator) StaleSymbols() []domain.Ticker {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := []domain.Ticker{}
	for t, s := range v.stale {
		if s {
			out = append(out, t)
		}
	}
	return out
}

// Last returns the last accepted quote for a symbol.
func (v *Validator) Last(t domain.Ticker) (domain.Quote, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	q, ok := v.last[t]
	return q, ok
}

// GetQuotes validates before returning, dropping bad prints rather than
// propagating them.
func (v *Validator) GetQuotes(ctx context.Context, tickers []domain.Ticker) ([]domain.Quote, error) {
	qs, err := v.Provider.GetQuotes(ctx, tickers)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Quote, 0, len(qs))
	for _, q := range qs {
		if err := v.ValidateQuote(q); err != nil {
			continue
		}
		out = append(out, q)
	}
	return out, nil
}

package marketdata

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// CircuitState is the state of a per-provider breaker.
type CircuitState int

const (
	CircuitClosed CircuitState = iota
	CircuitHalfOpen
	CircuitOpen
)

func (s CircuitState) String() string {
	switch s {
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// Breaker is a simple failure-count circuit breaker with a cooldown and a
// half-open probe. It is deliberately not adaptive: predictable behaviour
// during an incident is worth more than optimal throughput.
type Breaker struct {
	name      string
	threshold int
	cooldown  time.Duration
	clock     obs.Clock
	metrics   *obs.Metrics

	mu        sync.Mutex
	failures  int
	state     CircuitState
	openedAt  time.Time
	lastError error
	lastOK    time.Time
	probing   bool
}

// NewBreaker builds a breaker.
func NewBreaker(name string, threshold int, cooldown time.Duration, clock obs.Clock, m *obs.Metrics) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if clock == nil {
		clock = obs.SystemClock{}
	}
	return &Breaker{name: name, threshold: threshold, cooldown: cooldown, clock: clock, metrics: m}
}

// Allow reports whether a call may proceed, transitioning to half-open when the
// cooldown has elapsed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if b.clock.Now().Sub(b.openedAt) >= b.cooldown {
			b.state = CircuitHalfOpen
			b.probing = false
			b.setMetric()
			return true
		}
		return false
	default: // half-open: admit a single probe at a time
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

// Success records a successful call.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = CircuitClosed
	b.probing = false
	b.lastOK = b.clock.Now()
	b.setMetric()
}

// Failure records a failed call and may open the circuit.
func (b *Breaker) Failure(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastError = err
	b.probing = false
	b.failures++
	if b.state == CircuitHalfOpen || b.failures >= b.threshold {
		b.state = CircuitOpen
		b.openedAt = b.clock.Now()
	}
	b.setMetric()
}

func (b *Breaker) setMetric() {
	if b.metrics != nil {
		b.metrics.CircuitState.Set(float64(b.state), b.name)
	}
}

// State returns the current breaker state.
func (b *Breaker) State() CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Health snapshots the breaker.
func (b *Breaker) Health() Health {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := Health{
		Name: b.name, Healthy: b.state == CircuitClosed, Failures: b.failures,
		LastSuccess: b.lastOK, CircuitOpen: b.state == CircuitOpen,
	}
	if b.lastError != nil {
		h.LastError = b.lastError.Error()
	}
	return h
}

// Composite is an ordered failover provider. Providers are tried in order,
// skipping those whose breaker is open.
//
// Adjustment policy is enforced, not assumed: failing over from an adjusted
// series to an unadjusted one would silently corrupt every indicator, so a
// capability mismatch is an error rather than a fallback.
type Composite struct {
	providers []Provider
	breakers  []*Breaker
	clock     obs.Clock
	metrics   *obs.Metrics
	timeout   time.Duration
}

// NewComposite builds a failover chain.
func NewComposite(providers []Provider, threshold int, cooldown, timeout time.Duration, clock obs.Clock, m *obs.Metrics) (*Composite, error) {
	if len(providers) == 0 {
		return nil, errors.New("marketdata: composite requires at least one provider")
	}
	adjusted := providers[0].Capabilities().Adjusted
	for _, p := range providers[1:] {
		if p.Capabilities().Adjusted != adjusted {
			return nil, fmt.Errorf("marketdata: provider %q adjustment policy differs from %q; refusing to mix adjusted and unadjusted history",
				p.Name(), providers[0].Name())
		}
	}
	if clock == nil {
		clock = obs.SystemClock{}
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	c := &Composite{providers: providers, clock: clock, metrics: m, timeout: timeout}
	for _, p := range providers {
		c.breakers = append(c.breakers, NewBreaker("marketdata."+p.Name(), threshold, cooldown, clock, m))
	}
	return c, nil
}

// Name implements Provider.
func (c *Composite) Name() string { return "composite" }

// Capabilities returns the intersection of member capabilities, so a caller
// cannot be promised something a failover target cannot deliver.
func (c *Composite) Capabilities() Capabilities {
	out := c.providers[0].Capabilities()
	for _, p := range c.providers[1:] {
		pc := p.Capabilities()
		out.Quotes = out.Quotes && pc.Quotes
		out.Trades = out.Trades && pc.Trades
		out.Streaming = out.Streaming && pc.Streaming
		out.CorporateEvents = out.CorporateEvents && pc.CorporateEvents
		out.Simulated = out.Simulated || pc.Simulated
		if pc.MaxHistory < out.MaxHistory {
			out.MaxHistory = pc.MaxHistory
		}
		var common []domain.Interval
		for _, i := range out.Intervals {
			if pc.Supports(i) {
				common = append(common, i)
			}
		}
		out.Intervals = common
	}
	return out
}

// do runs fn against each provider in order until one succeeds.
func (c *Composite) do(ctx context.Context, op string, fn func(context.Context, Provider) error) error {
	var lastErr error
	attempted := 0
	for i, p := range c.providers {
		b := c.breakers[i]
		if !b.Allow() {
			continue
		}
		attempted++
		cctx, cancel := context.WithTimeout(ctx, c.timeout)
		err := fn(cctx, p)
		cancel()
		if err == nil {
			b.Success()
			return nil
		}
		// A capability gap is not a provider failure; do not trip the breaker.
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrNotFound) {
			lastErr = err
			continue
		}
		b.Failure(err)
		lastErr = fmt.Errorf("%s via %s: %w", op, p.Name(), err)
	}
	if attempted == 0 {
		return fmt.Errorf("%w: all providers circuit-open", ErrUnavailable)
	}
	if lastErr == nil {
		lastErr = ErrUnavailable
	}
	return lastErr
}

// GetQuotes implements Provider with failover.
func (c *Composite) GetQuotes(ctx context.Context, tickers []domain.Ticker) ([]domain.Quote, error) {
	var out []domain.Quote
	err := c.do(ctx, "GetQuotes", func(ctx context.Context, p Provider) error {
		q, err := p.GetQuotes(ctx, tickers)
		if err != nil {
			return err
		}
		out = q
		return nil
	})
	return out, err
}

// GetHistoricalBars implements Provider with failover.
func (c *Composite) GetHistoricalBars(ctx context.Context, req BarRequest) ([]domain.Candle, error) {
	var out []domain.Candle
	err := c.do(ctx, "GetHistoricalBars", func(ctx context.Context, p Provider) error {
		if !p.Capabilities().Supports(req.Interval) {
			return ErrUnsupported
		}
		b, err := p.GetHistoricalBars(ctx, req)
		if err != nil {
			return err
		}
		out = b
		return nil
	})
	return out, err
}

// GetTrades implements Provider with failover.
func (c *Composite) GetTrades(ctx context.Context, req TradeRequest) ([]domain.Trade, error) {
	var out []domain.Trade
	err := c.do(ctx, "GetTrades", func(ctx context.Context, p Provider) error {
		if !p.Capabilities().Trades {
			return ErrUnsupported
		}
		t, err := p.GetTrades(ctx, req)
		if err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

// GetMarketStatus implements Provider with failover.
func (c *Composite) GetMarketStatus(ctx context.Context) (domain.MarketStatus, error) {
	var out domain.MarketStatus
	err := c.do(ctx, "GetMarketStatus", func(ctx context.Context, p Provider) error {
		s, err := p.GetMarketStatus(ctx)
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// GetCorporateEvents implements Provider with failover.
func (c *Composite) GetCorporateEvents(ctx context.Context, req EventRequest) ([]domain.CorporateEvent, error) {
	var out []domain.CorporateEvent
	err := c.do(ctx, "GetCorporateEvents", func(ctx context.Context, p Provider) error {
		if !p.Capabilities().CorporateEvents {
			return ErrUnsupported
		}
		e, err := p.GetCorporateEvents(ctx, req)
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	return out, err
}

// Health reports each member's state, which the /health endpoint surfaces.
func (c *Composite) Health() []Health {
	out := make([]Health, 0, len(c.breakers))
	for i, b := range c.breakers {
		h := b.Health()
		h.Name = c.providers[i].Name()
		out = append(out, h)
	}
	return out
}

// RateLimited wraps a provider with a token bucket, so a vendor's quota is
// respected by construction rather than by hoping.
type RateLimited struct {
	Provider
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64
	last     time.Time
	clock    obs.Clock
}

// NewRateLimited wraps p with a token bucket of the given rate and burst.
func NewRateLimited(p Provider, perSecond float64, burst int, clock obs.Clock) *RateLimited {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	if burst <= 0 {
		burst = 1
	}
	return &RateLimited{
		Provider: p, tokens: float64(burst), capacity: float64(burst),
		rate: perSecond, last: clock.Now(), clock: clock,
	}
}

func (r *RateLimited) take() error {
	if r.rate <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.rate
	if r.tokens > r.capacity {
		r.tokens = r.capacity
	}
	r.last = now
	if r.tokens < 1 {
		return ErrRateLimited
	}
	r.tokens--
	return nil
}

// GetQuotes implements Provider with rate limiting.
func (r *RateLimited) GetQuotes(ctx context.Context, t []domain.Ticker) ([]domain.Quote, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.Provider.GetQuotes(ctx, t)
}

// GetHistoricalBars implements Provider with rate limiting.
func (r *RateLimited) GetHistoricalBars(ctx context.Context, req BarRequest) ([]domain.Candle, error) {
	if err := r.take(); err != nil {
		return nil, err
	}
	return r.Provider.GetHistoricalBars(ctx, req)
}

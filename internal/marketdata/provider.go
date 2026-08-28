// Package marketdata defines the vendor-neutral market data abstraction
// described in ADR-008 and the implementations QuantOS ships: a deterministic
// simulator, a historical replayer, and decorators for validation, caching,
// rate limiting and failover.
package marketdata

import (
	"context"
	"errors"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Sentinel errors. Callers distinguish "this provider cannot do that" from
// "this provider failed", because the two have different remedies.
var (
	ErrUnsupported = errors.New("marketdata: capability not supported by provider")
	ErrNotFound    = errors.New("marketdata: symbol not found")
	ErrRateLimited = errors.New("marketdata: rate limited")
	ErrUnavailable = errors.New("marketdata: provider unavailable")
	ErrStale       = errors.New("marketdata: data is stale")
	ErrInvalidData = errors.New("marketdata: data failed validation")
)

// Capabilities declares what a provider can actually do. Callers negotiate
// against this rather than assuming, so an unsupported request returns
// ErrUnsupported instead of silently wrong data.
type Capabilities struct {
	Intervals       []domain.Interval
	MaxHistory      time.Duration
	Quotes          bool
	Trades          bool
	Streaming       bool
	CorporateEvents bool
	Options         bool
	Fundamentals    bool
	// Adjusted reports whether historical prices are split/dividend adjusted.
	// Mixing adjusted and unadjusted series across a failover is refused.
	Adjusted        bool
	RateLimitPerSec float64
	Delayed         time.Duration
	Simulated       bool
}

// Supports reports whether an interval is available.
func (c Capabilities) Supports(i domain.Interval) bool {
	for _, x := range c.Intervals {
		if x == i {
			return true
		}
	}
	return false
}

// BarRequest asks for historical candles.
type BarRequest struct {
	Tickers  []domain.Ticker
	Interval domain.Interval
	Start    time.Time
	End      time.Time
	Limit    int
	Adjusted bool
}

// TradeRequest asks for trade prints.
type TradeRequest struct {
	Ticker domain.Ticker
	Start  time.Time
	End    time.Time
	Limit  int
}

// EventRequest asks for corporate events.
type EventRequest struct {
	Tickers []domain.Ticker
	Start   time.Time
	End     time.Time
	Types   []domain.CorporateEventType
}

// Provider is the market data interface every adapter implements.
type Provider interface {
	Name() string
	Capabilities() Capabilities
	GetQuotes(ctx context.Context, tickers []domain.Ticker) ([]domain.Quote, error)
	GetHistoricalBars(ctx context.Context, req BarRequest) ([]domain.Candle, error)
	GetTrades(ctx context.Context, req TradeRequest) ([]domain.Trade, error)
	GetMarketStatus(ctx context.Context) (domain.MarketStatus, error)
	GetCorporateEvents(ctx context.Context, req EventRequest) ([]domain.CorporateEvent, error)
}

// StreamEvent is one item from a push feed.
type StreamEvent struct {
	Quote  *domain.Quote
	Trade  *domain.Trade
	Candle *domain.Candle
}

// StreamProvider is implemented by providers with a push feed. It is optional
// so that a polling-only vendor is not forced to fake one.
type StreamProvider interface {
	Provider
	Stream(ctx context.Context, tickers []domain.Ticker) (<-chan StreamEvent, error)
}

// Healther is implemented by providers that can report their own health, which
// the composite provider uses for failover scoring.
type Healther interface {
	Healthy(ctx context.Context) bool
}

// FundamentalsProvider is an optional capability.
type FundamentalsProvider interface {
	GetFundamentals(ctx context.Context, ticker domain.Ticker, limit int) ([]domain.Fundamental, error)
}

// OptionsProvider is an optional capability.
type OptionsProvider interface {
	GetOptionsMetrics(ctx context.Context, ticker domain.Ticker) (domain.OptionsMetrics, error)
}

// Health describes one provider's observed state, used by the composite.
type Health struct {
	Name        string        `json:"name"`
	Healthy     bool          `json:"healthy"`
	Failures    int           `json:"failures"`
	LastError   string        `json:"last_error,omitempty"`
	LastSuccess time.Time     `json:"last_success"`
	Latency     time.Duration `json:"latency"`
	CircuitOpen bool          `json:"circuit_open"`
}

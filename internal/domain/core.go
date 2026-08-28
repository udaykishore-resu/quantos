// Package domain holds the canonical QuantOS data model.
//
// Every type in this package is a value type with no behaviour that depends on
// infrastructure. Engines consume and produce these types; adapters translate
// vendor payloads into them. domain imports nothing else from this repository,
// which keeps the dependency graph a DAG (see ADR-011).
package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Side describes a directional intent. It is deliberately not called
// "recommendation": QuantOS emits research classifications, never advice.
type Side string

const (
	SideLong  Side = "LONG"
	SideShort Side = "SHORT"
	SideFlat  Side = "FLAT"
)

// Ticker is a normalised instrument symbol. Adapters must normalise vendor
// symbology into this form (dot-separated share classes, upper case, no spaces).
type Ticker string

// NormalizeTicker canonicalises the common vendor spellings of a symbol.
func NormalizeTicker(s string) Ticker {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.ReplaceAll(s, "-", ".")
	s = strings.ReplaceAll(s, "/", ".")
	return Ticker(s)
}

func (t Ticker) String() string { return string(t) }

// Exchange identifies a listing venue.
type Exchange string

const (
	ExchangeNYSE   Exchange = "NYSE"
	ExchangeNASDAQ Exchange = "NASDAQ"
	ExchangeAMEX   Exchange = "AMEX"
	ExchangeARCA   Exchange = "ARCA"
	ExchangeOther  Exchange = "OTHER"
)

// AssetClass separates equities from the ETFs and indices used as market context.
type AssetClass string

const (
	AssetEquity    AssetClass = "EQUITY"
	AssetETF       AssetClass = "ETF"
	AssetIndex     AssetClass = "INDEX"
	AssetFuture    AssetClass = "FUTURE"
	AssetFX        AssetClass = "FX"
	AssetRate      AssetClass = "RATE"
	AssetCommodity AssetClass = "COMMODITY"
)

// Stock is a point-in-time description of an instrument in the universe.
//
// ListedAt/DelistedAt exist so that historical universes are point-in-time and
// backtests do not suffer survivorship bias (ADR-007).
type Stock struct {
	Ticker     Ticker     `json:"ticker"`
	Company    string     `json:"company"`
	Sector     string     `json:"sector"`
	Industry   string     `json:"industry"`
	Exchange   Exchange   `json:"exchange"`
	AssetClass AssetClass `json:"asset_class"`
	Currency   string     `json:"currency"`
	MarketCap  float64    `json:"market_cap"`
	SharesOut  float64    `json:"shares_outstanding"`
	AvgVolume  float64    `json:"avg_volume_30d"`
	// ReferencePrice is a nominal price level used only by the simulator to
	// start an instrument at a recognisable value. It is fixture data and is
	// never presented as a quote.
	ReferencePrice float64    `json:"reference_price,omitempty"`
	AvgNotional    float64    `json:"avg_notional_30d"`
	Beta           float64    `json:"beta"`
	ListedAt       time.Time  `json:"listed_at"`
	DelistedAt     *time.Time `json:"delisted_at,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
}

// Active reports whether the instrument was tradable on the given date.
func (s Stock) Active(at time.Time) bool {
	if !s.ListedAt.IsZero() && at.Before(s.ListedAt) {
		return false
	}
	if s.DelistedAt != nil && !at.Before(*s.DelistedAt) {
		return false
	}
	return true
}

// LiquidityScore maps average notional traded to a bounded 0..1 score.
// It is deliberately logarithmic: the difference between $1M and $10M a day
// matters far more than between $1B and $10B.
func (s Stock) LiquidityScore() float64 {
	if s.AvgNotional <= 0 {
		return 0
	}
	// $100k -> ~0, $1bn -> ~1
	v := (math.Log10(s.AvgNotional) - 5) / 4
	return clamp01(v)
}

// Quote is a top-of-book snapshot.
type Quote struct {
	Ticker    Ticker    `json:"ticker"`
	Bid       float64   `json:"bid"`
	Ask       float64   `json:"ask"`
	BidSize   float64   `json:"bid_size"`
	AskSize   float64   `json:"ask_size"`
	Last      float64   `json:"last"`
	LastSize  float64   `json:"last_size"`
	Volume    float64   `json:"volume"`
	Timestamp time.Time `json:"ts"`
	Seq       uint64    `json:"seq"`
	Source    string    `json:"source"`
}

// Mid returns the mid price, falling back to last when the book is one-sided.
func (q Quote) Mid() float64 {
	if q.Bid > 0 && q.Ask > 0 {
		return (q.Bid + q.Ask) / 2
	}
	return q.Last
}

// SpreadBps returns the quoted spread in basis points of the mid.
func (q Quote) SpreadBps() float64 {
	m := q.Mid()
	if m <= 0 || q.Bid <= 0 || q.Ask <= 0 {
		return math.NaN()
	}
	return (q.Ask - q.Bid) / m * 10000
}

// Valid performs structural validation. Semantic validation (gaps, staleness)
// belongs to marketdata.Validator, which needs prior state.
func (q Quote) Valid() error {
	switch {
	case q.Ticker == "":
		return fmt.Errorf("quote: empty ticker")
	case q.Timestamp.IsZero():
		return fmt.Errorf("quote %s: zero timestamp", q.Ticker)
	case q.Bid < 0 || q.Ask < 0 || q.Last < 0:
		return fmt.Errorf("quote %s: negative price", q.Ticker)
	case q.Bid > 0 && q.Ask > 0 && q.Bid > q.Ask:
		return fmt.Errorf("quote %s: crossed book bid=%.4f ask=%.4f", q.Ticker, q.Bid, q.Ask)
	case q.Last == 0 && q.Bid == 0 && q.Ask == 0:
		return fmt.Errorf("quote %s: no price", q.Ticker)
	case q.Bid > 0 && q.Ask > 0 && (q.BidSize <= 0 || q.AskSize <= 0):
		// A two-sided book with no size on one side is not a market you could
		// trade against, and the spread it implies is fiction. The risk
		// engine's spread check reads that number, so accepting it here means
		// a zero-size book passes a liquidity test it never met.
		return fmt.Errorf("quote %s: quoted %.4f x %.4f with no size on one or both sides",
			q.Ticker, q.Bid, q.Ask)
	}
	return nil
}

// Trade is a single execution print.
type Trade struct {
	Ticker    Ticker    `json:"ticker"`
	Price     float64   `json:"price"`
	Size      float64   `json:"size"`
	Timestamp time.Time `json:"ts"`
	Seq       uint64    `json:"seq"`
	Aggressor Side      `json:"aggressor,omitempty"`
	Source    string    `json:"source"`
}

// Interval is a bar duration.
type Interval string

const (
	Interval1m  Interval = "1m"
	Interval5m  Interval = "5m"
	Interval15m Interval = "15m"
	Interval1h  Interval = "1h"
	Interval1d  Interval = "1d"
)

// Duration converts an Interval to a time.Duration.
func (i Interval) Duration() time.Duration {
	switch i {
	case Interval1m:
		return time.Minute
	case Interval5m:
		return 5 * time.Minute
	case Interval15m:
		return 15 * time.Minute
	case Interval1h:
		return time.Hour
	case Interval1d:
		return 24 * time.Hour
	default:
		return time.Minute
	}
}

// Candle is an OHLCV bar. Adjusted records whether prices are split/dividend
// adjusted; mixing adjusted and unadjusted series is refused upstream.
type Candle struct {
	Ticker   Ticker    `json:"ticker"`
	Interval Interval  `json:"interval"`
	Open     float64   `json:"open"`
	High     float64   `json:"high"`
	Low      float64   `json:"low"`
	Close    float64   `json:"close"`
	Volume   float64   `json:"volume"`
	VWAP     float64   `json:"vwap"`
	Trades   int64     `json:"trades"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Adjusted bool      `json:"adjusted"`
	Source   string    `json:"source"`
	Partial  bool      `json:"partial"`
	Sequence uint64    `json:"seq"`
}

// TypicalPrice is (H+L+C)/3, the standard VWAP input.
func (c Candle) TypicalPrice() float64 { return (c.High + c.Low + c.Close) / 3 }

// Range is the bar's high-low range.
func (c Candle) Range() float64 { return c.High - c.Low }

// Valid performs structural validation of a bar.
func (c Candle) Valid() error {
	switch {
	case c.Ticker == "":
		return fmt.Errorf("candle: empty ticker")
	case c.Open <= 0 || c.High <= 0 || c.Low <= 0 || c.Close <= 0:
		return fmt.Errorf("candle %s: non-positive price", c.Ticker)
	case c.High < c.Low:
		return fmt.Errorf("candle %s: high %.4f < low %.4f", c.Ticker, c.High, c.Low)
	case c.High < c.Open || c.High < c.Close:
		return fmt.Errorf("candle %s: high below open/close", c.Ticker)
	case c.Low > c.Open || c.Low > c.Close:
		return fmt.Errorf("candle %s: low above open/close", c.Ticker)
	case c.Volume < 0:
		return fmt.Errorf("candle %s: negative volume", c.Ticker)
	case c.End.Before(c.Start):
		return fmt.Errorf("candle %s: end before start", c.Ticker)
	}
	return nil
}

// MarketSession enumerates the trading session.
type MarketSession string

const (
	SessionClosed     MarketSession = "CLOSED"
	SessionPreMarket  MarketSession = "PRE_MARKET"
	SessionOpen       MarketSession = "OPEN"
	SessionAfterHours MarketSession = "AFTER_HOURS"
	SessionHoliday    MarketSession = "HOLIDAY"
)

// MarketStatus describes venue state at a moment in time.
type MarketStatus struct {
	Session   MarketSession `json:"session"`
	Timestamp time.Time     `json:"ts"`
	NextOpen  time.Time     `json:"next_open"`
	NextClose time.Time     `json:"next_close"`
	Venue     string        `json:"venue"`
}

// CorporateEventType enumerates scheduled and announced corporate actions.
type CorporateEventType string

const (
	EventEarnings  CorporateEventType = "EARNINGS"
	EventDividend  CorporateEventType = "DIVIDEND"
	EventSplit     CorporateEventType = "SPLIT"
	EventGuidance  CorporateEventType = "GUIDANCE"
	EventMerger    CorporateEventType = "MERGER"
	EventOffering  CorporateEventType = "OFFERING"
	EventIndexAdd  CorporateEventType = "INDEX_ADD"
	EventIndexDrop CorporateEventType = "INDEX_DROP"
	EventMacro     CorporateEventType = "MACRO"
)

// CorporateEvent is a scheduled or announced event with a known timestamp.
// Confirmed distinguishes an exchange-confirmed date from an estimate, which
// matters to the risk engine's earnings-proximity check.
type CorporateEvent struct {
	ID          string             `json:"id"`
	Ticker      Ticker             `json:"ticker"`
	Type        CorporateEventType `json:"type"`
	ScheduledAt time.Time          `json:"scheduled_at"`
	Confirmed   bool               `json:"confirmed"`
	Detail      string             `json:"detail,omitempty"`
	Source      string             `json:"source"`
}

// Within reports whether the event falls inside [now, now+d).
func (e CorporateEvent) Within(now time.Time, d time.Duration) bool {
	return !e.ScheduledAt.Before(now) && e.ScheduledAt.Before(now.Add(d))
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Clamp01 exposes the bounded clamp used throughout the scoring engines.
func Clamp01(v float64) float64 { return clamp01(v) }

// Clamp bounds v to [lo, hi].
func Clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// AuditEvent is one recorded state-changing action.
//
// It lives in domain rather than in the storage layer because the middleware
// that produces audit records must not depend on where they are kept: an
// HTTP layer that imports a database package cannot be run without one.
type AuditEvent struct {
	ID        string            `json:"id"`
	At        time.Time         `json:"at"`
	Principal string            `json:"principal"`
	Action    string            `json:"action"`
	Resource  string            `json:"resource"`
	Outcome   string            `json:"outcome"`
	RequestID string            `json:"request_id,omitempty"`
	TraceID   string            `json:"trace_id,omitempty"`
	Detail    map[string]string `json:"detail,omitempty"`
	RemoteIP  string            `json:"remote_ip,omitempty"`
}

package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Alpaca is a live Provider backed by Alpaca's market-data API.
//
// It is a polling adapter: the free plan allows 200 requests a minute, which is
// plenty for a latest-quote snapshot of the whole universe every few seconds
// plus one bar request a minute, and a polling adapter has no reconnect state
// to get wrong. The ingest loop in internal/app drives it.
//
// Two limits are worth knowing before reading anything it produces:
//
//   - The free "iex" feed is the IEX exchange only — a few percent of US
//     volume. Prices track the consolidated market closely for liquid names;
//     volumes, and therefore volume-based features, do not. The "sip" feed
//     (paid) is the consolidated tape.
//   - Indices are not equities. VIX and DXY have no Alpaca stock data, so they
//     are excluded by default and the regime engine runs without implied
//     volatility, which it supports.
//
// It only ever issues GET requests: it reads market data, and it reads the
// market clock from the trading API, which needs no trading permission.
type Alpaca struct {
	keyID, secret string
	dataURL       string
	tradingURL    string
	feed          string
	client        *http.Client
	exclude       map[domain.Ticker]bool

	mu       sync.Mutex
	lastErr  error
	lastOK   time.Time
	failures int
}

// AlpacaConfig parameterises the adapter.
type AlpacaConfig struct {
	KeyID     string
	SecretKey string
	// Feed is "iex" (free) or "sip" (paid).
	Feed string
	// DataURL and TradingURL default to Alpaca's hosts; tests point them at a
	// loopback server. TradingURL is used only for the market clock.
	DataURL    string
	TradingURL string
	// Exclude lists symbols Alpaca has no stock data for.
	Exclude []domain.Ticker
	Timeout time.Duration
}

// Default Alpaca hosts.
const (
	AlpacaDataURL    = "https://data.alpaca.markets"
	AlpacaTradingURL = "https://paper-api.alpaca.markets"
)

// DefaultAlpacaExclude are universe symbols that are indices, not equities.
var DefaultAlpacaExclude = []domain.Ticker{"VIX", "DXY"}

// NewAlpaca builds the adapter.
func NewAlpaca(cfg AlpacaConfig) (*Alpaca, error) {
	if cfg.KeyID == "" || cfg.SecretKey == "" {
		return nil, errors.New("marketdata: alpaca key id and secret are required")
	}
	if cfg.DataURL == "" {
		cfg.DataURL = AlpacaDataURL
	}
	if cfg.TradingURL == "" {
		cfg.TradingURL = AlpacaTradingURL
	}
	for _, u := range []string{cfg.DataURL, cfg.TradingURL} {
		if err := checkAlpacaHost(u); err != nil {
			return nil, err
		}
	}
	switch cfg.Feed {
	case "":
		cfg.Feed = "iex"
	case "iex", "sip":
	default:
		return nil, fmt.Errorf("marketdata: alpaca feed %q is not iex or sip", cfg.Feed)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Exclude == nil {
		cfg.Exclude = DefaultAlpacaExclude
	}
	ex := map[domain.Ticker]bool{}
	for _, t := range cfg.Exclude {
		ex[domain.NormalizeTicker(string(t))] = true
	}
	return &Alpaca{
		keyID: cfg.KeyID, secret: cfg.SecretKey, feed: cfg.Feed,
		dataURL:    strings.TrimRight(cfg.DataURL, "/"),
		tradingURL: strings.TrimRight(cfg.TradingURL, "/"),
		client:     &http.Client{Timeout: cfg.Timeout},
		exclude:    ex,
	}, nil
}

// checkAlpacaHost allows Alpaca's own hosts over HTTPS, and loopback for tests.
func checkAlpacaHost(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("marketdata: alpaca url: %w", err)
	}
	h := u.Hostname()
	if h == "localhost" {
		return nil
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return nil
	}
	if u.Scheme == "https" && (h == "data.alpaca.markets" || h == "paper-api.alpaca.markets") {
		return nil
	}
	return fmt.Errorf("marketdata: refusing alpaca url %q", raw)
}

// Name implements Provider.
func (a *Alpaca) Name() string { return "alpaca-" + a.feed }

// Capabilities implements Provider.
func (a *Alpaca) Capabilities() Capabilities {
	return Capabilities{
		Intervals:       []domain.Interval{domain.Interval1m, domain.Interval5m, domain.Interval15m, domain.Interval1h, domain.Interval1d},
		MaxHistory:      5 * 365 * 24 * time.Hour,
		Quotes:          true,
		Adjusted:        false,
		RateLimitPerSec: 200.0 / 60,
	}
}

// Excluded reports whether a symbol is skipped because Alpaca has no stock
// data for it.
func (a *Alpaca) Excluded(t domain.Ticker) bool { return a.exclude[t] }

func (a *Alpaca) record(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.lastOK, a.failures, a.lastErr = time.Now(), 0, nil
		return
	}
	a.failures++
	a.lastErr = err
}

// Healthy implements Healther: healthy until three requests in a row fail.
func (a *Alpaca) Healthy(context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failures < 3
}

func (a *Alpaca) get(ctx context.Context, base, path string, q url.Values, out any) (err error) {
	defer func() { a.record(err) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", a.keyID)
	req.Header.Set("APCA-API-SECRET-KEY", a.secret)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: alpaca %s: %v", ErrUnavailable, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%w: alpaca read: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		var body struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &body) == nil && body.Message != "" {
			msg = body.Message
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return fmt.Errorf("%w: alpaca: %s", ErrRateLimited, msg)
		case resp.StatusCode == http.StatusNotFound || strings.Contains(strings.ToLower(msg), "invalid symbol"):
			return fmt.Errorf("%w: alpaca: %s", ErrNotFound, msg)
		default:
			return fmt.Errorf("%w: alpaca HTTP %d: %s", ErrUnavailable, resp.StatusCode, msg)
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: alpaca decode %s: %v", ErrInvalidData, path, err)
	}
	return nil
}

// symbols returns the supported, de-duplicated symbols in batches Alpaca's
// query string comfortably holds.
func (a *Alpaca) symbols(ts []domain.Ticker) [][]string {
	seen := map[domain.Ticker]bool{}
	var all []string
	for _, t := range ts {
		t = domain.NormalizeTicker(string(t))
		if t == "" || a.exclude[t] || seen[t] {
			continue
		}
		seen[t] = true
		all = append(all, string(t))
	}
	sort.Strings(all)
	var out [][]string
	for len(all) > 0 {
		n := min(len(all), 100)
		out = append(out, all[:n])
		all = all[n:]
	}
	return out
}

type alpacaQuote struct {
	Ask     float64   `json:"ap"`
	AskSize float64   `json:"as"`
	Bid     float64   `json:"bp"`
	BidSize float64   `json:"bs"`
	T       time.Time `json:"t"`
}

type alpacaTrade struct {
	Price float64   `json:"p"`
	Size  float64   `json:"s"`
	T     time.Time `json:"t"`
}

// GetQuotes implements Provider: the latest quote per symbol, with the latest
// trade folded in as Last. Symbols with no data are absent from the result.
func (a *Alpaca) GetQuotes(ctx context.Context, tickers []domain.Ticker) ([]domain.Quote, error) {
	var out []domain.Quote
	for _, batch := range a.symbols(tickers) {
		q := url.Values{"symbols": {strings.Join(batch, ",")}, "feed": {a.feed}}
		var qr struct {
			Quotes map[string]alpacaQuote `json:"quotes"`
		}
		if err := a.get(ctx, a.dataURL, "/v2/stocks/quotes/latest", q, &qr); err != nil {
			return out, err
		}
		var tr struct {
			Trades map[string]alpacaTrade `json:"trades"`
		}
		if err := a.get(ctx, a.dataURL, "/v2/stocks/trades/latest", q, &tr); err != nil {
			return out, err
		}
		for _, sym := range batch {
			qq, hasQ := qr.Quotes[sym]
			tt, hasT := tr.Trades[sym]
			if !hasQ && !hasT {
				continue
			}
			ts := qq.T
			if tt.T.After(ts) {
				ts = tt.T
			}
			out = append(out, domain.Quote{
				Ticker: domain.Ticker(sym), Bid: qq.Bid, Ask: qq.Ask,
				BidSize: qq.BidSize, AskSize: qq.AskSize,
				Last: tt.Price, LastSize: tt.Size,
				Timestamp: ts.UTC(), Source: a.Name(),
			})
		}
	}
	return out, nil
}

var alpacaTimeframes = map[domain.Interval]string{
	domain.Interval1m: "1Min", domain.Interval5m: "5Min", domain.Interval15m: "15Min",
	domain.Interval1h: "1Hour", domain.Interval1d: "1Day",
}

type alpacaBar struct {
	T      time.Time `json:"t"`
	Open   float64   `json:"o"`
	High   float64   `json:"h"`
	Low    float64   `json:"l"`
	Close  float64   `json:"c"`
	Volume float64   `json:"v"`
	Trades int64     `json:"n"`
	VWAP   float64   `json:"vw"`
}

// GetHistoricalBars implements Provider. Bars come back in time order per
// symbol; req.Limit, when set, keeps the most recent Limit bars per symbol.
func (a *Alpaca) GetHistoricalBars(ctx context.Context, req BarRequest) ([]domain.Candle, error) {
	if req.Interval == "" {
		req.Interval = domain.Interval1m
	}
	tf, ok := alpacaTimeframes[req.Interval]
	if !ok {
		return nil, fmt.Errorf("%w: interval %s", ErrUnsupported, req.Interval)
	}
	if req.End.IsZero() {
		req.End = time.Now().UTC()
	}
	adjustment := "raw"
	if req.Adjusted {
		adjustment = "all"
	}
	dur := req.Interval.Duration()
	per := map[domain.Ticker][]domain.Candle{}
	for _, batch := range a.symbols(req.Tickers) {
		token := ""
		for page := 0; page < 500; page++ {
			q := url.Values{
				"symbols": {strings.Join(batch, ",")}, "timeframe": {tf}, "feed": {a.feed},
				"start": {req.Start.UTC().Format(time.RFC3339)}, "end": {req.End.UTC().Format(time.RFC3339)},
				"limit": {"10000"}, "adjustment": {adjustment}, "sort": {"asc"},
			}
			if token != "" {
				q.Set("page_token", token)
			}
			var r struct {
				Bars          map[string][]alpacaBar `json:"bars"`
				NextPageToken *string                `json:"next_page_token"`
			}
			if err := a.get(ctx, a.dataURL, "/v2/stocks/bars", q, &r); err != nil {
				return nil, err
			}
			for sym, bars := range r.Bars {
				t := domain.Ticker(sym)
				for _, b := range bars {
					per[t] = append(per[t], domain.Candle{
						Ticker: t, Interval: req.Interval,
						Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
						Volume: b.Volume, VWAP: b.VWAP, Trades: b.Trades,
						Start: b.T.UTC(), End: b.T.UTC().Add(dur),
						Adjusted: req.Adjusted, Source: a.Name(),
					})
				}
			}
			if r.NextPageToken == nil || *r.NextPageToken == "" {
				break
			}
			token = *r.NextPageToken
		}
	}
	var out []domain.Candle
	for _, cs := range per {
		sort.Slice(cs, func(i, j int) bool { return cs[i].Start.Before(cs[j].Start) })
		if req.Limit > 0 && len(cs) > req.Limit {
			cs = cs[len(cs)-req.Limit:]
		}
		out = append(out, cs...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].End.Equal(out[j].End) {
			return out[i].End.Before(out[j].End)
		}
		return out[i].Ticker < out[j].Ticker
	})
	return out, nil
}

// GetTrades implements Provider. Not used by the platform; not implemented.
func (a *Alpaca) GetTrades(context.Context, TradeRequest) ([]domain.Trade, error) {
	return nil, ErrUnsupported
}

// GetCorporateEvents implements Provider. Alpaca's corporate-actions feed is
// not wired; the platform's earnings guard keeps using its own source.
func (a *Alpaca) GetCorporateEvents(context.Context, EventRequest) ([]domain.CorporateEvent, error) {
	return nil, ErrUnsupported
}

// GetMarketStatus implements Provider from Alpaca's market clock, which knows
// about holidays and early closes.
func (a *Alpaca) GetMarketStatus(ctx context.Context) (domain.MarketStatus, error) {
	var c struct {
		Timestamp time.Time `json:"timestamp"`
		IsOpen    bool      `json:"is_open"`
		NextOpen  time.Time `json:"next_open"`
		NextClose time.Time `json:"next_close"`
	}
	if err := a.get(ctx, a.tradingURL, "/v2/clock", url.Values{}, &c); err != nil {
		return domain.MarketStatus{}, err
	}
	s := domain.SessionClosed
	if c.IsOpen {
		s = domain.SessionOpen
	}
	return domain.MarketStatus{
		Session: s, Timestamp: c.Timestamp.UTC(), NextOpen: c.NextOpen.UTC(),
		NextClose: c.NextClose.UTC(), Venue: "US equities (" + a.Name() + ")",
	}, nil
}

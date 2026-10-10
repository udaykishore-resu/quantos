package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// defaultClient honours HTTPS_PROXY and friends through http.ProxyFromEnvironment.
func defaultClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// --- Stooq -------------------------------------------------------------------

// Stooq fetches daily history from stooq.com as CSV. Symbols are lower-case
// with a market suffix: "aapl.us", "spy.us", "^vix".
//
// As of October 2026 the site fronts the download endpoint with a JavaScript
// browser-verification page, which a plain HTTP client cannot pass; the
// fetch then fails with "unexpected response <!DOCTYPE html>". The source is
// kept because the wall has come and gone before, but it is no longer the
// default.
//
// Stooq documents its daily series as adjusted for splits and dividends. That
// claim is recorded in the manifest, not verified here.
type Stooq struct {
	BaseURL string
	Client  *http.Client
}

// NewStooq returns a Stooq source.
func NewStooq() *Stooq {
	return &Stooq{BaseURL: "https://stooq.com", Client: defaultClient(0)}
}

// Name implements Source.
func (s *Stooq) Name() string { return "stooq" }

// Adjustment implements Source.
func (s *Stooq) Adjustment() string { return "split and dividend adjusted (per stooq.com)" }

// StooqSymbol translates a QuantOS ticker to the Stooq convention.
func StooqSymbol(t domain.Ticker) string {
	switch t {
	case "VIX":
		return "^vix"
	case "DXY":
		// Stooq has no spot dollar index; the continuous future is the closest
		// series. Callers who disagree can override it in the symbol map.
		return "dx.f"
	}
	s := strings.ToLower(string(t))
	s = strings.ReplaceAll(s, ".", "-") // BRK.B -> brk-b
	return s + ".us"
}

// Fetch implements Source.
func (s *Stooq) Fetch(ctx context.Context, symbol string, start, end time.Time) ([]domain.Candle, error) {
	q := url.Values{}
	q.Set("s", symbol)
	q.Set("i", "d")
	if !start.IsZero() {
		q.Set("d1", start.Format("20060102"))
	}
	if !end.IsZero() {
		q.Set("d2", end.Format("20060102"))
	}
	u := strings.TrimRight(s.BaseURL, "/") + "/q/d/l/?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "quantos-ingest/1.0 (research; paper trading only)")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ingest: stooq %s: %w", symbol, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ingest: stooq %s: HTTP %d", symbol, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(body))
	switch {
	case text == "" || strings.HasPrefix(text, "No data"):
		return nil, ErrBadSymbol
	case strings.Contains(strings.ToLower(text), "exceeded the daily hits limit"):
		return nil, ErrRateLimited
	case strings.Contains(text, "requires JavaScript"):
		return nil, fmt.Errorf("ingest: stooq %s: the site is behind a browser-verification wall; use -source yahoo or -source tiingo", symbol)
	case !strings.HasPrefix(strings.ToLower(text), "date,"):
		return nil, fmt.Errorf("ingest: stooq %s: unexpected response %q", symbol, firstLine(text))
	}
	bars, err := parseDailyCSV(strings.NewReader(text), nil)
	if err != nil {
		return nil, err
	}
	for i := range bars {
		bars[i].Source = "stooq"
		bars[i].Adjusted = true
	}
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	return bars, nil
}

// --- Yahoo Finance -----------------------------------------------------------

// Yahoo fetches daily history from the chart endpoint the Yahoo Finance site
// itself uses (and that the yfinance library wraps). It needs no key and
// returns an adjusted close alongside raw OHLC; the whole bar is scaled by
// adjclose/close so the series is consistent, which is the standard
// approximation. It is an unofficial endpoint: it has changed before and will
// change again, and the parser fails loudly rather than guessing when it does.
type Yahoo struct {
	BaseURL string
	Client  *http.Client
}

// NewYahoo returns a Yahoo source.
func NewYahoo() *Yahoo {
	return &Yahoo{BaseURL: "https://query1.finance.yahoo.com", Client: defaultClient(0)}
}

// Name implements Source.
func (y *Yahoo) Name() string { return "yahoo" }

// Adjustment implements Source.
func (y *Yahoo) Adjustment() string {
	return "split and dividend adjusted (bars scaled by Yahoo adjclose/close)"
}

// YahooSymbol translates a QuantOS ticker to the Yahoo convention.
func YahooSymbol(t domain.Ticker) string {
	switch t {
	case "VIX":
		return "^VIX"
	case "DXY":
		return "DX-Y.NYB"
	}
	return strings.ReplaceAll(string(t), ".", "-") // BRK.B -> BRK-B
}

type yahooChart struct {
	Chart struct {
		Result []struct {
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
				AdjClose []struct {
					AdjClose []*float64 `json:"adjclose"`
				} `json:"adjclose"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// Fetch implements Source.
func (y *Yahoo) Fetch(ctx context.Context, symbol string, start, end time.Time) ([]domain.Candle, error) {
	if start.IsZero() {
		start = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if end.IsZero() {
		end = time.Now().UTC()
	}
	q := url.Values{}
	q.Set("interval", "1d")
	q.Set("events", "div,splits")
	q.Set("includeAdjustedClose", "true")
	q.Set("period1", strconv.FormatInt(start.Unix(), 10))
	q.Set("period2", strconv.FormatInt(end.Add(24*time.Hour).Unix(), 10))
	u := fmt.Sprintf("%s/v8/finance/chart/%s?%s", strings.TrimRight(y.BaseURL, "/"), url.PathEscape(symbol), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// The endpoint answers a browser; a bare Go client gets a 429 or an
	// empty body. The string is the point, not a disguise: the request is
	// the same one the site makes.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	resp, err := y.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ingest: yahoo %s: %w", symbol, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrBadSymbol
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	default:
		return nil, fmt.Errorf("ingest: yahoo %s: HTTP %d", symbol, resp.StatusCode)
	}
	var ch yahooChart
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&ch); err != nil {
		return nil, fmt.Errorf("ingest: yahoo %s: decode: %w", symbol, err)
	}
	if ch.Chart.Error != nil {
		if strings.Contains(strings.ToLower(ch.Chart.Error.Code), "not found") {
			return nil, ErrBadSymbol
		}
		return nil, fmt.Errorf("ingest: yahoo %s: %s: %s", symbol, ch.Chart.Error.Code, ch.Chart.Error.Description)
	}
	if len(ch.Chart.Result) == 0 || len(ch.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, ErrNoData
	}
	r := ch.Chart.Result[0]
	qt := r.Indicators.Quote[0]
	var adj []*float64
	if len(r.Indicators.AdjClose) > 0 {
		adj = r.Indicators.AdjClose[0].AdjClose
	}
	at := func(v []*float64, i int) (float64, bool) {
		if i >= len(v) || v[i] == nil || math.IsNaN(*v[i]) {
			return 0, false
		}
		return *v[i], true
	}
	out := make([]domain.Candle, 0, len(r.Timestamp))
	for i, ts := range r.Timestamp {
		o, ok1 := at(qt.Open, i)
		h, ok2 := at(qt.High, i)
		l, ok3 := at(qt.Low, i)
		c, ok4 := at(qt.Close, i)
		if !(ok1 && ok2 && ok3 && ok4) || c <= 0 {
			continue // a null row is a holiday or a gap, not a price
		}
		v, _ := at(qt.Volume, i)
		k := 1.0
		if a, ok := at(adj, i); ok && a > 0 {
			k = a / c
		}
		day := time.Unix(ts, 0).UTC()
		y0, m0, d0 := day.Date()
		day = time.Date(y0, m0, d0, 0, 0, 0, 0, time.UTC)
		out = append(out, domain.Candle{
			Start: day, End: day.Add(24 * time.Hour),
			Open: o * k, High: h * k, Low: l * k, Close: c * k, Volume: v / k,
			Source: "yahoo", Adjusted: true,
		})
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// --- Tiingo ------------------------------------------------------------------

// Tiingo fetches daily history from api.tiingo.com. It needs an API token
// (free tier is enough for a few hundred names a day) and returns both raw
// and adjusted fields; the adjusted ones are used.
type Tiingo struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewTiingo returns a Tiingo source.
func NewTiingo(token string) *Tiingo {
	return &Tiingo{BaseURL: "https://api.tiingo.com", Token: token, Client: defaultClient(0)}
}

// Name implements Source.
func (t *Tiingo) Name() string { return "tiingo" }

// Adjustment implements Source.
func (t *Tiingo) Adjustment() string {
	return "split and dividend adjusted (tiingo adjOpen/adjHigh/adjLow/adjClose/adjVolume)"
}

// TiingoSymbol translates a QuantOS ticker to the Tiingo convention.
func TiingoSymbol(t domain.Ticker) string {
	return strings.ReplaceAll(string(t), ".", "-")
}

type tiingoRow struct {
	Date      string  `json:"date"`
	AdjOpen   float64 `json:"adjOpen"`
	AdjHigh   float64 `json:"adjHigh"`
	AdjLow    float64 `json:"adjLow"`
	AdjClose  float64 `json:"adjClose"`
	AdjVolume float64 `json:"adjVolume"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	Volume    float64 `json:"volume"`
}

// Fetch implements Source.
func (t *Tiingo) Fetch(ctx context.Context, symbol string, start, end time.Time) ([]domain.Candle, error) {
	if t.Token == "" {
		return nil, ErrUnauthorized
	}
	q := url.Values{}
	q.Set("format", "json")
	q.Set("resampleFreq", "daily")
	if !start.IsZero() {
		q.Set("startDate", start.Format("2006-01-02"))
	}
	if !end.IsZero() {
		q.Set("endDate", end.Format("2006-01-02"))
	}
	u := fmt.Sprintf("%s/tiingo/daily/%s/prices?%s", strings.TrimRight(t.BaseURL, "/"), url.PathEscape(symbol), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+t.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ingest: tiingo %s: %w", symbol, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrBadSymbol
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("ingest: tiingo %s: HTTP %d", symbol, resp.StatusCode)
	}
	var rows []tiingoRow
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&rows); err != nil {
		return nil, fmt.Errorf("ingest: tiingo %s: decode: %w", symbol, err)
	}
	out := make([]domain.Candle, 0, len(rows))
	for _, r := range rows {
		ts, err := parseDate(r.Date)
		if err != nil {
			continue
		}
		c := domain.Candle{
			Start: ts, End: ts.Add(24 * time.Hour),
			Open: r.AdjOpen, High: r.AdjHigh, Low: r.AdjLow, Close: r.AdjClose, Volume: r.AdjVolume,
			Source: "tiingo", Adjusted: true,
		}
		if c.Close == 0 { // older rows occasionally lack adjusted fields
			c.Open, c.High, c.Low, c.Close, c.Volume = r.Open, r.High, r.Low, r.Close, r.Volume
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// --- Local directory ---------------------------------------------------------

// LocalDir reads daily CSVs someone already downloaded (a Yahoo Finance
// export, a broker statement, anything with a date and OHLC columns). It
// looks for <dir>/<SYMBOL>.csv, case-insensitively, and prefers an adjusted
// close column when one exists.
//
// A Yahoo export has "Adj Close" but unadjusted open/high/low; this source
// scales the whole bar by adjclose/close so the series is consistent. That is
// the standard approximation and it is recorded in the manifest.
type LocalDir struct {
	Dir string
}

// Name implements Source.
func (l *LocalDir) Name() string { return "local:" + l.Dir }

// Adjustment implements Source.
func (l *LocalDir) Adjustment() string {
	return "as found in the files; when an adj_close/adjclose column exists the bar is scaled by adjclose/close"
}

// Fetch implements Source.
func (l *LocalDir) Fetch(_ context.Context, symbol string, start, end time.Time) ([]domain.Candle, error) {
	path, err := findFile(l.Dir, symbol)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		return nil, err
	}
	text := string(data)
	bars, err := parseDailyCSV(strings.NewReader(text), nil)
	if err != nil {
		return nil, err
	}
	// Second pass for an adjusted close, if present under any common name.
	var adj []domain.Candle
	for _, name := range []string{"adj close", "adjclose", "adj_close", "adjusted close"} {
		cand, err := parseDailyCSV(strings.NewReader(text), map[string]string{"close": name})
		if err == nil && hasDifferentClose(bars, cand) {
			adj = cand
			break
		}
	}
	if adj != nil {
		for i := range bars {
			if bars[i].Close > 0 && adj[i].Close > 0 {
				k := adj[i].Close / bars[i].Close
				bars[i].Open *= k
				bars[i].High *= k
				bars[i].Low *= k
				bars[i].Close = adj[i].Close
				if k != 0 {
					bars[i].Volume /= k
				}
			}
		}
	}
	out := bars[:0]
	for _, c := range bars {
		if !start.IsZero() && c.Start.Before(start) {
			continue
		}
		if !end.IsZero() && c.Start.After(end) {
			continue
		}
		c.Source = "local"
		c.Adjusted = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

func hasDifferentClose(a, b []domain.Candle) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Close != b[i].Close {
			return true
		}
	}
	return false
}

func findFile(dir, symbol string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(symbol)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if name == want+".csv" || name == strings.ReplaceAll(want, ".", "-")+".csv" {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", ErrBadSymbol
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// DefaultSymbols returns the provider's symbol map for a set of tickers.
func DefaultSymbols(source string, tickers []domain.Ticker) SymbolMap {
	m := SymbolMap{}
	for _, t := range tickers {
		switch source {
		case "stooq":
			m[t] = StooqSymbol(t)
		case "yahoo":
			m[t] = YahooSymbol(t)
		case "tiingo":
			m[t] = TiingoSymbol(t)
		default:
			m[t] = string(t)
		}
	}
	return m
}

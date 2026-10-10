// Package ingest fetches real daily market history from external sources and
// writes it in the layout the replay provider reads (internal/marketdata/replay.go):
//
//	<dir>/<TICKER>.csv   with header  ts,open,high,low,close,volume
//	<dir>/MANIFEST.json  what was fetched, from where, when, and how it is adjusted
//	<dir>/universe.json  per-instrument liquidity derived from the data itself
//
// It exists because every model and backtest in this repository was, until
// this package, run on the platform's own simulator. A simulator is a fine
// place to test plumbing and a useless place to measure edge: whatever the
// pipeline learns there, it learns about the simulator.
//
// Sources are deliberately simple HTTP clients. There is no streaming, no
// intraday, no fundamentals. Daily bars are the right first step: the signal
// to noise ratio is tolerable, the cost of a round-trip is small relative to
// the move, and ten years of history for a few hundred names fits in memory.
package ingest

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Source fetches daily bars for one instrument.
type Source interface {
	Name() string
	// Adjustment describes how prices are adjusted, verbatim from the
	// provider's documentation. It is recorded in the manifest rather than
	// trusted: mixing adjusted and unadjusted series is refused downstream.
	Adjustment() string
	Fetch(ctx context.Context, symbol string, start, end time.Time) ([]domain.Candle, error)
}

// Sentinel errors.
var (
	ErrNoData       = errors.New("ingest: source returned no bars")
	ErrBadSymbol    = errors.New("ingest: symbol not found at source")
	ErrRateLimited  = errors.New("ingest: rate limited by source")
	ErrUnauthorized = errors.New("ingest: source rejected the API token")
)

// SymbolMap translates QuantOS tickers to provider symbols. Entries are
// provider-specific; Stooq wants "aapl.us" and "^vix", Tiingo wants "AAPL".
type SymbolMap map[domain.Ticker]string

// Request configures one ingestion run.
type Request struct {
	Tickers []domain.Ticker
	Start   time.Time
	End     time.Time
	OutDir  string
	// Symbols overrides the provider's default symbol translation.
	Symbols SymbolMap
	// Delay between requests, so an unauthenticated source is not hammered.
	Delay time.Duration
	// MinBars rejects series too short to be useful. A name that only has a
	// few weeks of history cannot warm the feature engine, let alone be
	// evaluated.
	MinBars int
	// Progress receives one line per instrument. Nil is fine.
	Progress func(string)
}

// Result summarises a run.
type Result struct {
	Written  []domain.Ticker          `json:"written"`
	Skipped  map[domain.Ticker]string `json:"skipped,omitempty"`
	Bars     int                      `json:"bars"`
	Manifest Manifest                 `json:"manifest"`
}

// Manifest is written next to the data so that a later reader knows what it
// is looking at. The replay provider does not read it; people do.
type Manifest struct {
	Source     string                  `json:"source"`
	Adjustment string                  `json:"adjustment"`
	Interval   domain.Interval         `json:"interval"`
	FetchedAt  time.Time               `json:"fetched_at"`
	Start      time.Time               `json:"requested_start"`
	End        time.Time               `json:"requested_end"`
	Series     map[domain.Ticker]Range `json:"series"`
	Notes      []string                `json:"notes,omitempty"`
}

// Range records the actual coverage of one series.
type Range struct {
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	Bars  int       `json:"bars"`
	// AvgNotional30 is the trailing-30-bar average of close*volume at the end
	// of the series. It is what the risk engine's liquidity check needs, and
	// without it every instrument fails that check as unmeasurable.
	AvgNotional30 float64 `json:"avg_notional_30d"`
	AvgVolume30   float64 `json:"avg_volume_30d"`
}

// Run fetches every ticker and writes the replay directory. It continues past
// per-symbol failures and reports them, because one delisted name must not
// abort a 500-name pull.
func Run(ctx context.Context, src Source, req Request) (Result, error) {
	if src == nil {
		return Result{}, errors.New("ingest: nil source")
	}
	if req.OutDir == "" {
		return Result{}, errors.New("ingest: output directory is required")
	}
	if len(req.Tickers) == 0 {
		return Result{}, errors.New("ingest: no tickers requested")
	}
	if req.MinBars <= 0 {
		req.MinBars = 60
	}
	if err := os.MkdirAll(req.OutDir, 0o750); err != nil {
		return Result{}, fmt.Errorf("ingest: output directory: %w", err)
	}

	res := Result{Skipped: map[domain.Ticker]string{}}
	man := Manifest{
		Source: src.Name(), Adjustment: src.Adjustment(), Interval: domain.Interval1d,
		FetchedAt: time.Now().UTC(), Start: req.Start, End: req.End,
		Series: map[domain.Ticker]Range{},
	}
	say := req.Progress
	if say == nil {
		say = func(string) {}
	}

	tickers := append([]domain.Ticker(nil), req.Tickers...)
	sort.Slice(tickers, func(i, j int) bool { return tickers[i] < tickers[j] })

	for i, t := range tickers {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if i > 0 && req.Delay > 0 {
			select {
			case <-time.After(req.Delay):
			case <-ctx.Done():
				return res, ctx.Err()
			}
		}
		symbol := string(t)
		if s, ok := req.Symbols[t]; ok {
			symbol = s
		}
		bars, err := src.Fetch(ctx, symbol, req.Start, req.End)
		if err != nil {
			res.Skipped[t] = err.Error()
			say(fmt.Sprintf("%-8s skipped: %v", t, err))
			continue
		}
		bars = Clean(t, bars)
		if len(bars) < req.MinBars {
			res.Skipped[t] = fmt.Sprintf("only %d usable bars (minimum %d)", len(bars), req.MinBars)
			say(fmt.Sprintf("%-8s skipped: only %d usable bars", t, len(bars)))
			continue
		}
		path := filepath.Join(req.OutDir, string(t)+".csv")
		if err := WriteReplayCSV(path, bars); err != nil {
			return res, err
		}
		rng := rangeOf(bars)
		man.Series[t] = rng
		res.Written = append(res.Written, t)
		res.Bars += len(bars)
		say(fmt.Sprintf("%-8s %5d bars  %s .. %s  adv $%.1fM", t, len(bars),
			rng.First.Format("2006-01-02"), rng.Last.Format("2006-01-02"), rng.AvgNotional30/1e6))
	}
	if len(res.Written) == 0 {
		return res, ErrNoData
	}
	man.Notes = append(man.Notes,
		"prices are as delivered by the source; see adjustment for what that means",
		"avg_notional_30d is computed from the final 30 bars of each series and is used only by the liquidity gate",
		"this directory is replay input: the platform treats every bar as adjusted=true, so do not mix sources")
	res.Manifest = man
	if err := writeJSON(filepath.Join(req.OutDir, "MANIFEST.json"), man); err != nil {
		return res, err
	}
	return res, nil
}

// Clean sorts, de-duplicates and validates a series. Bars that fail
// domain.Candle.Valid are dropped rather than repaired: a repaired price is a
// guess, and the series is long enough that one missing day does not matter.
func Clean(t domain.Ticker, bars []domain.Candle) []domain.Candle {
	sort.SliceStable(bars, func(i, j int) bool { return bars[i].Start.Before(bars[j].Start) })
	out := bars[:0]
	var last time.Time
	for _, c := range bars {
		c.Ticker = t
		c.Interval = domain.Interval1d
		if c.End.IsZero() || !c.End.After(c.Start) {
			c.End = c.Start.Add(24 * time.Hour)
		}
		if !last.IsZero() && !c.Start.After(last) {
			continue // duplicate date
		}
		if c.Valid() != nil {
			continue
		}
		if c.VWAP == 0 {
			c.VWAP = c.TypicalPrice()
		}
		out = append(out, c)
		last = c.Start
	}
	return out
}

// WriteReplayCSV writes bars in the replay layout.
func WriteReplayCSV(path string, bars []domain.Candle) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("ingest: create %s: %w", path, err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"ts", "open", "high", "low", "close", "volume"}); err != nil {
		return err
	}
	for _, c := range bars {
		rec := []string{
			c.Start.UTC().Format(time.RFC3339),
			strconv.FormatFloat(c.Open, 'f', 6, 64),
			strconv.FormatFloat(c.High, 'f', 6, 64),
			strconv.FormatFloat(c.Low, 'f', 6, 64),
			strconv.FormatFloat(c.Close, 'f', 6, 64),
			strconv.FormatFloat(c.Volume, 'f', 0, 64),
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func rangeOf(bars []domain.Candle) Range {
	r := Range{First: bars[0].Start, Last: bars[len(bars)-1].Start, Bars: len(bars)}
	n := 30
	if len(bars) < n {
		n = len(bars)
	}
	tail := bars[len(bars)-n:]
	var notional, vol float64
	for _, c := range tail {
		notional += c.Close * c.Volume
		vol += c.Volume
	}
	r.AvgNotional30 = notional / float64(n)
	r.AvgVolume30 = vol / float64(n)
	return r
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o640)
}

// --- shared CSV parsing ------------------------------------------------------

// parseDailyCSV reads a Date,Open,High,Low,Close,Volume table with a header,
// tolerating the column names the common sources use. Columns are matched by
// name, never by position, because the position is the first thing a provider
// changes without notice.
func parseDailyCSV(r io.Reader, prefer map[string]string) ([]domain.Candle, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("ingest: csv header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	col := func(names ...string) (int, bool) {
		for _, n := range names {
			if i, ok := idx[strings.ToLower(n)]; ok {
				return i, true
			}
		}
		return 0, false
	}
	pick := func(key string, defaults ...string) (int, bool) {
		if p, ok := prefer[key]; ok {
			if i, ok := col(p); ok {
				return i, true
			}
		}
		return col(defaults...)
	}
	iDate, okD := pick("date", "date", "ts", "timestamp", "time")
	iOpen, okO := pick("open", "open")
	iHigh, okH := pick("high", "high")
	iLow, okL := pick("low", "low")
	iClose, okC := pick("close", "close")
	iVol, okV := pick("volume", "volume", "vol")
	if !okD || !okO || !okH || !okL || !okC {
		return nil, fmt.Errorf("ingest: csv is missing one of date/open/high/low/close (header %q)", header)
	}
	var out []domain.Candle
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("ingest: csv row: %w", err)
		}
		if iDate >= len(rec) || iClose >= len(rec) {
			continue
		}
		ts, err := parseDate(rec[iDate])
		if err != nil {
			continue
		}
		c := domain.Candle{
			Start: ts, End: ts.Add(24 * time.Hour),
			Open: num(rec, iOpen), High: num(rec, iHigh), Low: num(rec, iLow), Close: num(rec, iClose),
		}
		if okV {
			c.Volume = num(rec, iVol)
		}
		if math.IsNaN(c.Close) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func num(rec []string, i int) float64 {
	if i < 0 || i >= len(rec) {
		return math.NaN()
	}
	s := strings.TrimSpace(rec[i])
	if s == "" || strings.EqualFold(s, "null") || strings.EqualFold(s, "nan") {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05", "01/02/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			y, m, d := t.UTC().Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date %q", s)
}

package marketdata

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Replay serves historical bars from CSV files and can drive the live pipeline
// by emitting them in timestamp order. It is what turns "the backtester replays
// into the production engines" (ADR-007) into something you can point at a
// directory of files.
//
// Expected layout: <dir>/<TICKER>.csv with a header row
//
//	ts,open,high,low,close,volume[,vwap]
//
// where ts is RFC3339 or a Unix seconds integer.
type Replay struct {
	dir      string
	interval domain.Interval

	mu   sync.RWMutex
	bars map[domain.Ticker][]domain.Candle
	// cursor is the replay position, shared across symbols and advanced in
	// timestamp order so cross-symbol causality is preserved.
	ordered []domain.Candle
	cursor  int
	loaded  bool
}

// NewReplay creates a replay provider rooted at dir.
func NewReplay(dir string, interval domain.Interval) *Replay {
	if interval == "" {
		interval = domain.Interval1m
	}
	return &Replay{dir: dir, interval: interval, bars: map[domain.Ticker][]domain.Candle{}}
}

// Name implements Provider.
func (r *Replay) Name() string { return "replay" }

// Capabilities implements Provider.
func (r *Replay) Capabilities() Capabilities {
	return Capabilities{
		Intervals:  []domain.Interval{r.interval},
		MaxHistory: 10 * 365 * 24 * time.Hour,
		Quotes:     true,
		Adjusted:   true,
		Simulated:  false,
	}
}

// Load reads every CSV in the directory. It is idempotent.
func (r *Replay) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return fmt.Errorf("replay: read dir %s: %w", r.dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".csv") {
			continue
		}
		ticker := domain.NormalizeTicker(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		bars, err := r.readFile(filepath.Join(r.dir, e.Name()), ticker)
		if err != nil {
			return err
		}
		r.bars[ticker] = bars
		r.ordered = append(r.ordered, bars...)
	}
	sort.SliceStable(r.ordered, func(i, j int) bool {
		if r.ordered[i].Start.Equal(r.ordered[j].Start) {
			return r.ordered[i].Ticker < r.ordered[j].Ticker
		}
		return r.ordered[i].Start.Before(r.ordered[j].Start)
	})
	r.loaded = true
	return nil
}

func (r *Replay) readFile(path string, ticker domain.Ticker) ([]domain.Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open %s: %w", path, err)
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("replay: header %s: %w", path, err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	need := []string{"ts", "open", "high", "low", "close", "volume"}
	for _, n := range need {
		if _, ok := idx[n]; !ok {
			return nil, fmt.Errorf("replay: %s missing column %q", path, n)
		}
	}
	var out []domain.Candle
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("replay: %s line %d: %w", path, line, err)
		}
		ts, err := parseTime(rec[idx["ts"]])
		if err != nil {
			return nil, fmt.Errorf("replay: %s line %d: %w", path, line, err)
		}
		c := domain.Candle{
			Ticker:   ticker,
			Interval: r.interval,
			Open:     mustFloat(rec[idx["open"]]),
			High:     mustFloat(rec[idx["high"]]),
			Low:      mustFloat(rec[idx["low"]]),
			Close:    mustFloat(rec[idx["close"]]),
			Volume:   mustFloat(rec[idx["volume"]]),
			Start:    ts,
			End:      ts.Add(r.interval.Duration()),
			Adjusted: true,
			Source:   "replay",
		}
		if i, ok := idx["vwap"]; ok && i < len(rec) {
			c.VWAP = mustFloat(rec[i])
		}
		if c.VWAP == 0 {
			c.VWAP = c.TypicalPrice()
		}
		if err := c.Valid(); err != nil {
			return nil, fmt.Errorf("replay: %s line %d: %w", path, line, err)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", s)
}

func mustFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// Next returns the next bar in global timestamp order, or false at the end.
func (r *Replay) Next() (domain.Candle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cursor >= len(r.ordered) {
		return domain.Candle{}, false
	}
	c := r.ordered[r.cursor]
	r.cursor++
	return c, true
}

// Reset rewinds the cursor, which makes a replay repeatable.
func (r *Replay) Reset() {
	r.mu.Lock()
	r.cursor = 0
	r.mu.Unlock()
}

// Len returns the number of bars loaded.
func (r *Replay) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ordered)
}

// Tickers returns the loaded symbols in sorted order.
func (r *Replay) Tickers() []domain.Ticker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.Ticker, 0, len(r.bars))
	for t := range r.bars {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// GetQuotes derives a synthetic top-of-book from the bar at the cursor.
func (r *Replay) GetQuotes(_ context.Context, tickers []domain.Ticker) ([]domain.Quote, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []domain.Quote{}
	for _, t := range tickers {
		bars := r.bars[t]
		if len(bars) == 0 {
			continue
		}
		c := bars[len(bars)-1]
		half := c.Close * 0.0002
		out = append(out, domain.Quote{
			Ticker: t, Bid: c.Close - half, Ask: c.Close + half, Last: c.Close,
			Volume: c.Volume, Timestamp: c.End, Source: "replay",
		})
	}
	return out, nil
}

// GetHistoricalBars implements Provider over the loaded series.
func (r *Replay) GetHistoricalBars(_ context.Context, req BarRequest) ([]domain.Candle, error) {
	if req.Interval != "" && req.Interval != r.interval {
		return nil, ErrUnsupported
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []domain.Candle{}
	for _, t := range req.Tickers {
		for _, c := range r.bars[t] {
			if !req.Start.IsZero() && c.Start.Before(req.Start) {
				continue
			}
			if !req.End.IsZero() && !c.Start.Before(req.End) {
				continue
			}
			out = append(out, c)
			if req.Limit > 0 && len(out) >= req.Limit {
				break
			}
		}
	}
	return out, nil
}

// GetTrades is unsupported for bar-level replay.
func (r *Replay) GetTrades(context.Context, TradeRequest) ([]domain.Trade, error) {
	return nil, ErrUnsupported
}

// GetMarketStatus derives the session from the cursor's timestamp.
func (r *Replay) GetMarketStatus(context.Context) (domain.MarketStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var t time.Time
	if r.cursor > 0 && r.cursor <= len(r.ordered) {
		t = r.ordered[r.cursor-1].Start
	}
	return domain.MarketStatus{Session: sessionFor(t), Timestamp: t, Venue: "REPLAY"}, nil
}

// GetCorporateEvents is unsupported for bar-level replay.
func (r *Replay) GetCorporateEvents(context.Context, EventRequest) ([]domain.CorporateEvent, error) {
	return nil, ErrUnsupported
}

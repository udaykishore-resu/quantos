package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
)

const stooqBody = `Date,Open,High,Low,Close,Volume
2024-01-02,187.15,188.44,183.89,185.64,82488700
2024-01-03,184.22,185.88,183.43,184.25,58414500
2024-01-03,184.22,185.88,183.43,184.25,58414500
2024-01-04,182.15,183.09,180.88,181.91,71983600
2024-01-05,181.99,182.76,180.17,181.18,62303300
2024-01-08,0,0,0,0,0
`

func TestStooqFetchParsesAndCleans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("s") != "aapl.us" {
			_, _ = w.Write([]byte("No data"))
			return
		}
		if r.URL.Query().Get("i") != "d" {
			t.Errorf("expected daily interval, got %q", r.URL.Query().Get("i"))
		}
		_, _ = w.Write([]byte(stooqBody))
	}))
	defer srv.Close()

	src := &Stooq{BaseURL: srv.URL, Client: srv.Client()}
	bars, err := src.Fetch(context.Background(), "aapl.us", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	bars = Clean("AAPL", bars)
	if len(bars) != 4 {
		t.Fatalf("expected 4 clean bars (duplicate and zero row dropped), got %d", len(bars))
	}
	if bars[0].Start != time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC) {
		t.Errorf("first bar start = %s", bars[0].Start)
	}
	if bars[0].End.Sub(bars[0].Start) != 24*time.Hour {
		t.Errorf("daily bar should span 24h, got %s", bars[0].End.Sub(bars[0].Start))
	}
	if bars[0].Interval != domain.Interval1d || !bars[0].Adjusted {
		t.Errorf("bar should be 1d and adjusted: %+v", bars[0])
	}

	if _, err := src.Fetch(context.Background(), "nope.us", time.Time{}, time.Time{}); err != ErrBadSymbol {
		t.Errorf("unknown symbol should be ErrBadSymbol, got %v", err)
	}
}

func TestStooqSymbolConvention(t *testing.T) {
	cases := map[domain.Ticker]string{"AAPL": "aapl.us", "BRK.B": "brk-b.us", "VIX": "^vix", "SPY": "spy.us"}
	for in, want := range cases {
		if got := StooqSymbol(in); got != want {
			t.Errorf("StooqSymbol(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestTiingoFetchUsesAdjustedFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.URL.Path, "/tiingo/daily/MSFT/prices") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"date": "2024-01-02T00:00:00.000Z", "open": 100, "high": 110, "low": 90, "close": 105, "volume": 1000,
				"adjOpen": 50, "adjHigh": 55, "adjLow": 45, "adjClose": 52.5, "adjVolume": 2000},
			{"date": "2024-01-03T00:00:00.000Z", "open": 106, "high": 112, "low": 100, "close": 110, "volume": 1200,
				"adjOpen": 53, "adjHigh": 56, "adjLow": 50, "adjClose": 55, "adjVolume": 2400},
		})
	}))
	defer srv.Close()

	src := &Tiingo{BaseURL: srv.URL, Token: "secret", Client: srv.Client()}
	bars, err := src.Fetch(context.Background(), "MSFT", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(bars) != 2 || bars[0].Close != 52.5 || bars[0].Volume != 2000 {
		t.Fatalf("adjusted fields not used: %+v", bars)
	}
	bad := &Tiingo{BaseURL: srv.URL, Token: "wrong", Client: srv.Client()}
	if _, err := bad.Fetch(context.Background(), "MSFT", time.Time{}, time.Time{}); err != ErrUnauthorized {
		t.Errorf("bad token should be ErrUnauthorized, got %v", err)
	}
	if _, err := src.Fetch(context.Background(), "ZZZZ", time.Time{}, time.Time{}); err != ErrBadSymbol {
		t.Errorf("unknown symbol should be ErrBadSymbol, got %v", err)
	}
}

func TestLocalDirScalesByAdjustedClose(t *testing.T) {
	dir := t.TempDir()
	yahoo := "Date,Open,High,Low,Close,Adj Close,Volume\n" +
		"2024-01-02,100,110,90,100,50,1000\n" +
		"2024-01-03,104,112,100,110,55,1200\n"
	if err := os.WriteFile(filepath.Join(dir, "nvda.csv"), []byte(yahoo), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &LocalDir{Dir: dir}
	bars, err := src.Fetch(context.Background(), "NVDA", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("expected 2 bars, got %d", len(bars))
	}
	// Whole bar scaled by 0.5, volume scaled by 2.
	if bars[0].Open != 50 || bars[0].High != 55 || bars[0].Low != 45 || bars[0].Close != 50 || bars[0].Volume != 2000 {
		t.Errorf("bar not scaled consistently: %+v", bars[0])
	}
	if _, err := src.Fetch(context.Background(), "MISSING", time.Time{}, time.Time{}); err != ErrBadSymbol {
		t.Errorf("missing file should be ErrBadSymbol, got %v", err)
	}
}

// fakeSource serves canned series so Run can be exercised without a network.
type fakeSource struct{ series map[string][]domain.Candle }

func (f fakeSource) Name() string       { return "fake" }
func (f fakeSource) Adjustment() string { return "none" }
func (f fakeSource) Fetch(_ context.Context, symbol string, _, _ time.Time) ([]domain.Candle, error) {
	s, ok := f.series[symbol]
	if !ok {
		return nil, ErrBadSymbol
	}
	return append([]domain.Candle(nil), s...), nil
}

func synthetic(n int, start float64) []domain.Candle {
	out := make([]domain.Candle, 0, n)
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	px := start
	for i := 0; i < n; i++ {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
			i--
			continue
		}
		o := px
		c := px * (1 + 0.001*float64((i%7)-3))
		h, l := o, o
		if c > h {
			h = c
		}
		if c < l {
			l = c
		}
		out = append(out, domain.Candle{Start: day, Open: o, High: h * 1.001, Low: l * 0.999, Close: c, Volume: 1_000_000})
		px = c
		day = day.AddDate(0, 0, 1)
	}
	return out
}

func TestRunWritesReplayLayoutTheReplayProviderCanRead(t *testing.T) {
	dir := t.TempDir()
	src := fakeSource{series: map[string][]domain.Candle{
		"AAPL": synthetic(120, 150), "SPY": synthetic(120, 400), "SHORT": synthetic(10, 20),
	}}
	res, err := Run(context.Background(), src, Request{
		Tickers: []domain.Ticker{"SPY", "AAPL", "SHORT", "GONE"}, OutDir: dir, MinBars: 60,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Written) != 2 || res.Written[0] != "AAPL" || res.Written[1] != "SPY" {
		t.Fatalf("written = %v", res.Written)
	}
	if _, ok := res.Skipped["SHORT"]; !ok {
		t.Errorf("short series should be skipped: %v", res.Skipped)
	}
	if _, ok := res.Skipped["GONE"]; !ok {
		t.Errorf("unknown symbol should be skipped: %v", res.Skipped)
	}
	if res.Manifest.Series["AAPL"].AvgNotional30 <= 0 {
		t.Errorf("manifest should carry liquidity: %+v", res.Manifest.Series["AAPL"])
	}
	if _, err := os.Stat(filepath.Join(dir, "MANIFEST.json")); err != nil {
		t.Errorf("manifest not written: %v", err)
	}

	// The whole point: the replay provider must read what ingest wrote.
	rp := marketdata.NewReplay(dir, domain.Interval1d)
	if err := rp.Load(); err != nil {
		t.Fatalf("replay load: %v", err)
	}
	if rp.Len() != 240 {
		t.Fatalf("replay loaded %d bars, want 240", rp.Len())
	}
	bars, err := rp.GetHistoricalBars(context.Background(), marketdata.BarRequest{
		Tickers: []domain.Ticker{"AAPL"}, Interval: domain.Interval1d,
	})
	if err != nil || len(bars) != 120 {
		t.Fatalf("replay bars for AAPL: %d, %v", len(bars), err)
	}
	if bars[0].End.Sub(bars[0].Start) != 24*time.Hour || !bars[0].Adjusted {
		t.Errorf("replayed bar lost its daily shape: %+v", bars[0])
	}
}

func TestYahooFetchScalesByAdjustedCloseAndSkipsNulls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || !strings.Contains(r.Header.Get("User-Agent"), "Mozilla") {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/chart/NOPE"):
			_, _ = w.Write([]byte(`{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`))
		case strings.HasSuffix(r.URL.Path, "/chart/BRK-B"):
			if r.URL.Query().Get("interval") != "1d" || r.URL.Query().Get("period1") == "" {
				t.Errorf("unexpected query %s", r.URL.RawQuery)
			}
			// 2024-01-02, 2024-01-03 (null row), 2024-01-04 as unix seconds.
			_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"symbol":"BRK-B"},"timestamp":[1704153600,1704240000,1704326400],
			  "indicators":{"quote":[{"open":[100,null,104],"high":[110,null,112],"low":[90,null,100],"close":[100,null,110],"volume":[1000,null,1200]}],
			  "adjclose":[{"adjclose":[50,null,110]}]}}],"error":null}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	src := &Yahoo{BaseURL: srv.URL, Client: srv.Client()}
	bars, err := src.Fetch(context.Background(), "BRK-B", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("expected the null row to be skipped, got %d bars", len(bars))
	}
	if bars[0].Start != time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC) {
		t.Errorf("first bar date %s", bars[0].Start)
	}
	// Day one is scaled by 0.5 across the whole bar; day two is unadjusted.
	if bars[0].Open != 50 || bars[0].High != 55 || bars[0].Low != 45 || bars[0].Close != 50 || bars[0].Volume != 2000 {
		t.Errorf("bar not scaled consistently: %+v", bars[0])
	}
	if bars[1].Close != 110 || bars[1].Volume != 1200 {
		t.Errorf("unadjusted bar altered: %+v", bars[1])
	}
	if _, err := src.Fetch(context.Background(), "NOPE", time.Time{}, time.Time{}); err != ErrBadSymbol {
		t.Errorf("unknown symbol should be ErrBadSymbol, got %v", err)
	}
	if _, err := src.Fetch(context.Background(), "GONE", time.Time{}, time.Time{}); err != ErrBadSymbol {
		t.Errorf("404 should be ErrBadSymbol, got %v", err)
	}
	if got := YahooSymbol("BRK.B"); got != "BRK-B" {
		t.Errorf("YahooSymbol(BRK.B) = %s", got)
	}
	if got := YahooSymbol("VIX"); got != "^VIX" {
		t.Errorf("YahooSymbol(VIX) = %s", got)
	}
}

func TestStooqRecognisesTheBrowserWall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><noscript>This site requires JavaScript to verify your browser.</noscript></body></html>`))
	}))
	defer srv.Close()
	src := &Stooq{BaseURL: srv.URL, Client: srv.Client()}
	_, err := src.Fetch(context.Background(), "aapl.us", time.Time{}, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "browser-verification") {
		t.Errorf("expected a clear wall error, got %v", err)
	}
}

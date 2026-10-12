package marketdata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func alpacaFake(t *testing.T) (*Alpaca, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("market data must only GET, got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("APCA-API-KEY-ID") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v2/stocks/quotes/latest", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if strings.Contains(r.URL.Query().Get("symbols"), "VIX") {
			t.Error("excluded symbols must not be requested")
		}
		_, _ = w.Write([]byte(`{"quotes":{"AAPL":{"ap":190.52,"as":3,"bp":190.48,"bs":2,"t":"2026-10-12T14:30:01Z"}}}`))
	})
	mux.HandleFunc("GET /v2/stocks/trades/latest", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"trades":{"AAPL":{"p":190.5,"s":100,"t":"2026-10-12T14:30:02Z"},"MSFT":{"p":410.1,"s":5,"t":"2026-10-12T14:29:00Z"}}}`))
	})
	mux.HandleFunc("GET /v2/stocks/bars", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		if q.Get("timeframe") != "1Min" || q.Get("adjustment") != "raw" || q.Get("feed") != "iex" {
			t.Errorf("unexpected bar query %v", q)
		}
		if q.Get("page_token") == "" {
			_, _ = w.Write([]byte(`{"bars":{"AAPL":[{"t":"2026-10-12T14:28:00Z","o":1,"h":2,"l":0.5,"c":1.5,"v":10,"n":3,"vw":1.2}],
			                      "MSFT":[{"t":"2026-10-12T14:28:00Z","o":4,"h":4,"l":4,"c":4,"v":1,"n":1,"vw":4}]},
			                      "next_page_token":"p2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"bars":{"AAPL":[{"t":"2026-10-12T14:29:00Z","o":1.5,"h":1.6,"l":1.4,"c":1.55,"v":7,"n":2,"vw":1.5}]},"next_page_token":null}`))
	})
	mux.HandleFunc("GET /v2/clock", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"timestamp":"2026-10-12T10:30:00-04:00","is_open":true,"next_open":"2026-10-13T09:30:00-04:00","next_close":"2026-10-12T16:00:00-04:00"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a, err := NewAlpaca(AlpacaConfig{KeyID: "k", SecretKey: "s", DataURL: srv.URL, TradingURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return a, &calls
}

func TestAlpacaRefusesForeignHosts(t *testing.T) {
	for _, u := range []string{"https://example.com", "http://data.alpaca.markets", "https://api.alpaca.markets"} {
		if _, err := NewAlpaca(AlpacaConfig{KeyID: "k", SecretKey: "s", DataURL: u}); err == nil {
			t.Errorf("%s must be refused", u)
		}
	}
	if _, err := NewAlpaca(AlpacaConfig{KeyID: "k", SecretKey: "s", Feed: "delayed"}); err == nil {
		t.Error("an unknown feed must be refused")
	}
}

func TestAlpacaQuotes(t *testing.T) {
	a, _ := alpacaFake(t)
	qs, err := a.GetQuotes(context.Background(), []domain.Ticker{"AAPL", "MSFT", "VIX", "aapl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("want AAPL and MSFT, got %+v", qs)
	}
	aapl := qs[0]
	if aapl.Ticker != "AAPL" || aapl.Bid != 190.48 || aapl.Ask != 190.52 || aapl.Last != 190.5 ||
		!aapl.Timestamp.Equal(time.Date(2026, 10, 12, 14, 30, 2, 0, time.UTC)) || aapl.Source != "alpaca-iex" {
		t.Fatalf("AAPL quote %+v", aapl)
	}
	if err := aapl.Valid(); err != nil {
		t.Fatalf("the quote must pass the platform's structural check: %v", err)
	}
	if msft := qs[1]; msft.Last != 410.1 || msft.Bid != 0 {
		t.Fatalf("a trade with no quote still yields a last price: %+v", msft)
	}
}

func TestAlpacaBarsPaginateAndOrder(t *testing.T) {
	a, _ := alpacaFake(t)
	cs, err := a.GetHistoricalBars(context.Background(), BarRequest{
		Tickers: []domain.Ticker{"AAPL", "MSFT"}, Interval: domain.Interval1m,
		Start: time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("want 3 bars across two pages, got %d", len(cs))
	}
	if cs[0].End.After(cs[2].End) || cs[2].Ticker != "AAPL" || cs[2].Close != 1.55 {
		t.Fatalf("bars must be in time order: %+v", cs)
	}
	if !cs[0].End.Equal(cs[0].Start.Add(time.Minute)) || cs[0].Interval != domain.Interval1m {
		t.Fatalf("a 1m bar ends a minute after it starts: %+v", cs[0])
	}
	limited, _ := a.GetHistoricalBars(context.Background(), BarRequest{
		Tickers: []domain.Ticker{"AAPL"}, Interval: domain.Interval1m, Limit: 1,
		Start: time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC),
	})
	n := 0
	for _, c := range limited {
		if c.Ticker == "AAPL" {
			n++
			if c.Close != 1.55 {
				t.Fatalf("limit must keep the most recent bar, got %+v", c)
			}
		}
	}
	if n != 1 {
		t.Fatalf("limit 1 kept %d AAPL bars", n)
	}
	if _, err := a.GetHistoricalBars(context.Background(), BarRequest{Interval: "2m"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for 2m, got %v", err)
	}
}

func TestAlpacaClockAndErrors(t *testing.T) {
	a, _ := alpacaFake(t)
	st, err := a.GetMarketStatus(context.Background())
	if err != nil || st.Session != domain.SessionOpen || st.NextClose.Hour() != 20 {
		t.Fatalf("clock: %+v %v", st, err)
	}
	bad, _ := NewAlpaca(AlpacaConfig{KeyID: "wrong", SecretKey: "s", DataURL: a.dataURL, TradingURL: a.tradingURL})
	_, err = bad.GetQuotes(context.Background(), []domain.Ticker{"AAPL"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a 401 is ErrUnavailable, got %v", err)
	}
	for i := 0; i < 3; i++ {
		_, _ = bad.GetMarketStatus(context.Background())
	}
	if bad.Healthy(context.Background()) {
		t.Fatal("three failures in a row should mark the provider unhealthy")
	}
}

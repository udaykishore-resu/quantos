package backtest

import (
	"math"
	"testing"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func TestSpreadFromNotionalIsMonotoneAndAnchored(t *testing.T) {
	cases := []struct {
		notional float64
		lo, hi   float64
	}{
		{10e9, 4, 6},    // mega cap: well inside the soft limit
		{1e9, 10, 13},   // large cap
		{100e6, 30, 34}, // mid cap: just over the 25 bps soft limit
		{10e6, 90, 100}, // thin: over the 80 bps hard limit
	}
	prev := 0.0
	for _, c := range cases { // cases run from most to least liquid
		got := spreadFromNotional(c.notional)
		if got < c.lo || got > c.hi {
			t.Errorf("notional $%.0fM: spread %.1f bps, want %.0f..%.0f", c.notional/1e6, got, c.lo, c.hi)
		}
		if got <= prev {
			t.Errorf("spread must rise as notional falls: %.1f then %.1f", prev, got)
		}
		prev = got
	}
	if spreadFromNotional(0) != 0 || spreadFromNotional(-1) != 0 {
		t.Error("non-positive notional must give 0 so the caller floors it")
	}
}

func TestSpreadEstimatorRollsNotional(t *testing.T) {
	e := newSpreadEstimator()
	var last float64
	for i := 0; i < 50; i++ {
		last = e.observe(domain.Candle{Ticker: "X", Close: 100, Volume: 1_000_000}) // $100M/day
	}
	if len(e.notional["X"]) != notionalWindow {
		t.Errorf("window not capped: %d", len(e.notional["X"]))
	}
	if math.Abs(last-spreadFromNotional(100e6)) > 1e-9 {
		t.Errorf("steady state should equal the point estimate: %.3f", last)
	}
	// A burst of volume lowers the implied spread, gradually.
	after := e.observe(domain.Candle{Ticker: "X", Close: 100, Volume: 50_000_000})
	if !(after < last) {
		t.Errorf("more volume should imply a tighter spread: %.2f -> %.2f", last, after)
	}
	if got := e.observe(domain.Candle{Ticker: "VIX", Close: 18}); got != 0 {
		t.Errorf("a volume-less series gives 0, got %.2f", got)
	}
}

func TestSyntheticQuoteIsFlooredAndTwoSided(t *testing.T) {
	a := domain.Candle{Ticker: "X", High: 100.01, Low: 99.99, Close: 100}
	q := syntheticQuote(a, 1.0, 4)
	if q.Bid >= q.Ask || q.Last != 100 {
		t.Fatalf("quote is not two-sided: %+v", q)
	}
	if got := q.SpreadBps(); math.Abs(got-4) > 1e-6 {
		t.Errorf("floor not applied: spread %.4f bps", got)
	}
	q = syntheticQuote(a, 12, 4)
	if got := q.SpreadBps(); math.Abs(got-12) > 1e-6 {
		t.Errorf("estimate above the floor should be used: %.4f", got)
	}
}

package backtest

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// trendingUniverse builds names with persistent, distinct drifts, which is
// the one world where a momentum sort must rank correctly.
func trendingUniverse(seed int64, n, days int) (map[domain.Ticker][]domain.Candle, []domain.Ticker) {
	r := rand.New(rand.NewSource(seed))
	day := time.Date(2014, 1, 2, 0, 0, 0, 0, time.UTC)
	var dates []time.Time
	for len(dates) < days {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			dates = append(dates, day)
		}
		day = day.AddDate(0, 0, 1)
	}
	series := map[domain.Ticker][]domain.Candle{}
	var names []domain.Ticker
	mk := func(t domain.Ticker, drift, vol float64) {
		px := 100.0
		for _, d := range dates {
			c := px * math.Exp(drift+r.NormFloat64()*vol)
			series[t] = append(series[t], domain.Candle{Ticker: t, Start: d, End: d.Add(24 * time.Hour), Open: px, High: math.Max(px, c), Low: math.Min(px, c), Close: c, Volume: 1e6})
			px = c
		}
	}
	mk("SPY", 0.0003, 0.008)
	for i := 0; i < n; i++ {
		t := domain.Ticker(fmt.Sprintf("N%02d", i))       // sorts in drift order
		drift := -0.0012 + 0.0024*float64(i)/float64(n-1) // from losers to winners, ±30% a year
		mk(t, drift, 0.008)
		names = append(names, t)
	}
	return series, names
}

func TestRankSleeveRanksAndChargesCosts(t *testing.T) {
	series, names := trendingUniverse(4, 30, 1600)
	run := func(mult float64) RankResult {
		r, err := RunRank(RankConfig{Signal: Momentum12_1, Series: series, Candidates: names, Benchmark: "SPY",
			TopFraction: 0.2, MinNames: 3, MaxNames: 6, StartingCash: 100000, CostMultiple: mult, Trials: 4})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r1, r2 := run(1), run(2)
	if len(r1.Months) < 36 {
		t.Fatalf("expected years of rebalances, got %d", len(r1.Months))
	}
	// Persistent drifts: the long-short spread must be strongly positive.
	if r1.SpreadTStat < 3 {
		t.Errorf("long-short t-stat %.2f should be large on persistent drifts", r1.SpreadTStat)
	}
	// The top slice should be drawn from the high-drift names.
	for _, h := range r1.Current {
		if h.Ticker < "N15" {
			t.Errorf("current slice holds a low-drift name %s", h.Ticker)
		}
	}
	if !(r2.TotalCosts > r1.TotalCosts*1.9) {
		t.Errorf("doubling costs should double the charge: %.2f vs %.2f", r1.TotalCosts, r2.TotalCosts)
	}
	if !(r2.Metrics.CAGR < r1.Metrics.CAGR) {
		t.Errorf("higher costs must not raise CAGR: %.4f vs %.4f", r2.Metrics.CAGR, r1.Metrics.CAGR)
	}
	if r1.Metrics.DeflatedSharpe > r1.Metrics.Sharpe {
		t.Errorf("deflated Sharpe %.2f exceeds Sharpe %.2f", r1.Metrics.DeflatedSharpe, r1.Metrics.Sharpe)
	}
	if r1.Benchmark.Volatility == 0 || len(r1.ByYear) < 3 {
		t.Errorf("benchmark or yearly table missing: %+v", r1.Benchmark)
	}
	// Membership gating: exclude the top names and they must not appear.
	excluded := map[domain.Ticker]bool{names[len(names)-1]: true, names[len(names)-2]: true}
	r3, err := RunRank(RankConfig{Signal: Momentum12_1, Series: series, Candidates: names, Benchmark: "SPY",
		Member: func(tk domain.Ticker, _ time.Time) bool { return !excluded[tk] }, TopFraction: 0.2, MinNames: 3, MaxNames: 6})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r3.Months {
		for _, h := range m.Holdings {
			if excluded[h.Ticker] {
				t.Fatalf("membership gate failed: %s held on %s", h.Ticker, m.At)
			}
		}
	}
	// Determinism.
	if a, b := run(1), run(1); a.Metrics != b.Metrics {
		t.Error("the sleeve is not deterministic")
	}
}

func TestRankFormationHasNoLookAhead(t *testing.T) {
	series, names := trendingUniverse(8, 12, 900)
	full, err := RunRank(RankConfig{Signal: Momentum6_1, Series: series, Candidates: names, Benchmark: "SPY", TopFraction: 0.25, MinNames: 2, MaxNames: 4})
	if err != nil {
		t.Fatal(err)
	}
	cut := full.Months[len(full.Months)/2].At
	// Truncate every series at the cut and rerun: the slices up to the cut
	// must be identical, since nothing after the cut was used to form them.
	trunc := map[domain.Ticker][]domain.Candle{}
	for tk, bars := range series {
		for _, c := range bars {
			if !c.Start.After(cut) {
				trunc[tk] = append(trunc[tk], c)
			}
		}
	}
	part, err := RunRank(RankConfig{Signal: Momentum6_1, Series: trunc, Candidates: names, Benchmark: "SPY", TopFraction: 0.25, MinNames: 2, MaxNames: 4})
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range part.Months {
		if i >= len(full.Months) || !m.At.Equal(full.Months[i].At) {
			break
		}
		for j, h := range m.Holdings {
			if h.Ticker != full.Months[i].Holdings[j].Ticker {
				t.Fatalf("slice at %s differs with future data present: %v vs %v", m.At, m.Holdings, full.Months[i].Holdings)
			}
		}
	}
}

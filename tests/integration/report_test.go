package integration

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/backtest"
	"github.com/udaykishoreresu/quantos/internal/config"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/ingest"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
	"github.com/udaykishoreresu/quantos/internal/pipeline"
	"github.com/udaykishoreresu/quantos/internal/universe"
)

// dailyFixture writes a small replay directory of synthetic *daily* bars in
// the layout `quantos ingest` produces, with a membership table that admits
// one candidate only from a date inside the schedule. It is a random walk
// with a market factor; it exists to prove the report's mechanics, not to
// show an edge, and the assertions below are structural for that reason.
func dailyFixture(t *testing.T) (dir string, membership string, first, last time.Time) {
	t.Helper()
	dir = t.TempDir()
	r := rand.New(rand.NewSource(11))
	first = time.Date(2016, 1, 4, 0, 0, 0, 0, time.UTC)
	last = time.Date(2021, 12, 31, 0, 0, 0, 0, time.UTC)

	var days []time.Time
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days = append(days, d)
		}
	}
	mkt := make([]float64, len(days))
	for i := range mkt {
		mkt[i] = r.NormFloat64()*0.009 + 0.0003
	}
	type spec struct {
		beta, vol, px, vol0 float64
	}
	names := map[domain.Ticker]spec{
		"SPY": {1, 0.002, 200, 80e6}, "QQQ": {1.1, 0.004, 120, 40e6}, "IWM": {1.1, 0.006, 110, 25e6},
		"TLT": {-0.3, 0.005, 120, 10e6}, "HYG": {0.4, 0.003, 85, 20e6}, "GLD": {0, 0.008, 120, 8e6},
		"XLK": {1.1, 0.005, 50, 10e6}, "XLF": {1.1, 0.006, 25, 40e6}, "XLE": {0.9, 0.01, 60, 20e6},
		"AAPL": {1.2, 0.012, 100, 60e6}, "MSFT": {1.0, 0.011, 60, 30e6}, "JPM": {1.1, 0.012, 65, 15e6},
		"XOM": {0.8, 0.011, 80, 20e6}, "LATE": {1.0, 0.013, 40, 10e6},
	}
	var series []domain.Candle
	manifest := ingest.Manifest{Source: "fixture", Adjustment: "none", Interval: domain.Interval1d,
		Start: first, End: last, Series: map[domain.Ticker]ingest.Range{}}
	for tk, sp := range names {
		px := sp.px
		var bars []domain.Candle
		for i, day := range days {
			ret := sp.beta*mkt[i] + r.NormFloat64()*sp.vol
			o := px * (1 + r.NormFloat64()*sp.vol*0.2)
			c := px * math.Exp(ret)
			hi := math.Max(o, c) * (1 + math.Abs(r.NormFloat64())*sp.vol*0.3)
			lo := math.Min(o, c) * (1 - math.Abs(r.NormFloat64())*sp.vol*0.3)
			bars = append(bars, domain.Candle{Ticker: tk, Start: day, End: day.Add(24 * time.Hour),
				Open: o, High: hi, Low: lo, Close: c, Volume: sp.vol0 * math.Exp(r.NormFloat64()*0.3)})
			px = c
		}
		if err := ingest.WriteReplayCSV(filepath.Join(dir, string(tk)+".csv"), bars); err != nil {
			t.Fatal(err)
		}
		manifest.Series[tk] = ingest.Range{First: first, Last: last, Bars: len(bars), AvgNotional30: sp.px * sp.vol0}
		series = append(series, bars...)
	}
	// VIX: a volume-less index level.
	var vix []domain.Candle
	for _, day := range days {
		v := 12 + math.Abs(r.NormFloat64())*10
		vix = append(vix, domain.Candle{Ticker: "VIX", Start: day, End: day.Add(24 * time.Hour),
			Open: v, High: v * 1.02, Low: v * 0.98, Close: v})
	}
	if err := ingest.WriteReplayCSV(filepath.Join(dir, "VIX.csv"), vix); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "MANIFEST.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Not inside the replay directory: the replay provider reads every CSV
	// it finds there and would refuse a table without bar columns.
	membership = filepath.Join(t.TempDir(), "membership.csv")
	table := "ticker,start,end\nAAPL,2000-01-01,\nMSFT,2000-01-01,\nJPM,2000-01-01,\nXOM,2000-01-01,2019-06-01\nLATE,2020-06-01,\n"
	if err := os.WriteFile(membership, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, membership, first, last
}

func newDailyApp(t *testing.T, dir string, members string) (*app.App, *universe.Membership) {
	t.Helper()
	root := repoRoot(t)
	cfg, err := config.Load(root + "/config/quantos.daily.yaml")
	if err != nil {
		t.Fatalf("configuration: %v", err)
	}
	cfg.Strategies.Dir = root + "/strategies"
	cfg.Predict.ArtifactDir = "" // rule prior only: the artifact is a 60-minute model
	cfg.Market.Provider = "replay"
	cfg.Market.ReplayDir = dir
	cfg.Store.Driver = "memory"
	cfg.Bus.Driver = "memory"
	cfg.LLM.Provider = "mock"
	cfg.Auth.Enabled = false
	cfg.Log.Level = "error"
	cfg.HTTP.Addr = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("configuration: %v", err)
	}
	cfg.Hash = cfg.ComputeHash()
	a, err := app.New(context.Background(), cfg, app.Options{WithHTTP: false})
	if err != nil {
		t.Fatalf("start-up: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	m, err := universe.LoadMembership(members)
	if err != nil {
		t.Fatal(err)
	}
	a.Universe.SetMembership(m)
	for _, tk := range []domain.Ticker{"AAPL", "MSFT", "JPM", "XOM", "LATE"} {
		s, _ := a.Universe.Get(tk)
		s.AvgNotional, s.AvgVolume = 2e9, 20e6
		a.Universe.Add(s)
	}
	return a, m
}

func TestTheReportRunsOutOfSampleOnDailyReplay(t *testing.T) {
	dir, members, first, last := dailyFixture(t)
	a, _ := newDailyApp(t, dir, members)
	rp := a.Provider.(*marketdata.Replay)
	all := rp.Tickers()
	ctx := context.Background()

	warmup := time.Duration(float64(a.Cfg.Features.Warmup)*1.55) * 24 * time.Hour
	folds, err := backtest.BuildFolds(backtest.WalkForwardConfig{
		Start: first.Add(warmup), End: last,
		TrainWindow: 365 * 24 * time.Hour, ValidWindow: 60 * 24 * time.Hour, TestWindow: 240 * 24 * time.Hour,
		MaxHorizon: 5 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	barsFor := func(s, e time.Time) ([]domain.Candle, error) {
		return rp.GetHistoricalBars(ctx, marketdata.BarRequest{Tickers: all, Interval: domain.Interval1d, Start: s, End: e})
	}

	type gate struct{ rejected, signalled int }
	gates := map[domain.Ticker]*gate{"LATE": {}, "XOM": {}}
	observer := func(bar domain.Candle, res pipeline.Result) {
		g, ok := gates[bar.Ticker]
		if !ok {
			return
		}
		outside := (bar.Ticker == "LATE" && bar.End.Before(time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC))) ||
			(bar.Ticker == "XOM" && !bar.End.Before(time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)))
		if !outside {
			return
		}
		if res.Rejection != nil && res.Rejection.Stage == "universe" {
			g.rejected++
		}
		if res.Signal != nil || res.Prediction != nil {
			g.signalled++
		}
	}
	run := func() backtest.Report {
		rep, err := a.Backtest.RunReport(ctx, backtest.ReportConfig{
			Base: backtest.Request{
				Name: "test", StrategyID: "momentum", Interval: domain.Interval1d, Seed: 7,
				StartingCash: 100_000, Costs: domain.DefaultCostModel(), Trials: 3,
				MacroTickers: universe.MacroTickers(), FillAtNextOpen: true, Observer: observer,
			},
			Folds: folds, Warmup: warmup, CostMultipliers: []float64{1, 2}, Benchmark: "SPY",
		}, barsFor)
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		return rep
	}

	rep := run()
	if len(rep.Costs) != 2 || rep.Costs[0].Multiplier != 1 || rep.Costs[1].Multiplier != 2 {
		t.Fatalf("expected two cost scenarios, got %+v", rep.Costs)
	}
	if len(folds) < 2 || rep.Costs[0].WalkForward.FoldCount != len(folds) {
		t.Fatalf("expected %d folds, report has %d", len(folds), rep.Costs[0].WalkForward.FoldCount)
	}
	for _, f := range rep.Costs[0].WalkForward.Folds {
		if f.Error != "" {
			t.Fatalf("fold %d failed: %s", f.Fold.Index, f.Error)
		}
		if !f.Run.Start.Equal(f.Fold.TestStart) {
			t.Errorf("fold %d run starts %s, want the test window start %s", f.Fold.Index, f.Run.Start, f.Fold.TestStart)
		}
		if len(f.Run.EquityCurve) > 0 && f.Run.EquityCurve[0].At.Before(f.Fold.TestStart) {
			t.Errorf("fold %d equity curve includes warm-up points from %s", f.Fold.Index, f.Run.EquityCurve[0].At)
		}
		if f.Run.EventsProcessed == 0 {
			t.Errorf("fold %d processed no bars", f.Fold.Index)
		}
	}

	// Predictions are scored on every candidate bar, so there must be many of
	// them, with the 1d horizon resolving on the next session.
	c1 := rep.Costs[0]
	ev, ok := c1.Evaluation["1d"]
	if !ok || ev.Samples < 500 {
		t.Fatalf("expected hundreds of scored 1d predictions, got %+v", ev)
	}
	if ev.BaseRate <= 0 || ev.BaseRate > 1 {
		t.Errorf("base rate %.3f is not a probability", ev.BaseRate)
	}
	if c1.Predictions < ev.Samples {
		t.Errorf("tracked %d predictions but scored %d", c1.Predictions, ev.Samples)
	}

	// The membership gate: a name outside its interval is rejected at the
	// universe stage and never reaches prediction or signal.
	for tk, g := range gates {
		if g.rejected == 0 {
			t.Errorf("%s: no universe-stage rejections outside its membership interval", tk)
		}
		if g.signalled != 0 {
			t.Errorf("%s: %d predictions or signals outside its membership interval", tk, g.signalled)
		}
	}

	// Buy-and-hold over the same windows is always computable.
	if rep.Benchmark.Error != "" || rep.Benchmark.Metrics.Volatility == 0 {
		t.Errorf("benchmark missing: %+v", rep.Benchmark)
	}

	// Doubling costs cannot make the book cheaper.
	a1, a2 := rep.Costs[0].WalkForward.Aggregate, rep.Costs[1].WalkForward.Aggregate
	if a1.Trades > 0 && !(a2.TotalCosts > a1.TotalCosts) {
		t.Errorf("costs did not double: x1 $%.2f, x2 $%.2f over %d trades", a1.TotalCosts, a2.TotalCosts, a1.Trades)
	}
	if rep.Verdict.Summary == "" || len(rep.Notes) == 0 {
		t.Error("report has no verdict or notes")
	}

	// The same inputs produce the same report: the slippage model is seeded
	// and the stream order is total.
	again := run()
	j1, _ := json.Marshal(rep.Costs[0].WalkForward.Aggregate)
	j2, _ := json.Marshal(again.Costs[0].WalkForward.Aggregate)
	if string(j1) != string(j2) || again.Costs[0].Outcomes != rep.Costs[0].Outcomes {
		t.Errorf("report is not deterministic:\n%s\n%s", j1, j2)
	}

	if md := backtest.RenderMarkdown(rep); len(md) < 500 {
		t.Errorf("markdown rendering is suspiciously short: %d bytes", len(md))
	}
}

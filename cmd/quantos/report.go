package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// runReport runs the net-of-cost walk-forward over a replay directory of real
// history and writes the result as JSON and Markdown.
//
// What this is: the same pipeline the demo runs, pointed at real daily bars,
// with the one question that matters asked properly — does anything survive
// out of sample, after costs, against buy-and-hold?
//
// What this is not: a model refit per fold. The Go walk-forward schedules
// disjoint, embargoed test windows for a *fixed* configuration (the shipped
// strategies and, if loaded, the shipped artifact). Refitting lives in
// ml/evaluation/walk_forward.py and consumes features this binary exports.
func runReport() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.daily.yaml"), "configuration file")
		data       = flag.String("data", "", "replay directory written by `quantos ingest` (default: market.replay_dir)")
		members    = flag.String("membership", "", "point-in-time membership CSV (scripts/build_membership.py)")
		strategy   = flag.String("strategy", "", "strategy id (default: signals.default_strategy)")
		startS     = flag.String("start", "", "first date of the schedule (default: earliest bar)")
		endS       = flag.String("end", "", "last date of the schedule (default: latest bar)")
		trainS     = flag.String("train", "2y", "nominal training window per fold (e.g. 2y, 500d, 720h)")
		validS     = flag.String("valid", "90d", "nominal validation window per fold")
		testS      = flag.String("test", "180d", "out-of-sample test window per fold")
		stepS      = flag.String("step", "0", "fold advance (default: the test window, disjoint folds)")
		embargoS   = flag.String("embargo", "0", "gap before each test window (floored at twice the longest horizon)")
		anchored   = flag.Bool("anchored", false, "expanding rather than rolling schedule")
		costs      = flag.String("costs", "1,2", "cost multipliers to run, comma-separated")
		trials     = flag.Int("trials", 1, "configurations tried in total, for the deflated Sharpe ratio")
		bench      = flag.String("benchmark", "SPY", "buy-and-hold comparison ticker")
		noModel    = flag.Bool("no-model", false, "serve from the rule prior only; do not load the ML artifact")
		closeFill  = flag.Bool("fill-at-close", false, "fill at the last close instead of the next open (optimistic on daily bars)")
		tickers    = flag.String("tickers", "", "restrict candidates to these tickers (comma-separated)")
		seed       = flag.Int64("seed", 20260827, "deterministic seed for the slippage model")
		outJSON    = flag.String("out", "reports/walkforward.json", "write the report as JSON here")
		outMD      = flag.String("md", "reports/walkforward.md", "write the report as Markdown here")
		explain    = flag.Int("explain", 0, "print this many declined candidates with their risk checks, to see why nothing trades")
	)
	flag.Parse()

	train, valid, test, step, embargo := mustSpan("train", *trainS), mustSpan("valid", *validS),
		mustSpan("test", *testS), mustSpan("step", *stepS), mustSpan("embargo", *embargoS)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	cfg.Market.Provider = "replay"
	if *data != "" {
		cfg.Market.ReplayDir = *data
	}
	if cfg.Market.BarInterval == "" {
		cfg.Market.BarInterval = domain.Interval1d
	}
	cfg.Store.Driver = "memory"
	cfg.Bus.Driver = "memory"
	cfg.Log.Level = "warn"
	if *strategy != "" {
		cfg.Signals.DefaultStrategy = *strategy
	}
	if *noModel {
		cfg.Predict.ArtifactDir = ""
	}
	cfg.Market.Seed = *seed
	interval := cfg.Market.BarInterval

	manifest, err := readManifest(cfg.Market.ReplayDir)
	if err != nil {
		fatal("%v (run `quantos ingest` first)", err)
	}

	ctx := context.Background()
	a, err := app.New(ctx, cfg, app.Options{Now: manifest.Start})
	if err != nil {
		fatal("start-up: %v", err)
	}
	defer a.Close()
	rp, ok := a.Provider.(*marketdata.Replay)
	if !ok {
		fatal("provider is %s, expected replay", a.Provider.Name())
	}

	// --- Universe: what has data, what is a candidate, how liquid it is ----
	contextSyms := map[domain.Ticker]bool{}
	for _, t := range universe.MacroTickers() {
		contextSyms[t] = true
	}
	for _, t := range universe.SectorETFs() {
		contextSyms[t] = true
	}
	restrict := map[domain.Ticker]bool{}
	for _, t := range strings.Split(*tickers, ",") {
		if t = strings.TrimSpace(t); t != "" {
			restrict[domain.NormalizeTicker(t)] = true
		}
	}
	var membership *universe.Membership
	if *members != "" {
		membership, err = universe.LoadMembership(*members)
		if err != nil {
			fatal("%v", err)
		}
		a.Universe.SetMembership(membership)
	}
	var candidates, all []domain.Ticker
	for _, t := range rp.Tickers() {
		rng, known := manifest.Series[t]
		if contextSyms[t] {
			all = append(all, t)
			continue
		}
		if len(restrict) > 0 && !restrict[t] {
			continue
		}
		if membership != nil && len(membership.Intervals(t)) == 0 {
			continue // data for a name the table never admits is context at best
		}
		s, ok := a.Universe.Get(t)
		if !ok {
			s = domain.Stock{Ticker: t, Currency: "USD", AssetClass: domain.AssetEquity, Tags: []string{"replay"}}
		}
		if known && rng.AvgNotional30 > 0 {
			// Liquidity measured from the data itself; the builtin fixture
			// values describe the simulator, not this history.
			s.AvgNotional, s.AvgVolume = rng.AvgNotional30, rng.AvgVolume30
		}
		a.Universe.Add(s)
		candidates = append(candidates, t)
		all = append(all, t)
	}
	if len(candidates) == 0 {
		fatal("no candidate instruments: the replay directory has %d series and none passed the universe filters", len(rp.Tickers()))
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	// --- Schedule -------------------------------------------------------------
	first, last := seriesBounds(manifest)
	if *startS != "" {
		if first, err = time.Parse("2006-01-02", *startS); err != nil {
			fatal("bad -start: %v", err)
		}
	}
	if *endS != "" {
		if last, err = time.Parse("2006-01-02", *endS); err != nil {
			fatal("bad -end: %v", err)
		}
	}
	maxHorizon := time.Duration(0)
	for _, h := range cfg.Predict.Horizons {
		if d := h.Duration(); d > maxHorizon {
			maxHorizon = d
		}
	}
	// Warm-up in calendar time: warmup_bars trading days plus slack for
	// weekends and holidays.
	warmup := time.Duration(float64(cfg.Features.Warmup)*1.55) * 24 * time.Hour
	folds, err := backtest.BuildFolds(backtest.WalkForwardConfig{
		Start: first.Add(warmup), End: last,
		TrainWindow: train, ValidWindow: valid, TestWindow: test, Step: step,
		Embargo: embargo, Anchored: *anchored, MaxHorizon: maxHorizon,
	})
	if err != nil {
		fatal("%v", err)
	}

	var multipliers []float64
	for _, s := range strings.Split(*costs, ",") {
		if s = strings.TrimSpace(s); s != "" {
			k, err := strconv.ParseFloat(s, 64)
			if err != nil || k <= 0 {
				fatal("bad -costs entry %q", s)
			}
			multipliers = append(multipliers, k)
		}
	}
	sort.Float64s(multipliers)

	fmt.Printf("report: %s on %s bars, %d candidates + %d context symbols, %d folds of %s test windows, costs x%v\n",
		cfg.Signals.DefaultStrategy, interval, len(candidates), len(all)-len(candidates), len(folds), *testS, multipliers)
	fmt.Printf("        data %s (%s, %s)\n", cfg.Market.ReplayDir, manifest.Source, manifest.Adjustment)
	if membership != nil {
		fmt.Printf("        membership %s (%d tickers)\n", membership.Source(), membership.Len())
	}
	if len(a.Models.All()) == 0 {
		fmt.Println("        model: none loaded; predictions use the rule prior")
	} else {
		fmt.Printf("        model: %s (trained on simulated history; see docs/ml/model-card.md)\n", cfg.Predict.ModelID)
	}

	barsFor := func(start, end time.Time) ([]domain.Candle, error) {
		return rp.GetHistoricalBars(ctx, marketdata.BarRequest{
			Tickers: all, Interval: interval, Start: start, End: end, Adjusted: true,
		})
	}
	var observer func(domain.Candle, pipeline.Result)
	if *explain > 0 {
		remaining := *explain
		seenStage := map[string]int{}
		inTest := func(at time.Time) bool {
			for _, f := range folds {
				if !at.Before(f.TestStart) && at.Before(f.TestEnd) {
					return true
				}
			}
			return false
		}
		observer = func(bar domain.Candle, res pipeline.Result) {
			if !inTest(bar.End) {
				return // warm-up bars are not traded and would mislead here
			}
			if res.Signal != nil && seenStage["signal"] < (*explain+4)/5 {
				seenStage["signal"]++
				fmt.Printf("\n[%s %s] SIGNAL %s weight %.3f confidence %.2f\n", bar.End.Format("2006-01-02"), bar.Ticker,
					res.Signal.Side, res.Signal.SuggestedWeight, res.Signal.Confidence)
				return
			}
			if remaining <= 0 || res.Macro || res.Rejection == nil || res.Rejection.Stage == "universe" {
				return
			}
			// Spread the examples across stages rather than printing the
			// same rejection a hundred times.
			if seenStage[res.Rejection.Stage] >= (*explain+4)/5 {
				return
			}
			seenStage[res.Rejection.Stage]++
			remaining--
			fmt.Printf("\n[%s %s] declined at %s: %s\n", bar.End.Format("2006-01-02"), bar.Ticker, res.Rejection.Stage, res.Rejection.Reason)
			if res.Prediction != nil && len(res.Prediction.Scenarios) > 0 {
				sc := res.Prediction.Scenarios[0]
				fmt.Printf("  prediction %s: up %.2f flat %.2f down %.2f (confidence %.2f, source %s)\n",
					sc.Horizon, sc.Dist.Up, sc.Dist.Flat, sc.Dist.Down, sc.Confidence, res.Prediction.Source)
			}
			if res.Risk != nil {
				fmt.Printf("  risk %s (score %.0f)\n", res.Risk.Decision, res.Risk.Score)
				for _, c := range res.Risk.Checks {
					if c.Status != domain.CheckPass || c.Decision != domain.RiskAllowPaperSignal {
						fmt.Printf("    %-22s %-6s %-18s %s %s\n", c.Name, c.Status, c.Decision, c.Reason, c.Skipped)
					}
				}
			}
		}
	}

	rep, err := a.Backtest.RunReport(ctx, backtest.ReportConfig{
		Base: backtest.Request{
			Name: "report", StrategyID: cfg.Signals.DefaultStrategy, Interval: interval, Seed: *seed,
			StartingCash: cfg.Paper.StartingCash, Costs: cfg.Paper.Costs, Trials: *trials,
			ConfigHash: cfg.Hash, CodeVersion: cfg.Version, MacroTickers: universe.MacroTickers(),
			FillAtNextOpen: !*closeFill, Observer: observer,
		},
		Folds: folds, Warmup: warmup, CostMultipliers: multipliers,
		Benchmark: domain.NormalizeTicker(*bench), CalibrationBins: cfg.Evaluation.CalibrationBins,
	}, barsFor)
	if err != nil {
		fatal("report: %v", err)
	}
	rep.Notes = append(rep.Notes, fmt.Sprintf("data: %s from %s, %s", cfg.Market.ReplayDir, manifest.Source, manifest.Adjustment))
	if membership == nil {
		rep.Notes = append(rep.Notes, "no membership table: the candidate set is whatever has data, which for a list of today's large caps is survivorship-biased")
	}

	printReport(rep)
	md := backtest.RenderMarkdown(rep)
	for path, body := range map[string][]byte{*outJSON: mustJSON(rep), *outMD: []byte(md)} {
		if path == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			fatal("output directory: %v", err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			fatal("write %s: %v", path, err)
		}
		fmt.Printf("wrote %s\n", path)
	}
}

func printReport(rep backtest.Report) {
	fmt.Println()
	fmt.Printf("%-18s %8s %7s %9s %8s %8s %8s %7s %10s\n", "costs", "signals", "trades", "return", "CAGR", "Sharpe", "maxDD", "folds+", "costs($)")
	for _, c := range rep.Costs {
		m := c.WalkForward.Aggregate
		fmt.Printf("%-18s %8d %7d %+8.2f%% %+7.2f%% %8.2f %7.2f%% %6.0f%% %10.0f\n",
			fmt.Sprintf("x%.1f", c.Multiplier), c.Signals, m.Trades, m.TotalReturn*100, m.CAGR*100, m.Sharpe, m.MaxDrawdown*100,
			c.WalkForward.Consistency*100, m.TotalCosts)
	}
	if rep.Benchmark.Error == "" {
		m := rep.Benchmark.Metrics
		fmt.Printf("%-18s %8s %7s %+8.2f%% %+7.2f%% %8.2f %7.2f%%\n", "buy&hold "+string(rep.Benchmark.Ticker), "—", "—",
			m.TotalReturn*100, m.CAGR*100, m.Sharpe, m.MaxDrawdown*100)
	}
	if len(rep.Costs) > 0 {
		c := rep.Costs[0]
		fmt.Println()
		horizons := make([]domain.Horizon, 0, len(c.Evaluation))
		for h := range c.Evaluation {
			horizons = append(horizons, h)
		}
		sort.Slice(horizons, func(i, j int) bool { return horizons[i].Duration() < horizons[j].Duration() })
		for _, h := range horizons {
			ev := c.Evaluation[h]
			fmt.Printf("predictions %-4s n=%-7d accuracy %.3f  base rate %.3f  lift %+.3f  Brier skill %+.3f\n",
				h, ev.Samples, ev.Accuracy, ev.BaseRate, ev.Accuracy-ev.BaseRate, ev.BrierSkill)
		}
	}
	fmt.Println()
	fmt.Printf("verdict: %s\n", rep.Verdict.Summary)
	for _, r := range rep.Verdict.Reasons {
		fmt.Printf("  - %s\n", r)
	}
	fmt.Println()
	fmt.Println(rep.Disclaimer)
}

func readManifest(dir string) (ingest.Manifest, error) {
	var m ingest.Manifest
	data, err := os.ReadFile(filepath.Join(dir, "MANIFEST.json"))
	if err != nil {
		return m, fmt.Errorf("no MANIFEST.json in %s", dir)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// seriesBounds returns the first and last dates covered by *every* series
// that has data, which is where a schedule can safely start and end.
func seriesBounds(m ingest.Manifest) (time.Time, time.Time) {
	var first, last time.Time
	for _, r := range m.Series {
		if first.IsZero() || r.First.Before(first) {
			first = r.First
		}
		if last.IsZero() || r.Last.After(last) {
			last = r.Last
		}
	}
	return first, last
}

func mustJSON(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatal("encode: %v", err)
	}
	return data
}

// parseSpan accepts Go durations plus the calendar units people actually use
// for backtests: "180d", "2y", "6m" (months, 30 days), "3w".
func parseSpan(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	unit := s[len(s)-1]
	mult := map[byte]time.Duration{'d': 24 * time.Hour, 'w': 7 * 24 * time.Hour, 'y': 365 * 24 * time.Hour}
	if d, ok := mult[unit]; ok {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("bad span %q", s)
		}
		return time.Duration(n * float64(d)), nil
	}
	if strings.HasSuffix(s, "mo") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "mo"), 64)
		if err != nil {
			return 0, fmt.Errorf("bad span %q", s)
		}
		return time.Duration(n * float64(30*24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func mustSpan(flagName, s string) time.Duration {
	d, err := parseSpan(s)
	if err != nil {
		fatal("-%s: %v", flagName, err)
	}
	return d
}

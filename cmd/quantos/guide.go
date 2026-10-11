package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/config"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/guide"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
	"github.com/udaykishoreresu/quantos/internal/universe"
)

// runGuide dispatches the investor-facing subcommands. They read the same
// replay directory the research tools read and produce the same kind of
// output: a Markdown page a person reads and a JSON file a program reads,
// every figure with its window and method, no advice.
func runGuide() {
	if len(os.Args) < 2 {
		guideUsage()
		os.Exit(2)
	}
	sub := os.Args[1]
	os.Args = append(os.Args[:1], os.Args[2:]...)
	switch sub {
	case "book":
		guideBook()
	case "vol":
		guideVol()
	case "plan":
		guidePlan()
	case "-h", "--help", "help":
		guideUsage()
	default:
		fmt.Fprintf(os.Stderr, "quantos guide: unknown subcommand %q\n\n", sub)
		guideUsage()
		os.Exit(2)
	}
}

func guideUsage() {
	fmt.Fprint(os.Stderr, `usage: quantos guide <command> [flags]

commands:
  book   mark a holdings file against real history and measure its exposure, concentration,
         correlation and drawdown against the configured caps
  vol    fit and evaluate the realised-volatility forecaster out of sample
  plan   the arithmetic of a savings goal: contributions, horizon, bootstrapped outcomes

Every output is a measurement with a window and a method. None of it is advice.
`)
}

// loadReplay opens a replay directory and returns per-instrument history.
func loadReplay(dir string) (*marketdata.Replay, guide.Series) {
	rp := marketdata.NewReplay(dir, domain.Interval1d)
	if err := rp.Load(); err != nil {
		fatal("%v", err)
	}
	series := guide.Series{}
	for _, t := range rp.Tickers() {
		bars, err := rp.GetHistoricalBars(context.Background(), marketdata.BarRequest{Tickers: []domain.Ticker{t}, Interval: domain.Interval1d})
		if err == nil && len(bars) > 0 {
			series[t] = bars
		}
	}
	return rp, series
}

func guideBook() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.daily.yaml"), "configuration file (risk caps come from its risk block)")
		data       = flag.String("data", "data/daily", "replay directory from `quantos ingest`")
		holdings   = flag.String("holdings", "examples/holdings/model-100k.csv", "holdings CSV: ticker,quantity[,cost_basis][,note]; a CASH row carries cash")
		bench      = flag.String("benchmark", "SPY", "benchmark ticker")
		asOfS      = flag.String("as-of", "", "mark date (default: last bar)")
		outJSON    = flag.String("out", "reports/book.json", "JSON output")
		outMD      = flag.String("md", "reports/book.md", "Markdown output")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	h, err := guide.LoadHoldings(*holdings)
	if err != nil {
		fatal("%v", err)
	}
	_, series := loadReplay(*data)
	var asOf time.Time
	if *asOfS != "" {
		if asOf, err = time.Parse("2006-01-02", *asOfS); err != nil {
			fatal("bad -as-of: %v", err)
		}
	}
	u := universe.Builtin()
	caps := guide.Caps{
		MaxPosition: cfg.Risk.MaxPositionWeight, SoftPosition: cfg.Risk.SoftPositionWeight,
		MaxSector: cfg.Risk.MaxSectorWeight, SoftSector: cfg.Risk.SoftSectorWeight,
		MaxCorrelated: cfg.Risk.MaxCorrelatedExposure, SoftCorrelated: cfg.Risk.SoftCorrelatedExposure,
		MaxDrawdown: cfg.Risk.MaxDrawdown, DrawdownWarn: cfg.Risk.DrawdownWarn,
		CorrelationThreshold: 0.60,
	}
	book, err := guide.Assess(h, series, guide.BookConfig{
		Caps: caps, Benchmark: domain.NormalizeTicker(*bench), AsOf: asOf,
		Lookup: func(t domain.Ticker) (domain.Stock, bool) { return u.Get(t) },
	})
	if err != nil {
		fatal("%v", err)
	}

	fmt.Printf("book %s marked %s: equity $%.0f, %d positions, effective %.1f, cash %.0f%%\n",
		book.Name, book.AsOf.Format("2006-01-02"), book.Equity, len(book.Positions), book.EffectiveN, book.CashWeight*100)
	for _, f := range book.Flags {
		fmt.Printf("  %-4s %-15s %s\n", f.Level, f.Check, f.Detail)
	}
	for _, t := range book.Trailing {
		fmt.Printf("  %-14s return %+6.1f%%  vol %5.1f%%  Sharpe %5.2f  maxDD %5.1f%%  beta %.2f   (%s %+6.1f%%, maxDD %5.1f%%)\n",
			t.Window.Label, t.Return*100, t.Volatility*100, t.Sharpe, t.MaxDrawdown*100, t.Beta, book.Benchmark, t.BenchmarkReturn*100, t.BenchmarkMaxDD*100)
	}
	for _, s := range book.Stress {
		if s.Days > 0 {
			fmt.Printf("  %-22s book %+6.1f%%   %s %+6.1f%%\n", s.Window.Label, s.Return*100, book.Benchmark, s.BenchmarkReturn*100)
		}
	}
	fmt.Println()
	fmt.Println(book.Disclaimer)
	writeGuideOutputs(*outJSON, *outMD, book, guide.RenderBook(book))
}

func writeGuideOutputs(outJSON, outMD string, v any, md string) {
	write := func(path string, body []byte) {
		if path == "" {
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			fatal("output directory: %v", err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			fatal("write %s: %v", path, err)
		}
		fmt.Printf("wrote %s\n", path)
	}
	write(outMD, []byte(md))
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatal("encode: %v", err)
	}
	write(outJSON, data)
}

func guideVol() {
	var (
		data    = flag.String("data", "data/daily", "replay directory from `quantos ingest`")
		tickers = flag.String("tickers", "", "restrict the forecast table to these tickers (comma-separated); evaluation always uses every series")
		outJSON = flag.String("out", "reports/vol.json", "JSON output")
		outMD   = flag.String("md", "reports/vol.md", "Markdown output")
	)
	flag.Parse()
	_, series := loadReplay(*data)

	// Feed every bar in time order so the pooled fit sees the cross-section
	// the way a live process would.
	var all []domain.Candle
	for t, bars := range series {
		if t == "VIX" || t == "DXY" {
			continue // index levels, not tradeable variance
		}
		all = append(all, bars...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Start.Equal(all[j].Start) {
			return all[i].Start.Before(all[j].Start)
		}
		return all[i].Ticker < all[j].Ticker
	})
	vf := guide.NewVolForecaster()
	for _, c := range all {
		vf.Observe(c)
	}
	ev := vf.Evaluate()

	want := map[domain.Ticker]bool{}
	for _, t := range strings.Split(*tickers, ",") {
		if t = strings.TrimSpace(t); t != "" {
			want[domain.NormalizeTicker(t)] = true
		}
	}
	type row struct {
		Ticker   domain.Ticker `json:"ticker"`
		Forecast float64       `json:"forecast_annual_vol"`
		Trailing float64       `json:"trailing_22d_annual_vol"`
	}
	var table []row
	for t, bars := range series {
		if len(want) > 0 && !want[t] {
			continue
		}
		if t == "VIX" || t == "DXY" {
			continue
		}
		if f, ok := vf.Forecast(t); ok {
			n := len(bars)
			s := 0.0
			for i := n - 22; i < n; i++ {
				r := math.Log(bars[i].Close / bars[i-1].Close)
				s += r * r
			}
			table = append(table, row{t, f, math.Sqrt(s / 22 * 252)})
		}
	}
	sort.Slice(table, func(i, j int) bool { return table[i].Forecast > table[j].Forecast })

	fmt.Printf("volatility forecaster: %d matured out-of-sample forecasts %s .. %s, fitted on %d labels\n",
		ev.Samples, ev.From.Format("2006-01-02"), ev.To.Format("2006-01-02"), ev.LabelsFitted)
	fmt.Printf("  HAR   R² (log var) %.3f   QLIKE %.4f   MAE %.1f vol points   bias %+.1f\n", ev.R2Log, ev.QLIKE, ev.MAEVol, ev.Bias)
	fmt.Printf("  naive R² (log var) %.3f   QLIKE %.4f   MAE %.1f vol points   (last month's vol as the forecast)\n", ev.R2LogNaive, ev.QLIKENaive, ev.MAEVolNaive)
	fmt.Printf("  coefficients: intercept %.3f  daily %.3f  weekly %.3f  monthly %.3f\n", ev.Coefficients[0], ev.Coefficients[1], ev.Coefficients[2], ev.Coefficients[3])
	fmt.Printf("  verdict: %s\n\n", ev.Verdict)
	fmt.Printf("%-8s %10s %12s\n", "ticker", "forecast", "trailing22d")
	for i, r := range table {
		if i >= 15 && len(want) == 0 {
			fmt.Printf("  ... %d more in the report\n", len(table)-i)
			break
		}
		fmt.Printf("%-8s %9.1f%% %11.1f%%\n", r.Ticker, r.Forecast*100, r.Trailing*100)
	}
	fmt.Println()
	fmt.Println(domain.Disclaimer)

	out := struct {
		Evaluation guide.VolEvaluation `json:"evaluation"`
		Forecasts  []row               `json:"forecasts"`
		Disclaimer string              `json:"disclaimer"`
	}{ev, table, domain.Disclaimer}
	var md strings.Builder
	fmt.Fprintf(&md, "# Volatility forecast\n\nPooled HAR on log realised variance, fitted online with no look-ahead; %d matured out-of-sample forecasts from %s to %s.\n\n> %s\n\n",
		ev.Samples, ev.From.Format("2006-01-02"), ev.To.Format("2006-01-02"), domain.Disclaimer)
	fmt.Fprintf(&md, "**%s**\n\n| forecaster | R² (log variance) | QLIKE | MAE (vol points) |\n|---|---|---|---|\n| HAR | %.3f | %.4f | %.1f |\n| persistence (last 22 days) | %.3f | %.4f | %.1f |\n\n",
		ev.Verdict, ev.R2Log, ev.QLIKE, ev.MAEVol, ev.R2LogNaive, ev.QLIKENaive, ev.MAEVolNaive)
	fmt.Fprintf(&md, "Coefficients: intercept %.3f, daily %.3f, weekly %.3f, monthly %.3f (%d labels). Bias %+.1f vol points.\n\n", ev.Coefficients[0], ev.Coefficients[1], ev.Coefficients[2], ev.Coefficients[3], ev.LabelsFitted, ev.Bias)
	md.WriteString("## Forecast deciles vs realised\n\n| decile | mean forecast | mean realised | n |\n|---|---|---|---|\n")
	for _, d := range ev.DecileTable {
		fmt.Fprintf(&md, "| %d | %.1f%% | %.1f%% | %d |\n", d.Decile, d.MeanForecast*100, d.MeanRealised*100, d.Count)
	}
	md.WriteString("\n## By year\n\n| year | n | R² (log) | QLIKE HAR | QLIKE persistence |\n|---|---|---|---|---|\n")
	for _, y := range ev.ByYear {
		fmt.Fprintf(&md, "| %d | %d | %.3f | %.4f | %.4f |\n", y.Year, y.Samples, y.R2Log, y.QLIKE, y.QLIKENaive)
	}
	md.WriteString("\n## Current forecasts (next 21 trading days, annualised)\n\n| ticker | forecast | trailing 22d |\n|---|---|---|\n")
	for _, r := range table {
		fmt.Fprintf(&md, "| %s | %.1f%% | %.1f%% |\n", r.Ticker, r.Forecast*100, r.Trailing*100)
	}
	fmt.Fprintf(&md, "\n_Generated %s._\n", time.Now().UTC().Format(time.RFC3339))
	writeGuideOutputs(*outJSON, *outMD, out, md.String())
}

func guidePlan() {
	var (
		data       = flag.String("data", "data/daily", "replay directory from `quantos ingest`")
		bench      = flag.String("benchmark", "SPY", "series to bootstrap returns from")
		capital    = flag.Float64("capital", 100_000, "starting capital")
		monthly    = flag.Float64("monthly", 2_000, "monthly contribution")
		years      = flag.Int("years", 20, "horizon in years")
		income     = flag.Float64("income", 50_000, "target annual income ($200 a day is $50,000 over 250 trading days); 0 for wealth only")
		withdrawal = flag.Float64("withdrawal", 0.04, "sustainable withdrawal rate")
		haircut    = flag.Float64("haircut", 0.04, "annual return haircut applied to every sampled return (the 2010–2026 sample is a strong one; 4% brings it near the long-run nominal average)")
		paths      = flag.Int("paths", 10000, "Monte Carlo paths")
		seed       = flag.Int64("seed", 20260827, "seed")
		outJSON    = flag.String("out", "reports/plan.json", "JSON output")
		outMD      = flag.String("md", "reports/plan.md", "Markdown output")
	)
	flag.Parse()
	_, series := loadReplay(*data)
	b := domain.NormalizeTicker(*bench)
	rets, from, to := guide.MonthlyReturns(series[b])
	res, err := guide.Plan(rets, from, to, guide.PlanInput{
		Capital: *capital, Monthly: *monthly, Years: *years, AnnualIncome: *income,
		WithdrawalRate: *withdrawal, Haircut: *haircut, Benchmark: b, Paths: *paths, Seed: *seed,
	})
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("plan: %s sample %s..%s, %.1f%%/yr at %.1f%% vol (worst 12m %.1f%%), using %.1f%% after haircut\n",
		b, from.Format("2006-01"), to.Format("2006-01"), res.SampleAnnualReturn*100, res.SampleAnnualVol*100, res.SampleWorstYear*100, res.UsedAnnualReturn*100)
	fmt.Printf("%s\n\n", res.Verdict)
	fmt.Printf("%4s %12s %12s %12s %12s %12s %9s %9s\n", "year", "contributed", "P10", "P50", "P90", "fixed-rate", "P(sust)", "P(ret)")
	for _, y := range res.Years {
		if y.Year%2 == 1 || y.Year == len(res.Years) {
			fmt.Printf("%4d %12s %12s %12s %12s %12s %8.0f%% %8.0f%%\n", y.Year, money(y.Contributed), money(y.P10), money(y.P50), money(y.P90), money(y.Fixed), y.ProbSustainable*100, y.ProbFromReturns*100)
		}
	}
	fmt.Println()
	fmt.Println(domain.Disclaimer)
	writeGuideOutputs(*outJSON, *outMD, res, guide.RenderPlan(res))
}

func money(v float64) string { return guide.Money(v) }

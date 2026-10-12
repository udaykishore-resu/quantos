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

	"github.com/udaykishoreresu/quantos/internal/backtest"
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
	case "rank":
		guideRank()
	case "brief":
		guideBrief()
	case "import-robinhood":
		guideImportRobinhood()
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
  rank   the cross-sectional monthly ranking sleeve, every variant, net of costs, against the index
  brief  one page a day: market context, your book against the caps, forecast volatility per
         holding, what the sleeve would hold, and how every earlier brief's calls turned out
  import-robinhood
         rebuild a holdings file from a Robinhood account-activity CSV, for book and brief

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
		capsName   = flag.String("caps", "personal", capsUsage)
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
	caps := guideCaps(*capsName, cfg)
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
	if len(book.Exposures) > 0 {
		fmt.Printf("  largest company exposures, through funds (constituents %s):\n", guide.LookThroughAsOf)
		for i, e := range book.Exposures {
			if i == 5 {
				break
			}
			fmt.Printf("    %-6s %5.1f%%  (direct %4.1f%%, via %s %4.1f%%)\n", e.Ticker, e.Total*100, e.Direct*100, orDash(e.Funds), e.ViaFunds*100)
		}
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

// rankVariants are the formation rules run together; the deflated Sharpe
// counts all of them as trials, which is what they are.
var rankVariants = []struct {
	signal     backtest.RankSignal
	inverseVol bool
	label      string
}{
	{backtest.Momentum12_1, false, "momentum 12-1, equal weight"},
	{backtest.Momentum6_1, false, "momentum 6-1, equal weight"},
	{backtest.MomentumVolAdj, true, "momentum 12-1 / vol, inverse-vol weight"},
	{backtest.LowVol, true, "low volatility, inverse-vol weight"},
}

func guideRank() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.daily.yaml"), "configuration file (costs come from its paper block)")
		data       = flag.String("data", "data/daily", "replay directory from `quantos ingest`")
		members    = flag.String("membership", "", "point-in-time membership CSV")
		bench      = flag.String("benchmark", "SPY", "benchmark ticker")
		topFlag    = flag.Float64("top", 0.10, "fraction of the universe held")
		volTarget  = flag.Float64("vol-target", 0, "scale the sleeve's exposure by min(1, target/trailing vol); 0 disables")
		startS     = flag.String("start", "", "first rebalance (default: when formation data allows)")
		univ       = flag.String("universe", "stocks", "stocks (membership-gated single names) | etfs (sector and asset-class ETFs: no survivorship bias, the control)")
		outJSON    = flag.String("out", "reports/rank.json", "JSON output")
		outMD      = flag.String("md", "reports/rank.md", "Markdown output")
	)
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	rp, series := loadReplay(*data)
	var start time.Time
	if *startS != "" {
		if start, err = time.Parse("2006-01-02", *startS); err != nil {
			fatal("bad -start: %v", err)
		}
	}
	var membership *universe.Membership
	if *members != "" {
		if membership, err = universe.LoadMembership(*members); err != nil {
			fatal("%v", err)
		}
	}
	contextSyms := map[domain.Ticker]bool{}
	for _, t := range universe.MacroTickers() {
		contextSyms[t] = true
	}
	for _, t := range universe.SectorETFs() {
		contextSyms[t] = true
	}
	var candidates []domain.Ticker
	member := func(domain.Ticker, time.Time) bool { return true }
	top := *topFlag
	switch *univ {
	case "etfs":
		// Every sector and asset-class ETF with data except index levels.
		// This set has existed, unchanged, for the whole sample: whatever
		// the sleeve does here it does without a survivorship tailwind.
		for _, t := range rp.Tickers() {
			if contextSyms[t] && t != "VIX" && t != "DXY" && t != domain.NormalizeTicker(*bench) {
				candidates = append(candidates, t)
			}
		}
		if top < 0.2 {
			top = 0.25 // a decile of twenty ETFs is two names
		}
	default:
		for _, t := range rp.Tickers() {
			if contextSyms[t] {
				continue
			}
			if membership != nil && len(membership.Intervals(t)) == 0 {
				continue
			}
			candidates = append(candidates, t)
		}
		if membership != nil {
			member = membership.MemberAt
		}
	}

	var results []rankVariantOut
	fmt.Printf("ranking sleeve: %s universe, %d candidates, top %.0f%%, benchmark %s, %d variants\n\n", *univ, len(candidates), top*100, *bench, len(rankVariants))
	fmt.Printf("%-42s %7s %8s %7s %7s %6s %7s %8s %6s\n", "variant (x1 costs / x2 costs)", "CAGR", "Sharpe", "defl.", "maxDD", "hit", "t(exc)", "t(L-S)", "turn")
	for _, v := range rankVariants {
		var pair [2]backtest.RankResult
		for i, mult := range []float64{1, 2} {
			r, err := backtest.RunRank(backtest.RankConfig{
				Signal: v.signal, Series: series, Candidates: candidates, Benchmark: domain.NormalizeTicker(*bench),
				Member: member, Start: start, TopFraction: top, InverseVol: v.inverseVol, VolTarget: *volTarget,
				StartingCash: cfg.Paper.StartingCash, Costs: cfg.Paper.Costs, CostMultiple: mult, Trials: len(rankVariants),
			})
			if err != nil {
				fatal("%s: %v", v.label, err)
			}
			pair[i] = r
		}
		results = append(results, rankVariantOut{v.label, pair[0], pair[1]})
		for i, r := range pair {
			label := v.label
			if i == 1 {
				label = "  … at 2x costs"
			}
			fmt.Printf("%-42s %+6.1f%% %8.2f %7.2f %6.1f%% %5.0f%% %7.2f %8.2f %5.0f%%\n", label, r.Metrics.CAGR*100, r.Metrics.Sharpe,
				r.Metrics.DeflatedSharpe, r.Metrics.MaxDrawdown*100, r.HitRate*100, r.ExcessTStat, r.SpreadTStat, r.AvgTurnover*100)
		}
	}
	b := results[0].X1.Benchmark
	fmt.Printf("%-42s %+6.1f%% %8.2f %7s %6.1f%%\n", "buy-and-hold "+*bench+" (same span)", b.CAGR*100, b.Sharpe, "—", b.MaxDrawdown*100)
	fmt.Printf("\nspan %s .. %s, %d rebalances\n", results[0].X1.Start.Format("2006-01-02"), results[0].X1.End.Format("2006-01-02"), len(results[0].X1.Months))
	fmt.Println(domain.Disclaimer)

	md := renderRank(results, *bench, *univ, len(candidates), top)
	out := struct {
		Variants   []rankVariantOut `json:"variants"`
		Candidates int              `json:"candidates"`
		Disclaimer string           `json:"disclaimer"`
	}{results, len(candidates), domain.Disclaimer}
	writeGuideOutputs(*outJSON, *outMD, out, md)
}

// rankVariantOut is one variant at both cost levels.
type rankVariantOut struct {
	Label string              `json:"label"`
	X1    backtest.RankResult `json:"x1"`
	X2    backtest.RankResult `json:"x2"`
}

func renderRank(results []rankVariantOut, bench, univ string, candidates int, top float64) string {
	var md strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&md, f, a...) }
	first := results[0].X1
	w("# Ranking sleeve — %s universe\n\n%d candidates, top %.0f%% held, rebalanced monthly, %s .. %s (%d rebalances). Nothing fitted; every month is out of sample.\n\n> %s\n\n",
		univ, candidates, top*100, first.Start.Format("2006-01-02"), first.End.Format("2006-01-02"), len(first.Months), domain.Disclaimer)
	if univ == "stocks" {
		w("**Read the long-short column before the CAGR.** The single-name universe is a list of today's large caps — a winners-only sample. A long-only sleeve that beats the index while the long-short spread is near zero is being carried by the universe, not by the signal. The ETF universe (`-universe etfs`) is the control: it has existed unchanged for the whole sample.\n\n")
	}
	w("## Variants, net of costs\n\n| variant | costs | CAGR | Sharpe | deflated (%d trials) | max DD | worst year | months beating %s | excess/month | t(excess) | long-short/month | t(L-S) | turnover/month | total costs |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n", len(results), bench)
	for _, v := range results {
		for i, r := range []backtest.RankResult{v.X1, v.X2} {
			w("| %s | x%d | %+.1f%% | %.2f | %.2f | %.1f%% | %+.1f%% | %.0f%% | %+.2f%% | %.2f | %+.2f%% | %.2f | %.0f%% | $%s |\n",
				v.Label, i+1, r.Metrics.CAGR*100, r.Metrics.Sharpe, r.Metrics.DeflatedSharpe, r.Metrics.MaxDrawdown*100, r.WorstYear*100,
				r.HitRate*100, r.ExcessMonthly*100, r.ExcessTStat, r.SpreadMonthly*100, r.SpreadTStat, r.AvgTurnover*100, guide.Money(r.TotalCosts))
		}
	}
	b := first.Benchmark
	w("| buy-and-hold %s | — | %+.1f%% | %.2f | — | %.1f%% | | | | | | | | |\n\n", bench, b.CAGR*100, b.Sharpe, b.MaxDrawdown*100)
	w("A t-statistic above about 2 on the excess return is the conventional bar for \"not noise\"; the long-short t-statistic says whether the *signal* ranks names at all, which it can do while the long-only sleeve still trails the index.\n\n")
	for _, v := range results {
		r := v.X1
		w("## %s\n\n| year | sleeve | %s |\n|---|---|---|\n", v.Label, bench)
		for _, y := range r.ByYear {
			w("| %d | %+.1f%% | %+.1f%% |\n", y.Year, y.Sleeve*100, y.Benchmark*100)
		}
		w("\nCurrent slice (%s):", r.Months[len(r.Months)-1].At.Format("2006-01-02"))
		for _, h := range r.Current {
			w(" %s %.0f%%,", h.Ticker, h.Weight*100)
		}
		w("\n\n")
	}
	w("## Notes\n\n")
	for _, n := range first.Notes {
		w("- %s\n", n)
	}
	w("- rebalance trades are assumed at the month-end close; on a monthly cadence the next-open difference is immaterial next to the cost assumption\n")
	w("\n_Generated %s._\n", time.Now().UTC().Format(time.RFC3339))
	return md.String()
}

// guideBrief composes the daily page and maintains the ledger of calls.
func guideBrief() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.daily.yaml"), "configuration file")
		data       = flag.String("data", "data/daily", "replay directory from `quantos ingest`")
		holdings   = flag.String("holdings", "examples/holdings/model-100k.csv", "holdings CSV")
		capsName   = flag.String("caps", "personal", capsUsage)
		members    = flag.String("membership", "", "point-in-time membership CSV, for the sleeve")
		bench      = flag.String("benchmark", "SPY", "benchmark ticker")
		asOfS      = flag.String("as-of", "", "date of the brief (default: last bar)")
		ledgerPath = flag.String("ledger", "reports/brief/ledger.jsonl", "the ledger of calls")
		outDir     = flag.String("dir", "reports/brief", "where dated pages go")
		backfill   = flag.Int("backfill", 0, "also run the brief at this many month-ends before as-of, so the ledger starts with a scored history")
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
	rp, series := loadReplay(*data)
	benchT := domain.NormalizeTicker(*bench)
	var asOf time.Time
	if *asOfS != "" {
		if asOf, err = time.Parse("2006-01-02", *asOfS); err != nil {
			fatal("bad -as-of: %v", err)
		}
	} else {
		b := series[benchT]
		asOf = b[len(b)-1].Start
	}
	var membership *universe.Membership
	if *members != "" {
		if membership, err = universe.LoadMembership(*members); err != nil {
			fatal("%v", err)
		}
	}
	u := universe.Builtin()
	caps := guideCaps(*capsName, cfg)
	ledger, err := guide.LoadLedger(*ledgerPath)
	if err != nil {
		fatal("%v", err)
	}

	// Candidates for the sleeve.
	contextSyms := map[domain.Ticker]bool{}
	for _, t := range universe.MacroTickers() {
		contextSyms[t] = true
	}
	for _, t := range universe.SectorETFs() {
		contextSyms[t] = true
	}
	var candidates []domain.Ticker
	for _, t := range rp.Tickers() {
		if contextSyms[t] || (membership != nil && len(membership.Intervals(t)) == 0) {
			continue
		}
		candidates = append(candidates, t)
	}
	member := func(domain.Ticker, time.Time) bool { return true }
	if membership != nil {
		member = membership.MemberAt
	}

	// Dates to run: backfilled month-ends, then as-of.
	var dates []time.Time
	if *backfill > 0 {
		b := series[benchT]
		for i := len(b) - 2; i >= 0 && len(dates) < *backfill; i-- {
			if b[i].Start.Before(asOf) && (b[i+1].Start.Month() != b[i].Start.Month()) {
				dates = append(dates, b[i].Start)
			}
		}
		sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	}
	dates = append(dates, asOf)

	baseline := []string{
		"volatility forecaster, 455k out-of-sample forecasts 2010–2026: MAE 7.9 vol points against 8.3 for last month's vol; QLIKE 0.353 against 0.461 (reports/vol.md)",
		"ranking sleeve on the survivorship-free ETF universe: every variant trails SPY; the signal does not rank (reports/rank-etfs.md)",
		"direction: three model variants over 27 out-of-sample windows, none beat SPY; predictions below base rate (docs/research/real-data-walk-forward.md)",
	}

	var last guide.Brief
	for _, d := range dates {
		// A forecaster fitted only on bars up to d: the brief at a past date
		// must not know the future.
		vf := guide.NewVolForecaster()
		var all []domain.Candle
		for t, bars := range series {
			if t == "VIX" || t == "DXY" {
				continue
			}
			for _, c := range bars {
				if !c.Start.After(d) {
					all = append(all, c)
				}
			}
		}
		sort.SliceStable(all, func(i, j int) bool {
			if !all[i].Start.Equal(all[j].Start) {
				return all[i].Start.Before(all[j].Start)
			}
			return all[i].Ticker < all[j].Ticker
		})
		for _, c := range all {
			vf.Observe(c)
		}
		// The sleeve's slice at d.
		var sleeve *guide.SleeveView
		if r, err := backtest.RunRank(backtest.RankConfig{
			Signal: backtest.Momentum12_1, Series: series, Candidates: candidates, Benchmark: benchT, Member: member,
			End: d, StartingCash: cfg.Paper.StartingCash, Costs: cfg.Paper.Costs,
		}); err == nil && len(r.Current) > 0 {
			names := make([]string, 0, len(r.Current))
			for _, hld := range r.Current {
				names = append(names, string(hld.Ticker))
			}
			sleeve = &guide.SleeveView{Signal: "momentum 12-1, top decile", Holdings: names,
				Verdict: "Shown for information. On the ETF universe, where survivorship cannot help it, this signal trails the index; on single names its apparent edge came from the universe being today's winners. It is recorded as a call so its live record accumulates either way."}
		}

		brief, err := guide.Compose(guide.BriefInput{
			Holdings: h, Series: series, Benchmark: benchT, AsOf: d, Caps: caps,
			Lookup: func(t domain.Ticker) (domain.Stock, bool) { return u.Get(t) },
			Vol:    vf, Sleeve: sleeve, Ledger: ledger, Baseline: baseline, Now: time.Now().UTC(),
		})
		if err != nil {
			fatal("brief %s: %v", d.Format("2006-01-02"), err)
		}
		last = brief
		if *backfill > 0 && !d.Equal(asOf) {
			fmt.Printf("backfill %s: equity $%s, %d calls recorded, %d scored\n", d.Format("2006-01-02"), guide.Money(brief.Book.Equity), brief.NewCalls, brief.Scored)
		}
	}
	// Score once more with the full data so backfilled calls mature.
	if *backfill > 0 {
		ledger.Score(series, benchT, asOf)
		last.Record = ledger.Record()
	}
	if err := ledger.Save(); err != nil {
		fatal("ledger: %v", err)
	}

	md := guide.RenderBrief(last)
	fmt.Print(md)
	page := filepath.Join(*outDir, asOf.Format("2006-01-02")+".md")
	writeGuideOutputs(filepath.Join(*outDir, asOf.Format("2006-01-02")+".json"), page, last, md)
	if err := os.WriteFile(filepath.Join(*outDir, "latest.md"), []byte(md), 0o644); err == nil {
		fmt.Printf("wrote %s\n", filepath.Join(*outDir, "latest.md"))
	}
}

// guideImportRobinhood turns a Robinhood account-activity report into the
// holdings file the other guide commands read.
func guideImportRobinhood() {
	var (
		in   = flag.String("in", "", "Robinhood account-activity CSV (Account → Reports and statements → Account activity)")
		out  = flag.String("out", "private/holdings.csv", "holdings CSV to write (private/ is git-ignored)")
		cash = flag.Float64("cash", 0, "uninvested cash, from the app; it is not derived from the report")
		data = flag.String("data", "data/daily", "replay directory, checked for history of every holding")
	)
	flag.Parse()
	if *in == "" {
		fatal("-in is required: the CSV Robinhood emails when an account-activity report is ready")
	}
	f, err := os.Open(*in)
	if err != nil {
		fatal("%v", err)
	}
	defer f.Close()
	imp, err := guide.ReadRobinhoodActivity(f, *cash)
	if err != nil {
		fatal("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		fatal("%v", err)
	}
	w, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fatal("%v", err)
	}
	if err := guide.WriteHoldings(w, imp.Holdings); err != nil {
		fatal("%v", err)
	}
	_ = w.Close()

	fmt.Printf("replayed %d buys and sells from %s to %s → %s\n",
		imp.Applied, imp.From.Format("2006-01-02"), imp.To.Format("2006-01-02"), *out)
	fmt.Printf("%d open positions, %d closed within the report, cash $%.2f\n\n",
		len(imp.Holdings.Lines), imp.Closed, imp.Holdings.Cash)
	var missing []string
	for _, l := range imp.Holdings.Lines {
		note := ""
		if _, err := os.Stat(filepath.Join(*data, string(l.Ticker)+".csv")); err != nil {
			note = "  ← no history in " + *data
			missing = append(missing, string(l.Ticker))
		}
		fmt.Printf("  %-6s %12g  avg $%9.2f%s\n", l.Ticker, l.Quantity, l.CostBasis, note)
	}
	if len(imp.Review) > 0 {
		fmt.Printf("\nNOT APPLIED — rows that change a position but whose effect is not guessed at.\n" +
			"Check each against the app and edit the holdings file by hand:\n")
		for _, r := range imp.Review {
			fmt.Printf("  %s  %-6s %-6s %10g  %s\n", r.Date.Format("2006-01-02"), r.Ticker, r.Code, r.Quantity, r.Description)
		}
	}
	if len(imp.Negative) > 0 {
		fmt.Printf("\nSOLD MORE THAN BOUGHT in this report — the report starts too late or a transfer is missing,\n"+
			"so these are left out: %v. Generate the report from the account's opening date.\n", imp.Negative)
	}
	if len(missing) > 0 {
		// Ingest into a scratch directory and copy the CSVs across: ingest
		// rewrites MANIFEST.json for the tickers it fetched, and pointing it
		// at the shared directory would drop every other series from it.
		fmt.Printf("\nFetch history for the holdings above before running book or brief:\n"+
			"  go run ./cmd/quantos ingest -tickers %s -no-macro -out /tmp/quantos-extra && cp /tmp/quantos-extra/*.csv %s/\n",
			strings.Join(missing, ","), *data)
	}
	fmt.Printf("\nCompare every line with the Robinhood app before relying on it. Then:\n"+
		"  go run ./cmd/quantos guide book -holdings %s\n", *out)
}

const capsUsage = "personal (a book of index funds and a few stocks: one company 25%, sector 40%) | strategy (the risk block of -config, sized for a twenty-name signal book)"

// guideCaps picks the thresholds a book is held to.
func guideCaps(name string, cfg config.Config) guide.Caps {
	switch name {
	case "personal":
		return guide.PersonalCaps()
	case "strategy":
		return guide.Caps{
			MaxPosition: cfg.Risk.MaxPositionWeight, SoftPosition: cfg.Risk.SoftPositionWeight,
			MaxSector: cfg.Risk.MaxSectorWeight, SoftSector: cfg.Risk.SoftSectorWeight,
			MaxCorrelated: cfg.Risk.MaxCorrelatedExposure, SoftCorrelated: cfg.Risk.SoftCorrelatedExposure,
			MaxDrawdown: cfg.Risk.MaxDrawdown, DrawdownWarn: cfg.Risk.DrawdownWarn, CorrelationThreshold: 0.60,
		}
	}
	fatal("-caps %q is not personal or strategy", name)
	return guide.Caps{}
}

func orDash(ts []domain.Ticker) string {
	if len(ts) == 0 {
		return "-"
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return strings.Join(out, ",")
}

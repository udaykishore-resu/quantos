// Command quantos is the single-process build of the platform and the
// operator's toolbox.
//
// Subcommands:
//
//	run              run every role in one process (the `make dev` target)
//	demo             run the ten reproducible demo scenarios
//	backtest         run a deterministic historical simulation
//	export-features  export feature snapshots for research in Python
//	validate         validate configuration and strategies, then exit
//	topics           print the Kafka topic specification
//
// The all-in-one build exists so the platform can be evaluated, demonstrated
// and developed without Kafka, PostgreSQL, ClickHouse or a model server. It
// runs the same engines as the split deployment; only the bus and store drivers
// differ (see docs/architecture/overview.md).
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/backtest"
	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/cli"
	"github.com/udaykishoreresu/quantos/internal/config"
	"github.com/udaykishoreresu/quantos/internal/demo"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/rules"
	"github.com/udaykishoreresu/quantos/internal/universe"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	os.Args = append(os.Args[:1], os.Args[2:]...)

	switch cmd {
	case "run":
		cli.Main(cli.Spec{
			Service:    "quantos",
			Roles:      app.AllRoles,
			WithHTTP:   true,
			TradePaper: true,
			Explain:    true,
		})
	case "demo":
		runDemo()
	case "backtest":
		runBacktest()
	case "export-features":
		exportFeatures()
	case "validate":
		validate()
	case "topics":
		printTopics()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "quantos: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `QuantOS — AI market intelligence, prediction, risk and simulation platform

  This is an educational research and paper-trading platform. Output is
  probabilistic, may be wrong, and is not financial advice. No real-money
  orders are placed.

usage: quantos <command> [flags]

commands:
  run              run every role in one process
  demo             run the ten reproducible demo scenarios
  backtest         run a deterministic historical simulation
  export-features  export feature snapshots as CSV for research
  validate         validate configuration and strategies, then exit
  topics           print the event topic specification

run "quantos <command> -h" for the flags of a command.
`)
}

// --- demo -------------------------------------------------------------------

func runDemo() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.yaml"), "configuration file")
		seed       = flag.Int64("seed", 0, "simulator seed (0 uses the configured value)")
		jsonOut    = flag.String("json", "", "write the machine-readable report to this path")
		only       = flag.Int("only", 0, "run a single scenario by number")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	if *seed != 0 {
		cfg.Market.Seed = *seed
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := demo.NewRunner(ctx, demo.Config{Base: cfg})
	if err != nil {
		fatal("demo: %v", err)
	}
	defer r.Close()

	rep := r.RunAll(ctx)
	_ = only // reserved: single-scenario selection requires ordered state, see docs/runbooks/demo.md

	if *jsonOut != "" {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			if err := os.WriteFile(*jsonOut, data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "could not write report: %v\n", err)
			}
		}
	}
	if rep.Failed > 0 {
		os.Exit(1)
	}
}

// --- backtest ---------------------------------------------------------------

func runBacktest() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.yaml"), "configuration file")
		strategy   = flag.String("strategy", "momentum", "strategy id")
		days       = flag.Int("days", 30, "number of days of history to simulate")
		seed       = flag.Int64("seed", 20260827, "deterministic seed")
		tickers    = flag.String("tickers", "", "comma-separated tickers (default: the configured universe)")
		out        = flag.String("out", "", "write the run as JSON to this path")
		trials     = flag.Int("trials", 1, "number of configurations tried, for the deflated Sharpe ratio")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	cfg.Market.Seed = *seed
	cfg.Store.Driver = "memory"
	cfg.Bus.Driver = "memory"
	cfg.Log.Level = "warn"

	ctx := context.Background()
	end := time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -*days)

	a, err := app.New(ctx, cfg, app.Options{Now: start})
	if err != nil {
		fatal("start-up: %v", err)
	}
	defer a.Close()

	var want []domain.Ticker
	if *tickers != "" {
		for _, t := range strings.Split(*tickers, ",") {
			if t = strings.TrimSpace(t); t != "" {
				want = append(want, domain.NormalizeTicker(t))
			}
		}
		// Market context must be present or there is no regime to condition on.
		want = append(want, universe.MacroTickers()...)
		want = append(want, universe.SectorETFs()...)
	}
	if len(want) == 0 {
		for _, s := range a.Universe.All() {
			want = append(want, s.Ticker)
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

	bars, err := a.Provider.GetHistoricalBars(ctx, marketdata.BarRequest{
		Tickers: want, Interval: domain.Interval1m, Start: start, End: end, Limit: 5000, Adjusted: true,
	})
	if err != nil {
		fatal("historical data: %v", err)
	}
	fmt.Printf("loaded %d bars across %d instruments (%s .. %s)\n",
		len(bars), len(want), start.Format("2006-01-02"), end.Format("2006-01-02"))

	run, err := a.Backtest.Run(ctx, backtest.Request{
		Name: "cli", StrategyID: *strategy, Start: start, End: end,
		Interval: domain.Interval1m, Seed: *seed, StartingCash: cfg.Paper.StartingCash,
		Costs: cfg.Paper.Costs, Trials: *trials, ConfigHash: cfg.Hash,
		CodeVersion: cfg.Version, MacroTickers: universe.MacroTickers(),
	}, bars)
	if err != nil {
		fatal("backtest: %v", err)
	}
	printBacktest(run)

	if *out != "" {
		data, _ := json.MarshalIndent(run, "", "  ")
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "could not write run: %v\n", err)
		} else {
			fmt.Printf("\nwrote %s\n", *out)
		}
	}
}

func printBacktest(run domain.BacktestRun) {
	m := run.Metrics
	fmt.Printf(`
run:            %s  (%s)
strategy:       %s
window:         %s .. %s
seed:           %d          config hash: %s
events:         %d
result hash:    %s

total return:   %+.2f%%      CAGR: %+.2f%%
Sharpe:         %.2f         Sortino: %.2f        Calmar: %.2f
volatility:     %.2f%%       max drawdown: %.2f%% over %.1f days
trades:         %d           win rate: %.1f%%      profit factor: %.2f
expectancy:     %.2f         turnover: %.2fx       avg hold: %.1fh
total costs:    %.2f         avg exposure: %.2f
`,
		run.ID, run.Status, run.StrategyID,
		run.Start.Format("2006-01-02 15:04"), run.End.Format("2006-01-02 15:04"),
		run.Seed, run.ConfigHash, run.EventsProcessed, run.ResultHash,
		m.TotalReturn*100, m.CAGR*100, m.Sharpe, m.Sortino, m.Calmar,
		m.Volatility*100, m.MaxDrawdown*100, m.MaxDrawdownDays,
		m.Trades, m.WinRate*100, m.ProfitFactor, m.Expectancy, m.Turnover,
		m.AvgHoldHours, m.TotalCosts, m.AvgExposure)

	if m.TrialsConsidered > 1 {
		fmt.Printf("deflated Sharpe: %.2f over %d trials\n", m.DeflatedSharpe, m.TrialsConsidered)
	}
	if len(run.RegimeMetrics) > 0 {
		fmt.Println("\nby regime:")
		regimes := make([]string, 0, len(run.RegimeMetrics))
		for rg := range run.RegimeMetrics {
			regimes = append(regimes, string(rg))
		}
		sort.Strings(regimes)
		for _, rg := range regimes {
			rm := run.RegimeMetrics[domain.Regime(rg)]
			fmt.Printf("  %-16s trades %3d  win rate %5.1f%%  profit factor %5.2f  expectancy %8.2f\n",
				rg, rm.Trades, rm.WinRate*100, rm.ProfitFactor, rm.Expectancy)
		}
	}
	if run.Error != "" {
		fmt.Printf("\nerror: %s\n", run.Error)
	}
	fmt.Printf("\n%s\n", domain.Disclaimer)
	fmt.Println("Simulated data produces simulated results. No number above is evidence about real markets.")
}

// --- export-features --------------------------------------------------------

// exportFeatures writes feature snapshots as CSV.
//
// This is the training/serving-skew guard from ADR-001 made concrete: Python
// research consumes features produced by the *Go* feature engine, so a model is
// never fitted on a subtly different definition from the one that will serve it.
func exportFeatures() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.yaml"), "configuration file")
		days       = flag.Int("days", 30, "days of history")
		out        = flag.String("out", "ml/data/features.csv", "output CSV path")
		seed       = flag.Int64("seed", 20260827, "deterministic seed")
		horizonBps = flag.Float64("flat-band-bps", 15, "half-width of the FLAT label band, in basis points")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	cfg.Market.Seed = *seed
	cfg.Store.Driver = "memory"
	cfg.Bus.Driver = "memory"
	cfg.Log.Level = "error"

	ctx := context.Background()
	end := time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -*days)

	a, err := app.New(ctx, cfg, app.Options{Now: start})
	if err != nil {
		fatal("start-up: %v", err)
	}
	defer a.Close()

	var tickers []domain.Ticker
	for _, s := range a.Universe.All() {
		tickers = append(tickers, s.Ticker)
	}
	bars, err := a.Provider.GetHistoricalBars(ctx, marketdata.BarRequest{
		Tickers: tickers, Interval: domain.Interval1m, Start: start, End: end, Limit: 5000, Adjusted: true,
	})
	if err != nil {
		fatal("historical data: %v", err)
	}
	sort.SliceStable(bars, func(i, j int) bool {
		if !bars[i].End.Equal(bars[j].End) {
			return bars[i].End.Before(bars[j].End)
		}
		return bars[i].Ticker < bars[j].Ticker
	})

	if err := os.MkdirAll(dirOf(*out), 0o750); err != nil {
		fatal("output directory: %v", err)
	}
	f, err := os.Create(*out)
	if err != nil {
		fatal("output file: %v", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	names := domain.ModelFeatureSet
	header := append([]string{"ts", "ticker", "close", "regime"}, names...)
	// Forward returns for label construction, at each default horizon.
	for _, h := range domain.DefaultHorizons {
		header = append(header, "fwd_ret_bps_"+string(h), "label_"+string(h))
	}
	if err := w.Write(header); err != nil {
		fatal("write header: %v", err)
	}

	type row struct {
		ts     time.Time
		ticker domain.Ticker
		close  float64
		regime string
		vals   []float64
	}
	var rows []row
	prices := map[domain.Ticker][]struct {
		at time.Time
		px float64
	}{}

	for _, c := range bars {
		res := a.Pipeline.OnBar(ctx, c)
		prices[c.Ticker] = append(prices[c.Ticker], struct {
			at time.Time
			px float64
		}{c.End, c.Close})
		if res.Macro {
			continue
		}
		vec := make([]float64, 0, len(names))
		complete := true
		for _, n := range names {
			v, ok := res.Snapshot.Get(n)
			if !ok {
				complete = false
				break
			}
			vec = append(vec, v)
		}
		if !complete {
			continue // cold features are never exported: a model must not learn from them
		}
		rows = append(rows, row{c.End, c.Ticker, c.Close, string(res.Regime.Regime), vec})
	}

	// Labels are computed *after* the pass, from prices that arrive strictly
	// later than the feature timestamp. That ordering is the whole point.
	written := 0
	for _, r := range rows {
		rec := []string{
			r.ts.UTC().Format(time.RFC3339), string(r.ticker),
			strconv.FormatFloat(r.close, 'f', 4, 64), r.regime,
		}
		for _, v := range r.vals {
			rec = append(rec, strconv.FormatFloat(v, 'f', 8, 64))
		}
		complete := true
		for _, h := range domain.DefaultHorizons {
			target := r.ts.Add(h.Duration())
			px, ok := priceAt(prices[r.ticker], target)
			if !ok {
				complete = false
				break
			}
			retBps := (px/r.close - 1) * 10000
			label := "FLAT"
			switch {
			case retBps > *horizonBps:
				label = "UP"
			case retBps < -*horizonBps:
				label = "DOWN"
			}
			rec = append(rec, strconv.FormatFloat(retBps, 'f', 4, 64), label)
		}
		if !complete {
			continue // no forward price yet: exporting a guessed label would be leakage
		}
		if err := w.Write(rec); err != nil {
			fatal("write row: %v", err)
		}
		written++
	}
	w.Flush()
	fmt.Printf("wrote %d labelled rows to %s (%d features, flat band ±%.0f bps)\n",
		written, *out, len(names), *horizonBps)
	fmt.Println("labels use forward prices strictly after each feature timestamp; cold features are excluded")
}

func priceAt(series []struct {
	at time.Time
	px float64
}, target time.Time) (float64, bool) {
	for _, p := range series {
		if !p.at.Before(target) {
			return p.px, true
		}
	}
	return 0, false
}

// --- validate ---------------------------------------------------------------

func validate() {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.yaml"), "configuration file")
		strategies = flag.String("strategies", "", "strategy directory (defaults to the configured value)")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	dir := cfg.Strategies.Dir
	if *strategies != "" {
		dir = *strategies
	}
	reg, err := rules.LoadDir(dir)
	if err != nil {
		fatal("strategies: %v", err)
	}
	fmt.Printf("configuration OK   (hash %s, mode %s, source %s)\n",
		cfg.Hash, cfg.Mode, orDefault(cfg.SourcePath, "defaults"))
	fmt.Printf("strategies OK      (%d loaded from %s)\n", reg.Len(), dir)
	for _, s := range reg.All() {
		regimes := "any regime"
		if len(s.Regimes) > 0 {
			parts := make([]string, 0, len(s.Regimes))
			for _, r := range s.Regimes {
				parts = append(parts, string(r))
			}
			regimes = strings.Join(parts, ", ")
		}
		fmt.Printf("  %-16s v%-8s %2d rules  %2d invalidations  enabled=%-5t  %s\n",
			s.ID, s.Version, len(s.Rules), len(s.Invalidation), s.Enabled, regimes)
	}
	u := universe.Builtin().Apply(cfg.Universe.Include, cfg.Universe.Exclude, cfg.Universe.MaxSymbols)
	fmt.Printf("universe OK        (%d instruments, %d sectors)\n", u.Len(), len(u.Sectors()))
}

// --- topics -----------------------------------------------------------------

func printTopics() {
	flag.Parse()
	fmt.Printf("%-26s %-11s %-14s %s\n", "TOPIC", "PARTITIONS", "RETENTION", "COMPACT")
	for _, t := range bus.TopicConfigs {
		fmt.Printf("%-26s %-11d %-14s %t\n", t.Name, t.Partitions, t.Retention.String(), t.Compact)
	}
	fmt.Println("\nPartition key is the ticker on every live-path topic, which gives per-symbol ordering.")
	fmt.Println("regime.updated is single-partition on purpose: there is one market regime and total order over it")
	fmt.Println("is worth more than parallelism.")
}

// --- helpers ----------------------------------------------------------------

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "quantos: "+format+"\n", a...)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func dirOf(path string) string {
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return "."
	}
	return path[:i]
}

var _ = obs.SystemClock{}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/ingest"
	"github.com/udaykishoreresu/quantos/internal/universe"
)

// runIngest fetches real daily history into a replay directory.
//
// It is the first command in this binary that talks to the outside world, and
// it talks only to read prices. There is still no order path.
func runIngest() {
	var (
		source   = flag.String("source", "stooq", "data source: stooq (no key) | tiingo (TIINGO_TOKEN) | local (a directory of CSVs)")
		out      = flag.String("out", "data/daily", "replay directory to write")
		in       = flag.String("in", "", "for -source local: directory containing <SYMBOL>.csv files")
		tickers  = flag.String("tickers", "", "comma-separated tickers (default: the builtin universe plus macro and sector ETFs)")
		list     = flag.String("list", "", "a builtin list to fetch instead: sp500, nasdaq100, watchlist")
		members  = flag.String("membership", "", "membership CSV; fetch every ticker that was ever a member")
		startS   = flag.String("start", "2010-01-01", "first date (YYYY-MM-DD)")
		endS     = flag.String("end", "", "last date (default: today)")
		delay    = flag.Duration("delay", 400*time.Millisecond, "pause between requests")
		minBars  = flag.Int("min-bars", 300, "skip series shorter than this")
		noMacro  = flag.Bool("no-macro", false, "do not add the macro and sector-ETF context symbols")
		symbolKV = flag.String("map", "", "symbol overrides, e.g. DXY=dx.f,VIX=^vix")
		token    = flag.String("token", os.Getenv("TIINGO_TOKEN"), "Tiingo API token (or TIINGO_TOKEN)")
	)
	flag.Parse()

	start, err := time.Parse("2006-01-02", *startS)
	if err != nil {
		fatal("bad -start: %v", err)
	}
	end := time.Now().UTC()
	if *endS != "" {
		if end, err = time.Parse("2006-01-02", *endS); err != nil {
			fatal("bad -end: %v", err)
		}
	}

	var src ingest.Source
	switch *source {
	case "stooq":
		src = ingest.NewStooq()
	case "tiingo":
		if *token == "" {
			fatal("tiingo needs -token or TIINGO_TOKEN")
		}
		src = ingest.NewTiingo(*token)
	case "local":
		if *in == "" {
			fatal("-source local needs -in <dir>")
		}
		src = &ingest.LocalDir{Dir: *in}
	default:
		fatal("unknown source %q", *source)
	}

	want := map[domain.Ticker]bool{}
	switch {
	case *tickers != "":
		for _, t := range strings.Split(*tickers, ",") {
			if t = strings.TrimSpace(t); t != "" {
				want[domain.NormalizeTicker(t)] = true
			}
		}
	case *members != "":
		m, err := universe.LoadMembership(*members)
		if err != nil {
			fatal("%v", err)
		}
		for _, t := range m.Tickers() {
			want[t] = true
		}
	case *list != "":
		for _, s := range universe.Builtin().List(*list) {
			want[s.Ticker] = true
		}
		if len(want) == 0 {
			fatal("unknown list %q (try sp500, nasdaq100, watchlist)", *list)
		}
	default:
		for _, s := range universe.Builtin().All() {
			want[s.Ticker] = true
		}
	}
	if !*noMacro {
		for _, t := range universe.MacroTickers() {
			want[t] = true
		}
		for _, t := range universe.SectorETFs() {
			want[t] = true
		}
	}
	all := make([]domain.Ticker, 0, len(want))
	for t := range want {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	symbols := ingest.DefaultSymbols(*source, all)
	if *symbolKV != "" {
		for _, kv := range strings.Split(*symbolKV, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				fatal("bad -map entry %q (want TICKER=symbol)", kv)
			}
			symbols[domain.NormalizeTicker(k)] = strings.TrimSpace(v)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("ingest: %d instruments from %s, %s .. %s, into %s\n",
		len(all), src.Name(), start.Format("2006-01-02"), end.Format("2006-01-02"), *out)
	res, err := ingest.Run(ctx, src, ingest.Request{
		Tickers: all, Start: start, End: end, OutDir: *out, Symbols: symbols,
		Delay: *delay, MinBars: *minBars,
		Progress: func(line string) { fmt.Println("  " + line) },
	})
	if err != nil && len(res.Written) == 0 {
		fatal("ingest: %v", err)
	}
	fmt.Printf("\nwrote %d series (%d bars) to %s; %d skipped\n", len(res.Written), res.Bars, *out, len(res.Skipped))
	if len(res.Skipped) > 0 {
		skipped := make([]string, 0, len(res.Skipped))
		for t := range res.Skipped {
			skipped = append(skipped, string(t))
		}
		sort.Strings(skipped)
		fmt.Printf("skipped: %s\n", strings.Join(skipped, " "))
	}
	fmt.Printf("adjustment: %s\n", res.Manifest.Adjustment)
	fmt.Println("next: quantos report -config config/quantos.daily.yaml -data " + *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ingest finished with errors: %v\n", err)
		os.Exit(1)
	}
}

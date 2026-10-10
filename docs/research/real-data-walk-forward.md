# Real data, out of sample, after costs

Everything else in this repository runs on the platform's own simulator. That
is the right place to test plumbing — a risk veto, a bus outage, a provenance
chain — and the wrong place to measure edge: whatever the pipeline learns
there, it learns about the simulator. The shipped model card says so in its
first paragraph. This document is the path from that admission to a number
that means something.

The three commands:

```bash
make ingest         # real daily bars into data/daily  (Stooq, no API key)
make membership     # point-in-time S&P 500 constituents into data/membership/
make report         # net-of-cost walk-forward → reports/walkforward-momentum.{md,json}
```

and what each one does, what it cannot do, and how to read what comes out.

---

## 1. Ingest: `quantos ingest`

Fetches daily OHLCV for the builtin universe plus the macro and sector-ETF
context symbols, writes one `<TICKER>.csv` per instrument in the layout the
replay provider reads, and a `MANIFEST.json` recording the source, the
adjustment it claims, the coverage of every series and the trailing 30-day
traded notional (which the risk engine's liquidity gate needs).

| source | key | what you get |
|---|---|---|
| `stooq` (default) | none | split/dividend-adjusted daily bars; throttled after a few hundred requests a day, so `-delay` matters |
| `tiingo` | `TIINGO_TOKEN` | adjusted fields from the API; free tier covers a few hundred names a day |
| `local` | none | any directory of CSVs you already have (a Yahoo Finance export works); an `Adj Close` column scales the whole bar |

```bash
go run ./cmd/quantos ingest -source stooq -out data/daily -start 2010-01-01
go run ./cmd/quantos ingest -source stooq -membership data/membership/sp500.csv   # every name ever in the index
go run ./cmd/quantos ingest -source local -in ~/Downloads/yahoo -out data/daily
```

Two things the manifest records that you should read before trusting a
result: `adjustment` (never mix sources — the replay provider marks every bar
adjusted and cannot tell them apart) and `skipped` (a delisted name a free
source no longer carries is *absent*, not present-and-losing; see §2).

## 2. Membership: `scripts/build_membership.py`

The builtin universe is a list of today's large caps, and every name on it is
there *because* it did well. A backtest over that list buys NVDA in 2012 on
the strength of its 2024 index weight. The membership table —
`ticker,start,end` — is what stops that: a bar for a name that was not a
constituent on that date still warms the feature engine (the data existed),
but can never become a candidate, a prediction or a signal. The gate is in the
pipeline, not the backtester, so it also holds for a live replay.

The table is built from the public Wikipedia constituents page and its
"selected changes" table. The current-constituent half is complete; the
changes half is explicitly *selected* and thins out before the 2000s. A
removed name whose addition date is unknown gets a placeholder start and is
flagged in the `note` column.

What this fixes: the pre-inclusion run-up bias, and trading a name after it
was removed. What it cannot fix: a company that failed in 2009 usually has no
price history at a free source, so it is missing from the data rather than
counted as a loss. The correction is partial and the report's notes say so
every time.

## 3. Report: `quantos report`

Runs the production pipeline — features, regime, rules, prediction, the
eleven-check risk veto, signal policy, paper broker — over disjoint,
embargoed, out-of-sample test windows, once per cost multiplier, and scores
every prediction against the realised outcome as the bars arrive.

```bash
go run ./cmd/quantos report -config config/quantos.daily.yaml \
    -data data/daily -membership data/membership/sp500.csv \
    -strategy momentum -test 180d -costs 1,2 -trials 5
```

Three columns of the output matter more than the rest.

**Sharpe at 2× costs.** The cost model is deliberately pessimistic and it is
still the first thing that is wrong. A result that survives the configured
costs and dies when they double was never there.

**Buy-and-hold over the same windows.** The strategy's out-of-sample curve is
compared with holding `SPY` across exactly the same test windows, chained the
same way. A strategy that makes money but makes less than sitting still, with
more turnover, is not paying for its own complexity.

**Lift over base rate, with its standard error.** Predictions are scored on
every candidate bar, not only the ones that became signals, so the sample is
large. The base rate is the accuracy of always guessing the most common class;
a lift inside one binomial standard error is noise. Brier skill against the
climatological forecast is the calibration-aware version of the same
question.

The verdict at the top of the report is mechanical and its thresholds are in
`internal/backtest/report.go` (`judge`), so they can be argued with rather
than trusted.

### What the schedule is, and is not

`-train`, `-valid` and `-test` lay out a walk-forward schedule with an
embargo of at least twice the longest prediction horizon before every test
window. Nothing is *refitted* per fold in this binary: the shipped strategies
and, unless `-no-model`, the shipped artifact are a fixed configuration, and
the folds are disjoint out-of-sample periods for it. Per-fold refitting lives
in `ml/evaluation/walk_forward.py`, which consumes features this binary
exports with `export-features`.

Before each test window the pipeline is warmed on `features.warmup_bars` of
earlier bars without trading, so the first year of each window is not
silently untradable. The equity curve and every statistic cover the test
window only.

### The daily profile

`config/quantos.daily.yaml` overlays the base configuration for daily bars.
Every difference is a unit conversion, not a change of stance: horizons of
`1d` and `5d` instead of minutes, a ±50 bps flat band instead of ±15, a
two-week signal time-to-live, staleness measured in days, and a regime trend
threshold scaled from per-minute to per-day. Read it before changing it.

### What the backtester now does differently

Three things in `internal/backtest` changed to make a daily-bar report
honest, and they apply to the simulated backtest too:

- **Fills at the next open** (`-fill-at-close` restores the old behaviour).
  A decision made on the close of bar *i* used to fill at that close. On
  one-minute bars that is a fair proxy; on daily bars it books the overnight
  gap in the strategy's favour.
- **A quote for the spread check.** Replayed history has no quotes, and a
  check the risk engine cannot evaluate is `WATCH_ONLY`, not a pass — which
  meant no backtest with the shipped risk configuration could ever trade. The
  backtester now supplies the two-sided quote a bar implies, with a spread
  derived from traded notional and floored at the cost model's own
  assumption. `internal/backtest/spread.go` explains why that is a stated
  proxy rather than a high-low estimate: daily bars cannot resolve a 3 bps
  spread, and the estimators that claim to read tens of basis points of noise
  on every liquid name.
- **Provisional model health.** The live pipeline serves a loaded model on
  its offline evaluation until live outcomes replace it; the backtest
  pipeline now gets the same record instead of "unmeasured", which was
  another way for every candidate to be `WATCH_ONLY`.

## 4. Reading a result

The honest ceiling for a system like this on daily data is modest: a
calibrated 52–56% directional accuracy, a Sharpe near one after costs, and a
drawdown you could live through. If the report shows a Sharpe of three or an
accuracy of seventy percent, the correct reaction is "where is the leak", and
the first places to look are the adjustment field in the manifest, the
membership table's coverage, and the trial count you passed to `-trials`.

Three outcomes are possible and two of them are useful:

1. **No demonstrated edge.** The most likely result for the five shipped
   strategies, which were written as regime-gated illustrations rather than
   fitted to anything. This is a finding, not a failure: it is the number
   that was missing.
2. **Survives costs, does not beat buy-and-hold.** The second most likely.
   The platform is doing what it was built to do — declining most setups —
   and the few it takes are not better than the index.
3. **Edge not rejected.** Every box ticked at 2× costs. This is where paper
   trading on live data for several months begins, with the evaluation
   scheduler scoring every prediction (governance rule G-7). It is not where
   real money begins, and this repository has no path for that by design.

Nothing in this document, the report, or the platform is investment advice.

# Real data, out of sample, after costs

Everything else in this repository runs on the platform's own simulator. That
is the right place to test plumbing — a risk veto, a bus outage, a provenance
chain — and the wrong place to measure edge: whatever the pipeline learns
there, it learns about the simulator. The shipped model card says so in its
first paragraph. This document is the path from that admission to a number
that means something.

The three commands:

```bash
make ingest         # real daily bars into data/daily  (Yahoo chart API, no key)
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
| `yahoo` (default) | none | the chart endpoint the Yahoo Finance site uses (what `yfinance` wraps); OHLC scaled by `adjclose/close`. Unofficial, so expect it to change someday; the parser fails loudly when it does |
| `tiingo` | `TIINGO_TOKEN` | adjusted fields from a documented API; free tier covers a few hundred names a day. The right choice once this is a routine |
| `stooq` | none | CSV, but as of October 2026 behind a JavaScript browser-verification wall that a plain client cannot pass; kept in case the wall goes away |
| `local` | none | any directory of CSVs you already have (a Yahoo Finance export works); an `Adj Close` column scales the whole bar |

```bash
go run ./cmd/quantos ingest -out data/daily -start 2010-01-01
go run ./cmd/quantos ingest -membership data/membership/sp500.csv       # every name ever in the index
go run ./cmd/quantos ingest -source tiingo -token $TIINGO_TOKEN -out data/daily
go run ./cmd/quantos ingest -source local -in ~/Downloads/yahoo -out data/daily
```

Ingest overwrites per instrument, so a run that was rate-limited halfway can
simply be re-run; the names that already landed are refreshed, not lost.

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

## 4. Retraining on real features

The shipped artifact was fitted on the simulator. Once a replay directory
exists, the same trainer fits the same model on real bars:

```bash
make features-daily       # export-features against data/daily, membership-gated, horizons 1d and 5d
make train-daily          # fit direction-3class-daily, then the per-fold refit walk-forward
make report-daily-model   # the Go report served by that artifact
make report-no-model      # and by the rule prior alone, for the comparison
```

`export-features -data <dir>` uses the configured bar interval, horizons and
flat band, exports every bar the membership table admits, and labels against
the first close at or after each horizon. The Python side accepts `5d` as a
horizon. The promotion gate's maximum calibration deviation now ignores bins
with fewer than 100 samples (`MIN_BIN_SUPPORT`, mirrored in Go as
`evaluation.MinCalibrationBinSupport`): a bin of twenty predictions has a
ten-point standard error on its observed frequency, and letting it veto a
model whose ECE is 0.006 was rejecting calibration for being occasionally
confident.

## 5. Results, October 2026

Yahoo daily bars 2010-01-04 .. 2026-10-09, 92 S&P 500 candidates plus 22
context symbols, membership-gated, 27 disjoint 180-day test windows from
2013-05 to 2026-09, `momentum` strategy, fills at the next open, trial count 5.
Reproducible with the targets above; the JSON reports carry every number.

| served by | trades | return | CAGR | Sharpe | max DD | folds positive | 1d accuracy / base |
|---|---|---|---|---|---|---|---|
| shipped artifact (simulator-trained), x1 | 2273 | +19.2% | 1.3% | 0.28 | 12.5% | 56% | 0.345 / 0.360 |
| shipped artifact, x2 costs | 2275 | +10.8% | 0.8% | 0.17 | 12.6% | 48% | |
| rule prior only, x1 | 2303 | +12.2% | 0.9% | 0.19 | 13.0% | 48% | 0.345 / 0.360 |
| rule prior only, x2 costs | 2304 | +4.1% | 0.3% | 0.08 | 14.9% | 44% | |
| real-data artifact, x1 | 2316 | +15.7% | 1.1% | 0.23 | 12.5% | 52% | 0.345 / 0.360 |
| real-data artifact, x2 costs | 2316 | +7.5% | 0.6% | 0.13 | 14.0% | 48% | |
| **buy-and-hold SPY, same windows** | — | **+420.7%** | **13.2%** | **0.82** | 33.7% | | |

The real-data artifact on its own held-out split (2023-09 .. 2026-10, 71,023
rows): accuracy 0.390 against a base rate of 0.374, lift +0.016 at about nine
standard errors, ECE 0.010. Refit per fold over eight expanding windows the
lift is positive in every fold and **decays monotonically**, from +0.069 in
the earliest window to +0.011 in the latest. That is what a weak technical
signal that the market has been arbitraging for a decade looks like.

What the table says:

- **The book does not depend on the model.** Three different prediction
  sources produce the same scored accuracy and Sharpe ratios within noise of
  each other. The blend weight scales with measured lift over a 0.05
  reference, so a model with +0.016 moves the argmax almost never; the
  rule-score floor and the risk engine decide what trades. That is the
  design working as specified, and it means the ML layer is currently
  decorative.
- **The rule prior is anti-predictive at one day.** 0.345 against 0.360 on
  292k predictions is a measured negative, not noise. The regime-gated
  rules were written as illustrations; on real data their directional calls
  are wrong slightly more often than always calling FLAT.
- **Doubling costs halves the return, every time.** The P&L is the same
  size as the friction.
- **Nothing beats holding the index**, with survivorship bias in the
  strategy's favour.

The honest conclusion is the one the README's disclaimer always made:
QuantOS is a measurement instrument, and it has now measured that its
shipped strategies and both models have no tradeable edge on daily US large
caps. The useful next questions are not about thresholds. They are whether
a different *target* carries information — realised volatility for sizing,
or cross-sectional monthly ranking for selection — and the same report will
answer them the same way.

## 6. Reading a result

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

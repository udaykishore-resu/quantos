# The guide: measuring a real book

`quantos guide` is the investor-facing layer. It reads a book of holdings,
marks it against the same real history the research tools use, and reports
what the data supports — exposure, concentration, correlation, drawdown,
forecast volatility, and the arithmetic of a savings goal.

It gives no advice. That is not a disclaimer; it is the design. The platform's
own walk-forward report (`docs/research/real-data-walk-forward.md`) measured
its directional predictions on thirteen years of real data and found nothing
tradeable, three ways. A tool that then told you what to buy would be lying
about what it knows. What it *can* measure, it measures with a window and a
method, and every forecast carries its own out-of-sample error.

```bash
make guide-book      # your holdings against the caps: reports/book.md
make guide-vol       # the volatility forecaster, evaluated: reports/vol.md
make guide-plan      # the goal arithmetic: reports/plan.md
make guide-rank      # the monthly ranking sleeve, stocks and the ETF control: reports/rank*.md
make guide-brief     # today's page, and the ledger of every earlier page's calls: reports/brief/
```

All three read `data/daily` from `quantos ingest`.

---

## `guide book` — what you own, measured

Input is a CSV of what you hold, not what you weight; weights are prices'
business:

```
ticker,quantity,cost_basis,note
CASH,5000,,
SPY,45,,core
AAPL,24,,
```

`examples/holdings/model-100k.csv` is a model book to start from. Output:

- **Positions** marked at the last close, with weight, one-year return,
  volatility, beta to the benchmark and maximum drawdown.
- **Findings** against the caps the risk engine applies to the paper book
  (the `risk:` block of the configuration): single-name weight, single-name
  sector weight, correlated exposure, one-year drawdown, diversification.
  ETFs are reported but not held to the single-name cap; there is no
  look-through.
- **Correlated exposure**: for each name, the weight of *other* names whose
  one-year daily correlation exceeds 0.6. NVDA plus QQQ plus SPY is one
  exposure, whatever the three line items say.
- **At today's weights, historically**: trailing one, three and five years
  and the full history, holding today's weights constant with daily
  rebalancing — return, volatility, Sharpe, drawdown, beta — next to the
  benchmark. This measures the exposure you have now, not the path you took.
  The full-history row for a book of today's winners is hindsight and reads
  as such.
- **Stress episodes**: Q4 2018, the COVID crash, the 2022 bear market, the
  April 2025 tariff shock — what the current weights would have done,
  against the benchmark.

What it does not do: tell you what to change. The findings name the cap and
the number; the decision is the owner's.

## `guide vol` — the one forecast the platform can make

A pooled HAR regression (Corsi 2009) of next-month realised variance on
today's, this week's and this month's, fitted online with no look-ahead and
scored on every forecast as it matures. On the ingested history (115 names,
2010–2026, 455,000 out-of-sample forecasts):

| forecaster | R² (log variance) | QLIKE | MAE (vol points) |
|---|---|---|---|
| HAR | 0.46 | 0.353 | 7.9 |
| last month's volatility | 0.42 | 0.461 | 8.3 |

Forecast deciles are monotone in realised volatility, and the regression
beats persistence on QLIKE in fourteen of seventeen years. Coefficients come
out as the literature says they should: monthly 0.58, weekly 0.13, daily
0.01.

That is a real, durable, useful forecast — and it is about *risk*, not
direction. It is used in two places: the per-name forecasts in the book
report, and `quantos report -vol-target 0.15`, which sizes every paper
position by min(1, target / forecast). The scaling only ever reduces size;
it never leverages a quiet name up.

What it does to the momentum rules on real data, rule prior only, 27
out-of-sample windows: maximum drawdown 13.0% → 7.8%, costs halved, return
+12.2% → +5.1%, Sharpe 0.19 → 0.17. Sizing scales the picture; it does not
change what the picture is. A risk layer makes a bad strategy lose less and
a good one survive, and this is the measurement that says which you have.

## `guide plan` — the arithmetic

A block-bootstrap Monte Carlo (twelve-month blocks, ten thousand paths) of
wealth under contributions, drawn from the benchmark's own monthly returns
with a haircut you choose. The default haircut is four points: the 2010–2026
sample returned 14.6% a year, which is not a planning assumption.

Two capital figures are reported for an income target, because people
confuse them:

- **sustainable**: income ÷ withdrawal rate (4% by default). $200 a day is
  $50,000 a year is $1.25M. The capital keeps growing.
- **from returns**: income ÷ expected return. About $400k–$500k. Spending the
  whole expected return leaves nothing for the bad years, and the sample's
  worst twelve months were −18%.

The year-by-year table gives P10/P50/P90 wealth, a fixed-rate projection for
comparison, and the share of paths that have reached each target. Change the
contribution and the horizon and watch which one moves the answer; the
return assumption is the lever you control least.

## `guide rank` — the sleeve, and the control that exposes it

A cross-sectional monthly sleeve: sort the universe on a formation signal at
each month end, hold the top slice for a month, repeat. Four variants
(momentum 12-1, momentum 6-1, volatility-adjusted momentum, low volatility),
each at the configured costs and at twice them, with the deflated Sharpe
counting all four as the trials they are. Nothing is fitted, so every month
is out of sample.

Two universes, and the second is the point:

| universe | variant | CAGR | Sharpe | max DD | t(excess vs SPY) | t(long−short) |
|---|---|---|---|---|---|---|
| 92 single names (today's large caps) | momentum 12-1 | +25.6% | 1.02 | 34% | **2.76** | 0.80 |
| 19 sector / asset-class ETFs | momentum 12-1 | +11.4% | 0.78 | 26% | −1.12 | 0.81 |
| 19 ETFs | low volatility | +5.1% | 0.79 | 16% | −3.18 | −1.43 |
| SPY, same span (2011–2026) | | +14.0% | 0.85 | 34% | | |

The single-name sleeve looks like an edge: eleven points a year over the
index at a t-statistic of 2.8. It is not. The universe is the list of
today's large caps — names that are on it *because* they won — and a
momentum sort of a winners-only list concentrates in the decade's biggest
winners. The tell is the long-short column: the signal separates the top of
that universe from the bottom at t = 0.8, which is nothing. The ETF
universe has existed unchanged for the whole sample, so survivorship cannot
help it, and there every variant trails the index. That is the control, and
it is what the single-name number is worth.

The sleeve is still shown in the brief, labelled with this verdict, and
every month's slice is recorded as a call — so if it ever does rank, the
ledger will be the first to know.

## `guide brief` — one page, with its own track record

```bash
make guide-brief                 # today
make guide-brief BACKFILL=24     # and the last 24 month-ends, so the ledger starts scored
```

The page: the benchmark's position (returns, distance from the 200-day
average, drawdown, realised and forecast volatility, VIX), the book against
the caps with the change since the last brief, the volatility forecast per
holding translated into a one-sigma monthly dollar move, what the sleeve
would hold, and the track record.

The track record is the part nobody else ships. Every statement that can be
scored is written to `reports/brief/ledger.jsonl` when the brief runs — each
holding's volatility forecast, matured 21 sessions later against realised
volatility; the sleeve's slice, matured a month later against the
benchmark. The next brief scores whatever has matured and prints the running
record next to the persistence baseline. A backfill replays the brief at
past month-ends with a forecaster that has seen nothing after each date, so
day one is not an empty page. Risk findings are not calls: "NVDA is 7% of
the book" is a fact, and facts are not scored.

On the model book, backfilled over 24 months: 207 volatility calls scored,
QLIKE 0.307 against 0.388 for last month's volatility; 23 sleeve months, 61%
beating SPY — on the single-name universe, with the verdict above attached.

## What this is, and is not

It is a measurement instrument pointed at your own money, with the same
honesty rules as the research tools: no certainty language, provenance on
every figure, the disclaimer on every record, and a ledger that scores the
instrument itself. It is not a recommendation engine; the walk-forward
report and the ETF control are the reasons it is not one.

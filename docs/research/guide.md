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

## What this is, and is not

It is a measurement instrument pointed at your own money, with the same
honesty rules as the research tools: no certainty language, provenance on
every figure, the disclaimer on every record. It is not a recommendation
engine, and the walk-forward report is the reason it is not one.

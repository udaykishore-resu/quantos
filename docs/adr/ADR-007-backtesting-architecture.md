# ADR-007 — Deterministic event-driven backtesting sharing the live code path

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

A backtest that does not exercise the same code as production measures a
different system. The classic failure is a vectorised research backtest that
shows Sharpe 2.1 and a live system that shows 0.3, because the research code
peeked at the close, resampled with a centred window, or sized positions using
information that arrived later.

We need: bit-reproducible runs, no look-ahead, no survivorship bias, no leakage,
realistic costs, and the ability to say "this strategy under this regime" with a
straight face.

## Options

1. **Vectorised backtest (pandas over a price matrix).** Fast, easy, and the
   default source of the failure above. Look-ahead is one `shift(-1)` typo away
   and there is no natural place for partial fills, spreads or queue position.
2. **Event-driven backtest, separate implementation from live.** Realistic, but
   two implementations drift and the drift is invisible until money is involved.
3. **Event-driven backtest that replays into the live engines.** Chosen.

## Decision

The backtester is a **deterministic event loop that feeds the production
engines**. `internal/backtest` constructs the same `features.Engine`,
`regime.Engine`, `rules.Engine`, `predict.Engine`, `risk.Engine`,
`signal.Engine` and `paper.Broker` objects used by the services, wires them to
the in-memory bus, and drives them from a historical event stream in
timestamp order.

Determinism is guaranteed by construction:

- **No wall clock.** Every component takes a `quantos.Clock`. In backtests it is
  a `SimClock` advanced only by the event loop. `time.Now()` is banned outside
  `internal/obs`; a CI grep enforces it.
- **No unseeded randomness.** Slippage models, the simulated provider and any
  stochastic component take an explicit `*rand.Rand` seeded from the run config.
  The seed is recorded on the `BacktestRun`.
- **Stable ordering.** Events with identical timestamps are ordered by
  `(ticker, seq, event_id)`. Map iteration never determines output order;
  `internal/backtest` sorts before emitting.
- **Single-threaded core.** Parallelism happens *across* runs (parameter sweeps),
  never inside one.

### Bias prevention

- **Look-ahead.** `LeakageGuard` asserts, on every decision, that
  `max(feature.AsOf) <= decision.At` and that the fill timestamp is strictly
  after the decision timestamp. A violation aborts the run — it does not warn.
- **Bar semantics.** A decision made on bar *t* fills at the open of bar *t+1*
  (configurable to VWAP of *t+1*), never at the close of *t*.
- **Survivorship.** The universe is point-in-time: `universe.AsOf(date)` returns
  constituents as of that date, including names later delisted, with delisting
  handled as a forced liquidation at the last valid price.
- **Data leakage across folds.** Walk-forward folds are separated by an
  `embargo` window (default = max horizon × 2) so that a label in the training
  fold cannot overlap the test fold's feature window.
- **Restatement.** Fundamentals are stored with both `period_end` and
  `filed_at`; only rows with `filed_at <= decision.At` are visible.

### Cost model

Configurable per run: commission (bps and/or per-share with a minimum), spread
cost (half-spread, from historical quotes where available, else an ATR-derived
estimate), slippage (fixed bps, square-root market-impact
`k·σ·sqrt(order/ADV)`, or a supplied model), borrow cost for shorts, and a
participation cap that splits an order that exceeds a fraction of bar volume.

### Metrics

Total return, CAGR, Sharpe, Sortino, Calmar, max drawdown and its duration, win
rate, profit factor, expectancy, turnover, exposure, and every one of these
**partitioned by market regime**, which is the answer to product question 9.

## Trade-offs

- (+) What is backtested is what runs. Divergence is structurally impossible for
  the decision path.
- (+) Bias prevention is enforced, not documented.
- (−) Slower than vectorised: ~1e5–1e6 events/s single-threaded rather than
  instant. Mitigated by parallel sweeps and by ClickHouse-side pre-aggregation
  of bars.
- (−) Research iteration on *new* features is slower than in a notebook. Handled
  by the feature-export path (ADR-001): researchers explore in Python over
  exported snapshots, then promote the feature into Go.
- (−) The event loop is a single point of subtle bugs; heavily property-tested.

## Failure modes

- *Silent non-determinism.* Detected by a CI test that runs the same config
  twice and byte-compares the result, including the equity curve.
- *Guard false positives* on legitimately-timestamped data. Surfaces as an
  aborted run with the offending feature named; better than a quiet wrong number.
- *Overfitting via repeated sweeps.* Not preventable by architecture. Mitigated
  by reporting deflated Sharpe and the number of configurations tried on every
  sweep report, and by mandatory out-of-sample holdout.

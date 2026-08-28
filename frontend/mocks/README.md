# Mock fixtures

Complete response envelopes for every route the dashboard reads. They exist so
the UI can be developed, demonstrated and tested without a running backend.

```bash
npm run dev:mock     # or: QUANTOS_USE_MOCKS=1 npm run dev
```

In mock mode `src/lib/server/mock-transport.ts` supplies a `fetch` that resolves
these files. Nothing else changes: the same `QuantosClient`, the same envelope
parsing, the same error handling. A fixture that does not match the real
envelope shape will fail here exactly as it would against the platform, which is
the point — a mock layer that bypassed the envelope would let envelope bugs
reach production.

## Provenance of each fixture

Everything below was captured from a real `go run ./cmd/quantos run` on
2026-08-28 unless it is listed as hand-built.

### Captured verbatim from the running platform

| File | Route |
| --- | --- |
| `healthz.json`, `readyz.json` | `/healthz`, `/readyz` |
| `market-regime.json` | `/api/v1/market/regime` |
| `market-regime-history.json` | `/api/v1/market/regime/history` (trimmed to 60 records) |
| `market-relationships.json` | `/api/v1/market/relationships` |
| `market-status.json` | `/api/v1/market/status` |
| `stocks.json` | `/api/v1/stocks?limit=25` |
| `stock-<TICKER>.json` | `/api/v1/stocks/{ticker}` for the top six ranked instruments |
| `predictions.json` | `/api/v1/predictions?limit=12` |
| `model-health.json` | `/api/v1/model-health` |
| `evaluations.json` | `/api/v1/evaluations` (outcomes trimmed to the most recent 120) |
| `strategies.json` | `/api/v1/strategies` |
| `paper-portfolio.json` | `/api/v1/paper/portfolio` (equity curve trimmed to 180 points) |
| `audit.json` | `/api/v1/audit` |
| `brief.json` | `/api/v1/brief` |

Trimming only ever drops elements from an array; no field was edited.

### Hand-built to the Go types

These are states the local `run` could not reach. Each was written directly
against the corresponding struct in `internal/domain` and its JSON tags.

| File | Why it is hand-built |
| --- | --- |
| `signals.json`, `provenance-*.json`, `risk-BA.json` | **The local run emits no signals at all.** Every risk assessment resolves to `WATCH_ONLY` because the `spread` check skips — the simulator timestamps quotes ahead of wall clock, the validator rejects them as `future_timestamp`, and with no two-sided quote the spread cannot be evaluated. `domain.NewSignal` refuses to construct a signal unless the decision is `ALLOW_PAPER_SIGNAL`, so the signal and provenance screens have nothing to render against the live platform. See the note in the top-level `frontend/README.md`. |
| `alerts.json` | `/alerts` requires a persistent store; the local run uses the in-memory driver and the endpoint returns an empty list. |
| `news.json` | No news source is configured in the local run. |
| `backtests.json`, `backtest-*.json` | No backtest had been submitted. Includes one `COMPLETED` run and one `RUNNING` run so both states are demonstrable. |
| `candles.json`, `candles-<TICKER>.json` | Candles are read from the persistent store, which is not configured locally. |
| `models.json` | The shipped artifact `ml/artifacts/direction-3class-v1.000339040.d95f1a79.json` **fails validation at load time** — its `metadata` block omits `test_samples`, `test_accuracy` and `test_base_rate`, and `mlinfer` refuses to serve a model whose accuracy is not recorded next to its base-rate benchmark. This fixture is what the registry would return once that artifact loads; its feature list and digest are taken from the artifact itself. |
| `paper-positions.json`, `paper-orders.json` | The broker had received no orders. The portfolio fixture's `positions` and `sector_exposure` were updated to agree with these. |
| `readyz-degraded.json` | Not served by default. Copy over `readyz.json` to exercise the "platform is not ready, signal emission is suspended" banner. |

### Fixtures chosen to be awkward on purpose

A fixture set that only contains flattering data is not much of a test.

- `model-health.json` carries the real measurement: **accuracy 0.4033 against a
  base rate 0.3993**, over 1,768 resolved predictions. The model is 0.4
  accuracy points above always guessing the most common class. Any UI that shows
  accuracy without the base rate turns that into "40% accurate" and tells the
  reader nothing true.
- `evaluations.json` contains real wrong predictions, so `/predictions` renders
  the model being wrong rather than a curated highlight reel.
- The feature snapshot in `provenance-01a7c3d2-….json` marks `sma_200` and
  `vol_of_vol_20` **cold**, so the cold-feature marking is exercised.
- `provenance-01a7c3b0-….json` is deliberately **not reproducible**: its feature
  snapshot, prediction and risk assessment are all missing, which is the state
  the provenance screen must refuse to present as complete.
- The completed backtest reports a Sharpe of 0.914 and a **deflated Sharpe of
  0.187 over 48 trials** — the case where the headline number survives selection
  bias and the honest one does not.
- `paper-orders.json` includes a `REJECTED` order with its reject reason.
- `alerts.json` includes a `CRITICAL` stale-data alert and a `MODEL_DRIFT`
  warning.
- The regime in every fixture is `RANGE` with confidence 0.68 and a runner-up
  (`LOW_VOLATILITY`) scoring 0.680 — essentially a tie, so the "the
  classification is not settled" path renders.

## Adding or refreshing a fixture

```bash
TOKEN=$(curl -s -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"subject":"operator","password":"operator"}' | jq -r .data.token)

curl -s -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/market/regime | jq . > mocks/market-regime.json
```

Save the **whole envelope**, not just `data`. The filename must match what
`resolveMock` expects — the mapping is in `src/lib/server/mock-transport.ts`,
and `src/lib/server/mock-transport.test.ts` asserts that every route the client
can call resolves to a fixture, so a missing one fails `npm test` rather than
surfacing as an empty page mid-demo.

There is no live event stream in mock mode. `/api/stream` emits a single frame
saying so and closes, which exercises the dashboard's disconnected-stream state
honestly instead of faking a feed.

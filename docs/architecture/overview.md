# QuantOS — Architecture Overview

> **Educational / research platform.** QuantOS produces *probabilistic* market
> scenarios, rankings, risk assessments and **paper-trading** signals. It does not
> execute real-money orders, and nothing it emits is financial advice. See
> [`docs/security/governance.md`](../security/governance.md) for the safety charter.

---

## 1. What the system is

QuantOS is an event-driven, cloud-native platform that continuously ingests market
data, derives deterministic features, classifies the prevailing market regime, runs
calibrated statistical models, applies a risk engine with **veto authority**, and
emits fully-traceable paper-trading signals with machine-generated explanations.

The product answers ten questions, and every answer is reproducible from stored
inputs:

| # | Question | Owning component |
|---|----------|------------------|
| 1 | What is the current market regime? | `internal/regime` |
| 2 | Which stocks have the strongest setups? | `internal/scoring`, `internal/opportunity` |
| 3 | Probability of short-term scenarios? | `internal/predict` |
| 4 | Why does the system believe that? | `internal/rules` (evidence) + `internal/analyst` (prose) |
| 5 | What risks invalidate the thesis? | `internal/risk`, `internal/signal` (invalidation) |
| 6 | What events could change the setup? | `internal/news`, corporate-event calendar |
| 7 | How did previous predictions perform? | `internal/evaluation` |
| 8 | Is the model calibrated? | `internal/evaluation` (Brier, reliability, drift) |
| 9 | Which strategies work in which regime? | `internal/backtest` + regime-partitioned metrics |
| 10 | What should paper trading monitor? | `internal/paper`, `internal/alerts` |

## 2. The inviolable pipeline

```
Market Data ─▶ Validation ─▶ Feature Engineering ─▶ Deterministic Rules
     ─▶ Quantitative Models ─▶ ML Predictions ─▶ Market Regime ─▶ Risk Engine
     ─▶ Opportunity Scoring ─▶ Policy Validation ─▶ AI Explanation
     ─▶ Paper-Trading Signal ─▶ Monitoring ─▶ Outcome Evaluation
```

**Core design principle: an LLM never invents a trading signal.**

The LLM sits *downstream* of every numeric decision. It receives a frozen,
structured `analyst.Input` and may only summarise, contrast and explain. A
grounding validator (`internal/analyst/grounding.go`) parses every numeric token
out of the generated prose and rejects the narrative if a number is not present in
the structured input within tolerance. A rejected narrative degrades to the
deterministic template renderer; the signal itself is unaffected because the
signal was already final before the LLM was called.

This is enforced structurally, not by convention: `internal/llm` has no import
edge to `internal/signal`, `internal/risk`, `internal/predict` or `internal/scoring`,
and `tests/integration/architecture_test.go` fails the build if that edge appears.

## 3. Service topology

```
                             QuantOS
                                |
                     ┌──────────┴──────────┐
                  Dashboard            API Gateway
                (Next.js/TS)        (REST + SSE + RBAC)
                                         |
        ┌───────────────┬────────────────┼───────────────┬────────────────┐
   market-service  prediction-svc    signal-svc      risk-svc        alert-svc
        │               │                │               │                │
        └───────────────┴────────────────┼───────────────┴────────────────┘
                                       Kafka
                                         │
        ┌──────────────┬─────────────────┼─────────────────┬───────────────┐
   market data     news pipeline    fundamentals      evaluation-svc   portfolio-svc
        └──────────────┴─────────────────┼─────────────────┴───────────────┘
                                Feature Engineering
                                         │
                    ┌────────────────────┼────────────────────┐
             Technical Engine      Regime Engine          ML Engine
                    └────────────────────┼────────────────────┘
                                   Signal Engine
                                         │
                                    Risk Engine  (veto)
                                         │
                                 Opportunity Engine
                                         │
                          ┌──────────────┴──────────────┐
                     AI Analyst                    Alert Engine
                          └──────────────┬──────────────┘
                                    Dashboard
```

Every service is a thin `main()` over shared `internal/` packages. That is a
deliberate monorepo choice — see [ADR-011](../adr/ADR-011-monorepo-single-module.md).

## 4. Runtime modes

QuantOS runs in three modes, selected by `QUANTOS_MODE` / `--mode`:

| Mode | Bus | Stores | Purpose |
|------|-----|--------|---------|
| `embedded` | in-process fan-out | in-memory | `make demo`, tests, laptops with no Docker |
| `compose` | Kafka | Postgres + Redis + ClickHouse | `make dev`, full local stack |
| `cluster` | MSK | RDS + ElastiCache + ClickHouse | EKS |

The *same* engine code runs in all three. The bus and store abstractions
(`internal/bus`, `internal/store`) are the only thing that changes. This is what
makes the demo scenarios reproducible and the integration tests fast.

## 5. Data flow, in detail

1. **Ingestion.** `market-service` polls or streams from a `MarketDataProvider`
   (`internal/marketdata`). Quotes/trades/bars are stamped with a monotonic
   `seq`, an `event_id` (UUIDv7) and the ingest `trace_id`, then validated:
   non-positive prices, crossed books, absurd gaps, stale timestamps and
   duplicate sequence numbers are quarantined to `market.rejected`.
2. **Staleness gate.** A per-symbol freshness tracker publishes
   `market.stale` when the last accepted tick exceeds
   `market.staleness_threshold`. Downstream engines *refuse to emit new signals*
   for stale symbols (§35 of the requirements). This is a hard gate, not a warning.
3. **Features.** `internal/features` maintains incremental, allocation-free
   rolling windows per symbol and emits a `FeatureSnapshot` — a complete,
   immutable, hash-addressed record of every input a downstream model saw.
   The snapshot hash is stored on the prediction and the signal, which is what
   makes "can the entire decision be reproduced?" answerable with `yes`.
4. **Regime.** `internal/regime` fuses SPY/QQQ trend, VIX level and term
   structure, breadth, volume, yields and DXY into a `MarketRegime` with
   confidence. Regime changes are debounced (hysteresis) to avoid flapping.
5. **Rules.** `internal/rules` evaluates YAML strategies. Rules are pure
   functions of a snapshot; each fires with a weight and a human-readable
   evidence string. No strategy logic lives in Go source.
6. **Prediction.** `internal/predict` combines a rule-derived prior with a
   calibrated model (`internal/mlinfer`, artifact from `ml/training`) to produce
   a 3-class distribution (UP / FLAT / DOWN) per horizon, with explicit
   `Confidence`, `Uncertainty`, `ModelVersion`, `FeatureHash` and `Timestamp`.
7. **Risk.** `internal/risk` runs ten checks and returns
   `ALLOW_PAPER_SIGNAL | WATCH_ONLY | BLOCK`. **BLOCK wins over everything.**
8. **Scoring & opportunity.** Ten sub-scores → `final_score` 0-100 →
   classification. Combined with prediction, liquidity and event risk into an
   `OpportunityScore`.
9. **Signal.** Emitted only if risk allows. Carries explicit invalidation
   conditions that the signal service re-evaluates on every tick and regime change.
10. **Explanation.** AI analyst, grounded and validated (§2 above).
11. **Paper trading & alerts.** Idempotent, keyed by `signal_id`.
12. **Evaluation.** Every prediction becomes an evaluation record when its
    horizon elapses; accuracy, Brier score, calibration and regime-partitioned
    performance feed the model-health dashboard and drift alerts.

## 6. Traceability contract

Every event carries an `Envelope`:

```go
type Envelope struct {
    EventID       string  // UUIDv7, dedup key
    Type          string  // topic-qualified event type
    OccurredAt    time.Time
    ProducedAt    time.Time
    TraceID       string  // W3C trace id, propagated end-to-end
    SpanID        string
    CorrelationID string  // ties an entire causal chain together
    CausationID   string  // the EventID that directly caused this one
    PartitionKey  string  // ticker, for ordering
    SchemaVersion int
    Payload       json.RawMessage
}
```

Given a `signal_id`, `GET /api/v1/signals/{id}/provenance` walks
`CausationID` backwards and returns: the feature snapshot (and its hash), the
regime record, the model version and artifact digest, every rule that fired, the
risk assessment with each check's verdict, the news events considered, and the
evaluation outcome once known. That endpoint *is* the answer to §50.

## 7. Storage

| Store | Holds | Why |
|-------|-------|-----|
| PostgreSQL | stocks, strategies, signals, risk assessments, paper orders/positions, portfolios, model versions, evaluations, users, audit log | transactional, relational, needs constraints and joins |
| ClickHouse | quotes, trades, bars, feature snapshots, prediction records, alert history | append-only, high volume, columnar scans for calibration and backtests |
| Redis | latest quote, latest regime, rate-limit buckets, dedup set, hot feature cache | sub-millisecond reads, TTL semantics |
| S3 | historical datasets, model artifacts, backtest reports | durable, cheap, versioned |

See [ADR-003](../adr/ADR-003-postgres-vs-clickhouse.md).

## 8. Failure model

QuantOS degrades along one axis: **it gets quieter, never wronger.**

| Failure | Behaviour |
|---------|-----------|
| Market data stale/outage | stop emitting signals for affected symbols; serve last-known state marked `stale`; alert |
| Kafka outage | producers buffer to a bounded on-disk WAL, then shed with backpressure; consumers resume from committed offsets; embedded bus fallback available |
| Postgres outage | reads served from Redis/ClickHouse where possible; writes queued to WAL; signal emission disabled (cannot persist provenance ⇒ cannot claim reproducibility) |
| ClickHouse outage | features/predictions buffered; analytics endpoints return `503` with `Retry-After`; live path unaffected |
| Redis outage | fall through to source of truth, rate limiter fails **closed** for writes and open for reads |
| Model service failure | fall back to deterministic rule prior, mark `model_version="rules-fallback"`, lower confidence ceiling |
| LLM failure | deterministic template explanation; `explanation_source="template"` |
| Partial service failure | each service has independent liveness/readiness; the gateway serves partial payloads with `degraded[]` listing what is missing |

Full analysis in [`docs/operations/failure-modes.md`](../operations/failure-modes.md).

## 9. SLOs

See [`docs/operations/slo.md`](../operations/slo.md). Headline targets:
market event processing P99 < 100 ms, API P99 < 300 ms, alert generation
P99 < 500 ms, prediction P95 < 1 s.

## 10. Where to go next

- [`docs/architecture/data-flow.md`](data-flow.md) — sequence diagrams
- [`docs/architecture/components.md`](components.md) — package-by-package responsibilities
- [`docs/adr/`](../adr/) — the eleven decision records
- [`docs/api/rest.md`](../api/rest.md) — API surface
- [`docs/runbooks/`](../runbooks/) — operational procedures
- [`docs/ml/`](../ml/) — modelling, calibration, walk-forward protocol

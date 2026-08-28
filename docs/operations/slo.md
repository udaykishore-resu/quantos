# QuantOS — Service Level Objectives

SLOs are measured over a rolling 28-day window. Each has an error budget; budget
burn drives alerting (multi-window, multi-burn-rate) rather than raw threshold
crossings.

## Latency

| SLI | Objective | Measured as | Notes |
|-----|-----------|-------------|-------|
| Market event processing | P99 < 100 ms | `market_event_processing_duration_seconds`, from envelope `ProducedAt` at ingest to `features.updated` publish | The dominant cost is incremental indicator update (~5 µs) plus bus publish; the budget is mostly network and serialisation |
| API read | P99 < 300 ms | `http_server_duration_seconds{route,method="GET"}` | Cache-hit path is ~5 ms; the tail is Postgres joins on provenance |
| API write | P99 < 500 ms | same, `method!="GET"` | Includes durable write + audit log |
| Alert generation | P99 < 500 ms | condition true (domain time) → `alert.generated` published | Excludes external delivery (email/webhook), which has its own SLO |
| Prediction | P95 < 1 s | `features.updated` consumed → `prediction.generated` published | Inference itself is µs; the budget covers regime lookup, rules and bus hops |
| SSE fan-out | P95 < 200 ms | hub receive → socket write | |
| AI explanation | P95 < 8 s, best-effort | LLM call duration | **Off the critical path.** Never gates a signal |

## Availability and correctness

| SLI | Objective |
|-----|-----------|
| API availability (non-5xx / total, excluding 4xx) | 99.9 % |
| Ingestion continuity (minutes with ≥1 accepted tick per active symbol, during market hours) | 99.5 % |
| Signal provenance completeness (signals whose full provenance chain resolves) | 100 % — any miss is a bug, not a budget |
| Alert duplication rate | 0 — enforced by idempotency, alerted on any occurrence |
| Paper order duplication rate | 0 — same |
| Prediction evaluation coverage (predictions with a recorded outcome within horizon + 1 h) | 99.9 % |
| Consumer lag (P95, per group, market hours) | < 5 s |

## Capacity assumptions behind these numbers

- Universe: 500 symbols active, 2 000 configured.
- Quote rate: 5/s/symbol steady, 50/s/symbol burst → 2.5 k–25 k events/s.
- Feature updates: 1 per bar per symbol on 1-minute bars = ~8 events/s, plus
  event-driven recomputation on material ticks.
- Predictions: ~4 horizons × 500 symbols per bar = 2 000/min.
- Signals: single digits to low hundreds per day (the risk engine is a strong filter).

At these rates a single feature-engine instance is CPU-bound at roughly 40 k
events/s on 2 vCPU, so the SLOs have ~10× headroom at steady state. Scaling is
by partition count, and the HPA targets consumer lag rather than CPU.

## Deliberate non-goals

- **Sub-millisecond tick-to-signal.** QuantOS is not a low-latency execution
  system; it makes minute-scale decisions and there is no advantage in shaving
  microseconds off a path that ends in a paper order.
- **Optimising the LLM path.** Explanations are asynchronous and cached; making
  them faster changes nothing about decision quality.
- **Premature ClickHouse tuning.** Query patterns are measured first; the
  ordering key `(ticker, ts)` covers the known access paths.

## Trade-offs made for these targets

- Per-symbol ordering constrains parallelism to partition count. Chosen over the
  alternative (out-of-order features) because correctness beats throughput here.
- Synchronous durable write before signal publication costs ~10 ms of the alert
  budget, and buys the provenance guarantee.
- The staleness gate deliberately converts a data-availability problem into
  *reduced output* rather than degraded output, which shows up as an ingestion
  SLO miss, not a correctness one. That is the intended shape.

## Alerting policy

Multi-burn-rate on each budget: page at 14.4× over 1 h *and* 6× over 6 h;
ticket at 3× over 24 h. Raw-threshold pages exist only for the zero-budget SLIs
(duplication, provenance completeness) and for the kill switches (drawdown,
severe model drift).

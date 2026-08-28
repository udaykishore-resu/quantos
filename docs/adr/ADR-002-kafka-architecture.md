# ADR-002 — Kafka topic design, partitioning and delivery semantics

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

QuantOS is causally chained: a quote produces features, which produce a
prediction, which produces a risk assessment, which may produce a signal, which
may produce an alert and a paper order. Two properties matter more than raw
throughput:

1. **Per-symbol ordering.** Features computed from bar *n* must not be
   overtaken by bar *n−1*.
2. **Exactly-once *effects*.** Duplicate delivery is acceptable; duplicate
   alerts and duplicate paper orders are not.

We also need topics that can be replayed to rebuild derived state, and a
partitioning scheme that does not hot-spot on `SPY`.

## Options

1. **One topic, one type field.** Simple, but consumers deserialise everything,
   retention policies cannot differ per stream, and a slow consumer of news
   blocks quotes.
2. **Topic per event type, partitioned by ticker.** Chosen.
3. **Topic per ticker.** Ordering is trivial but partition count explodes
   (S&P 500 × 14 types) and rebalances become pathological.
4. **No Kafka; direct RPC chaining.** Loses replay, loses buffering under
   backpressure, couples deploy lifecycles, and makes the backtest/live
   symmetry impossible.

## Decision

**Topic per event type, keyed by ticker**, with the envelope in ADR-010.

```
market.quotes          12 partitions   retention 24h    key=ticker
market.trades          12              24h              key=ticker
market.bars            12              30d              key=ticker
market.news             6              30d              key=ticker|"__MACRO__"
market.events           3              90d              key=ticker
market.stale            3              7d               key=ticker
market.rejected         3              7d               key=ticker
features.updated       12              7d               key=ticker
regime.updated          1              90d              key="__MARKET__"
prediction.generated   12              30d              key=ticker
signal.generated        6              1y (compacted)   key=signal_id
signal.invalidated      6              1y               key=signal_id
risk.updated            6              30d              key=ticker
alert.generated         6              90d              key=alert_id
prediction.evaluated    6              1y               key=prediction_id
model.drift.detected    1              1y               key=model_version
```

- **Ordering:** partition key = ticker for everything on the live path, so all
  events for one symbol land on one partition and are processed in order by one
  consumer instance.
- **`regime.updated`** is single-partition on purpose: there is exactly one
  market regime and total order over it is worth more than parallelism.
- **Delivery:** at-least-once with **idempotent consumers**. Every consumer
  writes `(consumer_group, event_id)` to a dedup store (Redis set with TTL >
  max retention lag, backed by a Postgres unique index for the effectful
  consumers) *inside the same transaction* as its effect where the sink is
  Postgres. Kafka transactions are used only for the read-process-write hop in
  `signal-service`, where the effect is another Kafka record.
- **Offsets** are committed after the effect, never before.
- **Schema:** JSON with an explicit `schema_version` in the envelope, validated
  against `docs/api/schemas/*.json` in a CI contract test. Protobuf was rejected
  for now (see ADR-004) but the envelope is codec-agnostic.
- **Consumer groups** are named `<service>.<purpose>` so that adding a second
  reader of a topic never disturbs the first.

## Trade-offs

- (+) Replay rebuilds any derived state; a backtest is "the same consumers, fed
  from a bounded replay".
- (+) Independent retention and scaling per stream.
- (+) Per-symbol ordering without per-symbol topics.
- (−) 16 topics to operate; partition counts need review as the universe grows.
- (−) Hot symbols (`SPY`, `QQQ`) concentrate load on one partition. Accepted:
  measured throughput per partition is ~3 orders of magnitude above the hot-key
  rate. If it ever binds, the escape hatch is a composite key
  `ticker#bucket(seq)` for non-order-sensitive topics only.
- (−) At-least-once forces dedup discipline in every consumer.

## Failure modes

- *Broker unavailable.* Producers buffer into a bounded on-disk WAL
  (`internal/bus/wal.go`), then apply backpressure and finally shed with a
  counter. No silent drops.
- *Consumer lag growth.* `kafka_lag` per group is an SLO-backed alert; the
  signal path additionally refuses to act on events older than
  `max_event_age` so lag turns into *silence*, not into stale trading signals.
- *Rebalance storms.* Cooperative-sticky assignor, `max.poll.interval.ms` sized
  to 3× the P99 processing time, and processing is chunked so a poll loop never
  blocks longer than the interval.
- *Poison message.* Three retries with jitter, then routed to
  `<topic>.dlq` with the failure reason; a DLQ depth > 0 pages.
- *Dedup store loss.* Redis loss degrades to the Postgres unique index for
  effectful consumers; alert and paper-order idempotency survive a full Redis
  wipe.

# ADR-010 — Event-driven core with an idempotent, traceable envelope

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

QuantOS must ingest continuously, react within milliseconds, survive partial
outages, replay history to rebuild derived state, and answer "reproduce this
decision" months later. A synchronous request-chained architecture fails the
second, third and fourth of those.

## Options

1. **Synchronous chain (market → features → predict → risk → signal via RPC).**
   Simple mental model. One slow component stalls ingestion; no replay; a
   deployment of any link is an outage of the chain; backpressure propagates as
   errors rather than as buffering.
2. **Batch/cron pipeline.** Fine for daily research, useless for intraday alerts.
3. **Event-driven with a durable log.** Chosen.
4. **Actor framework / in-process only.** Fast, but loses durability and
   cross-service replay, and couples scaling to a single process.

## Decision

The durable log (Kafka, ADR-002) is the backbone. Every component is a consumer
that produces further events. Three properties are mandatory for every event:

### 1. The envelope

```go
type Envelope struct {
    EventID       string          // UUIDv7 — time-ordered, globally unique, the dedup key
    Type          string          // "prediction.generated"
    SchemaVersion int
    OccurredAt    time.Time       // when the fact happened (domain time)
    ProducedAt    time.Time       // when we emitted it (wall time)
    TraceID       string          // W3C trace, propagated from ingestion
    SpanID        string
    CorrelationID string          // stable across an entire causal chain
    CausationID   string          // the EventID that directly caused this one
    PartitionKey  string
    Producer      string          // service name + version
    Payload       json.RawMessage
}
```

`CausationID` + `CorrelationID` are what make the provenance endpoint possible:
`GET /api/v1/signals/{id}/provenance` walks causation backwards from the signal
to the originating market event, returning every intermediate artifact.

### 2. Idempotency

Every consumer is idempotent (§36). The pattern is uniform:

- Compute a **deterministic effect key** — for alerts,
  `sha256(signal_id | alert_type | bucket(occurred_at))`; for paper orders,
  `signal_id | intent_seq`.
- Check-and-set the key in the dedup store *in the same transaction as the
  effect* when the sink is Postgres (unique index does the work); otherwise via
  Redis `SET NX` with a TTL longer than the maximum replay window.
- Commit the Kafka offset **after** the effect.

Result: at-least-once delivery with exactly-once effects. Reprocessing a whole
day of `signal.generated` produces zero duplicate alerts and zero duplicate
paper orders — asserted by an integration test that replays a fixture twice.

### 3. Ordering and time

- Per-symbol ordering via the partition key.
- Components reason in **domain time** (`OccurredAt`), never wall time. A
  component that needs "now" takes a `Clock`.
- Late events (`OccurredAt` older than `max_event_age`) are recorded but do not
  produce new signals. Late data becomes evaluation input, not action input.

### Embedded bus

`internal/bus` defines `Publisher`/`Subscriber` and ships two implementations:
`kafka` and `memory`. The memory bus preserves per-key ordering and supports
deterministic drain, which is what lets the entire platform — and all ten demo
scenarios — run in one process with no infrastructure, and what makes
integration tests fast and reliable.

## Trade-offs

- (+) Components deploy, scale and fail independently.
- (+) Replay rebuilds derived state and powers backtests.
- (+) Full causal traceability, which is a stated core product requirement.
- (+) Backpressure is absorbed by the log instead of surfacing as cascading errors.
- (−) Eventual consistency: the dashboard can briefly show a prediction whose
  signal has not yet appeared. Handled by rendering pipeline stage explicitly
  rather than pretending it is atomic.
- (−) Debugging a distributed causal chain is harder than a stack trace.
  Mitigated by end-to-end trace propagation and the provenance endpoint.
- (−) Idempotency discipline is required everywhere and is easy to forget.
  Mitigated by a shared `bus.IdempotentHandler` wrapper that consumers must use;
  a lint rule flags raw handler registration.
- (−) Operating Kafka is real work. Mitigated in dev by the memory bus and in
  prod by MSK.

## Failure modes

- *Duplicate delivery.* Absorbed by dedup keys.
- *Out-of-order across partitions.* Only cross-symbol ordering can invert, which
  no component depends on. Asserted by a property test.
- *Consumer lag.* Turns into silence (max event age), not stale action.
- *Poison event.* Bounded retries → DLQ with reason → alert.
- *Dedup store outage.* Postgres unique indexes remain authoritative for the two
  effects that must never duplicate (alerts, paper orders).
- *Envelope schema evolution.* Additive-only; `SchemaVersion` gates readers;
  contract tests run old fixtures against new consumers.

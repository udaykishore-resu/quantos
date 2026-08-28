# Events

QuantOS is event-driven. Market data arrives, a bar is published, and everything
downstream — features, regime, prediction, risk verdict, signal, alert,
eventually a scored outcome — is a consequence of that bar, published as its own
event with a pointer back to what caused it.

That backward pointer is the reason the architecture is worth its cost. Given
any signal, you can walk the chain to the exact bar that produced it, without
guessing and without a join on timestamps. This document explains the sixteen
topics, the envelope every event travels in, and the two behaviours — idempotency
and age filtering — that decide what a consumer does when delivery is imperfect.

The platform is educational research and paper trading. No event on any topic
reaches a broker.

---

## The envelope

Every event is an `Envelope` with a JSON `payload` and this metadata around it:

| Field | Required | Meaning |
|---|---|---|
| `event_id` | yes | UUIDv7-shaped: 48 bits of millisecond timestamp then randomness. Time-ordered, so it sorts by creation and keeps the dedup index and log lookups efficient |
| `type` | yes | The topic name. The envelope names its own type so a consumer reading a dead-letter queue knows what it holds |
| `schema_version` | yes | Positive integer, currently 1. Bumped only for breaking envelope changes |
| `occurred_at` | yes | When the thing happened, in domain time — the bar's close, the quote's timestamp, the news item's publication |
| `produced_at` | yes | When the envelope was built. The gap between this and `occurred_at` is the platform's own latency, separable from the source's |
| `trace_id` | no | W3C trace id, inherited from the request or event that led here |
| `span_id` | no | The producing span |
| `correlation_id` | no | Groups everything belonging to one logical operation. Falls back to the trace id when unset |
| `causation_id` | no | The `event_id` of the single event that directly caused this one |
| `partition_key` | yes | What ordering is guaranteed on. Almost always the ticker |
| `producer` | no | Which component emitted it, e.g. `quantos/engine` |
| `payload` | yes | The domain object as JSON |

`Validate` rejects an envelope with an empty `event_id`, `type` or `payload`, a
zero `occurred_at`, or a non-positive `schema_version`, so a consumer never has
to defend against those.

Two fields are set by the driver on delivery and are not part of the
producer-side contract: `Offset` and `Partition`. They are excluded from the
JSON encoding entirely, so an event that round-trips through storage does not
carry a stale offset from a previous delivery.

### `correlation_id` and `causation_id` are not the same thing

`correlation_id` is a *set* label: everything on one logical operation shares it,
however deep the chain runs. It answers "what else belongs to this?"

`causation_id` is a *link*: it names the one event that directly caused this one.
It answers "what produced this, specifically?" Follow it repeatedly and you walk
the chain backwards, one hop at a time.

Both are inherited automatically. A consumer derives its handler context from
the envelope it received, which installs the trace, the correlation id and — as
the causation for anything it emits — that envelope's `event_id`. Emissions made
from that context are linked without the handler doing anything.

---

## Walking a decision backwards

This is the payoff. Suppose a signal looks wrong and you want the bar behind it.

```
signal.generated        event_id = E4   causation_id = E3   correlation_id = C
  └─ risk.updated       event_id = E3   causation_id = E1   correlation_id = C
  └─ prediction.gen…    event_id = E2   causation_id = E1   correlation_id = C
       └─ features.upd… event_id = E1   causation_id = E0   correlation_id = C
            └─ market.bars   event_id = E0   correlation_id = C
```

Every event the bar handler emits is built with the incoming bar envelope as its
parent, so each carries `causation_id = E0` directly, and each carries the bar's
`correlation_id`. Reading the chain therefore works two ways, and both are
useful:

**By causation, one hop at a time.** Take the signal's `causation_id`, fetch that
event, take *its* `causation_id`, and repeat until you reach an event with none.
That last one is the root — the bar, or the quote, or the news item. It gives you
the precise lineage with nothing else mixed in.

**By correlation, all at once.** Query every event sharing the signal's
`correlation_id`. You get the whole fan-out from that bar — the feature snapshot,
the prediction, the risk assessment, the alerts, the signal — in one result,
which is what you want when the question is "what else did this bar do?" rather
than "what caused this?"

The chain has a REST counterpart. `GET /api/v1/signals/{id}/provenance` assembles
the same picture from the store and tells you whether it is complete: when
`reproducible` is true, the stored feature snapshot plus the named model artifact
recompute the prediction exactly. Use the event chain when you need timing and
the causal ordering; use provenance when you need the inputs.

A caveat worth knowing: the sweeper and the evaluator publish with
`builder.New` rather than `builder.Caused`, because what they react to is the
passage of time rather than an incoming event. `signal.invalidated` emitted by
the sweeper, `prediction.evaluated` and `model.drift.detected` therefore have no
`causation_id`. The same `signal.invalidated` type *does* carry one when it is
emitted from the bar handler, where a bar caused it. Do not assume the field is
present on a topic; check it.

---

## The sixteen topics

Partition count, retention and compaction are the authoritative specification —
provisioning renders them, and `quantos topics` prints them. Changing a topic
name is a breaking change requiring a schema-version bump and a migration plan.

| Topic | Parts | Retention | Compact | Partition key |
|---|---|---|---|---|
| `market.quotes` | 12 | 24h | no | ticker |
| `market.trades` | 12 | 24h | no | ticker |
| `market.bars` | 12 | 30d | no | ticker |
| `market.news` | 6 | 30d | no | ticker (`__MACRO__` for market-wide) |
| `market.events` | 3 | 90d | no | ticker |
| `market.stale` | 3 | 7d | no | ticker |
| `market.rejected` | 3 | 7d | no | ticker |
| `features.updated` | 12 | 7d | no | ticker |
| `regime.updated` | 1 | 90d | no | `__MARKET__` |
| `prediction.generated` | 12 | 30d | no | ticker |
| `signal.generated` | 6 | 365d | **yes** | signal id |
| `signal.invalidated` | 6 | 365d | no | signal id |
| `risk.updated` | 6 | 30d | no | ticker |
| `alert.generated` | 6 | 90d | no | alert id |
| `prediction.evaluated` | 6 | 365d | no | prediction id |
| `model.drift.detected` | 1 | 365d | no | model version |

Three of those choices are deliberate rather than incidental.

**The partition key is the ticker on every live-path topic**, which is what gives
per-symbol ordering: bars for one instrument are never reordered relative to each
other, and the feature engine can therefore hold per-symbol state without
defending against out-of-order delivery.

**`regime.updated` is single-partition on purpose.** There is one market regime,
and total order over it is worth more than parallelism. A consumer that saw a
regime transition out of order would gate rules on an environment that had
already been superseded.

**`signal.generated` is the only compacted topic.** Compaction keyed on the
signal id means the log retains the latest state of every signal rather than
every intermediate write, so a consumer rebuilding the signal set from the log
gets the current picture without replaying a year of history. It is compacted
and `signal.invalidated` is not, because the invalidation stream is a record of
events that happened and collapsing it would lose the history that makes the
lifecycle auditable.

### What each topic carries

#### `market.quotes` — top-of-book snapshots
Payload: `Quote`. Produced by `<service>/ingest`, after validation: a quote with
a crossed book, a negative price, no price at all, or a two-sided quote with no
size on a side is rejected instead, and goes to `market.rejected`. A book with no
size is not a market you could trade against, and the spread it implies is
fiction that the risk engine's spread check would read as real.

Consumed by the **engine** group, which feeds the pipeline's quote path, and by
the **stream** group, which throttles them to at most one frame every 500 ms
before fanning them out to dashboards. An unthrottled quote fan-out is how an SSE
hub becomes the bottleneck.

#### `market.trades` — individual execution prints
Payload: `Trade`. **Provisioned but not used in-tree.** No component publishes to
it and none consumes it; the topic exists so a provider adapter that carries
prints has somewhere to put them without a topic-list change. Do not build on it
expecting traffic.

#### `market.bars` — completed OHLCV bars
Payload: `Candle`. Produced by `<service>/ingest`, validated first — a bar whose
high is below its low or below its open, whose prices are non-positive, or whose
end precedes its start is rejected to `market.rejected`.

Consumed by the **engine** group. This is the topic that drives the decision
path: every feature snapshot, prediction, risk assessment, signal and alert
descends from a bar, which is why it is the root of nearly every causation chain.

#### `market.news` — processed news events
Payload: `NewsEvent`. Produced by `<service>/news` on its poll interval. Only the
typed fields cross into the decision path; the raw article text never does.
Deterministic classification runs before any language model, and a model may
agree or add detail but never overturn it.

No in-tree consumer subscribes to this topic — the news pipeline writes into the
read model and the decision pipeline directly, in the same process, and publishes
here for consumers outside the platform.

#### `market.events` — the corporate-action calendar
Payload: `CorporateEvent`. **Provisioned but not used in-tree.** The calendar
reaches the risk engine's event-proximity check through the pipeline rather than
over the bus. The topic and its 90-day retention exist for a calendar source that
publishes asynchronously.

#### `market.stale` — instruments whose data has aged out
Payload: `{ticker, age_seconds, reason, hard}`. Produced by `<service>/ingest` on
every tick, for each instrument the freshness validator reports stale.

It exists so downstream consumers *suspend* rather than act on old prices. `hard`
distinguishes the soft threshold, beyond which no new signal is created, from the
hard one, beyond which the symbol is treated as offline. Seven-day retention is
enough to investigate an outage and no more.

#### `market.rejected` — data that failed validation
Payload: `{ticker, reason}`. Produced by `<service>/ingest` for every quote or bar
that failed structural validation.

Publishing rejections rather than dropping them silently is the point: a feed
that has started emitting crossed books looks exactly like a quiet feed if you
only count what was accepted.

#### `features.updated` — feature snapshots
Payload: `FeatureSnapshot`. Produced by `<service>/engine` for every bar, before
anything else in the chain, and caused by that bar.

The snapshot is the unit of reproducibility: it plus a model artifact digest is
sufficient to recompute a prediction exactly. Retention is only 7 days because
the durable copy lives in the store — the topic is a transport, not the archive.

#### `regime.updated` — the market regime
Payload: `MarketRegime`. Produced by `<service>/engine` when a macro bar is
processed, partition key `__MARKET__`.

Consumed by the **stream** group and forwarded to dashboards as the `regime`
event. A regime must persist past the configured hysteresis before it replaces
the incumbent: flapping regimes are worse than a slightly stale one, because
every downstream threshold keys off the regime.

#### `prediction.generated` — model output
Payload: `Prediction`. Produced by `<service>/engine`, caused by the bar.
Carries every horizon's distribution plus the rule prior, the raw model output
and the blend weight, so the served number can be decomposed after the fact.

Consumed by the **stream** group as the `prediction` event.

#### `signal.generated` — an emitted signal
Payload: `Signal`. Produced by `<service>/engine`, caused by the bar, partition
key the signal id. Compacted.

There is a gate in front of this publish worth knowing about: if the provenance
store is unavailable, the signal is **not** published and is invalidated instead,
with kind `RISK_VETO` and the note `provenance store unavailable at emission
time`. A signal the platform could not later explain is a signal it declines to
emit at all.

Consumed by the **stream** group as the `signal` event.

#### `signal.invalidated` — a signal whose thesis broke
Payload: `Signal`, with `invalidated_at`, `invalidation_kind` and
`invalidation_note` populated. Produced by `<service>/engine` when a bar
triggered the invalidation, and by `<service>/sweeper` when time or a regime
change did. The sweeper exists separately because a signal must be invalidated by
the passage of time and by a regime change, not only by its own instrument
printing a new bar.

Consumed by the **stream** group as `signal_invalidated`. Not compacted: the
history of what invalidated what is the record that makes the lifecycle
auditable.

#### `risk.updated` — a risk assessment
Payload: `RiskAssessment`, with all eleven checks and their numeric evidence.
Produced by `<service>/engine`, caused by the bar.

No in-tree consumer. It is published so an external risk-monitoring consumer can
observe every verdict, including the ones that allowed a signal, rather than
inferring the engine's behaviour from what it blocked.

#### `alert.generated` — an emitted alert
Payload: `Alert`. Produced by `<service>/engine`, caused by the bar, partition
key the alert id.

Consumed by the **alert-delivery** group, which dispatches to its configured
sinks — the structured log by default, so a deployment with no webhook still has
a durable record, plus any webhook sink. Also consumed by the **stream** group as
the `alert` event.

Duplicate pages are prevented at two separate points, and it is worth being
precise about which does what. **Before** publishing, the durable store's unique
index on `dedup_key` catches an alert the in-memory dedup missed; when it does,
the alert is counted as suppressed and never published at all. **At** delivery,
the alert-delivery handler is wrapped in the same idempotency wrapper as every
other consumer, keyed on the group and the `event_id` — so a redelivery of the
same envelope is dropped, while two distinct envelopes carrying the same
`dedup_key` would each be delivered if they ever reached this point. They do not,
because the store gate runs first.

#### `prediction.evaluated` — a resolved outcome
Payload: `PredictionOutcome`. Produced by `<service>/evaluation` on its sweep,
once a prediction's horizon has elapsed and a **non-stale** price is available.
A stale price is not a resolution price: scoring against one would manufacture
outcomes out of missing data, so the prediction stays pending instead.

Carries the realised outcome, the Brier score, the log loss and the
`risk_decision` that was in force — that last field is what makes the cost of the
risk veto measurable rather than assumed. No `causation_id`: the trigger is
elapsed time, not an event.

#### `model.drift.detected` — drift worth acting on
Payload: `DriftReport`. Produced by `<service>/evaluation`, single partition,
**only** at severity `moderate` or `severe`. Drift is computed every sweep; it is
published only when it crosses that bar, so the topic is a queue of things to act
on rather than a metric stream. Use `GET /api/v1/model-health` for the continuous
picture.

### Consumer groups at a glance

| Group | Subscribes to | Runs in role |
|---|---|---|
| `<prefix>.engine` | `market.quotes`, `market.bars` | `engine` |
| `<prefix>.stream` | `market.quotes`, `regime.updated`, `signal.generated`, `signal.invalidated`, `alert.generated`, `prediction.generated` | `api` |
| `<prefix>.alert-delivery` | `alert.generated` | `alert-delivery` |

The prefix is `bus.group_prefix`, `quantos` by default. A single-process
deployment runs every role; the compose and cluster deployments split them across
services, and that split is the only difference between the topologies.

---

## Idempotency

Delivery is at-least-once. That is the safe direction — losing an event is worse
than seeing one twice — but it means a consumer that applies an effect on every
delivery will eventually apply it twice. Two mechanisms turn at-least-once
delivery into exactly-once *effects*.

### The handler wrapper

Every engine and alert-delivery handler is wrapped so an event is processed at
most once per consumer group. The key is derived from the group and the
`event_id`, so the same event delivered to two different groups is processed by
both — which is correct, they are independent consumers — while a redelivery to
the same group is dropped and counted.

The claim happens *before* the handler runs and is released if the handler
returns an error, so a transient failure does not permanently swallow an event.
The default claim lifetime is 48 hours (`bus.dedup_ttl`).

If the dedup store itself fails, processing continues. That is the deliberate
choice: at-least-once is the safe direction, and the two effects that genuinely
cannot duplicate — alerts and paper orders — additionally carry a unique index in
PostgreSQL, so a Redis wipe cannot cause a duplicate.

### Durable unique indexes

The second layer is where the real guarantee lives for those two effects. An
alert's `dedup_key` and a paper order's `idempotency_key` are unique in the
database. When the in-memory deduper misses — after a restart, an eviction, or a
cache flush — the insert fails and the effect is suppressed. The in-memory layer
is an optimisation; the index is the guarantee.

### Building your own key

Consumers computing their own effect keys should include everything that
distinguishes one effect from another and **nothing that varies between
redeliveries of the same effect**. A wall-clock timestamp is the classic mistake:
it differs on every redelivery, so the key never collides and the deduplication
silently does nothing.

Where an effect should collapse within a time window rather than being unique
forever, bucket the timestamp — rounding down to the minute is what makes "the
same alert condition within the same minute" one effect instead of sixty.

The in-memory deduper is bounded. When it fills, it first sweeps expired entries,
and if that is not enough it drops an arbitrary tenth: bounded memory beats
perfect dedup history, and the durable indexes remain.

---

## `MaxAgeFilter`, or what to do about lag

Consumer lag is not a delay. A bar handled ten minutes late produces a signal
against prices that no longer exist, and that signal looks exactly like a fresh
one to everything downstream.

`MaxAgeFilter` wraps the bar handler and drops any event whose age exceeds
`bus.max_event_age` (5 minutes by default), measured as `now - occurred_at`. Age
is domain age, not delivery age, so an event that sat in the log is judged on
when the thing happened rather than when it arrived.

**Dropped means not acted on, not unnoticed.** The dropped envelope is passed to
a callback which logs it with its event id, ticker and age. Nothing else in the
chain runs: no features, no prediction, no signal.

The filter wraps *outside* the idempotency wrapper on the bar handler, so an
event dropped for age never claims a dedup key. If lag clears and the event is
redelivered while still inside the window, it is processed normally.

The effect is that lag turns into **silence** rather than into stale action. That
is the right trade for this platform: producing nothing is a visible, diagnosable
state — the log says what was dropped and why, `market.stale` names the affected
instruments, and `/healthz` reports the degraded subsystem. Producing confident
signals from ten-minute-old prices is none of those things.

Only the bar handler is age-filtered. Quotes are not, because the quote path
updates a last-known price rather than making a decision, and the freshness gate
downstream is what stops an old price being used. Alert delivery is not, because
a late alert is still worth delivering — it says what happened, not what to do
now.

---

## Related documents

- [`README.md`](./README.md) — authentication, the response envelope, errors and
  the SSE contract.
- [`openapi.yaml`](./openapi.yaml) — the REST schemas, including every payload
  type above.
- [`../adr/ADR-002-kafka-architecture.md`](../adr/ADR-002-kafka-architecture.md)
  and [`../adr/ADR-010-event-driven-architecture.md`](../adr/ADR-010-event-driven-architecture.md)
  — why the bus is shaped this way.

# Runbook — Consumer lag

**Severity:** ticket (page if it crosses into `MarketEventProcessingSLOFastBurn`)
**Alerts:** `ConsumerLagHigh`, `EngineAtPartitionCeiling`, `MarketEventProcessingSLOFastBurn`
**Owner:** platform on-call

---

## Read this first

Lag is not just latency here. `bus.max_event_age` is **5 minutes**, and an
envelope older than that is recorded and then **not acted on**:

> Events older than this are recorded but never acted on: consumer lag turns
> into silence rather than into stale trading signals.
> — `config/quantos.yaml`

So lag has two regimes with completely different meanings:

| Lag | Meaning |
|---|---|
| under 5 minutes of backlog | latency. The SLO is burning; the work will still be done. |
| over 5 minutes of backlog | **data loss by design.** Events are being dropped. |

The threshold in events rather than seconds: at the steady quote rate in
`docs/operations/slo.md` (5/s/symbol, 500 symbols, 12 partitions) a partition
carries roughly 200 events/s, so 5 minutes is on the order of **60 000 events per
partition**. The `ConsumerLagHigh` alert fires at 5 000, which is about
25 seconds — deliberately well before the cliff.

## Symptoms

- `ConsumerLagHigh`: `max(quantos_kafka_lag) > 5000` for 5 m.
- `MarketEventProcessingSLOFastBurn`: the P99 < 100 ms budget burning 14×.
- Signals arriving late rather than not at all.
- `quantos_bus_consumed_total` rate below `quantos_bus_published_total` rate.
- In the over-5-minute regime: signals simply stop for the lagging partitions,
  and `quantos_signals_generated_total` falls without any error.

## Diagnosis

```sh
# Which group, which topic?
kubectl -n quantos exec deploy/api-gateway -- \
  wget -qO- http://localhost:8080/metrics | grep '^quantos_kafka_lag'
```

There are exactly three consumer groups (`Cfg.Bus.GroupPrefix` + suffix):

| Group | Service | Topics |
|---|---|---|
| `quantos.engine` | signal-service | `market.quotes`, `market.bars` |
| `quantos.stream` | api-gateway | `market.quotes`, `regime.updated`, `signal.generated`, `signal.invalidated`, `alert.generated`, `prediction.generated` |
| `quantos.alert-delivery` | alert-service | `alert.generated` |

```sh
# Publish vs consume rate, per topic. A gap here is the whole diagnosis.
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics \
  | grep -E '^quantos_bus_(published|consumed|errors)_total'
```

```sh
# Is the consumer saturated, or is it stuck?
kubectl -n quantos top pods -l app.kubernetes.io/name=signal-service
kubectl -n quantos get hpa signal-service
```

```sh
# Is a rebalance in progress? Repeated rebalances pause every instance.
kubectl -n quantos logs deploy/signal-service --tail=200 | grep -iE 'rebalanc|partition|assign'
```

| Pattern | Cause | Branch |
|---|---|---|
| CPU near the limit, HPA below max | under-provisioned | A |
| HPA at max replicas | at the partition ceiling | B |
| CPU low, lag high | stuck consumer or poison message | C |
| Repeated rebalance log lines | consumers flapping | D |
| `bus_errors_total{op="consume"}` rising | cannot reach the broker | E |

## Remediation

### A. Under-provisioned

The HPA is keyed on `quantos_kafka_lag` at an average of 500 per pod, precisely
so this case resolves itself. If it has not:

```sh
kubectl -n quantos describe hpa signal-service | tail -20
```

`<unknown>` for the custom metric means prometheus-adapter is not exposing it,
and the HPA has silently fallen back to the CPU target — which is exactly the
signal it cannot see.

```sh
kubectl -n monitoring get deploy prometheus-adapter
kubectl get --raw "/apis/custom.metrics.k8s.io/v1beta1" | jq -r '.resources[].name' | grep kafka_lag
```

Fix the adapter, or scale by hand in the meantime:

```sh
kubectl -n quantos scale deploy/signal-service --replicas=8
```

### B. At the partition ceiling

`maxReplicas` is **12** because `market.bars` and `market.quotes` have 12
partitions (ADR-002) and per-symbol ordering means one partition is consumed by
exactly one instance. The thirteenth replica does not consume anything; it idles
in the group.

Adding partitions is the only real fix, and it is not a hot operation:

```sh
# Read the current state first.
kafka-topics.sh --bootstrap-server "$BROKERS" --command-config client.properties \
  --describe --topic market.bars
```

**Before you increase it, understand what changes.** The partition key is the
ticker. Changing the partition count re-hashes every symbol to a different
partition, so:

- ordering is not preserved across the change for any symbol,
- in-flight state in the consumers refers to symbols they no longer own,
- ADR-002's per-symbol ordering guarantee is broken for the duration.

The safe sequence is to drain first:

```sh
# 1. Stop the producer. Ingestion pauses; the staleness gate handles it
#    (see market-data-outage.md) and no bad signal is produced.
kubectl -n quantos scale deploy/market-service --replicas=0

# 2. Let the consumers catch up to zero lag.
watch 'kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics | grep quantos_kafka_lag'

# 3. Repartition.
kafka-topics.sh --bootstrap-server "$BROKERS" --command-config client.properties \
  --alter --topic market.bars --partitions 24

# 4. Raise the ceiling to match, then restart the producer.
kubectl -n quantos patch hpa signal-service --type merge \
  -p '{"spec":{"maxReplicas":24}}'
kubectl -n quantos scale deploy/market-service --replicas=1
```

Then update `infra/kubernetes/base/hpa.yaml`, the chart's
`services.signal-service.autoscaling.maxReplicas`, and — most importantly —
`internal/bus.TopicConfigs`, which is the source of truth all three topic
provisioners render from. A partition count changed only on the broker is one
that the next environment build will silently revert.

### C. Stuck consumer or poison message

CPU low and lag high means it is not working, not that it cannot keep up.

```sh
kubectl -n quantos logs deploy/signal-service --tail=200 | grep -iE 'error|panic|retry'
```

`bus.max_retries` is 2. After that the envelope goes to the topic's dead-letter
partner (`<topic>.dlq`), which every topic has for exactly this reason:

```sh
kafka-console-consumer.sh --bootstrap-server "$BROKERS" \
  --consumer.config client.properties \
  --topic market.bars.dlq --from-beginning --max-messages 5
```

A message in the DLQ is a decoding or handler bug. Capture the envelope —
`event_id`, `correlation_id` and `causation_id` are on it (ADR-010) — and file
it. Do not hand-edit offsets to skip it; the DLQ already did that for you.

If a consumer is wedged with nothing in the DLQ, restart it:

```sh
kubectl -n quantos rollout restart deploy/signal-service
```

### D. Consumers flapping

Repeated rebalances pause consumption for **every** instance in the group, so a
flapping consumer is worse than a missing one.

Usual causes: an HPA scaling up and down repeatedly, or pods being OOMKilled.

```sh
kubectl -n quantos get events --sort-by=.lastTimestamp | grep -iE 'oom|kill|scal'
kubectl -n quantos get pods -l app.kubernetes.io/name=signal-service \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].restartCount}{"\n"}{end}'
```

The HPA's `scaleDown.stabilizationWindowSeconds` is 600 precisely to stop this.
If it is still flapping, the metric is noisy — widen the window rather than the
thresholds.

For OOMKills, raise the memory limit; `signal-service` holds
`features.max_history_bars` (500) of history per symbol, so its footprint scales
with universe size.

### E. Cannot reach the broker

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics | grep 'bus_errors_total'
```

Two things block MSK and both are silent:

```sh
# IAM: the role must hold ReadData on the topic and AlterGroup on the group.
aws iam get-role-policy --role-name quantos-prod-signal-service --policy-name msk

# NetworkPolicy: 9098 to the VPC CIDR.
kubectl -n quantos describe networkpolicy allow-egress-datastores
```

A prod overlay still carrying dev's `10.40.0.0/16` produces exactly this.

## Verify recovery

```
max(quantos_kafka_lag) < 1000
rate(quantos_bus_consumed_total[5m]) ≈ rate(quantos_bus_published_total[5m])
```

And check the SLI directly, since the lag metric can look fine while the burn
continues:

```
histogram_quantile(0.99,
  sum by (le) (rate(quantos_market_event_processing_duration_seconds_bucket[5m]))) < 0.1
```

If the backlog exceeded `max_event_age`, the dropped events are gone. They are
not replayable in any useful sense — replaying a 20-minute-old quote produces a
signal about a price that no longer exists, which is the thing `max_event_age`
is there to prevent.

## Rollback

Lag is rarely fixed by a rollback, but if it began with a deploy of
signal-service:

```sh
kubectl -n quantos rollout undo deploy/signal-service
```

Note the asymmetry: rolling the code back does **not** roll the committed
offsets back. The consumer group resumes where the new version left off. If the
new version was committing offsets without doing the work, the events between
are lost and the rollback does not recover them.
[`deploy-rollback.md`](deploy-rollback.md) covers that case.

If you repartitioned, that is not reversible: Kafka does not support reducing a
partition count. The topic has to be recreated, which discards its retained
history.

## Related

- [`market-data-outage.md`](market-data-outage.md) — when the producer is the problem
- [`deploy-rollback.md`](deploy-rollback.md) — offsets and rollbacks
- `docs/adr/ADR-002-kafka-architecture.md` — partitions, keys, delivery semantics
- `internal/bus/bus.go` — `TopicConfigs`, the authoritative specification

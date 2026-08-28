# Runbook — Market data outage

**Severity:** page
**Alert:** `MarketDataStale`
**Owner:** platform on-call

---

## Read this first

QuantOS converts a data-availability problem into **reduced output**, never into
degraded output. Governance rule G-5 and `market.staleness_soft` /
`staleness_hard` are the mechanism:

- Past `staleness_soft` (30 s): no *new* signal is created for that symbol.
- Past `staleness_hard` (2 m): the symbol is treated as offline, `market.stale`
  is published, and the risk engine returns `BLOCK` on it.

So the symptom is the same shape as a Postgres outage: fewer signals, no errors.
The difference is that this one is per-symbol, and the ingestion-continuity SLO
(99.5 % of market-hours minutes with ≥1 accepted tick per active symbol) is the
one it burns.

`docs/operations/slo.md` says this explicitly: the staleness gate "deliberately
converts a data-availability problem into *reduced output* rather than degraded
output, which shows up as an ingestion SLO miss, not a correctness one. That is
the intended shape."

## Symptoms

- `MarketDataStale` fires: `sum(quantos_market_data_stale) > 5`.
- `quantos_market_events_processed_total` flat or falling.
- `quantos_market_events_rejected_total` rising, with a `reason` label that says
  which validation failed.
- Signals stop for the affected tickers while others continue.
- `/healthz` on api-gateway returns 200 with the affected symbols absent from
  `/api/v1/market/status`.
- If the *whole* universe goes stale, the regime engine also stalls — it needs
  SPY and QQQ (`regime.benchmarks`).

## Diagnosis

```sh
# Which symbols, and how many?
kubectl -n quantos exec deploy/api-gateway -- \
  wget -qO- http://localhost:8080/metrics | grep '^quantos_market_data_stale' | grep ' 1$'

# Is it one symbol, one sector, or everything? The answer decides the branch.
kubectl -n quantos exec deploy/api-gateway -- \
  wget -qO- http://localhost:8080/metrics \
  | awk '/^quantos_market_data_stale/ && $2 == 1' | wc -l
```

```sh
# What is the producer doing?
kubectl -n quantos logs deploy/market-service --tail=100 | grep -iE 'stale|reject|circuit|provider'

# Is it publishing at all?
kubectl -n quantos exec deploy/market-service -- \
  wget -qO- http://localhost:8081/metrics \
  | grep -E '^quantos_(market_events_processed_total|market_events_rejected_total|bus_published_total)'
```

```sh
# Rejection reasons. internal/marketdata validates before publishing, so a
# rejected tick is a tick that failed a specific check - gap, spread, ordering.
kubectl -n quantos exec deploy/market-service -- \
  wget -qO- http://localhost:8081/metrics | grep 'market_events_rejected_total'
```

| Reason label | Meaning | Branch |
|---|---|---|
| — (nothing published at all) | producer stopped or cannot reach the bus | A |
| `circuit` in logs | provider circuit breaker open (`market.circuit_threshold`) | B |
| `gap` | price moved more than `market.max_gap_pct` between ticks | C |
| `spread` | spread wider than `market.max_spread_bps` | C |
| stale but publishing | consumers are behind, not the producer | D → [`kafka-lag.md`](kafka-lag.md) |

## Remediation

### A. The producer is not producing

```sh
kubectl -n quantos get pods -l app.kubernetes.io/name=market-service
kubectl -n quantos describe pod -l app.kubernetes.io/name=market-service | tail -30
```

market-service runs **one replica** by design: the simulated provider generates
its own tick stream from a seed, so a second instance would publish a second,
divergent stream rather than sharing the work. That means a single pod stuck in
`Pending` or `CrashLoopBackOff` stops all ingestion.

```sh
# Pending: no node in the platform pool. Check the node group.
kubectl get nodes -l quantos.io/pool=platform

# CrashLoop: almost always a rejected configuration. internal/config.Validate
# prints exactly what it rejected.
kubectl -n quantos logs deploy/market-service --previous | head -20
```

If the pod is healthy but silent, the bus is the next suspect:

```sh
kubectl -n quantos exec deploy/market-service -- \
  wget -qO- http://localhost:8081/metrics | grep 'quantos_bus_errors_total'
```

`op="publish"` errors mean it cannot reach MSK — see the IAM and NetworkPolicy
checks in [`kafka-lag.md`](kafka-lag.md).

### B. Provider circuit breaker open

`market.circuit_threshold` is 5 consecutive failures, `circuit_cooldown` 30 s.
The breaker is doing its job; opening it is how a flapping provider stops
producing a stream of bad prices.

```sh
kubectl -n quantos exec deploy/market-service -- \
  wget -qO- http://localhost:8081/metrics | grep '^quantos_circuit_state'
# 0 closed, 1 half-open, 2 open
```

It closes itself after a successful probe. If it flaps between 1 and 2 for more
than ten minutes, the upstream is genuinely degraded and the correct action is to
**leave it open** and accept reduced output. Forcing it closed produces signals
from prices you already know are unreliable.

With `market.provider: sim` — the default in every mode, including cluster —
there is no upstream and this branch cannot occur. If you see it with the
simulator configured, the process is the problem, not the provider.

### C. Validation rejecting good data

This is the branch that needs judgement.

```sh
# What was rejected, and what did it look like?
kubectl -n quantos logs deploy/market-service --tail=500 | grep -i reject | head -20
```

A genuine market event — a halt-and-reopen, an index rebalance, an earnings gap —
can legitimately exceed `max_gap_pct` (0.25) or `max_spread_bps` (500). The
validator rejecting it is correct: those thresholds exist because a 30 % gap is
far more often a bad tick than a real move.

**Do not widen the thresholds during an incident.** A threshold changed under
time pressure is a threshold nobody reviews afterwards, and every risk
assessment carries the config hash, so the change is permanently visible in the
provenance of everything issued after it.

If the rejections are genuinely wrong, that is a defect in the validator, and
the incident action is to record the ticks and file it.

### D. Stale but publishing

The producer is fine and consumers are behind. Go to
[`kafka-lag.md`](kafka-lag.md) — the staleness you are seeing is
`bus.max_event_age` (5 m) dropping aged events, which is the same safety
property from the other end.

## Verify recovery

```sh
# No symbol stale.
kubectl -n quantos exec deploy/api-gateway -- \
  wget -qO- http://localhost:8080/metrics | awk '/^quantos_market_data_stale/ && $2 == 1' | wc -l
# expect 0
```

```
rate(quantos_market_events_processed_total[5m]) > 0
rate(quantos_market_events_rejected_total[5m])  ≈ 0
increase(quantos_signals_generated_total[15m])  > 0
```

Signals suppressed during the outage are **not backfilled**. The staleness gate
declined to create them because the prices were not trustworthy at the time, and
creating them afterwards from prices that have since been corrected is exactly
the "degraded output" the design refuses.

## Rollback

If the outage began with a deployment of market-service:

```sh
kubectl -n quantos rollout undo deploy/market-service
kubectl -n quantos rollout status deploy/market-service --timeout=180s
```

market-service uses `strategy: Recreate`, so the rollback is a stop-then-start
and there will be a gap of a few seconds in ingestion. That is unavoidable and
preferable to two producers running at once.

Full procedure: [`deploy-rollback.md`](deploy-rollback.md).

## Related

- [`kafka-lag.md`](kafka-lag.md) — when the producer is fine and consumers are not
- [`incident-response.md`](incident-response.md)
- `internal/app/run.go` — `runIngest`, the staleness gate and `market.stale`
- `docs/operations/slo.md` — ingestion continuity, and why this is reduced output

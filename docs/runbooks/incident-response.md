# Runbook — Incident response

The entry point. If you were paged and do not know which runbook you need, start
here.

---

## The first thing to understand about this platform

**Most QuantOS failures present as silence, not as errors.**

Three separate safety mechanisms convert a problem into *less output* rather than
into *wrong output*:

| Mechanism | Trigger | Effect |
|---|---|---|
| `App.CanEmitSignals` (ADR-003) | Postgres unwritable | no signals emitted at all |
| Staleness gate (G-5) | data older than `staleness_hard` | that symbol goes offline |
| `bus.max_event_age` | consumer lag over 5 m | aged events recorded, not acted on |

All three are working as designed when they fire. None of them produces an error
a user sees. So "no signals for an hour" is a real incident with a green
dashboard, and the first question is always **which gate closed**, not what
crashed.

## Triage: four commands

```sh
# 1. Is anything unhealthy, and what does it say is wrong?
kubectl -n quantos get pods -l app.kubernetes.io/part-of=quantos
kubectl -n quantos exec deploy/api-gateway -- \
  wget -qO- http://localhost:8080/healthz | jq '.status, .degraded, .store'

# 2. Which gate is closed?
kubectl -n quantos exec deploy/api-gateway -- \
  wget -qO- http://localhost:8080/metrics \
  | grep -E '^quantos_(degraded_component|market_data_stale|kafka_lag|circuit_state)'

# 3. Is the platform producing anything?
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics \
  | grep -E '^quantos_(signals_generated_total|predictions_total|risk_decisions_total)'

# 4. Did anything change?
argocd app history quantos-prod | tail -5
kubectl -n quantos logs deploy/signal-service | grep -m1 config_hash
```

## Routing

| What you see | Runbook |
|---|---|
| `quantos_degraded_component{component="postgres"}` = 1 | [`postgres-unavailable.md`](postgres-unavailable.md) |
| `quantos_market_data_stale` > 5 | [`market-data-outage.md`](market-data-outage.md) |
| `quantos_kafka_lag` high | [`kafka-lag.md`](kafka-lag.md) |
| `quantos_model_drift{kind="overall"}` ≥ 3 | [`model-drift.md`](model-drift.md) |
| veto rate > 80 % | [`risk-veto-storm.md`](risk-veto-storm.md) |
| `quantos_llm_failures_total` rising | [`llm-provider-failure.md`](llm-provider-failure.md) |
| symptoms started at a deploy | [`deploy-rollback.md`](deploy-rollback.md) |
| API 5xx or latency SLO burning | below, "API degradation" |
| `DuplicateAlertEmitted` | below, "Zero-budget alerts" |

## Severity

Derived from `docs/operations/slo.md`, not from how loud the reporter is.

| Sev | Meaning | Examples | Response |
|---|---|---|---|
| **1** | Correctness compromised: the platform produced something it cannot explain, or something duplicated | `DuplicateAlertEmitted`; a signal with no provenance chain | page, all hands, postmortem required |
| **2** | The platform is not producing | provenance suppression; all symbols stale; API down | page, postmortem required |
| **3** | Degraded but correct | one symbol stale; lag under `max_event_age`; a latency SLO burning | ticket, business hours |
| **4** | Explanation quality only | LLM down; grounding rejections | ticket |

The distinction between 1 and 2 is the one that matters. Sev 2 is "we produced
nothing", which is the designed failure mode and is recoverable. Sev 1 is "we
produced something wrong", which is what the whole architecture exists to
prevent, and any occurrence is a defect rather than a spent error budget.

## Zero-budget alerts

Two SLIs have an error budget of zero. A single occurrence is a bug.

**`DuplicateAlertEmitted`** — the durable unique index caught a duplicate the
in-memory dedup missed. Delivery is idempotent on the alert's dedup key
(`bus.Idempotent`, `dedup_ttl` 48 h), so this firing means either the dedup key
is not unique for a case it should cover, or an alert was replayed after the TTL
expired.

```sh
kubectl -n quantos exec deploy/alert-service -- \
  wget -qO- http://localhost:8087/metrics | grep 'alerts_suppressed_total.*durable_dedup'
kubectl -n quantos logs deploy/alert-service --tail=500 | grep -i dedup
```

Capture the two alerts and their `dedup_key`, `event_id` and `correlation_id`
(ADR-010 puts all three on the envelope) and file a defect. The suppression
worked — nobody was paged twice — but the fact that it had to is the finding.

**Signal provenance completeness** — a signal whose chain does not resolve. Check
directly:

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/signals/<id>/provenance | jq .
```

A gap here means a signal exists that the platform cannot explain, which is a
Sev 1 by definition.

## API degradation

```sh
kubectl -n quantos exec deploy/api-gateway -- \
  wget -qO- http://localhost:8080/metrics \
  | grep -E '^quantos_(http_requests_total|http_in_flight_requests|rate_limited_total)'
```

```
# Which route, which status?
topk(5, sum by (route, status) (rate(quantos_http_requests_total{status=~"5.."}[5m])))

# The SLI itself: P99 < 300 ms for reads.
histogram_quantile(0.99,
  sum by (le, route) (rate(quantos_http_server_duration_seconds_bucket{method="GET"}[5m])))
```

| Pattern | Cause |
|---|---|
| `rate_limited_total` rising | a client over `http.rate_limit_rps` (50). Working as intended |
| in-flight climbing, CPU flat | waiting on Postgres. The read tail is provenance joins |
| 503 from `/readyz` | a gate is closed — go back to triage |
| 5xx on one route | a handler bug; the route label names it |

Scaling api-gateway helps only the first two cases, and the HPA already does it
on `quantos_http_in_flight_requests`.

## Communication

- **Sev 1 or 2:** open the incident channel immediately, post the triage output
  above verbatim, and name an incident lead before starting to fix anything.
- Say **which gate closed** in the first update. "Signal emission is suspended
  because the provenance store is unwritable" tells everyone what is and is not
  affected; "the platform is down" does not, and is usually wrong — ingest,
  features, predictions and risk assessments are almost always still running.
- Update every 30 minutes even when the update is "still investigating".
- Reiterate what is *not* affected. This is a paper-trading platform: no order
  reaches a venue in any failure mode, and `internal/paper` panics at start-up
  if `QUANTOS_ALLOW_REAL_MONEY` is set. That is worth saying out loud when
  someone senior joins the channel.

## Evidence to capture before you fix

Fixing destroys the evidence. Two minutes here saves the postmortem.

```sh
mkdir -p /tmp/incident && cd /tmp/incident

kubectl -n quantos get pods -o yaml > pods.yaml
kubectl -n quantos get events --sort-by=.lastTimestamp > events.txt
for s in api-gateway market-service signal-service risk-service alert-service; do
  kubectl -n quantos logs "deploy/$s" --tail=2000 > "$s.log" 2>&1
  kubectl -n quantos exec "deploy/$s" -- wget -qO- "http://localhost:$(
    kubectl -n quantos get svc "$s" -o jsonpath='{.spec.ports[0].port}')/metrics" \
    > "$s.metrics" 2>/dev/null
done

# The config hash links every decision made during the incident to the exact
# configuration that produced it.
kubectl -n quantos logs deploy/signal-service | grep -m1 config_hash > config_hash.txt
```

## Postmortem

Blameless, within five working days, for every Sev 1 and Sev 2.

Structure:

1. **What the user saw.** Usually "no signals between 14:02 and 14:47", not an
   error message.
2. **Which gate closed, and why it was right to close.** Nearly every incident
   here is a safety mechanism doing its job in response to an upstream problem.
   Say so — it keeps the mechanism from being blamed for the outage it prevented.
3. **The upstream cause.**
4. **Timeline**, with the config hash before and after if a deploy was involved.
5. **What was lost.** Suppressed signals are not backfilled and dropped events
   are not replayed, both deliberately. State the window.
6. **Detection.** How long between the gate closing and the page? Silence is
   hard to alert on, and a slow detection here is usually a missing alert rather
   than a slow responder.
7. **Actions**, each with an owner and a date.

Ask specifically: **could this have produced a wrong signal rather than no
signal?** If the honest answer is "possibly", the severity was a 1 and the
architecture has a gap worth an ADR.

## Related

- `docs/operations/slo.md` — the objectives every severity is derived from
- `docs/operations/deployment.md` — the three runtime modes
- `docs/adr/` — ADR-003, ADR-005, ADR-009 and ADR-010 explain most of the
  behaviour that looks surprising at 03:00

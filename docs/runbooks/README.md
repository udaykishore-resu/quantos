# QuantOS — Runbooks

One per realistic failure. Each has symptoms, diagnosis commands, remediation
and rollback.

**Start at [`incident-response.md`](incident-response.md)** if you were paged and
do not know which of these you need.

| Runbook | Alert | Severity |
|---|---|---|
| [`incident-response.md`](incident-response.md) | — | triage, severity, comms, postmortem |
| [`postgres-unavailable.md`](postgres-unavailable.md) | `SignalProvenanceSuppression` | page |
| [`market-data-outage.md`](market-data-outage.md) | `MarketDataStale` | page |
| [`kafka-lag.md`](kafka-lag.md) | `ConsumerLagHigh`, `MarketEventProcessingSLOFastBurn` | ticket → page |
| [`model-drift.md`](model-drift.md) | `ModelDriftSevere` | page at severe |
| [`risk-veto-storm.md`](risk-veto-storm.md) | `VetoRateExcessive` | ticket |
| [`llm-provider-failure.md`](llm-provider-failure.md) | `LLMProviderDegraded`, `ExplanationsAlwaysRejected` | ticket, never a page |
| [`deploy-rollback.md`](deploy-rollback.md) | — | follows the incident |

## The thing that surprises everyone the first time

Most QuantOS failures present as **silence, not errors**. Three safety
mechanisms convert a problem into less output rather than into wrong output:

- `App.CanEmitSignals` — Postgres unwritable, so no signals at all (ADR-003)
- the staleness gate — data too old, so that symbol goes offline (rule G-5)
- `bus.max_event_age` — consumer lag over 5 minutes, so aged events are recorded
  and not acted on

All three are working correctly when they fire. So "no signals for an hour" is a
real incident with a green dashboard, and the first question is always *which
gate closed*, not what crashed.

## Not in scope for any of these

No failure mode of this platform can route a real-money order. `internal/paper`
panics at start-up if `QUANTOS_ALLOW_REAL_MONEY` is set, and the infrastructure
holds no credential, permission or network route to any venue. Say so early when
someone senior joins an incident channel.

## Related

- `docs/operations/slo.md` — the objectives every alert is derived from
- `docs/operations/deployment.md` — the three runtime modes end to end
- `deploy/prometheus/rules/slo.yml` — the compose-mode alert definitions
- `infra/kubernetes/base/prometheusrule.yaml` — the same rules for the cluster

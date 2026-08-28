# Runbook — Deploy rollback

**Severity:** follows the incident that prompted it
**Owner:** whoever deployed, plus platform on-call

---

## Read this first

Rolling back the images is easy. The three things that do **not** roll back with
them are what this runbook is actually about:

1. **Applied schema migrations.** `deploy/sql/` is additive and has no down
   migration. A rolled-back binary meets a forward schema.
2. **Committed consumer offsets.** The group resumes where the *new* version
   left off. Events it consumed and mishandled are not re-delivered.
3. **Persisted decisions.** Signals, risk assessments and paper orders written
   by the bad version are in Postgres and stay there. They carry the bad
   version's config hash, which is how you find them.

Decide about those three before you type `rollout undo`, not after.

## When to roll back

Roll back when the new version is worse than the old one and you do not know why
yet. Do not roll back to "see if it helps" — a rollback is a state change that
makes the next diagnosis harder.

| Signal | Action |
|---|---|
| CrashLoopBackOff on the new version | roll back |
| `/readyz` 503 across the fleet after a deploy | diagnose first — it may be [`postgres-unavailable.md`](postgres-unavailable.md), which a rollback will not fix |
| Latency SLO burning since the deploy | roll back |
| Veto rate up since the deploy | check the config hash first — [`risk-veto-storm.md`](risk-veto-storm.md) |
| Drift alert since the deploy | roll the **artifact** back, not the image — [`model-drift.md`](model-drift.md) |
| One service degraded | roll back that service only |

## Diagnosis

```sh
# What changed, and when?
kubectl -n quantos rollout history deploy/signal-service
argocd app history quantos-prod
```

```sh
# Is the running config what you think it is? The hash is stamped on every
# assessment and score, so this is also how you find affected records later.
kubectl -n quantos logs deploy/signal-service | grep -m1 config_hash
```

```sh
# Did the deploy actually complete, or is it half-rolled?
kubectl -n quantos get deploy -o wide
kubectl -n quantos get pods -l app.kubernetes.io/part-of=quantos \
  -o custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image,READY:.status.containerStatuses[0].ready
```

A half-rolled deployment is the most common state to find, because
`maxUnavailable: 0` means a failing new pod never replaces a healthy old one —
the rollout stalls with both versions running rather than taking the service
down. That is the intended behaviour and it buys you time.

## Remediation

### Rollback via ArgoCD (prod, and dev when Argo owns it)

```sh
# 1. What was it before?
argocd app history quantos-prod

# 2. Roll to a specific revision id from that list.
argocd app rollback quantos-prod <REVISION_ID>

# 3. Wait for health, not just for the sync to apply. A Deployment whose pods
#    are CrashLoopBackOff is Synced and not Healthy.
argocd app wait quantos-prod --health --timeout 900
```

Prod's Application has no `automated` block, so a rollback stays rolled back
until someone syncs forward again. Dev has `selfHeal: true` and will revert your
rollback within the minute — disable it first:

```sh
argocd app set quantos-dev --sync-policy none
```

### Rollback via kubectl (faster, and correct when Argo is the problem)

```sh
kubectl -n quantos rollout undo deploy/signal-service
kubectl -n quantos rollout status deploy/signal-service --timeout=300s
```

To a specific revision:

```sh
kubectl -n quantos rollout history deploy/signal-service --revision=3
kubectl -n quantos rollout undo deploy/signal-service --to-revision=3
```

`revisionHistoryLimit` is 3, so only the last three are available. If you need
further back, deploy the tag explicitly:

```sh
kubectl -n quantos set image deploy/signal-service \
  signal-service=<registry>/quantos/signal-service:v1.4.1
```

Argo will report OutOfSync and, in dev, revert it. That is a feature: an
out-of-band change should not survive quietly.

### Rollback via Helm

```sh
helm -n quantos history quantos
helm -n quantos rollback quantos <REVISION>
helm -n quantos status quantos
```

Helm rolls the ConfigMap back with the Deployments, which `kubectl rollout undo`
does not — the deployments carry a `checksum/config` annotation, so a Helm
rollback restarts pods when the config changes and a kubectl rollback leaves the
new ConfigMap in place under the old image.

### The singletons need care

market, risk, portfolio, news and evaluation use `strategy: Recreate`, because
two instances would double-publish. A rollback of any of them is a
stop-then-start with a real gap:

```sh
kubectl -n quantos rollout undo deploy/market-service
# ~15-30s with no ingestion. The staleness gate handles it correctly - see
# market-data-outage.md - and no bad signal is produced during the gap.
```

Roll them back one at a time and confirm each is ready before the next.

### Migrations

Check whether the bad release applied one:

```sh
git diff "v1.4.1..v1.4.2" -- deploy/sql/
```

If it did **not**, the rollback is complete and you can skip the rest.

If it did: the schemas are additive (`CREATE TABLE IF NOT EXISTS`, added
columns), so the old binary generally tolerates the new schema — it ignores
columns it does not know about. That is why the migrations are written that way.

The exception is a `NOT NULL` column without a default, which the old binary's
inserts will not populate. That combination should not pass review, and if one
reached production the choices are to add a default forward:

```sql
ALTER TABLE signals ALTER COLUMN new_field SET DEFAULT '';
```

or to restore, which loses everything written since. Prefer the first.

**Never write a down migration during an incident.** Dropping a column drops the
provenance stored in it, and ADR-003 makes that provenance a product
requirement.

### Offsets

Rolling the code back does not roll the offsets back. If the bad version was
committing offsets without doing the work — a handler that swallowed an error,
say — those events are gone from the consumer's point of view.

To reprocess, reset the group. This is disruptive and is only correct when you
know the events were dropped rather than mishandled:

```sh
# 1. Stop the consumers. A reset against a live group is rejected.
kubectl -n quantos scale deploy/signal-service --replicas=0

# 2. Reset to a timestamp, not to earliest. Earliest replays up to a year of
#    signal.generated and re-derives a year of state.
kafka-consumer-groups.sh --bootstrap-server "$BROKERS" \
  --command-config client.properties \
  --group quantos.engine --topic market.bars \
  --reset-offsets --to-datetime 2026-08-28T09:00:00.000 --execute

# 3. Restart.
kubectl -n quantos scale deploy/signal-service --replicas=3
```

Reprocessed events older than `bus.max_event_age` (5 m) are recorded and not
acted on, so a reset further back than five minutes produces records without
producing signals. That is usually what you want — you get the audit trail
without a burst of signals about prices from an hour ago.

Idempotency protects the rest: `bus.Idempotent` dedups on `event_id` with a
48-hour TTL, and the durable unique indexes on alerts and paper orders catch
anything the in-memory layer misses. A duplicate reaching the durable index
fires `DuplicateAlertEmitted`, which is a zero-budget alert — see
[`incident-response.md`](incident-response.md).

### Decisions written by the bad version

They stay. Find them by config hash:

```sql
SELECT id, ticker, created_at, config_hash
FROM signals
WHERE config_hash = '<hash from the bad version''s startup log>'
ORDER BY created_at;
```

Do not delete them. They are the record of what the platform decided and why,
and deleting them removes the evidence of the incident. Invalidate them instead
if they are still live:

```sql
-- Or via the sweeper, which will expire them at signals.default_ttl (4h).
SELECT id FROM signals WHERE status = 'ACTIVE' AND config_hash = '<hash>';
```

## Verify recovery

```sh
kubectl -n quantos get pods -l app.kubernetes.io/part-of=quantos
kubectl -n quantos get deploy -o custom-columns=NAME:.metadata.name,IMAGE:.spec.containers[0].image
QUANTOS_API=http://127.0.0.1:18080 ./scripts/smoke-test.sh
```

```
increase(quantos_signals_generated_total[15m]) > 0
max(quantos_kafka_lag) < 1000
sum(quantos_degraded_component) == 0
```

Then confirm the config hash in the logs matches the version you rolled back to.
A rollback that left the new ConfigMap in place is a rollback that did not
happen.

## Afterwards

- Note the config hashes of both versions in the incident record. They are the
  only durable link between "what we deployed" and "what it decided".
- If the release passed CI and still failed, the gap is in CI. `make demo` runs
  ten end-to-end scenarios in seconds; if the failure mode is expressible there,
  add it.
- Postmortem: [`incident-response.md`](incident-response.md).

## Related

- [`incident-response.md`](incident-response.md)
- [`postgres-unavailable.md`](postgres-unavailable.md) — a 503 after a deploy is often this
- [`kafka-lag.md`](kafka-lag.md) — offsets and reprocessing
- `docs/operations/deployment.md` — the three runtime modes

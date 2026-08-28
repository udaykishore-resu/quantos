# Runbook — PostgreSQL unavailable

**Severity:** page
**Alert:** `SignalProvenanceSuppression`
**Owner:** platform on-call

---

## Read this first

When PostgreSQL cannot be written, QuantOS **stops emitting signals**. It does
not degrade, buffer, or emit-and-reconcile. It goes quiet.

That is a deliberate design decision, not a bug and not a bug's side effect:

```go
// internal/app/app.go
func (a *App) CanEmitSignals() bool {
    if a.SQL == nil { return true }   // in-memory store is always writable
    return a.SQL.CanEmitSignals()     // pg != nil && pgOK
}
```

and at the emission point in `internal/app/run.go`:

```go
if !a.CanEmitSignals() {
    a.Log.Error("suppressing signal: provenance store unavailable", ...)
    a.Signals.Invalidate(res.Signal.ID, domain.InvalidRiskVeto,
        "provenance store unavailable at emission time", a.Clock.Now())
}
```

ADR-003 makes "reproduce this decision" a product requirement. A signal whose
provenance was never written is a signal nobody can later explain, so the
platform declines to make one. **The silence is the safety property working.**

The consequence for you: the user-visible symptom is *absence*. Nobody gets an
error. The dashboard just stops filling up. Do not spend the first ten minutes
looking for a crash.

## Symptoms

- `SignalProvenanceSuppression` fires (`quantos_degraded_component{component="postgres"}`).
- Signal generation drops to zero: `rate(quantos_signals_generated_total[5m]) == 0`.
- `/readyz` returns **503** with
  `"provenance store is unavailable; signal emission is suspended"`.
- `/healthz` still returns **200**, with `"status":"degraded"` and
  `"degraded":["postgres"]`. This is correct — the process is fine.
- Pods leave the Service endpoints but are **not restarted**: the liveness probe
  is on `/healthz`, which is still healthy.
- Ingest, features, predictions and risk assessments keep flowing. Only
  emission is gated.

## Diagnosis

Work outward from the pod.

```sh
# 1. What does the platform itself say is wrong?
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/healthz | jq '.degraded, .store'

# 2. Is it every pod or one pod?
for s in signal-service api-gateway portfolio-service; do
  echo "--- $s"
  kubectl -n quantos exec "deploy/$s" -- wget -qO- http://localhost:8080/healthz \
    2>/dev/null | jq -r '.degraded // []'
done
```

One pod degraded means a connection pool problem. All pods degraded means the
database.

```sh
# 3. Is RDS up at all?
aws rds describe-db-instances --db-instance-identifier quantos-prod-postgres \
  --query 'DBInstances[0].{status:DBInstanceStatus,az:AvailabilityZone,multiAZ:MultiAZ}'

# 4. Recent events — failover, storage-full, maintenance
aws rds describe-events --source-identifier quantos-prod-postgres \
  --source-type db-instance --duration 60

# 5. Connections and storage, the two that fill up quietly
aws cloudwatch get-metric-statistics --namespace AWS/RDS \
  --metric-name DatabaseConnections --statistics Maximum --period 60 \
  --start-time "$(date -u -d '30 min ago' +%FT%TZ)" --end-time "$(date -u +%FT%TZ)" \
  --dimensions Name=DBInstanceIdentifier,Value=quantos-prod-postgres

aws cloudwatch get-metric-statistics --namespace AWS/RDS \
  --metric-name FreeStorageSpace --statistics Minimum --period 300 \
  --start-time "$(date -u -d '2 hours ago' +%FT%TZ)" --end-time "$(date -u +%FT%TZ)" \
  --dimensions Name=DBInstanceIdentifier,Value=quantos-prod-postgres
```

```sh
# 6. Can anything reach it from inside the cluster?
kubectl -n quantos run pgcheck --rm -it --restart=Never \
  --image=postgres:16-alpine --command -- \
  psql "$(kubectl -n quantos get secret quantos-store \
          -o jsonpath='{.data.QUANTOS_POSTGRES_DSN}' | base64 -d)" -c 'SELECT 1'
```

The four causes, in the order they actually occur:

| Cause | Tell | Section |
|---|---|---|
| Multi-AZ failover in progress | RDS event `Multi-AZ instance failover started` | A |
| Connection pool exhausted | `DatabaseConnections` at `max_connections`; app logs `too many clients` | B |
| Storage full | `FreeStorageSpace` near zero; writes fail, reads succeed | C |
| Security group / network | `pgcheck` times out rather than erroring | D |

## Remediation

### A. Failover in progress

Do nothing for 60–120 seconds. RDS multi-AZ failover is 60–120 s and the DNS
name follows the standby. `internal/store` re-probes on its health interval and
flips `pgOK` back on its own; emission resumes without a restart.

If pods are still degraded after **three minutes**, the connections are stale
rather than the database being down:

```sh
kubectl -n quantos rollout restart deploy/signal-service deploy/portfolio-service
```

### B. Connection pool exhausted

`store.max_open_conns` is 25 per instance. Nine services at three replicas is a
theoretical 675 connections against a `db.r6g.large` default of ~800 — the
margin is thinner than it looks, and an HPA scale-up can cross it.

```sh
# Who is holding them?
psql "$DSN" -c "
  SELECT usename, application_name, state, count(*)
  FROM pg_stat_activity GROUP BY 1,2,3 ORDER BY 4 DESC;"

# Idle-in-transaction is the usual culprit: a connection held across a network
# blip that never closed.
psql "$DSN" -c "
  SELECT pg_terminate_backend(pid) FROM pg_stat_activity
  WHERE state = 'idle in transaction' AND state_change < now() - interval '5 min';"
```

Then reduce the fan-in rather than raising the ceiling:

```sh
kubectl -n quantos scale deploy/signal-service --replicas=2
```

Follow up by lowering `store.max_open_conns` in the ConfigMap. Raising
`max_connections` on the instance trades one limit for a memory limit and
usually moves the outage rather than removing it.

### C. Storage full

Storage autoscaling is on (`max_allocated_storage`), but it takes minutes and
will not act if the ceiling is already reached.

```sh
aws rds modify-db-instance --db-instance-identifier quantos-prod-postgres \
  --max-allocated-storage 2000 --apply-immediately
```

Then find what grew. Provenance rows accumulate by design; `store.retention` is
2160 h (90 days) and the sweeper should be trimming.

```sh
psql "$DSN" -c "
  SELECT relname, pg_size_pretty(pg_total_relation_size(relid)) AS size
  FROM pg_catalog.pg_statio_user_tables ORDER BY pg_total_relation_size(relid) DESC LIMIT 10;"
```

If a time-series table is the largest, it is in the wrong engine: quotes, bars,
feature snapshots and predictions belong in ClickHouse (ADR-003). That is a
schema bug, not a capacity problem.

### D. Network

```sh
# Does the RDS security group still allow the node group?
aws ec2 describe-security-groups --group-ids "$(aws rds describe-db-instances \
  --db-instance-identifier quantos-prod-postgres \
  --query 'DBInstances[0].VpcSecurityGroups[0].VpcSecurityGroupId' --output text)" \
  --query 'SecurityGroups[0].IpPermissions'
```

The rule is a security-group reference, not a CIDR
(`infra/terraform/modules/data/main.tf`). A node group replaced outside
Terraform gets a new security group and the reference no longer matches — the
fix is `terraform apply`, not a hand-added rule that the next apply removes.

Also check the NetworkPolicy, which is the other thing that silently blocks
5432:

```sh
kubectl -n quantos describe networkpolicy allow-egress-datastores
```

The `ipBlock` CIDR must be the VPC's. A prod overlay still carrying dev's
`10.40.0.0/16` produces exactly this symptom.

## Verify recovery

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/readyz            # 200, {"status":"ready"}

kubectl -n quantos get endpoints signal-service     # pods back in the Service
```

Then confirm emission actually resumed — readiness is necessary, not sufficient:

```
increase(quantos_signals_generated_total[10m]) > 0
quantos_degraded_component{component="postgres"} == 0
```

Signals suppressed during the outage are **not replayed**. They were invalidated
with `RISK_VETO` and the note `provenance store unavailable at emission time`,
and they stay that way — re-emitting them later would attach a decision to
market conditions that no longer hold.

## Rollback

There is nothing to roll back in the platform: it did not change and it did not
fail. If the outage began with a deployment, the deployment is the suspect and
the sequence is in [`deploy-rollback.md`](deploy-rollback.md).

If the database itself must be restored:

```sh
# 1. Stop the writers first. Restoring underneath live connections gives you a
#    database and a set of processes that disagree about what exists.
kubectl -n quantos scale deploy --replicas=0 \
  signal-service portfolio-service evaluation-service api-gateway

# 2. Point-in-time restore to a NEW instance. Never restore in place: the
#    original is the only evidence of what went wrong.
aws rds restore-db-instance-to-point-in-time \
  --source-db-instance-identifier quantos-prod-postgres \
  --target-db-instance-identifier quantos-prod-postgres-restored \
  --restore-time "$(date -u -d '20 min ago' +%FT%TZ)" \
  --db-subnet-group-name quantos-prod-postgres \
  --no-publicly-accessible

# 3. Repoint the secret, then bring the writers back.
aws secretsmanager put-secret-value --secret-id quantos-prod/postgres \
  --secret-string "$(...)"     # dsn field -> the restored endpoint
kubectl -n quantos delete secret quantos-store   # External Secrets re-creates it
kubectl -n quantos scale deploy --replicas=3 signal-service api-gateway
kubectl -n quantos scale deploy --replicas=1 portfolio-service evaluation-service
```

Restoring to a point in time loses the provenance written after it. Those
signals become unexplainable, which is the exact thing this whole design exists
to prevent — record the restore point in the incident, and treat any signal
issued after it as unverifiable.

## Related

- [`incident-response.md`](incident-response.md) — severity, comms, postmortem
- [`deploy-rollback.md`](deploy-rollback.md) — if a deploy started this
- `docs/adr/ADR-003-postgres-vs-clickhouse.md` — why Postgres holds provenance
- `internal/store/composite.go` — `SQL.CanEmitSignals`, `SQL.Degraded`

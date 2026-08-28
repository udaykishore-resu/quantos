# QuantOS — Deployment

QuantOS runs in three topologies. They share every engine; the only difference is
what backs the bus and the store, and how many processes the roles are spread
across. `mode` in `config/quantos.yaml` selects one, and
`internal/config.Validate` rejects anything else.

| | `embedded` | `compose` | `cluster` |
|---|---|---|---|
| Processes | 1 | 9 + infrastructure | 9, on Kubernetes |
| Bus | in-memory | WAL on disk | Kafka (MSK) |
| Store | in-memory | PostgreSQL, ClickHouse, Redis | RDS, ClickHouse, ElastiCache |
| Secrets | none | compose environment | AWS Secrets Manager |
| Survives restart | no | yes | yes |
| Start-up | seconds | ~1 minute | ~25 minutes from empty |
| Cost | zero | zero | ~$860/mo dev, ~$1,575/mo prod |
| For | demo, workshop, CI | development, integration | the real thing |

The engines are byte-identical across all three. That is the point of the split:
a bug reproduced in `embedded` is the same bug that occurs in `cluster`, and a
decision made in `embedded` is derivable from the same provenance.

---

## `embedded`

Everything in one process: the in-memory bus, the in-memory store, and all six
roles (ingest, engine, news, evaluation, api, sweeper).

```sh
make demo    # ten scenarios end to end, no infrastructure, seconds
make run     # every role in one process, serving on :8080
```

Nothing survives a restart. `store.driver: memory` means `App.CanEmitSignals`
returns `true` unconditionally — the in-memory store is always writable — so the
provenance gate that dominates cluster operations never fires here. That is
worth knowing when a behaviour reproduces in `cluster` and not in `embedded`.

`auth.dev_users` supplies two local principals (`demo`/`demo`,
`operator`/`operator`). They exist in `embedded` and `compose` only.

To run this shape on Kubernetes — a workshop cluster, a CI environment:

```sh
helm upgrade --install quantos infra/helm/quantos \
  -n quantos --create-namespace --set mode=embedded
```

The chart renders a single Deployment with `strategy: Recreate`: two instances of
an in-memory platform are two divergent worlds, not one shared one.

---

## `compose`

The nine services split out, against real backing stores on one machine. This is
`deploy/docker-compose.yml`, and it is the shape most development happens in.

```sh
scripts/bootstrap-dev.sh
```

That brings the stack up, waits for the four stores to report healthy, applies
both schemas, creates the topic set, and waits for all nine services to report
ready. `make dev` does the first and third of those; the script exists because
the other two are what make the stack answer questions rather than just be up.

| | |
|---|---|
| API | http://localhost:8080 |
| Grafana | http://localhost:3001 (admin / admin) |
| Prometheus | http://localhost:9091 |
| Jaeger | http://localhost:16686 |
| Services | 8081–8088, in the order in `deploy/prometheus/prometheus.yml` |

Two things differ from a laptop-shaped simplification:

**The bus is the WAL driver, not Kafka.** `config/quantos.compose.yaml` sets
`bus.driver: wal`, which gives durability, replay and consumer groups on one node
with no broker. Kafka is in the compose file so the split topology *can* be
exercised; point `bus.driver` at `kafka` when you want that.

**The store is real.** `store.driver: sql` against PostgreSQL, ClickHouse and
Redis, so `App.CanEmitSignals` behaves as it does in production — stop Postgres
and signal emission stops, which is the single most useful thing this mode
reproduces.

```sh
# The provenance gate, live:
docker compose -f deploy/docker-compose.yml stop postgres
curl -s localhost:8080/readyz | jq .
# 503, "provenance store is unavailable; signal emission is suspended"
docker compose -f deploy/docker-compose.yml start postgres
# resumes on its own within one health interval
```

Schemas: `scripts/db-migrate.sh` (or `make migrate`). Both are idempotent.

To run this shape on Kubernetes against in-cluster dependencies you supply:

```sh
helm upgrade --install quantos infra/helm/quantos -n quantos \
  --set mode=compose \
  --set backing.redisAddr=redis:6379 \
  --set backing.kafkaBrokers=kafka:9092
```

---

## `cluster`

The nine services on EKS against managed AWS backing stores. This is what
`infra/terraform` provisions and `infra/kubernetes` and `infra/helm` deploy.

`config.Validate` refuses `cluster` with `auth.enabled: false`, so there is no
way to run this mode without authentication.

### 1. Infrastructure

```sh
cd infra/terraform/environments/prod

terraform init \
  -backend-config=bucket=quantos-tfstate-<account-id> \
  -backend-config=key=prod/terraform.tfstate \
  -backend-config=region=us-east-1 \
  -backend-config=dynamodb_table=quantos-tfstate-lock \
  -backend-config=encrypt=true

terraform plan -var-file=prod.tfvars -out=prod.plan
terraform apply prod.plan
```

Roughly 40 minutes from empty. The long poles are the EKS control plane (~10 m),
RDS multi-AZ (~15 m) and MSK provisioned brokers (~25 m).

Read `infra/terraform/README.md` before the first apply. It lists what is
deliberately **not** automated — the state backend, the LLM API key value,
ClickHouse, the topics, the schema, DNS and certificates — and why each is a
decision rather than an omission.

### 2. Secrets

Terraform generates the database password, the Redis auth token and the JWT
signing key, and writes them to Secrets Manager. It creates the LLM key's
*container* and never its value:

```sh
aws secretsmanager put-secret-value \
  --secret-id quantos-prod/llm-api-key \
  --secret-string '{"api_key":"<paste>"}'
```

A key in a variable default is a key in git, and a key in a plan output is a key
in every CI log that renders a plan.

### 3. Cluster add-ons

Not installed by this repository, each with its own lifecycle:

- External Secrets Operator (namespace `external-secrets`)
- AWS Load Balancer Controller
- Prometheus Operator (namespace `monitoring`)
- prometheus-adapter — without it the HPAs cannot see `quantos_kafka_lag` and
  silently fall back to their CPU target
- OpenTelemetry Collector (namespace `observability`)
- **A CNI that enforces NetworkPolicy.** The EKS VPC CNI does not by default.
  Without one, every policy in `infra/kubernetes/base/networkpolicy.yaml`
  renders and does nothing, and the cluster is open while appearing locked down.

### 4. Schema

```sh
QUANTOS_POSTGRES_DSN="$(aws secretsmanager get-secret-value \
  --secret-id quantos-prod/postgres --query SecretString --output text | jq -r .dsn)" \
QUANTOS_CLICKHOUSE_URL=http://clickhouse.quantos.svc.cluster.local:8123 \
  scripts/db-migrate.sh --target cluster
```

Postgres first, always. A platform with a ClickHouse schema and no Postgres
schema starts, reports healthy, and emits nothing.

### 5. Workloads

Either kustomize:

```sh
kustomize build infra/kubernetes/overlays/prod | kubectl apply -f -
```

or Helm:

```sh
helm upgrade --install quantos infra/helm/quantos -n quantos \
  -f infra/helm/quantos/values-prod.yaml --set image.tag=v1.4.2
```

or GitOps, which is what `.github/workflows/cd.yml` drives:

```sh
kubectl apply -f infra/argocd/project.yaml
kubectl apply -f infra/argocd/application-prod.yaml
```

Never two of the three into one namespace. Both the manifests and the chart are
kept in step deliberately, and either alone is complete.

Before the first apply, replace the placeholders. The base ships with
`000000000000` as the IRSA account id and `REPLACE-ME` in the certificate ARN so
that an unpatched overlay fails loudly rather than producing pods that cannot
assume a role. `cd.yml`'s `render` job checks exactly this.

```sh
terraform output -json service_role_arns   # -> ServiceAccount annotations
terraform output -json s3_buckets          # -> MODEL_BUCKET
terraform output -raw kafka_bootstrap_brokers
```

### 6. Verify

```sh
kubectl -n quantos get pods
kubectl -n quantos logs job/quantos-topics          # topics created?
kubectl -n quantos get externalsecret               # secrets resolved?

kubectl -n quantos port-forward svc/api-gateway 18080:8080 &
QUANTOS_TOKEN=<token> QUANTOS_API=http://127.0.0.1:18080 scripts/smoke-test.sh
```

The chart's `NOTES.txt` walks the same checks in the order they fail in.

---

## What each role runs where

The role split is in `internal/app/roles.go` and `run.go`; each binary names its
roles in `cli.Spec` and nothing else differs (ADR-011).

| Service | Port | Roles | Replicas | Why |
|---|---|---|---|---|
| api-gateway | 8080 | `api` | scales | HTTP edge; no decision logic |
| market-service | 8081 | `ingest` | **1** | the sim provider generates its own stream from a seed; a second instance publishes a divergent one |
| signal-service | 8082 | `engine` | scales to 12 | the decision path; 12 = `market.bars` partitions |
| risk-service | 8083 | `sweeper` | **1** | a timer over shared state |
| portfolio-service | 8084 | — (`RunPortfolio`) | **1** | two markers write two snapshots per interval |
| news-service | 8085 | `news` | **1** | a poller; two pollers pay the LLM twice for one headline |
| evaluation-service | 8086 | `evaluation` | **1** | a timer; on the batch pool |
| alert-service | 8087 | — (`RunAlertDelivery`) | scales to 6 | delivery is idempotent on the dedup key |
| backtest-service | 8088 | — (`RunBacktestWorker`) | scales | on the batch pool, away from the live path |

Ports are identical in all three modes, so a port in a runbook is right whichever
one is running. `/metrics` is on the same port as the API:
`internal/api.Mount` registers it on the main router and
`telemetry.metrics_addr` starts no second listener.

---

## Probes

Every binary serves both, because a service you cannot probe is a service you
cannot operate.

**`/healthz`** — the process is up. Returns **200 even when degraded**, with the
degraded subsystems listed in the body. A degraded service is not killed:
degradation is a state this platform is designed to run in.

**`/readyz`** — this instance can serve its purpose. Returns **503** when the
universe is empty, no strategies are loaded, or `App.CanEmitSignals` is false.

That last case is the one to understand before you meet it. When Postgres cannot
be written, the platform stops emitting signals rather than emitting one it
could not later explain (ADR-003), and the readiness failure is what makes the
suppression visible instead of silent. It is not a crash and it recovers on its
own. [`docs/runbooks/postgres-unavailable.md`](../runbooks/postgres-unavailable.md).

---

## Configuration precedence

Lowest to highest:

1. `internal/config.Default()` — complete; the platform runs with no file at all
2. the YAML file named by `QUANTOS_CONFIG`
3. `QUANTOS_*` environment variables (`internal/config.applyEnv`)
4. command-line flags (`-addr`, `-mode`)

The effective configuration is hashed at load and stamped on every risk
assessment, score and signal, so a historical decision can be re-derived after
the values change. Secrets are never part of it: only the *name* of the
environment variable that holds one is stored.

```sh
# What is this binary actually going to use?
./bin/signal-service -print-config
kubectl -n quantos logs deploy/signal-service | grep -m1 config_hash
```

---

## What no mode does

QuantOS is an educational research and paper-trading platform.

`internal/paper` panics at start-up if `QUANTOS_ALLOW_REAL_MONEY` is set, with
the message that this build contains no broker integration. There is no venue
connection, no order-routing credential, and no IAM action anywhere in
`infra/terraform` that resembles one — the workload permissions boundary in
`modules/iam` enumerates the service categories the platform uses and nothing
else. `security.yml` fails the build if a manifest, chart, script or workflow
sets that variable.

---

## Related

- `docs/operations/slo.md` — the objectives the alerts are derived from
- `docs/runbooks/` — one per realistic failure
- `infra/terraform/README.md` — apply order, cost, and what is not automated
- `infra/kubernetes/README.md` — the manifests and their placeholders
- `docs/adr/` — ADR-002, ADR-003, ADR-010 and ADR-011 explain the shape

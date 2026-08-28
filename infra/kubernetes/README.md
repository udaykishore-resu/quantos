# QuantOS — Kubernetes manifests

Plain manifests for the `cluster` runtime mode, as a kustomize base with a dev
and a prod overlay. `infra/helm/quantos` parameterises the same thing for people
who prefer a chart; the two are kept in step deliberately and either can be
deployed on its own — never both into the same namespace.

```
base/           the whole deployment with placeholder values
overlays/dev/   one replica of everything, debug logs, mock LLM
overlays/prod/  three engine replicas, priority classes, real hostname
```

```sh
kustomize build overlays/dev  | kubectl apply -f -
kustomize build overlays/prod | kubectl apply -f -
```

## What the base contains

| File | What it is for |
|---|---|
| `namespace.yaml` | Namespace with Pod Security `restricted` enforced |
| `serviceaccounts.yaml` | One SA per service, annotated for IRSA, plus the topics Job and External Secrets |
| `configmap.yaml` | `quantos.cluster.yaml` override sheet, and the non-secret `QUANTOS_*` environment |
| `externalsecrets.yaml` | SecretStore + ExternalSecrets. References only; a Sealed Secrets equivalent is documented at the bottom |
| `services.yaml` | ClusterIP per service, ports identical to `deploy/docker-compose.yml` |
| `deploy-*.yaml` | Nine Deployments |
| `pdb.yaml` | PodDisruptionBudgets |
| `hpa.yaml` | Three HPAs; the other six services are singletons |
| `networkpolicy.yaml` | Default-deny plus explicit allows |
| `ingress.yaml` | ALB for api-gateway, TLS, `/api/v1` only |
| `servicemonitor.yaml` `prometheusrule.yaml` | Prometheus Operator scrape config and the SLO rules from `deploy/prometheus/rules/slo.yml` |
| `topics-job.yaml` | Pre-sync Job creating the ADR-002 topic set on MSK |

## Decisions worth knowing before you change something

**Ports are per service, not uniform.** api-gateway on 8080, the rest on
8081–8088, in the same order as `deploy/docker-compose.yml` and
`deploy/prometheus/prometheus.yml`. Making them all 8080 in the cluster would
mean a port in a runbook is right in one mode and wrong in another.

**`/metrics` is on the service port.** `internal/api.Mount` registers it on the
main router and `telemetry.metrics_addr` starts no second listener, so there is
no separate metrics port. The Ingress therefore routes `/api/v1` and not `/`,
because a `/` prefix would publish the metrics endpoint to the internet.

**Five of the nine are singletons.** market, risk, portfolio, news and
evaluation are timer-driven or sole producers; a second replica would not share
work, it would duplicate events. They use `strategy: Recreate` and get a
`maxUnavailable: 1` budget rather than `minAvailable: 1`, which would block
every node drain forever on a one-replica Deployment.

**The HPAs are keyed on lag, not only CPU.** `docs/operations/slo.md` is
explicit that scaling is by partition count and the HPA targets consumer lag.
`maxReplicas` on signal-service is 12 because `market.bars` has 12 partitions
(ADR-002) and the thirteenth replica would idle in the consumer group.

**Readiness is load-bearing.** `/readyz` returns 503 when the universe is empty,
no strategies are loaded, or `App.CanEmitSignals` is false. That last case is
the platform suspending signal emission because it cannot persist provenance
(ADR-003), and taking the pod out of the Service is what makes that visible
rather than silent. `/healthz` deliberately stays 200 while degraded — a
degraded service should not be killed, because degradation is a state this
platform is designed to run in.

**Only two Deployments get the LLM key.** signal-service and news-service. The
ExternalSecret is projected into those two alone, the IRSA grant matches, and
the NetworkPolicy allows egress to the internet from those two alone. Three
layers saying the same thing, because api-gateway renders responses to a browser
and must not hold a provider key.

## Placeholders you must replace

The base is not applyable on its own. Three values are deliberately wrong until
an overlay patches them, and each fails loudly rather than quietly misbehaving:

- **`000000000000`** in the IRSA role ARNs. Not a valid AWS account.
- **`quantos-models-REPLACE-ME`** as `MODEL_BUCKET`.
- **`REPLACE-ME`** in the ACM certificate ARN and `example.internal` hostnames.

The overlays use `111111111111` (dev) and `222222222222` (prod), which are also
placeholders. Replace them from `terraform output` before the first apply; the
values are in `service_role_arns` and `s3_buckets`.

## Cluster prerequisites

These are not installed by these manifests. Each is a cluster add-on with its
own lifecycle (see `infra/terraform/README.md`):

- External Secrets Operator, in namespace `external-secrets`
- AWS Load Balancer Controller (for `ingressClassName: alb`)
- Prometheus Operator, in namespace `monitoring`, with a `prometheus` pod label
- prometheus-adapter, exposing `quantos_kafka_lag` and
  `quantos_http_in_flight_requests` as custom metrics — without it the HPAs
  fall back to their CPU target and the lag metric reports `<unknown>`
- OpenTelemetry Collector, in namespace `observability`
- A CNI that enforces NetworkPolicy. The EKS VPC CNI does **not** on its own;
  enable its network-policy support or run Calico. Without one, every policy in
  `networkpolicy.yaml` is inert and the cluster is wide open while appearing
  locked down.

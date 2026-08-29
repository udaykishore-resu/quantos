# Running QuantOS on a local Kubernetes cluster

This describes the `infra/kubernetes/local` overlay: one [kind](https://kind.sigs.k8s.io/) node running PostgreSQL, ClickHouse, Redis, Prometheus, Grafana and QuantOS itself.

It is not the deployment in `infra/kubernetes/base`, and the difference is the first thing to understand.

---

## What runs, and why it is one process

The base manifests split the platform into nine Deployments, one per role — the topology in ADR-011. That split requires a broker. The roles publish events to each other, and a consumer in one pod has to be able to read what a producer in another pod wrote.

The WAL bus driver cannot carry that traffic. It serialises appends behind an in-process mutex through a buffered writer, and each process keeps its own record-offset counter. Two processes sharing a log directory interleave partial records and hand out the same offsets twice, and neither notices — the damage surfaces later as a CRC failure that looks like a disk fault. Since this was found, `bus.NewWALBus` takes an exclusive lock on the directory and a second writer **refuses to start** rather than corrupting it (`internal/bus.acquireDirLock`, covered by `TestASecondWALWriterIsRefused`).

The broker driver, `drivers/kafka`, is not written. So the honest local deployment is a single pod running all six roles against real backing stores:

| Component | What it is here | What it is in cluster mode |
|---|---|---|
| QuantOS | one Deployment, all roles, `mode: compose` | nine Deployments, one per role |
| Event bus | WAL on an `emptyDir` | MSK |
| Provenance store | PostgreSQL StatefulSet | RDS, multi-AZ |
| Analytical store | ClickHouse StatefulSet | managed ClickHouse |
| Cache, dedup, rate limits | Redis StatefulSet | ElastiCache |
| Metrics | plain Prometheus + Grafana | Prometheus Operator |
| Ingress | NodePort through kind port mappings | ALB with TLS |
| Secrets | one plain `Secret` | ExternalSecrets from Secrets Manager |

The nine-service manifests stay in `infra/kubernetes/base`, unchanged and ready. Deploying them today would produce nine pods that cannot talk to each other — which looks like a working system and is not one.

**What this still exercises that `make run` does not:** SQL persistence and the provenance gate, the ClickHouse analytical path, Redis-backed deduplication and rate limiting, container probes against `/healthz` and `/readyz`, and the metrics pipeline end to end. Those four are exactly the parts the embedded mode stubs out.

---

## Prerequisites

```bash
brew install kind kubectl kustomize
```

Plus a running Docker daemon — Docker Desktop, colima, or OrbStack. `make k8s-preflight` checks all four and says which is missing.

Budget about **6 GB of RAM** for the cluster. On Docker Desktop, raise the VM memory limit in Settings → Resources if it is at the 4 GB default, or the ClickHouse pod will be OOM-killed with no obvious explanation.

---

## Bringing it up

```bash
make k8s-up
```

That does five things, each available on its own if you want to run them separately:

1. `k8s-cluster` — creates the kind cluster from `infra/kubernetes/local/kind-cluster.yaml`. Idempotent.
2. `k8s-image` — builds `quantos/quantos:local` from `deploy/Dockerfile` with `SERVICE=quantos`, then `kind load`s it into the node. There is no registry, so this is the only way the image gets in; the Deployment sets `imagePullPolicy: Never` so a typo fails immediately instead of hanging on `ImagePullBackOff`.
3. Applies the manifests.
4. Waits for the three StatefulSets.
5. Waits for QuantOS, then prints where to reach everything.

**The first run is slow.** The image build compiles the whole dependency graph, and start-up replays `features.warmup_bars` (260) of history per symbol across 115 instruments before the first live bar. Two to four minutes is normal. The `startupProbe` allows five minutes before it gives up, which is why the pod does not restart-loop while warming.

Once it is up:

| | |
|---|---|
| API | http://localhost:8080/api/v1 |
| Health / readiness | http://localhost:8080/healthz, `/readyz` |
| Prometheus | http://localhost:9091 |
| Grafana | http://localhost:3001 |

```bash
TOKEN=$(curl -s -XPOST localhost:8080/api/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"subject":"demo","password":"demo"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')

curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/api/v1/market/regime
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/api/v1/model-health
```

The dashboard runs outside the cluster, pointed at the NodePort:

```bash
cd frontend && QUANTOS_API_URL=http://localhost:8080 npm run dev
```

---

## Day-to-day

```bash
make k8s-status     # pods, and the URLs again
make k8s-logs       # follow the platform log
make k8s-psql       # a psql shell against the in-cluster database
make k8s-restart    # rebuild the image and roll the pod
make k8s-render     # print the manifests without applying them
make k8s-down       # delete the objects, keep the cluster
make k8s-destroy    # delete the cluster
```

`k8s-render` is worth knowing: it is the same command `k8s-up` pipes into `kubectl apply`, so anything you can see there is exactly what will be applied.

Confirm the provenance store is actually being written, which is the point of running this instead of `make run`:

```bash
make k8s-psql
# then:
SELECT count(*) FROM predictions;
SELECT count(*) FROM risk_assessments;
SELECT status, count(*) FROM signals GROUP BY status;
```

---

## Things that will confuse you once

**No signals.** Expected on a quiet simulated tape — a random walk has no directional edge and the signal engine correctly declines. `GET /api/v1/stocks` shows the stage and reason for every symbol. The `--scenario` flag that `make run` accepts is not wired into the Deployment; to use it, add `--scenario`, `bull` to the container `args` in `infra/kubernetes/local/quantos.yaml` and `make k8s-restart`.

**`Recreate`, not `RollingUpdate`.** A rolling update briefly runs two pods, and the incoming one cannot take the WAL lock while the outgoing one holds it. A few seconds of downtime is the honest trade for a single-writer log. For the same reason `replicas` must stay at 1 — a second replica would refuse to start, correctly, and you would have a broken deployment reported as a healthy one.

**Pod Security is `baseline`, not `restricted`.** The QuantOS pod satisfies `restricted` — non-root, read-only root filesystem, no privilege escalation, all capabilities dropped, seccomp. PostgreSQL does not: its entrypoint starts as root to initialise the data directory before dropping to the postgres user, and `restricted` rejects it before it runs. The namespace enforces `baseline` and leaves `restricted` on warn and audit so the gap stays visible. This is a property of the local stack only — in cluster mode the databases are managed services outside the cluster, which is why `infra/kubernetes/base` enforces `restricted`.

**`--load-restrictor LoadRestrictionsNone`.** The overlay generates ConfigMaps from `deploy/sql/` and `deploy/grafana/`, which live above the kustomization root. kustomize refuses that by default. The alternative is a second copy of the schema inside the overlay, and a schema that drifts between two local environments is a bug that only appears in one of them. This is also why `kubectl apply -k` does not work here and the Make targets do.

**No traces.** No OTel collector or Jaeger runs in the local cluster, and `telemetry.otlp_endpoint` is empty. Spans are still generated and sampled; they have nowhere to go. The Grafana datasource file for the local cluster deliberately omits Jaeger — a datasource pointing at nothing makes every panel that uses it render an error, and then you cannot tell whether the platform is broken or the wiring is.

**Where the model artifact comes from.** It is baked into the image (`deploy/Dockerfile` copies `ml/artifacts`), not mounted. Compose bind-mounts it from the host, which hid the omission for a while: in a cluster there is no host to mount from, no artifact would be found, and every prediction would silently fall back to the deterministic rule prior. If you retrain with `make train`, run `make k8s-restart` to get the new artifact into the cluster.

---

## What has not been verified

The manifests are schema-valid — all 27 documents pass strict validation against the Kubernetes 1.29 API, and every image, port, environment variable, config path and probe endpoint has been cross-checked against the Go source. The `dev` and `prod` overlays pass the same check.

**They have never been applied to a running cluster.** There was no Docker daemon in the environment where this was written, so `kind create`, `docker build`, `kind load` and `kubectl apply` are all unexercised. Schema validity is not the same as working: an image that runs as an unexpected uid, a volume permission, or an init container that waits on the wrong thing would all pass every check above and still fail on first contact.

Expect to fix something. `kubectl -n quantos describe pod <name>` and `make k8s-logs` are where to look, and the most likely candidates are the ClickHouse schema Job and the PostgreSQL `fsGroup`.

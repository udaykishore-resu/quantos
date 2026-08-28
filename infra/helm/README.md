# QuantOS — Helm chart

`quantos/` parameterises the same deployment as `infra/kubernetes`. Either is
complete on its own; **never install both into one namespace**.

```sh
helm lint infra/helm/quantos
helm template quantos infra/helm/quantos --kube-version 1.30.0 --set mode=cluster

helm upgrade --install quantos infra/helm/quantos \
  -n quantos --create-namespace \
  -f infra/helm/quantos/values-prod.yaml \
  --set image.tag=v1.4.2
```

## The `mode` switch

The chart's one non-obvious feature. `mode` selects a runtime topology and
changes what renders:

| `mode` | Renders |
|---|---|
| `embedded` | one Deployment running every role in one process; no secrets, no NetworkPolicies, no topic Job |
| `compose` | nine Deployments against in-cluster dependencies you supply |
| `cluster` | nine Deployments, IRSA, External Secrets, NetworkPolicies, the topic Job |

`docs/operations/deployment.md` describes all three end to end.
`internal/config.Validate` refuses `cluster` with `auth.enabled: false`, so a
half-configured cluster release fails at start-up rather than at the first
request.

## `image.tag` is not set in `values-prod.yaml`

Deliberately. A tag committed to git drifts from what is deployed the moment
someone runs an upgrade with `--set`, and a floating tag is how a rollback
silently rolls forward. The release workflow passes the immutable tag.

## Namespace labels

`--create-namespace` creates a bare Namespace with no labels, so the Pod
Security `restricted` enforcement that
`infra/kubernetes/base/namespace.yaml` applies is **not** applied by this chart.
Every pod it renders already satisfies `restricted` — non-root uid 65532,
read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp —
but the admission controller will not be checking. Label the namespace yourself
if you want it enforced:

```sh
kubectl label namespace quantos \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest
```

The chart does not create the Namespace itself because a chart that owns its
namespace cannot be uninstalled without taking the ExternalSecret-materialised
Secrets and any PVCs with it.

## Secrets

The chart renders no Secret containing a value, in any mode. `helm template`
output is safe to paste into a ticket. Every credential is an `ExternalSecret`
pointing at AWS Secrets Manager, resolved at runtime by the External Secrets
operator using the IRSA role in `infra/terraform/modules/iam`.

There is deliberately no `--set postgresPassword=...`: a flag like that puts the
password in shell history, in `helm get values`, and in the release Secret.

## Prerequisites

Same as the manifests — see `infra/kubernetes/README.md`. The one worth
repeating: **NetworkPolicies need a CNI that enforces them.** The EKS VPC CNI
does not by default, and a policy set that renders and does nothing is worse
than none.

## Verifying a change

```sh
helm lint infra/helm/quantos
helm lint infra/helm/quantos -f infra/helm/quantos/values-dev.yaml
helm lint infra/helm/quantos -f infra/helm/quantos/values-prod.yaml

for mode in embedded compose cluster; do
  helm template q infra/helm/quantos --kube-version 1.30.0 --set mode="$mode" >/dev/null
done
```

`.github/workflows/ci.yml` runs exactly this, plus `kubeconform` over the
rendered output and a check that no rendered document is a `Secret`.

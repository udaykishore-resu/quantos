# QuantOS — ArgoCD

GitOps definitions matching `.github/workflows/cd.yml`.

```
project.yaml            AppProject: what may be deployed, where, by whom
application-dev.yaml    dev, automated sync with self-heal
application-prod.yaml   prod, sync only after the CD workflow's approval gate
applicationset.yaml     the same two environments from one generator
```

Apply the project first, then **either** the two Applications **or** the
ApplicationSet — never both. Two controllers owning the same Application name is
a reconcile loop.

```sh
kubectl apply -f infra/argocd/project.yaml
kubectl apply -f infra/argocd/application-dev.yaml
kubectl apply -f infra/argocd/application-prod.yaml
```

## Why prod has no `automated` block

Dev syncs itself: git is the truth and a manual `kubectl edit` should be
reverted within the minute. Prod does not, because an automated sync means a
merge to `main` reaches production without anyone deciding it should.

`cd.yml` triggers the prod sync after the `production` environment's approval
gate, and it rewrites `targetRevision` to the released tag first. Prod therefore
tracks a name that cannot move underneath it — a branch would mean prod changes
on someone else's merge.

## Why `/spec/replicas` is ignored

The HPA owns the replica count once it is running. Without the
`ignoreDifferences` entry, Argo reports permanent drift and `selfHeal` fights
the autoscaler on every reconcile — the two write the same field on a loop and
the pod count oscillates.

The same applies to `Secret./data`: the External Secrets operator materialises
those values from AWS Secrets Manager, and Argo must not try to manage them. The
project also blacklists `Secret` entirely: a Secret appearing in a manifest would
mean someone committed a literal value, and it should fail to sync rather than
be applied.

## The sync window

`project.yaml` denies automated sync into prod between 13:30 and 20:30 UTC on
weekdays — the US cash session. A bad deploy costs nothing but data on a
paper-trading platform, but a rollout during the open produces exactly the gap
in coverage the ingestion-continuity SLO measures, at the moment the data is
least reproducible. `manualSync: true` keeps the human path open: this blocks
the unattended one.

## Prerequisites

- ArgoCD in namespace `argocd`, with the kustomize build option enabled
  (default).
- The cluster add-ons listed in `infra/kubernetes/README.md`. Argo will sync the
  manifests happily without External Secrets installed, and every pod will then
  sit waiting on a Secret that never appears.

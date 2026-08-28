# Runbook — Model drift

**Severity:** page at `severe`, ticket at `moderate`
**Alerts:** `ModelDriftSevere`, `PredictionLatencySLOSlowBurn`
**Owner:** platform on-call, with the research owner

---

## Read this first

Drift is not an outage. Nothing is broken; the model is simply describing a
world that has moved. The platform keeps running and keeps producing signals,
and the risk engine keeps vetoing the ones it should — `risk.min_confidence` is
0.30 and a degraded model naturally produces lower confidence, so the veto
tightens on its own.

The scale is an ordinal, exported as `quantos_model_drift{kind}`:

| Value | `DriftSeverity` | Meaning |
|---|---|---|
| 0 | `none` | within the reference distribution |
| 1 | `mild` | watch |
| 2 | `moderate` | ticket; schedule a retrain |
| 3 | `severe` | demote to shadow |

The thresholds are in `evaluation:` — `psi_warn` 0.15, `psi_alert` 0.25,
`accuracy_drop_warn` 0.05, `accuracy_drop_alert` 0.10, over a `drift_window` of
500 against a `drift_reference` of 2000, with `min_samples` 100.

The last number matters most in practice: **under 100 samples there is no
verdict**, so a drift alert on a quiet universe or shortly after a deploy is
often a sample-size artefact rather than a signal.

## Symptoms

- `ModelDriftSevere`: `max(quantos_model_drift{kind="overall"}) >= 3` for 5 m.
- `quantos_prediction_accuracy` falling against its own recent history.
- `quantos_prediction_brier_score` rising (worse calibration).
- Veto rate climbing — a symptom, not a second incident, unless it crosses
  80 %, in which case see [`risk-veto-storm.md`](risk-veto-storm.md).
- `/api/v1/model-health` reporting the severity and the contributing kinds.

## Diagnosis

```sh
kubectl -n quantos port-forward svc/api-gateway 8080:8080 &
TOKEN=$(curl -sS -H 'Content-Type: application/json' \
  -d '{"subject":"operator","password":"operator"}' \
  localhost:8080/api/v1/auth/login | jq -r .data.token)

curl -sS -H "Authorization: Bearer $TOKEN" localhost:8080/api/v1/model-health | jq .
```

The report separates the kinds, and they mean different things:

| `kind` | What moved | Usually means |
|---|---|---|
| feature | input distribution (PSI) | the market regime changed |
| prediction | output distribution | the model is behaving differently on similar inputs |
| accuracy | realised outcomes | the relationship the model learned has weakened |
| overall | worst of the above | — |

```sh
# Which features moved?
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/evaluations | jq '.data[] | {model, horizon, regime, accuracy, brier}'
```

```sh
# Is this drift, or a regime change the model was never trained on?
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/market/regime | jq '.data | {regime, confidence, as_of}'
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/market/regime/history | jq '.data[:10]'
```

```sh
# Sample size, before anything else.
kubectl -n quantos exec deploy/evaluation-service -- \
  wget -qO- http://localhost:8086/metrics | grep -E 'quantos_prediction_(accuracy|brier)'
```

The four causes:

| Cause | Tell | Branch |
|---|---|---|
| Regime change | feature PSI high, accuracy falls in the new regime only | A |
| Genuine decay | accuracy falling steadily across regimes over weeks | B |
| Artifact mismatch | drift appears abruptly at a deploy boundary | C |
| Sample-size artefact | fewer than `min_samples` in the window | D |

## Remediation

### A. Regime change

Check accuracy **per regime**, not in aggregate. A model that is accurate in the
regime the market just left and inaccurate in the one it entered is a model with
a coverage gap, not a broken model — and the drift metric cannot tell them
apart.

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/evaluations | jq 'group_by(.regime)[] | {regime: .[0].regime, n: length, acc: (map(.accuracy) | add / length)}'
```

If the new regime has few training samples, the right action is to retrain with
history that covers it. In the meantime the platform is already handling it
correctly: `regime.min_confidence` (0.45) and `risk.min_regime_confidence`
(0.35) tighten the veto when the regime itself is uncertain.

Do not demote for a regime change alone. The model is not wrong; it is
uninformed, and its own confidence should already be saying so.

### B. Genuine decay

Retrain, evaluate walk-forward, and promote — in that order, off the critical
path:

```sh
make export-features                  # labelled snapshots from history
make train                            # writes a versioned artifact to ml/artifacts
make evaluate                         # walk-forward report in ml/reports
```

Read the walk-forward report before promoting. A model that beats the incumbent
on a single split and loses on the others is a model that got lucky on one
split.

Promote by publishing the artifact and moving `MODEL_ID`:

```sh
aws s3 sync ml/artifacts/ "s3://quantos-prod-<account>-models/direction-3class-v2/"

kubectl -n quantos patch configmap quantos-env --type merge \
  -p '{"data":{"MODEL_ID":"direction-3class-v2"}}'
kubectl -n quantos rollout restart deploy/signal-service deploy/evaluation-service
```

The artifact is pulled by the `fetch-models` init container at start-up, so a
promotion is a restart rather than an image rebuild.

Consider a canary first. `predict.canary_fraction` serves a fraction of
predictions from the new model while the incumbent stays authoritative:

```sh
kubectl -n quantos patch configmap quantos-config --type merge \
  -p '{"data":{"quantos.cluster.yaml":"...predict:\n  canary_fraction: 0.1\n..."}}'
```

### C. Artifact mismatch

Drift that appears exactly at a deploy boundary is usually not drift. The
feature contract is the suspect: the model was fitted on features computed one
way and is being served features computed another.

```sh
# The parity test is the check that Go and Python compute the same function.
python -m pytest ml/tests/test_go_parity.py -q
python -m pytest ml/tests/test_feature_contract.py -q
```

```sh
# Did the artifact even load?
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics | grep model_artifact_load_failures_total
```

A non-zero load-failure count means the service is serving from the deterministic
rule prior alone. That is handled — `predict.fallback_confidence_ceiling` (0.55)
caps confidence so a degraded prediction naturally fails the risk engine's
check — but it is a degraded state and it should be fixed rather than lived in.

Roll back the artifact:

```sh
kubectl -n quantos patch configmap quantos-env --type merge \
  -p '{"data":{"MODEL_ID":"direction-3class"}}'
kubectl -n quantos rollout restart deploy/signal-service
```

### D. Sample-size artefact

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/model-health | jq '.data.samples'
```

Below `min_samples` (100), the verdict is not meaningful. Wait for the window to
fill. This is common after a deploy, after a market-data outage, and on a
weekend.

If it is persistently below the threshold during market hours, the problem is
upstream: fewer predictions are being made than expected. See
[`market-data-outage.md`](market-data-outage.md).

### Demotion to shadow

At `severe`, the model should stop being authoritative. Shadow mode keeps it
producing predictions that are recorded and scored but not acted on:

```sh
# blend_weight 0 serves the deterministic rule prior alone; the model still runs
# and is still evaluated, so you keep measuring it while it stops deciding.
kubectl -n quantos patch configmap quantos-config --type merge \
  -p '{"data":{"quantos.cluster.yaml":"...predict:\n  blend_weight: 0\n..."}}'
kubectl -n quantos rollout restart deploy/signal-service
```

Expect signal volume to fall. The rule prior is capped at
`fallback_confidence_ceiling` 0.55 and `risk.min_confidence` is 0.30, so more
predictions fail the confidence check. That is the intended trade: fewer signals
from a model you trust, rather than the usual number from one you do not.

## Verify recovery

```
max(quantos_model_drift{kind="overall"}) <= 1
quantos_prediction_accuracy         back within its recent band
quantos_prediction_brier_score      falling
quantos_model_artifact_load_failures_total  no increase
```

Then check the veto rate has come back down, since drift and veto rate move
together:

```
sum(rate(quantos_risk_decisions_total{decision!="ALLOW_PAPER_SIGNAL"}[1h]))
  / sum(rate(quantos_risk_decisions_total[1h])) < 0.8
```

## Rollback

Rolling back a model is rolling back `MODEL_ID` and restarting — the artifacts
are versioned in S3 with versioning enabled and a 365-day noncurrent retention
precisely so that a model from four months ago is still there to roll back to.

Predictions made by the bad model are **not** retracted. They are recorded,
evaluated and scored like any other, which is what makes the decision to demote
measurable rather than a matter of opinion. Their signals expire normally
(`signals.default_ttl` 4 h) or are invalidated by the sweeper.

## Related

- [`risk-veto-storm.md`](risk-veto-storm.md) — when drift pushes the veto rate up
- `docs/adr/ADR-006-model-serving.md`
- `docs/ml/` — training, evaluation, the feature contract
- `internal/evaluation/` — drift computation and the PSI thresholds

# ADR-006 — In-process model serving from versioned artifacts

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

Prediction must complete within a P95 of 1 s end-to-end, and inference happens
per symbol per feature update — up to a few thousand calls per second at full
universe scale. We also need: exact reproducibility of a historical prediction,
identical behaviour in backtests and live, safe rollout of new models, and
operation with **no** GPU and **no** external dependency in the demo mode.

## Options

1. **Python model server (FastAPI/TorchServe/Triton) over HTTP or gRPC.**
   Standard, flexible, supports any framework. Costs a network hop (0.5–5 ms
   P99, worse under load), adds a hard runtime dependency on the critical path,
   and makes backtests either slow or dependent on a running service.
2. **Sidecar per pod.** Removes the network hop's variance but keeps the
   process, the language runtime, and the memory cost per pod.
3. **In-process inference in Go from a versioned artifact.** Chosen.
4. **ONNX Runtime via cgo.** Supports arbitrary architectures. Rejected for now:
   cgo complicates cross-compilation and static binaries, and our model families
   (regularised logistic regression and gradient-boosted stumps over ~40
   engineered features) do not need it. The artifact format leaves a door open:
   `type: "onnx"` is reserved.

## Decision

Models are trained in Python and exported to a **content-addressed JSON
artifact** loaded in-process by `internal/mlinfer`.

```
ml/artifacts/direction-3class-v4.json        # weights + calibration
ml/artifacts/direction-3class-v4.meta.json   # manifest
```

The manifest records: model id and semantic version, artifact `sha256`, model
family, the **ordered feature list**, standardisation parameters, target
definition and horizon, training/validation/test windows, walk-forward summary,
calibration report, regime-partitioned metrics, git commit of the training code,
and the `mlinfer` schema version it targets.

At load, `mlinfer` verifies the digest and asserts that the feature list matches
exactly — same names, same order — what `internal/features` produces for that
model's declared feature set. A mismatch is a hard start-up failure in
`cluster` mode and a demotion to the rules-only prior in `embedded`/`compose`.

**Rollout.** The model registry (Postgres `model_versions`) marks each version
`shadow`, `canary`, `active` or `retired`. Shadow models are scored on every
request and their predictions recorded, but they never influence output. Canary
serves a configurable fraction keyed by a hash of the ticker (stable, so a
symbol does not flap between models within a session). Promotion requires the
gates in the governance charter §5.

**Fallback.** If no artifact is loadable, `predict` uses the deterministic rule
prior alone, stamps `model_version = "rules-fallback"`, and caps confidence at
`fallback_confidence_ceiling` (default 0.55) so downstream risk naturally moves
signals to `WATCH_ONLY`.

## Trade-offs

- (+) Inference is ~1–10 µs, allocation-light, and adds no failure domain.
- (+) Backtests and live use the identical code path and artifact — a
  historical prediction can be recomputed bit-for-bit from the stored feature
  snapshot and artifact digest.
- (+) `make demo` needs no model server, no GPU and no network.
- (+) Shadow/canary is trivial when models are just data.
- (−) Model families are limited to what `mlinfer` implements. Deep models would
  need ONNX or an external server; that is a deliberate future step, not a
  current need.
- (−) Artifact size is bounded by memory; a very large ensemble would not fit
  this pattern.
- (−) Two implementations of the same math (Python fit, Go predict) must agree.
  Guarded by a golden-vector test: training emits 500 (features → probability)
  pairs into `ml/artifacts/*.golden.json`, and a Go test asserts agreement to
  1e-9.

## Failure modes

- *Digest mismatch / corrupt artifact.* Refuse to load; keep the previously
  loaded version; alert. Never serve an unverified artifact.
- *Feature-list drift.* Hard fail in production (see above) — the alternative,
  silently reordered features, is a wrong-answer failure and is worse than an
  outage.
- *Calibration decay.* Drift detector demotes `active` → `shadow` automatically
  and emits `model.drift.detected`.
- *Golden-vector divergence after a library upgrade.* CI fails before merge.

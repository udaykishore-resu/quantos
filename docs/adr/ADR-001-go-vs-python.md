# ADR-001 — Go for the production runtime, Python for research

- **Status:** Accepted
- **Date:** 2026-08-27
- **Deciders:** Platform architecture

## Problem

QuantOS has two workloads with opposite characteristics. The runtime path
(ingest → features → regime → predict → risk → signal → alert) is high-fan-in,
latency-sensitive (P99 < 100 ms per market event), long-lived and highly
concurrent. The research path (feature discovery, model training, statistical
validation, calibration analysis) is iterative, notebook-shaped, and depends on
the numerical Python ecosystem. Using one language for both means either
fighting the GIL and packaging in production, or reimplementing statsmodels and
scikit-learn in Go.

## Options

1. **Python everywhere.** Fastest research iteration. Loses on concurrency
   (GIL forces multiprocessing for CPU-bound feature math), on memory footprint
   per pod, on cold start, and on deployment hygiene. A single asyncio consumer
   handling 500 symbols × 4 topics is achievable but the tail latency is
   dominated by GC pauses and serialisation.
2. **Go everywhere.** Excellent runtime properties. Research becomes painful:
   no mature equivalent of pandas/statsmodels/scikit-learn, and quant work is
   exploratory by nature. Model experimentation velocity would collapse.
3. **Rust runtime + Python research.** Best raw latency. Team velocity cost and
   ecosystem cost (Kafka, OTel, AWS SDK maturity) do not pay for themselves at
   our SLO targets, which Go meets comfortably.
4. **Go runtime + Python research, artifacts as the contract.** Chosen.

## Decision

Go 1.24 owns everything on the request/event path: API services, Kafka
consumers and producers, market-data ingestion, feature computation, the rule
engine, prediction *inference*, risk, signals, alerts, paper trading,
backtesting execution and orchestration.

Python 3.11 owns model *training*, statistical analysis, feature research,
calibration studies, walk-forward experiment orchestration and evaluation
reporting.

The interface between them is a **versioned, content-addressed model artifact**
(`ml/artifacts/<name>-<version>.json` plus a `.meta.json` manifest) containing
the feature list in canonical order, standardisation parameters, model
coefficients or tree structure, the calibration map, and the training/validation
windows. Go loads it in `internal/mlinfer` and refuses artifacts whose feature
list does not match what `internal/features` produces.

Crucially: **the feature definitions are authored once, in Go**, and Python
consumes feature snapshots exported from the same code path (via
`quantos export-features`). This eliminates the classic training/serving skew
where a Python `rolling(14).mean()` and a Go incremental EMA disagree at the
boundary.

## Trade-offs

- (+) Runtime meets latency SLOs with a single process per service and no GIL work-arounds.
- (+) Research keeps the full scientific Python stack.
- (+) Training/serving skew is structurally prevented by the export path.
- (−) Two toolchains, two CI lanes, two dependency ecosystems.
- (−) A new model family (e.g. gradient boosting) needs a Go inference
  implementation, not just a `pickle`. Mitigated by keeping the artifact format
  explicit and small; `mlinfer` currently supports logistic and gradient-boosted
  stumps, both trivially portable.
- (−) Engineers must read both languages.

## Failure modes

- *Artifact/feature drift.* Go rejects on feature-list mismatch at load and the
  service starts with the rules-only fallback rather than serving a
  silently-misaligned model. Alarmed via `model_artifact_load_failures_total`.
- *Python-only feature leaks into research.* Prevented by the export path and by
  a CI check that fails if `ml/features/` defines a transform not present in
  `internal/features`.
- *Version pinning drift.* Both toolchains are pinned in CI; the artifact
  manifest records the exact `mlinfer` schema version it targets.

# QuantOS Safety & Governance Charter

This document is normative. Code that violates it is a defect regardless of test
status.

## 1. What QuantOS is

An **educational and research** platform for studying market structure,
probabilistic forecasting, risk control and strategy evaluation, with a
**paper-trading** simulator attached.

## 2. What QuantOS is not

- It is **not** investment advice, a recommendation, a solicitation, or an offer.
- It does **not** execute real-money orders. There is no broker execution
  adapter in this repository, and `paper.Broker` is the only order sink.
- It does **not** claim predictive certainty about any instrument.

## 3. Hard product rules

| ID | Rule | Enforcement |
|----|------|-------------|
| G-1 | No output may state that an instrument *will* move. All directional output is a probability distribution over UP/FLAT/DOWN with explicit uncertainty. | `analyst.Guard` rejects certainty language; `predict` has no point-forecast API |
| G-2 | An LLM may never originate a numeric decision. | package import boundary + `tests/integration/architecture_test.go` |
| G-3 | Every numeric claim in generated prose must be traceable to structured input. | `analyst/grounding.go`, fails closed to the template renderer |
| G-4 | The risk engine has veto authority. No component may emit a signal that risk blocked. | `signal.Engine` requires a `risk.Assessment` with `Decision == AllowPaperSignal`; unit + property tests |
| G-5 | Stale market data suspends signal generation. | `features.Freshness` gate, checked in `signal.Engine.Evaluate` |
| G-6 | Every signal carries invalidation conditions and is auto-invalidated when they trigger. | `signal.Invalidator`, exercised by DEMO 6 |
| G-7 | Every prediction is evaluated against realised outcome and the result is published. | `evaluation.Scheduler` |
| G-8 | Real-money execution requires a build tag that does not exist, an explicit config flag, and a signed operator attestation. None are shipped. | `paper.guardRealMoney()` panics at init if `QUANTOS_ALLOW_REAL_MONEY` is set |
| G-9 | Classifications (`STRONG_WATCH` … `AVOID`) are research labels, not advice, and every API response carrying one includes the disclaimer field. | `httpx.Disclaimer` middleware |
| G-10 | Options and positioning claims must express uncertainty and may not assert institutional intent. | `options` package returns `Confidence` and `DataQuality` on every metric |

## 4. Disclaimer surface

Every API response envelope contains:

```json
"disclaimer": "QuantOS is an educational research and paper-trading platform. Output is probabilistic, may be wrong, and is not financial advice. No real-money orders are placed."
```

The dashboard renders it persistently in the layout chrome, not as a
dismissible toast.

## 5. Model governance

- Every model artifact is content-addressed (`sha256`) and registered in
  `model_versions` with its training window, feature list, hyperparameters,
  walk-forward results and calibration report.
- A model may not be promoted to `active` unless: out-of-sample Brier score
  beats the base rate, reliability-diagram max deviation < 0.10, and
  regime-partitioned accuracy is reported for all regimes with n ≥ 100.
- Drift detection (PSI on features, KL on prediction distribution, rolling
  accuracy) demotes a model to `shadow` automatically and raises
  `model.drift.detected`.

## 6. Data governance

- Provider terms are respected via the provider abstraction; no scraping
  adapters ship in this repo.
- Backtests must pass the leakage guard (`backtest.LeakageGuard`) which fails a
  run if any feature timestamp is ≥ the decision timestamp.
- Survivorship bias: the universe loader is point-in-time; delisted symbols are
  retained with their delisting date and included in historical universes.

## 7. Security posture

OAuth2/OIDC + JWT, RBAC with four roles (`viewer`, `analyst`, `operator`,
`admin`), per-principal rate limits, TLS everywhere, secrets from AWS Secrets
Manager / Kubernetes Secrets (never in images or the frontend bundle),
append-only audit log for every state-changing action. Details in
[`threat-model.md`](threat-model.md).

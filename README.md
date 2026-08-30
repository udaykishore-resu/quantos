# QuantOS

An educational and research platform for market intelligence, probabilistic prediction, risk assessment and paper trading.

**QuantOS does not give financial advice, does not guarantee outcomes, and does not place real-money orders.** Every number it produces is a probability with a horizon, a confidence and a provenance chain. Some of them are wrong, and the platform is built to tell you which ones and how often.

```
make demo        # ten scenarios, one process, no infrastructure, ~80 seconds
```

---

## What it actually does

A bar of market data arrives and moves through a fixed pipeline. Each stage is deterministic, each stage records what it did, and no stage can be skipped:

```
market data → validation → features → deterministic rules → quantitative models
  → ML prediction → market regime → RISK ENGINE (veto) → opportunity scoring
  → policy validation → AI explanation → paper signal → monitoring → evaluation
```

Three properties of that pipeline are the product:

**The risk engine has a veto, and the veto is structural.** `domain.Signal` has no exported constructor other than `NewSignal(draft, assessment)`, which returns `ErrRiskVeto` unless the assessment says `ALLOW_PAPER_SIGNAL`. Eleven checks run; resolution is most-severe-wins with no averaging and no override. A check that *cannot* be evaluated returns `WATCH_ONLY`, not a pass — if we cannot measure a risk, we do not get to assume it is absent.

**A language model can never invent a number.** `internal/llm` and `internal/analyst` have no import path, direct or transitive, to `internal/signal`, `internal/risk`, `internal/predict`, `internal/scoring`, `internal/rules`, `internal/opportunity` or `internal/paper`. `tests/integration/architecture_test.go` fails the build if that edge ever appears. On top of the structural boundary sit a grounding validator (every number in the narrative must trace to a value in the record) and a language guard (no certainty, no advice, no claims about positioning), with a deterministic template as the fallback. The narrative is generated *after* the decision is final and can only write prose.

**Every decision is reproducible.** Feature snapshots are content-hashed. Engines take an `obs.Clock` and never call `time.Now()` in a decision path — a test enforces that too. Iteration is sorted, randomness is seeded per symbol. Two independently constructed platforms given the same seed produce byte-identical feature hashes and predictions; `tests/integration/pipeline_test.go` checks it.

Ask "why did QuantOS generate this signal?" and `GET /api/v1/signals/{id}/provenance` answers it: the data, the feature snapshot and its hash, the rules that fired, the counter-evidence that also fired, the model and version, the regime, the risk decision with all eleven checks, and the conditions that would invalidate it.

---

## Running it

### The demo — nothing to install

```bash
make demo
```

Ten scenarios in one process, on a simulated market with a fixed seed. It is the fastest way to see the whole platform behave, including the parts that only show up when something breaks: a risk veto, a drift alert, a bus outage, and a PostgreSQL outage that suspends signal emission because provenance can no longer be recorded.

### A live process

```bash
make run                              # every role, in-memory bus and store
go run ./cmd/quantos run --scenario bull
```

A note worth reading before you conclude something is broken: **on a quiet simulated tape the platform emits no signals, and that is correct.** A random walk has no directional edge, the rule scores sit below their strategy minimums, and the signal engine declines. `/api/v1/stocks` will show you the stage and reason for every symbol it declined. `--scenario` (`bull`, `bear`, `breakout`, `volatile`, `crash`, `illiquid`) applies a deliberately extreme market condition so the later stages of the pipeline are actually exercised. Real markets do not trend at 45% a year on demand; the scenarios are a demonstration device, not a claim.

### The full local stack

```bash
make dev          # Kafka, Postgres, ClickHouse, Redis, Prometheus, Grafana, Jaeger, OTel
make frontend     # the Next.js dashboard on :3000
```

| | |
|---|---|
| API | http://localhost:8080/api/v1 |
| Dashboard | http://localhost:3000 |
| Grafana | http://localhost:3001 |
| Prometheus | http://localhost:9091 |
| Jaeger | http://localhost:16686 |

Log in with the local development principals in `config/quantos.yaml` (`demo`/`demo`, `operator`/`operator`). They exist in `embedded` and `compose` mode only; `cluster` mode refuses to start without a real OIDC issuer.

### A local Kubernetes cluster

```bash
brew install kind kubectl kustomize   # plus a running Docker daemon
make k8s-up
```

One kind node running PostgreSQL, ClickHouse, Redis, Prometheus, Grafana and
QuantOS. The API lands on http://localhost:8080, Prometheus on :9091, Grafana
on :3001. `make k8s-status` prints them again; `make k8s-destroy` removes the
cluster.

This runs QuantOS as a **single all-roles process**, not the nine-service split
in `infra/kubernetes/base`. That split needs a broker to carry events between
pods, and `drivers/kafka` is the one substantial piece of the specification that
is not built. The WAL driver is a single-process log — it now refuses to start
if a second process tries to share its directory rather than silently
interleaving records. `docs/operations/local-kubernetes.md` has the full
reasoning and the list of what has not been verified.

### Research and backtesting

```bash
make export-features   # labelled snapshots from the *Go* feature engine
make train             # fit the direction model, write a versioned artifact
make evaluate          # walk-forward validation with an embargo
make backtest          # deterministic event-driven backtest
```

Training data comes from the Go feature engine rather than a parallel Python implementation, so a model is never fitted on a subtly different definition of a feature from the one that will serve it. `ml/tests/` includes a Go/Python parity check that pins the two implementations to within 1e-12.

---

## Three runtime modes, one set of engines

| Mode | Bus | Store | Used for |
|---|---|---|---|
| `embedded` | in-process | in-memory | `make demo`, `make run`, tests |
| `compose` | WAL or Kafka | Postgres + ClickHouse + Redis | `make dev` |
| `cluster` | MSK | RDS + ElastiCache + S3 | `infra/` |

The engines are identical in all three. The difference is only where events and rows go, which is what makes the demo a real test of the platform rather than a separate toy.

---

## What the platform will not do

These are enforced in code, not documented as intentions:

- **It will not emit a signal it cannot record.** `App.CanEmitSignals()` gates emission on the provenance store being writable. Under a PostgreSQL outage the platform suppresses signals, invalidates the in-flight one, and fails readiness — reads continue from cache, and `degraded` names what is missing.
- **It will not act on stale data.** Past the staleness threshold, a symbol publishes to `market.stale` and downstream consumers suspend. Consumer lag past a bound becomes silence, not late action.
- **It will not score a prediction dishonestly.** A prediction resolved more than five minutes after its horizon is *discarded as unresolvable* rather than scored against a price from outside its window, and the discard is counted so a sweeper that has fallen behind is visible.
- **It will not trust an unmeasured model.** `mlinfer` refuses to load an artifact that does not carry its own held-out accuracy and base rate. A model that did not beat its base rate offline is marked unhealthy and blocked by the risk engine.
- **It will not take a model's word on a question it has not been measured to answer.** The blend weight scales with the model's measured lift over its base rate, and the *directional* half of the blend scales separately with its measured UP-vs-DOWN AUC. A model that separates quiet tapes from moving ones but cannot tell up from down gets the first job and not the second. Unmeasured counts as no skill.
- **It will not share a single-writer log between processes.** The WAL bus takes an exclusive lock on its directory and a second writer refuses to start, because two processes appending to the same log interleave partial records and reuse offsets — and the damage only surfaces later as a CRC failure that looks like a disk fault.
- **It will not report accuracy without a base rate.** 55% accuracy against a 60% base rate is worse than useless, and the platform says so rather than showing you the 55%.
- **It will not hide what the veto cost.** Blocked predictions are evaluated too, so the veto is falsifiable rather than superstitious.

---

## Repository layout

```
cmd/quantos/          run | demo | backtest | export-features | validate | topics
services/             nine deployable binaries, each a thin entrypoint (ADR-011)
internal/domain/      the shared vocabulary; imports nothing else in the repo
internal/bus/         envelopes, topics, in-memory and WAL drivers, idempotency
internal/marketdata/  provider abstraction, deterministic simulator, validation
internal/features/    incremental indicators → content-hashed snapshots
internal/rules/       YAML strategies, condition evaluation, counter-evidence
internal/predict/     rule prior, model blending, horizon scaling, flat band
internal/risk/        eleven checks, most-severe-wins, the veto
internal/signal/      construction, policy, invalidation
internal/backtest/    event-driven engine, leakage guard, walk-forward, metrics
internal/evaluation/  outcome scoring, calibration, drift, veto cost
internal/analyst/     LLM narrative, grounding validator, language guard, template
internal/app/         the composition root
strategies/           the five shipped strategies, as configuration
ml/                   Python training, inference parity, walk-forward evaluation
frontend/             Next.js + TypeScript dashboard
infra/                Terraform, Kubernetes, Helm, ArgoCD
deploy/               docker-compose, Dockerfile, SQL schemas, observability config
infra/kubernetes/local/  the kind stack: backing stores plus one all-roles pod
docs/                 architecture, ADRs, runbooks, security governance, SLOs
tests/                architecture invariants, pipeline integration, failure injection
```

---

## Testing

```bash
make test        # go test ./... -race
make cover       # coverage report
make arch        # the architecture invariants alone
make validate    # configuration, strategies and universe
```

The tests worth knowing about:

- `tests/integration/architecture_test.go` — the import-graph invariants. It is the reason the LLM boundary is a fact rather than a policy.
- `tests/integration/pipeline_test.go` — the whole decision path on the shipped configuration, plus the reproducibility check.
- `tests/failure/` — fault injection: stale data, future-dated ticks, a bus outage, a handler that keeps failing, a dead provider, and missing risk inputs.
- `internal/domain/signal_test.go` — a 100,000-iteration property test that no signal can be constructed without an allowing risk assessment.
- `internal/backtest/backtest_test.go` — the leakage guard, which *aborts* a run rather than warning: a fill at the decision timestamp is the most common way a backtest reports returns it could never have earned.

---

## Documentation

- `docs/architecture/overview.md` — the system, end to end
- `docs/adr/` — eleven decision records, including why the LLM is isolated (ADR-005), why the risk engine has a veto (ADR-009), and why this is one module (ADR-011)
- `docs/security/governance.md` — the G-1..G-10 rules, and where each is enforced
- `docs/operations/slo.md` — the latency and correctness objectives
- `docs/operations/deployment.md` — the three runtime modes in practice
- `docs/operations/local-kubernetes.md` — running the whole stack on kind
- `docs/runbooks/` — one per realistic failure, with diagnosis and rollback
- `docs/ml/model-card.md` — what the shipped model does, what it was measured at, and what it cannot do

---

## Honest limitations

The shipped model has a small measured lift over its base rate and **no measurable directional skill** — its UP-vs-DOWN AUC on held-out data is approximately 0.5. That is written in the model card, and the prediction engine acts on it: the model earns weight over the FLAT/moving split and none over direction. The platform is built to make that kind of finding visible rather than to hide it behind an impressive-looking accuracy number.

The market data is simulated. The simulator is careful — stochastic volatility, jumps, an intraday volume smile, a market factor, per-symbol betas, a VIX that tracks synthetic implied volatility — but it is not a market, and nothing measured against it transfers to one.

This is research software. It is not a trading system, it is not advice, and it should not be connected to a broker.

---

## License

[MIT](LICENSE). Use it, fork it, build on it.

One thing the licence says in legal capitals and this says in plain words: the
software comes with **no warranty of any kind**, and the authors carry no
liability for what anyone does with it. That is standard boilerplate in most
repositories and less boilerplate here. This platform models markets, produces
probabilities, and is wrong a measurable fraction of the time by design. It is
research and teaching software. Nothing in it is financial advice, none of its
output is a guarantee, and it must not be connected to a broker or used to move
real money.

# QuantOS research tree

Python research for the 3-class direction model. It fits a model, writes an
artifact that `internal/mlinfer` can load, and measures how well that model
actually does — against the base rate, out of sample, with an embargo.

**This is educational research software on simulated market history.** Its
output is probabilistic, frequently wrong, is not financial advice, and never
reaches a real-money order path.

---

## The one-minute version

```bash
make export-features      # Go writes ml/data/features.csv
make train                # fits and writes ml/artifacts/<model-id>-<version>.json
make evaluate             # walk-forward report into ml/reports/
python3 -m pytest ml/tests -q
```

The current shipped result, for the impatient: **test accuracy 0.4190 against a
base rate of 0.3923** — a lift of **+0.0268** — and a walk-forward mean lift of
**+0.0068 with a fold-to-fold standard deviation of 0.0141**. The dispersion is
larger than the mean, so the honest reading is *no demonstrated edge*. The
directional AUC is 0.495, which is a coin flip: what little the model knows is
about *whether* the price will move, not *which way*. See
[`docs/ml/model-card.md`](../docs/ml/model-card.md), and in particular the
integration note — with this artifact serving, the platform emits no paper
signals, and that is the model being weak rather than the platform being broken.

## Install

```bash
pip install --break-system-packages -r ml/requirements.txt
```

numpy, pandas, scikit-learn and pytest. Nothing heavier: no torch, no xgboost,
no lightgbm. If scikit-learn will not install, the trainer detects that at
import time and falls back to a pure-numpy gradient-descent fit of the same
penalised objective (`models._fit_logistic_numpy`), and records which path it
used in `metadata.hyperparameters.solver_lbfgs`. The stumps family and every
metric are numpy-only already. The trainer runs end to end either way.

## Layout

| path | what it is |
|---|---|
| `features/schema.py` | The feature contract, mirrored from Go and *verified against the Go source at runtime* |
| `features/dataset.py` | CSV loading with a strict header check, plus chronological splitting with an embargo |
| `training/models.py` | The two fitters `mlinfer` can serve, and temperature calibration |
| `training/train.py` | CLI: fit, calibrate, evaluate, write the artifact |
| `inference/forward.py` | A Python re-implementation of the artifact forward pass, used **only** to verify parity with Go |
| `evaluation/metrics.py` | Scoring rules, mirroring `domain.BrierScore` / `domain.LogLoss` |
| `evaluation/walk_forward.py` | CLI: anchored or rolling folds, JSON + Markdown report |
| `tools/parity/main.go` | Runs vectors through the *production* Go inference path so tests can diff it |
| `tests/` | pytest: contract, look-ahead, calibration, schema, Go/Python parity |
| `artifacts/` | What the Go runtime loads (`predict.artifact_dir`) |
| `data/`, `reports/` | Exported features; generated reports |

## The five guarantees

Everything in this tree exists to hold five properties. Each is enforced by a
test rather than by a convention.

### 1. The feature contract cannot drift

`domain.ModelFeatureSet` is 16 ordered names, and `mlinfer` indexes coefficient
rows positionally. Two features whose names were swapped would still validate
and still serve — as a different model.

So `features/schema.py` does not merely copy the list: `verify_against_go_source`
re-derives it by parsing `internal/domain/features.go` and
`internal/domain/prediction.go`, and `train.py` calls it before it reads a
single row. The CSV header check is exact and positional, not set membership.
Both fail loudly. `tests/test_feature_contract.py` proves it.

### 2. No look-ahead

Labels come from the `fwd_ret_bps_<h>` columns the Go exporter computed from
prices strictly after each feature timestamp — Python never constructs a label.

Splits are chronological, never random, and carry an **embargo of at least 2x
the horizon** between every pair of partitions. A training row's label reads
prices up to `ts + horizon`, so a row within one horizon of the boundary carries
information from inside the next partition; the rows in the gap are discarded
rather than assigned to either side. `chronological_split` refuses an
`embargo_multiple` below 2 and refuses unsorted input. Boundaries snap to
timestamp edges so the ~100 symbols sharing a bar close are never split apart.

The standardiser is fitted on training rows only. The calibration temperature is
fitted on validation only. Test is touched exactly once, to report a number.
`tests/test_splits.py` proves it.

### 3. Determinism

Running `train.py` twice on the same CSV produces a **byte-identical** artifact
apart from `metadata.trained_at` (and set `SOURCE_DATE_EPOCH` to pin even that).

Neither fitter draws a random number — the stumps search is exhaustive over a
fixed quantile grid, and the temperature fit is a golden-section search. The
`--seed` flag is recorded and inert, and a test asserts that two seeds produce
identical coefficients so that stops being true loudly rather than quietly.
Every float is rounded to 12 significant digits before serialisation, so a
last-bit difference in a BLAS reduction cannot change the artifact's digest —
and the digest is a deployed model's identity.

### 4. Go and Python compute the same function

`ml/tools/parity` runs feature vectors through `mlinfer.Model.PredictVector` —
the production path, not a copy of it — and prints the distributions.
`tests/test_go_parity.py` feeds the same vectors through `ml/inference/forward`
and diffs.

**Measured: max absolute deviation 2.2e-16 over 2,000 vectors**, i.e. one unit
in the last place, with 22% of vectors matching bit-for-bit. It is not exact by
design: Go's `math.Exp` and `math.Pow` are Go's own, not glibc's. The test
tolerance is 1e-12; a real divergence — a transposed class row, a missed clamp,
calibration applied in logit space instead of probability space — moves
probabilities by 1e-3 or more.

To make that comparison meaningful, `forward.py` mirrors Go's *operation order*,
not just its algebra: the score accumulates one scalar term at a time in
ascending feature index, because a vectorised dot product reassociates the sum.
A vectorised path exists for bulk evaluation and a test pins the two together.

Run it by hand:

```bash
GOPROXY=direct GOSUMDB=off GOPRIVATE='*' GOFLAGS=-mod=mod \
  go run ./ml/tools/parity -artifact ml/artifacts/<file>.json -vectors 8
```

### 5. The artifact carries its own report card, or it does not load

`mlinfer.Artifact.Validate` **rejects** any artifact that omits
`metadata.test_samples`, `metadata.test_accuracy` or `metadata.test_base_rate`,
or whose accuracy and base rate are not proportions in `(0, 1]`. There is no
degraded mode for an unmeasured model: the file simply does not load, the
registry has nothing to serve, and every prediction comes from the deterministic
rule prior.

So `train.py` stamps four fields onto every artifact, from the **test split** —
the one the fit never saw and the calibrator never saw either:

| field | meaning |
|---|---|
| `metadata.test_samples` | rows in the held-out split |
| `metadata.test_accuracy` | accuracy on it |
| `metadata.test_base_rate` | accuracy of always predicting its majority class |
| `metadata.test_brier` | multiclass Brier score, 0 best / 2 worst |

The base rate travels in the same file as the accuracy because that is the only
way the accuracy means anything: 0.42 on a three-class problem is a result or a
failure depending entirely on a number that has to be quoted beside it.

`calibration.ece` and `calibration.max_deviation` come from the **validation**
split, because they describe the temperature that was fitted there.

At load time `mlinfer.Artifact.ProvisionalHealth` turns those numbers into a
health record so a freshly trained model can serve before any live prediction has
resolved. It marks the model **unhealthy** — and the risk engine then blocks it —
if either:

- `test_accuracy <= test_base_rate`: the model is no better than a constant
  guess, or
- `calibration.max_deviation > 0.10`: its stated confidence is not its empirical
  accuracy, and every downstream gate thresholds on that confidence.

`train.py` mirrors both rules (`check_report_card`, `provisional_health_verdict`)
and prints the verdict as it writes the file, so you learn that the runtime will
refuse a model at training time rather than at start-up. It refuses to write an
artifact whose report card would fail `Validate` at all. `tests/test_artifact.py`
recomputes the four fields from the test split and fails if they were taken from
validation or train; `tests/test_go_parity.py` deletes each required field in
turn and asserts the **real Go loader** refuses the file.

## How the artifact is discovered

`mlinfer.LoadDir` reads every `*.json` in `predict.artifact_dir` that is not a
`*.meta.json` or `*.golden.json` sidecar, and for each `model_id` promotes the
**lexicographically greatest version string** to active, registering the rest as
shadows. There is no `latest.json`; the registry derives "latest" from the
version itself, so writing one would load a second copy of the same model that
competes for the active slot.

The version is therefore built to sort correctly:

```
v1.<samples, zero-padded to 9 digits>.<first 8 hex of the CSV's sha256>
   e.g. v1.000339040.e6a8254a
```

Without the padding, a 9,000-row artifact would outrank a 100,000-row one.
`train.py` also writes `<model-id>-latest.meta.json` — a human-facing pointer
carrying the version, the artifact's sha256 and the measured metrics. The
`.meta.json` suffix is deliberate: the Go loader skips it.

`metadata.generator` is set to `quantos/ml/training/train.py`, so a fitted model
can never be mistaken for a synthetic bootstrap one.

## Reading the numbers honestly

Accuracy never appears in this tree without its base rate beside it. On a
three-class problem where the majority class holds 39% of the mass, 39% accuracy
is what you get for free.

Two baselines are reported, because they answer different questions:

- **Base rate** — always predict the most common class. Beating it means the
  argmax carries directional information.
- **Class-prior log loss** — always predict the sample's own class distribution.
  Beating it means the *probabilities* carry information, which is a weaker and
  quite different claim.

The current model beats the second and, on walk-forward, not reliably the first.
The report and the model card say so in those words.

## Known limits of this tree

- **The exported window is short.** `cmd/quantos export-features` caps at 5,000
  bars per symbol, so `-days 45` yields roughly 3.5 days of one-minute bars.
  Those are now the *most recent* 3.5 days: `Sim.GetHistoricalBars` anchors a
  limit-truncated window to the **end** of the requested range, so the shipped
  model is fitted on 2026-03-07 to 2026-03-09 and the platform serves from
  2026-03-10. The covariate gap earlier versions of this file described is
  closed — measured, not assumed: across 100 live prediction snapshots, **no
  feature is clamped at ±6σ** where the old January-anchored export clamped
  routinely. It is still only 3.5 days.
- **`features.warmup_bars` is 260**, raised from 50 because the feature set
  contains a 200-period average. Any `features.csv` exported before that change
  has its slowest features computed from too little history and should be
  re-exported rather than trusted; `make train` re-exports first, so the normal
  path is safe.
- **No directional skill.** The fitted model separates *moving* from *quiet*, not
  *up* from *down*: its UP and UP-minus-DOWN coefficient rows are nearly
  identical (‖UP−DOWN‖ = 0.15 against ‖UP−FLAT‖ = 0.44) and the directional AUC
  is 0.495 on test, 0.491 on validation. This has a direct operational
  consequence for the platform — see the model card's integration note.
- **Only two regimes appear in the data** (`RANGE`, `LOW_VOLATILITY`). Nothing
  here has been measured in a trending or high-volatility market. See the model
  card's failure modes.
- **1d horizon does not fit.** A 2x embargo on a 24-hour horizon is 48 hours,
  which exceeds the exported span. `--horizon 1d` fails loudly rather than
  silently shrinking the embargo.

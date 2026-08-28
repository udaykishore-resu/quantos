# Model card — `direction-3class`

| | |
|---|---|
| **Model id** | `direction-3class` |
| **Version** | `v1.000339040.e6a8254a` |
| **Artifact** | `ml/artifacts/direction-3class-v1.000339040.e6a8254a.json` |
| **sha256** | `8cf7664c0c579e7ad9b344b2532c3c942c7acd04bf316fdd3c86aa70d192db3b` (the file digest covers `metadata.trained_at`, so it moves when the trainer is re-run; the fitted parameters do not — see "Reproducing this card") |
| **Family** | `multinomial_logistic` (3-class softmax regression on 16 standardised features) |
| **Horizon** | 60 minutes |
| **Generator** | `quantos/ml/training/train.py` |
| **Trained** | 2026-08-28 |

> **This is educational research software.** It runs on *simulated* market
> history produced by the platform's own market simulator. It makes no
> guarantees of any kind, its output is probabilistic and frequently wrong, it
> is not financial advice or a recommendation to transact, and it does not and
> cannot execute real-money trades. Every prediction the platform emits carries
> `domain.Disclaimer` on the record itself for the same reason.

---

## What it predicts

Given a `domain.FeatureSnapshot` for one instrument at one instant, the model
returns a probability distribution over three outcomes for the price 60 minutes
later:

| class | definition |
|---|---|
| **UP** | forward return > **+15 bps** |
| **FLAT** | forward return within **±15 bps** |
| **DOWN** | forward return < **−15 bps** |

**The FLAT band is the whole meaning of the output.** "UP: 0.63" is not a
statement until you know what counts as up. The band is ±15 bps
(`predict.flat_band_bps` in `config/quantos.yaml`), it is stamped on the
artifact as `flat_band_bps`, and the trainer refuses to run if the labels in the
CSV are inconsistent with the declared value rather than taking the flag on
trust.

The model does **not** predict a price, a return, or a trade. It is one input to
`internal/predict`, which blends it with a deterministic rule prior, rebands it
against the volatility-implied move for the horizon, and hands the result to a
risk engine that holds an unconditional veto.

## Inputs

The 16 features of `domain.ModelFeatureSet`, in that exact order, computed by
the **Go** feature engine and exported by `cmd/quantos export-features`:

`ret_log_1`, `ret_log_5`, `momentum_10`, `momentum_60`, `rsi_14`, `macd_hist`,
`vwap_dist_bps`, `bb_percent_b`, `adx_14`, `rel_strength_20`,
`volume_ratio_20`, `volume_z_20`, `atr_pct`, `realized_vol_20`, `zscore_20`,
`trend_slope_20`

Training consumes features produced by the same code that will serve them,
which is the training/serving-skew guard from ADR-001 made concrete. Each is
standardised with the mean and standard deviation stored in the artifact, then
clamped to ±6 standard deviations — the clamp is part of the model, applied
identically at fit time and at serve time, so a bad tick cannot be extrapolated
far outside the training distribution.

## Training window and sample count

| | |
|---|---|
| **Source** | `ml/data/features.csv`, sha256 `e6a8254ae6bb…` |
| **Rows exported** | 364,000 (104 symbols, 1-minute bars) |
| **Span** | 2026-03-07T09:41Z → 2026-03-09T20:00Z (**~2.4 days**) |
| **Samples used** | **339,040** after the embargo |
| **Train** | 218,504 rows, 2026-03-07T09:41Z → 2026-03-08T20:41Z |
| **Validation** | 60,320 rows, 2026-03-08T22:42Z → 2026-03-09T08:21Z |
| **Test** | 60,216 rows, 2026-03-09T10:22Z → 2026-03-09T20:00Z |
| **Embargo** | 120 minutes (2× horizon) between each pair; 24,960 rows discarded |
| **Regimes present** | `RANGE` (97.9%), `LOW_VOLATILITY` (2.1%) |

The window is short because `export-features` caps at 5,000 bars per symbol; a
`-days 45` request yields ~2.4 days of it. Those are now the **most recent** 2.4
days of the requested range — `Sim.GetHistoricalBars` anchors a limit-truncated
window to its end — so the training window sits immediately before the
2026-03-10 date the platform serves from. Earlier versions of this card recorded
a January window and a two-month covariate gap; both are gone, and this artifact
was retrained from a fresh export rather than having its dates edited.

**Two and a half days of simulated one-minute bars is still not enough history to
establish a market edge**, and no claim on this page should be read as though it
were.

Splits are chronological with an embargo of twice the horizon, never random.
The standardiser was fitted on training rows only; the calibration temperature
on validation rows only; the test split was read exactly once.

## Measured performance

### Held-out test split — the numbers, next to the bar

| metric | value | reference |
|---|---:|---|
| samples | 60,216 | |
| **accuracy** | **0.4190** | **base rate 0.3923** (always predict DOWN) |
| **lift over base rate** | **+0.0268** | 0.0000 is no edge |
| train accuracy | 0.4233 | close to test: not overfitted |
| Brier | 0.6290 | 0 perfect, 2 worst |
| log loss | 1.0349 | uniform 1.0986, class prior **1.0669** |
| **expected calibration error** | **0.0143** | max bin deviation 0.0357 |
| calibration temperature | 1.0333 | fitted on validation; ~1 means the raw softmax was already close |

Class balance in the test split: UP 0.3889, FLAT 0.2189, DOWN 0.3923.

### The report card the artifact carries

Four of those numbers are not merely reported here — they are written into the
artifact, and the runtime refuses to load it without them:

```json
"metadata": {
  "test_samples":   60216,
  "test_accuracy":  0.419041,
  "test_base_rate": 0.392288,
  "test_brier":     0.629036
},
"calibration": { "ece": 0.001329, "max_deviation": 0.002504 }
```

`mlinfer.Artifact.Validate` rejects an artifact that omits `test_samples`,
`test_accuracy` or `test_base_rate`, or whose accuracy and base rate are not
proportions in `(0, 1]`. There is no partial credit: an artifact without its own
held-out numbers does not load, the registry has nothing to serve, and every
prediction falls back to the deterministic rule prior. The accuracy and the base
rate are required *together* because neither is a measurement without the other.

The two calibration figures come from the **validation** split, since they
describe the temperature that was fitted there; the four `test_*` figures come
from the test split and nowhere else.

### What the runtime does with it

`mlinfer.Artifact.ProvisionalHealth` reads the report card at load time and
turns it into a health record, so a freshly promoted model can serve before any
live prediction has resolved. It marks the model **unhealthy** — after which the
risk engine blocks it rather than serving it — when either:

| condition | why it is fatal |
|---|---|
| `test_accuracy <= test_base_rate` | the model is no better than a constant guess, so its argmax is decoration |
| `calibration.max_deviation > 0.10` | its stated confidence is not its empirical accuracy, and `risk.MinConfidence` and every strategy policy threshold on that confidence |

**This artifact is marked healthy**: 0.4190 > 0.3923, and its maximum validation
calibration deviation is 0.0025, far inside the 0.10 gate. The record is flagged
`Provisional: true` with `Samples: 0`; the first live evaluation sweep replaces
it with evidence from resolved predictions.

Being marked healthy is a statement that the model cleared two floors, not that
it is good. Read the next two sections before treating it as either.

### Walk-forward — the number that should be believed

A single split answers "did this work on one particular day". Five anchored
folds with the same 120-minute embargo, each refitting the standardiser,
coefficients and temperature from scratch:

| fold | test rows | accuracy | base rate | lift |
|---:|---:|---:|---:|---:|
| 1 | 38,480 | 0.4128 | 0.3989 | +0.0138 |
| 2 | 38,480 | 0.4097 | 0.4007 | +0.0091 |
| 3 | 38,480 | 0.4044 | 0.4215 | **−0.0170** |
| 4 | 38,480 | 0.4316 | 0.4117 | +0.0198 |
| 5 | 38,480 | 0.4133 | 0.4050 | +0.0082 |

**Mean lift +0.0068, fold-to-fold standard deviation 0.0141. Four of five folds
beat their own base rate; one did not.**

> **Verdict: the dispersion across folds is roughly twice the mean, which is
> consistent with no edge at all.** The single-split figure of +0.0268 is the
> optimistic end of that range, not a better measurement of the same thing. This
> is a working pipeline with an honestly calibrated model attached. It is not
> evidence of a tradeable edge, and it should not be described as one.
>
> Note what this implies about `ProvisionalHealth`. The gate asks whether the
> model beat its base rate on *one* held-out split, and this model did. Fold 3
> did not. A single split is the only evidence available at load time, so the
> gate is a floor against serving something demonstrably worthless, not a
> promotion criterion — the walk-forward number is the one to believe.

Pooled over all folds: accuracy 0.4144 against a base rate 0.4019 (192,400
rows), Brier 0.6296, log loss 1.0358, ECE 0.0093.

### What the lift is actually made of

The accuracy lift comes almost entirely from separating **FLAT from
directional**, not from calling direction. Ranking the test rows by the model's
implied `P(up) / (P(up) + P(down))` and measuring the realised up-share among
directional outcomes gives an **AUC of 0.495** — indistinguishable from a coin
flip. It is 0.491 on validation, and the gradient-boosted-stumps family fitted
on the same data gives 0.492 on validation (0.514 on test, which is what
selecting a family on the test split would buy you, and is the reason this tree
does not).

The fitted coefficients say the same thing more directly. The UP and DOWN rows
are nearly the same vector — ‖UP − DOWN‖ = 0.151 against ‖UP − FLAT‖ = 0.442 —
and the two largest weights by magnitude are `atr_pct` and `realized_vol_20`,
both pure volatility features that carry no sign. The in-sample directional AUC
of 0.520 falls to 0.491 on validation, so even that small tilt is fitting noise.

Stated plainly: **the model has no measurable ability to call direction on this
data.** It has a small, real ability to tell a quiet hour from a moving one,
which is what its calibrated probabilities are reporting. The integration note at
the foot of this page is the operational consequence.

### Per regime

| regime | n | accuracy | base rate | lift |
|---|---:|---:|---:|---:|
| RANGE | 58,968 | 0.4190 | 0.3938 | +0.0253 |
| LOW_VOLATILITY | 1,248 | 0.4191 | 0.4567 | **−0.0377** |

It is worse than a constant guess in the low-volatility regime, on a small
sample. No trending or high-volatility regime appears in the training data at
all, so its behaviour there is unmeasured rather than good.

## Calibration

Temperature scaling, fitted on the **validation** split by minimising negative
log likelihood with a deterministic golden-section search. Go applies it in
probability space as `p^(1/T)` renormalised (`domain.Distribution.Sharpen`),
which is algebraically identical to dividing the logits by T.

The fitted temperature is 1.0333 — the uncalibrated model was already close to
calibrated — and validation ECE improved from 0.0031 to 0.0013. Test ECE is
0.0143 with a maximum bin deviation of 0.0357.

The **validation** figures are the ones written to the artifact
(`calibration.ece` 0.001329, `calibration.max_deviation` 0.002504), because they
describe the split the temperature was fitted on. `ProvisionalHealth` reads
`max_deviation` and blocks the model above 0.10; this one is forty times inside
that gate.

This matters operationally: `risk.checkConfidence` thresholds on the model's
stated confidence, and `signal.Engine` thresholds on its directional edge. An
uncalibrated classifier's "probability" is not a probability, and both gates
would then be thresholding on a number that means nothing. Being well calibrated
is not the same as being useful — this model is honestly calibrated *and* has no
directional skill, and the two facts are independent.

## Known failure modes

1. **No directional skill.** AUC ≈ 0.50 for up-vs-down (above). Treat the argmax
   as noise; only the FLAT-vs-directional split and the calibration carry
   information.
2. **Regime coverage is two of eight.** Trained entirely in `RANGE` and
   `LOW_VOLATILITY`. `BULL_TREND`, `BEAR_TREND`, `HIGH_VOLATILITY`, `BREAKOUT`,
   `REVERSAL` and `RISK_OFF` are **unmeasured**. Demo scenario 8 shows exactly
   this: under an injected regime shift the drift detector reports a feature PSI
   above 6 across 34 features and flags the model unhealthy. That is the
   detector working, and the correct response is retraining, not overriding it.
3. **Covariate shift between training and serving — now small, not zero.** This
   used to be the headline failure mode: the exported window ended 2026-01-27
   while the platform served from 2026-03-10, and serving features routinely
   exceeded anything in the training sample. `Sim.GetHistoricalBars` now anchors
   a limit-truncated window to its end, so the training window ends 2026-03-09
   and the gap is closed. Measured on 100 live prediction snapshots taken from a
   running instance, **no feature is clamped at ±6σ**. Injected demo scenarios
   still reach far outside it — `vwap_dist_bps` hits 3,277 bps for NVDA under the
   breakout injection against a training range of [−716, +1019] — so the clamp
   still does real work under stress, and predictions in those conditions remain
   extrapolations the clamp merely bounds.
4. **Two and a half days is not a sample.** Every number here has a standard
   error that the short window makes larger than it looks, and the folds are not
   independent: 104 symbols moving in the same simulated market are far fewer
   than 364,000 independent observations.
5. **Simulated data.** The market simulator is not the market. Nothing measured
   here transfers to real instruments, and it has never been tested against
   real market data.
6. **Underconfident by construction, and correctly so.** The maximum probability
   the model assigns any class across all 364,000 rows is 0.524, and the median
   is 0.412. `domain.Distribution.Confidence()` rescales the top probability as
   `(p − ⅓) / (⅔)`, so clearing the 0.30 minimum that `risk.MinConfidence` and
   every strategy policy require needs a top probability of 0.533 — which this
   model never once reaches. **With this model active the platform emits no
   paper signals.** That is the correct behaviour for a model with no
   directional skill. The integration note below traces the mechanism.

## Intended use, and use that is out of scope

**Intended:** one probabilistic input to `internal/predict`, blended with a
deterministic rule prior and subject to the risk engine's veto, inside an
educational research and paper-trading platform.

**Out of scope:** any real-money decision; use as a standalone forecast; use on
real market data; use in any regime not listed above; use without the base rate
quoted beside the accuracy.

## Reproducing this card

```bash
make export-features
python3 ml/training/train.py --data ml/data/features.csv --out ml/artifacts \
    --model-id direction-3class --horizon 60m
python3 ml/evaluation/walk_forward.py --data ml/data/features.csv --out ml/reports
python3 -m pytest ml/tests -q
```

The trainer is deterministic: the same CSV produces a byte-identical artifact
apart from `metadata.trained_at`. The full walk-forward report, including
reliability tables and confusion matrices, is at
`ml/reports/walk-forward-60m-logistic.{json,md}`.

## Integration note: why the demo drops to 9/10 with this artifact serving

**The bootstrap deadlock this section used to describe is fixed.** Earlier
versions of this card reported that `risk.checkModelHealth` returned a WARN
whenever a model served with no health record, that the WARN forced
`WATCH_ONLY`, and that no health record could ever exist because none could be
emitted. `mlinfer.Artifact.ProvisionalHealth` resolves that: the artifact's own
offline report card becomes a provisional health record at load time, so a newly
deployed model starts with evidence instead of with a hole. The start-up log now
reads:

```
model serving on its offline evaluation
  model=direction-3class@v1.000339040.e6a8254a accuracy=0.419041 base_rate=0.392288 healthy=true
```

With the deadlock gone, a different and entirely real cause is now visible.
`go run ./cmd/quantos demo` reports **9 passed, 1 failed** with this artifact in
`ml/artifacts` and **10 passed** with the directory empty. Scenario 6 ("a
signal's premise breaks and it is invalidated automatically") fails with *"no
active signal was available to invalidate"*. This time the fault is the model,
not the platform:

1. `predict.Engine` blends the model into the rule prior at
   `predict.blend_weight: 0.6` — `base = prior.Blend(model, 0.6)`.
2. The rule prior carries a real directional tilt. The model does not: its
   `P(up) / (P(up) + P(down))` sits between 0.469 and 0.545 from the 1st to the
   99th percentile, and at serve time it agrees with the prior's direction in
   **43 of 100** snapshots — noise, as the 0.495 AUC above already said.
3. Blending a zero-tilt distribution at weight `w` multiplies the prior's tilt
   by `1 − w`. Measured across 100 live predictions, the mean directional tilt
   falls from 0.0505 to 0.0201 — **0.40×**, exactly `1 − 0.6`.
4. `rebandDistribution` then hands FLAT the mass the band geometry implies and
   splits the rest by that diluted ratio, so the diluted tilt is what reaches
   `Confidence()`.
5. Confidence lands at 0.19–0.24 for the instruments the strategies watch,
   against the 0.30 minimum. The demo records it verbatim: *"no signal for AAPL
   — recorded reason: confidence: model confidence 0.20 is below the strategy
   minimum of 0.30"*. With no artifact, AAPL's 15m P(up) is 0.628 at confidence
   0.44 and the platform holds 50 active signals; with it, 0.496 at 0.24 and
   zero.

**This was not fixed by adjusting the artifact, and should not be.** The
adjustments available — sharpening the calibration temperature away from the
value fitted on validation, or scaling up the UP-minus-DOWN coefficient row —
would raise confidence only by making the model assert a direction it has been
measured not to know. That is precisely the "confident nonsense" `Validate` is
strict to prevent, and it would push a 0.495-AUC signal into a paper-trading
path. Retraining with the gradient-boosted-stumps family was tried and rejected
on validation: AUC 0.492 and a *lower* test accuracy of 0.4083.

For the platform team, the honest options are all outside `ml/`:

- **Lower `predict.blend_weight`** so a skill-less model dilutes the prior less.
  At `w = 0.6` the prior keeps 40% of its tilt; the weight is a statement about
  how much the model is trusted, and 0.6 is not currently earned.
- **Gate the blend on health.** `predict.Engine` selects from the registry
  without consulting `ProvisionalHealth`, so an unhealthy model is blended in at
  full weight and only *afterwards* blocked by risk. Weighting the blend by model
  health would let a model earn its weight instead of being granted it.
- **Accept it.** Emitting no paper signals is the correct response to a model
  with no directional skill, and demo scenario 6 asserting that a signal exists
  is arguably the thing that is wrong.

The demo failure is therefore reported, not papered over. Evidence for every
number above is reproducible from `ml/artifacts`, a running instance's
`/api/v1/predictions`, and `go run ./cmd/quantos demo`.

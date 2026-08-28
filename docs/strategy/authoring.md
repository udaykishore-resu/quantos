# Writing a strategy

A QuantOS strategy is a YAML file. No strategy logic lives in Go source: a rule
is a named conjunction of typed conditions over feature names, and the engine is
a pure evaluator. That is what makes a strategy reviewable by someone who does
not read Go, versionable in git, and diffable in a pull request.

Files live in `strategies/` and are loaded in sorted filename order so the
registry is deterministic. Validation failures are **fatal at startup**: a
malformed strategy that silently degraded to "no rules fire" would be far worse
than a startup error, because a platform producing nothing looks healthy doing
it.

This platform is educational research and paper trading. A strategy produces
research classifications and simulated positions. It never produces advice, and
nothing it emits reaches a broker.

---

## The shape of a file

```yaml
id: momentum                 # required, unique across the directory
name: Trend Momentum         # required
version: "1.2.0"             # required — see below
description: >               # what the thesis is, and why it is restricted
  ...
enabled: true
interval: 1m                 # bar size the rules are written against

regimes:                     # optional: gate the whole strategy
  - BULL_TREND
  - BREAKOUT

rules:                       # required, at least one
  - id: ...

weights:                     # required: the ten composite-score weights
  ...

policy:                      # required: the gates before a signal is emitted
  ...

invalidation:                # required, at least one
  - kind: ...

fundamental:                 # optional: what "quality" means for this strategy
  ...
```

`version` is required and the validator says why: *a strategy without a version
cannot be attributed to a historical signal.* Six months from now, looking at a
signal this file produced, `strategy_id` alone will not tell you which rules ran.
Bump it whenever you change behaviour.

Alongside the version, the loader records a content `hash` of the file and its
`modified_at` — the file's own modification time, not the time it was read. A
load timestamp changes on every restart while saying nothing about which rules
ran. Both are stamped on every signal.

---

## Rules

```yaml
- id: trend_is_directional          # required, unique within the strategy
  description: ADX confirms a directional market rather than chop
  side: LONG                        # LONG or SHORT — nothing else
  weight: 2.0                       # must be positive
  regimes: [BULL_TREND]             # optional, per-rule gate
  counter: false                    # optional
  enabled: true                     # optional, defaults to true
  all:                              # every condition must hold
    - feature: adx_14
      op: gte
      value: 22
    - feature: plus_di_14
      op: gt
      other: minus_di_14
```

A rule has up to three condition blocks and they compose:

- `all` — every condition must hold.
- `any` — at least one must hold.
- `none` — no condition may hold.

At least one block must be present. All three may be, and they are evaluated in
that order.

`description` is not a comment. It is the evidence string shown to users on a
signal, with the values that satisfied it appended:

```
"ADX is below the trending threshold, so the range premise holds (adx_14 below 20 is 15.02)"
```

Write it as a statement about the market, not a restatement of the condition.

**`weight` must be positive.** The validator rejects a negative one, and the
message says why: *direction comes from side, not sign.* A rule that argues for
the short side has `side: SHORT` and a positive weight. A rule that argues
against its own side is a counter-rule, below. Encoding direction in the sign of
a weight would make those three cases indistinguishable.

Rules are scored as a fraction of the strategy's *eligible* weight — the total
weight of every rule that was allowed to run in the current regime. The net
directional score is `(long - short) / eligible × 100`, clamped to `[-100, 100]`,
and confidence is `fired / eligible`. Both are therefore bounded and meaningful:
they say how much of what could have fired did, which is a real quantity, unlike
a number invented to look like a probability.

---

## Every operator, with an example

The left-hand side is always `feature`. The right-hand side is either a literal
(`value`, plus `value2` for the two-bound operators) or another feature
(`other`, optionally scaled).

### `gt` — strictly above

```yaml
- feature: vwap_dist_bps
  op: gt
  value: 20                       # more than 20 bps above the session VWAP
```

### `gte` — at or above

```yaml
- feature: volume_ratio_20
  op: gte
  value: 2.0                      # volume at least twice its 20-bar average
```

### `lt` — strictly below

```yaml
- feature: rsi_14
  op: lt
  value: 28                       # oversold
```

### `lte` — at or below

```yaml
- feature: bb_percent_b
  op: lte
  value: 0.0                      # at or below the lower Bollinger band
```

### `between` — inclusive range

```yaml
- feature: rsi_14
  op: between
  value: 35
  value2: 62                      # recovered out of oversold, not yet overbought
```

Bounds are sorted, so order does not matter, but they must **differ** — the
validator rejects `between` or `outside` with two identical bounds, because a
zero-width range is either a mistake or an equality test written the long way.

### `outside` — inclusive range, negated

```yaml
- feature: zscore_20
  op: outside
  value: -2.0
  value2: 2.0                     # more than two sigma from the 20-bar mean, either way
```

### `cross_up` — was at or below, now above

```yaml
- feature: macd_hist
  op: cross_up
  value: 0                        # histogram turned positive on this bar
```

Crossovers need a prior snapshot. On the first bar for an instrument there is
none, and the condition reports the feature as unavailable rather than false —
see [cold and false](#cold-is-not-false) below. That is correct: on the first bar
we genuinely cannot tell whether a crossing occurred.

### `cross_down` — was at or above, now below

```yaml
- feature: last
  op: cross_down
  other: vwap                     # price closed back below the session VWAP
```

### `is_true` — the flag is set

```yaml
- feature: breakout_up
  op: is_true                     # a volume-confirmed upside breakout is in force
```

The boolean features (`breakout_up`, `breakout_down`) are carried as 0 or 1.
`is_true` means "not zero" and `is_false` means "exactly zero", so these
operators work on any numeric feature, though they are only meaningful on the
flags.

### `is_false` — the flag is not set

```yaml
- feature: breakout_down
  op: is_false                    # no downside breakout in progress
```

### Comparing two features, with `scale`

```yaml
- feature: plus_di_14
  op: gt
  other: minus_di_14
  scale: 1.15                     # +DI exceeds -DI by at least 15%
```

`scale` multiplies `other` before the comparison, defaulting to 1. It exists so
`last > 1.02 × sma_50` is expressible without inventing a feature for it. Both
features must be warm; if either is cold, the condition is unevaluable.

### `optional` and `describe`

```yaml
- feature: rel_strength_20
  op: gt
  value: 0
  optional: true                  # a cold value does not skip the rule
  describe: Outperforming the benchmark
```

`optional` changes what happens when a feature is cold: instead of making the
rule unevaluable, the condition is skipped and the rest of the rule is judged on
its own. Use it where a condition adds confirmation but its absence should not
silence the rule — `rel_strength_20` needs a warm benchmark, which is a
dependency outside this instrument's control.

`describe` overrides the generated evidence sentence for one condition. Use it
when the generated text ("`macd_hist` above 0 is 0.4213") reads worse than a
plain statement.

---

## Cold is not false

This is the distinction that matters most when a strategy is not producing what
you expect.

A feature is **cold** when its indicator has not seen enough bars for its value
to mean anything. A 200-period moving average is cold for the first 200 bars. The
snapshot carries the value anyway, alongside a per-feature `warm` flag, and a
cold value is never fed to a model and never satisfies a condition.

The rule engine treats "we could not tell" and "the condition was false" as
different outcomes, and they lead to different places:

| Situation | Rule fires? | Counts toward eligible weight? | Reported as |
|---|---|---|---|
| Condition evaluated, held | yes | yes | evidence |
| Condition evaluated, did not hold | no | **yes** | nothing |
| Feature cold or absent | no | **no** | `skipped` |
| Rule excluded by regime | no | **no** | `skipped` |

A rule whose feature is cold is **skipped**: it does not fire, and it does not
count against the strategy. Counting it as a failure would penalise a strategy
for a warm-up state it has no control over, and would make the confidence number
— fired weight over eligible weight — mean two different things depending on how
long the process had been running.

The three blocks handle coldness slightly differently, and the differences are
deliberate:

- **`all`** — one cold required condition makes the whole rule unevaluable. The
  rule is a conjunction, so a missing term means the conjunction is unknown.
- **`any`** — a cold condition is passed over. Only if *every* `any` condition is
  cold does the rule become unevaluable; if one is warm and false while another
  is cold, the rule is simply false. There is enough information to answer.
- **`none`** — a cold condition makes the rule unevaluable, unless marked
  `optional`. A `none` block asserts an absence, and you cannot assert the
  absence of something you cannot observe.

Everything skipped is reported. The evaluation result carries a `skipped` list
with a line per exclusion:

```
rule volume_zscore skipped: features unavailable or cold: volume_z_20
rule breakout_up_confirmed not applicable in regime RANGE
strategy breakout is not enabled in regime RANGE
```

That visibility is the whole point. A rule that silently never fires is the worst
failure mode a strategy definition has, which is also why an unknown feature name
is a **load-time error** rather than a rule that quietly evaluates to nothing.

### The warm-up trap

Warm-up (`features.warmup_bars`) must be at least as long as the slowest
indicator you reference — 200 bars for `sma_200`. A platform warmed for fewer
bars than its longest lookback runs with that indicator permanently cold: every
rule referencing it is skipped for ever, the strategies depending on those rules
never fire, and nothing reports a problem. The platform simply produces nothing
and looks healthy doing it. The shipped configuration warms 260 bars for exactly
this reason.

---

## Regime gating, at two levels

The same indicators mean different things in different environments. Mean
reversion in a strong trend is a way of losing money slowly and then quickly;
momentum rules in a range produce a stream of losing signals. Gating is how the
platform avoids applying indicators blindly.

**Strategy level** — `regimes:` at the top of the file. Outside the listed
regimes the strategy is not evaluated at all, and the result records why:

```yaml
regimes:
  - RANGE
  - LOW_VOLATILITY
```

```
strategy mean_reversion is not enabled in regime BULL_TREND
```

**Rule level** — `regimes:` on an individual rule. The strategy runs; that one
rule does not, and its weight is excluded from the eligible total, so the
remaining rules are scored against what could actually have fired:

```yaml
- id: momentum_continuation
  side: LONG
  weight: 2.0
  regimes: [BULL_TREND, BREAKOUT]
  all:
    - feature: momentum_60
      op: gt
      value: 0
```

An empty or absent `regimes` list means regime-agnostic. That is a legitimate
choice, not an oversight — `quality_growth` ships without one, because quality is
a slow-moving property and gating it on an intraday regime would add noise rather
than remove it. Say so in the description when you make that choice.

The valid regimes are `BULL_TREND`, `BEAR_TREND`, `RANGE`, `HIGH_VOLATILITY`,
`LOW_VOLATILITY`, `BREAKOUT`, `REVERSAL`, `RISK_ON` and `RISK_OFF`. An unknown
name is a load-time error. `UNDEFINED` is not accepted in a gate: it is the
unclassified state, not an environment to trade in.

---

## Counter-evidence rules, and why they exist

A rule marked `counter: true` argues **against** the strategy's thesis. When it
fires it subtracts its weight from the side it opposes, and it is surfaced
separately in the explanation as counter-evidence rather than mixed in with the
supporting evidence.

```yaml
- id: resistance_overhead
  description: A resistance level sits within thirty basis points overhead
  side: LONG                      # the side it argues AGAINST
  weight: 1.0
  counter: true
  all:
    - feature: dist_resistance_bps
      op: lt
      value: 30
    - feature: dist_resistance_bps
      op: gt
      value: 0
```

Note `side: LONG` on a rule that opposes a long setup. `side` names the thesis
the rule bears on; `counter` says which way it bears.

They exist for two reasons, and the second is the important one.

**Mechanically**, they stop a strategy scoring highly on a setup it should be
wary of. Seven momentum rules firing on a name that is stretched into overbought
territory with resistance directly overhead is not the same setup as seven firing
on a clean breakout, and a score that cannot tell them apart is a score that will
mislead you at exactly the wrong moment.

**Presentationally**, they are what makes a signal evaluable by a human. A system
that shows only what agrees with itself is a system nobody can check. Counter-
evidence appears on the signal in `counter_evidence`, on the instrument view, and
beside the reasons in the brief — deliberately not in a separate section a reader
could skip. If you cannot name what would make your thesis wrong, you do not yet
have a thesis, and the file should not pass review.

A counter rule does not count toward `fired_weight`, so it lowers the score
without inflating the confidence. That asymmetry is intentional: finding a reason
to doubt is not evidence that the strategy is working.

Every shipped strategy carries two to three of them. `value` flags an active
downtrend, which is the situation a contrarian value screen most needs to be
warned about; `breakout` flags RSI exhaustion and a large gap open, because a
breakout that is already extended is the one most likely to fail immediately.

---

## Weights

Ten non-negative numbers setting the relative contribution of each component of
the composite stock score:

```yaml
weights:
  fundamental: 0.5      # balance-sheet and income-statement health
  growth: 0.5           # revenue and earnings expansion
  quality: 0.5          # margin durability, returns on capital, consistency
  valuation: 0.25       # cheapness against own history and sector
  technical: 2.0        # price structure
  momentum: 2.0         # persistence of direction
  sector: 0.75          # the sector's standing in the current rotation
  market_regime: 1.5    # how well the setup fits the environment
  risk: 1.0             # inverse of risk severity — higher is safer
  event_risk: 1.0       # inverse of scheduled-event proximity — higher is safer
```

They need not sum to anything. They are renormalised, and — this is the part
worth knowing — they are renormalised **again** when a component cannot be
computed. An ETF has no fundamentals; rather than scoring it zero on
`fundamental`, that component is dropped, listed in the score's `missing` array,
and the remaining weights are rescaled. Treating "unknown" as zero would drag the
composite down for a reason that does not exist.

The validator requires every weight to be non-negative and at least one to be
positive. Beyond that the shape is yours: `quality_growth` weights fundamentals
at 2.5 and technicals at 1.0; `breakout` inverts that almost exactly. Those are
different theses, and the weights are where you say so.

---

## Policy — the gates before a signal

```yaml
policy:
  min_rule_score: 35          # net directional rule score, 0..100
  min_final_score: 60         # composite stock score
  min_confidence: 0.30        # prediction confidence, in [0,1]
  min_directional_edge: 0.10  # P(up) - P(down)
  min_risk_reward: 1.5        # target distance / stop distance
  horizon: 60m                # 15m | 60m | 1d | 5d
  stop_atr_multiple: 1.5      # must be positive
  target_atr_multiple: 2.5    # must be positive
  max_weight: 0.06            # fraction of simulated equity, in (0,1]
  cooldown_minutes: 30
  allow_short: false
```

Four of these need explaining.

**`min_confidence` is the max-probability margin, not `1 - uncertainty`.**
Confidence is `(max_p - 1/3) / (1 - 1/3)`: how far the most likely outcome sits
above a uniform guess. Normalised entropy is a poor threshold over three classes
— the distribution `0.63 / 0.21 / 0.16` has an entropy ratio of 0.83, so
`1 - uncertainty` would call it confidence 0.17 and fail every sensible gate,
while the margin gives 0.45. Calibrate your threshold against the margin. Shipped
values sit between 0.28 and 0.32.

**`min_directional_edge` is a separate gate for a reason.** It is
`P(up) - P(down)`, and it distinguishes distributions that confidence cannot:
`0.40 / 0.35 / 0.25` and `0.40 / 0.20 / 0.40` have the same argmax and similar
confidence, but the first has a real directional lean and the second has none.

**Both ATR multiples must be positive.** The validator says why for the stop: *a
signal without a stop reference has no risk/reward.* Without both, the
`min_risk_reward` gate has nothing to compute and `stop_reference` on the emitted
signal would be meaningless.

**`allow_short: false` does not suppress short conclusions silently.** When the
rules conclude SHORT and the policy forbids it, the outcome is recorded as a
rejection you can read on the ranked list:

```
direction: strategy mean_reversion does not permit short signals
```

That matters. "Produced nothing" and "declined, for this reason" look identical
from outside and mean entirely different things.

Nothing in `policy` can override the risk engine. Clearing every gate here makes
a signal a *candidate*; the risk engine's eleven checks decide whether it exists,
and its veto is unconditional.

---

## Invalidation templates

At least one is required — the validator cites the governance rule. A signal that
cannot be proven wrong is not a research artifact.

```yaml
invalidation:
  - kind: PRICE_BELOW
    ref: entry
    multiple: 1.5
    description: Price closes below the entry reference less 1.5 ATR
  - kind: VWAP_CROSS
    ref: vwap
    description: Price closes below the session VWAP, removing the intraday trend premise
  - kind: REGIME_CHANGE
    description: The market regime leaves the trending or breakout set the strategy requires
  - kind: VOLATILITY_ABOVE
    ref: atr
    value: 0.9
    description: Realised volatility exceeds the level at which the model was calibrated
  - kind: CONFIDENCE_BELOW
    ref: confidence
    value: 0.22
    description: Model confidence falls below the level that justified the signal
  - kind: TIME_EXPIRY
    minutes: 240
    description: The signal horizon elapses without the thesis resolving
```

These are templates. The signal engine resolves them against live values at
emission time — `ref: entry` with `multiple: 1.5` becomes an absolute price 1.5
ATRs below the entry reference, and that resolved number is what appears on the
signal.

| `kind` | Fields | Fires when |
|---|---|---|
| `PRICE_BELOW` | `ref`, `multiple` or `value` | Price falls through the resolved level |
| `PRICE_ABOVE` | `ref`, `multiple` or `value` | Price rises through it |
| `VWAP_CROSS` | `ref: vwap` | Price closes on the wrong side of the session VWAP |
| `REGIME_CHANGE` | — | The regime leaves the set the strategy requires |
| `VOLATILITY_ABOVE` | `ref: atr`, `value` | Realised volatility exceeds the level the thesis assumed |
| `CONFIDENCE_BELOW` | `ref: confidence`, `value` | The prediction's confidence drops below the threshold |
| `MATERIAL_NEWS` | — | A high-materiality negative news event arrives |
| `RISK_VETO` | — | The risk engine withdraws its allowance |
| `TIME_EXPIRY` | `minutes` | The window elapses without resolution |

`description` is **required on every template** and the validator says why: *it is
shown to users.* It is the sentence a reader sees when the signal ends, so write
what would have to happen for the thesis to be wrong, not what the field does.

Invalidation is checked two ways. The bar handler re-evaluates when the
instrument prints; the sweeper re-evaluates on its own interval, because a signal
must also be invalidated by the passage of time and by a regime change, not only
by its own instrument trading.

---

## Every valid feature name

These are the thirty-nine names a condition may reference. Anything else is a
load-time error naming the offending condition. All are computed from the
strategy's `interval` bars.

### Price and moving averages

| Name | What it measures | Warm after |
|---|---|---|
| `last` | The bar's close | immediately |
| `sma_20` | 20-bar simple moving average | 20 bars |
| `sma_50` | 50-bar simple moving average | 50 bars |
| `sma_200` | 200-bar simple moving average | 200 bars |
| `ema_9` | 9-bar exponential moving average | 9 bars |
| `ema_21` | 21-bar exponential moving average | 21 bars |
| `vwap` | Session volume-weighted average price, reset daily | first session |
| `vwap_dist_bps` | Distance from price to VWAP, in basis points. Signed: negative is below | with `vwap` |

### Oscillators and trend

| Name | What it measures | Warm after |
|---|---|---|
| `rsi_14` | 14-bar relative strength index, 0..100 | 14 bars |
| `macd` | MACD line — the fast minus slow EMA | 26 bars |
| `macd_signal` | The MACD line's own EMA | 26 bars |
| `macd_hist` | MACD minus signal, **normalised by price into basis points** so a $900 stock and a $20 stock are the same unit | 26 bars |
| `adx_14` | Average directional index. Above ~25 is a directional market, below ~20 is chop | 14 bars |
| `plus_di_14` | Positive directional indicator — upward pressure | 14 bars |
| `minus_di_14` | Negative directional indicator — downward pressure | 14 bars |
| `trend_slope_20` | Linear-regression slope of the last 20 closes, normalised by price into basis points, so it is unit-free | 20 bars |

### Volatility and bands

| Name | What it measures | Warm after |
|---|---|---|
| `atr_14` | 14-bar average true range, in price units | 14 bars |
| `atr_pct` | ATR as a fraction of price — the cross-instrument-comparable form | 14 bars |
| `bb_upper` | Upper Bollinger band | 20 bars |
| `bb_lower` | Lower Bollinger band | 20 bars |
| `bb_width` | Band width as a fraction of the middle band. Compression precedes expansion | 20 bars |
| `bb_percent_b` | Where price sits across the bands: 0 at the lower, 1 at the upper, outside those when price is beyond them | 20 bars |
| `realized_vol_20` | Annualised realised volatility from 20 bars of log returns | 20 bars |
| `vol_of_vol_20` | Volatility of that volatility — how unstable the volatility regime itself is | 40 bars |

### Returns, momentum and relative strength

| Name | What it measures | Warm after |
|---|---|---|
| `ret_log_1` | Log return of the last bar | 1 bar |
| `ret_log_5` | Cumulative log return over 5 bars | 5 bars |
| `momentum_10` | Fractional price change over 10 bars | 11 bars |
| `momentum_60` | Fractional price change over 60 bars | 61 bars |
| `zscore_20` | Standard deviations of price from its own 20-bar mean | 20 bars |
| `rel_strength_20` | 20-bar log return minus the benchmark's over the same window. Positive is outperformance | 20 bars **and** a warm benchmark |

### Volume

| Name | What it measures | Warm after |
|---|---|---|
| `volume_ratio_20` | Bar volume over its 20-bar mean. 2.0 is twice normal participation | 20 bars |
| `volume_z_20` | Bar volume as a z-score against its recent mean and standard deviation | short window full |

### Levels and structure

| Name | What it measures | Warm after |
|---|---|---|
| `support` | Nearest support below price, from fractal pivots, falling back to the rolling 20-bar low | a pivot, or 20 bars |
| `resistance` | Nearest resistance above price, same construction | a pivot, or 20 bars |
| `dist_support_bps` | Distance down to `support`, in basis points | with `support` |
| `dist_resistance_bps` | Distance up to `resistance`, in basis points | with `resistance` |
| `breakout_up` | 1 when the close is beyond the prior 20-bar high **on volume above 1.5× its average**. The volume requirement is what separates a breakout from a wick | 21 bars |
| `breakout_down` | 1 when the close is beyond the prior 20-bar low on the same volume confirmation | 21 bars |
| `gap_pct` | This bar's open against the previous close, as a fraction | 2 bars |

Two notes that will save you a debugging session. Anything ending `_bps` is in
basis points, so `20` means 0.2%, not 20%. And any feature that computes to NaN
or infinity is written as `0` and marked **cold**, so a division by zero upstream
becomes a skipped rule rather than a rule that fires on a garbage value.

---

## A complete worked example

`strategies/pullback.yaml` — buying a pullback within an established uptrend.
The thesis is that a trend that pulls back to its own 20-bar mean without
breaking structure is more likely to resume than to reverse, and the strategy is
restricted to trending regimes because in a range the same rules are just buying
noise.

```yaml
id: pullback
name: Trend Pullback
version: "1.0.0"
description: >
  Buys a pullback inside an established uptrend, and only where the trend
  structure is still intact. The discriminating condition is that price has come
  back toward the 20-period mean while remaining above the 50-period one: a
  pullback that has broken the slower average is not a pullback, it is the start
  of something else. Restricted to trending regimes, because in a range every
  move toward the mean satisfies these rules and the strategy becomes a
  random-entry generator.
enabled: true
interval: 1m

regimes:
  - BULL_TREND
  - RISK_ON

rules:
  - id: structure_intact
    description: Price is above the 50 period average, so the larger trend still holds
    side: LONG
    weight: 2.5
    all:
      - feature: last
        op: gt
        other: sma_50

  - id: pulled_back_to_mean
    description: Price has come back into the lower half of its Bollinger range
    side: LONG
    weight: 2.0
    all:
      - feature: bb_percent_b
        op: between
        value: 0.15
        value2: 0.45

  - id: trend_still_directional
    description: ADX confirms the market is still trending rather than chopping
    side: LONG
    weight: 2.0
    all:
      - feature: adx_14
        op: gte
        value: 22
      - feature: plus_di_14
        op: gt
        other: minus_di_14

  - id: not_oversold_breakdown
    description: RSI has softened without collapsing, consistent with a pause rather than a reversal
    side: LONG
    weight: 1.5
    all:
      - feature: rsi_14
        op: between
        value: 38
        value2: 58

  - id: momentum_still_positive
    description: Sixty period momentum remains positive through the pullback
    side: LONG
    weight: 1.5
    all:
      - feature: momentum_60
        op: gt
        value: 0

  - id: turning_back_up
    description: MACD histogram has crossed back above zero
    side: LONG
    weight: 1.5
    all:
      - feature: macd_hist
        op: cross_up
        value: 0

  - id: no_active_breakdown
    description: No volume-confirmed downside breakout is in progress
    side: LONG
    weight: 1.0
    none:
      - feature: breakout_down
        op: is_true

  - id: outperforming
    description: Still outperforming the benchmark over the last twenty bars
    side: LONG
    weight: 1.0
    all:
      - feature: rel_strength_20
        op: gt
        value: 0
        optional: true

  # ---- counter-evidence: what would make this thesis wrong
  - id: pullback_is_a_breakdown
    description: Price has lost the 200 period average, so this is a trend change rather than a pullback
    side: LONG
    weight: 2.5
    counter: true
    all:
      - feature: last
        op: lt
        other: sma_200

  - id: selling_on_volume
    description: The pullback is happening on heavy volume, which suggests distribution
    side: LONG
    weight: 2.0
    counter: true
    all:
      - feature: volume_ratio_20
        op: gt
        value: 1.8
      - feature: ret_log_1
        op: lt
        value: -0.002

  - id: volatility_expanding
    description: Realised volatility has expanded beyond the level the stop assumes
    side: LONG
    weight: 1.5
    counter: true
    all:
      - feature: realized_vol_20
        op: gt
        value: 0.55

weights:
  fundamental: 0.5
  growth: 0.5
  quality: 0.75
  valuation: 0.25
  technical: 2.25
  momentum: 1.75
  sector: 0.75
  market_regime: 1.5
  risk: 1.25
  event_risk: 1.0

policy:
  min_rule_score: 38
  min_final_score: 60
  min_confidence: 0.30
  min_directional_edge: 0.10
  min_risk_reward: 1.6
  horizon: 60m
  stop_atr_multiple: 1.4
  target_atr_multiple: 2.4
  max_weight: 0.05
  cooldown_minutes: 45
  allow_short: false

invalidation:
  - kind: PRICE_BELOW
    ref: entry
    multiple: 1.4
    description: Price closes below the entry reference less 1.4 ATR, meaning the pullback did not hold
  - kind: VWAP_CROSS
    ref: vwap
    description: Price closes back below the session VWAP, removing the intraday premise
  - kind: REGIME_CHANGE
    description: The market leaves the trending regimes this strategy requires
  - kind: VOLATILITY_ABOVE
    ref: atr
    value: 0.85
    description: Realised volatility exceeds the level the stop distance assumes
  - kind: CONFIDENCE_BELOW
    ref: confidence
    value: 0.22
    description: Model confidence falls below the level that justified the signal
  - kind: TIME_EXPIRY
    minutes: 180
    description: The trend does not resume within the expected window

fundamental:
  min_revenue_growth: 0.02
  min_operating_margin: 0.05
  max_debt_to_equity: 2.5
  prefer_low_valuation: false
```

Three things in there are worth pointing at.

`turning_back_up` uses `cross_up`, so it needs a prior snapshot. On an
instrument's first bar the rule is skipped rather than counted as false — which
is right, because on the first bar nobody can say whether a crossing occurred.

`outperforming` is marked `optional`. `rel_strength_20` depends on a warm
benchmark, which is outside this instrument's control, so a cold benchmark should
not silence a rule whose other conditions are fine.

The three counter rules are the answer to "what would make this wrong": the
pullback is actually a breakdown, the selling is distribution, or volatility has
outgrown the stop. Writing those down is not optional in this repository, and a
strategy that cannot name them should not merge.

---

## Validating

```bash
go run ./cmd/quantos validate
```

```
configuration OK   (hash 3a56228e6f3be51f, mode embedded, source config/quantos.yaml)
strategies OK      (5 loaded from strategies)
  breakout         v1.1.0     9 rules   6 invalidations  enabled=true   BREAKOUT, BULL_TREND, HIGH_VOLATILITY, RISK_ON
  mean_reversion   v1.1.0    11 rules   5 invalidations  enabled=true   RANGE, LOW_VOLATILITY
  momentum         v1.2.0    10 rules   6 invalidations  enabled=true   BULL_TREND, BREAKOUT, RISK_ON
  quality_growth   v1.0.0     8 rules   5 invalidations  enabled=true   any regime
  value            v1.0.0     8 rules   5 invalidations  enabled=true   RANGE, LOW_VOLATILITY, BULL_TREND, REVERSAL
universe OK        (115 instruments, 13 sectors)
```

Two flags:

```bash
go run ./cmd/quantos validate -strategies ./my-strategies
go run ./cmd/quantos validate -config config/quantos.compose.yaml
```

Every problem in the file is reported at once, each naming its exact location,
so you fix a file in one pass rather than one error per run:

```
quantos: strategies: strategy strategies/pullback.yaml: invalid strategy:
  - version is required: a strategy without a version cannot be attributed to a historical signal
  - rule structure_intact.all[0]: unknown feature "sma50" (see internal/domain/features.go for the produced set)
  - rule pulled_back_to_mean: weight must be positive (direction comes from side, not sign)
  - rule trend_still_directional.all[1]: unknown operator "greater"
  - invalidation[2]: description is required; it is shown to users
  - policy.stop_atr_multiple must be positive: a signal without a stop reference has no risk/reward
```

The full checklist the validator applies:

- `id`, `version` and at least one rule and one invalidation template are present.
- Rule ids are non-empty and unique within the strategy.
- Every rule has `side: LONG` or `side: SHORT`, a positive `weight`, and at least
  one condition across `all`, `any` and `none`.
- Every condition names a feature the engine produces, and a valid operator.
- `other`, where present, also names a produced feature.
- `between` and `outside` have two distinct bounds.
- Every regime named — on the strategy or on a rule — is one of the nine valid
  ones.
- Every invalidation template has a `kind` and a `description`.
- Score weights are non-negative and not all zero.
- `policy.min_confidence` is within `[0,1]`, both ATR multiples are positive, and
  `max_weight` is within `(0,1]`.

Duplicate strategy ids are caught when the registry loads them, not by
per-file validation, so run `validate` against the whole directory rather than
one file at a time.

### Then run it

Validation proves the file is well-formed. It does not tell you whether the
strategy does anything, and those are very different questions.

```bash
# Start the platform and watch what your rules do.
make run

# In another shell: the ranked list carries the rejection reason per instrument.
TOKEN=$(curl -sS -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"subject":"demo","password":"demo"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["token"])')

curl -sS "localhost:8080/api/v1/stocks?limit=10" -H "Authorization: Bearer $TOKEN" \
  | python3 -c '
import json,sys
for r in json.load(sys.stdin)["data"]:
    print("%-6s opp=%5.1f  %-18s %s" % (r["ticker"], r["opportunity"], r["risk_decision"], r.get("rejection","")))'
```

```
DIS    opp= 59.8  WATCH_ONLY         direction: strategy mean_reversion does not permit short signals
XLK    opp= 53.4  WATCH_ONLY         direction: no directional bias: edge is inside the neutral band
CSCO   opp= 52.7  WATCH_ONLY         rule_score: rule score 34.1 is below the strategy minimum of 40.0
AMGN   opp= 51.5  WATCH_ONLY         rule_score: rule score 25.0 is below the strategy minimum of 40.0
```

That `rejection` field is where a new strategy is actually debugged. It names the
stage that declined and the number that fell short, so "my strategy produces
nothing" becomes a specific, answerable question — the rules are not firing, or
they are firing and the composite score is short, or both clear and the risk
engine vetoed.

Then check it against history:

```bash
curl -sS -X POST localhost:8080/api/v1/backtests \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"pullback-check","strategy_id":"pullback",
       "start":"2026-08-01T00:00:00Z","end":"2026-08-27T00:00:00Z",
       "interval":"1m","seed":42,"starting_cash":100000}'
```

Read `regime_metrics` on the finished run before the headline `metrics`. A Sharpe
that survives only in the one regime the strategy is gated to is not a discovery,
and if you ran a sweep, `deflated_sharpe` is the number that discounts for how
many configurations you tried.

---

## Related documents

- [`../api/README.md`](../api/README.md) — authenticating and reading the API.
- [`../api/openapi.yaml`](../api/openapi.yaml) — the `Strategy`, `Rule` and
  `Condition` schemas as `GET /api/v1/strategies` returns them.
- [`../api/events.md`](../api/events.md) — what a firing rule eventually
  publishes, and how to trace it back.

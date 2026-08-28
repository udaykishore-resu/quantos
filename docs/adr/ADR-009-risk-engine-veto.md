# ADR-009 — The risk engine holds unconditional veto authority

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

In most trading architectures risk is a *score* blended with alpha. That design
has a predictable failure: a sufficiently strong alpha signal outvotes risk, and
the system's worst decisions occur exactly when conviction is highest — during
volatility spikes, around earnings, and in illiquid names, which is precisely
when the model's training distribution no longer applies.

We need risk to be able to say *no* in a way nothing downstream can overrule.

## Options

1. **Risk as a weighted term in the composite score.** Smooth, tunable,
   and unable to guarantee anything. There always exists a score that clears the
   threshold with maximal risk.
2. **Risk as a post-hoc filter on emitted signals.** Better, but the signal has
   already been published, alerted on and possibly acted upon; retraction is
   messier than prevention.
3. **Risk as a gate that must return ALLOW before a signal can exist.** Chosen.
4. **Risk as a gate plus a manual override.** Rejected: an override path is an
   override path, and the first incident review always finds it was used.

## Decision

`risk.Engine.Assess(ctx, Input) Assessment` returns exactly one of:

- `ALLOW_PAPER_SIGNAL` — a paper signal may be created
- `WATCH_ONLY` — the setup is recorded and displayed, but no signal and no paper
  order is created
- `BLOCK` — nothing is emitted beyond the assessment record itself

`signal.Engine.Generate` **requires** an `Assessment` argument and returns
`ErrRiskVeto` unless `Decision == AllowPaperSignal`. There is no code path that
constructs a `domain.Signal` without one; the constructor is unexported and the
only exported factory takes the assessment. A property test asserts that for
100 000 randomly generated inputs, no `BLOCK` or `WATCH_ONLY` assessment ever
yields a signal.

### The ten checks

Each check returns `pass | warn | fail` with a numeric detail and a reason
string, and every check's result is persisted:

| Check | `BLOCK` when | `WATCH_ONLY` when |
|-------|--------------|-------------------|
| Data freshness | quote older than `staleness_hard` | older than `staleness_soft` |
| Liquidity | ADV or notional below `min_liquidity` | below `preferred_liquidity` |
| Spread | spread bps > `max_spread_hard` | > `max_spread_soft` |
| Volatility | realised or implied vol > `max_vol_hard` | > `max_vol_soft` |
| Event proximity | earnings within `earnings_block_hours` | within `earnings_watch_hours` |
| Model confidence | — | confidence < `min_confidence` |
| Model health | active model in drift `severe` | drift `moderate` |
| Market regime | regime ∈ `blocked_regimes` (e.g. high-vol risk-off for long setups) | regime confidence < `min_regime_confidence` |
| Portfolio concentration | position or sector exposure would exceed hard cap | would exceed soft cap |
| Correlation | portfolio correlation-adjusted exposure > hard cap | > soft cap |
| Drawdown | portfolio drawdown > `max_drawdown` (kill switch) | > `drawdown_warn` |

Resolution is **most severe wins**: any `BLOCK` ⇒ `BLOCK`; else any
`WATCH_ONLY` ⇒ `WATCH_ONLY`; else `ALLOW`. There is no averaging and no
weighting, deliberately — averaging is how a veto becomes a suggestion.

All thresholds live in configuration (`config/risk.yaml`), are versioned, and
the config hash is recorded on every assessment so a historical decision can be
re-derived.

### Continuous re-assessment

Risk is not evaluated once. `risk-service` re-assesses every active signal on
each feature update, regime change and news event. A transition to `BLOCK` on an
active signal triggers `signal.invalidated` with reason `risk_veto`.

## Trade-offs

- (+) A hard, testable guarantee that survives refactoring.
- (+) Blocked setups are still *recorded*, so we can measure the veto's cost:
  `evaluation` scores blocked predictions too, and the model-health page reports
  "opportunity cost of veto" per rule. Risk is thereby falsifiable rather than
  superstitious.
- (+) Kill-switch semantics (drawdown, model drift) fall out naturally.
- (−) The system is conservative and will miss real opportunities, especially
  around earnings and in high volatility. This is an accepted product stance for
  an educational platform, and it is measured, not assumed.
- (−) Threshold tuning is a permanent activity, and badly-tuned thresholds can
  silence the system. Mitigated by the veto-cost report and by a `veto_rate`
  SLO alert (sustained > 80 % blocks is treated as a misconfiguration).
- (−) No emergency override exists. Operators can change config and redeploy —
  an auditable act with a review trail — but cannot bypass the engine at runtime.

## Failure modes

- *Risk engine unavailable.* Fails **closed**: no assessment ⇒ no signal. The
  engine is in-process precisely so this is a code failure, not a network one.
- *Config load failure.* Last-known-good config is retained; if none, the engine
  starts in `BLOCK`-everything mode and reports unhealthy.
- *Stale portfolio state* (concentration checks need current positions).
  Portfolio staleness is itself a check; a stale portfolio degrades
  concentration and correlation checks to `WATCH_ONLY`.
- *Threshold drift making the veto vacuous.* CI asserts a canary scenario set:
  known-bad inputs (illiquid, pre-earnings, vol spike) must still produce
  `BLOCK` under the shipped config.

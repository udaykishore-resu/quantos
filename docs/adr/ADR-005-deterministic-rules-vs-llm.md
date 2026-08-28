# ADR-005 — Deterministic and quantitative layers are authoritative; the LLM explains

- **Status:** Accepted
- **Date:** 2026-08-27
- **Supersedes:** nothing. **This is the load-bearing decision of the platform.**

## Problem

Large language models are excellent at reading heterogeneous evidence and
producing readable, contrastive explanations. They are unsuitable as the origin
of a numeric trading decision: they are non-deterministic across runs, sensitive
to prompt phrasing, unable to guarantee arithmetic, unversionable in the sense
that matters for backtesting, and — most damning for this product — their output
cannot be *reproduced* from stored inputs, which is a stated core requirement
(§50).

The temptation is real: "give the model the market state and ask what to buy" is
one prompt and zero engineering. It is also unauditable, uncalibratable, and
untestable against history.

## Options

1. **LLM originates signals.** Rejected. Fails reproducibility, calibration,
   backtesting and governance rule G-2. Cannot answer "why did QuantOS generate
   this signal?" with anything but a paraphrase of itself.
2. **LLM as one voting member alongside quantitative models.** Superficially
   principled. Rejected because a stochastic voter destroys reproducibility of
   the *ensemble* and because assigning it a weight implies a calibration we
   cannot measure — the model's "confidence" is not a probability.
3. **LLM as a pre-filter on news, feeding numeric features.** Partially adopted,
   but only *behind* deterministic preprocessing and only producing bounded,
   typed fields with an explicit confidence, never a directional call. See §12
   of the requirements and `internal/news`.
4. **LLM strictly downstream, for explanation and summarisation.** Chosen.

## Decision

The pipeline order is fixed and enforced:

```
data → validation → features → deterministic rules → quantitative models
     → ML inference → regime → risk (veto) → opportunity → policy validation
     → [ LLM: explanation only ] → signal → monitoring → evaluation
```

**The LLM's permitted outputs:** narrative summary, restatement of supporting
evidence, restatement of counter-evidence, articulation of uncertainty,
qualitative scenario description, news interpretation into a *typed* schema,
and the daily market brief prose.

**The LLM's forbidden outputs:** any probability, score, threshold, price level,
position size, direction, or classification that is not already present in its
structured input.

### Enforcement, in three layers

1. **Structural.** `internal/llm` and `internal/analyst` receive an
   `analyst.Input` struct built from already-final artifacts. They have no
   client for market data, no access to the feature store, and no import edge to
   the decision packages. `tests/integration/architecture_test.go` walks the
   import graph and fails the build if that changes.
2. **Grounding validation.** `analyst.Ground()` extracts every numeric token from
   the generated text and matches it against a whitelist built from the
   structured input (values, and permitted derived forms: percentages of
   provided ratios, rounded forms, and differences between two provided values).
   An unmatched number invalidates the narrative.
3. **Language guard.** `analyst.Guard()` rejects certainty constructions
   ("will rise", "guaranteed", "risk-free", "definitely") and advice
   constructions ("you should buy", "recommend buying"), per G-1 and G-9.

On any failure — validation, guard, timeout, provider error — the system falls
back to `analyst.Template`, a deterministic renderer that produces a correct,
if drier, explanation from the same structured input, and sets
`explanation_source = "template"`. **The signal is unaffected**, because the
signal was already final before the LLM was invoked.

## Trade-offs

- (+) Every numeric decision is reproducible, backtestable and calibratable.
- (+) The system degrades to full functionality minus prose when the LLM is
  unavailable — the single most likely third-party failure.
- (+) Explanations are auditable: each claim maps to a structured field.
- (+) LLM cost is bounded and off the critical path (async, cached by
  signal_id + input hash).
- (−) Explanations are less fluent than an unconstrained model would produce,
  and occasionally the grounding validator rejects a *correct* narrative
  (false positive) and we ship the template instead. Measured as
  `analyst_grounding_rejections_total`; current tolerance envelope is documented
  in `docs/ml/analyst-evaluation.md`.
- (−) News interpretation cannot use the model's full judgement; it is boxed
  into a typed schema with deterministic pre-filters.
- (−) More engineering than "just prompt it".

## Failure modes

- *Provider outage / timeout / rate limit.* Template fallback, counter
  incremented, no user-visible error beyond `explanation_source`.
- *Prompt injection via a news article.* News text is passed as data inside a
  delimited block with an explicit instruction that content within is untrusted;
  the output schema is validated; and even a fully compromised output can only
  produce a *narrative*, never a signal, because of layer 1.
- *Hallucinated number slips through grounding.* Possible only if the number
  coincides with a permitted derived form. Residual risk accepted and monitored;
  the dashboard renders numbers from the structured payload, not by parsing
  prose, so the *displayed* figures are always authoritative.
- *Cost blow-up.* Per-tenant token budget, response cache keyed by input hash,
  and a circuit breaker that trips to template mode.

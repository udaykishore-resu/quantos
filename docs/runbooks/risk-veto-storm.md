# Runbook — Risk veto storm

**Severity:** ticket
**Alert:** `VetoRateExcessive`
**Owner:** platform on-call, with the risk owner

---

## Read this first

A high veto rate is **the risk engine working**, and the first instinct — widen
the thresholds until signals come back — is the single worst action available.

ADR-009 gives the engine unconditional veto authority, resolved most-severe-wins
with no averaging and no override, because averaging is exactly how a veto
degrades into a suggestion. `domain.NewSignal` refuses to construct a signal
without an allowing assessment, so the guarantee is structural: you cannot get
signals back by editing something downstream. The only lever is the thresholds
themselves, and every assessment is stamped with the config hash, so a threshold
you loosen at 03:00 is permanently visible in the provenance of everything
issued afterwards.

The alert's own wording is the right frame:

> The risk engine is blocking over 80 % of evaluations, which usually means a
> threshold needs review.

**Review**, on a normal working day, with the person who owns the threshold. Not
"adjust", now, alone.

## Symptoms

- `VetoRateExcessive`: non-`ALLOW_PAPER_SIGNAL` decisions over 80 % for 30 m.
- `quantos_signals_generated_total` near zero while
  `quantos_predictions_total` is normal — the pipeline runs, the gate closes.
- `quantos_risk_decisions_total` concentrated on one `reason` label.
- Users report "no signals" while every health check is green.

The three decisions are `ALLOW_PAPER_SIGNAL`, `WATCH_ONLY` and `BLOCK`. Only the
first produces a signal.

## Diagnosis

The `reason` label is the entire diagnosis. Everything else is context.

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics \
  | grep '^quantos_risk_decisions_total' | sort -t' ' -k2 -rn
```

In PromQL, the shape that answers it in one query:

```
topk(5,
  sum by (reason, decision) (rate(quantos_risk_decisions_total[1h]))
)
```

```sh
# The assessment for one ticker, with every check and its verdict.
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/risk/AAPL | jq '.data | {decision, reasons}'
```

Now map the dominant reason to a cause:

| Dominant reason | Threshold | Legitimate cause | Branch |
|---|---|---|---|
| staleness | `risk.staleness_soft` 30 s / `staleness_hard` 2 m | market data problem | A |
| confidence | `risk.min_confidence` 0.30 | model degraded | B |
| regime confidence | `risk.min_regime_confidence` 0.35 | regime is genuinely ambiguous | C |
| volatility | `max_vol_soft` 0.45 / `max_vol_hard` 0.90 | the market actually is volatile | D |
| spread | `max_spread_soft_bps` 25 / `max_spread_hard_bps` 80 | illiquid conditions | D |
| liquidity | `min_liquidity` 2 M / `preferred_liquidity` 20 M | universe drifted into thin names | E |
| drawdown | `max_drawdown` 0.20 | **the kill switch fired** | F |
| position / sector / correlation | the weight caps | the book is concentrated | G |
| earnings | `earnings_block_window` 24 h | earnings season | H |

## Remediation

### A. Staleness dominant

This is not a risk incident. The risk engine is correctly refusing to act on old
prices. Go to [`market-data-outage.md`](market-data-outage.md).

### B. Confidence dominant

The model is producing low-confidence predictions and the engine is filtering
them out, which is the arrangement working end to end.

```sh
curl -sS -H "Authorization: Bearer $TOKEN" localhost:8080/api/v1/model-health | jq .
```

If drift is `moderate` or worse, go to [`model-drift.md`](model-drift.md).

Check whether the model artifact loaded at all — a service falling back to the
deterministic rule prior is capped at `predict.fallback_confidence_ceiling`
(0.55), and that cap exists *precisely so that a degraded prediction fails this
check*:

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics | grep model_artifact_load_failures_total
```

Non-zero means the veto storm is a symptom of a failed artifact load, and the
fix is in [`model-drift.md`](model-drift.md) section C, not here.

### C. Regime confidence dominant

`regime.hysteresis` is 5 m and `regime.min_confidence` 0.45. A market genuinely
between regimes produces low regime confidence, and the engine tightening in
response is the correct behaviour.

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/market/regime/history | jq '.data[:20] | map({regime, confidence, as_of})'
```

Look for flapping. If the regime is oscillating faster than the hysteresis
window, the classifier is unstable and that is the bug — not the threshold.

### D. Volatility or spread dominant

Check whether the market is actually like that before touching anything:

```sh
curl -sS -H "Authorization: Bearer $TOKEN" localhost:8080/api/v1/market/status | jq .
```

On a genuinely volatile day, a veto rate of 80 % is the correct output. The
platform is telling you it does not have a confident, tradeable view, and the
honest response is to accept fewer signals.

The `soft`/`hard` split already encodes the graded response: past soft, the
decision degrades to `WATCH_ONLY` rather than `BLOCK`, so the signal is still
recorded and still visible — it is just not actionable. That is the designed
middle ground, and it is already in use.

### E. Liquidity dominant

`universe.max_symbols` is 120 and `universe.source` is `builtin`. A veto storm on
liquidity usually means the universe picked up names that do not clear
`min_liquidity` (2 M average daily notional).

That is a universe problem, not a risk problem. Fix the universe:

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/stocks | jq '[.data[] | select(.adv < 2000000) | .ticker]'
```

and exclude them via `universe.exclude` rather than lowering `min_liquidity`.
Lowering the liquidity floor makes every position harder to exit, which is what
the floor is for.

### F. Drawdown dominant — the kill switch

**Stop and read.** `risk.max_drawdown` is 0.20 and it is a portfolio kill switch,
not a per-signal check. It firing means the paper book is down 20 % from its
high-water mark, and the engine has stopped issuing new signals across the
board.

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/paper/portfolio | jq '.data | {equity, high_water, drawdown}'
```

Do not raise `max_drawdown` to resume. The kill switch fired because the
strategy lost 20 %; raising the limit does not change that fact, it removes the
only thing that noticed.

The correct path is a review: read the invalidated signals and the evaluation
records for the drawdown period, decide whether the strategy or the model is at
fault, and reset the book deliberately once that is understood. This is a paper
book, so nothing is at stake but the evidence — and the evidence is the point.

### G. Concentration dominant

`max_position_weight` 0.10, `max_sector_weight` 0.35,
`max_correlated_exposure` 0.50. These veto *new* positions when the book is
already concentrated, so the storm ends as positions close.

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/v1/paper/positions \
  | jq '[.data[] | {ticker, weight, sector}] | sort_by(-.weight)'
```

If one sector is at the cap, that is the constraint doing its job. Waiting is a
legitimate remediation.

### H. Earnings dominant

`earnings_block_window` is 24 h and `earnings_watch_window` 72 h. During a busy
earnings week a large fraction of the universe is inside one or the other, and a
high veto rate is seasonal rather than anomalous.

Confirm by counting how many symbols are in the window; if it tracks the
earnings calendar, close the ticket.

## If a threshold genuinely needs to change

Not during the incident. The process:

1. Quantify the cost of the veto. `evaluation-service` scores the predictions
   the engine **blocked** as well as the ones it allowed, which is what makes
   this measurable rather than a matter of opinion:

   ```sh
   curl -sS -H "Authorization: Bearer $TOKEN" \
     localhost:8080/api/v1/evaluations | jq '[.data[] | select(.decision != "ALLOW_PAPER_SIGNAL")] | {n: length, acc: (map(.correct) | add / length)}'
   ```

   If the blocked predictions would have been *wrong*, the veto was right and
   the threshold stays.

2. Change the value in `config/quantos.yaml` (or the ConfigMap), in a pull
   request, reviewed by the risk owner.
3. Backtest it: `make backtest` over the same period with both values.
4. Deploy normally. The config hash changes, which is how a future reader knows
   the decision boundary moved.

## Verify recovery

```
sum(rate(quantos_risk_decisions_total{decision!="ALLOW_PAPER_SIGNAL"}[1h]))
  / sum(rate(quantos_risk_decisions_total[1h])) < 0.8

increase(quantos_signals_generated_total[1h]) > 0
```

"Recovery" here may legitimately mean "the market changed", not "we fixed
something". Record which it was.

## Rollback

If a config change caused the storm, roll the ConfigMap back and restart:

```sh
kubectl -n quantos rollout undo deploy/signal-service
```

Assessments made under the previous config are not re-derived. They carry the
old config hash and remain reproducible against it, which is exactly what the
hash is for (`internal/risk/risk.go`: "thresholds live in configuration and the
config hash is stamped on each assessment, so a historical veto can be
re-derived even after the thresholds change").

## Related

- [`model-drift.md`](model-drift.md) — the most common upstream cause
- [`market-data-outage.md`](market-data-outage.md) — when staleness dominates
- `docs/adr/ADR-009-risk-engine-veto.md`
- `internal/risk/risk.go` — every threshold and how the checks resolve

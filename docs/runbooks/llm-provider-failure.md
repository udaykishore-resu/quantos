# Runbook — LLM provider failure

**Severity:** ticket. Never a page.
**Alerts:** `LLMProviderDegraded`, `ExplanationsAlwaysRejected`
**Owner:** platform on-call

---

## Read this first

**No decision depends on the language model.** Not one number, not one
threshold, not one veto.

ADR-005 puts the deterministic classifier first and always; the model may only
refine that result into the same typed schema, and its influence is capped. The
`internal/pipeline` package has no import edge to `internal/analyst` or
`internal/llm`, which makes the separation structural rather than a convention —
`make arch` fails if anyone adds one. Explanations are produced in
`App.explainSignal`, strictly after the decision is final, and the only thing
they write back is prose.

`docs/operations/slo.md` states the consequence: the AI explanation SLO is
"P95 < 8 s, **best-effort**. Off the critical path. Never gates a signal."

So when the provider is down:

- signals are still generated, with the same numbers,
- risk assessments are unchanged,
- alerts still fire,
- some signals have no narrative, and news classification falls back to the
  deterministic path alone.

That is a degradation of explanation quality. It is not an outage, and it does
not warrant waking anyone.

`llm.fail_open` is `true`, which encodes exactly this: a failed LLM call returns
nothing rather than failing the operation around it.

## Symptoms

- `LLMProviderDegraded`: over half of calls failing for 10 m.
- `quantos_llm_failures_total` rising, with a `reason` label.
- `quantos_circuit_state{name="llm"}` at 2 (open).
- Signals present in `/api/v1/signals` with an empty or absent narrative.
- `quantos_analyst_grounding_rejections_total` rising — a different problem, see
  below.

## Diagnosis

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics \
  | grep -E '^quantos_(llm_calls_total|llm_failures_total|circuit_state|analyst_grounding)'
```

```sh
kubectl -n quantos logs deploy/signal-service --tail=200 | grep -i llm
kubectl -n quantos logs deploy/news-service   --tail=200 | grep -i llm
```

Only these two services can call the provider. api-gateway holds no key by
design — it renders responses to a browser, and a provider key must not be one
bug away from a response body. If you see LLM activity from any other service,
that is a finding in itself.

| Pattern | Cause | Branch |
|---|---|---|
| `reason="auth"` / 401 | key missing, wrong, or revoked | A |
| `reason="rate_limit"` / 429 | over the provider quota | B |
| `reason="timeout"` | provider slow or unreachable | C |
| circuit open, no recent calls | breaker holding it open | D |
| calls succeeding, grounding rejections high | **not a provider problem** | E |

## Remediation

### A. Authentication

```sh
# Is the key even projected? Length only - never print the value.
kubectl -n quantos get secret quantos-llm \
  -o jsonpath='{.data.ANTHROPIC_API_KEY}' | base64 -d | wc -c
```

Zero or missing means the ExternalSecret has not resolved:

```sh
kubectl -n quantos get externalsecret quantos-llm
kubectl -n quantos describe externalsecret quantos-llm | tail -20
```

The usual cause is that the Secrets Manager entry exists but has never been
populated. Terraform creates the *container* and deliberately never writes the
value — a key in a variable default is a key in git, and a key in a plan output
is a key in every CI log that renders a plan. Populate it out of band:

```sh
aws secretsmanager put-secret-value --secret-id quantos-prod/llm-api-key \
  --secret-string '{"api_key":"<paste>"}'

kubectl -n quantos delete secret quantos-llm   # External Secrets re-creates it
kubectl -n quantos rollout restart deploy/signal-service deploy/news-service
```

If the key is present and still rejected, it has been revoked. Rotate it at the
provider, write the new value with the same command, and restart.

`app.New` logs a warning and falls back to the mock when
`llm.api_key_env` resolves to nothing, so a missing key produces mock
explanations rather than a crash. That is the fail-open behaviour working, and
it is also why nobody notices for a while.

### B. Rate limited

`llm.max_per_minute` is 60 and `news.max_llm_per_minute` is 20. Two services
sharing one provider quota can exceed it even when each is within its own limit.

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics | grep 'llm_failures_total.*rate_limit'
```

Lower the limits rather than raising the quota during an incident:

```sh
kubectl -n quantos patch configmap quantos-config --type merge \
  -p '{"data":{"quantos.cluster.yaml":"...llm:\n  max_per_minute: 30\n..."}}'
kubectl -n quantos rollout restart deploy/signal-service
```

`llm.cache_ttl` is 30 m and `internal/llm.NewCached` wraps every provider, so
repeated requests for the same signal do not hit the API. A rate-limit storm
with a working cache usually means signal volume genuinely rose.

### C. Timeout or unreachable

`llm.timeout` is 20 s. Check whether it is the provider or the route:

```sh
# From a pod that is allowed to make the call. If this hangs rather than
# returning an HTTP error, it is the NetworkPolicy.
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- --timeout=10 https://api.anthropic.com/ 2>&1 | head -5
```

```sh
kubectl -n quantos describe networkpolicy allow-egress-llm
```

That policy selects `signal-service` and `news-service` only, and allows 443 to
`0.0.0.0/0` minus the VPC. If it selects nothing — for instance because the
service labels changed — every call times out with no error at the provider end.

### D. Circuit breaker open

`internal/llm.NewBreaker` opens after 5 failures and holds for a minute.

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics | grep 'circuit_state{name="llm"}'
```

Leave it. It closes on the first successful probe. Restarting the pod to force it
closed just re-opens it five calls later and loses the backoff.

### E. Grounding rejections — a different problem

`ExplanationsAlwaysRejected` is **not** a provider failure. The calls are
succeeding; the grounding validator is rejecting what comes back.

```sh
kubectl -n quantos exec deploy/signal-service -- \
  wget -qO- http://localhost:8082/metrics | grep grounding_rejections_total
```

The validator checks that a narrative only asserts things present in the typed
inputs. High rejection means the model is producing prose that claims facts the
data does not contain — the exact failure ADR-005 exists to prevent, and the
validator catching it is the design working.

Causes, in order of likelihood: the provider silently changed model version; the
prompt drifted; `llm.model` was changed. Pin the model explicitly:

```sh
kubectl -n quantos set env deploy/signal-service QUANTOS_LLM_MODEL=claude-sonnet-4-5
```

Rejected narratives are discarded, not published. The signal is unaffected. This
is a ticket for the research owner, not an operational fix.

### Falling back to the mock

If the provider is unavailable for an extended period, switch to the
deterministic mock rather than leaving calls failing:

```sh
kubectl -n quantos set env deploy/signal-service deploy/news-service \
  QUANTOS_LLM_PROVIDER=mock
```

The mock is deterministic and needs no key, so the platform runs completely
offline. Explanations become templated rather than generated; every number in
them is still correct, because they were always derived from the typed inputs
rather than from the model.

Remember to switch back. `QUANTOS_LLM_PROVIDER=mock` set by hand does not appear
in git and survives until someone notices the explanations got repetitive.

## Verify recovery

```
rate(quantos_llm_failures_total[10m]) / rate(quantos_llm_calls_total[10m]) < 0.1
quantos_circuit_state{name="llm"} == 0
rate(quantos_analyst_grounding_rejections_total[30m]) low
```

And confirm the thing that was never at risk is still fine:

```
rate(quantos_signals_generated_total[15m])   unchanged through the incident
rate(quantos_risk_decisions_total[15m])      unchanged
```

If either of those moved, the incident was not this one.

## Rollback

Nothing to roll back on the platform side: the LLM path is optional and
fail-open, so its failure changes no state.

If a config change (a new `llm.model`, a raised `max_per_minute`) caused it:

```sh
kubectl -n quantos rollout undo deploy/signal-service
```

Signals emitted without a narrative are not re-explained retroactively. They
carry their full typed provenance — features, prediction, risk assessment — which
is what "explain this decision" actually means here. The prose was always the
optional part.

## Related

- `docs/adr/ADR-005-deterministic-rules-vs-llm.md` — why the model cannot decide
- `internal/analyst/` — the grounding validator
- `internal/llm/` — the cache, the rate limiter and the breaker
- `docs/operations/slo.md` — the best-effort explanation SLO

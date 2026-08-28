# QuantOS API

QuantOS is an educational research and paper-trading platform. Every route
returns research output — probability distributions, risk verdicts, simulated
positions. Nothing here is advice, nothing is a guarantee, and no endpoint
places a real-money order. `POST /api/v1/paper/orders` writes to a simulated
book that has no brokerage counterpart anywhere in this repository.

The machine-readable contract is [`openapi.yaml`](./openapi.yaml). This document
is the part that a schema cannot tell you: how to get a token, what to do when a
call fails, and how to hold the stream open without losing events.

---

## Getting a token

Two authentication schemes exist and they correspond to two ways of running the
platform.

**Cluster mode** validates RS256 tokens against a configured OIDC issuer's
JWKS. Roles arrive in the `roles` claim, and the common issuer shapes
(`realm_access.roles`, `groups`) are read too, so a real identity provider works
without a custom mapper. There is no login route in this mode: get your token
from the issuer.

**Embedded and compose mode** sign their own HS256 tokens for the principals
listed under `auth.dev_users` in the configuration. That is what
`POST /api/v1/auth/login` is for, and it is the only thing it is for. The
shipped configuration defines two: `demo` / `demo` with roles
`[viewer, analyst]`, and `operator` / `operator` with `[viewer, analyst,
operator]`.

Either way, an unknown signing algorithm is rejected outright rather than
tolerated, which is what closes the `alg: none` and algorithm-confusion attacks.

### Log in and keep the token

```bash
# Start the platform (defaults to :8080).
make run

# Exchange development credentials for a bearer token.
TOKEN=$(curl -sS -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"subject":"demo","password":"demo"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["token"])')

# Every subsequent call carries it.
curl -sS localhost:8080/api/v1/market/regime -H "Authorization: Bearer $TOKEN"
```

The login response also returns the principal, which is the fastest way to see
what a token can actually do:

```bash
curl -sS -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"subject":"operator","password":"operator"}' \
  | python3 -m json.tool
```

```json
{
  "data": {
    "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
    "principal": {
      "sub": "operator",
      "roles": ["viewer", "analyst", "operator"],
      "scopes": ["audit:read", "backtest:run", "market:read", "models:read",
                 "paper:write", "portfolio:read", "signals:read"],
      "expires_at": "2026-08-28T15:20:51.770147811Z",
      "issuer": "quantos-local"
    }
  },
  "disclaimer": "QuantOS is an educational research and paper-trading platform. Output is probabilistic, may be wrong, and is not financial advice. No real-money orders are placed."
}
```

A failed login is deliberately vague — a wrong password and an unknown subject
return the identical 401 with `detail: "invalid credentials"`, and the server
hashes a throwaway password for an unknown subject so the two paths take the
same time. Distinguishing them would hand an attacker a user-enumeration
oracle.

The token may also travel in a `quantos_token` cookie, which a browser dashboard
served from an allow-listed origin can use instead of a header.

### Roles and what they reach

Roles grant scopes; handlers require scopes. The mapping is one table rather
than conditionals scattered through the handlers, so "who can do what" is
answerable by reading one place.

| Role | Scopes | Reaches |
|---|---|---|
| `viewer` | `market:read`, `signals:read`, `models:read`, `portfolio:read` | Market, signals, predictions, alerts, risk, models, strategies, the paper book (read-only), the stream |
| `analyst` | viewer plus `backtest:run` | Adds `POST`/`GET /api/v1/backtests` |
| `operator` | analyst plus `paper:write`, `audit:read` | Adds `POST /api/v1/paper/orders` and `GET /api/v1/audit` |
| `admin` | operator plus `config:write`, `models:write` | Configuration and model management |

A token with no recognisable role is treated as `viewer`. When authentication is
disabled entirely the anonymous principal holds the `viewer` scopes only —
turning auth off must never silently grant write access.

Missing a scope is a 403, and the response names what was missing:

```bash
curl -sS localhost:8080/api/v1/audit -H "Authorization: Bearer $TOKEN"
```

```json
{
  "error": {
    "code": "forbidden",
    "message": "the principal lacks the required scope",
    "detail": "required scope: audit:read",
    "request_id": "d0297f3fb3cb19a3"
  },
  "disclaimer": "QuantOS is an educational research and paper-trading platform. Output is probabilistic, may be wrong, and is not financial advice. No real-money orders are placed."
}
```

---

## The envelope

Every success looks the same:

```json
{
  "data": { },
  "meta": { "count": 1, "limit": 2, "as_of": "2026-08-28T03:21:00Z" },
  "degraded": ["llm"],
  "disclaimer": "QuantOS is an educational research and paper-trading platform. Output is probabilistic, may be wrong, and is not financial advice. No real-money orders are placed.",
  "request_id": "017ba85cee958cd1",
  "served_at": "2026-08-28T03:21:07.774774284Z"
}
```

Every failure looks the same, and never carries `data`:

```json
{
  "error": {
    "code": "not_found",
    "message": "the resource does not exist",
    "detail": "unknown ticker ZZZZ",
    "request_id": "8413fd9c7404d896"
  },
  "disclaimer": "QuantOS is an educational research and paper-trading platform. Output is probabilistic, may be wrong, and is not financial advice. No real-money orders are placed."
}
```

Four fields are worth understanding rather than skipping.

**`disclaimer`** is on every response, success and failure alike. It is also on
the records themselves — a `Signal`, `StockScore`, `OpportunityScore`, `Alert`
and `Prediction` each carry their own `disclaimer` field inside `data`. That
duplication is deliberate: a client that stores one of those objects and
redisplays it a week later would otherwise have stripped the caveat.

**`degraded`** names subsystems that were unavailable when the response was
assembled. It appears only when something was degraded. The platform still
answers, and says what was missing, rather than returning a complete-looking
body assembled from less than it claims.

**`request_id`** also comes back in the `X-Request-ID` header and is written to
the structured log line and, for writes, the audit record. Quote it when
reporting a problem; it is how a support conversation finds the server's side of
the interaction. You can set it yourself by sending `X-Request-ID` on the
request, which is useful when a client already has its own correlation scheme.

**`served_at`** is when the response was written, in UTC. It is *not* the age of
the data. For that, read `meta.as_of`.

### `meta`

Every field is omitted at its zero value, so an absent `count` means zero and an
absent `stale` means not stale.

| Field | Meaning |
|---|---|
| `count` | Items in *this* response, not the total available |
| `limit` | The effective limit, after defaulting and clamping |
| `offset` | The offset applied. Echoed only where the route supports it |
| `as_of` | The domain timestamp of the data |
| `stale` | The data has aged past the freshness gate |
| `stale_reason` | Why the gate fired |

`as_of` is omitted rather than zero-valued when the server has no timestamp to
report. A plain zero time would serialise as `0001-01-01T00:00:00Z`, and a
client that trusts its presence renders year one — so the field is a pointer
internally and simply absent on the wire.

`stale` is not cosmetic. A stale instrument may still be displayed, but it can
never produce a new signal, and a stale price is never used to resolve a
prediction: scoring against one would manufacture outcomes out of missing data.

### Correlation and tracing

Three headers come back on every response, and two are read from the request.

| Header | Direction | Meaning |
|---|---|---|
| `X-Request-ID` | in and out | Per-request identity. Generated when absent |
| `X-Correlation-ID` | in and out | Groups requests into one logical operation. Defaults to the request id |
| `traceparent` | in and out | W3C trace context, propagated onto every event the request causes |

Sending `traceparent` joins your call to a trace you already own. The resulting
trace and correlation ids travel onto every event the request emits, which is
what lets you follow a decision from an HTTP call through the bus to the signal
it produced — see [`events.md`](./events.md).

---

## Pagination

There is no cursor. List routes take `limit`, some take `offset`, and several
take a `from` / `to` time window.

`limit` is forgiving by design: a non-numeric or negative value falls back to
the route's default rather than erroring, and anything above 5000 is clamped to
5000. Read `meta.limit` to see what was actually applied.

`from` and `to` are RFC3339 and equally forgiving — an unparseable value is
ignored, so the filter is simply not applied. Check that you got the window you
asked for rather than assuming.

| Route | `limit` default | Other |
|---|---|---|
| `GET /api/v1/market/regime/history` | 100 | `from` |
| `GET /api/v1/stocks` | 100 | `sector` |
| `GET /api/v1/stocks/{ticker}/candles` | 500 | `interval` (default `1m`), `from`, `to` |
| `GET /api/v1/news` | 50 | `ticker` |
| `GET /api/v1/signals` | 100 | `status`, `ticker`, `strategy`, `offset`, `from` |
| `GET /api/v1/predictions` | 100 | `ticker`, `from` |
| `GET /api/v1/alerts` | 100 | `ticker`, `offset`, `from` |
| `GET /api/v1/evaluations` | 500 | — |
| `GET /api/v1/paper/orders` | 100 | — |
| `GET /api/v1/backtests` | 50 | — |
| `GET /api/v1/audit` | 200 | — |
| `GET /api/v1/brief` | — | `top` (default 10) |

Two behaviours will surprise you if you have not read them here.

**An empty list can be `null` rather than `[]`.** Where a handler builds its
result by appending to an unallocated slice, no items means no allocation, and
that encodes as JSON `null`. Treat `null` and `[]` as the same thing. It affects
`/signals`, `/news`, `/alerts`, `/market/regime/history`, `/models`,
`/predictions` (store path), and the array fields inside `/brief` and
`/paper/portfolio`.

**`GET /api/v1/signals` reads two different sources.** With no `status`, or with
`status=ACTIVE`, the answer comes from the signal engine, which is authoritative
for the live set. Any other `status` reads history from the store. The engine
does not index its live set, so `ticker`, `strategy`, `from` and `offset` are
applied to it by hand — deliberately, because silently ignoring `?ticker=` would
be worse than not supporting it: a caller would believe it had filtered.

---

## Rate limiting

A token bucket, refilling continuously rather than resetting on a window
boundary. The shipped configuration is 50 requests per second with a burst of
100 (`http.rate_limit_rps`, `http.rate_limit_burst`).

**The bucket is keyed on the authenticated subject.** The rate-limiting
middleware is mounted inside the authenticated route group, after the principal
is on the context, so each caller has an allowance of their own:

- One principal calling from two addresses shares one bucket.
- Two principals behind one address get two buckets — one caller cannot exhaust
  the allowance of everyone behind the same proxy.

Requests that reach the platform without a principal — `POST /api/v1/auth/login`
and the health endpoints — fall back to the client address, preferring
`X-Real-IP` and then the first entry of `X-Forwarded-For` so that a deployment
behind a load balancer does not put every client in one bucket.

Exceeding it returns 429 with `Retry-After: 1`:

```json
{
  "error": {
    "code": "rate_limited",
    "message": "too many requests",
    "request_id": "bab66706dfbfec66"
  },
  "disclaimer": "QuantOS is an educational research and paper-trading platform. Output is probabilistic, may be wrong, and is not financial advice. No real-money orders are placed."
}
```

The failure direction depends on the method. A read fails **open** when the
limiter itself is unreachable, so a Redis outage does not take the dashboard
down. A write fails **closed**, returning 503 with
`detail: "rate limiter unavailable; write rejected"`, because an outage must not
become an unmetered write channel.

Request bodies are bounded too: 1 MiB globally, 8 KiB for an order and 16 KiB
for a backtest request. Bodies are decoded with unknown fields rejected, so a
typo is a 400 rather than a silently dropped setting.

---

## Errors and what to do about each

`code` is stable and machine-readable. Branch on it. `message` is fixed per code
and identical on every occurrence, so it carries no information beyond the code;
`detail` is where the specifics are, when the server can say them without
leaking internals.

| Status | `code` | What happened | What to do |
|---|---|---|---|
| 400 | `bad_request` | Malformed body, unknown JSON field, failed validation | Fix the request. Retrying unchanged will not help. `detail` names the field or the rule |
| 401 | `unauthorized` | No token, expired token, or verification failed | Obtain a new token. Do not retry the same one |
| 403 | `forbidden` | Token is valid, principal lacks the scope | Permanent for that principal. `detail` names the scope. Escalate to a role that holds it |
| 404 | `not_found` | No such resource | Do not retry. On `/stocks/{ticker}` it means the symbol is not in the configured universe; on `/risk/{ticker}` it means nothing has been assessed yet |
| 429 | `rate_limited` | Bucket empty | Back off and retry. `Retry-After: 1`. The bucket refills continuously, so a short pause suffices |
| 500 | `internal` | Unexpected failure | Retry once with backoff. The detail is in the server log under your `request_id`, deliberately not in the response |
| 503 | `unavailable` | A backing service the route needs is absent or unreachable | Retry with backoff. `Retry-After: 5`. If it persists, check `/healthz` — `degraded` will name the subsystem |

Two of these deserve a note.

**503 is an honest answer, not a bug.** Routes that cannot be served from memory
— regime history, candles, stored predictions, alerts, backtests, audit, the
brief — return 503 when their store is not configured, rather than an empty
list. An empty list would be indistinguishable from "there is nothing to
report", and those mean entirely different things.

**500 never carries the underlying error text.** The detail goes to the log with
the `request_id` attached. That is both a security decision and a support one:
the information exists, it is just not in a place an untrusted caller can read.

---

## Streaming

`GET /api/v1/stream` is a long-lived `text/event-stream`. Server-to-client only,
which is why it is SSE and not a WebSocket: the browser reconnects
automatically and `Last-Event-ID` resume comes for free.

### Connecting

```bash
curl -sS -N localhost:8080/api/v1/stream \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: text/event-stream'
```

`EventSource` cannot set headers, so this one route also accepts the token as a
query parameter:

```bash
curl -sS -N "localhost:8080/api/v1/stream?access_token=$TOKEN&topics=signal,alert"
```

That allowance is scoped to this route alone rather than enabled globally,
because a token in a URL ends up in access logs. Use the header wherever your
client can set one.

### Frames

```
id: 4193
event: quote
data: [{"ticker":"AAPL","bid":230.8123,"ask":230.8949,"last":230.8536,"ts":"2026-08-28T03:20:05Z","seq":1794231,"source":"sim"}]

retry: 3000

: heartbeat 1787887250

id: 4194
event: alert
data: {"id":"01a04662-92b4-7d6f-9d61-071b76b91325","dedup_key":"826e8eb6f3dcd3c858b4e7dcd1bb6365","type":"PROBABILITY_SHIFT","severity":"NOTICE","ticker":"WMT","title":"WMT 15m upside probability moved","status":"PAPER-TRADING WATCH"}
```

The `event:` name is the type tag — SSE has nowhere to put a discriminator
inside the JSON, so the name is what tells you how to parse `data:`.

| `event:` | Payload |
|---|---|
| `quote` | Array of quotes, throttled to at most one frame every 500 ms |
| `regime` | The market regime, published only when it changes |
| `signal` | A newly emitted signal |
| `signal_invalidated` | A signal whose invalidation condition fired |
| `alert` | An alert |
| `prediction` | A new prediction |
| `portfolio` | The paper book, on every mark |
| `health` | `{ "health": ..., "drift": ... }` |
| `resync` | Control frame. **A gap notice** — see below |
| `overflow` | Control frame. Your connection was dropped for falling behind |

`retry: 3000` is the server telling the browser how long to wait before
reconnecting. `: heartbeat <unix>` is a comment frame every 20 seconds; it is
what detects a dead peer, which is why this route deliberately has no write
deadline.

### Resuming, and the gap you must handle

The hub keeps a bounded ring of the most recent events (1024 by default,
`http.sse_replay`). Reconnect with `Last-Event-ID` set to the last id you
processed, and everything with a higher id that the ring still holds is replayed
before the live stream resumes. `?last_event_id=` does the same thing and is
consulted only when the header is absent or zero.

If your last id is older than everything the ring still holds, the server cannot
close the gap. It says so, explicitly, with an unnumbered `resync` frame *before*
the replay:

```
event: resync
data: {"reason":"the replay buffer no longer covers your last event id; refetch state over REST"}
```

**A `resync` frame means the events that follow are not a continuation.** They
are whatever the ring happens to still hold, which begins somewhere after your
last id with an unknown number of events missing in between. A client that
applies them as deltas will carry a silently corrupt view for as long as the
process lives.

The correct handling is a hard reset, in this order:

1. Stop applying stream frames to local state, and buffer them instead.
2. Refetch the state you care about over REST — `/api/v1/market/regime`,
   `/api/v1/stocks`, `/api/v1/signals`, `/api/v1/paper/portfolio` — and note the
   `meta.as_of` of each.
3. Replace your local state wholesale with what you fetched. Do not merge.
4. Drain the buffer, discarding frames older than the state you just fetched,
   and resume applying from there.
5. Record the highest `id:` you have seen, and use it as `Last-Event-ID` on the
   next reconnect.

#### A worked resume

Say you processed up to event 4193, then lost the connection for four minutes
while the platform published 6000 events. The ring holds 1024, so its oldest
surviving event is around 9177.

```bash
# Reconnect from where you left off.
curl -sS -N localhost:8080/api/v1/stream \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Last-Event-ID: 4193'
```

```
event: resync
data: {"reason":"the replay buffer no longer covers your last event id; refetch state over REST"}

id: 9177
event: quote
data: [{"ticker":"AAPL", ...}]
...
```

Events 4194 through 9176 are gone. Refetch, then continue from the ids you are
now seeing:

```bash
curl -sS localhost:8080/api/v1/market/regime      -H "Authorization: Bearer $TOKEN"
curl -sS "localhost:8080/api/v1/signals?limit=500" -H "Authorization: Bearer $TOKEN"
curl -sS localhost:8080/api/v1/paper/portfolio     -H "Authorization: Bearer $TOKEN"
```

Had you reconnected within the window instead — say from 9500 while the ring
held 9177 onward — there would be no `resync` frame, and the replay from 9501
would be a genuine continuation you can apply directly. The presence or absence
of that one frame is the whole difference, so branch on it explicitly rather
than inferring a gap from event ids.

### Overflow

Each connection has a bounded queue (256 by default, `http.sse_buffer`). A
client that cannot keep up has its connection closed with:

```
event: overflow
data: {"reason":"this connection could not keep up and was closed; reconnect to resume"}
```

The connection is dropped rather than the server growing a buffer without bound
— dropping the connection is recoverable, an unbounded queue is not. Reconnect
with `Last-Event-ID`; if you were far enough behind, you will get `resync` and
should follow the reset above.

### One caveat about `topics`

`?topics=` filters the **live** fan-out only. The replay that runs at connection
time is not filtered, so a client that subscribes to `topics=regime` and resumes
from an older id receives buffered frames of other topics before the live stream
narrows. Filter on the `event:` name client-side rather than assuming the server
already did.

---

## Idempotency

Two places in the API make a guarantee about repeated requests, and they make it
differently.

### `POST /api/v1/paper/orders` — client-supplied key, required

`idempotency_key` is required, not optional. A client that times out and
resubmits must not double its position, and making the key optional would make
that outcome available by omission.

The key is namespaced server-side as `api:<your key>` and stored on the order.
Resubmitting the same key returns the *original* order — same `id`, same
`created_at`, still `202` rather than a conflict:

```bash
KEY=$(python3 -c 'import uuid; print(uuid.uuid4())')

curl -sS -X POST localhost:8080/api/v1/paper/orders \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"idempotency_key\":\"$KEY\",\"ticker\":\"AAPL\",\"side\":\"BUY\",\"type\":\"MARKET\",\"quantity\":10}"

# Exactly the same call again — same order comes back, nothing new is created.
curl -sS -X POST localhost:8080/api/v1/paper/orders \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"idempotency_key\":\"$KEY\",\"ticker\":\"AAPL\",\"side\":\"BUY\",\"type\":\"MARKET\",\"quantity\":10}"
```

Choose a key that identifies the *intent*. A client-generated UUID per
submission is right. The ticker is not — that would collapse two genuinely
different orders into one.

Omitting it is a 400 that says why:

```json
{
  "error": {
    "code": "bad_request",
    "message": "the request could not be understood",
    "detail": "idempotency_key is required: order submission must be safe to retry",
    "request_id": "c3f377608b4733b3"
  }
}
```

The response is `202 Accepted` with the order `PENDING`. Fills happen on the
simulator's next mark, not inside the request, so poll
`GET /api/v1/paper/orders` for the terminal status.

### `POST /api/v1/backtests` — server-derived seed, no client key

Backtests take no idempotency key. Submitting the same request twice queues two
runs with two ids. What is guaranteed instead is *determinism*: given the same
`seed`, `config_hash`, `model_version` and `code_version`, a run reproduces byte
for byte, and `result_hash` is what you compare to confirm it did.

Omit `seed`, or pass `0`, and the runner derives one deterministically from the
request itself. A zero seed would make the run non-reproducible in the one way
that matters, so it is never left as zero.

```bash
curl -sS -X POST localhost:8080/api/v1/backtests \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"momentum-2h","strategy_id":"momentum",
       "start":"2026-08-28T01:00:00Z","end":"2026-08-28T03:00:00Z",
       "interval":"1m","seed":42,"starting_cash":100000,
       "tickers":["AAPL","MSFT"]}'
```

```json
{
  "data": {
    "id": "bt_ef429e7f663c0191",
    "name": "momentum-2h",
    "status": "QUEUED",
    "strategy_id": "momentum",
    "seed": 42,
    "config_hash": "5113ecd5ec193503",
    "code_version": "dev",
    "starting_cash": 100000
  }
}
```

Poll for completion:

```bash
curl -sS localhost:8080/api/v1/backtests/bt_ef429e7f663c0191 \
  -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
```

### Everything else

`GET` requests are naturally idempotent and are not audited — recording them
would bury the writes that matter under a stream of dashboard polls. Every
non-`GET` request *is* audited, including the ones that failed:
`outcome` is `failure` at status 400 and above, because a security record that
holds only the successes is not one.

Alerts carry their own idempotency, one level down. Each has a `dedup_key`, and
the same condition in the same time bucket collapses to one alert. The durable
store holds a unique index on that key, so an in-memory dedup cache being wiped
cannot cause a duplicate page. See [`events.md`](./events.md) for how the same
mechanism protects event consumers.

---

## A short tour

Everything below runs against a local `make run` with `$TOKEN` set as above.

```bash
# Is it up, and is anything degraded?
curl -sS localhost:8080/healthz | python3 -m json.tool

# What environment are we in?
curl -sS localhost:8080/api/v1/market/regime -H "Authorization: Bearer $TOKEN" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["regime"], round(d["confidence"],3), "| runner-up:", d.get("secondary"))'

# The top of the ranked list, with why nothing was emitted.
curl -sS "localhost:8080/api/v1/stocks?limit=5" -H "Authorization: Bearer $TOKEN" \
  | python3 -c '
import json,sys
for r in json.load(sys.stdin)["data"]:
    print("%-6s opp=%5.1f  %-18s %s" % (r["ticker"], r["opportunity"], r["risk_decision"], r.get("rejection","")))'

# Why is this instrument not producing a signal? Read the risk verdict.
curl -sS localhost:8080/api/v1/risk/AAPL -H "Authorization: Bearer $TOKEN" \
  | python3 -c '
import json,sys
d = json.load(sys.stdin)["data"]
print(d["decision"], d["level"], d["score"])
for c in d["checks"]:
    print("  %-24s %-5s %s" % (c["name"], c["status"], c["reason"]))'

# Given a signal id, the complete causal record behind it.
curl -sS localhost:8080/api/v1/signals/$SIGNAL_ID/provenance \
  -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
```

The last one is the route worth knowing. `reproducible` is the field to read
first: when it is true, recomputing the prediction from the stored feature
snapshot with the named model artifact reproduces the stored distribution
exactly. When it is false, `missing` names the inputs that could not be
retrieved and `note` says so, rather than the record claiming a completeness it
does not have.

---

## Related documents

- [`openapi.yaml`](./openapi.yaml) — the machine-readable contract.
- [`events.md`](./events.md) — the sixteen event topics, the envelope, and how
  `causation_id` walks a decision back to the bar that caused it.
- [`../strategy/authoring.md`](../strategy/authoring.md) — how to write and
  validate a strategy.

# QuantOS dashboard

The Next.js + TypeScript front end for QuantOS — an **educational research and
paper-trading platform**. Nothing this dashboard displays is financial advice,
no probability it shows is a guarantee, and no order it can submit reaches a
real venue.

## Running it

### Against `make dev` (the full local stack)

`make dev` brings up Kafka, Postgres, ClickHouse, Redis and the observability
stack, then runs the API on `:8080`. From the repository root:

```bash
make dev            # in one shell — brings up the stack and waits
make frontend       # in another — this is `cd frontend && npm install && npm run dev`
```

The dashboard is then on <http://localhost:3000>. `http.cors_origins` in
`config/quantos.yaml` already lists `http://localhost:3000`, though the browser
never talks to the API cross-origin anyway — see **Credentials** below.

### Against `go run ./cmd/quantos run` (single process, no infrastructure)

```bash
# shell 1 — the platform, in-process bus and in-memory stores
cd /root/quantos
GOPROXY=direct GOSUMDB=off GOPRIVATE='*' GOFLAGS=-mod=mod go run ./cmd/quantos run

# shell 2 — the dashboard
cd frontend
npm install
npm run dev
```

Defaults point at `http://localhost:8080`, so no configuration is needed.

In this mode the platform runs with `store.driver: memory`, so the endpoints
backed by the persistent store — `/alerts`, `/stocks/{ticker}/candles`,
historical `/signals`, `/backtests`, `/audit`, `/market/regime/history` — report
themselves unavailable or empty. The dashboard says which of those it is on
every affected panel rather than rendering a blank chart.

### Against the fixtures, with no backend at all

```bash
npm run dev:mock
```

Serves everything from `mocks/`, which holds complete response envelopes
captured from a real run. See [`mocks/README.md`](mocks/README.md) for what each
fixture is and which ones are hand-built. The nav shows a standing "Mock data"
notice so a fixture is never mistaken for a live reading.

## Configuration

Every value has a working default; `.env.local` is an override sheet.

| Variable | Default | Meaning |
| --- | --- | --- |
| `QUANTOS_API_URL` | `http://localhost:8080` | Origin of the Go API. |
| `QUANTOS_API_TOKEN` | *(unset)* | A pre-issued bearer token. Preferred in any real deployment. |
| `QUANTOS_DEV_SUBJECT` | `operator` | Local principal, used only when no token is set. |
| `QUANTOS_DEV_PASSWORD` | `operator` | Its password. |
| `QUANTOS_USE_MOCKS` | *(unset)* | `1` serves `mocks/` instead of a backend. |
| `QUANTOS_REVALIDATE_SECONDS` | `5` | Server-side cache window for list reads. |

The dev defaults match `auth.dev_users` in `config/quantos.yaml`, which ships
`demo/demo` (viewer, analyst) and `operator/operator` (adds operator scopes).
`operator` is the default here because it additionally holds `audit:read`, which
the Settings page uses; with `demo` that one panel reports a 403 and explains why.

## Credentials — where the token lives, and where it does not

QuantOS authenticates with OAuth2/OIDC bearer tokens (`internal/auth`). **The
browser never receives one.**

```
browser ──/api/quantos/*──▶ Next.js server ──Authorization: Bearer ──▶ Go API
browser ──/api/stream ─────▶ Next.js server ──Authorization: Bearer ──▶ /api/v1/stream
```

- Server components call the Go API directly, with the token attached in the
  Node process (`src/lib/server/token.ts`).
- Client components call this app's own same-origin proxy
  (`src/app/api/quantos/[...path]/route.ts`), which carries an explicit
  allowlist of read routes. A `[...path]` proxy without one is an open relay:
  every endpoint the platform ever adds would become browser-reachable the day
  it ships.
- The SSE stream is proxied for the same reason. `EventSource` cannot set
  headers, which is why the Go hub also accepts `?access_token=` — this
  dashboard uses neither, because a token in a query string ends up in
  `document.location` and in every access log on the path.

The token is held in memory in the Next.js process, refreshed a minute before
expiry, and never serialised into a page, a prop, or a script-readable cookie.

## Live updates

`/api/v1/stream` is Server-Sent Events (`internal/httpx/sse.go`). The contract
has three parts the dashboard implements rather than ignores:

- **Resume.** Every real frame carries `id:`. The browser sends the highest one
  back as `Last-Event-ID` on reconnect and the hub replays from its bounded ring
  buffer. The proxy forwards that header untouched.
- **`resync`.** The hub emits this when the client fell outside the replay
  window. It means *the stream has a gap you cannot fill; refetch over REST*.
  The dashboard raises a visible "Resyncing" state, calls `router.refresh()` to
  refetch through the server components, and only clears the flag once that
  refetch resolves. Treating `resync` as informational is the specific bug this
  design exists to prevent — the UI would keep rendering state that silently
  missed events.
- **`overflow`.** The connection could not keep up and was dropped after events
  were already lost. It is treated as a gap too, identically to `resync`.

Neither control frame carries an `id:`, so neither advances `Last-Event-ID`, and
the reducer enforces that. Heartbeats arrive as SSE comment frames, which the
browser never surfaces as events, so liveness is tracked from the timestamp of
the last frame of any kind.

All of this reasoning lives in `src/lib/sse-reducer.ts` as a pure function, and
is covered by `src/lib/sse-reducer.test.ts`. `src/hooks/use-stream.ts` is only
plumbing.

## Commands

```bash
npm run dev          # dev server against a live API
npm run dev:mock     # dev server against mocks/
npm run build        # production build
npm start            # serve the production build
npm run typecheck    # tsc --noEmit, strict mode
npm run lint         # eslint via next lint
npm test             # vitest
```

## Layout

```
src/
  app/                      App Router pages, one per route
    api/quantos/[...path]/  same-origin read proxy (allowlisted)
    api/stream/             SSE proxy, forwards Last-Event-ID
    api/backtests/          backtest submission, validated field by field
  components/               charts (hand-written SVG), verdict badges, states
  hooks/                    use-stream (SSE), use-api (client fetch)
  lib/
    types.ts                mirror of internal/domain, internal/api, internal/httpx
    api-client.ts           the only place base URL and auth header are set
    format.ts               formatting, sample gating, distribution maths
    sse-reducer.ts          the stream state machine, pure and tested
    server/                 server-only: env, token, mock transport, platform status
mocks/                      captured response fixtures
```

Charts are hand-written SVG components with **no charting dependency**. That
keeps the bundle small, the CSP tight, and the build immune to a registry
outage.

## What the dashboard refuses to do

These are the product's safety requirements, expressed as things the code
declines rather than as copy in a footer.

- **A probability is always shown as a probability.** There is no code path that
  turns a distribution into a single directional verdict. Every distribution is
  rendered with its horizon and its flat band, because "UP 63%" without the band
  does not say up *by how much*, and is therefore not a claim at all.
- **Accuracy is never shown without its base rate.** Over three classes with a
  flat band, always guessing the most common class already scores about 40%.
  `accuracyVsBaseRate` returns the two together with their difference, or
  neither.
- **A rate over too few samples is not rendered as a number.** `sampleGate`
  returns the reason instead, and the UI prints the reason. "100% accurate over
  three observations" is a false statement, not a rounding choice.
- **Colour is never the only carrier of meaning.** Every verdict badge pairs its
  colour with a glyph and the full text of the verdict. A red/green risk
  decision fails WCAG 1.4.1 and fails a colour-blind reader outright.
- **`domain.Disclaimer` is part of the layout** wherever a signal, prediction or
  classification appears — and the text is taken from the API response, so what
  the reader sees is the string the server actually sent.
- **An empty chart is never drawn.** Every chart with no data renders an
  explicit message saying so. An axis with nothing between it is
  indistinguishable from a rendering failure.
- **Loading, empty, error, stale, degraded and disconnected are distinct
  states**, each with its own component, so a page cannot accidentally render
  nothing and pass it off as a result.
- **A degraded platform says so at the top of every page**, and a platform whose
  `/readyz` reports it cannot serve says that signal emission is suspended.
- **Provenance never overstates itself.** When `reproducible` is false the
  signal page leads with that and lists exactly which inputs are missing.

## Notes on the local run

Two things about a local `go run ./cmd/quantos run` are worth knowing before you
conclude the dashboard is broken.

**No signals are emitted.** Every risk assessment resolves to `WATCH_ONLY`,
because the `spread` check skips: the simulator timestamps quotes roughly 50
minutes ahead of wall clock, `marketdata.Validator` rejects all of them as
`future_timestamp`, and with no accepted two-sided quote the spread cannot be
evaluated. `domain.NewSignal` refuses to construct a signal unless the decision
is `ALLOW_PAPER_SIGNAL`, so the live signal list is legitimately empty. The
`/signals` page explains this and links to the per-instrument rejection reasons.
Use `npm run dev:mock` to see the provenance screen populated.

**No model is loaded.** The shipped artifact fails validation at load time
because its `metadata` block omits `test_samples`, `test_accuracy` and
`test_base_rate` — `mlinfer` refuses to serve a model whose accuracy is not
recorded next to its base-rate benchmark, which is the right refusal. The
platform therefore reports `degraded: ["model"]` and every prediction is
labelled `rules-fallback`. The dashboard surfaces that label on every prediction
rather than letting a deterministic prior read as a model output.

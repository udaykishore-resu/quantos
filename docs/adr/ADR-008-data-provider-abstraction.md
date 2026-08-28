# ADR-008 — Vendor-neutral market data provider abstraction

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

Market data vendors differ in coverage, symbology, adjustment policy, rate
limits, licensing, latency and failure behaviour — and they get replaced.
Coupling the platform to one vendor's SDK means a vendor change is a rewrite,
local development requires a paid key, and tests depend on a third party.

## Options

1. **Direct vendor SDK calls in services.** Fastest to write, worst to live with.
2. **Thin per-vendor wrapper, vendor types leaking through.** The usual
   compromise; the leak becomes the coupling.
3. **Domain-owned interface with vendor adapters.** Chosen.

## Decision

A single narrow interface, owned by the domain, in `internal/marketdata`:

```go
type Provider interface {
    Name() string
    Capabilities() Capabilities
    GetQuotes(ctx, []string) ([]domain.Quote, error)
    GetHistoricalBars(ctx, BarRequest) ([]domain.Candle, error)
    GetTrades(ctx, TradeRequest) ([]domain.Trade, error)
    GetMarketStatus(ctx) (domain.MarketStatus, error)
    GetCorporateEvents(ctx, EventRequest) ([]domain.CorporateEvent, error)
}
```

Plus an optional `StreamProvider` for push feeds, so a polling-only vendor does
not have to fake a stream.

**Capabilities, not assumptions.** `Capabilities()` declares supported bar
intervals, history depth, whether prices are split/dividend adjusted, tick
availability, corporate-event coverage and rate limits. Callers negotiate
against capabilities instead of assuming; asking for something unsupported
returns `ErrUnsupported`, not silently wrong data.

**Normalisation is the adapter's job.** Every adapter must emit canonical
`domain` types: UTC timestamps, a normalised ticker (`BRK.B`, not `BRK-B` or
`BRK/B`), explicit adjustment status, and explicit currency. Symbology mapping
lives in the adapter, not in business logic.

**Shipped implementations:**

- `marketdata/sim` — deterministic simulator. A regime-driven price process
  (drift + stochastic volatility + fat-tailed jumps + intraday volume smile),
  seeded, reproducible, with scriptable scenario injection (bullish shift, vol
  spike, breakout with volume expansion, gap on news). This is what makes
  the demo scenarios and the whole platform work with **no paid API**.
- `marketdata/replay` — replays CSV/Parquet history at configurable speed
  (including "as fast as possible") with correct inter-event spacing.
- `marketdata/composite` — ordered failover with per-provider circuit breakers
  and a health scorer; falls back on error, timeout or capability gap.
- `marketdata/cached` — decorator adding Redis caching with per-method TTLs and
  single-flight de-duplication of concurrent identical requests.
- `marketdata/ratelimited` — decorator with a token bucket per provider.

A real vendor adapter is ~200 lines against this interface; none ships here
because none is needed to run the platform, and adding one must not be required
to evaluate it.

**Validation is not the provider's job.** `marketdata.Validator` wraps any
provider and enforces: positive prices, `bid <= ask`, sane spread, no absurd
gaps versus the previous close, monotonic timestamps, and freshness. Rejects go
to `market.rejected` with a reason.

## Trade-offs

- (+) Vendor swap is one adapter plus config; no business-logic change.
- (+) Full local development and CI without credentials or network.
- (+) Failover, caching and rate limiting compose as decorators rather than
  being reimplemented per vendor.
- (−) The interface is a lowest-common-denominator: vendor-specific extras
  (order-book depth, sentiment feeds) need either a capability flag or an
  optional interface. Accepted; `Capabilities` plus optional interfaces covers
  it without widening the core.
- (−) Normalisation bugs are concentrated in adapters and can be subtle
  (adjustment policy especially). Mitigated by a shared adapter conformance
  suite in `internal/marketdata/conformance` that every adapter must pass.
- (−) The simulator is not the market. It is explicitly a development and
  demonstration tool; no performance claim is derived from simulated data.

## Failure modes

- *Provider timeout.* Per-call deadline; circuit breaker opens after N failures;
  composite fails over; if all providers fail, data goes stale and the staleness
  gate suspends signal generation (governance G-5).
- *Silent bad data.* The validator quarantines; a rejection-rate alert fires.
- *Adjustment mismatch between providers on failover.* `Capabilities.Adjusted`
  is compared; mixing adjusted and unadjusted history is refused rather than
  concatenated.
- *Rate-limit exhaustion.* Token bucket sheds with a typed error; the cache
  absorbs repeat reads; the scheduler backs off with jitter.

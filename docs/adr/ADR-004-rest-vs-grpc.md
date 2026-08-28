# ADR-004 — REST+SSE at the edge, gRPC between services

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

Two different consumers with different needs. The dashboard is a browser: it
wants JSON, cookies/bearer tokens, CORS, cacheable GETs, and a streaming channel
that survives corporate proxies. Internal services want low-latency, typed,
schema-evolvable calls with deadlines, streaming and cheap serialisation.

## Options

1. **REST everywhere.** Simplest. Internal calls pay JSON cost and lose
   compile-time contract enforcement. At our internal call volume (feature
   snapshot → inference, per symbol per tick) the JSON tax is real but not fatal.
2. **gRPC everywhere, grpc-web at the edge.** Uniform, but grpc-web needs a
   proxy, streaming semantics through it are fiddly, and browser debuggability
   drops sharply.
3. **REST + SSE at the edge, gRPC internally.** Chosen.
4. **GraphQL at the edge.** The dashboard's queries are known and few; the
   flexibility does not pay for the complexity, and per-field authorisation on
   research data is a liability we do not need.

## Decision

**Edge (browser ⇄ api-gateway):** REST/JSON over HTTP/1.1+2, documented by an
OpenAPI 3.1 spec (`docs/api/openapi.yaml`) that is the contract test's source of
truth. Real-time updates use **Server-Sent Events** at `/api/v1/stream`, not
WebSocket — see below.

**Internal (service ⇄ service):** gRPC with protobuf
(`docs/api/proto/quantos/v1/*.proto`), deadlines propagated from the inbound
request, OTel interceptors on both sides, and `grpc-health-probe` for
Kubernetes.

**Asynchronous fan-out** stays on Kafka (ADR-002). gRPC is used only for
synchronous request/response where a caller genuinely needs an answer now:
gateway → prediction/backtest/portfolio queries, and signal-service →
inference.

### Why SSE rather than WebSocket for the dashboard

The dashboard's real-time need is strictly server → client (prices, signals,
alerts, prediction and regime changes). SSE gives us: plain HTTP so every proxy
and load balancer handles it; automatic browser reconnection with
`Last-Event-ID` for gap-free resume; trivial authentication with the same bearer
token as REST; and no separate protocol to secure or scale. The one thing SSE
cannot do — client → server streaming — we do not need; client actions are
ordinary POSTs. The implementation keeps a WebSocket adapter behind the same
`stream.Hub` interface should a future feature (collaborative annotation)
require duplex.

## Trade-offs

- (+) Browser path is debuggable with `curl`, cacheable, and proxy-friendly.
- (+) Internal path is typed, fast, and evolves safely with protobuf rules.
- (+) SSE resume semantics give the dashboard correctness on flaky networks.
- (−) Two IDLs to maintain (OpenAPI + proto) and a mapping layer between them.
  Mitigated by generating both from the Go domain types where practical and by a
  contract test that fails if they diverge.
- (−) SSE is limited to ~6 concurrent connections per origin on HTTP/1.1.
  Mitigated by HTTP/2 (single multiplexed connection) and by one stream endpoint
  with topic subscriptions rather than one endpoint per topic.
- (−) gRPC adds a proto toolchain to CI.

## Failure modes

- *SSE connection loss.* Client reconnects with `Last-Event-ID`; the hub replays
  from a bounded ring buffer (default 1024 events, 60 s). Beyond the buffer the
  client receives `event: resync` and refetches state via REST.
- *Slow consumer.* Per-connection bounded queue; on overflow the hub drops the
  connection with `event: overflow` rather than growing unbounded.
- *gRPC deadline exhaustion.* Deadlines are propagated and always set; a missing
  deadline is rejected by a server interceptor.
- *Version skew.* Proto changes are additive-only; a CI check runs
  `buf breaking` against the main branch.

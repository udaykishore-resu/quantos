# ADR-011 — Monorepo with one Go module and thin service binaries

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

QuantOS is ten Go services sharing one domain model, one event envelope, one
feature engine and one set of engines. Splitting them into separate repositories
or modules means every domain change is a multi-repo, multi-PR, version-bumping
exercise — and the platform's correctness depends on those types staying in
lockstep (a `Signal` written by one service and read by another must be the same
`Signal`).

## Options

1. **Repo per service.** Independent lifecycles and clear ownership. Costs:
   shared-library versioning, cross-repo atomic changes are impossible, CI
   duplication, and a real risk of skewed domain types between producer and
   consumer.
2. **Monorepo, module per service, `go.work`.** Some isolation, but the
   versioning tax reappears at the module boundary for shared code, and
   `replace` directives proliferate.
3. **Monorepo, one module, services as `main` packages.** Chosen.

## Decision

One module, `github.com/udaykishoreresu/quantos`.

```
services/<name>/main.go     package main — flags, config, wiring, lifecycle only
internal/<domain>/          all logic, importable only within the module
```

Rules that keep this from degenerating into a ball of mud:

1. **Service `main` packages contain no business logic.** They parse config,
   build dependencies, register handlers and manage shutdown. A CI check caps
   `services/**` at 400 lines per file and forbids importing another service.
2. **`internal/` packages form a DAG.** `domain` imports nothing of ours.
   Engines import `domain` and their own inputs, never each other's internals.
   `tests/integration/architecture_test.go` asserts the layering and the
   LLM-isolation rule from ADR-005.
3. **Each engine is independently constructible and testable** — no global
   state, no `init()` side effects, dependencies injected.
4. **The frontend and the Python ML tree are separate toolchains** in the same
   repository, with their own CI lanes and their own lockfiles.
5. **Docker builds are per-service** from a shared multi-stage base; only the
   target binary ships in the final image (`FROM gcr.io/distroless/static`).

## Trade-offs

- (+) Atomic cross-service changes: one PR changes the envelope and every
  producer and consumer, with CI proving the whole thing still builds and passes.
- (+) No internal versioning tax; no `replace` directives.
- (+) One `go test ./...` covers the platform.
- (+) `internal/` genuinely prevents accidental external coupling.
- (−) A change in a core package rebuilds and retests everything. Mitigated by
  Go's build cache and by CI test sharding; full suite target < 5 minutes.
- (−) Ownership boundaries are social rather than mechanical. Mitigated by
  `CODEOWNERS` per directory.
- (−) The repository grows large. Accepted; it is one product.
- (−) All services share a Go version and dependency set — an upgrade is
  all-or-nothing. Accepted, and arguably a feature.

## Failure modes

- *Layering erosion.* Caught by the architecture test, which fails the build
  rather than filing a ticket.
- *Service `main` accreting logic.* Caught by the line-count and import checks.
- *Dependency upgrade breaking one service.* Caught by the single test suite
  before merge, which is the point.
- *CI time creep.* Monitored; sharding and `-run` selection by changed package
  are the levers.

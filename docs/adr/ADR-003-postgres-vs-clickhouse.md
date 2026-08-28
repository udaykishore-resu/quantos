# ADR-003 — PostgreSQL and ClickHouse, not one or the other

- **Status:** Accepted
- **Date:** 2026-08-27

## Problem

QuantOS has two data shapes. One is small, relational, mutable and needs
constraints: strategies, signals and their lifecycle, risk assessments, paper
orders and positions, portfolios, model registry, users, audit log. The other is
enormous, append-only and read in wide analytical scans: quotes, trades, bars,
feature snapshots, prediction records.

Storing ticks in Postgres works until it doesn't: a year of 1-minute bars for
500 symbols is ~50M rows (fine), but tick quotes at 10/s/symbol is ~65B
rows/year (not fine), and calibration analysis wants `SELECT ... GROUP BY
regime, bucket(probability)` over hundreds of millions of predictions.

## Options

1. **Postgres only, with TimescaleDB.** Attractive — one engine, hypertables,
   continuous aggregates. Rejected because the analytical scans we care about
   (full-history calibration, regime-partitioned backtests) are 5–20× slower
   than a columnar store at our volumes, and because compression tuning becomes
   an ongoing operational tax.
2. **ClickHouse only.** Superb analytics. Poor fit for the transactional half:
   no real foreign keys, `ALTER ... UPDATE` is a mutation not a transaction, and
   the signal lifecycle (generated → active → invalidated → evaluated) genuinely
   wants row-level updates with isolation.
3. **Postgres + ClickHouse.** Chosen.
4. **Postgres + Parquet on S3 + DuckDB.** Cheapest, and excellent for research.
   Rejected as the *serving* store because the dashboard needs interactive
   latency over recent history without a query engine cold start. Parquet on S3
   remains the archival tier.

## Decision

- **PostgreSQL 16** is the system of record for transactional, relational,
  mutable state and for anything that needs a foreign key or a unique
  constraint — including the idempotency indexes for alerts and paper orders.
  Schema in `deploy/sql/postgres/`.
- **ClickHouse 24** stores append-only time series and analytical records:
  `quotes`, `trades`, `bars`, `feature_snapshots`, `predictions`, `alerts_history`.
  `MergeTree` ordered by `(ticker, ts)`, partitioned by month, with TTL to S3.
  Schema in `deploy/sql/clickhouse/`.
- **Redis 7** is a cache and a coordination primitive, never a source of truth:
  latest quote per symbol, latest regime, rate-limit buckets, the dedup set, and
  hot feature snapshots.
- **S3** holds historical datasets (Parquet), model artifacts and backtest
  reports.

**The boundary rule:** if losing a row would corrupt a *decision's provenance*,
it goes in Postgres. If losing a row would only blur a *statistic*, it goes in
ClickHouse. Signals, risk assessments and model versions are provenance.
Individual quotes are statistics — with the exception of the feature snapshot,
which is written to ClickHouse *and* whose SHA-256 hash is written to Postgres
on the signal row, so provenance survives even if the snapshot is lost.

## Trade-offs

- (+) Each engine does what it is good at; neither is tuned into a shape it hates.
- (+) Analytical queries never contend with the transactional path.
- (−) Two engines to operate, back up, upgrade and monitor. Two client libraries.
- (−) Cross-store joins must happen in application code (e.g. signal row in
  Postgres + snapshot in ClickHouse). Accepted; the join is by primary key and
  is done in `store.Provenance`.
- (−) Eventual consistency between the stores. Bounded by the writer: the
  ClickHouse write happens *before* the Postgres signal insert, so a signal row
  never references a snapshot that does not exist.

## Failure modes

- *Postgres down.* Signal emission is disabled (cannot persist provenance ⇒
  G-4/§50 traceability cannot be honoured). Reads degrade to Redis/ClickHouse
  with `degraded: ["postgres"]` in the response envelope. Writes queue to the WAL.
- *ClickHouse down.* Live path unaffected. Feature/prediction writes buffer in a
  bounded in-memory queue then spill to the WAL; analytics endpoints return 503
  with `Retry-After`.
- *Redis down.* Everything falls through to the source of truth at higher
  latency; rate limiting fails closed for mutating routes and open for reads;
  dedup falls back to the Postgres unique index.
- *Divergence between stores.* A nightly reconciliation job compares
  `signals.feature_hash` against `feature_snapshots` and reports orphans.

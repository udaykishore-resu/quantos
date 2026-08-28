-- QuantOS analytical schema.
--
-- This is the append-only tier (ADR-003): high-volume series read in wide
-- scans. Nothing here is provenance — losing a row blurs a statistic, it does
-- not make a decision unexplainable — with one deliberate exception, the
-- feature snapshot, whose hash is also stored on the signal row in PostgreSQL
-- so the chain survives even if the snapshot itself is lost.
--
-- Ordering keys are (ticker, ts) throughout, because every query the platform
-- issues filters on a symbol and a time window.

CREATE DATABASE IF NOT EXISTS quantos;

-- ---------------------------------------------------------------- quotes ---

CREATE TABLE IF NOT EXISTS quantos.quotes
(
    ticker    LowCardinality(String),
    ts        DateTime64(3, 'UTC'),
    bid       Float64,
    ask       Float64,
    last      Float64,
    bid_size  Float64,
    ask_size  Float64,
    volume    Float64,
    seq       UInt64,
    source    LowCardinality(String),
    -- Derived at insert so the spread does not have to be recomputed on every
    -- analytical scan.
    spread_bps Float64 MATERIALIZED if(bid > 0 AND ask > 0, (ask - bid) / ((ask + bid) / 2) * 10000, nan)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (ticker, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;

-- ------------------------------------------------------------------ bars ---

CREATE TABLE IF NOT EXISTS quantos.bars
(
    ticker    LowCardinality(String),
    interval  LowCardinality(String),
    start     DateTime64(3, 'UTC'),
    end       DateTime64(3, 'UTC'),
    open      Float64,
    high      Float64,
    low       Float64,
    close     Float64,
    volume    Float64,
    vwap      Float64,
    trades    Int64,
    adjusted  UInt8,
    source    LowCardinality(String)
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(start)
ORDER BY (ticker, interval, start)
TTL toDateTime(start) + INTERVAL 3 YEAR
SETTINGS index_granularity = 8192;

-- ----------------------------------------------------- feature snapshots ---

CREATE TABLE IF NOT EXISTS quantos.feature_snapshots
(
    ticker     LowCardinality(String),
    as_of      DateTime64(3, 'UTC'),
    interval   LowCardinality(String),
    -- The content hash a signal stores. This is the reproducibility lookup.
    hash       String,
    last       Float64,
    bid        Float64,
    ask        Float64,
    volume     Float64,
    spread_bps Float64,
    stale      UInt8,
    values     Map(String, Float64),
    warm       Map(String, UInt8)
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(as_of)
ORDER BY (ticker, as_of, hash)
TTL toDateTime(as_of) + INTERVAL 1 YEAR
SETTINGS index_granularity = 8192;

-- A hash lookup must not scan a partition: this index makes
-- "reproduce this decision" an O(1)-ish read.
ALTER TABLE quantos.feature_snapshots
    ADD INDEX IF NOT EXISTS feature_hash_idx hash TYPE bloom_filter(0.01) GRANULARITY 4;

-- ----------------------------------------------------------- predictions ---

CREATE TABLE IF NOT EXISTS quantos.predictions
(
    id            String,
    ticker        LowCardinality(String),
    created_at    DateTime64(3, 'UTC'),
    model_version LowCardinality(String),
    source        LowCardinality(String),
    feature_hash  String,
    regime        LowCardinality(String),
    primary_up    Float64,
    primary_flat  Float64,
    primary_down  Float64,
    confidence    Float64,
    -- The complete record, so a stored prediction can be replayed exactly.
    payload       String
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (ticker, created_at, id)
TTL toDateTime(created_at) + INTERVAL 1 YEAR
SETTINGS index_granularity = 8192;

ALTER TABLE quantos.predictions
    ADD INDEX IF NOT EXISTS prediction_id_idx id TYPE bloom_filter(0.01) GRANULARITY 4;

-- --------------------------------------------------- prediction outcomes ---

CREATE TABLE IF NOT EXISTS quantos.prediction_outcomes
(
    prediction_id String,
    ticker        LowCardinality(String),
    horizon       LowCardinality(String),
    created_at    DateTime64(3, 'UTC'),
    resolved_at   DateTime64(3, 'UTC'),
    entry_price   Float64,
    exit_price    Float64,
    return_bps    Float64,
    predicted     LowCardinality(String),
    actual        LowCardinality(String),
    correct       UInt8,
    brier_score   Float64,
    log_loss      Float64,
    regime        LowCardinality(String),
    model_version LowCardinality(String),
    -- Blocked predictions are scored too: without them the cost of the risk
    -- veto is unmeasurable (ADR-009).
    risk_decision LowCardinality(String),
    p_up          Float64,
    p_flat        Float64,
    p_down        Float64
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(resolved_at)
ORDER BY (model_version, horizon, resolved_at, prediction_id)
TTL toDateTime(resolved_at) + INTERVAL 3 YEAR
SETTINGS index_granularity = 8192;

-- --------------------------------------------------------- alert history ---

CREATE TABLE IF NOT EXISTS quantos.alerts_history
(
    id         String,
    dedup_key  String,
    type       LowCardinality(String),
    severity   LowCardinality(String),
    ticker     LowCardinality(String),
    created_at DateTime64(3, 'UTC'),
    title      String,
    message    String,
    status     LowCardinality(String),
    signal_id  String
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (created_at, id)
TTL toDateTime(created_at) + INTERVAL 1 YEAR;

-- ------------------------------------------------------ materialised views -

-- Rolling calibration by model, horizon and predicted-probability bucket.
-- Computing this on read over hundreds of millions of rows is exactly the
-- workload that justifies a columnar store; computing it incrementally is
-- what makes the model-health page load in milliseconds.
CREATE TABLE IF NOT EXISTS quantos.calibration_daily
(
    day           Date,
    model_version LowCardinality(String),
    horizon       LowCardinality(String),
    regime        LowCardinality(String),
    bucket        UInt8,
    n             AggregateFunction(count, UInt8),
    mean_pred     AggregateFunction(avg, Float64),
    observed      AggregateFunction(avg, UInt8)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (model_version, horizon, regime, day, bucket);

CREATE MATERIALIZED VIEW IF NOT EXISTS quantos.calibration_daily_mv
TO quantos.calibration_daily
AS SELECT
    toDate(resolved_at)                       AS day,
    model_version,
    horizon,
    regime,
    toUInt8(least(9, floor(p_up * 10)))       AS bucket,
    countState(toUInt8(1))                    AS n,
    avgState(p_up)                            AS mean_pred,
    avgState(toUInt8(actual = 'UP'))          AS observed
FROM quantos.prediction_outcomes
GROUP BY day, model_version, horizon, regime, bucket;

-- Daily accuracy by regime, which is the report that actually matters:
-- a model that is excellent in a trend and useless in a range is not a
-- 60%-accurate model, it is two different models wearing one name.
CREATE TABLE IF NOT EXISTS quantos.accuracy_by_regime_daily
(
    day           Date,
    model_version LowCardinality(String),
    horizon       LowCardinality(String),
    regime        LowCardinality(String),
    n             AggregateFunction(count, UInt8),
    correct       AggregateFunction(avg, UInt8),
    brier         AggregateFunction(avg, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (model_version, horizon, regime, day);

CREATE MATERIALIZED VIEW IF NOT EXISTS quantos.accuracy_by_regime_mv
TO quantos.accuracy_by_regime_daily
AS SELECT
    toDate(resolved_at)     AS day,
    model_version,
    horizon,
    regime,
    countState(toUInt8(1))  AS n,
    avgState(correct)       AS correct,
    avgState(brier_score)   AS brier
FROM quantos.prediction_outcomes
GROUP BY day, model_version, horizon, regime;

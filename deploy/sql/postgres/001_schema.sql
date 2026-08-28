-- QuantOS transactional schema.
--
-- This is the provenance store (ADR-003): everything whose loss would make a
-- decision unexplainable. Two properties are enforced by the database rather
-- than by application code, because an index survives a cache wipe, a redeploy
-- and a careless refactor:
--
--   * alerts.dedup_key is unique          -> no duplicate alert can ever exist
--   * paper_orders.idempotency_key unique -> no duplicate paper order
--
-- Large nested structures are JSONB. They are written once, read whole, and
-- never queried field by field, so normalising them would buy nothing and cost
-- a join on every read.

BEGIN;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- --------------------------------------------------------------- stocks ----

CREATE TABLE IF NOT EXISTS stocks (
    ticker          TEXT PRIMARY KEY,
    company         TEXT NOT NULL,
    sector          TEXT,
    industry        TEXT,
    exchange        TEXT,
    asset_class     TEXT,
    currency        TEXT NOT NULL DEFAULT 'USD',
    market_cap      DOUBLE PRECISION,
    shares_out      DOUBLE PRECISION,
    avg_volume      DOUBLE PRECISION,
    avg_notional    DOUBLE PRECISION,
    beta            DOUBLE PRECISION,
    -- Point-in-time membership: a backtest asking for 2019 must see the 2019
    -- universe, including names that have since been delisted.
    listed_at       TIMESTAMPTZ,
    delisted_at     TIMESTAMPTZ,
    tags            JSONB NOT NULL DEFAULT '[]'::jsonb,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS stocks_sector_idx ON stocks (sector);
CREATE INDEX IF NOT EXISTS stocks_active_idx ON stocks (delisted_at) WHERE delisted_at IS NULL;

-- ---------------------------------------------------------- fundamentals ---

CREATE TABLE IF NOT EXISTS fundamentals (
    ticker              TEXT NOT NULL REFERENCES stocks(ticker) ON DELETE CASCADE,
    period_end          TIMESTAMPTZ NOT NULL,
    -- filed_at is what makes point-in-time correctness possible: only rows
    -- filed on or before a decision time are visible to it.
    filed_at            TIMESTAMPTZ NOT NULL,
    period              TEXT NOT NULL,
    currency            TEXT NOT NULL DEFAULT 'USD',
    revenue             DOUBLE PRECISION,
    revenue_yoy         DOUBLE PRECISION,
    gross_profit        DOUBLE PRECISION,
    operating_income    DOUBLE PRECISION,
    net_income          DOUBLE PRECISION,
    eps                 DOUBLE PRECISION,
    eps_yoy             DOUBLE PRECISION,
    free_cash_flow      DOUBLE PRECISION,
    operating_cash_flow DOUBLE PRECISION,
    capex               DOUBLE PRECISION,
    total_assets        DOUBLE PRECISION,
    total_equity        DOUBLE PRECISION,
    total_debt          DOUBLE PRECISION,
    cash                DOUBLE PRECISION,
    current_assets      DOUBLE PRECISION,
    current_liabilities DOUBLE PRECISION,
    invested_capital    DOUBLE PRECISION,
    shares_diluted      DOUBLE PRECISION,
    restated            BOOLEAN NOT NULL DEFAULT false,
    source              TEXT,
    PRIMARY KEY (ticker, period_end, filed_at)
);

CREATE INDEX IF NOT EXISTS fundamentals_pit_idx ON fundamentals (ticker, filed_at DESC);

-- --------------------------------------------------------------- regimes ---

CREATE TABLE IF NOT EXISTS regimes (
    as_of       TIMESTAMPTZ PRIMARY KEY,
    regime      TEXT NOT NULL,
    confidence  DOUBLE PRECISION NOT NULL,
    volatility  DOUBLE PRECISION,
    breadth     DOUBLE PRECISION,
    momentum    DOUBLE PRECISION,
    changed     BOOLEAN NOT NULL DEFAULT false,
    payload     JSONB NOT NULL
);

CREATE INDEX IF NOT EXISTS regimes_changed_idx ON regimes (as_of DESC) WHERE changed;

-- ------------------------------------------------------ risk assessments ---

CREATE TABLE IF NOT EXISTS risk_assessments (
    id            TEXT PRIMARY KEY,
    ticker        TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    decision      TEXT NOT NULL CHECK (decision IN ('ALLOW_PAPER_SIGNAL','WATCH_ONLY','BLOCK')),
    level         TEXT NOT NULL,
    score         DOUBLE PRECISION NOT NULL,
    -- Every individual check with its numeric evidence, so a historical veto
    -- can be explained precisely rather than paraphrased.
    checks        JSONB NOT NULL,
    blockers      JSONB NOT NULL DEFAULT '[]'::jsonb,
    warnings      JSONB NOT NULL DEFAULT '[]'::jsonb,
    config_hash   TEXT NOT NULL,
    regime        TEXT,
    feature_hash  TEXT,
    prediction_id TEXT
);

CREATE INDEX IF NOT EXISTS risk_ticker_time_idx ON risk_assessments (ticker, created_at DESC);
CREATE INDEX IF NOT EXISTS risk_decision_idx ON risk_assessments (decision, created_at DESC);

-- --------------------------------------------------------------- signals ---

CREATE TABLE IF NOT EXISTS signals (
    id                  TEXT PRIMARY KEY,
    ticker              TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    side                TEXT NOT NULL CHECK (side IN ('LONG','SHORT')),
    status              TEXT NOT NULL CHECK (status IN ('ACTIVE','INVALIDATED','EXPIRED','COMPLETED')),
    strength            DOUBLE PRECISION NOT NULL,
    confidence          DOUBLE PRECISION NOT NULL,
    horizon             TEXT NOT NULL,
    entry_reference     DOUBLE PRECISION NOT NULL,
    stop_reference      DOUBLE PRECISION NOT NULL,
    target_reference    DOUBLE PRECISION NOT NULL,
    suggested_weight    DOUBLE PRECISION NOT NULL,
    -- A signal without invalidation conditions is forbidden (governance G-6),
    -- and the constraint says so rather than trusting the writer.
    invalidations       JSONB NOT NULL CHECK (jsonb_array_length(invalidations) > 0),
    strategy_id         TEXT NOT NULL,
    rules_fired         JSONB NOT NULL DEFAULT '[]'::jsonb,
    evidence            JSONB NOT NULL DEFAULT '[]'::jsonb,
    counter_evidence    JSONB NOT NULL DEFAULT '[]'::jsonb,
    prediction_id       TEXT,
    -- The risk assessment is not optional: a signal that cannot name the
    -- assessment that allowed it is not a signal this platform produced.
    risk_assessment_id  TEXT NOT NULL REFERENCES risk_assessments(id),
    feature_hash        TEXT NOT NULL,
    regime              TEXT,
    model_version       TEXT NOT NULL,
    config_hash         TEXT NOT NULL,
    correlation_id      TEXT,
    invalidated_at      TIMESTAMPTZ,
    invalidation_kind   TEXT,
    invalidation_note   TEXT,
    explanation         TEXT,
    explanation_source  TEXT
);

CREATE INDEX IF NOT EXISTS signals_ticker_time_idx ON signals (ticker, created_at DESC);
CREATE INDEX IF NOT EXISTS signals_status_idx ON signals (status, created_at DESC);
CREATE INDEX IF NOT EXISTS signals_strategy_idx ON signals (strategy_id, created_at DESC);
CREATE INDEX IF NOT EXISTS signals_correlation_idx ON signals (correlation_id);

-- ---------------------------------------------------------------- alerts ---

CREATE TABLE IF NOT EXISTS alerts (
    id             TEXT PRIMARY KEY,
    -- The uniqueness that makes "no duplicate alerts" a guarantee rather than
    -- an aspiration. It survives a Redis wipe, which the in-memory dedup does not.
    dedup_key      TEXT NOT NULL UNIQUE,
    type           TEXT NOT NULL,
    severity       TEXT NOT NULL,
    ticker         TEXT,
    created_at     TIMESTAMPTZ NOT NULL,
    title          TEXT NOT NULL,
    message        TEXT NOT NULL,
    before_values  JSONB,
    after_values   JSONB,
    regime         TEXT,
    risk_level     TEXT,
    status         TEXT,
    signal_id      TEXT,
    prediction_id  TEXT,
    correlation_id TEXT
);

CREATE INDEX IF NOT EXISTS alerts_time_idx ON alerts (created_at DESC);
CREATE INDEX IF NOT EXISTS alerts_ticker_idx ON alerts (ticker, created_at DESC);
CREATE INDEX IF NOT EXISTS alerts_severity_idx ON alerts (severity, created_at DESC);

-- ------------------------------------------------------------ portfolios ---

CREATE TABLE IF NOT EXISTS portfolios (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    owner           TEXT,
    currency        TEXT NOT NULL DEFAULT 'USD',
    as_of           TIMESTAMPTZ NOT NULL,
    starting_cash   DOUBLE PRECISION NOT NULL,
    cash            DOUBLE PRECISION NOT NULL,
    equity          DOUBLE PRECISION NOT NULL,
    market_value    DOUBLE PRECISION NOT NULL,
    realized_pnl    DOUBLE PRECISION NOT NULL,
    unrealized_pnl  DOUBLE PRECISION NOT NULL,
    total_pnl       DOUBLE PRECISION NOT NULL,
    return_pct      DOUBLE PRECISION NOT NULL,
    gross_exposure  DOUBLE PRECISION NOT NULL,
    net_exposure    DOUBLE PRECISION NOT NULL,
    leverage        DOUBLE PRECISION NOT NULL,
    peak_equity     DOUBLE PRECISION NOT NULL,
    drawdown        DOUBLE PRECISION NOT NULL,
    max_drawdown    DOUBLE PRECISION NOT NULL,
    sector_exposure JSONB NOT NULL DEFAULT '{}'::jsonb,
    positions       JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- There is no real-money portfolio in this system and there is not going
    -- to be one (governance G-8). The constraint is a statement of intent.
    paper           BOOLEAN NOT NULL DEFAULT true CHECK (paper)
);

CREATE TABLE IF NOT EXISTS paper_orders (
    id              TEXT PRIMARY KEY,
    portfolio_id    TEXT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    -- Replaying signal.generated must never open a second position.
    idempotency_key TEXT NOT NULL UNIQUE,
    ticker          TEXT NOT NULL,
    side            TEXT NOT NULL CHECK (side IN ('BUY','SELL')),
    type            TEXT NOT NULL,
    quantity        DOUBLE PRECISION NOT NULL CHECK (quantity > 0),
    limit_price     DOUBLE PRECISION,
    stop_price      DOUBLE PRECISION,
    time_in_force   TEXT,
    status          TEXT NOT NULL,
    filled_qty      DOUBLE PRECISION NOT NULL DEFAULT 0,
    avg_fill_price  DOUBLE PRECISION NOT NULL DEFAULT 0,
    commission      DOUBLE PRECISION NOT NULL DEFAULT 0,
    slippage_bps    DOUBLE PRECISION NOT NULL DEFAULT 0,
    reject_reason   TEXT,
    signal_id       TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    fills           JSONB NOT NULL DEFAULT '[]'::jsonb
);

CREATE INDEX IF NOT EXISTS paper_orders_ticker_idx ON paper_orders (ticker, created_at DESC);
CREATE INDEX IF NOT EXISTS paper_orders_signal_idx ON paper_orders (signal_id);

-- ----------------------------------------------------------- model registry -

CREATE TABLE IF NOT EXISTS model_versions (
    id              TEXT PRIMARY KEY,
    model_id        TEXT NOT NULL,
    version         TEXT NOT NULL,
    family          TEXT NOT NULL,
    stage           TEXT NOT NULL CHECK (stage IN ('shadow','canary','active','retired')),
    artifact_uri    TEXT NOT NULL,
    -- Content addressing: an artifact that does not hash to this is not the
    -- artifact that produced the recorded predictions.
    artifact_sha    TEXT NOT NULL,
    features        JSONB NOT NULL,
    horizon         TEXT NOT NULL,
    flat_band_bps   DOUBLE PRECISION NOT NULL,
    train_start     TIMESTAMPTZ,
    train_end       TIMESTAMPTZ,
    valid_start     TIMESTAMPTZ,
    valid_end       TIMESTAMPTZ,
    test_start      TIMESTAMPTZ,
    test_end        TIMESTAMPTZ,
    trained_at      TIMESTAMPTZ NOT NULL,
    registered_at   TIMESTAMPTZ NOT NULL,
    promoted_at     TIMESTAMPTZ,
    training_commit TEXT,
    hyperparameters JSONB,
    canary_fraction DOUBLE PRECISION NOT NULL DEFAULT 0,
    out_of_sample   JSONB,
    by_regime       JSONB,
    notes           TEXT,
    UNIQUE (model_id, version)
);

-- At most one active version per model, enforced by the database so a bad
-- deploy cannot leave two models serving the same id.
CREATE UNIQUE INDEX IF NOT EXISTS model_versions_one_active
    ON model_versions (model_id) WHERE stage = 'active';

CREATE TABLE IF NOT EXISTS model_evaluations (
    id            BIGSERIAL PRIMARY KEY,
    model_version TEXT NOT NULL,
    window_name   TEXT NOT NULL,
    regime        TEXT,
    horizon       TEXT,
    samples       INTEGER NOT NULL,
    accuracy      DOUBLE PRECISION NOT NULL,
    base_rate     DOUBLE PRECISION NOT NULL,
    brier_score   DOUBLE PRECISION NOT NULL,
    log_loss      DOUBLE PRECISION NOT NULL,
    computed_at   TIMESTAMPTZ NOT NULL,
    payload       JSONB NOT NULL
);

CREATE INDEX IF NOT EXISTS model_eval_idx ON model_evaluations (model_version, computed_at DESC);

CREATE TABLE IF NOT EXISTS model_drift (
    id               BIGSERIAL PRIMARY KEY,
    model_version    TEXT NOT NULL,
    computed_at      TIMESTAMPTZ NOT NULL,
    severity         TEXT NOT NULL,
    max_feature_psi  DOUBLE PRECISION,
    prediction_drift DOUBLE PRECISION,
    accuracy_delta   DOUBLE PRECISION,
    payload          JSONB NOT NULL
);

CREATE INDEX IF NOT EXISTS model_drift_idx ON model_drift (model_version, computed_at DESC);

-- -------------------------------------------------------------- backtests --

CREATE TABLE IF NOT EXISTS backtest_runs (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    status        TEXT NOT NULL,
    strategy_id   TEXT NOT NULL,
    start_at      TIMESTAMPTZ NOT NULL,
    end_at        TIMESTAMPTZ NOT NULL,
    -- Seed, config hash and code version are the reproducibility triple: with
    -- them and the same input data, the run replays byte for byte.
    seed          BIGINT NOT NULL,
    config_hash   TEXT NOT NULL,
    model_version TEXT,
    code_version  TEXT,
    created_at    TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ,
    result_hash   TEXT,
    payload       JSONB NOT NULL
);

CREATE INDEX IF NOT EXISTS backtest_runs_time_idx ON backtest_runs (created_at DESC);
CREATE INDEX IF NOT EXISTS backtest_runs_strategy_idx ON backtest_runs (strategy_id, created_at DESC);

-- ------------------------------------------------------------------ news ---

CREATE TABLE IF NOT EXISTS news_events (
    id                TEXT PRIMARY KEY,
    ticker            TEXT NOT NULL,
    timestamp         TIMESTAMPTZ NOT NULL,
    ingested_at       TIMESTAMPTZ NOT NULL,
    source            TEXT NOT NULL,
    headline          TEXT NOT NULL,
    url               TEXT,
    category          TEXT NOT NULL,
    sentiment         TEXT NOT NULL,
    sentiment_score   DOUBLE PRECISION NOT NULL,
    materiality       TEXT NOT NULL,
    materiality_score DOUBLE PRECISION NOT NULL,
    confidence        DOUBLE PRECISION NOT NULL,
    affected_sector   TEXT,
    expected_horizon  TEXT,
    -- "deterministic" or "llm-enriched": which stage produced the labels.
    stage             TEXT NOT NULL,
    matched_terms     JSONB NOT NULL DEFAULT '[]'::jsonb,
    llm_model         TEXT,
    llm_agreed        BOOLEAN NOT NULL DEFAULT false,
    cluster_id        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS news_ticker_time_idx ON news_events (ticker, timestamp DESC);
CREATE INDEX IF NOT EXISTS news_cluster_idx ON news_events (cluster_id, timestamp DESC);

-- ------------------------------------------------------------- audit log ---

CREATE TABLE IF NOT EXISTS audit_log (
    id         TEXT PRIMARY KEY,
    at         TIMESTAMPTZ NOT NULL,
    principal  TEXT NOT NULL,
    action     TEXT NOT NULL,
    resource   TEXT NOT NULL,
    outcome    TEXT NOT NULL,
    request_id TEXT,
    trace_id   TEXT,
    detail     JSONB,
    remote_ip  TEXT
);

CREATE INDEX IF NOT EXISTS audit_time_idx ON audit_log (at DESC);
CREATE INDEX IF NOT EXISTS audit_principal_idx ON audit_log (principal, at DESC);

-- The audit log is append-only in practice, not only by convention: the
-- application role is granted INSERT and SELECT and nothing else.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'quantos_app') THEN
        REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM quantos_app;
        GRANT INSERT, SELECT ON audit_log TO quantos_app;
    END IF;
END$$;

INSERT INTO schema_migrations (version) VALUES ('001_schema')
ON CONFLICT (version) DO NOTHING;

COMMIT;

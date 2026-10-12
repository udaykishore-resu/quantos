-- Execution desk: order tickets awaiting, or past, the owner's approval, and
-- the kill switch.
--
-- internal/execution applies the same statements at startup (EnsureSchema),
-- because the Postgres image runs this directory only on an empty data
-- directory; a test keeps the two copies identical.
--
-- The two guarantees are enforced here rather than in the desk alone:
--   * execution_tickets_signal_uidx -> one ticket per signal, even on replay
--   * the desk updates a ticket only WHERE status = <the status it last saw>,
--     so two replicas (or two clicks) cannot both approve one ticket.

BEGIN;

CREATE TABLE IF NOT EXISTS execution_tickets (
    id          TEXT PRIMARY KEY,
    status      TEXT NOT NULL,
    ticker      TEXT NOT NULL,
    side        TEXT NOT NULL,
    signal_id   TEXT,
    venue       TEXT NOT NULL,
    paper       BOOLEAN NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    decided_by  TEXT,
    decided_at  TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL,
    body        JSONB NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS execution_tickets_signal_uidx
    ON execution_tickets (signal_id) WHERE signal_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS execution_tickets_time_idx
    ON execution_tickets (created_at DESC);
CREATE INDEX IF NOT EXISTS execution_tickets_open_idx
    ON execution_tickets (status) WHERE status IN ('PROPOSED','APPROVED','SUBMITTED','PARTIALLY_FILLED');

CREATE TABLE IF NOT EXISTS execution_halt (
    id      BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    halted  BOOLEAN NOT NULL,
    reason  TEXT,
    by_who  TEXT,
    at      TIMESTAMPTZ
);

INSERT INTO schema_migrations (version) VALUES ('002_execution')
ON CONFLICT (version) DO NOTHING;

COMMIT;

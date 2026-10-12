package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// Schema is the DDL for the execution tables. EnsureSchema applies it at
// startup, because the Postgres image only runs deploy/sql on an empty data
// directory and an existing local database would otherwise never get these
// tables. deploy/sql/postgres/002_execution.sql carries the same statements
// for fresh databases and for review.
const Schema = `
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
`

// PostgresStore is the durable Store.
//
// The ticket is stored whole as JSONB next to the columns it is filtered by,
// following the rest of the schema: it is written as a unit and read whole.
// The two guarantees that matter — one ticket per signal, and no update that
// skips a status — are enforced by the database (a unique index and a
// conditional UPDATE) rather than by the desk alone.
type PostgresStore struct {
	db      *sql.DB
	timeout time.Duration
}

// NewPostgresStore wraps an open database.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db, timeout: 5 * time.Second}
}

// EnsureSchema creates the execution tables if they are missing.
func (p *PostgresStore) EnsureSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := p.db.ExecContext(ctx, Schema); err != nil {
		return fmt.Errorf("execution: ensure schema: %w", err)
	}
	return nil
}

func (p *PostgresStore) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.timeout)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// InsertTicket implements Store.
func (p *PostgresStore) InsertTicket(ctx context.Context, t Ticket) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, `
INSERT INTO execution_tickets
  (id, status, ticker, side, signal_id, venue, paper, created_at, expires_at,
   decided_by, decided_at, updated_at, body)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		t.ID, string(t.Status), string(t.Ticker), string(t.Side), nullString(t.SignalID),
		t.Venue, t.Paper, t.CreatedAt, t.ExpiresAt, nullString(t.DecidedBy),
		nullTime(t.DecidedAt), lastEventAt(t), body)
	if err != nil {
		if isUniqueViolation(err, "execution_tickets_signal_uidx") {
			return ErrDuplicateSignal
		}
		return fmt.Errorf("execution: insert ticket %s: %w", t.ID, err)
	}
	return nil
}

// UpdateTicket implements Store.
func (p *PostgresStore) UpdateTicket(ctx context.Context, t Ticket, from TicketStatus) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	res, err := p.db.ExecContext(ctx, `
UPDATE execution_tickets
   SET status=$2, decided_by=$3, decided_at=$4, updated_at=$5, body=$6
 WHERE id=$1 AND status=$7`,
		t.ID, string(t.Status), nullString(t.DecidedBy), nullTime(t.DecidedAt),
		lastEventAt(t), body, string(from))
	if err != nil {
		return fmt.Errorf("execution: update ticket %s: %w", t.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// ListTickets implements Store.
func (p *PostgresStore) ListTickets(ctx context.Context, limit int) ([]Ticket, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 1000
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT body FROM execution_tickets ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("execution: list tickets: %w", err)
	}
	defer rows.Close()
	var out []Ticket
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var t Ticket
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("execution: decode ticket: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveHalt implements Store.
func (p *PostgresStore) SaveHalt(ctx context.Context, h Halt) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `
INSERT INTO execution_halt (id, halted, reason, by_who, at) VALUES (TRUE, $1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET halted=EXCLUDED.halted, reason=EXCLUDED.reason,
  by_who=EXCLUDED.by_who, at=EXCLUDED.at`,
		h.Halted, nullString(h.Reason), nullString(h.By), nullTime(h.At))
	if err != nil {
		return fmt.Errorf("execution: save halt: %w", err)
	}
	return nil
}

// LoadHalt implements Store.
func (p *PostgresStore) LoadHalt(ctx context.Context) (Halt, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	var (
		h          Halt
		reason, by sql.NullString
		at         sql.NullTime
	)
	err := p.db.QueryRowContext(ctx,
		`SELECT halted, reason, by_who, at FROM execution_halt WHERE id`).Scan(&h.Halted, &reason, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Halt{}, nil
	}
	if err != nil {
		return Halt{}, fmt.Errorf("execution: load halt: %w", err)
	}
	h.Reason, h.By, h.At = reason.String, by.String, at.Time
	return h, nil
}

func lastEventAt(t Ticket) time.Time {
	if n := len(t.History); n > 0 {
		return t.History[n-1].At
	}
	return t.CreatedAt
}

// isUniqueViolation reports whether err is a violation of the named unique
// index (SQLSTATE 23505).
func isUniqueViolation(err error, index string) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == index
}

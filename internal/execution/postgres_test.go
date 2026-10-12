package execution

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// TestPostgresStore runs against a real database when one is provided:
//
//	QUANTOS_TEST_POSTGRES_DSN=postgres://quantos:quantos@localhost:5432/quantos?sslmode=disable \
//	  go test ./internal/execution -run Postgres
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("QUANTOS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("QUANTOS_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := NewPostgresStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("schema must be idempotent: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM execution_tickets WHERE id LIKE 'qos-test-%'`); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	tk := Ticket{
		ID: "qos-test-" + now.Format("150405.000000"), Venue: "fake", Paper: true,
		CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
		Proposal: Proposal{Ticker: "AAPL", Side: domain.OrderBuy, Quantity: 2, LimitPrice: 10,
			ReferencePrice: 10, Rationale: "test", Source: SourceSignal, SignalID: "sig-test-" + now.Format("150405.000000")},
	}
	tk.transition(now, TicketProposed, "quantos", "")
	if err := st.InsertTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}
	dup := tk
	dup.ID += "-b"
	if err := st.InsertTicket(ctx, dup); !errors.Is(err, ErrDuplicateSignal) {
		t.Fatalf("want ErrDuplicateSignal, got %v", err)
	}

	approved := tk.clone()
	approved.DecidedBy, approved.DecidedAt = "uday", now
	approved.transition(now, TicketApproved, "uday", "")
	if err := st.UpdateTicket(ctx, approved, TicketProposed); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTicket(ctx, approved, TicketProposed); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale compare-and-set must conflict, got %v", err)
	}

	list, err := st.ListTickets(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var found *Ticket
	for i := range list {
		if list[i].ID == tk.ID {
			found = &list[i]
		}
	}
	if found == nil || found.Status != TicketApproved || len(found.History) != 2 || found.DecidedBy != "uday" {
		t.Fatalf("round trip lost data: %+v", found)
	}

	if err := st.SaveHalt(ctx, Halt{Halted: true, Reason: "test", By: "uday", At: now}); err != nil {
		t.Fatal(err)
	}
	h, err := st.LoadHalt(ctx)
	if err != nil || !h.Halted || h.Reason != "test" {
		t.Fatalf("halt round trip: %+v %v", h, err)
	}
	if err := st.SaveHalt(ctx, Halt{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := st.LoadHalt(ctx); h.Halted {
		t.Fatal("resume did not persist")
	}
}

// TestMigrationMatchesSchema keeps deploy/sql/postgres/002_execution.sql and
// the Schema that EnsureSchema applies from drifting apart.
func TestMigrationMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/sql/postgres/002_execution.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), strings.TrimSpace(Schema)) {
		t.Fatal("002_execution.sql no longer contains execution.Schema verbatim; update one from the other")
	}
}

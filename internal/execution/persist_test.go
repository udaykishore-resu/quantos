package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func deskOn(t *testing.T, h *harness, st Store) *Desk {
	t.Helper()
	lim := DefaultLimits()
	lim.Allowlist = []domain.Ticker{"AAPL"}
	d, err := NewDesk(context.Background(), DeskConfig{
		Venue: h.venue, Limits: lim, Clock: h.clock, Store: st,
		Quote:  func(context.Context, domain.Ticker) (float64, error) { return h.price, nil },
		Health: func(context.Context) (bool, string) { return h.ok, "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTicketsAndHaltSurviveARestart(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	st := NewMemoryStore()
	d1 := deskOn(t, h, st)
	tk, err := d1.Propose(ctx, buy(5, 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.Halt(ctx, "uday", "earnings week"); err != nil {
		t.Fatal(err)
	}

	d2 := deskOn(t, h, st)
	if !d2.HaltState().Halted {
		t.Fatal("a restart must not lift the kill switch")
	}
	got, err := d2.Get(ctx, tk.ID)
	if err != nil || got.Status != TicketCancelled {
		t.Fatalf("the cancelled ticket should be reloaded, got %s %v", got.Status, err)
	}
	if _, err := d2.Propose(ctx, buy(1, 100)); !errors.Is(err, ErrHalted) {
		t.Fatalf("a restarted desk must stay halted, got %v", err)
	}
}

func TestTwoReplicasCannotBothApprove(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	st := NewMemoryStore()
	a, b := deskOn(t, h, st), deskOn(t, h, st)
	tk, err := a.Propose(ctx, buy(5, 100))
	if err != nil {
		t.Fatal(err)
	}
	// b learns about the ticket the way a second replica would: from the store.
	b = deskOn(t, h, st)
	if _, err := a.Approve(ctx, tk.ID, "uday"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Approve(ctx, tk.ID, "uday"); !errors.Is(err, ErrNotProposed) {
		t.Fatalf("the second replica must lose the compare-and-set, got %v", err)
	}
	if n := h.venue.submits.Load(); n != 1 {
		t.Fatalf("two replicas placed %d orders", n)
	}
}

func TestOneTicketPerSignal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.desk.Propose(ctx, fromSignal(buy(5, 100), "sig-9")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.desk.Propose(ctx, fromSignal(buy(5, 100), "sig-9")); !errors.Is(err, ErrDuplicateSignal) {
		t.Fatalf("a replayed signal must not become a second ticket, got %v", err)
	}
}

func TestManualProposalSkipsTheModelGate(t *testing.T) {
	h := newHarness(t)
	h.ok = false
	tk := h.propose(t, buy(5, 100))
	got, err := h.desk.Approve(context.Background(), tk.ID, "uday")
	if err != nil {
		t.Fatalf("an unhealthy model must not block the owner's own trade: %v", err)
	}
	if got.Checks[2].Name != "model_health" || !got.Checks[2].Passed {
		t.Fatalf("the check list should say the gate was not applied: %+v", got.Checks)
	}
}

func TestInvalidatedSignalCancelsItsProposal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tk := h.propose(t, fromSignal(buy(5, 100), "sig-3"))
	h.desk.CancelForSignal(ctx, "sig-3", "price fell below the stop")
	got, _ := h.desk.Get(ctx, tk.ID)
	if got.Status != TicketCancelled {
		t.Fatalf("want CANCELLED, got %s", got.Status)
	}
}

func TestReconcileResolvesAnInterruptedApproval(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	st := NewMemoryStore()
	d := deskOn(t, h, st)
	tk, err := d.Propose(ctx, buy(5, 100))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the claim was written but before submission.
	claimed, _ := d.Get(ctx, tk.ID)
	claimed.transition(h.clock.Now(), TicketApproved, "uday", "")
	claimed.DecidedAt = h.clock.Now()
	if err := st.UpdateTicket(ctx, claimed, TicketProposed); err != nil {
		t.Fatal(err)
	}
	restarted := deskOn(t, h, st)

	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := restarted.Get(ctx, tk.ID); got.Status != TicketApproved {
		t.Fatalf("a fresh claim may still be in flight; want APPROVED, got %s", got.Status)
	}
	h.clock.Advance(3 * time.Minute)
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := restarted.Get(ctx, tk.ID); got.Status != TicketFailed {
		t.Fatalf("an old claim with no order is FAILED, got %s", got.Status)
	}
}

func TestReconcileFindsAnOrderFromAnInterruptedApproval(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	st := NewMemoryStore()
	d := deskOn(t, h, st)
	tk, _ := d.Propose(ctx, buy(5, 100))
	claimed, _ := d.Get(ctx, tk.ID)
	claimed.transition(h.clock.Now(), TicketApproved, "uday", "")
	_ = st.UpdateTicket(ctx, claimed, TicketProposed)
	// The order did reach the venue before the crash.
	h.venue.orders[tk.ID] = Order{ClientOrderID: tk.ID, Ticker: "AAPL", Status: domain.OrderFilled, VenueStatus: "filled", FilledQty: 5}

	restarted := deskOn(t, h, st)
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := restarted.Get(ctx, tk.ID); got.Status != TicketFilled {
		t.Fatalf("want FILLED, got %s", got.Status)
	}
	stored, _ := st.ListTickets(ctx, 10)
	if stored[0].Status != TicketFilled {
		t.Fatalf("the store must follow the venue too, got %s", stored[0].Status)
	}
}

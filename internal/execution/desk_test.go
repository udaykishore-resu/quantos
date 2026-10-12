package execution

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// fakeVenue is an in-memory paper venue.
type fakeVenue struct {
	mu         sync.Mutex
	paper      bool
	account    Account
	accountErr error
	positions  []Position
	orders     map[string]Order
	submits    atomic.Int32
	submitErr  error
	// submitLands makes a failing Submit still record the order, the way a
	// timeout can hide a successful submission.
	submitLands bool
	cancelAll   atomic.Int32
	nextStatus  domain.OrderStatus
}

func newFakeVenue() *fakeVenue {
	return &fakeVenue{
		paper: true,
		account: Account{
			Equity: 100_000, LastEquity: 100_000, Cash: 50_000,
			BuyingPower: 50_000, Status: "ACTIVE", Paper: true,
		},
		orders:     map[string]Order{},
		nextStatus: domain.OrderPending,
	}
}

func (v *fakeVenue) Name() string { return "fake" }
func (v *fakeVenue) Paper() bool  { return v.paper }
func (v *fakeVenue) Account(context.Context) (Account, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.account, v.accountErr
}
func (v *fakeVenue) Positions(context.Context) ([]Position, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Position(nil), v.positions...), nil
}
func (v *fakeVenue) Submit(_ context.Context, r OrderRequest) (Order, error) {
	v.submits.Add(1)
	v.mu.Lock()
	defer v.mu.Unlock()
	o := Order{
		ID: "ord-" + r.ClientOrderID, ClientOrderID: r.ClientOrderID, Ticker: r.Ticker,
		Side: r.Side, Quantity: r.Quantity, LimitPrice: r.LimitPrice,
		Status: v.nextStatus, VenueStatus: strings.ToLower(string(v.nextStatus)),
	}
	if v.submitErr != nil {
		if v.submitLands {
			v.orders[r.ClientOrderID] = o
		}
		return Order{}, v.submitErr
	}
	v.orders[r.ClientOrderID] = o
	return o, nil
}
func (v *fakeVenue) OrderByClientID(_ context.Context, id string) (Order, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.orders[id]
	if !ok {
		return Order{}, ErrOrderNotFound
	}
	return o, nil
}
func (v *fakeVenue) CancelAll(context.Context) error { v.cancelAll.Add(1); return nil }

type harness struct {
	desk  *Desk
	venue *fakeVenue
	clock *obs.SimClock
	price float64
	ok    bool
}

func newHarness(t *testing.T, mutate ...func(*Limits)) *harness {
	t.Helper()
	h := &harness{venue: newFakeVenue(), price: 100, ok: true,
		clock: obs.NewSimClock(time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC))}
	lim := DefaultLimits()
	lim.Allowlist = []domain.Ticker{"AAPL", "msft"}
	for _, m := range mutate {
		m(&lim)
	}
	d, err := NewDesk(context.Background(), DeskConfig{
		Venue: h.venue, Limits: lim, Clock: h.clock,
		Quote: func(context.Context, domain.Ticker) (float64, error) { return h.price, nil },
		Health: func(context.Context) (bool, string) {
			if h.ok {
				return true, ""
			}
			return false, "calibration degraded"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.desk = d
	return h
}

func buy(qty, limit float64) Proposal {
	return Proposal{Ticker: "AAPL", Side: domain.OrderBuy, Quantity: qty,
		LimitPrice: limit, ReferencePrice: 100, Rationale: "momentum and regime aligned"}
}

func fromSignal(p Proposal, id string) Proposal {
	p.Source, p.SignalID = SourceSignal, id
	return p
}

func (h *harness) propose(t *testing.T, p Proposal) Ticket {
	t.Helper()
	tk, err := h.desk.Propose(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestApproveSubmitsALimitOrder(t *testing.T) {
	h := newHarness(t)
	tk := h.propose(t, buy(5, 100.25))
	if tk.Status != TicketProposed || h.venue.submits.Load() != 0 {
		t.Fatalf("a proposal must not touch the venue: %+v", tk)
	}
	got, err := h.desk.Approve(context.Background(), tk.ID, "uday")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TicketSubmitted || got.Order == nil || got.Order.ClientOrderID != tk.ID {
		t.Fatalf("want SUBMITTED with the ticket id as client order id, got %+v", got)
	}
	if got.DecidedBy != "uday" || len(got.Checks) == 0 {
		t.Fatalf("approval must record who and what was checked: %+v", got)
	}
	for _, c := range got.Checks {
		if !c.Passed {
			t.Fatalf("unexpected failed check %+v", c)
		}
	}
}

func TestDoubleApprovalPlacesOneOrder(t *testing.T) {
	h := newHarness(t)
	tk := h.propose(t, buy(5, 100))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = h.desk.Approve(context.Background(), tk.ID, "uday") }()
	}
	wg.Wait()
	if n := h.venue.submits.Load(); n != 1 {
		t.Fatalf("8 concurrent approvals placed %d orders", n)
	}
}

func TestGuardrailsBlock(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*harness)
		prop  Proposal
		check string
	}{
		{"not on allowlist", nil, Proposal{Ticker: "TSLA", Side: domain.OrderBuy, Quantity: 1, LimitPrice: 100, ReferencePrice: 100, Rationale: "x"}, "allowlist"},
		{"order too large", nil, buy(20, 100), "max_order_notional"},
		{"model unhealthy", func(h *harness) { h.ok = false }, fromSignal(buy(5, 100), "sig-1"), "model_health"},
		{"price moved", func(h *harness) { h.price = 101 }, buy(5, 100), "price_drift"},
		{"down on the day", func(h *harness) { h.venue.account.Equity = 97_000 }, buy(5, 100), "daily_loss"},
		{"no buying power", func(h *harness) { h.venue.account.BuyingPower = 100 }, buy(5, 100), "buying_power"},
		{"position too big", func(h *harness) {
			h.venue.positions = []Position{{Ticker: "AAPL", Quantity: 96, MarketValue: 9_600}}
		}, buy(5, 100), "max_position_weight"},
		{"account blocked", func(h *harness) { h.venue.account.Blocked = true }, buy(5, 100), "account"},
		{"account unreadable", func(h *harness) { h.venue.accountErr = errors.New("503") }, buy(5, 100), "account"},
		{"selling what is not held", nil, Proposal{Ticker: "AAPL", Side: domain.OrderSell, Quantity: 5, LimitPrice: 100, ReferencePrice: 100, Rationale: "x"}, "position_held"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			if c.setup != nil {
				c.setup(h)
			}
			tk := h.propose(t, c.prop)
			got, err := h.desk.Approve(context.Background(), tk.ID, "uday")
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("want ErrBlocked, got %v", err)
			}
			if got.Status != TicketBlocked || !strings.HasPrefix(got.StatusReason, c.check+":") {
				t.Fatalf("want BLOCKED by %s, got %s %q", c.check, got.Status, got.StatusReason)
			}
			if h.venue.submits.Load() != 0 {
				t.Fatal("a blocked approval reached the venue")
			}
		})
	}
}

func TestSellIsAllowedWhenDownOnTheDay(t *testing.T) {
	h := newHarness(t)
	h.venue.account.Equity = 90_000
	h.venue.positions = []Position{{Ticker: "AAPL", Quantity: 10, MarketValue: 1_000}}
	tk := h.propose(t, Proposal{Ticker: "aapl", Side: domain.OrderSell, Quantity: 5,
		LimitPrice: 99.5, ReferencePrice: 100, Rationale: "trim after the stop"})
	if _, err := h.desk.Approve(context.Background(), tk.ID, "uday"); err != nil {
		t.Fatalf("a risk-reducing sell must not be stopped by the daily loss limit: %v", err)
	}
}

func TestExpiredTicketCannotBeApproved(t *testing.T) {
	h := newHarness(t)
	tk := h.propose(t, buy(5, 100))
	h.clock.Advance(16 * time.Minute)
	got, err := h.desk.Approve(context.Background(), tk.ID, "uday")
	if !errors.Is(err, ErrExpired) || got.Status != TicketExpired {
		t.Fatalf("want EXPIRED, got %s %v", got.Status, err)
	}
}

func TestHaltCancelsAndBlocks(t *testing.T) {
	h := newHarness(t)
	pending := h.propose(t, buy(5, 100))
	if err := h.desk.Halt(context.Background(), "uday", "market is wild"); err != nil {
		t.Fatal(err)
	}
	if h.venue.cancelAll.Load() != 1 {
		t.Fatal("halt must cancel open orders at the venue")
	}
	if got, _ := h.desk.Get(context.Background(), pending.ID); got.Status != TicketCancelled {
		t.Fatalf("pending proposal should be cancelled, got %s", got.Status)
	}
	if _, err := h.desk.Propose(context.Background(), buy(1, 100)); !errors.Is(err, ErrHalted) {
		t.Fatalf("proposals must be refused while halted, got %v", err)
	}
	if err := h.desk.Resume(context.Background(), "uday"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.desk.Propose(context.Background(), buy(1, 100)); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitTimeoutThatLandedIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.venue.submitErr = errors.New("context deadline exceeded")
	h.venue.submitLands = true
	tk := h.propose(t, buy(5, 100))
	got, err := h.desk.Approve(context.Background(), tk.ID, "uday")
	if err != nil || got.Status != TicketSubmitted {
		t.Fatalf("an order the venue has must not be reported as failed: %s %v", got.Status, err)
	}
}

func TestSubmitFailure(t *testing.T) {
	h := newHarness(t)
	h.venue.submitErr = errors.New("insufficient qty")
	tk := h.propose(t, buy(5, 100))
	got, err := h.desk.Approve(context.Background(), tk.ID, "uday")
	if err == nil || got.Status != TicketFailed {
		t.Fatalf("want FAILED, got %s %v", got.Status, err)
	}
}

func TestReconcileMovesToFilled(t *testing.T) {
	h := newHarness(t)
	tk := h.propose(t, buy(5, 100))
	if _, err := h.desk.Approve(context.Background(), tk.ID, "uday"); err != nil {
		t.Fatal(err)
	}
	h.venue.mu.Lock()
	o := h.venue.orders[tk.ID]
	o.Status, o.VenueStatus, o.FilledQty, o.AvgFillPrice = domain.OrderFilled, "filled", 5, 99.9
	h.venue.orders[tk.ID] = o
	h.venue.mu.Unlock()
	if err := h.desk.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := h.desk.Get(context.Background(), tk.ID)
	if got.Status != TicketFilled || got.Order.AvgFillPrice != 99.9 {
		t.Fatalf("want FILLED at 99.9, got %+v", got)
	}
	statuses := make([]TicketStatus, 0, len(got.History))
	for _, e := range got.History {
		statuses = append(statuses, e.Status)
	}
	want := []TicketStatus{TicketProposed, TicketApproved, TicketSubmitted, TicketFilled}
	if len(statuses) != len(want) {
		t.Fatalf("audit trail %v, want %v", statuses, want)
	}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("audit trail %v, want %v", statuses, want)
		}
	}
}

func TestRejectAndProposalValidation(t *testing.T) {
	h := newHarness(t)
	tk := h.propose(t, buy(5, 100))
	if got, err := h.desk.Reject(context.Background(), tk.ID, "uday", "not today"); err != nil || got.Status != TicketRejected {
		t.Fatalf("reject: %s %v", got.Status, err)
	}
	if _, err := h.desk.Approve(context.Background(), tk.ID, "uday"); !errors.Is(err, ErrNotProposed) {
		t.Fatalf("a rejected ticket must not be approvable, got %v", err)
	}
	bad := buy(5, 100)
	bad.Rationale = " "
	if _, err := h.desk.Propose(context.Background(), bad); err == nil {
		t.Fatal("a proposal without a rationale must be refused")
	}
}

func TestDeskRefusesLiveVenueAndEmptyAllowlist(t *testing.T) {
	v := newFakeVenue()
	v.paper = false
	lim := DefaultLimits()
	lim.Allowlist = []domain.Ticker{"AAPL"}
	q := func(context.Context, domain.Ticker) (float64, error) { return 1, nil }
	hf := func(context.Context) (bool, string) { return true, "" }
	if _, err := NewDesk(context.Background(), DeskConfig{Venue: v, Limits: lim, Quote: q, Health: hf}); err == nil {
		t.Fatal("a non-paper venue must be refused")
	}
	if _, err := NewDesk(context.Background(), DeskConfig{Venue: newFakeVenue(), Limits: DefaultLimits(), Quote: q, Health: hf}); err == nil {
		t.Fatal("an empty allowlist must be refused")
	}
}

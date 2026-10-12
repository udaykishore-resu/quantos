package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// QuoteFunc returns the current price of an instrument.
type QuoteFunc func(ctx context.Context, t domain.Ticker) (float64, error)

// HealthFunc reports whether the model behind the suggestions can be trusted
// today. When it returns false, approvals of signal proposals are refused.
type HealthFunc func(ctx context.Context) (ok bool, reason string)

// DeskConfig wires a desk.
type DeskConfig struct {
	Venue  Venue
	Limits Limits
	Quote  QuoteFunc
	Health HealthFunc
	// Store persists tickets and the kill switch. Nil means an in-memory
	// store, which loses everything on restart.
	Store  Store
	Clock  obs.Clock
	Logger *slog.Logger
	// LoadLimit bounds how many recent tickets are read back at startup.
	LoadLimit int
}

// Errors returned by the desk.
var (
	ErrNotFound    = errors.New("execution: ticket not found")
	ErrNotProposed = errors.New("execution: ticket is not awaiting a decision")
	// ErrBlocked wraps every guardrail refusal; the ticket's StatusReason says
	// which one.
	ErrBlocked = errors.New("execution: approval blocked by a guardrail")
	ErrExpired = errors.New("execution: ticket expired before it was approved")
	ErrHalted  = errors.New("execution: trading is halted")
)

// Desk holds tickets, applies guardrails, and routes approved orders.
//
// The in-memory map is a working copy; every state change is written to the
// Store with a compare-and-set on the previous status before it is applied,
// so the store is never behind the desk on anything that matters.
type Desk struct {
	venue  Venue
	limits Limits
	quote  QuoteFunc
	health HealthFunc
	store  Store
	clock  obs.Clock
	log    *slog.Logger

	mu      sync.Mutex
	tickets map[string]*Ticket
	halt    Halt
}

// NewDesk builds a desk and loads its state from the store. It refuses a venue
// that is not paper (rule G-8), a limit set that fails validation, and a
// missing quote or health source: without them two guardrails could not run,
// and a guardrail that cannot run must not silently pass.
func NewDesk(ctx context.Context, cfg DeskConfig) (*Desk, error) {
	if cfg.Venue == nil {
		return nil, errors.New("execution: a venue is required")
	}
	if !cfg.Venue.Paper() {
		return nil, fmt.Errorf("execution: venue %q is not paper; this build routes to paper only (governance rule G-8)", cfg.Venue.Name())
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, err
	}
	if cfg.Quote == nil || cfg.Health == nil {
		return nil, errors.New("execution: a quote source and a model-health gate are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}
	if cfg.LoadLimit <= 0 {
		cfg.LoadLimit = 1000
	}
	d := &Desk{
		venue: cfg.Venue, limits: cfg.Limits, quote: cfg.Quote, health: cfg.Health,
		store: cfg.Store, clock: cfg.Clock, log: cfg.Logger, tickets: map[string]*Ticket{},
	}
	halt, err := d.store.LoadHalt(ctx)
	if err != nil {
		return nil, err
	}
	d.halt = halt
	ts, err := d.store.ListTickets(ctx, cfg.LoadLimit)
	if err != nil {
		return nil, err
	}
	for i := range ts {
		t := ts[i]
		if t.Status == TicketApproved {
			// The process stopped between claiming a ticket and finishing the
			// approval. Whether an order reached the venue is unknown until
			// Reconcile asks, so it is treated as possibly submitted.
			d.log.Warn("execution: ticket was mid-approval at shutdown; reconcile will resolve it", "ticket", t.ID)
		}
		d.tickets[t.ID] = &t
	}
	return d, nil
}

// commit applies mutate to a copy of t, persists it with a compare-and-set on
// t's current status, and only then updates the working copy. Callers hold mu.
func (d *Desk) commitLocked(ctx context.Context, t *Ticket, mutate func(*Ticket)) error {
	next := t.clone()
	mutate(&next)
	if err := d.store.UpdateTicket(ctx, next, t.Status); err != nil {
		return err
	}
	*t = next
	return nil
}

// Propose records a suggestion for the owner to decide on. It never touches
// the venue.
func (d *Desk) Propose(ctx context.Context, p Proposal) (Ticket, error) {
	p.Ticker = domain.NormalizeTicker(string(p.Ticker))
	p.Source = orDefault(p.Source, SourceManual)
	switch {
	case p.Ticker == "":
		return Ticket{}, errors.New("execution: proposal needs a ticker")
	case p.Side != domain.OrderBuy && p.Side != domain.OrderSell:
		return Ticket{}, fmt.Errorf("execution: proposal side %q is not BUY or SELL", p.Side)
	case p.Quantity <= 0 || math.IsNaN(p.Quantity) || math.IsInf(p.Quantity, 0):
		return Ticket{}, errors.New("execution: proposal quantity must be positive")
	case p.LimitPrice <= 0 || p.ReferencePrice <= 0:
		return Ticket{}, errors.New("execution: proposal needs a positive limit and reference price")
	case strings.TrimSpace(p.Rationale) == "":
		return Ticket{}, errors.New("execution: proposal needs a rationale")
	case p.Source != SourceSignal && p.Source != SourceManual:
		return Ticket{}, fmt.Errorf("execution: proposal source %q is not signal or manual", p.Source)
	case p.Source == SourceSignal && p.SignalID == "":
		return Ticket{}, errors.New("execution: a signal proposal must name its signal")
	}
	now := d.clock.Now()
	t := &Ticket{
		ID: newTicketID(now), Proposal: p,
		Venue: d.venue.Name(), Paper: d.venue.Paper(),
		CreatedAt: now, ExpiresAt: now.Add(d.limits.TicketTTL),
	}
	t.transition(now, TicketProposed, p.Source, "")

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.halt.Halted {
		return Ticket{}, fmt.Errorf("%w: %s", ErrHalted, d.halt.Reason)
	}
	if err := d.store.InsertTicket(ctx, *t); err != nil {
		return Ticket{}, err
	}
	d.tickets[t.ID] = t
	return t.clone(), nil
}

// Approve is the only path to an order. It re-checks every guardrail at the
// moment of approval and submits a limit order only if all of them pass.
//
// Blocked and expired approvals return the ticket together with an error
// wrapping ErrBlocked or ErrExpired; the ticket records why.
func (d *Desk) Approve(ctx context.Context, id, by string) (Ticket, error) {
	if strings.TrimSpace(by) == "" {
		return Ticket{}, errors.New("execution: an approval must name who approved it")
	}
	d.mu.Lock()
	t, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return Ticket{}, ErrNotFound
	}
	now := d.clock.Now()
	d.expireLocked(ctx, now)
	switch {
	case t.Status == TicketExpired:
		snap := t.clone()
		d.mu.Unlock()
		return snap, ErrExpired
	case t.Status != TicketProposed:
		snap := t.clone()
		d.mu.Unlock()
		return snap, ErrNotProposed
	}
	if d.halt.Halted {
		reason := "kill switch engaged: " + d.halt.Reason
		err := d.commitLocked(ctx, t, func(n *Ticket) {
			n.DecidedBy, n.DecidedAt = by, now
			n.transition(now, TicketBlocked, by, reason)
		})
		snap := t.clone()
		d.mu.Unlock()
		if err != nil {
			return snap, d.claimErr(err)
		}
		return snap, fmt.Errorf("%w: %s", ErrBlocked, reason)
	}
	// APPROVED claims the ticket, in the store first: a second approval — a
	// double click, or another replica — fails the compare-and-set, so two
	// approvals can never become two orders.
	if err := d.commitLocked(ctx, t, func(n *Ticket) {
		n.DecidedBy, n.DecidedAt = by, now
		n.transition(now, TicketApproved, by, "")
	}); err != nil {
		snap := t.clone()
		d.mu.Unlock()
		return snap, d.claimErr(err)
	}
	prop := t.Proposal
	d.mu.Unlock()

	checks, failed := d.runChecks(ctx, prop)

	d.mu.Lock()
	if failed == "" && d.halt.Halted { // the kill switch was hit while checks ran
		failed = "kill switch engaged: " + d.halt.Reason
	}
	if failed != "" {
		err := d.commitLocked(ctx, t, func(n *Ticket) {
			n.Checks = checks
			n.transition(d.clock.Now(), TicketBlocked, "guardrail", failed)
		})
		snap := t.clone()
		d.mu.Unlock()
		if err != nil {
			d.log.Error("execution: could not record a blocked approval", "ticket", id, "error", err)
		}
		return snap, fmt.Errorf("%w: %s", ErrBlocked, failed)
	}
	t.Checks = checks
	d.mu.Unlock()

	order, err := d.venue.Submit(ctx, OrderRequest{
		ClientOrderID: id, Ticker: prop.Ticker, Side: prop.Side,
		Quantity: prop.Quantity, LimitPrice: prop.LimitPrice,
	})
	if err != nil {
		// A timeout can hide a successful submission. Ask before declaring
		// failure, so the ticket never disagrees with the venue.
		if o, lookupErr := d.venue.OrderByClientID(ctx, id); lookupErr == nil {
			order, err = o, nil
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		if cerr := d.commitLocked(ctx, t, func(n *Ticket) {
			n.transition(d.clock.Now(), TicketFailed, "venue", err.Error())
		}); cerr != nil {
			d.log.Error("execution: could not record a failed submission", "ticket", id, "error", cerr)
		}
		return t.clone(), err
	}
	if cerr := d.applyOrderLocked(ctx, t, order); cerr != nil {
		// The order exists at the venue. The working copy says so; the next
		// Reconcile writes it to the store.
		d.log.Error("execution: order submitted but not yet persisted", "ticket", id, "error", cerr)
	}
	return t.clone(), nil
}

func (d *Desk) claimErr(err error) error {
	if errors.Is(err, ErrConflict) {
		return ErrNotProposed
	}
	return fmt.Errorf("execution: could not record the decision, so nothing was sent: %w", err)
}

// runChecks evaluates every guardrail and returns the first failure. Any check
// that cannot get its data fails: a guardrail that cannot run does not pass.
func (d *Desk) runChecks(ctx context.Context, p Proposal) ([]Check, string) {
	var checks []Check
	add := func(name string, passed bool, format string, args ...any) bool {
		checks = append(checks, Check{Name: name, Passed: passed, Detail: fmt.Sprintf(format, args...)})
		return passed
	}
	fail := func() ([]Check, string) {
		last := checks[len(checks)-1]
		return checks, last.Name + ": " + last.Detail
	}

	if !add("allowlist", d.limits.allows(p.Ticker), "%s on the allowlist", p.Ticker) {
		return fail()
	}

	notional := p.Quantity * p.LimitPrice
	if !add("max_order_notional", notional <= d.limits.MaxOrderNotional,
		"order $%.2f against a cap of $%.2f", notional, d.limits.MaxOrderNotional) {
		return fail()
	}

	if p.Source == SourceSignal {
		ok, why := d.health(ctx)
		if !ok {
			why = orDefault(why, "the model-health gate is closed")
		}
		if !add("model_health", ok, "%s", orDefault(why, "model health ok")) {
			return fail()
		}
	} else {
		add("model_health", true, "not applied: a manual proposal is not the model's claim")
	}

	acct, err := d.venue.Account(ctx)
	if err != nil {
		add("account", false, "could not read the account: %v", err)
		return fail()
	}
	if !add("account", !acct.Blocked && acct.Equity > 0,
		"status %s, equity $%.2f", acct.Status, acct.Equity) {
		return fail()
	}

	px, err := d.quote(ctx, p.Ticker)
	if err != nil || px <= 0 {
		add("price_drift", false, "no current quote for %s: %v", p.Ticker, err)
		return fail()
	}
	drift := math.Abs(px-p.ReferencePrice) / p.ReferencePrice * 1e4
	if !add("price_drift", drift <= d.limits.MaxDriftBps,
		"%s now %.2f vs %.2f when suggested: %.0f bps against a cap of %.0f",
		p.Ticker, px, p.ReferencePrice, drift, d.limits.MaxDriftBps) {
		return fail()
	}

	positions, err := d.venue.Positions(ctx)
	if err != nil {
		add("positions", false, "could not read positions: %v", err)
		return fail()
	}
	var held Position
	for _, pos := range positions {
		if pos.Ticker == p.Ticker {
			held = pos
		}
	}

	if p.Side == domain.OrderSell {
		if !d.limits.AllowShort && !add("position_held", held.Quantity >= p.Quantity,
			"selling %g of %g held", p.Quantity, held.Quantity) {
			return fail()
		}
		return checks, ""
	}

	day := acct.DayPnLPct()
	if !add("daily_loss", day > -d.limits.MaxDailyLossPct,
		"account %+.2f%% today against a stop of -%.2f%%", day*100, d.limits.MaxDailyLossPct*100) {
		return fail()
	}
	if !add("buying_power", acct.BuyingPower >= notional,
		"needs $%.2f, has $%.2f", notional, acct.BuyingPower) {
		return fail()
	}
	weight := (math.Max(held.MarketValue, 0) + notional) / acct.Equity
	if !add("max_position_weight", weight <= d.limits.MaxPositionWeight,
		"%s would be %.1f%% of equity against a cap of %.1f%%",
		p.Ticker, weight*100, d.limits.MaxPositionWeight*100) {
		return fail()
	}
	return checks, ""
}

// Reject records the owner's refusal.
func (d *Desk) Reject(ctx context.Context, id, by, reason string) (Ticket, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tickets[id]
	if !ok {
		return Ticket{}, ErrNotFound
	}
	now := d.clock.Now()
	d.expireLocked(ctx, now)
	if t.Status != TicketProposed {
		return t.clone(), ErrNotProposed
	}
	if err := d.commitLocked(ctx, t, func(n *Ticket) {
		n.DecidedBy, n.DecidedAt = by, now
		n.transition(now, TicketRejected, by, reason)
	}); err != nil {
		return t.clone(), d.claimErr(err)
	}
	return t.clone(), nil
}

// CancelForSignal cancels any undecided proposal made from a signal that has
// since been invalidated. A suggestion whose premise is gone should not be
// waiting for an approval.
func (d *Desk) CancelForSignal(ctx context.Context, signalID, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.Now()
	for _, t := range d.tickets {
		if t.SignalID != signalID || t.Status != TicketProposed {
			continue
		}
		if err := d.commitLocked(ctx, t, func(n *Ticket) {
			n.transition(now, TicketCancelled, "quantos", "signal invalidated: "+reason)
		}); err != nil && !errors.Is(err, ErrConflict) {
			d.log.Error("execution: could not cancel a proposal for an invalidated signal", "ticket", t.ID, "error", err)
		}
	}
}

// Halt is the kill switch. It blocks every new proposal and approval, cancels
// every pending proposal, and asks the venue to cancel every open order. The
// halt takes effect in memory first, so a store or venue error cannot leave
// trading running.
func (d *Desk) Halt(ctx context.Context, by, reason string) error {
	reason = orDefault(reason, "halted by "+by)
	d.mu.Lock()
	now := d.clock.Now()
	d.halt = Halt{Halted: true, Reason: reason, By: by, At: now}
	var errs []error
	if err := d.store.SaveHalt(ctx, d.halt); err != nil {
		errs = append(errs, fmt.Errorf("the halt is in effect but was not persisted, so a restart would lift it: %w", err))
	}
	for _, t := range d.tickets {
		if t.Status == TicketProposed {
			if err := d.commitLocked(ctx, t, func(n *Ticket) {
				n.transition(now, TicketCancelled, by, "kill switch: "+reason)
			}); err != nil && !errors.Is(err, ErrConflict) {
				errs = append(errs, err)
			}
		}
	}
	d.mu.Unlock()
	if err := d.venue.CancelAll(ctx); err != nil {
		errs = append(errs, fmt.Errorf("cancelling open orders failed — check the venue directly: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("execution: halted, with errors: %w", errors.Join(errs...))
	}
	return nil
}

// Resume lifts the kill switch.
func (d *Desk) Resume(ctx context.Context, by string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := Halt{Halted: false, By: by, At: d.clock.Now()}
	if err := d.store.SaveHalt(ctx, next); err != nil {
		return fmt.Errorf("execution: still halted; could not persist the resume: %w", err)
	}
	d.halt = next
	return nil
}

// HaltState reports the kill switch.
func (d *Desk) HaltState() Halt {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.halt
}

// Reconcile refreshes every ticket that has, or may have, an order at the
// venue: SUBMITTED, PARTIALLY_FILLED, and APPROVED tickets left over from a
// restart. It returns the first error but keeps going through the rest.
func (d *Desk) Reconcile(ctx context.Context) error {
	d.mu.Lock()
	var ids []string
	for id, t := range d.tickets {
		if t.Status.Open() || t.Status == TicketApproved {
			ids = append(ids, id)
		}
	}
	d.mu.Unlock()

	var firstErr error
	keep := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, id := range ids {
		o, err := d.venue.OrderByClientID(ctx, id)
		d.mu.Lock()
		t := d.tickets[id]
		switch {
		case errors.Is(err, ErrOrderNotFound) && t.Status == TicketApproved &&
			d.clock.Now().Sub(t.DecidedAt) > 2*time.Minute:
			// Claimed, never sent, and long enough ago that it is not in flight.
			if cerr := d.commitLocked(ctx, t, func(n *Ticket) {
				n.transition(d.clock.Now(), TicketFailed, "reconcile", "approval was interrupted before the order was sent")
			}); cerr != nil {
				keep(cerr)
			}
		case err != nil && !errors.Is(err, ErrOrderNotFound):
			keep(fmt.Errorf("execution: reconcile %s: %w", id, err))
		case err == nil:
			if cerr := d.applyOrderLocked(ctx, t, o); cerr != nil {
				keep(cerr)
			}
		}
		d.mu.Unlock()
	}
	return firstErr
}

// applyOrderLocked records the venue's view of a ticket's order. It persists
// even when the status has not changed, because fill quantities move within a
// status and an earlier write may have failed.
func (d *Desk) applyOrderLocked(ctx context.Context, t *Ticket, o Order) error {
	var next TicketStatus
	switch o.Status {
	case domain.OrderFilled:
		next = TicketFilled
	case domain.OrderPartial:
		next = TicketPartial
	case domain.OrderCancelled:
		next = TicketCancelled
	case domain.OrderRejected:
		next = TicketVenueRejected
	default:
		next = TicketSubmitted
	}
	if t.Order != nil && *t.Order == o && t.Status == next {
		return nil
	}
	from := t.Status
	n := t.clone()
	n.Order = &o
	if next != n.Status {
		n.transition(d.clock.Now(), next, "venue", "venue status "+o.VenueStatus)
	}
	*t = n // the venue is the authority on the order; the working copy follows it
	if err := d.store.UpdateTicket(ctx, n, from); err != nil {
		// The stored status may lag (an earlier write failed); write over it.
		if errors.Is(err, ErrConflict) {
			for _, prev := range []TicketStatus{TicketApproved, TicketSubmitted, TicketPartial} {
				if d.store.UpdateTicket(ctx, n, prev) == nil {
					return nil
				}
			}
		}
		return err
	}
	return nil
}

// expireLocked moves every overdue proposal to EXPIRED.
func (d *Desk) expireLocked(ctx context.Context, now time.Time) {
	for _, t := range d.tickets {
		if t.Status == TicketProposed && !now.Before(t.ExpiresAt) {
			if err := d.commitLocked(ctx, t, func(n *Ticket) {
				n.transition(now, TicketExpired, "quantos", "no decision within "+d.limits.TicketTTL.String())
			}); err != nil && !errors.Is(err, ErrConflict) {
				d.log.Warn("execution: could not persist an expiry", "ticket", t.ID, "error", err)
			}
		}
	}
}

// Get returns one ticket.
func (d *Desk) Get(ctx context.Context, id string) (Ticket, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(ctx, d.clock.Now())
	t, ok := d.tickets[id]
	if !ok {
		return Ticket{}, ErrNotFound
	}
	return t.clone(), nil
}

// List returns tickets newest first, optionally filtered by status.
func (d *Desk) List(ctx context.Context, status TicketStatus, limit int) []Ticket {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(ctx, d.clock.Now())
	out := make([]Ticket, 0, len(d.tickets))
	for _, t := range d.tickets {
		if status == "" || t.Status == status {
			out = append(out, t.clone())
		}
	}
	sortNewestFirst(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Limits returns the guardrails in force.
func (d *Desk) Limits() Limits { return d.limits }

// Venue returns the venue the desk routes to.
func (d *Desk) Venue() Venue { return d.venue }

// newTicketID doubles as the venue's client_order_id, so it is unique, short
// (Alpaca allows 128 characters) and sortable by time.
func newTicketID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("qos-%s-%s", now.UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

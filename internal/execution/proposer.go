package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// ErrSkipped wraps every reason a signal does not become a proposal. A skip is
// a normal outcome, not a failure: most signals should not become orders.
var ErrSkipped = errors.New("execution: signal not proposed")

// ProposerConfig wires a proposer.
type ProposerConfig struct {
	Desk *Desk
	// Quote must be the venue's market price, not QuantOS's own feed: the
	// order meets the venue's market.
	Quote QuoteFunc
	// LimitBufferBps is how far past the current price the limit is set, so a
	// marketable order can fill without chasing the market. Default 10 bps.
	LimitBufferBps float64
	Clock          obs.Clock
}

// Proposer turns QuantOS signals into proposals the owner can approve.
//
// What it takes from the signal is the judgement — direction, conviction,
// suggested weight, the stop and target as distances, and the evidence. What
// it does not take is the price: entry, limit and size are computed from the
// venue's live quote and the account's real equity.
type Proposer struct {
	desk  *Desk
	quote QuoteFunc
	buf   float64
	clock obs.Clock
}

// NewProposer builds a proposer.
func NewProposer(cfg ProposerConfig) (*Proposer, error) {
	if cfg.Desk == nil || cfg.Quote == nil {
		return nil, errors.New("execution: a proposer needs a desk and a venue quote source")
	}
	if cfg.LimitBufferBps <= 0 {
		cfg.LimitBufferBps = 10
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	return &Proposer{desk: cfg.Desk, quote: cfg.Quote, buf: cfg.LimitBufferBps / 1e4, clock: cfg.Clock}, nil
}

func skip(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSkipped, fmt.Sprintf(format, args...))
}

// FromSignal builds a proposal from a signal without recording it.
func (p *Proposer) FromSignal(ctx context.Context, s domain.Signal) (Proposal, error) {
	now := p.clock.Now()
	lim := p.desk.Limits()
	t := domain.NormalizeTicker(string(s.Ticker))
	switch {
	case s.Status != domain.SignalActive:
		return Proposal{}, skip("signal %s is %s", s.ID, s.Status)
	case !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt):
		return Proposal{}, skip("signal %s expired at %s", s.ID, s.ExpiresAt.Format("15:04:05"))
	case !lim.allows(t):
		return Proposal{}, skip("%s is not on the allowlist", t)
	case s.Side != domain.SideLong && s.Side != domain.SideShort:
		return Proposal{}, skip("signal side %s has no order", s.Side)
	}

	venue := p.desk.Venue()
	positions, err := venue.Positions(ctx)
	if err != nil {
		return Proposal{}, fmt.Errorf("execution: read positions: %w", err)
	}
	var held Position
	for _, pos := range positions {
		if pos.Ticker == t {
			held = pos
		}
	}
	px, err := p.quote(ctx, t)
	if err != nil || px <= 0 {
		return Proposal{}, fmt.Errorf("execution: no venue quote for %s: %v", t, err)
	}

	prop := Proposal{
		Ticker: t, ReferencePrice: px, Source: SourceSignal,
		SignalID: s.ID, Confidence: s.Confidence,
	}

	if s.Side == domain.SideShort {
		// Shorting is off. A short signal on something held is a reason to
		// exit it; on something not held it is not a trade at all.
		if held.Quantity <= 0 {
			return Proposal{}, skip("short signal on %s, which is not held, and shorting is off", t)
		}
		prop.Side = domain.OrderSell
		prop.Quantity = held.Quantity
		prop.LimitPrice = roundCents(px * (1 - p.buf))
		prop.Rationale = rationale(s, prop, px, held, 0)
		return prop, nil
	}

	acct, err := venue.Account(ctx)
	if err != nil {
		return Proposal{}, fmt.Errorf("execution: read account: %w", err)
	}
	if acct.Equity <= 0 {
		return Proposal{}, skip("account equity is %.2f", acct.Equity)
	}
	limitPx := roundCents(px * (1 + p.buf))
	target := s.SuggestedWeight * acct.Equity
	room := lim.MaxPositionWeight*acct.Equity - math.Max(held.MarketValue, 0)
	budget := math.Min(math.Min(target, lim.MaxOrderNotional), math.Min(room, acct.BuyingPower))
	qty := math.Floor(budget / limitPx)
	if qty < 1 {
		return Proposal{}, skip("%s: budget $%.2f (weight %.1f%%, cap $%.0f, room $%.2f) buys no whole share at $%.2f",
			t, budget, s.SuggestedWeight*100, lim.MaxOrderNotional, room, limitPx)
	}
	prop.Side = domain.OrderBuy
	prop.Quantity = qty
	prop.LimitPrice = limitPx
	prop.Rationale = rationale(s, prop, px, held, target)
	return prop, nil
}

// Propose builds a proposal from a signal and records it on the desk.
func (p *Proposer) Propose(ctx context.Context, s domain.Signal) (Ticket, error) {
	prop, err := p.FromSignal(ctx, s)
	if err != nil {
		return Ticket{}, err
	}
	return p.desk.Propose(ctx, prop)
}

func roundCents(v float64) float64 { return math.Round(v*100) / 100 }

// rationale is what the owner reads before deciding. It says what the signal
// claims, what argues against it, and how the order was sized — and that the
// signal's own prices are not the order's.
func rationale(s domain.Signal, p Proposal, px float64, held Position, target float64) string {
	var b strings.Builder
	verb := "Buy"
	if p.Side == domain.OrderSell {
		verb = "Sell"
	}
	fmt.Fprintf(&b, "%s %g %s, limit $%.2f (venue price $%.2f).\n", verb, p.Quantity, p.Ticker, p.LimitPrice, px)
	fmt.Fprintf(&b, "Signal %s: %s, strength %.0f/100, confidence %.0f%%, horizon %s, regime %s, strategy %s.\n",
		s.ID, s.Side, s.Strength, s.Confidence*100, s.Horizon, s.Regime, s.StrategyID)
	if p.Side == domain.OrderSell {
		fmt.Fprintf(&b, "Exits the whole position of %g shares (avg $%.2f).\n", held.Quantity, held.AvgPrice)
	} else {
		fmt.Fprintf(&b, "Sized from a suggested weight of %.1f%% ($%.0f), capped by the order and position limits.\n",
			s.SuggestedWeight*100, target)
	}
	if s.EntryReference > 0 {
		if s.StopReference > 0 {
			stop := s.StopReference/s.EntryReference - 1
			fmt.Fprintf(&b, "Stop: %+.1f%% from entry (about $%.2f at the venue price).\n", stop*100, px*(1+stop))
		}
		if s.TargetReference > 0 {
			tgt := s.TargetReference/s.EntryReference - 1
			fmt.Fprintf(&b, "Target: %+.1f%% from entry (about $%.2f).\n", tgt*100, px*(1+tgt))
		}
	}
	if len(s.Evidence) > 0 {
		b.WriteString("For: " + strings.Join(first(s.Evidence, 3), "; ") + ".\n")
	}
	if len(s.CounterEvidence) > 0 {
		b.WriteString("Against: " + strings.Join(first(s.CounterEvidence, 2), "; ") + ".\n")
	}
	if s.Explanation != "" {
		b.WriteString(s.Explanation + "\n")
	}
	b.WriteString("The signal was computed on QuantOS's market feed; its stop and target are carried over as distances, " +
		"and the order is priced and sized from the venue's live quote and account.")
	return b.String()
}

func first(ss []string, n int) []string {
	if len(ss) > n {
		return ss[:n]
	}
	return ss
}

// Manual builds a proposal for a trade the owner thought of, priced the same
// way a signal proposal is: from the venue's live quote, with the limit set
// LimitBufferBps past it unless the owner names a limit.
func (p *Proposer) Manual(ctx context.Context, t domain.Ticker, side domain.OrderSide, qty, limit float64, why string) (Proposal, error) {
	t = domain.NormalizeTicker(string(t))
	px, err := p.quote(ctx, t)
	if err != nil || px <= 0 {
		return Proposal{}, fmt.Errorf("execution: no venue quote for %s: %v", t, err)
	}
	if limit <= 0 {
		if side == domain.OrderSell {
			limit = roundCents(px * (1 - p.buf))
		} else {
			limit = roundCents(px * (1 + p.buf))
		}
	}
	return Proposal{
		Ticker: t, Side: side, Quantity: qty, LimitPrice: limit, ReferencePrice: px,
		Source: SourceManual, Rationale: strings.TrimSpace(why),
	}, nil
}

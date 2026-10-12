package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func signalFixture(h *harness) domain.Signal {
	now := h.clock.Now()
	return domain.Signal{
		ID: "sig-1", Ticker: "AAPL", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Side: domain.SideLong, Status: domain.SignalActive, Strength: 72, Confidence: 0.61,
		Horizon: "1d", EntryReference: 50, StopReference: 48, TargetReference: 55,
		SuggestedWeight: 0.05, StrategyID: "momentum-v2", Regime: "TRENDING_UP",
		Evidence:        []string{"close above 20d high", "volume 1.8x average"},
		CounterEvidence: []string{"RSI 74"},
	}
}

func newProposer(t *testing.T, h *harness) *Proposer {
	t.Helper()
	p, err := NewProposer(ProposerConfig{
		Desk: h.desk, Clock: h.clock,
		Quote: func(context.Context, domain.Ticker) (float64, error) { return h.price, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProposerPricesFromTheVenueNotTheSignal(t *testing.T) {
	h := newHarness(t)
	p := newProposer(t, h)
	prop, err := p.FromSignal(context.Background(), signalFixture(h))
	if err != nil {
		t.Fatal(err)
	}
	// Signal entry was 50 on QuantOS's feed; the venue says 100.
	if prop.ReferencePrice != 100 || prop.LimitPrice != 100.10 {
		t.Fatalf("want reference 100 and limit 100.10 (10 bps), got %+v", prop)
	}
	// 5% of $100k is $5,000, capped at the $1,000 order limit: 9 shares at $100.10.
	if prop.Side != domain.OrderBuy || prop.Quantity != 9 {
		t.Fatalf("want BUY 9, got %s %g", prop.Side, prop.Quantity)
	}
	if prop.Source != SourceSignal || prop.SignalID != "sig-1" {
		t.Fatalf("provenance missing: %+v", prop)
	}
	for _, want := range []string{"momentum-v2", "Stop: -4.0%", "about $96.00", "Against: RSI 74", "venue's live quote"} {
		if !strings.Contains(prop.Rationale, want) {
			t.Errorf("rationale lacks %q:\n%s", want, prop.Rationale)
		}
	}
}

func TestProposerRespectsPositionRoom(t *testing.T) {
	h := newHarness(t)
	h.venue.positions = []Position{{Ticker: "AAPL", Quantity: 97, MarketValue: 9_700}}
	prop, err := newProposer(t, h).FromSignal(context.Background(), signalFixture(h))
	if err != nil {
		t.Fatal(err)
	}
	// 10% of $100k leaves $300 of room: 2 shares at $100.10.
	if prop.Quantity != 2 {
		t.Fatalf("want 2 shares within the position cap, got %g", prop.Quantity)
	}
}

func TestProposerSkips(t *testing.T) {
	cases := map[string]func(*harness, *domain.Signal){
		"inactive":     func(_ *harness, s *domain.Signal) { s.Status = domain.SignalInvalidated },
		"expired":      func(h *harness, s *domain.Signal) { s.ExpiresAt = h.clock.Now().Add(-time.Second) },
		"not allowed":  func(_ *harness, s *domain.Signal) { s.Ticker = "TSLA" },
		"flat":         func(_ *harness, s *domain.Signal) { s.Side = domain.SideFlat },
		"short unheld": func(_ *harness, s *domain.Signal) { s.Side = domain.SideShort },
		"too small":    func(_ *harness, s *domain.Signal) { s.SuggestedWeight = 0.0005 },
		"no room": func(h *harness, _ *domain.Signal) {
			h.venue.positions = []Position{{Ticker: "AAPL", Quantity: 100, MarketValue: 10_000}}
		},
		"no buying power": func(h *harness, _ *domain.Signal) { h.venue.account.BuyingPower = 50 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			s := signalFixture(h)
			mutate(h, &s)
			if _, err := newProposer(t, h).FromSignal(context.Background(), s); !errors.Is(err, ErrSkipped) {
				t.Fatalf("want ErrSkipped, got %v", err)
			}
		})
	}
}

func TestShortSignalExitsAHeldPosition(t *testing.T) {
	h := newHarness(t)
	h.venue.positions = []Position{{Ticker: "AAPL", Quantity: 7, AvgPrice: 90, MarketValue: 700}}
	s := signalFixture(h)
	s.Side = domain.SideShort
	prop, err := newProposer(t, h).FromSignal(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if prop.Side != domain.OrderSell || prop.Quantity != 7 || prop.LimitPrice != 99.90 {
		t.Fatalf("want SELL 7 @ 99.90, got %+v", prop)
	}
}

func TestProposedSignalCanBeApprovedEndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tk, err := newProposer(t, h).Propose(ctx, signalFixture(h))
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.desk.Approve(ctx, tk.ID, "uday")
	if err != nil || got.Status != TicketSubmitted {
		t.Fatalf("want SUBMITTED, got %s %v (%s)", got.Status, err, got.StatusReason)
	}
	if _, err := newProposer(t, h).Propose(ctx, signalFixture(h)); !errors.Is(err, ErrDuplicateSignal) {
		t.Fatalf("a replayed signal must not propose twice, got %v", err)
	}
}

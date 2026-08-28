// Package failure holds the tests that inject faults and assert the platform
// degrades in a way an operator can live with.
//
// The rule these tests encode is the one that governs the whole design: when a
// component fails, the platform must become quieter, never more confident. A
// missing risk input is not an absent risk, a stale price is not a current
// price, and a signal that cannot be recorded is a signal that must not be
// emitted. Every test here spoils exactly one thing and checks that the loss of
// capability shows up somewhere an operator will see it.
package failure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/risk"
)

var now = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

// --- market data ------------------------------------------------------------

// TestStaleDataSuspendsRatherThanGuesses covers governance rule G-5.
func TestStaleDataSuspendsRatherThanGuesses(t *testing.T) {
	clock := obs.NewSimClock(now)
	v := marketdata.NewValidator(nil, marketdata.ValidatorConfig{
		StalenessSoft: 30 * time.Second, StalenessHard: 2 * time.Minute, Clock: clock,
	})
	q := domain.Quote{
		Ticker: "AAPL", Bid: 229.98, Ask: 230.02, BidSize: 100, AskSize: 100,
		Last: 230, Timestamp: now, Source: "test",
	}
	if err := v.ValidateQuote(q); err != nil {
		t.Fatalf("a good quote was rejected: %v", err)
	}
	if v.Freshness("AAPL").Stale() {
		t.Fatal("a symbol quoted a moment ago was reported stale")
	}

	clock.Advance(3 * time.Minute)
	f := v.Freshness("AAPL")
	if !f.Stale() || !f.Hard {
		t.Fatal("a symbol silent for three minutes was not reported stale; every downstream " +
			"consumer would act on a three-minute-old price as if it were current")
	}
	if f.Reason == "" {
		t.Fatal("the staleness verdict carries no reason")
	}
}

func TestAFutureDatedTickIsRejected(t *testing.T) {
	clock := obs.NewSimClock(now)
	v := marketdata.NewValidator(nil, marketdata.ValidatorConfig{Clock: clock})
	q := domain.Quote{
		Ticker: "AAPL", Bid: 229.98, Ask: 230.02, BidSize: 100, AskSize: 100,
		Last: 230, Timestamp: now.Add(time.Hour), Source: "test",
	}
	if err := v.ValidateQuote(q); err == nil {
		t.Fatal("a quote timestamped an hour in the future was accepted")
	}
}

// TestStructurallyImpossibleQuotesAreRejected checks the cheap checks, which
// are the ones that catch a broken feed before anything expensive runs.
func TestStructurallyImpossibleQuotesAreRejected(t *testing.T) {
	clock := obs.NewSimClock(now)
	v := marketdata.NewValidator(nil, marketdata.ValidatorConfig{Clock: clock})
	cases := map[string]domain.Quote{
		"crossed book":   {Ticker: "AAPL", Bid: 231, Ask: 230, BidSize: 1, AskSize: 1, Last: 230, Timestamp: now},
		"negative price": {Ticker: "AAPL", Bid: -1, Ask: 230, BidSize: 1, AskSize: 1, Last: 230, Timestamp: now},
		"no size":        {Ticker: "AAPL", Bid: 229.9, Ask: 230.1, BidSize: 0, AskSize: 0, Last: 230, Timestamp: now},
		"no ticker":      {Bid: 229.9, Ask: 230.1, BidSize: 1, AskSize: 1, Last: 230, Timestamp: now},
	}
	for name, q := range cases {
		if err := v.ValidateQuote(q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// --- event bus --------------------------------------------------------------

var errBusDown = errors.New("bus: broker unavailable (simulated outage)")

type failingBus struct {
	inner bus.Bus
	fail  bool
}

func (f *failingBus) Publish(ctx context.Context, topic string, e bus.Envelope) error {
	if f.fail {
		return errBusDown
	}
	return f.inner.Publish(ctx, topic, e)
}
func (f *failingBus) PublishBatch(ctx context.Context, topic string, es []bus.Envelope) error {
	if f.fail {
		return errBusDown
	}
	return f.inner.PublishBatch(ctx, topic, es)
}
func (f *failingBus) Subscribe(topic, group string, h bus.Handler) error {
	return f.inner.Subscribe(topic, group, h)
}
func (f *failingBus) Run(ctx context.Context) error { return f.inner.Run(ctx) }
func (f *failingBus) Close() error                  { return nil }

// TestABusOutageSurfacesRatherThanSwallows is the difference between a platform
// that is down and a platform that looks fine while losing every event.
func TestABusOutageSurfacesRatherThanSwallows(t *testing.T) {
	clock := obs.NewSimClock(now)
	inner := bus.NewMemoryBus(bus.MemoryOptions{Synchronous: true})
	fb := &failingBus{inner: inner}

	received := 0
	if err := fb.Subscribe(bus.TopicBars, "test", func(context.Context, bus.Envelope) error {
		received++
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	b := bus.NewBuilder("test", clock)
	publish := func() error {
		env, err := b.New(context.Background(), bus.TopicBars, "AAPL", clock.Now(),
			domain.Candle{Ticker: "AAPL", Close: 230})
		if err != nil {
			return err
		}
		return fb.Publish(context.Background(), bus.TopicBars, env)
	}

	if err := publish(); err != nil {
		t.Fatalf("a healthy publish failed: %v", err)
	}
	fb.fail = true
	for i := 0; i < 5; i++ {
		if err := publish(); err == nil {
			t.Fatal("a publish during an outage reported success")
		}
	}
	fb.fail = false
	if err := publish(); err != nil {
		t.Fatalf("publishing did not recover after the outage cleared: %v", err)
	}
	if received != 2 {
		t.Fatalf("handler ran %d times; the five publishes during the outage must not have "+
			"been delivered, and the two outside it must have been", received)
	}
}

// TestAFailingHandlerIsRetriedBeforeTheEventIsGivenUp is what makes
// at-least-once delivery mean something: a handler that fails transiently must
// see the event again rather than have it silently dropped.
func TestAFailingHandlerIsRetriedBeforeTheEventIsGivenUp(t *testing.T) {
	clock := obs.NewSimClock(now)
	attempts := 0
	var lost []string
	b := bus.NewMemoryBus(bus.MemoryOptions{
		Synchronous: true, MaxRetries: 3,
		OnError: func(topic, group string, e bus.Envelope, err error) {
			lost = append(lost, topic+"/"+group)
		},
	})
	if err := b.Subscribe(bus.TopicBars, "flaky", func(context.Context, bus.Envelope) error {
		attempts++
		if attempts < 3 {
			return errors.New("handler is having a bad day")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	builder := bus.NewBuilder("test", clock)
	env, err := builder.New(context.Background(), bus.TopicBars, "AAPL", clock.Now(),
		domain.Candle{Ticker: "AAPL", Close: 230})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(context.Background(), bus.TopicBars, env); err != nil {
		t.Fatalf("publish reported an error although the handler eventually succeeded: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("the handler ran %d times; a transient failure must be retried", attempts)
	}
	if len(lost) != 0 {
		t.Fatalf("an event that eventually succeeded was reported lost: %v", lost)
	}
}

// TestAnEventThatNeverSucceedsIsReportedNotDropped covers the other side: the
// platform may give up, but it may not do so quietly.
func TestAnEventThatNeverSucceedsIsReportedNotDropped(t *testing.T) {
	clock := obs.NewSimClock(now)
	var lost int
	b := bus.NewMemoryBus(bus.MemoryOptions{
		Synchronous: true, MaxRetries: 2,
		OnError: func(string, string, bus.Envelope, error) { lost++ },
	})
	if err := b.Subscribe(bus.TopicBars, "broken", func(context.Context, bus.Envelope) error {
		return errors.New("permanently broken")
	}); err != nil {
		t.Fatal(err)
	}
	builder := bus.NewBuilder("test", clock)
	env, _ := builder.New(context.Background(), bus.TopicBars, "AAPL", clock.Now(),
		domain.Candle{Ticker: "AAPL", Close: 230})
	_ = b.Publish(context.Background(), bus.TopicBars, env)
	if lost != 1 {
		t.Fatalf("a permanently failing event was reported %d times; it must be surfaced exactly once", lost)
	}
}

// TestEventsSurviveARestart is the durability claim behind the WAL driver: a
// process that dies between publish and consume must not lose the event.
func TestEventsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	clock := obs.NewSimClock(now)

	w, err := bus.NewWALBus(bus.WALOptions{Dir: dir, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	builder := bus.NewBuilder("test", clock)
	env, err := builder.New(context.Background(), bus.TopicBars, "AAPL", clock.Now(),
		domain.Candle{Ticker: "AAPL", Close: 230})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Publish(context.Background(), bus.TopicBars, env); err != nil {
		t.Fatal(err)
	}
	// The process dies here, before anything consumed the event.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := bus.NewWALBus(bus.WALOptions{Dir: dir, Clock: clock, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("the log could not be reopened after a restart: %v", err)
	}
	defer w2.Close()

	got := make(chan string, 1)
	if err := w2.Subscribe(bus.TopicBars, "restarted", func(_ context.Context, e bus.Envelope) error {
		select {
		case got <- e.EventID:
		default:
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w2.Run(ctx) }()

	select {
	case id := <-got:
		if id != env.EventID {
			t.Fatalf("a different event came back: %s vs %s", id, env.EventID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an event published before a restart was never delivered afterwards")
	}
}

// --- risk engine ------------------------------------------------------------

// TestMissingRiskInputsNeverBecomeAPass is the rule that keeps degradation
// safe: an unevaluated check is not a passed check.
func TestMissingRiskInputsNeverBecomeAPass(t *testing.T) {
	cfg := risk.DefaultConfig()
	cfg.Clock = obs.NewSimClock(now)
	e := risk.NewEngine(cfg)

	in := healthyRiskInput()
	// Lose the portfolio: concentration, correlation and drawdown all become
	// unevaluable at once, which is exactly what a portfolio-service outage
	// looks like from here.
	in.Portfolio = nil

	a := e.Assess(in)
	if a.Decision == domain.RiskAllowPaperSignal {
		t.Fatal("with the portfolio unavailable the engine allowed a signal; it cannot know " +
			"whether the position would breach concentration or drawdown limits")
	}
	skipped := 0
	for _, c := range a.Checks {
		if c.Status == domain.CheckSkip {
			skipped++
			if c.Skipped == "" {
				t.Fatalf("check %s was skipped without recording why", c.Name)
			}
		}
	}
	if skipped < 3 {
		t.Fatalf("only %d checks were marked unevaluable; the rest must have silently passed", skipped)
	}
}

// TestNoSignalCanBeBuiltWithoutAnAllowingAssessment is the structural veto.
func TestNoSignalCanBeBuiltWithoutAnAllowingAssessment(t *testing.T) {
	for _, d := range []domain.RiskDecision{domain.RiskBlock, domain.RiskWatchOnly, ""} {
		_, err := domain.NewSignal(domain.SignalDraft{
			Ticker: "AAPL", CreatedAt: now, Side: domain.SideLong,
			Invalidations: []domain.InvalidationCondition{{
				Kind: domain.InvalidTimeExpiry, Description: "expires", Deadline: now.Add(time.Hour),
			}},
		}, domain.RiskAssessment{Decision: d})
		if !errors.Is(err, domain.ErrRiskVeto) {
			t.Fatalf("decision %q produced a signal (err=%v)", d, err)
		}
	}
}

// --- provider failover ------------------------------------------------------

type deadProvider struct{ calls *int }

func (deadProvider) Name() string { return "dead" }
func (deadProvider) Capabilities() marketdata.Capabilities {
	return marketdata.Capabilities{Intervals: []domain.Interval{domain.Interval1m}, Quotes: true}
}
func (d deadProvider) GetQuotes(context.Context, []domain.Ticker) ([]domain.Quote, error) {
	*d.calls++
	return nil, errors.New("provider is down")
}
func (d deadProvider) GetHistoricalBars(context.Context, marketdata.BarRequest) ([]domain.Candle, error) {
	*d.calls++
	return nil, errors.New("provider is down")
}
func (deadProvider) MarketStatus(context.Context) (domain.MarketStatus, error) {
	return domain.MarketStatus{}, errors.New("provider is down")
}

// TestABreakerStopsHammeringADeadProvider checks that a failing dependency is
// given up on rather than retried into the ground.
func TestABreakerStopsHammeringADeadProvider(t *testing.T) {
	calls := 0
	clock := obs.NewSimClock(now)
	br := marketdata.NewBreaker("test", 3, time.Minute, clock, nil)
	p := deadProvider{calls: &calls}

	for i := 0; i < 20; i++ {
		if !br.Allow() {
			continue
		}
		_, err := p.GetQuotes(context.Background(), []domain.Ticker{"AAPL"})
		if err != nil {
			br.Failure(err)
		} else {
			br.Success()
		}
	}
	if calls > 5 {
		t.Fatalf("the provider was called %d times in twenty attempts; the breaker never opened", calls)
	}
	if br.Allow() {
		t.Fatal("the breaker is still closed after repeated failures")
	}

	clock.Advance(2 * time.Minute)
	if !br.Allow() {
		t.Fatal("the breaker never reopened after its cooldown, so recovery is impossible")
	}
}

// --- helpers ----------------------------------------------------------------

func healthyRiskInput() risk.Input {
	snap := domain.FeatureSnapshot{
		Ticker: "AAPL", AsOf: now, LastTickAt: now.Add(-2 * time.Second),
		SpreadBps: 4, Last: 230,
		Values: map[string]float64{
			domain.FeatLast: 230, domain.FeatRealizedVol: 0.22, domain.FeatATRPct: 0.004,
		},
		Warm: map[string]bool{
			domain.FeatLast: true, domain.FeatRealizedVol: true, domain.FeatATRPct: true,
		},
	}
	pred := domain.Prediction{
		ID: "p1", Ticker: "AAPL", Source: domain.SourceModel, ModelVersion: "v1",
		Scenarios: []domain.ScenarioPrediction{{
			Horizon:    domain.Horizon60m,
			Dist:       domain.NewDistribution(0.60, 0.25, 0.15),
			Confidence: domain.NewDistribution(0.60, 0.25, 0.15).Confidence(),
		}},
	}
	return risk.Input{
		Ticker: "AAPL",
		Stock: domain.Stock{
			Ticker: "AAPL", Sector: "Technology",
			AvgNotional: 12_000_000_000, AvgVolume: 55_000_000,
		},
		Snapshot: snap, Prediction: &pred,
		Regime: domain.MarketRegime{Regime: domain.RegimeBullTrend, Confidence: 0.8},
		Portfolio: &domain.Portfolio{
			ID: "p", Equity: 100_000, PeakEquity: 100_000,
			SectorExposure: map[string]float64{"Technology": 0.05},
		},
		Health:         &domain.ModelHealth{ModelVersion: "v1", Healthy: true, Drift: domain.DriftNone},
		ProposedWeight: 0.03, Side: domain.SideLong, Now: now,
	}
}

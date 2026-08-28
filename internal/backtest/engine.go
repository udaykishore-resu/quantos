package backtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/paper"
	"github.com/udaykishoreresu/quantos/internal/pipeline"
)

// LeakageGuard enforces the no-look-ahead invariant.
//
// It aborts a run rather than warning, because a warning in a log is not a
// control: the entire value of a backtest depends on this property holding, and
// a run that violated it should produce no number at all.
type LeakageGuard struct {
	Enabled    bool
	Violations []string
}

// Check asserts that every feature timestamp precedes or equals the decision
// time, and that no decision is made on a bar that has not closed.
func (g *LeakageGuard) Check(snap domain.FeatureSnapshot, decisionAt time.Time, bar domain.Candle) error {
	if !g.Enabled {
		return nil
	}
	if snap.AsOf.After(decisionAt) {
		v := fmt.Sprintf("feature snapshot for %s is stamped %s, after the decision time %s",
			snap.Ticker, snap.AsOf.Format(time.RFC3339Nano), decisionAt.Format(time.RFC3339Nano))
		g.Violations = append(g.Violations, v)
		return fmt.Errorf("leakage guard: %s", v)
	}
	if bar.End.After(decisionAt) {
		v := fmt.Sprintf("bar for %s ends at %s, after the decision time %s",
			bar.Ticker, bar.End.Format(time.RFC3339Nano), decisionAt.Format(time.RFC3339Nano))
		g.Violations = append(g.Violations, v)
		return fmt.Errorf("leakage guard: %s", v)
	}
	return nil
}

// CheckFill asserts that a fill happens strictly after the decision that caused
// it. A fill at the decision bar's close is the single most common way a
// backtest reports returns it could never have earned.
func (g *LeakageGuard) CheckFill(decisionAt, fillAt time.Time, ticker domain.Ticker) error {
	if !g.Enabled {
		return nil
	}
	if !fillAt.After(decisionAt) {
		v := fmt.Sprintf("fill for %s at %s is not after the decision at %s",
			ticker, fillAt.Format(time.RFC3339Nano), decisionAt.Format(time.RFC3339Nano))
		g.Violations = append(g.Violations, v)
		return fmt.Errorf("leakage guard: %s", v)
	}
	return nil
}

// Request configures a run.
type Request struct {
	Name         string
	StrategyID   string
	Start        time.Time
	End          time.Time
	Interval     domain.Interval
	Seed         int64
	StartingCash float64
	Costs        domain.CostModel
	RiskFree     float64
	// Trials is the number of configurations tried in the sweep this run
	// belongs to, used for the deflated Sharpe ratio.
	Trials int
	// MacroTickers are market-context symbols, fed to the regime engine but
	// never traded.
	MacroTickers []domain.Ticker
	CodeVersion  string
	ConfigHash   string
	ModelVersion string
}

// Builder constructs a fresh pipeline bound to a simulated clock and broker.
// The backtester owns construction so that every run starts from clean state —
// a run that inherits warm indicators from a previous run is not reproducible.
type Builder func(clock *obs.SimClock, broker *paper.Broker) *pipeline.Pipeline

// Engine runs deterministic historical simulations.
type Engine struct {
	build Builder
	guard LeakageGuard
}

// NewEngine builds a backtest engine.
func NewEngine(build Builder) *Engine {
	return &Engine{build: build, guard: LeakageGuard{Enabled: true}}
}

// Run replays bars in timestamp order through the production pipeline.
//
// Bars must be supplied for the whole universe including macro symbols; the
// engine sorts them into a single deterministic stream by (timestamp, ticker,
// sequence) so that the interleaving does not depend on the caller.
func (e *Engine) Run(ctx context.Context, req Request, bars []domain.Candle) (domain.BacktestRun, error) {
	if len(bars) == 0 {
		return domain.BacktestRun{}, fmt.Errorf("backtest: no bars supplied")
	}
	if req.StartingCash <= 0 {
		req.StartingCash = 100_000
	}
	if req.Costs.ParticipationCap <= 0 {
		req.Costs = domain.DefaultCostModel()
	}

	stream := append([]domain.Candle(nil), bars...)
	sort.SliceStable(stream, func(i, j int) bool {
		if !stream[i].End.Equal(stream[j].End) {
			return stream[i].End.Before(stream[j].End)
		}
		if stream[i].Ticker != stream[j].Ticker {
			return stream[i].Ticker < stream[j].Ticker
		}
		return stream[i].Sequence < stream[j].Sequence
	})
	if !req.Start.IsZero() || !req.End.IsZero() {
		filtered := stream[:0]
		for _, c := range stream {
			if !req.Start.IsZero() && c.End.Before(req.Start) {
				continue
			}
			if !req.End.IsZero() && c.End.After(req.End) {
				continue
			}
			filtered = append(filtered, c)
		}
		stream = filtered
	}
	if len(stream) == 0 {
		return domain.BacktestRun{}, fmt.Errorf("backtest: no bars inside the requested window")
	}

	clock := obs.NewSimClock(stream[0].End)
	broker := paper.NewBroker(paper.Config{
		StartingCash: req.StartingCash,
		MaxPositions: 20,
		MaxWeight:    0.10,
		Costs:        req.Costs,
		Clock:        clock,
		// Deterministic randomness: the seed is recorded on the run so the
		// result can be reproduced exactly.
		Rand: seededRand(req.Seed),
	}, "backtest", req.Name)

	pl := e.build(clock, broker)
	guard := LeakageGuard{Enabled: true}

	run := domain.BacktestRun{
		ID:           newRunID(req),
		Name:         req.Name,
		Status:       domain.BacktestRunning,
		StrategyID:   req.StrategyID,
		Start:        stream[0].End,
		End:          stream[len(stream)-1].End,
		Interval:     req.Interval,
		Seed:         req.Seed,
		ConfigHash:   req.ConfigHash,
		ModelVersion: req.ModelVersion,
		CodeVersion:  req.CodeVersion,
		StartingCash: req.StartingCash,
		Costs:        req.Costs,
		CreatedAt:    stream[0].End,
		StartedAt:    stream[0].End,
	}
	seen := map[domain.Ticker]bool{}

	// The loop processes bars strictly in order and advances the clock to each
	// bar's close before evaluating it. Nothing in the pipeline can therefore
	// observe a timestamp later than the bar it is reacting to.
	for i := range stream {
		select {
		case <-ctx.Done():
			run.Status = domain.BacktestAborted
			run.Error = ctx.Err().Error()
			return run, ctx.Err()
		default:
		}
		c := stream[i]
		seen[c.Ticker] = true
		clock.Set(c.End)
		decisionAt := c.End

		// Mark the book *before* the decision, using prices from bars that have
		// already closed. Pending orders from the previous bar fill here, which
		// is the next-bar-open semantics ADR-007 requires.
		prices, volumes := pl.Marks()
		broker.Mark(prices, volumes, decisionAt)

		res := pl.OnBar(ctx, c)
		run.EventsProcessed++

		if err := guard.Check(res.Snapshot, decisionAt, c); err != nil {
			run.Status = domain.BacktestFailed
			run.Error = err.Error()
			run.CompletedAt = decisionAt
			return run, err
		}
		if res.Signal != nil {
			// A signal generated on this bar can only fill on a later one.
			if err := guard.CheckFill(decisionAt, decisionAt.Add(c.Interval.Duration()), c.Ticker); err != nil {
				run.Status = domain.BacktestFailed
				run.Error = err.Error()
				return run, err
			}
		}
	}

	// Final mark and forced liquidation, so open positions are valued at a real
	// price rather than left as an unrealised claim.
	last := stream[len(stream)-1].End
	clock.Set(last)
	prices, volumes := pl.Marks()
	broker.Mark(prices, volumes, last)
	liquidate(broker, last)
	broker.Mark(prices, volumes, last.Add(time.Minute))

	curve := broker.EquityCurve()
	trades := broker.Trades()
	run.EquityCurve = curve
	run.Trades = trades
	run.Metrics = ComputeMetrics(curve, trades, req.RiskFree)
	if req.Trials > 1 {
		run.Metrics.TrialsConsidered = req.Trials
		run.Metrics.DeflatedSharpe = DeflatedSharpe(run.Metrics.Sharpe, req.Trials, len(curve))
	}
	run.RegimeMetrics = PartitionByRegime(curve, trades, req.RiskFree)

	universe := make([]domain.Ticker, 0, len(seen))
	for t := range seen {
		universe = append(universe, t)
	}
	sort.Slice(universe, func(i, j int) bool { return universe[i] < universe[j] })
	run.Universe = universe

	run.Status = domain.BacktestCompleted
	run.CompletedAt = last
	run.ResultHash = resultHash(run)
	return run, nil
}

// liquidate closes every open position at the final mark, so the reported
// return is realised rather than notional.
func liquidate(b *paper.Broker, at time.Time) {
	for _, pos := range b.Positions() {
		if pos.Quantity == 0 {
			continue
		}
		side := domain.OrderSell
		qty := pos.Quantity
		if qty < 0 {
			side, qty = domain.OrderBuy, -qty
		}
		_, _ = b.Submit(paper.OrderRequest{
			IdempotencyKey: "liquidate:" + string(pos.Ticker) + ":" + at.Format(time.RFC3339Nano),
			Ticker:         pos.Ticker, Side: side, Type: domain.OrderMarket,
			Quantity: qty, Now: at,
		})
	}
}

// seededRand returns a deterministic pseudo-random source. Using an explicit
// linear congruential generator rather than math/rand's global source makes the
// sequence a function of the seed alone, independent of any other package.
func seededRand(seed int64) func() float64 {
	state := uint64(seed)*6364136223846793005 + 1442695040888963407
	return func() float64 {
		state = state*6364136223846793005 + 1442695040888963407
		return float64(state>>11) / float64(uint64(1)<<53)
	}
}

func newRunID(req Request) string {
	h := sha256.Sum256([]byte(req.Name + req.StrategyID + strconv.FormatInt(req.Seed, 10) +
		req.Start.Format(time.RFC3339) + req.End.Format(time.RFC3339) + req.ConfigHash))
	return "bt_" + hex.EncodeToString(h[:8])
}

// resultHash lets two runs be compared byte-for-byte, which is how the
// determinism test works: run the same config twice and compare this value.
func resultHash(run domain.BacktestRun) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%d", run.StrategyID, run.Seed, run.ConfigHash, run.EventsProcessed)
	for _, p := range run.EquityCurve {
		fmt.Fprintf(h, "|%s:%.6f", p.At.UTC().Format(time.RFC3339Nano), p.Equity)
	}
	for _, t := range run.Trades {
		fmt.Fprintf(h, "|%s:%s:%.6f:%.6f", t.Ticker, t.EntryAt.UTC().Format(time.RFC3339Nano), t.EntryPrice, t.PnL)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

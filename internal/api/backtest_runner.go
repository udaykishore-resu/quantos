package api

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/backtest"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/store"
)

// BacktestRunner accepts backtest requests and executes them off the request
// path.
//
// A backtest is minutes of CPU, so the API accepts it and returns 202 with a
// run id: holding an HTTP connection open for the duration would tie the
// client's timeout to the workload's runtime, which is how a slow backtest
// becomes a gateway timeout and a confused user.
type BacktestRunner struct {
	engine     *backtest.Engine
	store      store.BacktestStore
	bars       BarSource
	clock      obs.Clock
	log        *slog.Logger
	sem        chan struct{}
	codeVer    string
	configHash string

	mu   sync.RWMutex
	runs map[string]domain.BacktestRun
}

// BarSource supplies historical bars for a backtest.
type BarSource func(ctx context.Context, tickers []domain.Ticker, start, end time.Time, interval domain.Interval) ([]domain.Candle, error)

// RunnerConfig parameterises the runner.
type RunnerConfig struct {
	Engine      *backtest.Engine
	Store       store.BacktestStore
	Bars        BarSource
	Clock       obs.Clock
	Logger      *slog.Logger
	Concurrency int
	CodeVersion string
	ConfigHash  string
}

// NewBacktestRunner builds a runner.
func NewBacktestRunner(cfg RunnerConfig) *BacktestRunner {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	return &BacktestRunner{
		engine: cfg.Engine, store: cfg.Store, bars: cfg.Bars, clock: cfg.Clock,
		log: cfg.Logger, sem: make(chan struct{}, cfg.Concurrency),
		codeVer: cfg.CodeVersion, configHash: cfg.ConfigHash,
		runs: map[string]domain.BacktestRun{},
	}
}

// Submit queues a run and returns its queued record.
func (r *BacktestRunner) Submit(ctx context.Context, req backtest.Request, tickers []domain.Ticker) (domain.BacktestRun, error) {
	if r.engine == nil || r.bars == nil {
		return domain.BacktestRun{}, fmt.Errorf("backtesting is not configured")
	}
	if req.Seed == 0 {
		// A zero seed would make the run non-reproducible in the one way that
		// matters, so derive one deterministically from the request itself.
		req.Seed = deterministicSeed(req)
	}
	req.CodeVersion = r.codeVer
	req.ConfigHash = r.configHash

	queued := domain.BacktestRun{
		ID: "bt_" + obs.NewSpanID(), Name: req.Name, Status: domain.BacktestQueued,
		StrategyID: req.StrategyID, Start: req.Start, End: req.End,
		Interval: req.Interval, Seed: req.Seed, ConfigHash: req.ConfigHash,
		CodeVersion: req.CodeVersion, StartingCash: req.StartingCash,
		CreatedAt: r.clock.Now(),
	}
	r.mu.Lock()
	r.runs[queued.ID] = queued
	r.mu.Unlock()
	if r.store != nil {
		_ = r.store.SaveRun(ctx, queued)
	}

	go r.execute(queued, req, tickers)
	return queued, nil
}

func (r *BacktestRunner) execute(queued domain.BacktestRun, req backtest.Request, tickers []domain.Ticker) {
	r.sem <- struct{}{}
	defer func() { <-r.sem }()

	// A backtest must not inherit the request's cancellation: the client has
	// already been told it was accepted.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	bars, err := r.bars(ctx, tickers, req.Start, req.End, req.Interval)
	if err != nil {
		r.finish(ctx, queued, domain.BacktestFailed, err)
		return
	}
	if len(bars) == 0 {
		r.finish(ctx, queued, domain.BacktestFailed, fmt.Errorf("no historical bars available for the requested window"))
		return
	}

	run, err := r.engine.Run(ctx, req, bars)
	run.ID = queued.ID
	run.CreatedAt = queued.CreatedAt
	if err != nil {
		run.Status = domain.BacktestFailed
		run.Error = err.Error()
		r.log.Error("backtest failed", "id", run.ID, "error", err)
	}
	r.mu.Lock()
	r.runs[run.ID] = run
	r.mu.Unlock()
	if r.store != nil {
		if err := r.store.SaveRun(ctx, run); err != nil {
			r.log.Error("could not persist backtest run", "id", run.ID, "error", err)
		}
	}
}

func (r *BacktestRunner) finish(ctx context.Context, run domain.BacktestRun, status domain.BacktestStatus, err error) {
	run.Status = status
	run.CompletedAt = r.clock.Now()
	if err != nil {
		run.Error = err.Error()
	}
	r.mu.Lock()
	r.runs[run.ID] = run
	r.mu.Unlock()
	if r.store != nil {
		_ = r.store.SaveRun(ctx, run)
	}
}

// Get returns a queued or completed run held in memory.
func (r *BacktestRunner) Get(id string) (domain.BacktestRun, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.runs[id]
	return run, ok
}

// deterministicSeed derives a seed from the request so that an unseeded run is
// still reproducible: the same request always produces the same seed.
func deterministicSeed(req backtest.Request) int64 {
	var h int64 = 1469598103934665603
	for _, b := range []byte(req.Name + req.StrategyID +
		req.Start.UTC().Format(time.RFC3339) + req.End.UTC().Format(time.RFC3339)) {
		h ^= int64(b)
		h *= 1099511628211
	}
	if h < 0 {
		h = -h
	}
	return h
}

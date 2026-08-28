package backtest

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Fold is one train/validate/test split in a walk-forward evaluation.
//
// The embargo between folds is the part people leave out. Without it, a label
// computed over a forward window in the training fold overlaps the feature
// window of the test fold, and the model is scored on information it was
// trained on — which looks like skill and is not.
type Fold struct {
	Index        int       `json:"index"`
	TrainStart   time.Time `json:"train_start"`
	TrainEnd     time.Time `json:"train_end"`
	ValidStart   time.Time `json:"valid_start"`
	ValidEnd     time.Time `json:"valid_end"`
	TestStart    time.Time `json:"test_start"`
	TestEnd      time.Time `json:"test_end"`
	EmbargoStart time.Time `json:"embargo_start"`
	EmbargoEnd   time.Time `json:"embargo_end"`
}

// WalkForwardConfig parameterises fold construction.
type WalkForwardConfig struct {
	Start time.Time
	End   time.Time
	// TrainWindow, ValidWindow and TestWindow are durations.
	TrainWindow time.Duration
	ValidWindow time.Duration
	TestWindow  time.Duration
	// Step is how far each fold advances. Step < TestWindow gives overlapping
	// test windows, which inflates apparent sample size; the default is
	// Step == TestWindow, giving disjoint, honest out-of-sample periods.
	Step time.Duration
	// Embargo separates train and test. It must be at least the maximum label
	// horizon; the constructor enforces a floor of twice the maximum horizon.
	Embargo time.Duration
	// Anchored keeps the training window's start fixed (expanding window)
	// rather than rolling it forward.
	Anchored bool
	// MaxHorizon is the longest prediction horizon in use, used to floor the
	// embargo.
	MaxHorizon time.Duration
}

// BuildFolds constructs the walk-forward schedule.
func BuildFolds(cfg WalkForwardConfig) ([]Fold, error) {
	if !cfg.End.After(cfg.Start) {
		return nil, fmt.Errorf("walk-forward: end must be after start")
	}
	if cfg.TrainWindow <= 0 || cfg.TestWindow <= 0 {
		return nil, fmt.Errorf("walk-forward: train and test windows must be positive")
	}
	if cfg.Step <= 0 {
		cfg.Step = cfg.TestWindow
	}
	minEmbargo := 2 * cfg.MaxHorizon
	if cfg.Embargo < minEmbargo {
		cfg.Embargo = minEmbargo
	}

	var folds []Fold
	trainStart := cfg.Start
	i := 0
	for {
		trainEnd := trainStart.Add(cfg.TrainWindow)
		validStart := trainEnd
		validEnd := validStart.Add(cfg.ValidWindow)
		embargoStart := validEnd
		embargoEnd := embargoStart.Add(cfg.Embargo)
		testStart := embargoEnd
		testEnd := testStart.Add(cfg.TestWindow)
		if testEnd.After(cfg.End) {
			break
		}
		folds = append(folds, Fold{
			Index:      i,
			TrainStart: trainStart, TrainEnd: trainEnd,
			ValidStart: validStart, ValidEnd: validEnd,
			EmbargoStart: embargoStart, EmbargoEnd: embargoEnd,
			TestStart: testStart, TestEnd: testEnd,
		})
		i++
		if cfg.Anchored {
			// Expanding window: the start stays put, the end moves.
			cfg.TrainWindow += cfg.Step
		} else {
			trainStart = trainStart.Add(cfg.Step)
		}
	}
	if len(folds) == 0 {
		return nil, fmt.Errorf("walk-forward: the window %s..%s is too short for a %s train + %s valid + %s embargo + %s test schedule",
			cfg.Start.Format("2006-01-02"), cfg.End.Format("2006-01-02"),
			cfg.TrainWindow, cfg.ValidWindow, cfg.Embargo, cfg.TestWindow)
	}
	return folds, nil
}

// Validate asserts that no fold's test window overlaps its own training or
// validation window, which is the property the whole exercise exists to
// guarantee.
func Validate(folds []Fold) error {
	for _, f := range folds {
		if !f.TestStart.After(f.TrainEnd) {
			return fmt.Errorf("fold %d: test starts %s, not after training ends %s",
				f.Index, f.TestStart.Format(time.RFC3339), f.TrainEnd.Format(time.RFC3339))
		}
		if !f.TestStart.After(f.ValidEnd) {
			return fmt.Errorf("fold %d: test starts %s, not after validation ends %s",
				f.Index, f.TestStart.Format(time.RFC3339), f.ValidEnd.Format(time.RFC3339))
		}
		if f.TestStart.Sub(f.ValidEnd) < f.EmbargoEnd.Sub(f.EmbargoStart) {
			return fmt.Errorf("fold %d: embargo is not fully applied between validation and test", f.Index)
		}
	}
	return nil
}

// FoldResult is one fold's out-of-sample outcome.
type FoldResult struct {
	Fold    Fold                   `json:"fold"`
	Run     domain.BacktestRun     `json:"run"`
	Metrics domain.BacktestMetrics `json:"metrics"`
	Error   string                 `json:"error,omitempty"`
}

// WalkForwardReport aggregates the folds.
type WalkForwardReport struct {
	Folds []FoldResult `json:"folds"`
	// Aggregate is computed by concatenating the out-of-sample equity curves,
	// which is the only honest way to summarise a walk-forward: averaging
	// per-fold Sharpe ratios flatters a strategy with one lucky fold.
	Aggregate   domain.BacktestMetrics `json:"aggregate"`
	Consistency float64                `json:"consistency"`
	FoldCount   int                    `json:"fold_count"`
	Notes       []string               `json:"notes,omitempty"`
}

// RunWalkForward executes each fold's test window and aggregates the results.
//
// The caller supplies barsFor so that the harness stays agnostic about where
// history comes from; in practice it is a ClickHouse query or a replay
// directory.
func (e *Engine) RunWalkForward(ctx context.Context, base Request, folds []Fold,
	barsFor func(start, end time.Time) ([]domain.Candle, error)) (WalkForwardReport, error) {

	if err := Validate(folds); err != nil {
		return WalkForwardReport{}, err
	}
	rep := WalkForwardReport{FoldCount: len(folds)}
	var allCurve []domain.EquityPoint
	var allTrades []domain.BacktestTrade
	positive := 0

	for _, f := range folds {
		bars, err := barsFor(f.TestStart, f.TestEnd)
		if err != nil {
			rep.Folds = append(rep.Folds, FoldResult{Fold: f, Error: err.Error()})
			continue
		}
		req := base
		req.Start, req.End = f.TestStart, f.TestEnd
		req.Name = fmt.Sprintf("%s-fold-%d", base.Name, f.Index)
		run, err := e.Run(ctx, req, bars)
		fr := FoldResult{Fold: f, Run: run, Metrics: run.Metrics}
		if err != nil {
			fr.Error = err.Error()
		}
		rep.Folds = append(rep.Folds, fr)
		if err == nil {
			allCurve = append(allCurve, rebase(run.EquityCurve, allCurve)...)
			allTrades = append(allTrades, run.Trades...)
			if run.Metrics.TotalReturn > 0 {
				positive++
			}
		}
	}
	if len(rep.Folds) > 0 {
		rep.Consistency = float64(positive) / float64(len(rep.Folds))
	}
	sort.SliceStable(allCurve, func(i, j int) bool { return allCurve[i].At.Before(allCurve[j].At) })
	rep.Aggregate = ComputeMetrics(allCurve, allTrades, base.RiskFree)
	rep.Notes = append(rep.Notes,
		fmt.Sprintf("%d folds, %.0f%% with a positive out-of-sample return", len(rep.Folds), rep.Consistency*100),
		"aggregate metrics are computed from the concatenated out-of-sample curve, not by averaging per-fold statistics",
		fmt.Sprintf("embargo of %s applied between validation and test in every fold", folds[0].EmbargoEnd.Sub(folds[0].EmbargoStart)))
	return rep, nil
}

// rebase shifts a fold's equity curve so it continues from the previous fold's
// final equity, producing a continuous out-of-sample curve.
func rebase(curve []domain.EquityPoint, prior []domain.EquityPoint) []domain.EquityPoint {
	if len(curve) == 0 {
		return nil
	}
	if len(prior) == 0 {
		return curve
	}
	start := curve[0].Equity
	if start <= 0 {
		return curve
	}
	scale := prior[len(prior)-1].Equity / start
	out := make([]domain.EquityPoint, len(curve))
	for i, p := range curve {
		p.Equity *= scale
		p.Cash *= scale
		out[i] = p
	}
	return out
}

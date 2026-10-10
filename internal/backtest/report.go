package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/evaluation"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/pipeline"
)

// ReportConfig parameterises a net-of-cost walk-forward report.
//
// The report answers one question and refuses to answer a different one: over
// disjoint, embargoed, out-of-sample windows on the supplied history, did the
// strategy's paper trades and its predictions show anything that survives
// transaction costs — including costs set deliberately higher than the
// estimate, because the estimate is the first thing that is wrong.
type ReportConfig struct {
	Base   Request
	Folds  []Fold
	Warmup time.Duration
	// CostMultipliers scale every component of Base.Costs. 1 is the model as
	// configured; 2 is the stress case. A strategy whose edge disappears when
	// costs double never had one.
	CostMultipliers []float64
	// Benchmark is the buy-and-hold comparison, usually SPY. A strategy has
	// to beat sitting still, after costs, to be worth running.
	Benchmark domain.Ticker
	// ResolveLag bounds how late a prediction may be scored; on daily bars it
	// must cover a long weekend. Zero uses five days.
	ResolveLag time.Duration
	// CalibrationBins for the reliability diagram.
	CalibrationBins int
	// MinRegimeSamples below which a per-regime evaluation is not reported.
	MinRegimeSamples int
}

// CostResult is the report for one cost multiplier.
type CostResult struct {
	Multiplier  float64                                                     `json:"multiplier"`
	Costs       domain.CostModel                                            `json:"costs"`
	WalkForward WalkForwardReport                                           `json:"walk_forward"`
	Evaluation  map[domain.Horizon]domain.ModelEvaluation                   `json:"evaluation"`
	ByRegime    map[domain.Horizon]map[domain.Regime]domain.ModelEvaluation `json:"by_regime,omitempty"`
	VetoCost    map[domain.Horizon]evaluation.VetoCost                      `json:"veto_cost"`
	Signals     int                                                         `json:"signals_generated"`
	Predictions int                                                         `json:"predictions_tracked"`
	Outcomes    int                                                         `json:"outcomes_scored"`
	Discarded   int                                                         `json:"outcomes_discarded"`
	Rejections  map[string]int                                              `json:"rejections_by_stage"`
}

// Report is the complete net-of-cost walk-forward result.
type Report struct {
	StrategyID string          `json:"strategy_id"`
	Interval   domain.Interval `json:"interval"`
	Start      time.Time       `json:"start"`
	End        time.Time       `json:"end"`
	Folds      []Fold          `json:"folds"`
	Universe   []domain.Ticker `json:"universe"`
	Costs      []CostResult    `json:"costs"`
	Benchmark  BenchmarkResult `json:"benchmark"`
	Trials     int             `json:"trials_considered"`
	Verdict    Verdict         `json:"verdict"`
	Notes      []string        `json:"notes"`
	Disclaimer string          `json:"disclaimer"`
}

// BenchmarkResult is buy-and-hold over the same out-of-sample windows.
type BenchmarkResult struct {
	Ticker  domain.Ticker          `json:"ticker"`
	Metrics domain.BacktestMetrics `json:"metrics"`
	Error   string                 `json:"error,omitempty"`
}

// Verdict is the report's reading of its own numbers. It is deliberately
// mechanical: the thresholds are stated so that nobody has to trust prose.
type Verdict struct {
	SurvivesCosts  bool     `json:"survives_costs"`
	BeatsBenchmark bool     `json:"beats_benchmark"`
	PredictiveLift bool     `json:"predictive_lift"`
	Consistent     bool     `json:"consistent"`
	EnoughSamples  bool     `json:"enough_samples"`
	Summary        string   `json:"summary"`
	Reasons        []string `json:"reasons"`
}

// RunReport executes the walk-forward once per cost multiplier, scoring every
// prediction the pipeline makes against the realised outcome as the bars
// arrive, and compares the result with buy-and-hold over the same windows.
func (e *Engine) RunReport(ctx context.Context, cfg ReportConfig,
	barsFor func(start, end time.Time) ([]domain.Candle, error)) (Report, error) {

	if len(cfg.Folds) == 0 {
		return Report{}, fmt.Errorf("report: no folds")
	}
	if len(cfg.CostMultipliers) == 0 {
		cfg.CostMultipliers = []float64{1, 2}
	}
	if cfg.ResolveLag <= 0 {
		cfg.ResolveLag = 5 * 24 * time.Hour
	}
	if cfg.CalibrationBins <= 0 {
		cfg.CalibrationBins = 10
	}
	if cfg.MinRegimeSamples <= 0 {
		cfg.MinRegimeSamples = 100
	}
	if cfg.Base.Costs.ParticipationCap <= 0 {
		cfg.Base.Costs = domain.DefaultCostModel()
	}

	rep := Report{
		StrategyID: cfg.Base.StrategyID, Interval: cfg.Base.Interval,
		Start: cfg.Folds[0].TestStart, End: cfg.Folds[len(cfg.Folds)-1].TestEnd,
		Folds: cfg.Folds, Trials: cfg.Base.Trials, Disclaimer: domain.Disclaimer,
	}

	universe := map[domain.Ticker]bool{}
	for _, mult := range cfg.CostMultipliers {
		cr, err := e.runCostScenario(ctx, cfg, mult, barsFor)
		if err != nil {
			return rep, err
		}
		for _, f := range cr.WalkForward.Folds {
			for _, t := range f.Run.Universe {
				universe[t] = true
			}
		}
		rep.Costs = append(rep.Costs, cr)
	}
	for t := range universe {
		rep.Universe = append(rep.Universe, t)
	}
	sort.Slice(rep.Universe, func(i, j int) bool { return rep.Universe[i] < rep.Universe[j] })

	if cfg.Benchmark != "" {
		rep.Benchmark = benchmark(cfg, barsFor)
	}
	rep.Verdict = judge(rep, cfg)
	rep.Notes = notes(rep, cfg)
	return rep, nil
}

func (e *Engine) runCostScenario(ctx context.Context, cfg ReportConfig, mult float64,
	barsFor func(start, end time.Time) ([]domain.Candle, error)) (CostResult, error) {

	base := cfg.Base
	base.Costs = scaleCosts(cfg.Base.Costs, mult)
	base.Name = fmt.Sprintf("%s-x%.1f", nonEmpty(base.Name, "report"), mult)

	cr := CostResult{
		Multiplier: mult, Costs: base.Costs,
		Evaluation: map[domain.Horizon]domain.ModelEvaluation{},
		ByRegime:   map[domain.Horizon]map[domain.Regime]domain.ModelEvaluation{},
		VetoCost:   map[domain.Horizon]evaluation.VetoCost{},
		Rejections: map[string]int{},
	}

	// One scorer per fold, rebuilt with the pipeline so that a prediction made
	// in fold k can never be scored on a price from fold k+1's window. Every
	// outcome is pooled afterwards: pooling outcomes is honest, pooling
	// per-fold accuracies is not.
	var outcomes []domain.PredictionOutcome
	var scorer *evaluation.Scheduler
	var tradeFrom time.Time
	prevObserver := base.Observer
	base.Observer = func(bar domain.Candle, res pipeline.Result) {
		if prevObserver != nil {
			prevObserver(bar, res)
		}
		if scorer == nil {
			return
		}
		// Resolve first: this bar's close is the first observable price at or
		// after the horizon of anything created on an earlier bar.
		resolved := scorer.Resolve(bar.End, func(t domain.Ticker) (float64, bool) {
			if t == bar.Ticker {
				return bar.Close, true
			}
			return 0, false
		})
		outcomes = append(outcomes, resolved...)
		if res.Macro || bar.End.Before(tradeFrom) {
			return
		}
		if res.Rejection != nil {
			cr.Rejections[res.Rejection.Stage]++
		}
		if res.Signal != nil {
			cr.Signals++
		}
		if res.Prediction != nil {
			decision := domain.RiskWatchOnly
			if res.Risk != nil {
				decision = res.Risk.Decision
			}
			scorer.Track(*res.Prediction, bar.Close, decision)
			cr.Predictions++
		}
	}

	wrappedBars := func(start, end time.Time) ([]domain.Candle, error) {
		// A fresh scorer per fold. The clock argument is unused by Track and
		// Resolve, which take explicit times; the system clock is a placeholder.
		if scorer != nil {
			cr.Discarded += scorer.Discarded()
		}
		scorer = evaluation.NewScheduler(obs.SystemClock{}, nil, 1<<20)
		scorer.SetMaxLag(cfg.ResolveLag)
		tradeFrom = start
		if cfg.Warmup > 0 {
			tradeFrom = start.Add(cfg.Warmup)
		}
		return barsFor(start, end)
	}

	wf, err := e.RunWalkForwardWarm(ctx, base, cfg.Folds, cfg.Warmup, wrappedBars)
	if err != nil {
		return cr, err
	}
	if scorer != nil {
		cr.Discarded += scorer.Discarded()
	}
	cr.WalkForward = wf
	cr.Outcomes = len(outcomes)

	byHorizon := map[domain.Horizon][]domain.PredictionOutcome{}
	for _, o := range outcomes {
		byHorizon[o.Horizon] = append(byHorizon[o.Horizon], o)
	}
	for h, list := range byHorizon {
		cr.Evaluation[h] = evaluation.Evaluate(list, "walk-forward", cfg.CalibrationBins)
		cr.VetoCost[h] = evaluation.MeasureVetoCost(list)
		if rg := evaluation.ByRegime(list, cfg.CalibrationBins, cfg.MinRegimeSamples); len(rg) > 0 {
			cr.ByRegime[h] = rg
		}
	}
	return cr, nil
}

// scaleCosts multiplies every per-trade cost component. The participation cap
// and borrow rate are structural, not estimates, and are left alone.
func scaleCosts(c domain.CostModel, k float64) domain.CostModel {
	c.CommissionBps *= k
	c.CommissionPerShare *= k
	c.MinCommission *= k
	c.SpreadCostBps *= k
	c.SlippageBps *= k
	c.ImpactCoef *= k
	return c
}

// benchmark computes buy-and-hold over the concatenated test windows: buy at
// the first close of each fold's test window, hold to its last, and chain the
// folds the same way the strategy's curve is chained.
func benchmark(cfg ReportConfig, barsFor func(start, end time.Time) ([]domain.Candle, error)) BenchmarkResult {
	br := BenchmarkResult{Ticker: cfg.Benchmark}
	var curve []domain.EquityPoint
	equity := cfg.Base.StartingCash
	if equity <= 0 {
		equity = 100_000
	}
	for _, f := range cfg.Folds {
		bars, err := barsFor(f.TestStart, f.TestEnd)
		if err != nil {
			br.Error = err.Error()
			return br
		}
		var series []domain.Candle
		for _, c := range bars {
			if c.Ticker == cfg.Benchmark && !c.End.Before(f.TestStart) {
				series = append(series, c)
			}
		}
		if len(series) < 2 {
			continue
		}
		sort.Slice(series, func(i, j int) bool { return series[i].End.Before(series[j].End) })
		base := series[0].Close
		for _, c := range series {
			curve = append(curve, domain.EquityPoint{At: c.End, Equity: equity * c.Close / base, Exposure: 1})
		}
		equity = curve[len(curve)-1].Equity
	}
	if len(curve) < 2 {
		br.Error = fmt.Sprintf("no %s bars inside the test windows", cfg.Benchmark)
		return br
	}
	br.Metrics = ComputeMetrics(curve, nil, cfg.Base.RiskFree)
	return br
}

// judge reads the numbers. Thresholds, so they can be argued with:
//
//   - survives costs: aggregate Sharpe > 0 and total return > 0 at the
//     highest cost multiplier, with at least one trade;
//   - beats benchmark: Sharpe at the highest multiplier exceeds buy-and-hold
//     Sharpe over the same windows;
//   - predictive lift: at the configured cost (the scoring does not depend on
//     costs, but the first scenario is the canonical one) the shortest
//     horizon's accuracy exceeds its base rate by more than one standard
//     error of a binomial at that sample size;
//   - consistent: at least 60% of folds have a positive out-of-sample return
//     at the highest multiplier;
//   - enough samples: at least 30 trades and 500 scored predictions.
func judge(rep Report, cfg ReportConfig) Verdict {
	var v Verdict
	if len(rep.Costs) == 0 {
		v.Summary = "no cost scenarios were run"
		return v
	}
	worst := rep.Costs[len(rep.Costs)-1]
	agg := worst.WalkForward.Aggregate
	v.SurvivesCosts = agg.Trades > 0 && agg.Sharpe > 0 && agg.TotalReturn > 0
	v.Consistent = worst.WalkForward.Consistency >= 0.6
	v.BeatsBenchmark = rep.Benchmark.Error == "" && agg.Sharpe > rep.Benchmark.Metrics.Sharpe

	first := rep.Costs[0]
	var shortest domain.Horizon
	for h := range first.Evaluation {
		if shortest == "" || h.Duration() < shortest.Duration() {
			shortest = h
		}
	}
	scored := 0
	if shortest != "" {
		ev := first.Evaluation[shortest]
		scored = ev.Samples
		if ev.Samples > 0 {
			// ModelEvaluation.Lift is relative (accuracy/base − 1); the test
			// here is on the absolute difference against a binomial standard
			// error at the base rate, which is the quantity the s.e. describes.
			se := math.Sqrt(ev.BaseRate * (1 - ev.BaseRate) / float64(ev.Samples))
			absLift := ev.Accuracy - ev.BaseRate
			v.PredictiveLift = absLift > se
			v.Reasons = append(v.Reasons, fmt.Sprintf("%s: accuracy %.3f vs base rate %.3f (lift %+.3f, one s.e. %.3f, n=%d, Brier skill %+.3f)",
				shortest, ev.Accuracy, ev.BaseRate, absLift, se, ev.Samples, ev.BrierSkill))
		}
	}
	v.EnoughSamples = agg.Trades >= 30 && scored >= 500

	v.Reasons = append(v.Reasons,
		fmt.Sprintf("at %.1fx costs: %d trades, total return %+.2f%%, Sharpe %.2f, max drawdown %.2f%%, %.0f%% of folds positive",
			worst.Multiplier, agg.Trades, agg.TotalReturn*100, agg.Sharpe, agg.MaxDrawdown*100, worst.WalkForward.Consistency*100))
	if rep.Benchmark.Error == "" {
		b := rep.Benchmark.Metrics
		v.Reasons = append(v.Reasons, fmt.Sprintf("buy-and-hold %s over the same windows: total return %+.2f%%, Sharpe %.2f, max drawdown %.2f%%",
			rep.Benchmark.Ticker, b.TotalReturn*100, b.Sharpe, b.MaxDrawdown*100))
	}

	switch {
	case !v.EnoughSamples:
		v.Summary = "insufficient evidence: too few trades or scored predictions to say anything either way"
	case v.SurvivesCosts && v.BeatsBenchmark && v.PredictiveLift && v.Consistent:
		v.Summary = "edge not rejected: positive after stressed costs, ahead of buy-and-hold, predictions beat their base rate, and most folds were positive — this is the point at which paper trading on live data begins, not real money"
	case v.SurvivesCosts && !v.BeatsBenchmark:
		v.Summary = "survives costs but does not beat buy-and-hold over the same windows: the work is not paying for itself"
	default:
		v.Summary = "no demonstrated edge on this history after costs"
	}
	return v
}

func notes(rep Report, cfg ReportConfig) []string {
	n := []string{
		"every statistic is out-of-sample: computed on embargoed test windows the strategy never saw, chained in time order",
		"predictions are scored against the first close at or after their horizon; horizons are calendar durations, so 1d is the next session and 5d is roughly a trading week",
		"the deflated Sharpe ratio uses the trial count supplied by the caller; if more configurations were tried than reported, every Sharpe here is overstated",
		"survivorship: a membership table removes names before their inclusion date, but delisted names absent from the data directory are absent, not losing — the correction is partial",
	}
	if cfg.Base.FillAtNextOpen {
		n = append(n, "fills are at the next bar's open for the traded instrument, which is the first price actually available after a decision at the close")
	} else {
		n = append(n, "fills are at the last close, which on bars longer than a few minutes books the overnight gap in the strategy's favour")
	}
	for _, c := range rep.Costs {
		if c.Discarded > 0 {
			n = append(n, fmt.Sprintf("x%.1f: %d predictions were discarded as unresolvable (no bar within %s of the horizon), mostly around holidays", c.Multiplier, c.Discarded, cfg.ResolveLag))
		}
		if len(c.Rejections) > 0 {
			parts := make([]string, 0, len(c.Rejections))
			for stage, k := range c.Rejections {
				parts = append(parts, fmt.Sprintf("%s=%d", stage, k))
			}
			sort.Strings(parts)
			n = append(n, fmt.Sprintf("x%.1f: candidates declined by stage: %s", c.Multiplier, strings.Join(parts, ", ")))
		}
	}
	return n
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

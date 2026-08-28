package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func TestComputeMetricsOnAKnownCurve(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Ten daily observations rising 1% a day.
	var curve []domain.EquityPoint
	equity := 100_000.0
	for i := 0; i < 11; i++ {
		curve = append(curve, domain.EquityPoint{At: start.AddDate(0, 0, i), Equity: equity})
		equity *= 1.01
	}
	trades := []domain.BacktestTrade{
		{PnL: 100, ReturnPct: 0.01, EntryPrice: 100, ExitPrice: 101, Quantity: 100, HoldPeriod: 2 * time.Hour},
		{PnL: -50, ReturnPct: -0.005, EntryPrice: 100, ExitPrice: 99.5, Quantity: 100, HoldPeriod: time.Hour},
		{PnL: 200, ReturnPct: 0.02, EntryPrice: 100, ExitPrice: 102, Quantity: 100, HoldPeriod: 3 * time.Hour},
	}
	m := ComputeMetrics(curve, trades, 0)

	// Reported metrics are rounded to four decimal places on purpose, so that a
	// result hash is stable across platforms with different floating-point
	// summation orders. Assertions are made to that precision, not beyond it.
	const tol = 1e-4

	if math.Abs(m.TotalReturn-(math.Pow(1.01, 10)-1)) > tol {
		t.Fatalf("total return %.6f", m.TotalReturn)
	}
	if m.MaxDrawdown != 0 {
		t.Fatalf("a monotonically rising curve reported a %.4f drawdown", m.MaxDrawdown)
	}
	if m.Trades != 3 {
		t.Fatalf("counted %d trades", m.Trades)
	}
	if math.Abs(m.WinRate-2.0/3) > tol {
		t.Fatalf("win rate %.4f", m.WinRate)
	}
	if math.Abs(m.ProfitFactor-(300.0/50.0)) > tol {
		t.Fatalf("profit factor %.4f", m.ProfitFactor)
	}
	if math.Abs(m.Expectancy-(250.0/3)) > tol {
		t.Fatalf("expectancy %.4f", m.Expectancy)
	}
	// A curve that rises by exactly the same fraction every day has zero return
	// dispersion, so its Sharpe ratio is a division by zero. Reporting it as 0
	// rather than +Inf is the honest answer: the ratio is undefined, not
	// infinitely good. Sharpe is exercised on a dispersed curve below.
	if m.Sharpe != 0 {
		t.Fatalf("a zero-variance curve reported Sharpe %.4f instead of leaving it undefined", m.Sharpe)
	}
	if m.Volatility != 0 {
		t.Fatalf("a zero-variance curve reported volatility %.4f", m.Volatility)
	}
}

func TestSharpeIsPositiveOnADispersedRisingCurve(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Daily returns of roughly +2%, -1%, +3%, -0.5%, +2.5%: dispersed, but with
	// a positive mean.
	values := []float64{100, 102, 100.98, 104.01, 103.49, 106.07}
	var curve []domain.EquityPoint
	for i, v := range values {
		curve = append(curve, domain.EquityPoint{At: start.AddDate(0, 0, i), Equity: v})
	}
	m := ComputeMetrics(curve, nil, 0)
	if m.Sharpe <= 0 {
		t.Fatalf("a dispersed rising curve has Sharpe %.4f", m.Sharpe)
	}
	if m.Volatility <= 0 {
		t.Fatalf("a dispersed curve reported volatility %.4f", m.Volatility)
	}
}

func TestDrawdownIsMeasuredFromThePeak(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	curve := []domain.EquityPoint{
		{At: start, Equity: 100},
		{At: start.AddDate(0, 0, 1), Equity: 120},
		{At: start.AddDate(0, 0, 2), Equity: 90}, // -25% from the 120 peak
		{At: start.AddDate(0, 0, 3), Equity: 110},
		{At: start.AddDate(0, 0, 4), Equity: 130},
	}
	m := ComputeMetrics(curve, nil, 0)
	if math.Abs(m.MaxDrawdown-0.25) > 1e-9 {
		t.Fatalf("max drawdown %.4f, expected 0.25 measured from the peak", m.MaxDrawdown)
	}
}

func TestSortinoIgnoresUpsideVolatility(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// A curve with large gains and small losses: Sortino should exceed Sharpe,
	// because upside deviation is not risk.
	values := []float64{100, 110, 109, 125, 124, 145, 144, 170}
	var curve []domain.EquityPoint
	for i, v := range values {
		curve = append(curve, domain.EquityPoint{At: start.AddDate(0, 0, i), Equity: v})
	}
	m := ComputeMetrics(curve, nil, 0)
	if m.Sortino <= m.Sharpe {
		t.Fatalf("Sortino %.3f did not exceed Sharpe %.3f on an upside-skewed curve", m.Sortino, m.Sharpe)
	}
}

func TestDeflatedSharpeDiscountsTrials(t *testing.T) {
	single := DeflatedSharpe(2.0, 1, 250)
	many := DeflatedSharpe(2.0, 500, 250)
	if single != 2.0 {
		t.Fatalf("a single trial was discounted to %.4f", single)
	}
	if many >= single {
		t.Fatalf("500 trials were not discounted: %.4f vs %.4f", many, single)
	}
	if many < 0 && single > 0 {
		// A large discount is expected and fine; this only documents direction.
		t.Logf("deflated Sharpe with 500 trials: %.4f", many)
	}
}

func TestPartitionByRegimeSeparatesTrades(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	curve := []domain.EquityPoint{
		{At: start, Equity: 100}, {At: start.AddDate(0, 0, 1), Equity: 105},
	}
	trades := []domain.BacktestTrade{
		{Regime: domain.RegimeBullTrend, PnL: 100},
		{Regime: domain.RegimeBullTrend, PnL: 50},
		{Regime: domain.RegimeRange, PnL: -30},
	}
	by := PartitionByRegime(curve, trades, 0)
	if len(by) != 2 {
		t.Fatalf("expected two regimes, got %d", len(by))
	}
	if by[domain.RegimeBullTrend].Trades != 2 || by[domain.RegimeRange].Trades != 1 {
		t.Fatal("trades were not partitioned by the regime in force at entry")
	}
	if by[domain.RegimeBullTrend].WinRate != 1 {
		t.Fatalf("bull-trend win rate %.2f", by[domain.RegimeBullTrend].WinRate)
	}
	// Curve-derived statistics are not attributable to one regime and must be
	// blanked rather than repeated misleadingly.
	if by[domain.RegimeRange].TotalReturn != 0 || by[domain.RegimeRange].MaxDrawdown != 0 {
		t.Fatal("curve statistics were attributed to a single regime")
	}
}

// --- leakage guard ----------------------------------------------------------

func TestLeakageGuardRejectsAFutureFeature(t *testing.T) {
	g := LeakageGuard{Enabled: true}
	decision := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	snap := domain.FeatureSnapshot{Ticker: "AAPL", AsOf: decision.Add(time.Minute)}
	bar := domain.Candle{Ticker: "AAPL", End: decision}

	if err := g.Check(snap, decision, bar); err == nil {
		t.Fatal("a feature stamped after the decision was accepted; this is look-ahead bias")
	}
	if len(g.Violations) != 1 {
		t.Fatal("the violation was not recorded")
	}
}

func TestLeakageGuardRejectsAnUnclosedBar(t *testing.T) {
	g := LeakageGuard{Enabled: true}
	decision := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	snap := domain.FeatureSnapshot{Ticker: "AAPL", AsOf: decision}
	bar := domain.Candle{Ticker: "AAPL", End: decision.Add(time.Minute)}

	if err := g.Check(snap, decision, bar); err == nil {
		t.Fatal("a decision was allowed on a bar that had not closed")
	}
}

func TestLeakageGuardRejectsASameBarFill(t *testing.T) {
	g := LeakageGuard{Enabled: true}
	decision := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	if err := g.CheckFill(decision, decision, "AAPL"); err == nil {
		t.Fatal("a fill at the decision timestamp was accepted; this is the single most " +
			"common way a backtest reports returns it could never have earned")
	}
	if err := g.CheckFill(decision, decision.Add(time.Minute), "AAPL"); err != nil {
		t.Fatalf("a next-bar fill was rejected: %v", err)
	}
}

func TestLeakageGuardAcceptsCorrectOrdering(t *testing.T) {
	g := LeakageGuard{Enabled: true}
	decision := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	snap := domain.FeatureSnapshot{Ticker: "AAPL", AsOf: decision.Add(-time.Second)}
	bar := domain.Candle{Ticker: "AAPL", End: decision}
	if err := g.Check(snap, decision, bar); err != nil {
		t.Fatalf("correctly ordered inputs were rejected: %v", err)
	}
}

// --- walk-forward -----------------------------------------------------------

func TestBuildFoldsAppliesAnEmbargo(t *testing.T) {
	cfg := WalkForwardConfig{
		Start:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TrainWindow: 120 * 24 * time.Hour,
		ValidWindow: 30 * 24 * time.Hour,
		TestWindow:  30 * 24 * time.Hour,
		MaxHorizon:  24 * time.Hour,
	}
	folds, err := BuildFolds(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) < 3 {
		t.Fatalf("expected several folds, got %d", len(folds))
	}
	if err := Validate(folds); err != nil {
		t.Fatalf("the generated schedule failed its own validation: %v", err)
	}
	for _, f := range folds {
		embargo := f.EmbargoEnd.Sub(f.EmbargoStart)
		if embargo < 2*cfg.MaxHorizon {
			t.Fatalf("fold %d embargo is %s, below the two-horizon floor", f.Index, embargo)
		}
		if !f.TestStart.After(f.ValidEnd) {
			t.Fatalf("fold %d test window overlaps validation", f.Index)
		}
	}
}

func TestValidateRejectsAnOverlappingSchedule(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bad := []Fold{{
		Index:      0,
		TrainStart: base, TrainEnd: base.AddDate(0, 3, 0),
		ValidStart: base.AddDate(0, 3, 0), ValidEnd: base.AddDate(0, 4, 0),
		// Test starts before validation ends: the model would be scored on data
		// it was tuned on.
		TestStart: base.AddDate(0, 3, 15), TestEnd: base.AddDate(0, 5, 0),
	}}
	if err := Validate(bad); err == nil {
		t.Fatal("an overlapping schedule was accepted")
	}
}

func TestBuildFoldsRejectsAnImpossibleWindow(t *testing.T) {
	_, err := BuildFolds(WalkForwardConfig{
		Start:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC),
		TrainWindow: 365 * 24 * time.Hour,
		TestWindow:  30 * 24 * time.Hour,
	})
	if err == nil {
		t.Fatal("a window too short for even one fold produced folds")
	}
}

func TestAnchoredFoldsExpandTheTrainingWindow(t *testing.T) {
	cfg := WalkForwardConfig{
		Start:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TrainWindow: 120 * 24 * time.Hour,
		ValidWindow: 30 * 24 * time.Hour,
		TestWindow:  30 * 24 * time.Hour,
		MaxHorizon:  24 * time.Hour,
		Anchored:    true,
	}
	folds, err := BuildFolds(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) < 2 {
		t.Fatalf("expected several folds, got %d", len(folds))
	}
	for i := 1; i < len(folds); i++ {
		if !folds[i].TrainStart.Equal(folds[0].TrainStart) {
			t.Fatal("an anchored schedule moved the training start")
		}
		if !folds[i].TrainEnd.After(folds[i-1].TrainEnd) {
			t.Fatal("an anchored schedule did not expand the training window")
		}
	}
}

func TestSeededRandIsDeterministic(t *testing.T) {
	a, b := seededRand(42), seededRand(42)
	for i := 0; i < 100; i++ {
		x, y := a(), b()
		if x != y {
			t.Fatalf("two generators with the same seed diverged at draw %d", i)
		}
		if x < 0 || x >= 1 {
			t.Fatalf("draw %d is outside [0,1): %v", i, x)
		}
	}
	c := seededRand(43)
	if a() == c() {
		t.Fatal("different seeds produced identical draws")
	}
}

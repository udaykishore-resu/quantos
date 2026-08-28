package evaluation

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

var base = time.Date(2026, 3, 10, 14, 0, 0, 0, time.UTC)

func prediction(id string, dist domain.Distribution, createdAt time.Time) domain.Prediction {
	return domain.Prediction{
		ID: id, Ticker: "AAPL", ModelVersion: "v1", CreatedAt: createdAt,
		Regime: domain.RegimeBullTrend,
		Scenarios: []domain.ScenarioPrediction{{
			Horizon: domain.Horizon60m, Dist: dist, FlatBandBps: 20,
		}},
	}
}

// --- scheduler --------------------------------------------------------------

func TestPendingPredictionsAreNotResolvedEarly(t *testing.T) {
	s := NewScheduler(obs.NewSimClock(base), nil, 0)
	s.Track(prediction("p1", domain.NewDistribution(0.6, 0.25, 0.15), base), 100, domain.RiskAllowPaperSignal)

	out := s.Resolve(base.Add(30*time.Minute), func(domain.Ticker) (float64, bool) { return 105, true })
	if len(out) != 0 {
		t.Fatalf("a prediction was scored %d minutes before its horizon elapsed", 30)
	}
	if s.PendingCount() != 1 {
		t.Fatal("the pending prediction was dropped instead of retained")
	}
}

func TestResolutionScoresAgainstTheFlatBand(t *testing.T) {
	cases := []struct {
		name string
		exit float64
		want domain.Outcome
	}{
		// The band is 20 bps, so 100 -> 100.10 (10 bps) is FLAT, not UP.
		{"inside the band is flat", 100.10, domain.OutcomeFlat},
		{"above the band is up", 101.00, domain.OutcomeUp},
		{"below the band is down", 99.00, domain.OutcomeDown},
	}
	for _, c := range cases {
		s := NewScheduler(obs.NewSimClock(base), nil, 0)
		s.Track(prediction("p1", domain.NewDistribution(0.6, 0.25, 0.15), base), 100, domain.RiskAllowPaperSignal)
		out := s.Resolve(base.Add(time.Hour), func(domain.Ticker) (float64, bool) { return c.exit, true })
		if len(out) != 1 {
			t.Fatalf("%s: expected one outcome, got %d", c.name, len(out))
		}
		if out[0].Actual != c.want {
			t.Fatalf("%s: exit %.2f classified as %s, expected %s (return %.1f bps)",
				c.name, c.exit, out[0].Actual, c.want, out[0].ReturnBps)
		}
	}
}

// TestLatePredictionsAreDiscardedNotScored guards a real defect: resolving a
// prediction hours after its horizon against the current price measures the
// interval between the sweeps, not the prediction.
func TestLatePredictionsAreDiscardedNotScored(t *testing.T) {
	s := NewScheduler(obs.NewSimClock(base), nil, 0)
	s.Track(prediction("p1", domain.NewDistribution(0.6, 0.25, 0.15), base), 100, domain.RiskAllowPaperSignal)

	out := s.Resolve(base.Add(4*time.Hour), func(domain.Ticker) (float64, bool) { return 140, true })
	if len(out) != 0 {
		t.Fatalf("a prediction three hours past its horizon was scored against a %.0f bps move "+
			"that mostly happened outside its window", out[0].ReturnBps)
	}
	if s.Discarded() != 1 {
		t.Fatal("the discard was not counted, so a sweeper that fell behind would be invisible")
	}
	if s.PendingCount() != 0 {
		t.Fatal("the unresolvable prediction was retained and will be reconsidered forever")
	}
}

func TestMissingPriceRetainsThePrediction(t *testing.T) {
	s := NewScheduler(obs.NewSimClock(base), nil, 0)
	s.Track(prediction("p1", domain.NewDistribution(0.6, 0.25, 0.15), base), 100, domain.RiskAllowPaperSignal)

	out := s.Resolve(base.Add(time.Hour), func(domain.Ticker) (float64, bool) { return 0, false })
	if len(out) != 0 {
		t.Fatal("a prediction was scored without a price")
	}
	if s.PendingCount() != 1 {
		t.Fatal("a prediction with no price was dropped rather than retried")
	}
	// A price arriving inside the lag tolerance resolves it.
	out = s.Resolve(base.Add(time.Hour+time.Minute), func(domain.Ticker) (float64, bool) { return 101, true })
	if len(out) != 1 {
		t.Fatal("the retained prediction was not resolved once a price appeared")
	}
}

// TestBlockedPredictionsAreStillScored is ADR-009's falsifiability requirement:
// if only allowed predictions were evaluated, the veto could never be shown to
// be wrong.
func TestBlockedPredictionsAreStillScored(t *testing.T) {
	s := NewScheduler(obs.NewSimClock(base), nil, 0)
	s.Track(prediction("blocked", domain.NewDistribution(0.7, 0.2, 0.1), base), 100, domain.RiskBlock)
	out := s.Resolve(base.Add(time.Hour), func(domain.Ticker) (float64, bool) { return 102, true })
	if len(out) != 1 {
		t.Fatal("a blocked prediction was never evaluated")
	}
	if out[0].RiskDecision != domain.RiskBlock {
		t.Fatal("the outcome does not record that the prediction was blocked")
	}
}

// --- evaluation metrics -----------------------------------------------------

func outcome(pred, actual domain.Outcome, dist domain.Distribution, at time.Time) domain.PredictionOutcome {
	return domain.PredictionOutcome{
		Ticker: "AAPL", Horizon: domain.Horizon60m, ModelVersion: "v1",
		ResolvedAt: at, Predicted: pred, Actual: actual, Correct: pred == actual,
		Dist: dist, BrierScore: domain.BrierScore(dist, actual), LogLoss: domain.LogLoss(dist, actual),
		Regime: domain.RegimeBullTrend,
	}
}

func TestAccuracyIsReportedAgainstTheBaseRate(t *testing.T) {
	// Nine of ten outcomes are UP; a model that always says UP is 90% accurate
	// and completely uninformative. The base rate must expose that.
	var outs []domain.PredictionOutcome
	d := domain.NewDistribution(0.9, 0.05, 0.05)
	for i := 0; i < 9; i++ {
		outs = append(outs, outcome(domain.OutcomeUp, domain.OutcomeUp, d, base.Add(time.Duration(i)*time.Minute)))
	}
	outs = append(outs, outcome(domain.OutcomeUp, domain.OutcomeDown, d, base.Add(10*time.Minute)))

	e := Evaluate(outs, "test", 10)
	if math.Abs(e.Accuracy-0.9) > 1e-9 {
		t.Fatalf("accuracy %.4f", e.Accuracy)
	}
	if math.Abs(e.BaseRate-0.9) > 1e-9 {
		t.Fatalf("base rate %.4f: the constant-guess benchmark was not measured", e.BaseRate)
	}
	if math.Abs(e.Lift) > 1e-9 {
		t.Fatalf("lift %.4f: a model matching the base rate must show no lift", e.Lift)
	}
	if e.Samples != 10 {
		t.Fatalf("sample count %d", e.Samples)
	}
}

func TestConfusionMatrixDistinguishesPrecisionFromRecall(t *testing.T) {
	d := domain.NewDistribution(0.5, 0.3, 0.2)
	outs := []domain.PredictionOutcome{
		// Two correct UP calls, one false UP, and one missed UP.
		outcome(domain.OutcomeUp, domain.OutcomeUp, d, base),
		outcome(domain.OutcomeUp, domain.OutcomeUp, d, base.Add(time.Minute)),
		outcome(domain.OutcomeUp, domain.OutcomeDown, d, base.Add(2*time.Minute)),
		outcome(domain.OutcomeFlat, domain.OutcomeUp, d, base.Add(3*time.Minute)),
	}
	e := Evaluate(outs, "test", 10)
	if math.Abs(e.PrecisionUp-2.0/3) > 1e-4 {
		t.Fatalf("precision(UP) %.4f, expected 2/3", e.PrecisionUp)
	}
	if math.Abs(e.RecallUp-2.0/3) > 1e-4 {
		t.Fatalf("recall(UP) %.4f, expected 2/3", e.RecallUp)
	}
}

func TestCalibrationDetectsOverconfidence(t *testing.T) {
	// A model that claims 95% and is right half the time is badly calibrated,
	// and accuracy alone would not say so.
	var outs []domain.PredictionOutcome
	d := domain.NewDistribution(0.95, 0.03, 0.02)
	for i := 0; i < 100; i++ {
		actual := domain.OutcomeUp
		if i%2 == 0 {
			actual = domain.OutcomeDown
		}
		outs = append(outs, outcome(domain.OutcomeUp, actual, d, base.Add(time.Duration(i)*time.Minute)))
	}
	e := Evaluate(outs, "test", 10)
	if e.ExpectedCalibrationError < 0.4 {
		t.Fatalf("expected calibration error %.4f: a 95%%-confident coin flip was called well calibrated",
			e.ExpectedCalibrationError)
	}
	bins := Calibrate(outs, domain.OutcomeUp, 10)
	if len(bins) != 10 {
		t.Fatalf("expected ten reliability bins, got %d", len(bins))
	}
	last := bins[9]
	if last.Count != 100 {
		t.Fatalf("all predictions should land in the top bin, got %d", last.Count)
	}
	if math.Abs(last.Observed-0.5) > 1e-9 {
		t.Fatalf("observed frequency in the top bin %.4f", last.Observed)
	}
}

func TestBrierSkillIsNegativeForAWorseThanClimatologyModel(t *testing.T) {
	// The world is 50/50 up/down; the model insists on 95% up every time.
	var outs []domain.PredictionOutcome
	d := domain.NewDistribution(0.95, 0.03, 0.02)
	for i := 0; i < 100; i++ {
		actual := domain.OutcomeUp
		if i%2 == 0 {
			actual = domain.OutcomeDown
		}
		outs = append(outs, outcome(domain.OutcomeUp, actual, d, base.Add(time.Duration(i)*time.Minute)))
	}
	e := Evaluate(outs, "test", 10)
	if e.BrierSkill >= 0 {
		t.Fatalf("Brier skill %.4f: a model worse than the observed base rates scored as skilful", e.BrierSkill)
	}
}

func TestByRegimeSuppressesUnderpoweredPartitions(t *testing.T) {
	d := domain.NewDistribution(0.6, 0.25, 0.15)
	var outs []domain.PredictionOutcome
	for i := 0; i < 30; i++ {
		o := outcome(domain.OutcomeUp, domain.OutcomeUp, d, base.Add(time.Duration(i)*time.Minute))
		outs = append(outs, o)
	}
	// Two lonely range-regime samples: not enough to report an accuracy for.
	for i := 0; i < 2; i++ {
		o := outcome(domain.OutcomeUp, domain.OutcomeDown, d, base.Add(time.Duration(100+i)*time.Minute))
		o.Regime = domain.RegimeRange
		outs = append(outs, o)
	}
	by := ByRegime(outs, 10, 10)
	if _, ok := by[domain.RegimeRange]; ok {
		t.Fatal("an accuracy was reported for a two-sample regime partition")
	}
	if e, ok := by[domain.RegimeBullTrend]; !ok || e.Samples != 30 {
		t.Fatalf("the well-populated regime was not reported: %+v", by)
	}
}

func TestMeasureVetoCostMakesTheVetoFalsifiable(t *testing.T) {
	d := domain.NewDistribution(0.6, 0.25, 0.15)
	var outs []domain.PredictionOutcome
	// Allowed predictions: correct, +50 bps each.
	for i := 0; i < 10; i++ {
		o := outcome(domain.OutcomeUp, domain.OutcomeUp, d, base.Add(time.Duration(i)*time.Minute))
		o.RiskDecision = domain.RiskAllowPaperSignal
		o.ReturnBps = 50
		outs = append(outs, o)
	}
	// Blocked predictions: also correct, and would have returned more.
	for i := 0; i < 5; i++ {
		o := outcome(domain.OutcomeUp, domain.OutcomeUp, d, base.Add(time.Duration(50+i)*time.Minute))
		o.RiskDecision = domain.RiskBlock
		o.ReturnBps = 120
		outs = append(outs, o)
	}
	v := MeasureVetoCost(outs)
	if v.Allowed != 10 || v.Blocked != 5 {
		t.Fatalf("counted allowed=%d blocked=%d", v.Allowed, v.Blocked)
	}
	if v.BlockedMeanReturn <= v.AllowedMeanReturn {
		t.Fatalf("blocked mean %.1f did not exceed allowed mean %.1f in a rigged case",
			v.BlockedMeanReturn, v.AllowedMeanReturn)
	}
	if v.Interpretation == "" || !strings.Contains(v.Interpretation, "costly") {
		t.Fatalf("a costly veto was not described as such: %q", v.Interpretation)
	}
}

// --- drift ------------------------------------------------------------------

func TestPSIIsZeroForIdenticalDistributions(t *testing.T) {
	var xs []float64
	for i := 0; i < 1000; i++ {
		xs = append(xs, float64(i%50))
	}
	if psi := PSI(xs, xs, 10); psi > 1e-9 {
		t.Fatalf("PSI of a distribution against itself is %.6f", psi)
	}
}

func TestPSIGrowsWithShift(t *testing.T) {
	var ref, small, large []float64
	for i := 0; i < 1000; i++ {
		v := float64(i % 100)
		ref = append(ref, v)
		small = append(small, v+2)
		large = append(large, v+60)
	}
	a := PSI(ref, small, 10)
	b := PSI(ref, large, 10)
	if b <= a {
		t.Fatalf("a large shift (PSI %.4f) did not exceed a small one (PSI %.4f)", b, a)
	}
	if b < 0.25 {
		t.Fatalf("a 60%%-of-range shift produced PSI %.4f, below the significance threshold", b)
	}
}

func TestDetectorReportsNoDriftWithoutEnoughSamples(t *testing.T) {
	cfg := DefaultDriftConfig()
	cfg.Clock = obs.NewSimClock(base)
	d := NewDetector(cfg)
	for i := 0; i < 10; i++ {
		d.ObserveFeatures(domain.FeatureSnapshot{
			Ticker: "AAPL", AsOf: base,
			Values: map[string]float64{domain.FeatRSI14: 50 + float64(i)},
			Warm:   map[string]bool{domain.FeatRSI14: true},
		})
	}
	rep := d.Report("v1")
	if rep.Severity != domain.DriftNone {
		t.Fatalf("drift was graded %s on ten samples", rep.Severity)
	}
	if len(rep.DriftedFeatures) != 0 {
		t.Fatal("features were declared drifted without enough evidence")
	}
}

func TestDetectorFlagsAShiftedFeature(t *testing.T) {
	cfg := DefaultDriftConfig()
	cfg.Clock = obs.NewSimClock(base)
	cfg.Reference = 300
	cfg.Window = 300
	cfg.MinSamples = 100
	d := NewDetector(cfg)

	obsF := func(v float64) domain.FeatureSnapshot {
		return domain.FeatureSnapshot{
			Ticker: "AAPL", AsOf: base,
			Values: map[string]float64{domain.FeatRSI14: v},
			Warm:   map[string]bool{domain.FeatRSI14: true},
		}
	}
	// Reference window: RSI oscillating around 50.
	for i := 0; i < cfg.Reference; i++ {
		d.ObserveFeatures(obsF(40 + float64(i%20)))
	}
	// Recent window: RSI pinned high.
	for i := 0; i < cfg.Window; i++ {
		d.ObserveFeatures(obsF(85 + float64(i%5)))
	}
	rep := d.Report("v1")
	if rep.MaxFeaturePSI < cfg.PSIAlert {
		t.Fatalf("a feature that moved from ~50 to ~87 produced PSI %.4f", rep.MaxFeaturePSI)
	}
	if len(rep.DriftedFeatures) == 0 {
		t.Fatal("the drifted feature was not named")
	}
	if rep.Severity == domain.DriftNone {
		t.Fatal("severe feature drift was graded as none")
	}
	if rep.Recommendation == "" {
		t.Fatal("a drift report was produced with no recommended action")
	}
}

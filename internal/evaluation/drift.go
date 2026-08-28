package evaluation

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// DriftConfig parameterises detection.
type DriftConfig struct {
	// Window is the number of recent observations compared against Reference.
	Window    int
	Reference int
	// PSIWarn and PSIAlert are population-stability-index thresholds. 0.10-0.25
	// is the conventional "moderate shift" band and >0.25 "significant".
	PSIWarn  float64
	PSIAlert float64
	// AccuracyDrop thresholds are absolute drops against the reference window.
	AccuracyDropWarn  float64
	AccuracyDropAlert float64
	// KLAlert is the prediction-distribution divergence threshold.
	KLAlert    float64
	MinSamples int
	Bins       int
	Clock      obs.Clock
	Metrics    *obs.Metrics
}

// DefaultDriftConfig returns the shipped thresholds.
func DefaultDriftConfig() DriftConfig {
	return DriftConfig{
		Window: 500, Reference: 2000,
		PSIWarn: 0.15, PSIAlert: 0.25,
		AccuracyDropWarn: 0.05, AccuracyDropAlert: 0.10,
		KLAlert: 0.20, MinSamples: 100, Bins: 10,
		Clock: obs.SystemClock{},
	}
}

// Detector tracks feature and prediction distributions over time and reports
// drift.
//
// Three kinds of drift are distinguished because they have different remedies:
// feature drift means the world changed, prediction drift means the model's
// behaviour changed, and performance degradation means the model is now wrong.
// Only the third is unambiguously bad, and reporting all three separately stops
// an operator from reacting to a harmless input shift.
type Detector struct {
	cfg DriftConfig

	mu             sync.Mutex
	reference      map[string][]float64 // feature -> reference sample
	recent         map[string][]float64
	refPreds       []domain.Distribution
	recentPreds    []domain.Distribution
	refOutcomes    []domain.PredictionOutcome
	recentOutcomes []domain.PredictionOutcome
}

// NewDetector builds a drift detector.
func NewDetector(cfg DriftConfig) *Detector {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.Bins <= 0 {
		cfg.Bins = 10
	}
	return &Detector{
		cfg:       cfg,
		reference: map[string][]float64{},
		recent:    map[string][]float64{},
	}
}

// ObserveFeatures records a feature snapshot into the rolling windows.
func (d *Detector) ObserveFeatures(snap domain.FeatureSnapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range snap.Names() {
		v, ok := snap.Get(name)
		if !ok {
			continue
		}
		if len(d.reference[name]) < d.cfg.Reference {
			d.reference[name] = append(d.reference[name], v)
			continue
		}
		d.recent[name] = append(d.recent[name], v)
		if len(d.recent[name]) > d.cfg.Window {
			d.recent[name] = d.recent[name][1:]
		}
	}
}

// ObservePrediction records a prediction distribution.
func (d *Detector) ObservePrediction(p domain.Prediction) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dist := p.Primary().Dist
	if len(d.refPreds) < d.cfg.Reference {
		d.refPreds = append(d.refPreds, dist)
		return
	}
	d.recentPreds = append(d.recentPreds, dist)
	if len(d.recentPreds) > d.cfg.Window {
		d.recentPreds = d.recentPreds[1:]
	}
}

// ObserveOutcome records a resolved outcome.
func (d *Detector) ObserveOutcome(o domain.PredictionOutcome) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.refOutcomes) < d.cfg.Reference {
		d.refOutcomes = append(d.refOutcomes, o)
		return
	}
	d.recentOutcomes = append(d.recentOutcomes, o)
	if len(d.recentOutcomes) > d.cfg.Window {
		d.recentOutcomes = d.recentOutcomes[1:]
	}
}

// Report computes the current drift picture.
func (d *Detector) Report(modelVersion string) domain.DriftReport {
	d.mu.Lock()
	defer d.mu.Unlock()

	rep := domain.DriftReport{
		ModelVersion: modelVersion,
		ComputedAt:   d.cfg.Clock.Now(),
		Severity:     domain.DriftNone,
		FeatureDrift: map[string]float64{},
	}

	// --- Feature drift ------------------------------------------------------
	names := make([]string, 0, len(d.recent))
	for n := range d.recent {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ref, rec := d.reference[n], d.recent[n]
		if len(ref) < d.cfg.MinSamples || len(rec) < d.cfg.MinSamples/2 {
			continue
		}
		psi := PSI(ref, rec, d.cfg.Bins)
		rep.FeatureDrift[n] = round4(psi)
		if psi > rep.MaxFeaturePSI {
			rep.MaxFeaturePSI = round4(psi)
		}
		if psi >= d.cfg.PSIAlert {
			rep.DriftedFeatures = append(rep.DriftedFeatures, n)
		}
	}

	// --- Prediction drift ---------------------------------------------------
	if len(d.refPreds) >= d.cfg.MinSamples && len(d.recentPreds) >= d.cfg.MinSamples/2 {
		rep.PredictionDrift = round4(klDivergence(meanDist(d.refPreds), meanDist(d.recentPreds)))
	}

	// --- Performance --------------------------------------------------------
	rep.Samples = len(d.recentOutcomes)
	if len(d.refOutcomes) >= d.cfg.MinSamples && len(d.recentOutcomes) >= d.cfg.MinSamples/2 {
		refEval := Evaluate(append([]domain.PredictionOutcome(nil), d.refOutcomes...), "reference", d.cfg.Bins)
		recEval := Evaluate(append([]domain.PredictionOutcome(nil), d.recentOutcomes...), "recent", d.cfg.Bins)
		rep.ReferenceAccuracy = refEval.Accuracy
		rep.RecentAccuracy = recEval.Accuracy
		rep.AccuracyDelta = round4(recEval.Accuracy - refEval.Accuracy)
		rep.ReferenceBrier = refEval.BrierScore
		rep.RecentBrier = recEval.BrierScore

		// Regime degradation: accuracy below the base rate is the failure that
		// matters, because it means the model is worse than a constant guess.
		rep.RegimeDegradation = map[domain.Regime]float64{}
		for rg, e := range ByRegime(d.recentOutcomes, d.cfg.Bins, d.cfg.MinSamples/4) {
			if e.Accuracy < e.BaseRate {
				rep.RegimeDegradation[rg] = round4(e.Accuracy - e.BaseRate)
			}
		}
	}

	rep.Severity, rep.Reasons = d.grade(rep)
	rep.Recommendation = recommend(rep)

	if d.cfg.Metrics != nil {
		d.cfg.Metrics.ModelDrift.Set(severityOrdinal(rep.Severity), modelVersion, "overall")
		d.cfg.Metrics.ModelDrift.Set(rep.MaxFeaturePSI, modelVersion, "feature_psi")
		d.cfg.Metrics.ModelDrift.Set(rep.PredictionDrift, modelVersion, "prediction_kl")
	}
	return rep
}

func (d *Detector) grade(rep domain.DriftReport) (domain.DriftSeverity, []string) {
	sev := domain.DriftNone
	var reasons []string
	raise := func(s domain.DriftSeverity, reason string) {
		if severityOrdinal(s) > severityOrdinal(sev) {
			sev = s
		}
		reasons = append(reasons, reason)
	}

	switch {
	case rep.MaxFeaturePSI >= d.cfg.PSIAlert:
		raise(domain.DriftModerate, fmt.Sprintf(
			"feature population stability index reached %.2f on %v, above the %.2f alert threshold",
			rep.MaxFeaturePSI, rep.DriftedFeatures, d.cfg.PSIAlert))
	case rep.MaxFeaturePSI >= d.cfg.PSIWarn:
		raise(domain.DriftMild, fmt.Sprintf(
			"feature population stability index reached %.2f, above the %.2f warning threshold",
			rep.MaxFeaturePSI, d.cfg.PSIWarn))
	}

	if rep.PredictionDrift >= d.cfg.KLAlert {
		raise(domain.DriftModerate, fmt.Sprintf(
			"prediction distribution diverged from the reference window by %.2f nats", rep.PredictionDrift))
	}

	switch {
	case rep.AccuracyDelta <= -d.cfg.AccuracyDropAlert:
		raise(domain.DriftSevere, fmt.Sprintf(
			"accuracy fell by %.1f points against the reference window, beyond the %.1f-point alert threshold",
			-rep.AccuracyDelta*100, d.cfg.AccuracyDropAlert*100))
	case rep.AccuracyDelta <= -d.cfg.AccuracyDropWarn:
		raise(domain.DriftModerate, fmt.Sprintf(
			"accuracy fell by %.1f points against the reference window",
			-rep.AccuracyDelta*100))
	}

	if rep.RecentBrier > 0 && rep.ReferenceBrier > 0 && rep.RecentBrier > rep.ReferenceBrier*1.25 {
		raise(domain.DriftModerate, fmt.Sprintf(
			"Brier score worsened from %.3f to %.3f", rep.ReferenceBrier, rep.RecentBrier))
	}
	if len(rep.RegimeDegradation) > 0 {
		regimes := make([]string, 0, len(rep.RegimeDegradation))
		for rg := range rep.RegimeDegradation {
			regimes = append(regimes, string(rg))
		}
		sort.Strings(regimes)
		raise(domain.DriftSevere, fmt.Sprintf(
			"accuracy is below the base rate in regime(s) %v: the model is currently worse than a constant guess there", regimes))
	}
	return sev, reasons
}

func recommend(rep domain.DriftReport) string {
	switch rep.Severity {
	case domain.DriftSevere:
		return "Demote the model to shadow and fall back to the deterministic rule prior. Retrain on data including the recent regime before promoting again."
	case domain.DriftModerate:
		return "Raise the confidence floor in the risk engine and schedule a retrain. Continue serving, but treat probabilities as less reliable."
	case domain.DriftMild:
		return "Monitor. The input distribution has shifted but measured performance has not yet degraded."
	default:
		return "No action required."
	}
}

func severityOrdinal(s domain.DriftSeverity) float64 {
	switch s {
	case domain.DriftSevere:
		return 3
	case domain.DriftModerate:
		return 2
	case domain.DriftMild:
		return 1
	default:
		return 0
	}
}

// PSI computes the population stability index between a reference and a recent
// sample, using quantile bins from the reference distribution.
//
// Quantile bins rather than equal-width bins matter: with equal-width bins a
// single outlier in the reference sample creates mostly-empty buckets and the
// index becomes noise.
func PSI(reference, recent []float64, bins int) float64 {
	if len(reference) < bins*2 || len(recent) == 0 || bins < 2 {
		return 0
	}
	ref := append([]float64(nil), reference...)
	sort.Float64s(ref)

	edges := make([]float64, bins-1)
	for i := 1; i < bins; i++ {
		idx := int(float64(i) / float64(bins) * float64(len(ref)))
		if idx >= len(ref) {
			idx = len(ref) - 1
		}
		edges[i-1] = ref[idx]
	}

	refCounts := make([]int, bins)
	recCounts := make([]int, bins)
	for _, v := range reference {
		refCounts[bucketOf(v, edges)]++
	}
	for _, v := range recent {
		recCounts[bucketOf(v, edges)]++
	}

	psi := 0.0
	for i := 0; i < bins; i++ {
		// Laplace smoothing so an empty bucket does not produce an infinity.
		p := (float64(refCounts[i]) + 0.5) / (float64(len(reference)) + float64(bins)*0.5)
		q := (float64(recCounts[i]) + 0.5) / (float64(len(recent)) + float64(bins)*0.5)
		psi += (q - p) * math.Log(q/p)
	}
	if math.IsNaN(psi) || math.IsInf(psi, 0) {
		return 0
	}
	return psi
}

func bucketOf(v float64, edges []float64) int {
	i := sort.SearchFloat64s(edges, v)
	if i > len(edges) {
		i = len(edges)
	}
	return i
}

func meanDist(ds []domain.Distribution) domain.Distribution {
	if len(ds) == 0 {
		return domain.NewDistribution(1, 1, 1)
	}
	up, flat, down := 0.0, 0.0, 0.0
	for _, d := range ds {
		up += d.Up
		flat += d.Flat
		down += d.Down
	}
	return domain.NewDistribution(up, flat, down)
}

// klDivergence is D_KL(recent || reference) over the three outcome classes.
func klDivergence(reference, recent domain.Distribution) float64 {
	kl := 0.0
	for _, o := range domain.AllOutcomes {
		p := math.Max(recent.P(o), 1e-9)
		q := math.Max(reference.P(o), 1e-9)
		kl += p * math.Log(p/q)
	}
	return kl
}

// Health summarises evaluation and drift for the dashboard.
func Health(modelID, modelVersion string, stage domain.ModelStage,
	overall domain.ModelEvaluation, byRegime map[domain.Regime]domain.ModelEvaluation,
	drift domain.DriftReport, now time.Time) domain.ModelHealth {

	h := domain.ModelHealth{
		ModelID: modelID, ModelVersion: modelVersion, Stage: stage, AsOf: now,
		Accuracy: overall.Accuracy, BrierScore: overall.BrierScore,
		Calibration: round4(1 - overall.ExpectedCalibrationError),
		Drift:       drift.Severity, Samples: overall.Samples,
		LastEvaluatedAt: overall.ComputedAt,
	}
	if e, ok := byRegime[domain.RegimeHighVol]; ok {
		h.HighVolatilityAccuracy = e.Accuracy
	}
	if e, ok := byRegime[domain.RegimeBullTrend]; ok {
		h.TrendingAccuracy = e.Accuracy
	} else if e, ok := byRegime[domain.RegimeBearTrend]; ok {
		h.TrendingAccuracy = e.Accuracy
	}
	if e, ok := byRegime[domain.RegimeRange]; ok {
		h.RangeAccuracy = e.Accuracy
	}

	h.Healthy = true
	if overall.Samples < 50 {
		h.Healthy = false
		h.Issues = append(h.Issues, fmt.Sprintf("only %d resolved predictions: too few to judge", overall.Samples))
	}
	if overall.Samples >= 50 && overall.Accuracy < overall.BaseRate {
		h.Healthy = false
		h.Issues = append(h.Issues, fmt.Sprintf(
			"accuracy %.3f is below the base rate %.3f", overall.Accuracy, overall.BaseRate))
	}
	if overall.MaxCalibrationDeviation > 0.10 {
		h.Healthy = false
		h.Issues = append(h.Issues, fmt.Sprintf(
			"maximum calibration deviation %.3f exceeds the 0.10 promotion gate", overall.MaxCalibrationDeviation))
	}
	if drift.Severity == domain.DriftSevere || drift.Severity == domain.DriftModerate {
		h.Healthy = false
		h.Issues = append(h.Issues, "drift detected: "+drift.Recommendation)
	}
	return h
}

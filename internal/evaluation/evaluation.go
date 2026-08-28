// Package evaluation scores every prediction against its realised outcome and
// turns the results into calibration, accuracy and drift reports.
//
// The design rule is that a prediction is not finished when it is made. Every
// prediction becomes an evaluation record when its horizon elapses — including
// the ones the risk engine refused to act on, because the cost of the veto is
// only measurable if blocked predictions are scored too (ADR-009).
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

// Pending is a prediction awaiting resolution.
type Pending struct {
	PredictionID string
	Ticker       domain.Ticker
	Horizon      domain.Horizon
	CreatedAt    time.Time
	ResolveAt    time.Time
	EntryPrice   float64
	FlatBandBps  float64
	Dist         domain.Distribution
	Regime       domain.Regime
	ModelVersion string
	RiskDecision domain.RiskDecision
	FeatureHash  string
}

// MaxResolveLag bounds how late a prediction may be scored.
//
// Resolution uses the price observed at the sweep, so a prediction whose
// horizon elapsed hours ago would be scored against a price from hours after
// its window closed — which measures nothing. Past this lag the prediction is
// discarded as unresolvable rather than scored dishonestly, and the discard is
// counted so a sweeper that has fallen behind is visible.
const MaxResolveLag = 5 * time.Minute

// Scheduler tracks pending predictions and resolves them as their horizons
// elapse.
type Scheduler struct {
	mu      sync.Mutex
	pending map[string][]Pending // ticker -> pending, kept sorted by ResolveAt
	clock   obs.Clock
	metrics *obs.Metrics
	// resolved retains a bounded history for the rolling reports.
	resolved []domain.PredictionOutcome
	maxKeep  int
	// discarded counts predictions abandoned because the sweep ran too late to
	// score them honestly.
	discarded int
	// maxLag overrides MaxResolveLag, for tests and for backtests where the
	// event loop resolves exactly on the horizon.
	maxLag time.Duration
}

// SetMaxLag overrides the resolution lag tolerance.
func (s *Scheduler) SetMaxLag(d time.Duration) {
	s.mu.Lock()
	s.maxLag = d
	s.mu.Unlock()
}

// Discarded returns how many predictions were abandoned as unresolvable.
func (s *Scheduler) Discarded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discarded
}

// NewScheduler builds an evaluation scheduler.
func NewScheduler(clock obs.Clock, m *obs.Metrics, maxKeep int) *Scheduler {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	if maxKeep <= 0 {
		maxKeep = 50_000
	}
	return &Scheduler{pending: map[string][]Pending{}, clock: clock, metrics: m, maxKeep: maxKeep}
}

// Track registers every scenario of a prediction for later resolution.
func (s *Scheduler) Track(p domain.Prediction, entryPrice float64, decision domain.RiskDecision) {
	if entryPrice <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(p.Ticker)
	for _, sc := range p.Scenarios {
		s.pending[key] = append(s.pending[key], Pending{
			PredictionID: p.ID, Ticker: p.Ticker, Horizon: sc.Horizon,
			CreatedAt: p.CreatedAt, ResolveAt: p.CreatedAt.Add(sc.Horizon.Duration()),
			EntryPrice: entryPrice, FlatBandBps: sc.FlatBandBps, Dist: sc.Dist,
			Regime: p.Regime, ModelVersion: p.ModelVersion,
			RiskDecision: decision, FeatureHash: p.FeatureHash,
		})
	}
	sort.SliceStable(s.pending[key], func(i, j int) bool {
		return s.pending[key][i].ResolveAt.Before(s.pending[key][j].ResolveAt)
	})
}

// Resolve scores every pending prediction whose horizon has elapsed, using the
// supplied price lookup.
//
// A prediction whose price is unavailable at resolution time is retained rather
// than scored against a stale price: a wrong outcome is worse than a late one.
func (s *Scheduler) Resolve(now time.Time, priceAt func(domain.Ticker) (float64, bool)) []domain.PredictionOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()

	lag := s.maxLag
	if lag <= 0 {
		lag = MaxResolveLag
	}

	var out []domain.PredictionOutcome
	for key, list := range s.pending {
		kept := list[:0]
		for _, p := range list {
			if now.Before(p.ResolveAt) {
				kept = append(kept, p)
				continue
			}
			if now.Sub(p.ResolveAt) > lag {
				// Too late to score honestly: the price we can observe now is
				// not the price at the end of the prediction's window.
				s.discarded++
				continue
			}
			px, ok := priceAt(p.Ticker)
			if !ok || px <= 0 {
				// Keep it for the next sweep inside the lag tolerance.
				kept = append(kept, p)
				continue
			}
			out = append(out, score(p, px, now))
		}
		s.pending[key] = kept
		if len(kept) == 0 {
			delete(s.pending, key)
		}
	}
	s.resolved = append(s.resolved, out...)
	if len(s.resolved) > s.maxKeep {
		s.resolved = s.resolved[len(s.resolved)-s.maxKeep:]
	}
	if s.metrics != nil {
		for _, o := range out {
			acc := 0.0
			if o.Correct {
				acc = 1
			}
			s.metrics.PredictionAccuracy.Set(acc, o.ModelVersion, string(o.Horizon), string(o.Regime))
			s.metrics.PredictionBrier.Set(o.BrierScore, o.ModelVersion, string(o.Horizon))
		}
	}
	return out
}

// score turns one pending prediction into an outcome record.
func score(p Pending, exitPrice float64, now time.Time) domain.PredictionOutcome {
	retBps := (exitPrice/p.EntryPrice - 1) * 10000
	actual := domain.OutcomeFlat
	switch {
	case retBps > p.FlatBandBps:
		actual = domain.OutcomeUp
	case retBps < -p.FlatBandBps:
		actual = domain.OutcomeDown
	}
	predicted, _ := p.Dist.Argmax()
	return domain.PredictionOutcome{
		PredictionID: p.PredictionID, Ticker: p.Ticker, Horizon: p.Horizon,
		CreatedAt: p.CreatedAt, ResolvedAt: now,
		EntryPrice: round4(p.EntryPrice), ExitPrice: round4(exitPrice),
		ReturnBps: round2(retBps),
		Predicted: predicted, Actual: actual, Correct: predicted == actual,
		Dist:       p.Dist,
		BrierScore: round4(domain.BrierScore(p.Dist, actual)),
		LogLoss:    round4(domain.LogLoss(p.Dist, actual)),
		Regime:     p.Regime, ModelVersion: p.ModelVersion,
		RiskDecision: p.RiskDecision,
	}
}

// PendingCount returns how many predictions await resolution.
func (s *Scheduler) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.pending {
		n += len(l)
	}
	return n
}

// Resolved returns the retained outcome history.
func (s *Scheduler) Resolved() []domain.PredictionOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.PredictionOutcome(nil), s.resolved...)
}

// Evaluate computes a full evaluation over a set of outcomes.
func Evaluate(outcomes []domain.PredictionOutcome, window string, bins int) domain.ModelEvaluation {
	// ComputedAt is derived from the data rather than from the wall clock: an
	// evaluation of the same outcomes must produce the same report every time
	// it is run, including inside a backtest replayed months later.
	e := domain.ModelEvaluation{
		Window: window, Samples: len(outcomes), Confusion: domain.NewConfusionMatrix(),
	}
	if len(outcomes) == 0 {
		return e
	}
	if bins <= 0 {
		bins = 10
	}
	sort.SliceStable(outcomes, func(i, j int) bool { return outcomes[i].ResolvedAt.Before(outcomes[j].ResolvedAt) })
	e.Start, e.End = outcomes[0].ResolvedAt, outcomes[len(outcomes)-1].ResolvedAt
	e.ComputedAt = e.End
	e.Horizon = outcomes[0].Horizon
	e.ModelVersion = outcomes[0].ModelVersion

	brier, logloss := 0.0, 0.0
	counts := map[domain.Outcome]int{}
	for _, o := range outcomes {
		e.Confusion.Add(o.Predicted, o.Actual)
		brier += o.BrierScore
		logloss += o.LogLoss
		counts[o.Actual]++
	}
	n := float64(len(outcomes))
	e.Accuracy = round4(e.Confusion.Accuracy())
	e.BrierScore = round4(brier / n)
	e.LogLoss = round4(logloss / n)

	// The base rate is the accuracy of always predicting the most common class.
	// Reporting it is what makes the accuracy figure meaningful: 55% accuracy
	// against a 60% base rate is worse than useless.
	most := 0
	for _, c := range counts {
		if c > most {
			most = c
		}
	}
	e.BaseRate = round4(float64(most) / n)
	if e.BaseRate > 0 {
		e.Lift = round4(e.Accuracy/e.BaseRate - 1)
	}

	// Brier skill against a climatological forecast (the observed base rates).
	clim := domain.NewDistribution(
		float64(counts[domain.OutcomeUp]), float64(counts[domain.OutcomeFlat]), float64(counts[domain.OutcomeDown]))
	climBrier := 0.0
	for _, o := range outcomes {
		climBrier += domain.BrierScore(clim, o.Actual)
	}
	climBrier /= n
	if climBrier > 0 {
		e.BrierSkill = round4(1 - e.BrierScore/climBrier)
	}

	e.PrecisionUp = round4(e.Confusion.Precision(domain.OutcomeUp))
	e.RecallUp = round4(e.Confusion.Recall(domain.OutcomeUp))
	e.PrecisionDown = round4(e.Confusion.Precision(domain.OutcomeDown))
	e.RecallDown = round4(e.Confusion.Recall(domain.OutcomeDown))
	e.MacroF1 = round4((e.Confusion.F1(domain.OutcomeUp) + e.Confusion.F1(domain.OutcomeFlat) + e.Confusion.F1(domain.OutcomeDown)) / 3)

	e.Calibration = Calibrate(outcomes, domain.OutcomeUp, bins)
	e.MaxCalibrationDeviation, e.ExpectedCalibrationError = calibrationError(e.Calibration, len(outcomes))
	return e
}

// Calibrate builds a reliability diagram for one class: for each bucket of
// predicted probability, what fraction actually occurred.
func Calibrate(outcomes []domain.PredictionOutcome, class domain.Outcome, bins int) []domain.CalibrationBin {
	if bins <= 0 {
		bins = 10
	}
	type acc struct {
		n        int
		sumP     float64
		observed int
	}
	buckets := make([]acc, bins)
	for _, o := range outcomes {
		p := o.Dist.P(class)
		idx := int(p * float64(bins))
		if idx >= bins {
			idx = bins - 1
		}
		if idx < 0 {
			idx = 0
		}
		buckets[idx].n++
		buckets[idx].sumP += p
		if o.Actual == class {
			buckets[idx].observed++
		}
	}
	out := make([]domain.CalibrationBin, 0, bins)
	for i, b := range buckets {
		lo := float64(i) / float64(bins)
		hi := float64(i+1) / float64(bins)
		cb := domain.CalibrationBin{Lower: round4(lo), Upper: round4(hi), Count: b.n}
		if b.n > 0 {
			cb.MeanPredicted = round4(b.sumP / float64(b.n))
			cb.Observed = round4(float64(b.observed) / float64(b.n))
			cb.Deviation = round4(cb.Observed - cb.MeanPredicted)
		}
		out = append(out, cb)
	}
	return out
}

// calibrationError returns the maximum absolute deviation and the sample-
// weighted expected calibration error.
func calibrationError(bins []domain.CalibrationBin, total int) (float64, float64) {
	maxDev, ece := 0.0, 0.0
	for _, b := range bins {
		if b.Count == 0 {
			continue
		}
		d := math.Abs(b.Deviation)
		if d > maxDev {
			maxDev = d
		}
		ece += float64(b.Count) / float64(total) * d
	}
	return round4(maxDev), round4(ece)
}

// ByRegime partitions outcomes and evaluates each regime separately.
//
// This is the report that actually matters operationally: a model that is
// excellent in a trend and useless in a range is not a 60%-accurate model, it
// is two different models wearing one name.
func ByRegime(outcomes []domain.PredictionOutcome, bins, minSamples int) map[domain.Regime]domain.ModelEvaluation {
	byRegime := map[domain.Regime][]domain.PredictionOutcome{}
	for _, o := range outcomes {
		byRegime[o.Regime] = append(byRegime[o.Regime], o)
	}
	out := map[domain.Regime]domain.ModelEvaluation{}
	for rg, list := range byRegime {
		if rg == "" || len(list) < minSamples {
			continue
		}
		e := Evaluate(list, string(rg), bins)
		e.Regime = rg
		out[rg] = e
	}
	return out
}

// VetoCost measures what the risk engine's refusals cost, by evaluating the
// predictions it blocked.
type VetoCost struct {
	Blocked           int     `json:"blocked"`
	WatchOnly         int     `json:"watch_only"`
	Allowed           int     `json:"allowed"`
	BlockedAccuracy   float64 `json:"blocked_accuracy"`
	AllowedAccuracy   float64 `json:"allowed_accuracy"`
	BlockedMeanReturn float64 `json:"blocked_mean_return_bps"`
	AllowedMeanReturn float64 `json:"allowed_mean_return_bps"`
	Interpretation    string  `json:"interpretation"`
}

// MeasureVetoCost compares the predictions risk allowed against those it
// blocked, which is what makes the veto falsifiable rather than superstitious.
func MeasureVetoCost(outcomes []domain.PredictionOutcome) VetoCost {
	var v VetoCost
	blockedCorrect, allowedCorrect := 0, 0
	blockedRet, allowedRet := 0.0, 0.0
	for _, o := range outcomes {
		switch o.RiskDecision {
		case domain.RiskBlock:
			v.Blocked++
			if o.Correct {
				blockedCorrect++
			}
			blockedRet += directional(o)
		case domain.RiskWatchOnly:
			v.WatchOnly++
		default:
			v.Allowed++
			if o.Correct {
				allowedCorrect++
			}
			allowedRet += directional(o)
		}
	}
	if v.Blocked > 0 {
		v.BlockedAccuracy = round4(float64(blockedCorrect) / float64(v.Blocked))
		v.BlockedMeanReturn = round2(blockedRet / float64(v.Blocked))
	}
	if v.Allowed > 0 {
		v.AllowedAccuracy = round4(float64(allowedCorrect) / float64(v.Allowed))
		v.AllowedMeanReturn = round2(allowedRet / float64(v.Allowed))
	}
	switch {
	case v.Blocked == 0:
		v.Interpretation = "no predictions were blocked in this window, so the veto had no measurable cost"
	case v.BlockedMeanReturn > v.AllowedMeanReturn:
		v.Interpretation = fmt.Sprintf(
			"blocked setups would have returned %.1f bps on average against %.1f bps for allowed ones: the veto was costly in this window and its thresholds deserve review",
			v.BlockedMeanReturn, v.AllowedMeanReturn)
	default:
		v.Interpretation = fmt.Sprintf(
			"blocked setups would have returned %.1f bps on average against %.1f bps for allowed ones: the veto was protective in this window",
			v.BlockedMeanReturn, v.AllowedMeanReturn)
	}
	return v
}

// directional returns the return the predicted direction would have earned.
func directional(o domain.PredictionOutcome) float64 {
	switch o.Predicted {
	case domain.OutcomeUp:
		return o.ReturnBps
	case domain.OutcomeDown:
		return -o.ReturnBps
	default:
		return 0
	}
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10000) / 10000
}

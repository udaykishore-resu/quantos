package domain

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Outcome is the discrete class a prediction distributes probability over.
// Three classes with an explicit FLAT band is deliberate: a two-class model
// forces a directional call on noise, and "no material move" is the most common
// short-horizon outcome.
type Outcome string

const (
	OutcomeUp   Outcome = "UP"
	OutcomeFlat Outcome = "FLAT"
	OutcomeDown Outcome = "DOWN"
)

// AllOutcomes is the canonical ordering used by models, storage and metrics.
var AllOutcomes = []Outcome{OutcomeUp, OutcomeFlat, OutcomeDown}

// Horizon is a forecast horizon.
type Horizon string

const (
	Horizon15m Horizon = "15m"
	Horizon60m Horizon = "60m"
	Horizon1d  Horizon = "1d"
	Horizon5d  Horizon = "5d"
)

// Duration converts a Horizon to a time.Duration.
func (h Horizon) Duration() time.Duration {
	switch h {
	case Horizon15m:
		return 15 * time.Minute
	case Horizon60m:
		return time.Hour
	case Horizon1d:
		return 24 * time.Hour
	case Horizon5d:
		return 5 * 24 * time.Hour
	default:
		return time.Hour
	}
}

// DefaultHorizons is the set the platform predicts on by default.
var DefaultHorizons = []Horizon{Horizon15m, Horizon60m, Horizon1d}

// Distribution is a probability distribution over outcomes. It is always
// normalised; construction goes through NewDistribution which enforces that.
type Distribution struct {
	Up   float64 `json:"up"`
	Flat float64 `json:"flat"`
	Down float64 `json:"down"`
}

// NewDistribution normalises raw non-negative weights into a distribution.
// Zero or invalid input yields the maximum-entropy distribution, which is the
// honest answer when the model has nothing to say.
func NewDistribution(up, flat, down float64) Distribution {
	up, flat, down = nonNeg(up), nonNeg(flat), nonNeg(down)
	sum := up + flat + down
	if sum <= 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
		return Distribution{Up: 1.0 / 3, Flat: 1.0 / 3, Down: 1.0 / 3}
	}
	return Distribution{Up: up / sum, Flat: flat / sum, Down: down / sum}
}

func nonNeg(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// P returns the probability assigned to an outcome.
func (d Distribution) P(o Outcome) float64 {
	switch o {
	case OutcomeUp:
		return d.Up
	case OutcomeFlat:
		return d.Flat
	case OutcomeDown:
		return d.Down
	}
	return 0
}

// Argmax returns the most likely outcome and its probability. Ties resolve to
// FLAT, which is the conservative choice.
func (d Distribution) Argmax() (Outcome, float64) {
	best, p := OutcomeFlat, d.Flat
	if d.Up > p {
		best, p = OutcomeUp, d.Up
	}
	if d.Down > p {
		best, p = OutcomeDown, d.Down
	}
	return best, p
}

// Entropy returns Shannon entropy in nats, 0 for a point mass and ln(3) for
// maximum uncertainty.
func (d Distribution) Entropy() float64 {
	h := 0.0
	for _, p := range []float64{d.Up, d.Flat, d.Down} {
		if p > 0 {
			h -= p * math.Log(p)
		}
	}
	return h
}

// Uncertainty is normalised entropy in [0,1]. 1 means "no information".
func (d Distribution) Uncertainty() float64 { return d.Entropy() / math.Log(3) }

// Confidence is the normalised margin of the most likely outcome over the
// uniform distribution: 0 when the model has nothing to say, 1 for a point mass.
//
// It is deliberately *not* 1 - Uncertainty. Normalised entropy over three
// classes is a poor threshold: the requirements' own worked example
// (0.63 / 0.21 / 0.16) has an entropy ratio of 0.83, which would read as
// "confidence 0.17" and fail every sensible gate. The max-probability margin
// gives that example 0.45, which is a number an operator can reason about.
func (d Distribution) Confidence() float64 {
	_, p := d.Argmax()
	const uniform = 1.0 / 3
	return Clamp01((p - uniform) / (1 - uniform))
}

// DirectionalEdge is P(up) - P(down), in [-1,1]. It is the quantity the
// opportunity engine uses, not the argmax, because a 0.40/0.35/0.25 split
// carries different information from 0.40/0.20/0.40.
func (d Distribution) DirectionalEdge() float64 { return d.Up - d.Down }

// Blend returns a weighted mixture of two distributions.
func (d Distribution) Blend(other Distribution, w float64) Distribution {
	w = Clamp(w, 0, 1)
	return NewDistribution(
		d.Up*(1-w)+other.Up*w,
		d.Flat*(1-w)+other.Flat*w,
		d.Down*(1-w)+other.Down*w,
	)
}

// Sharpen applies a temperature to the distribution. t<1 sharpens, t>1 flattens.
// Used by calibration, never to manufacture confidence.
func (d Distribution) Sharpen(t float64) Distribution {
	if t <= 0 {
		return d
	}
	return NewDistribution(
		math.Pow(d.Up, 1/t),
		math.Pow(d.Flat, 1/t),
		math.Pow(d.Down, 1/t),
	)
}

// Valid checks normalisation within tolerance.
func (d Distribution) Valid() error {
	s := d.Up + d.Flat + d.Down
	if math.Abs(s-1) > 1e-6 {
		return fmt.Errorf("distribution not normalised: sum=%.9f", s)
	}
	if d.Up < 0 || d.Flat < 0 || d.Down < 0 {
		return fmt.Errorf("distribution has negative mass")
	}
	return nil
}

// ScenarioPrediction is the distribution for one horizon plus the move
// thresholds that define the classes.
type ScenarioPrediction struct {
	Horizon Horizon      `json:"horizon"`
	Dist    Distribution `json:"distribution"`
	// FlatBandBps is the half-width of the FLAT class in basis points. Without
	// it, "UP: 0.63" is meaningless.
	FlatBandBps float64 `json:"flat_band_bps"`
	// ExpectedMoveBps is the probability-weighted absolute move, derived from
	// the distribution and the volatility estimate.
	ExpectedMoveBps float64 `json:"expected_move_bps"`
	Confidence      float64 `json:"confidence"`
	Uncertainty     float64 `json:"uncertainty"`
}

// PredictionSource records which layer produced a distribution.
type PredictionSource string

const (
	SourceModel         PredictionSource = "model"
	SourceRulesFallback PredictionSource = "rules-fallback"
	SourceBlend         PredictionSource = "blend"
)

// Prediction is one model output for one symbol at one instant, across all
// horizons. It carries everything needed to reproduce and later evaluate it.
type Prediction struct {
	ID        string    `json:"id"`
	Ticker    Ticker    `json:"ticker"`
	CreatedAt time.Time `json:"created_at"`

	Scenarios []ScenarioPrediction `json:"scenarios"`

	// Provenance.
	ModelID          string           `json:"model_id"`
	ModelVersion     string           `json:"model_version"`
	ArtifactSHA      string           `json:"artifact_sha,omitempty"`
	Source           PredictionSource `json:"source"`
	FeatureHash      string           `json:"feature_hash"`
	FeatureAsOf      time.Time        `json:"feature_as_of"`
	Regime           Regime           `json:"regime"`
	RegimeConfidence float64          `json:"regime_confidence"`
	RulePrior        Distribution     `json:"rule_prior"`
	ModelOutput      Distribution     `json:"model_output"`
	BlendWeight      float64          `json:"blend_weight"`

	// Contributions is the per-feature contribution to the model's log-odds,
	// used for explanation. It is computed from the model, not invented.
	Contributions []FeatureContribution `json:"contributions,omitempty"`

	// Degraded lists subsystems that were unavailable when this was produced.
	Degraded []string `json:"degraded,omitempty"`
	// Disclaimer is carried on the record itself, not only at the API edge.
	Disclaimer string `json:"disclaimer"`
}

// FeatureContribution attributes part of a prediction to one feature.
type FeatureContribution struct {
	Feature      string  `json:"feature"`
	Value        float64 `json:"value"`
	Weight       float64 `json:"weight"`
	Contribution float64 `json:"contribution"`
}

// Scenario returns the prediction for one horizon.
func (p Prediction) Scenario(h Horizon) (ScenarioPrediction, bool) {
	for _, s := range p.Scenarios {
		if s.Horizon == h {
			return s, true
		}
	}
	return ScenarioPrediction{}, false
}

// Primary returns the shortest-horizon scenario, which drives intraday alerts.
func (p Prediction) Primary() ScenarioPrediction {
	if len(p.Scenarios) == 0 {
		return ScenarioPrediction{Horizon: Horizon60m, Dist: NewDistribution(1, 1, 1)}
	}
	out := p.Scenarios[0]
	for _, s := range p.Scenarios[1:] {
		if s.Horizon.Duration() < out.Horizon.Duration() {
			out = s
		}
	}
	return out
}

// TopContributions returns the n largest absolute contributions.
func (p Prediction) TopContributions(n int) []FeatureContribution {
	c := append([]FeatureContribution(nil), p.Contributions...)
	sort.Slice(c, func(i, j int) bool {
		return math.Abs(c[i].Contribution) > math.Abs(c[j].Contribution)
	})
	if len(c) > n {
		c = c[:n]
	}
	return c
}

// PredictionOutcome is the realised result of a prediction once its horizon has
// elapsed.
type PredictionOutcome struct {
	PredictionID string    `json:"prediction_id"`
	Ticker       Ticker    `json:"ticker"`
	Horizon      Horizon   `json:"horizon"`
	CreatedAt    time.Time `json:"created_at"`
	ResolvedAt   time.Time `json:"resolved_at"`

	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price"`
	ReturnBps  float64 `json:"return_bps"`

	Predicted  Outcome      `json:"predicted"`
	Actual     Outcome      `json:"actual"`
	Correct    bool         `json:"correct"`
	Dist       Distribution `json:"distribution"`
	BrierScore float64      `json:"brier_score"`
	LogLoss    float64      `json:"log_loss"`

	Regime       Regime `json:"regime"`
	ModelVersion string `json:"model_version"`
	// RiskDecision records whether this prediction was allowed to become a
	// signal, so the cost of the risk veto is measurable.
	RiskDecision RiskDecision `json:"risk_decision"`
}

// BrierScore is the multiclass Brier score for a distribution against a
// realised outcome. Lower is better; 0 is perfect, 2 is worst possible.
func BrierScore(d Distribution, actual Outcome) float64 {
	s := 0.0
	for _, o := range AllOutcomes {
		y := 0.0
		if o == actual {
			y = 1
		}
		diff := d.P(o) - y
		s += diff * diff
	}
	return s
}

// LogLoss is the negative log likelihood of the realised outcome, clipped to
// avoid infinities.
func LogLoss(d Distribution, actual Outcome) float64 {
	p := Clamp(d.P(actual), 1e-12, 1)
	return -math.Log(p)
}

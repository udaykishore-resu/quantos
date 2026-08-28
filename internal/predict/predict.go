// Package predict turns features, rules and a model into calibrated
// probabilistic scenarios.
//
// Three properties are non-negotiable here. Output is always a distribution
// over UP/FLAT/DOWN with an explicit flat band — "the stock will go up" is not
// expressible by this API. Every prediction carries the provenance needed to
// reproduce it: feature hash, model version, artifact digest, rule prior and
// blend weight. And when the model is unavailable the engine degrades to the
// deterministic rule prior with a capped confidence, rather than failing or,
// worse, guessing.
package predict

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/mlinfer"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/rules"
)

// Config parameterises the prediction engine.
type Config struct {
	ModelID  string
	Horizons []domain.Horizon
	// FlatBandBps is the half-width of the FLAT class, in basis points of the
	// reference price, for the primary horizon.
	FlatBandBps float64
	// BlendWeight is the *maximum* weight given to the model against the rule
	// prior. 1.0 is model-only; 0.0 is rules-only. The weight actually used is
	// scaled by how much the model has been measured to beat a constant guess;
	// see BlendReferenceLift.
	BlendWeight float64
	// DirectionalReferenceAUC is the UP-vs-DOWN AUC above 0.5 at which a model
	// earns full weight on the *direction* of a prediction, as opposed to on
	// how much probability sits in the FLAT band.
	//
	// The two are separated because a model can be measurably good at one and
	// measurably useless at the other, and blending a directional prior with a
	// model that cannot tell up from down pulls the prediction towards the
	// middle while looking like an improvement. Unmeasured counts as no skill,
	// which is the same rule the risk engine applies to everything else.
	DirectionalReferenceAUC float64
	// BlendReferenceLift is the accuracy lift over the base rate at which a
	// model earns its full BlendWeight.
	//
	// A fixed blend weight assumes every model that loads is worth listening
	// to. It is not: a model with no measured directional skill still absorbs
	// its share of the blend, diluting the deterministic prior towards the
	// middle, and the resulting prediction is less informative than either
	// input alone. Scaling the weight by measured lift means an uninformative
	// model contributes nothing and a good one contributes fully, with no
	// intervention and no separate promotion switch.
	BlendReferenceLift float64
	// FallbackCeiling caps confidence when serving from the rule prior alone,
	// so a degraded prediction naturally fails the risk engine's confidence
	// check rather than looking as authoritative as a model-backed one.
	FallbackCeiling float64
	Clock           obs.Clock
	Metrics         *obs.Metrics
}

// DefaultConfig returns sensible prediction parameters.
func DefaultConfig() Config {
	return Config{
		ModelID:                 "direction-3class",
		Horizons:                domain.DefaultHorizons,
		FlatBandBps:             15,
		BlendWeight:             0.6,
		BlendReferenceLift:      0.05,
		DirectionalReferenceAUC: 0.03,
		FallbackCeiling:         0.55,
		Clock:                   obs.SystemClock{},
	}
}

// effectiveBlendWeight scales the configured weight by the model's measured
// lift over its own base rate.
//
// The lift comes from the artifact's held-out report card, which mlinfer
// refuses to load without. A model at or below its base rate gets weight zero:
// it has been measured to know nothing, and averaging the deterministic prior
// with a coin flip produces a worse prediction than the prior alone.
func (e *Engine) effectiveBlendWeight(m *mlinfer.Model) float64 {
	max := domain.Clamp01(e.cfg.BlendWeight)
	ref := e.cfg.BlendReferenceLift
	if ref <= 0 {
		return max
	}
	md := m.Artifact().Metadata
	lift := md.TestAccuracy - md.TestBaseRate
	if lift <= 0 {
		return 0
	}
	return max * domain.Clamp01(lift/ref)
}

// directionalWeight is the weight the model earns over the *direction* of the
// prediction, as distinct from the FLAT/moving split.
func (e *Engine) directionalWeight(m *mlinfer.Model, blend float64) float64 {
	ref := e.cfg.DirectionalReferenceAUC
	if ref <= 0 {
		return blend
	}
	edge := m.Artifact().Metadata.TestDirectionalAUC - 0.5
	if edge <= 0 {
		// Either measured at or below a coin flip, or never measured at all.
		// Both mean the same thing here: the direction comes from the rules.
		return 0
	}
	return blend * domain.Clamp01(edge/ref)
}

// blendDirectional mixes a rule prior with a model output, using a separate
// weight for the direction.
//
// Splitting the blend this way is what lets the platform take the part of a
// model that has been measured to work without taking the part that has not.
// At wDir = 0 the FLAT mass still moves towards the model while UP and DOWN
// keep the prior's ratio exactly.
func blendDirectional(prior, model domain.Distribution, w, wDir float64) domain.Distribution {
	w = domain.Clamp01(w)
	wDir = domain.Clamp01(wDir)
	if wDir == w {
		return prior.Blend(model, w)
	}
	flat := (1-w)*prior.Flat + w*model.Flat
	moving := 1 - flat
	if moving <= 0 {
		return domain.NewDistribution(0, 1, 0)
	}
	share := func(d domain.Distribution) float64 {
		den := d.Up + d.Down
		if den <= 0 {
			return 0.5
		}
		return d.Up / den
	}
	up := (1-wDir)*share(prior) + wDir*share(model)
	return domain.NewDistribution(moving*up, flat, moving*(1-up))
}

// Engine produces predictions.
type Engine struct {
	cfg      Config
	registry *mlinfer.Registry

	mu     sync.RWMutex
	latest map[domain.Ticker]domain.Prediction
	// degraded records why the engine is not model-backed, for the health
	// endpoint and for the Degraded field on each prediction.
	degraded []string
}

// NewEngine builds a prediction engine. A nil registry is legal and puts the
// engine permanently in rule-prior fallback, which is what `make demo` uses
// before any model has been trained.
func NewEngine(cfg Config, reg *mlinfer.Registry) *Engine {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if len(cfg.Horizons) == 0 {
		cfg.Horizons = domain.DefaultHorizons
	}
	if cfg.FlatBandBps <= 0 {
		cfg.FlatBandBps = 15
	}
	e := &Engine{cfg: cfg, registry: reg, latest: map[domain.Ticker]domain.Prediction{}}
	if reg == nil || len(reg.All()) == 0 {
		e.degraded = []string{"model"}
	}
	return e
}

// Input is everything a prediction needs.
type Input struct {
	Snapshot domain.FeatureSnapshot
	Rules    rules.Result
	Regime   domain.MarketRegime
}

// Predict produces a prediction for one symbol.
func (e *Engine) Predict(ctx context.Context, in Input) (domain.Prediction, error) {
	start := time.Now()
	snap := in.Snapshot
	now := e.cfg.Clock.Now()

	prior := RulePrior(in.Rules, in.Regime)

	p := domain.Prediction{
		ID:               bus.NewEventID(now),
		Ticker:           snap.Ticker,
		CreatedAt:        now,
		FeatureHash:      snap.Hash,
		FeatureAsOf:      snap.AsOf,
		Regime:           in.Regime.Regime,
		RegimeConfidence: in.Regime.Confidence,
		RulePrior:        prior,
		ModelID:          e.cfg.ModelID,
		Disclaimer:       domain.Disclaimer,
	}

	var base domain.Distribution
	var model *mlinfer.Model
	if e.registry != nil {
		if m, stage, ok := e.registry.Select(e.cfg.ModelID, snap.Ticker); ok {
			model = m
			_ = stage
		}
	}

	switch {
	case model == nil:
		base = prior
		p.Source = domain.SourceRulesFallback
		p.ModelVersion = "rules-fallback"
		p.BlendWeight = 0
		p.Degraded = append(p.Degraded, "model_unavailable")
	default:
		dist, contribs, err := model.Predict(snap)
		if err != nil {
			// A cold or missing feature is an expected condition during warm-up,
			// not an error worth failing the pipeline over. Degrade explicitly.
			base = prior
			p.Source = domain.SourceRulesFallback
			p.ModelVersion = "rules-fallback"
			p.BlendWeight = 0
			p.Degraded = append(p.Degraded, "features_incomplete: "+err.Error())
		} else {
			p.ModelOutput = dist
			p.Contributions = contribs
			p.ModelVersion = model.Artifact().Version
			p.ArtifactSHA = model.Artifact().Digest
			w := e.effectiveBlendWeight(model)
			p.BlendWeight = w
			base = blendDirectional(prior, dist, w, e.directionalWeight(model, w))
			p.Source = domain.SourceBlend
			if w >= 1 {
				p.Source = domain.SourceModel
			}
		}
	}

	// Volatility drives the expected move and therefore how much probability
	// mass the FLAT band should absorb. A 15 bps band is nearly certain to be
	// breached on a high-volatility name over an hour and nearly certain to
	// hold on a quiet one over fifteen minutes; ignoring that would make the
	// three classes mean different things for different symbols.
	atrPct := snap.MustGet(domain.FeatATRPct, 0)
	realized := snap.MustGet(domain.FeatRealizedVol, 0)

	for _, h := range e.cfg.Horizons {
		d := scaleToHorizon(base, h, e.cfg.Horizons[0])
		band := e.cfg.FlatBandBps
		expected := expectedMoveBps(atrPct, realized, h)
		d = rebandDistribution(d, band, expected)
		if p.Source == domain.SourceRulesFallback {
			d = capConfidence(d, e.cfg.FallbackCeiling)
		}
		p.Scenarios = append(p.Scenarios, domain.ScenarioPrediction{
			Horizon:         h,
			Dist:            d,
			FlatBandBps:     band,
			ExpectedMoveBps: math.Round(expected*10) / 10,
			Confidence:      round4(d.Confidence()),
			Uncertainty:     round4(d.Uncertainty()),
		})
	}

	e.mu.Lock()
	e.latest[snap.Ticker] = p
	e.mu.Unlock()

	if e.cfg.Metrics != nil {
		e.cfg.Metrics.PredictionLatency.Observe(time.Since(start).Seconds(), e.cfg.ModelID)
		e.cfg.Metrics.PredictionsTotal.Inc(e.cfg.ModelID, string(p.Source))
	}
	return p, nil
}

// RulePrior converts a deterministic rule score into a distribution.
//
// The mapping is intentionally conservative. A rule engine that fires every
// bullish rule it has is evidence, not certainty: the maximum probability this
// function can produce for a direction is 0.72, and the regime's own confidence
// scales it further. Anything more would be dressing a heuristic up as a model.
func RulePrior(r rules.Result, regime domain.MarketRegime) domain.Distribution {
	// score in [-100, 100] -> tilt in [-1, 1], with a soft knee so that small
	// scores stay close to uninformative.
	tilt := math.Tanh(r.Score / 45)
	// Coverage: how much of the strategy's eligible weight actually fired.
	coverage := domain.Clamp01(r.Confidence)
	strength := 0.55 * tilt * (0.4 + 0.6*coverage)

	// A low-confidence regime classification should not amplify rule evidence,
	// because the rules were selected *by* the regime.
	if regime.Confidence > 0 {
		strength *= 0.5 + 0.5*domain.Clamp01(regime.Confidence)
	}

	up := 1.0 / 3
	down := 1.0 / 3
	flat := 1.0 / 3
	up += strength * 0.5
	down -= strength * 0.5
	// Counter-evidence and low coverage widen the flat band.
	flatBoost := 0.15 * (1 - coverage)
	if len(r.CounterFired) > 0 {
		flatBoost += 0.05 * float64(len(r.CounterFired))
	}
	flat += flatBoost
	up -= flatBoost / 2
	down -= flatBoost / 2
	return domain.NewDistribution(math.Max(up, 0.01), math.Max(flat, 0.01), math.Max(down, 0.01))
}

// scaleToHorizon adjusts a distribution produced for the base horizon to a
// longer or shorter one.
//
// Longer horizons carry more uncertainty from the same evidence, so the
// distribution flattens toward uniform with the square root of the time ratio —
// the same scaling that governs diffusion. This is an approximation and is
// documented as one: a per-horizon model, when trained, replaces it entirely.
func scaleToHorizon(d domain.Distribution, h, base domain.Horizon) domain.Distribution {
	if h == base {
		return d
	}
	ratio := h.Duration().Seconds() / base.Duration().Seconds()
	if ratio <= 0 {
		return d
	}
	// t > 1 flattens, t < 1 sharpens; sqrt keeps the effect gentle.
	t := math.Sqrt(ratio)
	return d.Sharpen(t)
}

// expectedMoveBps estimates the magnitude of the move over a horizon from ATR
// and realised volatility, in basis points.
func expectedMoveBps(atrPct, annualVol float64, h domain.Horizon) float64 {
	// Convert annualised volatility to the horizon.
	years := h.Duration().Seconds() / (252 * 6.5 * 3600)
	fromVol := annualVol * math.Sqrt(math.Max(years, 0)) * 10000
	// ATR is a per-bar range; scale it by the number of one-minute bars.
	bars := h.Duration().Minutes()
	fromATR := atrPct * math.Sqrt(math.Max(bars, 1)) * 10000 * 0.6
	switch {
	case fromVol > 0 && fromATR > 0:
		return (fromVol + fromATR) / 2
	case fromVol > 0:
		return fromVol
	default:
		return fromATR
	}
}

// rebandDistribution moves probability mass between the directional classes and
// FLAT so the distribution is consistent with the flat band actually in force.
//
// If the expected move is small relative to the band, most outcomes land inside
// it and FLAT should dominate; if the expected move dwarfs the band, FLAT
// should be small. Without this, a 15 bps band would mean something different
// for every symbol and horizon, and calibration would be meaningless.
func rebandDistribution(d domain.Distribution, bandBps, expectedBps float64) domain.Distribution {
	if bandBps <= 0 || expectedBps <= 0 {
		return d
	}
	// P(|move| < band) under a normal with sigma = expected move.
	sigma := expectedBps
	pFlat := erf(bandBps / (sigma * math.Sqrt2))
	pFlat = domain.Clamp(pFlat, 0.03, 0.90)

	// Preserve the directional *ratio* from the model, and give FLAT the mass
	// the band geometry implies.
	dirMass := 1 - pFlat
	upShare := 0.5
	if d.Up+d.Down > 0 {
		upShare = d.Up / (d.Up + d.Down)
	}
	// Blend the geometric FLAT with the model's own FLAT so a model that has
	// genuinely learned "no move" is not overridden entirely.
	flat := 0.6*pFlat + 0.4*d.Flat
	dirMass = 1 - flat
	return domain.NewDistribution(dirMass*upShare, flat, dirMass*(1-upShare))
}

// capConfidence flattens a distribution until its confidence is at most max.
func capConfidence(d domain.Distribution, max float64) domain.Distribution {
	if max <= 0 || max >= 1 {
		return d
	}
	for i := 0; i < 24 && d.Confidence() > max; i++ {
		d = d.Blend(domain.NewDistribution(1, 1, 1), 0.15)
	}
	return d
}

// erf is Abramowitz & Stegun 7.1.26, accurate to ~1.5e-7, which is far beyond
// what a probability displayed to two decimals requires.
func erf(x float64) float64 {
	sign := 1.0
	if x < 0 {
		sign, x = -1, -x
	}
	const (
		a1 = 0.254829592
		a2 = -0.284496736
		a3 = 1.421413741
		a4 = -1.453152027
		a5 = 1.061405429
		p  = 0.3275911
	)
	t := 1 / (1 + p*x)
	y := 1 - (((((a5*t+a4)*t)+a3)*t+a2)*t+a1)*t*math.Exp(-x*x)
	return sign * y
}

// Latest returns the most recent prediction for a symbol.
func (e *Engine) Latest(t domain.Ticker) (domain.Prediction, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	p, ok := e.latest[t]
	return p, ok
}

// Degraded reports which subsystems are unavailable.
func (e *Engine) Degraded() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]string(nil), e.degraded...)
}

// Describe renders a human-readable, non-committal summary of a prediction.
// It is used by the deterministic explanation fallback, so the platform can
// always explain itself without an LLM.
func Describe(p domain.Prediction) string {
	s := p.Primary()
	out, _ := s.Dist.Argmax()
	return fmt.Sprintf(
		"Over the next %s the model assigns %.0f%% to a move above +%.0f bps, %.0f%% to staying within ±%.0f bps, and %.0f%% to a move below −%.0f bps (most likely: %s; confidence %.2f, model %s).",
		s.Horizon, s.Dist.Up*100, s.FlatBandBps, s.Dist.Flat*100, s.FlatBandBps,
		s.Dist.Down*100, s.FlatBandBps, out, s.Confidence, p.ModelVersion)
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

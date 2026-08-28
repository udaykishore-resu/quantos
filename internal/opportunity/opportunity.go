// Package opportunity fuses the composite score, the prediction and the
// environment into a single ranked measure.
//
// The output is deliberately decomposable: Components carries the additive
// contribution of every input, so "why is this 84?" has an arithmetic answer
// rather than a narrative one.
package opportunity

import (
	"math"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Input is everything the engine consumes.
type Input struct {
	Stock      domain.Stock
	Score      domain.StockScore
	Prediction domain.Prediction
	Risk       domain.RiskAssessment
	Regime     domain.MarketRegime
	Snapshot   domain.FeatureSnapshot
	Strategy   domain.Strategy
	Now        time.Time
}

// Evaluate produces the opportunity score.
func Evaluate(in Input) domain.OpportunityScore {
	scenario := pickScenario(in.Prediction, in.Strategy.Policy.Horizon)
	edge := scenario.Dist.DirectionalEdge()

	bias := domain.SideFlat
	switch {
	case edge > 0.02:
		bias = domain.SideLong
	case edge < -0.02:
		bias = domain.SideShort
	}

	atr := in.Snapshot.MustGet(domain.FeatATR14, 0)
	price := in.Snapshot.MustGet(domain.FeatLast, in.Snapshot.Last)
	stopMult := orDefault(in.Strategy.Policy.StopATRMultiple, 1.5)
	targetMult := orDefault(in.Strategy.Policy.TargetATRMultiple, 2.5)

	stopBps, targetBps := 0.0, 0.0
	if price > 0 && atr > 0 {
		stopBps = atr * stopMult / price * 10000
		targetBps = atr * targetMult / price * 10000
	}
	rr := 0.0
	if stopBps > 0 {
		rr = targetBps / stopBps
	}

	// Components. Each is on a 0..100 scale before weighting, and the weights
	// sum to 1 so the result is directly interpretable.
	comp := map[string]float64{}
	comp["stock_score"] = in.Score.FinalScore
	comp["directional_edge"] = domain.Clamp01((math.Abs(edge)-0.02)/0.35) * 100
	comp["model_confidence"] = domain.Clamp01(scenario.Confidence) * 100
	comp["regime_alignment"] = regimeAlignment(in.Regime, bias) * 100
	comp["liquidity"] = in.Stock.LiquidityScore() * 100
	comp["risk_headroom"] = math.Max(0, 100-in.Risk.Score)
	comp["risk_reward"] = domain.Clamp01((rr-0.8)/2.2) * 100
	comp["volatility_fit"] = volatilityFit(in.Snapshot) * 100
	comp["event_headroom"] = in.Score.EventRiskScore

	weights := map[string]float64{
		"stock_score":      0.26,
		"directional_edge": 0.18,
		"model_confidence": 0.14,
		"regime_alignment": 0.12,
		"liquidity":        0.08,
		"risk_headroom":    0.08,
		"risk_reward":      0.08,
		"volatility_fit":   0.03,
		"event_headroom":   0.03,
	}

	total := 0.0
	contributions := map[string]float64{}
	for k, v := range comp {
		c := v * weights[k]
		contributions[k] = round1(c)
		total += c
	}

	// A blocked or watch-only assessment caps the opportunity, so the ranking a
	// user sees can never promote something the risk engine refused.
	switch in.Risk.Decision {
	case domain.RiskBlock:
		total = math.Min(total, 25)
	case domain.RiskWatchOnly:
		total = math.Min(total, 60)
	}

	return domain.OpportunityScore{
		Ticker:         in.Stock.Ticker,
		CreatedAt:      in.Now,
		Score:          round1(domain.Clamp(total, 0, 100)),
		Bias:           bias,
		Confidence:     round2(scenario.Confidence),
		Risk:           in.Risk.Level,
		Components:     contributions,
		RiskReward:     round2(rr),
		ExpectedMove:   round1(scenario.ExpectedMoveBps),
		StopDistance:   round1(stopBps),
		TargetDistance: round1(targetBps),
		PredictionID:   in.Prediction.ID,
		Regime:         in.Regime.Regime,
		Disclaimer:     domain.Disclaimer,
	}
}

func pickScenario(p domain.Prediction, h domain.Horizon) domain.ScenarioPrediction {
	if s, ok := p.Scenario(h); ok {
		return s
	}
	return p.Primary()
}

func regimeAlignment(r domain.MarketRegime, bias domain.Side) float64 {
	if r.Regime == domain.RegimeUndefined || bias == domain.SideFlat {
		return 0.5
	}
	aligned := (bias == domain.SideLong && r.Regime.Bullish()) ||
		(bias == domain.SideShort && r.Regime.Bearish())
	base := 0.35
	if aligned {
		base = 0.85
	}
	// Weak regime confidence pulls the alignment term toward neutral, because a
	// low-confidence classification should not strongly reward or punish.
	return 0.5 + (base-0.5)*domain.Clamp01(r.Confidence)
}

// volatilityFit rewards volatility in the band where the models were calibrated
// and penalises both extremes: too little movement leaves no opportunity, too
// much leaves no reliability.
func volatilityFit(snap domain.FeatureSnapshot) float64 {
	v, ok := snap.Get(domain.FeatRealizedVol)
	if !ok {
		return 0.5
	}
	const lo, ideal, hi = 0.08, 0.25, 0.70
	switch {
	case v <= lo || v >= hi:
		return 0.1
	case v < ideal:
		return 0.1 + 0.9*(v-lo)/(ideal-lo)
	default:
		return 1 - 0.9*(v-ideal)/(hi-ideal)
	}
}

func orDefault(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func round1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

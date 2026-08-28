// Package scoring produces the composite research score for an instrument.
//
// The score is a weighted blend of ten sub-scores, with the weights supplied by
// the active strategy so that no single investment philosophy is hard-coded
// (requirement §7). Two behaviours matter as much as the arithmetic:
//
//   - Unavailable components are excluded and the remaining weights are
//     renormalised. Treating a missing fundamental as a zero would rank every
//     ETF as garbage and every early-stage company as a fraud.
//   - Every sub-score records the evidence that produced it, so the composite
//     is decomposable rather than a black box.
package scoring

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/fundamentals"
	"github.com/udaykishoreresu/quantos/internal/rules"
)

// Input is everything the scorer consumes.
type Input struct {
	Stock       domain.Stock
	Snapshot    domain.FeatureSnapshot
	Rules       rules.Result
	Regime      domain.MarketRegime
	Risk        *domain.RiskAssessment
	Fundamental *fundamentals.Scores
	Valuation   *domain.ValuationMetrics
	Sector      *domain.SectorPerformance
	Events      []domain.CorporateEvent
	Strategy    domain.Strategy
	Now         time.Time
}

// Score computes the composite research score.
func Score(in Input) domain.StockScore {
	s := domain.StockScore{
		Ticker:     in.Stock.Ticker,
		CreatedAt:  in.Now,
		StrategyID: in.Strategy.ID,
		ConfigHash: in.Strategy.Hash,
		Weights:    in.Strategy.Weights.Map(),
		Disclaimer: domain.Disclaimer,
	}

	available := map[string]float64{}
	var evidence []string

	if in.Fundamental != nil {
		s.FundamentalScore = round1(in.Fundamental.Fundamental)
		s.GrowthScore = round1(in.Fundamental.Growth)
		s.QualityScore = round1(in.Fundamental.Quality)
		available["fundamental"] = s.FundamentalScore
		if in.Fundamental.Growth >= 0 {
			available["growth"] = s.GrowthScore
		}
		if in.Fundamental.Quality >= 0 {
			available["quality"] = s.QualityScore
		}
		evidence = append(evidence, in.Fundamental.Evidence...)
		s.Missing = append(s.Missing, in.Fundamental.Missing...)
	} else {
		s.Missing = append(s.Missing, "fundamental", "growth", "quality")
	}

	if in.Valuation != nil && in.Valuation.Score > 0 {
		s.ValuationScore = round1(in.Valuation.Score)
		available["valuation"] = s.ValuationScore
		if in.Valuation.Explanation != "" {
			evidence = append(evidence, in.Valuation.Explanation)
		}
	} else {
		s.Missing = append(s.Missing, "valuation")
	}

	tech, techEv, techOK := technicalScore(in.Snapshot, in.Rules)
	if techOK {
		s.TechnicalScore = round1(tech)
		available["technical"] = s.TechnicalScore
		evidence = append(evidence, techEv...)
	} else {
		s.Missing = append(s.Missing, "technical")
	}

	mom, momEv, momOK := momentumScore(in.Snapshot)
	if momOK {
		s.MomentumScore = round1(mom)
		available["momentum"] = s.MomentumScore
		evidence = append(evidence, momEv...)
	} else {
		s.Missing = append(s.Missing, "momentum")
	}

	if in.Sector != nil {
		s.SectorScore = round1(sectorScore(*in.Sector))
		available["sector"] = s.SectorScore
		evidence = append(evidence, fmt.Sprintf("%s sector ranks %d on relative strength",
			in.Sector.Sector, in.Sector.Rank))
	} else {
		s.Missing = append(s.Missing, "sector")
	}

	rs, rsEv := regimeScore(in.Regime, in.Rules.Side, in.Strategy)
	s.MarketRegimeScore = round1(rs)
	available["market_regime"] = s.MarketRegimeScore
	evidence = append(evidence, rsEv)

	if in.Risk != nil {
		// Risk sub-score is inverted: higher means safer, so that every
		// component points the same way and the weighted sum is meaningful.
		s.RiskScore = round1(100 - in.Risk.Score)
		available["risk"] = s.RiskScore
		if len(in.Risk.Warnings) > 0 {
			evidence = append(evidence, "risk warnings: "+joinFirst(in.Risk.Warnings, 2))
		}
	} else {
		s.Missing = append(s.Missing, "risk")
	}

	ev, evEvidence := eventRiskScore(in.Events, in.Now)
	s.EventRiskScore = round1(ev)
	available["event_risk"] = s.EventRiskScore
	if evEvidence != "" {
		evidence = append(evidence, evEvidence)
	}

	// Weighted blend over available components only.
	weights := in.Strategy.Weights.Map()
	sum, wsum := 0.0, 0.0
	for name, v := range available {
		w := weights[name]
		if w <= 0 {
			continue
		}
		sum += v * w
		wsum += w
	}
	if wsum > 0 {
		s.FinalScore = round1(sum / wsum)
	}
	s.Classification = classify(s, in.Risk)
	sort.Strings(s.Missing)
	s.Evidence = dedupe(evidence)
	return s
}

// technicalScore blends trend structure, position relative to VWAP and the
// oscillator into 0..100.
func technicalScore(snap domain.FeatureSnapshot, r rules.Result) (float64, []string, bool) {
	var parts []float64
	var ev []string

	last, okLast := snap.Get(domain.FeatLast)
	sma20, ok20 := snap.Get(domain.FeatSMA20)
	sma50, ok50 := snap.Get(domain.FeatSMA50)
	if okLast && ok20 && ok50 && sma50 > 0 {
		structure := 0.0
		if last > sma20 {
			structure += 0.4
		}
		if last > sma50 {
			structure += 0.3
		}
		if sma20 > sma50 {
			structure += 0.3
		}
		parts = append(parts, structure*100)
		ev = append(ev, fmt.Sprintf("price %.2f against 20-period %.2f and 50-period %.2f", last, sma20, sma50))
	}
	if adx, ok := snap.Get(domain.FeatADX14); ok {
		parts = append(parts, domain.Clamp01((adx-10)/30)*100)
		ev = append(ev, fmt.Sprintf("ADX at %.1f", adx))
	}
	if vd, ok := snap.Get(domain.FeatVWAPDist); ok {
		parts = append(parts, domain.Clamp01((vd+40)/80)*100)
		ev = append(ev, fmt.Sprintf("%.0f bps from session VWAP", vd))
	}
	if pb, ok := snap.Get(domain.FeatBBPercentB); ok {
		// Mid-band is healthiest; both extremes are penalised.
		parts = append(parts, (1-math.Abs(pb-0.5)*2)*100)
	}
	if len(parts) == 0 {
		return 0, nil, false
	}
	base := mean(parts)
	// The rule engine's own directional score contributes, because it encodes
	// the strategy's view of what "good technicals" means.
	base = 0.7*base + 0.3*(50+r.Score/2)
	return domain.Clamp(base, 0, 100), ev, true
}

func momentumScore(snap domain.FeatureSnapshot) (float64, []string, bool) {
	var parts []float64
	var ev []string
	if m10, ok := snap.Get(domain.FeatMomentum10); ok {
		parts = append(parts, domain.Clamp01((m10+0.02)/0.04)*100)
		ev = append(ev, fmt.Sprintf("10-period momentum %.2f%%", m10*100))
	}
	if m60, ok := snap.Get(domain.FeatMomentum60); ok {
		parts = append(parts, domain.Clamp01((m60+0.05)/0.10)*100)
		ev = append(ev, fmt.Sprintf("60-period momentum %.2f%%", m60*100))
	}
	if rs, ok := snap.Get(domain.FeatRelStrength); ok {
		parts = append(parts, domain.Clamp01((rs+0.02)/0.04)*100)
		ev = append(ev, fmt.Sprintf("relative strength %.2f%% against the benchmark", rs*100))
	}
	if mh, ok := snap.Get(domain.FeatMACDHist); ok {
		parts = append(parts, domain.Clamp01((mh+20)/40)*100)
	}
	if len(parts) == 0 {
		return 0, nil, false
	}
	return domain.Clamp(mean(parts), 0, 100), ev, true
}

func sectorScore(p domain.SectorPerformance) float64 {
	// Rank 1 of 11 maps to 100, last to 0, with relative strength as a tiebreak.
	rank := float64(p.Rank)
	if rank <= 0 {
		rank = 6
	}
	base := domain.Clamp01((11 - rank) / 10)
	return domain.Clamp((base*0.7+domain.Clamp01(p.RelStrength*20+0.5)*0.3)*100, 0, 100)
}

func regimeScore(r domain.MarketRegime, side domain.Side, s domain.Strategy) (float64, string) {
	if r.Regime == domain.RegimeUndefined {
		return 50, "market regime is not yet established; the regime component is neutral"
	}
	aligned := false
	switch side {
	case domain.SideLong:
		aligned = r.Regime.Bullish()
	case domain.SideShort:
		aligned = r.Regime.Bearish()
	}
	// A strategy that declares the regime is scored higher for being in it.
	declared := len(s.Regimes) == 0
	for _, x := range s.Regimes {
		if x == r.Regime {
			declared = true
		}
	}
	base := 50.0
	if aligned {
		base += 25
	} else if side != domain.SideFlat {
		base -= 20
	}
	if declared {
		base += 15
	} else {
		base -= 15
	}
	base = base*(0.6+0.4*domain.Clamp01(r.Confidence)) + 10*domain.Clamp01(r.Confidence)
	return domain.Clamp(base, 0, 100),
		fmt.Sprintf("regime %s at confidence %.2f, %s the %s setup",
			r.Regime, r.Confidence, alignmentWord(aligned), side)
}

func alignmentWord(aligned bool) string {
	if aligned {
		return "supporting"
	}
	return "not supporting"
}

// eventRiskScore is inverted like the risk score: 100 means no known event risk.
func eventRiskScore(events []domain.CorporateEvent, now time.Time) (float64, string) {
	if len(events) == 0 {
		return 100, ""
	}
	worst := 100.0
	note := ""
	for _, e := range events {
		if e.ScheduledAt.Before(now) {
			continue
		}
		hrs := e.ScheduledAt.Sub(now).Hours()
		if hrs > 168 {
			continue
		}
		severity := 1.0
		switch e.Type {
		case domain.EventEarnings, domain.EventMerger:
			severity = 1.0
		case domain.EventGuidance, domain.EventOffering:
			severity = 0.7
		default:
			severity = 0.3
		}
		// Closer events are worse; 168 hours out is nearly harmless.
		s := 100 - severity*100*domain.Clamp01((168-hrs)/168)
		if s < worst {
			worst = s
			note = fmt.Sprintf("%s scheduled in %.0f hours", string(e.Type), hrs)
		}
	}
	return domain.Clamp(worst, 0, 100), note
}

// classify assigns the research label. The risk engine's verdict dominates:
// a blocked instrument is never labelled a strong watch, whatever its score.
func classify(s domain.StockScore, r *domain.RiskAssessment) domain.Classification {
	if r != nil {
		switch r.Decision {
		case domain.RiskBlock:
			if r.Level == domain.RiskExtreme {
				return domain.ClassAvoid
			}
			return domain.ClassHighRisk
		case domain.RiskWatchOnly:
			if s.FinalScore >= 70 {
				return domain.ClassBullishSetup
			}
			return domain.ClassNeutral
		}
	}
	switch {
	case s.FinalScore >= 80:
		return domain.ClassStrongWatch
	case s.FinalScore >= 65:
		return domain.ClassBullishSetup
	case s.FinalScore >= 40:
		return domain.ClassNeutral
	case s.FinalScore >= 25:
		return domain.ClassHighRisk
	default:
		return domain.ClassAvoid
	}
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func round1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}

func joinFirst(ss []string, n int) string {
	if len(ss) > n {
		ss = ss[:n]
	}
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0]
	for _, s := range ss {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

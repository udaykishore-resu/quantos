package analyst

import (
	"fmt"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Template renders a deterministic explanation from the structured input.
//
// This is not a degraded mode in any meaningful sense. It is the platform's
// baseline explanation: complete, correct, grounded by construction, and
// available when no language model is configured or reachable. The LLM path
// exists to make this text read more naturally, not to say anything this text
// cannot.
func Template(in Input, now time.Time) Report {
	r := Report{
		Ticker:      in.Stock.Ticker,
		GeneratedAt: now,
		Source:      SourceTemplate,
		Disclaimer:  domain.Disclaimer,
	}

	var b strings.Builder
	name := in.Stock.Company
	if name == "" {
		name = string(in.Stock.Ticker)
	}
	b.WriteString(fmt.Sprintf("%s (%s, %s sector). ", name, in.Stock.Ticker, in.Stock.Sector))

	if in.Regime.Regime != "" && in.Regime.Regime != domain.RegimeUndefined {
		b.WriteString(fmt.Sprintf("The market regime is classified as %s with confidence %.2f. ",
			in.Regime.Regime, in.Regime.Confidence))
	}
	if s := in.Score; s != nil {
		b.WriteString(fmt.Sprintf("The composite research score is %.1f out of 100, classified %s. ",
			s.FinalScore, s.Classification))
		if len(s.Missing) > 0 {
			b.WriteString(fmt.Sprintf("These components were unavailable and excluded from the weighting: %s. ",
				strings.Join(s.Missing, ", ")))
		}
	}
	if p := in.Prediction; p != nil {
		sc := p.Primary()
		b.WriteString(fmt.Sprintf(
			"Over a %s horizon the model distributes probability as %.0f%% above +%.0f bps, %.0f%% inside ±%.0f bps, and %.0f%% below −%.0f bps. ",
			sc.Horizon, sc.Dist.Up*100, sc.FlatBandBps, sc.Dist.Flat*100, sc.FlatBandBps, sc.Dist.Down*100, sc.FlatBandBps))
		if p.Source == domain.SourceRulesFallback {
			b.WriteString("The trained model was unavailable, so this distribution comes from the deterministic rule prior with a capped confidence. ")
		}
	}
	if o := in.Opportunity; o != nil {
		b.WriteString(fmt.Sprintf("The opportunity score is %.1f with a %s bias and a risk/reward of %.2f to 1. ",
			o.Score, o.Bias, o.RiskReward))
	}
	if rk := in.Risk; rk != nil {
		switch rk.Decision {
		case domain.RiskBlock:
			b.WriteString("The risk engine blocked this instrument, so no paper signal was created. ")
		case domain.RiskWatchOnly:
			b.WriteString("The risk engine placed this instrument on watch only; no paper signal was created. ")
		default:
			b.WriteString("The risk engine allowed a paper signal. ")
		}
	}
	if sg := in.Signal; sg != nil {
		b.WriteString(fmt.Sprintf(
			"A %s paper signal was recorded at a reference price of %.4f, with a stop reference of %.4f and a target reference of %.4f. ",
			sg.Side, sg.EntryReference, sg.StopReference, sg.TargetReference))
	}
	r.Summary = strings.TrimSpace(b.String())

	// Supporting evidence.
	r.SupportingEvidence = append(r.SupportingEvidence, in.Evidence...)
	if in.Score != nil {
		r.SupportingEvidence = append(r.SupportingEvidence, in.Score.Evidence...)
	}
	if p := in.Prediction; p != nil {
		for _, c := range p.TopContributions(3) {
			dir := "toward an upward outcome"
			if c.Contribution < 0 {
				dir = "toward a downward outcome"
			}
			r.SupportingEvidence = append(r.SupportingEvidence,
				fmt.Sprintf("feature %s contributes %.4f %s", c.Feature, c.Contribution, dir))
		}
	}
	if len(r.SupportingEvidence) == 0 {
		r.SupportingEvidence = []string{"no supporting conditions fired"}
	}

	// Counter-evidence.
	r.CounterEvidence = append(r.CounterEvidence, in.CounterEvidence...)
	if rk := in.Risk; rk != nil {
		r.CounterEvidence = append(r.CounterEvidence, rk.Warnings...)
	}
	if len(r.CounterEvidence) == 0 {
		r.CounterEvidence = []string{"no counter-conditions fired; absence of counter-evidence is not confirmation"}
	}

	// Risks.
	if rk := in.Risk; rk != nil {
		r.Risks = append(r.Risks, rk.Blockers...)
		for _, c := range rk.Checks {
			if c.Status == domain.CheckSkip {
				r.Risks = append(r.Risks, fmt.Sprintf("%s could not be evaluated: %s", c.Name, c.Skipped))
			}
		}
	}
	for _, e := range in.Events {
		r.Risks = append(r.Risks, fmt.Sprintf("%s scheduled for %s",
			e.Type, e.ScheduledAt.UTC().Format("2006-01-02 15:04Z")))
	}
	if in.Snapshot != nil && in.Snapshot.Stale {
		r.Risks = append(r.Risks, "market data is stale: "+in.Snapshot.StaleReason)
	}
	if o := in.Options; o != nil && o.DataQuality != "good" {
		r.Risks = append(r.Risks, "options data quality is "+o.DataQuality+"; surface metrics are indicative only")
	}
	if len(r.Risks) == 0 {
		r.Risks = []string{"no blocking risk conditions were identified at evaluation time"}
	}

	// Uncertainty.
	if p := in.Prediction; p != nil {
		sc := p.Primary()
		r.Uncertainty = fmt.Sprintf(
			"Normalised uncertainty for the %s horizon is %.2f, giving a confidence of %.2f. The distribution was produced by model %s from feature snapshot %s. These are probabilities conditioned on the observed state, not statements about what will happen.",
			sc.Horizon, sc.Uncertainty, sc.Confidence, p.ModelVersion, shortHash(p.FeatureHash))
	} else {
		r.Uncertainty = "No prediction was available for this instrument at evaluation time."
	}

	// Scenarios: describe each horizon in words, without asserting any of them.
	if p := in.Prediction; p != nil {
		for _, sc := range p.Scenarios {
			out, prob := sc.Dist.Argmax()
			r.Scenarios = append(r.Scenarios, fmt.Sprintf(
				"%s horizon: the most probable single outcome is %s at %.0f%%, against %.0f%% and %.0f%% for the alternatives.",
				sc.Horizon, out, prob*100,
				otherTwo(sc.Dist, out)[0]*100, otherTwo(sc.Dist, out)[1]*100))
		}
	}
	if sg := in.Signal; sg != nil && len(sg.Invalidations) > 0 {
		var inv []string
		for _, c := range sg.Invalidations {
			inv = append(inv, c.Description)
		}
		r.Scenarios = append(r.Scenarios, "The signal is invalidated if: "+strings.Join(inv, "; ")+".")
	}
	return r
}

func otherTwo(d domain.Distribution, best domain.Outcome) [2]float64 {
	var out [2]float64
	i := 0
	for _, o := range domain.AllOutcomes {
		if o == best {
			continue
		}
		if i < 2 {
			out[i] = d.P(o)
			i++
		}
	}
	return out
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

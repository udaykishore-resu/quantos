// Package brief assembles the daily market brief and the intraday summary.
//
// The brief is composed from structured artifacts only. Its prose sections are
// produced by the analyst package, which means they inherit the grounding
// validator and the language guard: the brief cannot contain a number that is
// not in the record, and cannot tell anyone to buy anything.
package brief

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/analyst"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/evaluation"
)

// Opportunity is one ranked entry in the brief.
type Opportunity struct {
	Ticker         domain.Ticker         `json:"ticker"`
	Company        string                `json:"company"`
	Sector         string                `json:"sector"`
	Score          float64               `json:"score"`
	Classification domain.Classification `json:"classification"`
	Bias           domain.Side           `json:"bias"`
	Probability    domain.Distribution   `json:"probability"`
	Horizon        domain.Horizon        `json:"horizon"`
	Confidence     float64               `json:"confidence"`
	Risk           domain.RiskLevel      `json:"risk"`
	RiskDecision   domain.RiskDecision   `json:"risk_decision"`
	Reasons        []string              `json:"reasons"`
	CounterSignals []string              `json:"counter_signals"`
	Events         []string              `json:"important_events,omitempty"`
}

// MarketOverview is the top section of the brief.
type MarketOverview struct {
	AsOf        time.Time                  `json:"as_of"`
	Regime      domain.MarketRegime        `json:"regime"`
	Benchmarks  map[domain.Ticker]float64  `json:"benchmarks"`
	VIX         float64                    `json:"vix"`
	Breadth     float64                    `json:"breadth"`
	Sectors     []domain.SectorPerformance `json:"sector_rotation"`
	Rotation    string                     `json:"rotation"`
	Divergences []domain.Divergence        `json:"divergences,omitempty"`
	Notes       []string                   `json:"notes,omitempty"`
}

// Brief is the complete daily report.
type Brief struct {
	ID          string    `json:"id"`
	GeneratedAt time.Time `json:"generated_at"`
	TradingDay  string    `json:"trading_day"`
	Title       string    `json:"title"`

	Market            MarketOverview             `json:"market_overview"`
	MacroEvents       []domain.NewsEvent         `json:"major_macro_events"`
	SectorLeaders     []domain.SectorPerformance `json:"sector_leaders"`
	TopOpportunities  []Opportunity              `json:"top_opportunities"`
	HighRisk          []Opportunity              `json:"high_risk"`
	Earnings          []domain.CorporateEvent    `json:"important_earnings"`
	PredictionSummary PredictionSummary          `json:"prediction_summary"`
	ModelConfidence   ModelConfidence            `json:"model_confidence"`
	RiskWarnings      []string                   `json:"risk_warnings"`

	Narrative       string `json:"narrative"`
	NarrativeSource string `json:"narrative_source"`
	Disclaimer      string `json:"disclaimer"`
}

// PredictionSummary aggregates yesterday's forecasting record.
type PredictionSummary struct {
	Resolved       int                 `json:"resolved"`
	Accuracy       float64             `json:"accuracy"`
	BaseRate       float64             `json:"base_rate"`
	BrierScore     float64             `json:"brier_score"`
	Calibration    float64             `json:"calibration"`
	BestRegime     string              `json:"best_regime,omitempty"`
	WorstRegime    string              `json:"worst_regime,omitempty"`
	VetoCost       evaluation.VetoCost `json:"veto_cost"`
	Interpretation string              `json:"interpretation"`
}

// ModelConfidence describes the serving model's state.
type ModelConfidence struct {
	ModelVersion string               `json:"model_version"`
	Stage        domain.ModelStage    `json:"stage"`
	Healthy      bool                 `json:"healthy"`
	Drift        domain.DriftSeverity `json:"drift"`
	Issues       []string             `json:"issues,omitempty"`
	Fallback     bool                 `json:"serving_from_fallback"`
}

// Input is everything the generator consumes.
type Input struct {
	Now           time.Time
	Regime        domain.MarketRegime
	Relationship  domain.RelationshipState
	Snapshots     map[domain.Ticker]domain.FeatureSnapshot
	Universe      func(domain.Ticker) (domain.Stock, bool)
	Scores        map[domain.Ticker]domain.StockScore
	Predictions   map[domain.Ticker]domain.Prediction
	Opportunities map[domain.Ticker]domain.OpportunityScore
	Risks         map[domain.Ticker]domain.RiskAssessment
	Evidence      map[domain.Ticker][]string
	Counter       map[domain.Ticker][]string
	Events        map[domain.Ticker][]domain.CorporateEvent
	News          []domain.NewsEvent
	Outcomes      []domain.PredictionOutcome
	Health        domain.ModelHealth
	Drift         domain.DriftReport
	TopN          int
}

// Generator builds briefs.
type Generator struct {
	analyst *analyst.Analyst
}

// New builds a generator. A nil analyst yields template-only narratives.
func New(a *analyst.Analyst) *Generator { return &Generator{analyst: a} }

// Generate assembles the brief.
func (g *Generator) Generate(ctx context.Context, in Input) Brief {
	if in.TopN <= 0 {
		in.TopN = 10
	}
	b := Brief{
		ID:          "brief-" + in.Now.UTC().Format("20060102-1504"),
		GeneratedAt: in.Now,
		TradingDay:  in.Now.UTC().Format("2006-01-02"),
		Title:       "QuantOS Morning Brief — " + in.Now.UTC().Format("2 January 2006"),
		Disclaimer:  domain.Disclaimer,
	}

	// 1. Market overview.
	b.Market = MarketOverview{
		AsOf: in.Now, Regime: in.Regime, Benchmarks: map[domain.Ticker]float64{},
		Breadth: in.Regime.Breadth, Sectors: in.Relationship.Sectors,
		Rotation: in.Relationship.Rotation, Divergences: in.Relationship.Divergences,
		Notes: in.Relationship.Notes,
	}
	for _, t := range []domain.Ticker{"SPY", "QQQ", "IWM", "TLT", "DXY", "GLD"} {
		if s, ok := in.Snapshots[t]; ok {
			b.Market.Benchmarks[t] = s.MustGet(domain.FeatLast, 0)
		}
	}
	if s, ok := in.Snapshots["VIX"]; ok {
		b.Market.VIX = s.MustGet(domain.FeatLast, 0)
	}

	// 2. Macro events: material market-wide news.
	for _, n := range in.News {
		if n.Category == domain.NewsMacroNews && n.Material() {
			b.MacroEvents = append(b.MacroEvents, n)
		}
	}
	sort.SliceStable(b.MacroEvents, func(i, j int) bool {
		return b.MacroEvents[i].MaterialityScore > b.MacroEvents[j].MaterialityScore
	})
	if len(b.MacroEvents) > 5 {
		b.MacroEvents = b.MacroEvents[:5]
	}

	// 3. Sector leaders.
	b.SectorLeaders = in.Relationship.Sectors
	if len(b.SectorLeaders) > 5 {
		b.SectorLeaders = b.SectorLeaders[:5]
	}

	// 4/5. Opportunities and high-risk names.
	all := make([]Opportunity, 0, len(in.Opportunities))
	for t, opp := range in.Opportunities {
		stock := domain.Stock{Ticker: t}
		if in.Universe != nil {
			if s, ok := in.Universe(t); ok {
				stock = s
			}
		}
		o := Opportunity{
			Ticker: t, Company: stock.Company, Sector: stock.Sector,
			Score: opp.Score, Bias: opp.Bias, Confidence: opp.Confidence, Risk: opp.Risk,
			Reasons: trim(in.Evidence[t], 4), CounterSignals: trim(in.Counter[t], 4),
		}
		if sc, ok := in.Scores[t]; ok {
			o.Classification = sc.Classification
		}
		if p, ok := in.Predictions[t]; ok {
			pr := p.Primary()
			o.Probability, o.Horizon = pr.Dist, pr.Horizon
		}
		if r, ok := in.Risks[t]; ok {
			o.RiskDecision = r.Decision
		}
		for _, e := range in.Events[t] {
			if e.ScheduledAt.After(in.Now) && e.ScheduledAt.Before(in.Now.Add(7*24*time.Hour)) {
				o.Events = append(o.Events, fmt.Sprintf("%s on %s", e.Type, e.ScheduledAt.UTC().Format("2006-01-02 15:04Z")))
			}
		}
		all = append(all, o)
	}
	// Deterministic ordering: score descending, then ticker.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score == all[j].Score {
			return all[i].Ticker < all[j].Ticker
		}
		return all[i].Score > all[j].Score
	})
	for _, o := range all {
		if len(b.TopOpportunities) < in.TopN && o.RiskDecision != domain.RiskBlock {
			b.TopOpportunities = append(b.TopOpportunities, o)
		}
	}
	for i := len(all) - 1; i >= 0 && len(b.HighRisk) < 5; i-- {
		o := all[i]
		if o.RiskDecision == domain.RiskBlock || o.Classification == domain.ClassHighRisk || o.Classification == domain.ClassAvoid {
			b.HighRisk = append(b.HighRisk, o)
		}
	}

	// 6. Earnings inside the next week.
	for _, events := range in.Events {
		for _, e := range events {
			if e.Type != domain.EventEarnings {
				continue
			}
			if e.ScheduledAt.After(in.Now) && e.ScheduledAt.Before(in.Now.Add(7*24*time.Hour)) {
				b.Earnings = append(b.Earnings, e)
			}
		}
	}
	sort.SliceStable(b.Earnings, func(i, j int) bool {
		if b.Earnings[i].ScheduledAt.Equal(b.Earnings[j].ScheduledAt) {
			return b.Earnings[i].Ticker < b.Earnings[j].Ticker
		}
		return b.Earnings[i].ScheduledAt.Before(b.Earnings[j].ScheduledAt)
	})
	if len(b.Earnings) > 12 {
		b.Earnings = b.Earnings[:12]
	}

	// 7. Prediction summary.
	b.PredictionSummary = summarise(in.Outcomes)

	// 8. Model confidence.
	b.ModelConfidence = ModelConfidence{
		ModelVersion: in.Health.ModelVersion, Stage: in.Health.Stage,
		Healthy: in.Health.Healthy, Drift: in.Drift.Severity, Issues: in.Health.Issues,
		Fallback: in.Health.ModelVersion == "rules-fallback" || in.Health.ModelVersion == "",
	}

	// 9. Risk warnings, assembled from what the platform actually observed
	//    rather than from a fixed list of platitudes.
	b.RiskWarnings = warnings(in, b)

	// Narrative, produced by the analyst so it inherits the guards.
	b.Narrative, b.NarrativeSource = g.narrate(ctx, in, b)
	return b
}

func summarise(outcomes []domain.PredictionOutcome) PredictionSummary {
	s := PredictionSummary{Resolved: len(outcomes)}
	if len(outcomes) == 0 {
		s.Interpretation = "no predictions resolved in this window, so there is nothing to score"
		return s
	}
	e := evaluation.Evaluate(outcomes, "brief", 10)
	s.Accuracy, s.BaseRate = e.Accuracy, e.BaseRate
	s.BrierScore = e.BrierScore
	s.Calibration = 1 - e.ExpectedCalibrationError
	s.VetoCost = evaluation.MeasureVetoCost(outcomes)

	best, worst := "", ""
	bestAcc, worstAcc := -1.0, 2.0
	for rg, ev := range evaluation.ByRegime(outcomes, 10, 20) {
		if ev.Accuracy > bestAcc {
			bestAcc, best = ev.Accuracy, string(rg)
		}
		if ev.Accuracy < worstAcc {
			worstAcc, worst = ev.Accuracy, string(rg)
		}
	}
	s.BestRegime, s.WorstRegime = best, worst

	switch {
	case e.Accuracy < e.BaseRate:
		s.Interpretation = fmt.Sprintf(
			"accuracy of %.1f%% is below the %.1f%% base rate: over this window the model added nothing over a constant guess",
			e.Accuracy*100, e.BaseRate*100)
	case e.Lift < 0.05:
		s.Interpretation = fmt.Sprintf(
			"accuracy of %.1f%% is only marginally above the %.1f%% base rate",
			e.Accuracy*100, e.BaseRate*100)
	default:
		s.Interpretation = fmt.Sprintf(
			"accuracy of %.1f%% against a %.1f%% base rate, with a Brier score of %.3f",
			e.Accuracy*100, e.BaseRate*100, e.BrierScore)
	}
	return s
}

func warnings(in Input, b Brief) []string {
	var out []string
	if in.Regime.Volatility > 0.7 {
		out = append(out, fmt.Sprintf(
			"volatility is at %.0f%% of the normalised range; position sizing and stop distances derived from calmer conditions will be too tight",
			in.Regime.Volatility*100))
	}
	if in.Regime.Confidence < 0.5 {
		out = append(out, fmt.Sprintf(
			"regime classification confidence is %.2f; regime-conditional rules are correspondingly less reliable today",
			in.Regime.Confidence))
	}
	if in.Drift.Severity == domain.DriftSevere || in.Drift.Severity == domain.DriftModerate {
		out = append(out, "model drift detected: "+in.Drift.Recommendation)
	}
	if b.ModelConfidence.Fallback {
		out = append(out, "the trained model is unavailable; probabilities come from the deterministic rule prior with a capped confidence")
	}
	if len(b.Earnings) > 0 {
		out = append(out, fmt.Sprintf(
			"%d instruments in the universe report earnings within seven days; the risk engine blocks new signals inside the earnings window",
			len(b.Earnings)))
	}
	blocked := 0
	for _, r := range in.Risks {
		if r.Decision == domain.RiskBlock {
			blocked++
		}
	}
	if len(in.Risks) > 0 && float64(blocked)/float64(len(in.Risks)) > 0.5 {
		out = append(out, fmt.Sprintf(
			"the risk engine is blocking %d of %d evaluated instruments; if this persists it usually means a threshold needs review rather than that the market is uniquely dangerous",
			blocked, len(in.Risks)))
	}
	if len(out) == 0 {
		out = append(out, "no platform-level risk conditions were triggered when this brief was generated")
	}
	return out
}

// narrate produces the prose, via the analyst so the guards apply.
func (g *Generator) narrate(ctx context.Context, in Input, b Brief) (string, string) {
	if g.analyst == nil {
		return templateNarrative(b), string(analyst.SourceTemplate)
	}
	// The brief's narrative is generated over the market picture, not over one
	// instrument, so the input carries the regime and the leading opportunity.
	ai := analyst.Input{
		Stock:        domain.Stock{Ticker: "__MARKET__", Company: "Market", Sector: "Index"},
		AsOf:         in.Now,
		Regime:       in.Regime,
		Relationship: &in.Relationship,
		Evidence:     in.Regime.Evidence,
	}
	if len(b.TopOpportunities) > 0 {
		t := b.TopOpportunities[0].Ticker
		if sc, ok := in.Scores[t]; ok {
			ai.Score = &sc
		}
		if p, ok := in.Predictions[t]; ok {
			ai.Prediction = &p
		}
		if o, ok := in.Opportunities[t]; ok {
			ai.Opportunity = &o
		}
	}
	rep := g.analyst.Analyze(ctx, ai)
	if rep.Source == analyst.SourceTemplate {
		return templateNarrative(b), string(analyst.SourceTemplate)
	}
	return rep.Summary, string(rep.Source)
}

func templateNarrative(b Brief) string {
	s := fmt.Sprintf(
		"The market regime is %s at confidence %.2f, with breadth at %.2f and normalised volatility at %.2f. ",
		b.Market.Regime.Regime, b.Market.Regime.Confidence, b.Market.Breadth, b.Market.Regime.Volatility)
	if b.Market.Rotation != "" && b.Market.Rotation != "INDETERMINATE" {
		s += fmt.Sprintf("Sector rotation reads as %s. ", b.Market.Rotation)
	}
	if len(b.SectorLeaders) > 0 {
		s += fmt.Sprintf("The leading sector on relative strength is %s. ", b.SectorLeaders[0].Sector)
	}
	s += fmt.Sprintf("%d instruments cleared the risk engine into the opportunity list and %d are flagged high risk. ",
		len(b.TopOpportunities), len(b.HighRisk))
	s += b.PredictionSummary.Interpretation + ". "
	s += "Every figure here is a measurement of what the platform observed, not a forecast of what will happen."
	return s
}

func trim(ss []string, n int) []string {
	if len(ss) > n {
		return append([]string(nil), ss[:n]...)
	}
	return append([]string(nil), ss...)
}

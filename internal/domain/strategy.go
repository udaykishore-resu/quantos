package domain

import "time"

// Comparator is a rule condition operator.
type Comparator string

const (
	CmpGT        Comparator = "gt"
	CmpGTE       Comparator = "gte"
	CmpLT        Comparator = "lt"
	CmpLTE       Comparator = "lte"
	CmpBetween   Comparator = "between"
	CmpOutside   Comparator = "outside"
	CmpCrossUp   Comparator = "cross_up"
	CmpCrossDown Comparator = "cross_down"
	CmpTrue      Comparator = "is_true"
	CmpFalse     Comparator = "is_false"
)

// Condition is one machine-evaluable predicate over a feature snapshot.
// Either Value/Value2 (a literal) or Other (another feature) is used.
type Condition struct {
	Feature string     `yaml:"feature" json:"feature"`
	Op      Comparator `yaml:"op" json:"op"`
	Value   float64    `yaml:"value" json:"value"`
	Value2  float64    `yaml:"value2,omitempty" json:"value2,omitempty"`
	Other   string     `yaml:"other,omitempty" json:"other,omitempty"`
	// Scale multiplies Other before comparison, so "close > 1.02 * sma_50"
	// is expressible without inventing a new feature.
	Scale float64 `yaml:"scale,omitempty" json:"scale,omitempty"`
	// Optional conditions do not fail the rule when the feature is cold.
	Optional bool   `yaml:"optional,omitempty" json:"optional,omitempty"`
	Describe string `yaml:"describe,omitempty" json:"describe,omitempty"`
}

// Rule is a named conjunction of conditions with a directional weight.
type Rule struct {
	ID          string      `yaml:"id" json:"id"`
	Description string      `yaml:"description" json:"description"`
	Side        Side        `yaml:"side" json:"side"`
	Weight      float64     `yaml:"weight" json:"weight"`
	All         []Condition `yaml:"all,omitempty" json:"all,omitempty"`
	Any         []Condition `yaml:"any,omitempty" json:"any,omitempty"`
	None        []Condition `yaml:"none,omitempty" json:"none,omitempty"`
	// Regimes restricts the rule to specific market regimes. Empty means the
	// rule is regime-agnostic. This is how the platform avoids applying
	// indicators blindly (requirement §9).
	Regimes []Regime `yaml:"regimes,omitempty" json:"regimes,omitempty"`
	// Counter marks a rule as counter-evidence: when it fires it argues
	// against the strategy's thesis and is surfaced in the explanation.
	Counter bool  `yaml:"counter,omitempty" json:"counter,omitempty"`
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// Active reports whether the rule is enabled (default true).
func (r Rule) Active() bool { return r.Enabled == nil || *r.Enabled }

// AppliesTo reports whether the rule is permitted in a regime.
func (r Rule) AppliesTo(rg Regime) bool {
	if len(r.Regimes) == 0 {
		return true
	}
	for _, x := range r.Regimes {
		if x == rg {
			return true
		}
	}
	return false
}

// ScoreWeights parameterise the composite stock score.
type ScoreWeights struct {
	Fundamental  float64 `yaml:"fundamental" json:"fundamental"`
	Growth       float64 `yaml:"growth" json:"growth"`
	Quality      float64 `yaml:"quality" json:"quality"`
	Valuation    float64 `yaml:"valuation" json:"valuation"`
	Technical    float64 `yaml:"technical" json:"technical"`
	Momentum     float64 `yaml:"momentum" json:"momentum"`
	Sector       float64 `yaml:"sector" json:"sector"`
	MarketRegime float64 `yaml:"market_regime" json:"market_regime"`
	Risk         float64 `yaml:"risk" json:"risk"`
	EventRisk    float64 `yaml:"event_risk" json:"event_risk"`
}

// Map converts weights to a name-keyed map for renormalisation.
func (w ScoreWeights) Map() map[string]float64 {
	return map[string]float64{
		"fundamental":   w.Fundamental,
		"growth":        w.Growth,
		"quality":       w.Quality,
		"valuation":     w.Valuation,
		"technical":     w.Technical,
		"momentum":      w.Momentum,
		"sector":        w.Sector,
		"market_regime": w.MarketRegime,
		"risk":          w.Risk,
		"event_risk":    w.EventRisk,
	}
}

// SignalPolicy defines when rule output becomes a signal candidate.
type SignalPolicy struct {
	MinRuleScore       float64 `yaml:"min_rule_score" json:"min_rule_score"`
	MinFinalScore      float64 `yaml:"min_final_score" json:"min_final_score"`
	MinConfidence      float64 `yaml:"min_confidence" json:"min_confidence"`
	MinDirectionalEdge float64 `yaml:"min_directional_edge" json:"min_directional_edge"`
	MinRiskReward      float64 `yaml:"min_risk_reward" json:"min_risk_reward"`
	Horizon            Horizon `yaml:"horizon" json:"horizon"`
	StopATRMultiple    float64 `yaml:"stop_atr_multiple" json:"stop_atr_multiple"`
	TargetATRMultiple  float64 `yaml:"target_atr_multiple" json:"target_atr_multiple"`
	MaxWeight          float64 `yaml:"max_weight" json:"max_weight"`
	CooldownMinutes    int     `yaml:"cooldown_minutes" json:"cooldown_minutes"`
	AllowShort         bool    `yaml:"allow_short" json:"allow_short"`
}

// Strategy is a fully configuration-driven trading research strategy.
// No strategy logic lives in Go source (requirement §37).
type Strategy struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Version     string   `yaml:"version" json:"version"`
	Description string   `yaml:"description" json:"description"`
	Enabled     bool     `yaml:"enabled" json:"enabled"`
	Universe    []string `yaml:"universe,omitempty" json:"universe,omitempty"`
	Interval    Interval `yaml:"interval" json:"interval"`

	// Regimes restricts the whole strategy to specific regimes.
	Regimes []Regime `yaml:"regimes,omitempty" json:"regimes,omitempty"`

	Rules   []Rule       `yaml:"rules" json:"rules"`
	Weights ScoreWeights `yaml:"weights" json:"weights"`
	Policy  SignalPolicy `yaml:"policy" json:"policy"`

	// Invalidation templates are instantiated per signal.
	Invalidation []InvalidationTemplate `yaml:"invalidation" json:"invalidation"`

	// Fundamental thresholds used by the scoring engine, so that "quality"
	// means whatever this strategy says it means (requirement §7).
	Fundamental FundamentalPolicy `yaml:"fundamental,omitempty" json:"fundamental,omitempty"`

	SourcePath string `yaml:"-" json:"source_path,omitempty"`
	Hash       string `yaml:"-" json:"hash,omitempty"`
	// ModifiedAt is the strategy file's modification time, not the time it was
	// read. Provenance has to identify the configuration, and a load timestamp
	// changes on every restart while saying nothing about which rules ran.
	ModifiedAt time.Time `yaml:"-" json:"modified_at,omitempty"`
}

// AppliesTo reports whether the strategy is permitted in a regime. An empty
// Regimes list means the strategy is regime-agnostic.
func (s Strategy) AppliesTo(rg Regime) bool {
	if len(s.Regimes) == 0 {
		return true
	}
	for _, r := range s.Regimes {
		if r == rg {
			return true
		}
	}
	return false
}

// InvalidationTemplate describes an invalidation condition in configuration
// terms; the signal engine resolves it against live values.
type InvalidationTemplate struct {
	Kind InvalidationKind `yaml:"kind" json:"kind"`
	// Ref names what the threshold is relative to: "entry", "vwap", "atr",
	// "confidence", "regime".
	Ref         string  `yaml:"ref,omitempty" json:"ref,omitempty"`
	Multiple    float64 `yaml:"multiple,omitempty" json:"multiple,omitempty"`
	Value       float64 `yaml:"value,omitempty" json:"value,omitempty"`
	Minutes     int     `yaml:"minutes,omitempty" json:"minutes,omitempty"`
	Description string  `yaml:"description" json:"description"`
}

// FundamentalPolicy parameterises the fundamental scoring bands.
type FundamentalPolicy struct {
	MinRevenueGrowth   float64 `yaml:"min_revenue_growth,omitempty" json:"min_revenue_growth,omitempty"`
	MinEPSGrowth       float64 `yaml:"min_eps_growth,omitempty" json:"min_eps_growth,omitempty"`
	MinGrossMargin     float64 `yaml:"min_gross_margin,omitempty" json:"min_gross_margin,omitempty"`
	MinOperatingMargin float64 `yaml:"min_operating_margin,omitempty" json:"min_operating_margin,omitempty"`
	MinROE             float64 `yaml:"min_roe,omitempty" json:"min_roe,omitempty"`
	MinROIC            float64 `yaml:"min_roic,omitempty" json:"min_roic,omitempty"`
	MaxDebtToEquity    float64 `yaml:"max_debt_to_equity,omitempty" json:"max_debt_to_equity,omitempty"`
	MinCurrentRatio    float64 `yaml:"min_current_ratio,omitempty" json:"min_current_ratio,omitempty"`
	MaxPE              float64 `yaml:"max_pe,omitempty" json:"max_pe,omitempty"`
	MaxPEG             float64 `yaml:"max_peg,omitempty" json:"max_peg,omitempty"`
	MaxEVToEBITDA      float64 `yaml:"max_ev_to_ebitda,omitempty" json:"max_ev_to_ebitda,omitempty"`
	PreferLowValuation bool    `yaml:"prefer_low_valuation,omitempty" json:"prefer_low_valuation,omitempty"`
}

package domain

import "time"

// Regime is the coarse classification of market state. A regime is not a
// forecast: it describes the environment that has been observed, and it
// conditions which signals the platform considers meaningful.
type Regime string

const (
	RegimeBullTrend Regime = "BULL_TREND"
	RegimeBearTrend Regime = "BEAR_TREND"
	RegimeRange     Regime = "RANGE"
	RegimeHighVol   Regime = "HIGH_VOLATILITY"
	RegimeLowVol    Regime = "LOW_VOLATILITY"
	RegimeBreakout  Regime = "BREAKOUT"
	RegimeReversal  Regime = "REVERSAL"
	RegimeRiskOn    Regime = "RISK_ON"
	RegimeRiskOff   Regime = "RISK_OFF"
	RegimeUndefined Regime = "UNDEFINED"
)

// AllRegimes is the enumeration used by evaluation partitioning and config
// validation.
var AllRegimes = []Regime{
	RegimeBullTrend, RegimeBearTrend, RegimeRange, RegimeHighVol, RegimeLowVol,
	RegimeBreakout, RegimeReversal, RegimeRiskOn, RegimeRiskOff,
}

// Trending reports whether the regime is directional.
func (r Regime) Trending() bool { return r == RegimeBullTrend || r == RegimeBearTrend }

// Bullish reports whether the regime favours long setups.
func (r Regime) Bullish() bool {
	return r == RegimeBullTrend || r == RegimeRiskOn || r == RegimeBreakout
}

// Bearish reports whether the regime favours defensive posture.
func (r Regime) Bearish() bool {
	return r == RegimeBearTrend || r == RegimeRiskOff
}

// MarketRegime is the market-wide state record. Every prediction, signal and
// evaluation stores the regime that was in force, which is what makes
// regime-partitioned performance analysis possible.
type MarketRegime struct {
	Regime     Regime  `json:"regime"`
	Confidence float64 `json:"confidence"`
	// Secondary carries the runner-up classification and its score, so the
	// dashboard can show "bull trend, but close to range".
	Secondary      Regime  `json:"secondary,omitempty"`
	SecondaryScore float64 `json:"secondary_score,omitempty"`

	Volatility   float64 `json:"volatility"`    // 0..1 normalised, 1 = extreme
	Breadth      float64 `json:"breadth"`       // -1..1, share of advancers vs decliners
	Momentum     float64 `json:"momentum"`      // -1..1
	RiskAppetite float64 `json:"risk_appetite"` // -1..1, risk-off .. risk-on
	Liquidity    float64 `json:"liquidity"`     // 0..1 normalised market-wide volume

	// Raw inputs, retained for explanation and reproducibility.
	Inputs map[string]float64 `json:"inputs"`
	// Scores holds the score assigned to every candidate regime.
	Scores map[Regime]float64 `json:"scores"`
	// Evidence is a set of human-readable statements the AI analyst may quote.
	Evidence []string `json:"evidence,omitempty"`

	AsOf           time.Time     `json:"as_of"`
	Version        string        `json:"version"`
	Changed        bool          `json:"changed"`
	PreviousRegime Regime        `json:"previous_regime,omitempty"`
	StableFor      time.Duration `json:"stable_for"`
}

// SectorPerformance summarises one sector's relative behaviour.
type SectorPerformance struct {
	Sector      string  `json:"sector"`
	ETF         Ticker  `json:"etf,omitempty"`
	Return1D    float64 `json:"return_1d"`
	Return5D    float64 `json:"return_5d"`
	Return20D   float64 `json:"return_20d"`
	RelStrength float64 `json:"rel_strength"`
	Breadth     float64 `json:"breadth"`
	Rank        int     `json:"rank"`
}

// RelationshipState is the cross-asset picture produced by the relationship
// engine: correlations, rotation and divergence between the market's anchors.
type RelationshipState struct {
	AsOf              time.Time           `json:"as_of"`
	Correlation       map[string]float64  `json:"correlation"` // "SPY|QQQ" -> rho
	CorrelationChange map[string]float64  `json:"correlation_change"`
	Sectors           []SectorPerformance `json:"sectors"`
	Rotation          string              `json:"rotation"` // e.g. "CYCLICAL_TO_DEFENSIVE"
	RiskOnScore       float64             `json:"risk_on_score"`
	Divergences       []Divergence        `json:"divergences,omitempty"`
	Notes             []string            `json:"notes,omitempty"`
}

// Divergence records two series that normally move together and currently do not.
type Divergence struct {
	A         string  `json:"a"`
	B         string  `json:"b"`
	Expected  float64 `json:"expected_correlation"`
	Observed  float64 `json:"observed_correlation"`
	Magnitude float64 `json:"magnitude"`
	Note      string  `json:"note"`
}

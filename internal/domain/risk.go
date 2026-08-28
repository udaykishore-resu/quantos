package domain

import "time"

// RiskDecision is the risk engine's verdict. The ordering of the constants is
// meaningful: Severity() uses it to resolve multiple checks, most severe wins.
type RiskDecision string

const (
	RiskAllowPaperSignal RiskDecision = "ALLOW_PAPER_SIGNAL"
	RiskWatchOnly        RiskDecision = "WATCH_ONLY"
	RiskBlock            RiskDecision = "BLOCK"
)

// Severity maps a decision to a comparable integer. Higher is more restrictive.
func (d RiskDecision) Severity() int {
	switch d {
	case RiskBlock:
		return 2
	case RiskWatchOnly:
		return 1
	default:
		return 0
	}
}

// Allows reports whether a paper signal may be created.
func (d RiskDecision) Allows() bool { return d == RiskAllowPaperSignal }

// RiskLevel is the coarse label shown on the dashboard. It is descriptive; the
// Decision is what is enforced.
type RiskLevel string

const (
	RiskLow     RiskLevel = "LOW"
	RiskMedium  RiskLevel = "MEDIUM"
	RiskHigh    RiskLevel = "HIGH"
	RiskExtreme RiskLevel = "EXTREME"
)

// CheckStatus is the outcome of one individual risk check.
type CheckStatus string

const (
	CheckPass CheckStatus = "PASS"
	CheckWarn CheckStatus = "WARN"
	CheckFail CheckStatus = "FAIL"
	CheckSkip CheckStatus = "SKIP"
)

// RiskCheck is one named check with its numeric evidence. Every check is
// persisted so that a historical veto can be explained precisely.
type RiskCheck struct {
	Name      string       `json:"name"`
	Status    CheckStatus  `json:"status"`
	Decision  RiskDecision `json:"decision"`
	Value     float64      `json:"value"`
	Threshold float64      `json:"threshold"`
	Reason    string       `json:"reason"`
	Skipped   string       `json:"skipped_reason,omitempty"`
}

// RiskAssessment is the complete risk verdict for one symbol at one instant.
type RiskAssessment struct {
	ID        string    `json:"id"`
	Ticker    Ticker    `json:"ticker"`
	CreatedAt time.Time `json:"created_at"`

	Decision RiskDecision `json:"decision"`
	Level    RiskLevel    `json:"level"`
	Score    float64      `json:"score"` // 0..100, higher = riskier
	Checks   []RiskCheck  `json:"checks"`

	// Blockers and Warnings are the human-readable reasons, derived from Checks.
	Blockers []string `json:"blockers,omitempty"`
	Warnings []string `json:"warnings,omitempty"`

	// Provenance.
	ConfigHash   string    `json:"config_hash"`
	Regime       Regime    `json:"regime"`
	FeatureHash  string    `json:"feature_hash,omitempty"`
	PredictionID string    `json:"prediction_id,omitempty"`
	EvaluatedAt  time.Time `json:"evaluated_at"`
}

// Blocked reports whether this assessment forbids any emission.
func (a RiskAssessment) Blocked() bool { return a.Decision == RiskBlock }

// Check returns a named check.
func (a RiskAssessment) Check(name string) (RiskCheck, bool) {
	for _, c := range a.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return RiskCheck{}, false
}

// Canonical risk check names.
const (
	CheckFreshness     = "data_freshness"
	CheckLiquidity     = "liquidity"
	CheckSpread        = "spread"
	CheckVolatility    = "volatility"
	CheckEventRisk     = "event_proximity"
	CheckConfidence    = "model_confidence"
	CheckModelHealth   = "model_health"
	CheckRegime        = "market_regime"
	CheckConcentration = "portfolio_concentration"
	CheckCorrelation   = "correlation_exposure"
	CheckDrawdown      = "portfolio_drawdown"
)

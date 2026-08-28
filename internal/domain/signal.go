package domain

import (
	"fmt"
	"time"
)

// Classification is the research label attached to a scored instrument.
// It is explicitly not advice (governance rule G-9).
type Classification string

const (
	ClassStrongWatch  Classification = "STRONG_WATCH"
	ClassBullishSetup Classification = "BULLISH_SETUP"
	ClassNeutral      Classification = "NEUTRAL"
	ClassHighRisk     Classification = "HIGH_RISK"
	ClassAvoid        Classification = "AVOID"
)

// StockScore is the composite research score for one instrument.
// Every sub-score is 0..100 and every sub-score is independently explainable.
type StockScore struct {
	Ticker    Ticker    `json:"ticker"`
	CreatedAt time.Time `json:"created_at"`

	FundamentalScore  float64 `json:"fundamental_score"`
	GrowthScore       float64 `json:"growth_score"`
	QualityScore      float64 `json:"quality_score"`
	ValuationScore    float64 `json:"valuation_score"`
	TechnicalScore    float64 `json:"technical_score"`
	MomentumScore     float64 `json:"momentum_score"`
	SectorScore       float64 `json:"sector_score"`
	MarketRegimeScore float64 `json:"market_regime_score"`
	RiskScore         float64 `json:"risk_score"`       // higher = safer
	EventRiskScore    float64 `json:"event_risk_score"` // higher = safer

	FinalScore     float64        `json:"final_score"`
	Classification Classification `json:"classification"`

	// Weights records the strategy weights in force, so a historical score can
	// be recomputed even after the config changes.
	Weights    map[string]float64 `json:"weights"`
	StrategyID string             `json:"strategy_id"`
	ConfigHash string             `json:"config_hash"`

	// Missing lists sub-scores that could not be computed (e.g. no fundamentals
	// for an ETF). Missing components are excluded and weights renormalised,
	// never silently treated as zero.
	Missing  []string `json:"missing,omitempty"`
	Evidence []string `json:"evidence,omitempty"`

	Disclaimer string `json:"disclaimer"`
}

// OpportunityScore fuses the stock score, the prediction and the environment.
type OpportunityScore struct {
	Ticker     Ticker    `json:"ticker"`
	CreatedAt  time.Time `json:"created_at"`
	Score      float64   `json:"score"` // 0..100
	Bias       Side      `json:"bias"`
	Confidence float64   `json:"confidence"`
	Risk       RiskLevel `json:"risk"`

	// Components exposes the additive decomposition of Score, so the number is
	// never a black box.
	Components map[string]float64 `json:"components"`

	RiskReward     float64 `json:"risk_reward"`
	ExpectedMove   float64 `json:"expected_move_bps"`
	StopDistance   float64 `json:"stop_distance_bps"`
	TargetDistance float64 `json:"target_distance_bps"`

	PredictionID string `json:"prediction_id,omitempty"`
	ScoreRef     string `json:"score_ref,omitempty"`
	Regime       Regime `json:"regime"`
	Disclaimer   string `json:"disclaimer"`
}

// SignalStatus is the lifecycle state of a paper-trading signal.
type SignalStatus string

const (
	SignalActive      SignalStatus = "ACTIVE"
	SignalInvalidated SignalStatus = "INVALIDATED"
	SignalExpired     SignalStatus = "EXPIRED"
	SignalCompleted   SignalStatus = "COMPLETED"
)

// InvalidationKind enumerates the machine-checkable invalidation triggers.
type InvalidationKind string

const (
	InvalidPriceBelow      InvalidationKind = "PRICE_BELOW"
	InvalidPriceAbove      InvalidationKind = "PRICE_ABOVE"
	InvalidVWAPCross       InvalidationKind = "VWAP_CROSS"
	InvalidRegimeChange    InvalidationKind = "REGIME_CHANGE"
	InvalidVolatilityAbove InvalidationKind = "VOLATILITY_ABOVE"
	InvalidConfidenceBelow InvalidationKind = "CONFIDENCE_BELOW"
	InvalidRiskVeto        InvalidationKind = "RISK_VETO"
	InvalidTimeExpiry      InvalidationKind = "TIME_EXPIRY"
	InvalidNewsMaterial    InvalidationKind = "MATERIAL_NEWS"
)

// InvalidationCondition is one machine-evaluable condition. Signals are not
// allowed to exist without at least one (governance rule G-6).
type InvalidationCondition struct {
	Kind        InvalidationKind `json:"kind"`
	Threshold   float64          `json:"threshold,omitempty"`
	RegimeFrom  Regime           `json:"regime_from,omitempty"`
	Deadline    time.Time        `json:"deadline,omitempty"`
	Description string           `json:"description"`
}

// Signal is a paper-trading signal. It can only be constructed through
// NewSignal, which requires an allowing risk assessment (ADR-009).
type Signal struct {
	ID        string    `json:"id"`
	Ticker    Ticker    `json:"ticker"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	Side       Side         `json:"side"`
	Status     SignalStatus `json:"status"`
	Strength   float64      `json:"strength"` // 0..100
	Confidence float64      `json:"confidence"`
	Horizon    Horizon      `json:"horizon"`

	EntryReference  float64 `json:"entry_reference"`
	StopReference   float64 `json:"stop_reference"`
	TargetReference float64 `json:"target_reference"`
	SuggestedWeight float64 `json:"suggested_weight"` // fraction of paper equity

	Invalidations []InvalidationCondition `json:"invalidations"`

	// Provenance chain.
	StrategyID       string   `json:"strategy_id"`
	RulesFired       []string `json:"rules_fired"`
	Evidence         []string `json:"evidence"`
	CounterEvidence  []string `json:"counter_evidence,omitempty"`
	PredictionID     string   `json:"prediction_id"`
	RiskAssessmentID string   `json:"risk_assessment_id"`
	ScoreRef         string   `json:"score_ref,omitempty"`
	FeatureHash      string   `json:"feature_hash"`
	Regime           Regime   `json:"regime"`
	ModelVersion     string   `json:"model_version"`
	ConfigHash       string   `json:"config_hash"`
	CorrelationID    string   `json:"correlation_id"`

	// Lifecycle.
	InvalidatedAt    *time.Time       `json:"invalidated_at,omitempty"`
	InvalidationKind InvalidationKind `json:"invalidation_kind,omitempty"`
	InvalidationNote string           `json:"invalidation_note,omitempty"`

	// Explanation is filled asynchronously by the AI analyst and is never
	// required for the signal to be valid.
	Explanation       string `json:"explanation,omitempty"`
	ExplanationSource string `json:"explanation_source,omitempty"`

	Disclaimer string `json:"disclaimer"`
}

// ErrRiskVeto is returned when signal construction is attempted without an
// allowing risk assessment.
var ErrRiskVeto = fmt.Errorf("signal: blocked by risk engine")

// ErrNoInvalidation is returned when a signal is constructed without any
// invalidation condition.
var ErrNoInvalidation = fmt.Errorf("signal: at least one invalidation condition is required")

// SignalDraft carries everything needed to build a Signal. It exists so that
// Signal has no exported constructor that bypasses the risk gate.
type SignalDraft struct {
	Ticker          Ticker
	CreatedAt       time.Time
	ExpiresAt       time.Time
	Side            Side
	Strength        float64
	Confidence      float64
	Horizon         Horizon
	EntryReference  float64
	StopReference   float64
	TargetReference float64
	SuggestedWeight float64
	Invalidations   []InvalidationCondition
	StrategyID      string
	RulesFired      []string
	Evidence        []string
	CounterEvidence []string
	PredictionID    string
	ScoreRef        string
	FeatureHash     string
	Regime          Regime
	ModelVersion    string
	ConfigHash      string
	CorrelationID   string
	ID              string
}

// NewSignal is the only way to build a Signal. It enforces ADR-009 (the risk
// engine's veto) and governance rule G-6 (mandatory invalidation conditions).
func NewSignal(d SignalDraft, risk RiskAssessment) (Signal, error) {
	if !risk.Decision.Allows() {
		return Signal{}, fmt.Errorf("%w: decision=%s blockers=%v", ErrRiskVeto, risk.Decision, risk.Blockers)
	}
	if len(d.Invalidations) == 0 {
		return Signal{}, ErrNoInvalidation
	}
	if d.Ticker == "" {
		return Signal{}, fmt.Errorf("signal: empty ticker")
	}
	if d.Side != SideLong && d.Side != SideShort {
		return Signal{}, fmt.Errorf("signal: side must be LONG or SHORT, got %q", d.Side)
	}
	if d.CreatedAt.IsZero() {
		return Signal{}, fmt.Errorf("signal: zero creation time")
	}
	if d.ExpiresAt.IsZero() {
		d.ExpiresAt = d.CreatedAt.Add(d.Horizon.Duration())
	}
	return Signal{
		ID:               d.ID,
		Ticker:           d.Ticker,
		CreatedAt:        d.CreatedAt,
		ExpiresAt:        d.ExpiresAt,
		Side:             d.Side,
		Status:           SignalActive,
		Strength:         Clamp(d.Strength, 0, 100),
		Confidence:       Clamp01(d.Confidence),
		Horizon:          d.Horizon,
		EntryReference:   d.EntryReference,
		StopReference:    d.StopReference,
		TargetReference:  d.TargetReference,
		SuggestedWeight:  Clamp(d.SuggestedWeight, 0, 1),
		Invalidations:    d.Invalidations,
		StrategyID:       d.StrategyID,
		RulesFired:       d.RulesFired,
		Evidence:         d.Evidence,
		CounterEvidence:  d.CounterEvidence,
		PredictionID:     d.PredictionID,
		RiskAssessmentID: risk.ID,
		ScoreRef:         d.ScoreRef,
		FeatureHash:      d.FeatureHash,
		Regime:           d.Regime,
		ModelVersion:     d.ModelVersion,
		ConfigHash:       d.ConfigHash,
		CorrelationID:    d.CorrelationID,
		Disclaimer:       Disclaimer,
	}, nil
}

// Active reports whether the signal is live at the given time.
func (s Signal) Active(now time.Time) bool {
	return s.Status == SignalActive && now.Before(s.ExpiresAt)
}

// Disclaimer is attached to every artifact that carries a directional
// classification (governance rule G-9).
const Disclaimer = "QuantOS is an educational research and paper-trading platform. " +
	"Output is probabilistic, may be wrong, and is not financial advice. " +
	"No real-money orders are placed."

package domain

import "time"

// NewsCategory is the deterministic classification assigned before any LLM is
// involved (ADR-005: deterministic preprocessing first).
type NewsCategory string

const (
	NewsEarnings   NewsCategory = "EARNINGS"
	NewsGuidance   NewsCategory = "GUIDANCE"
	NewsMA         NewsCategory = "M_AND_A"
	NewsRegulatory NewsCategory = "REGULATORY"
	NewsLegal      NewsCategory = "LEGAL"
	NewsProduct    NewsCategory = "PRODUCT"
	NewsManagement NewsCategory = "MANAGEMENT"
	NewsAnalyst    NewsCategory = "ANALYST_ACTION"
	NewsMacroNews  NewsCategory = "MACRO"
	NewsGeneral    NewsCategory = "GENERAL"
)

// Sentiment is a bounded, coarse label. The numeric score carries the detail.
type Sentiment string

const (
	SentimentPositive Sentiment = "positive"
	SentimentNeutral  Sentiment = "neutral"
	SentimentNegative Sentiment = "negative"
)

// Materiality is how much the event could plausibly move the instrument.
type Materiality string

const (
	MaterialityHigh   Materiality = "high"
	MaterialityMedium Materiality = "medium"
	MaterialityLow    Materiality = "low"
)

// NewsEvent is a processed news item. Only the typed fields cross into the
// decision path; the raw text never does.
type NewsEvent struct {
	ID         string    `json:"id"`
	Ticker     Ticker    `json:"ticker"` // "__MACRO__" for market-wide items
	Timestamp  time.Time `json:"timestamp"`
	IngestedAt time.Time `json:"ingested_at"`
	Source     string    `json:"source"`
	Headline   string    `json:"headline"`
	URL        string    `json:"url,omitempty"`

	Category         NewsCategory `json:"category"`
	Sentiment        Sentiment    `json:"sentiment"`
	SentimentScore   float64      `json:"sentiment_score"` // -1..1
	Materiality      Materiality  `json:"materiality"`
	MaterialityScore float64      `json:"materiality_score"` // 0..1
	Confidence       float64      `json:"confidence"`        // 0..1
	AffectedSector   string       `json:"affected_sector,omitempty"`
	ExpectedHorizon  Horizon      `json:"expected_horizon"`

	// Processing provenance.
	Stage        string   `json:"stage"` // "deterministic" | "llm-enriched"
	Matched      []string `json:"matched_terms,omitempty"`
	LLMModel     string   `json:"llm_model,omitempty"`
	LLMAgreed    bool     `json:"llm_agreed,omitempty"`
	Deduplicated bool     `json:"deduplicated,omitempty"`
	ClusterID    string   `json:"cluster_id,omitempty"`
}

// Material reports whether the event clears the bar for affecting decisions.
func (n NewsEvent) Material() bool {
	return n.Materiality == MaterialityHigh ||
		(n.Materiality == MaterialityMedium && n.Confidence >= 0.7)
}

// SignedImpact returns sentiment × materiality × confidence in [-1, 1]. This is
// the only numeric channel by which news reaches the decision path.
func (n NewsEvent) SignedImpact() float64 {
	return Clamp(n.SentimentScore*n.MaterialityScore*n.Confidence, -1, 1)
}

// AlertType enumerates intraday alert triggers.
type AlertType string

const (
	AlertVWAPCross          AlertType = "VWAP_CROSS"
	AlertVolumeAcceleration AlertType = "VOLUME_ACCELERATION"
	AlertBreakout           AlertType = "BREAKOUT"
	AlertMomentumShift      AlertType = "MOMENTUM_SHIFT"
	AlertRegimeChange       AlertType = "REGIME_CHANGE"
	AlertVolatilitySpike    AlertType = "VOLATILITY_SPIKE"
	AlertNewsEvent          AlertType = "NEWS_EVENT"
	AlertProbabilityShift   AlertType = "PROBABILITY_SHIFT"
	AlertSignalGenerated    AlertType = "SIGNAL_GENERATED"
	AlertSignalInvalidated  AlertType = "SIGNAL_INVALIDATED"
	AlertRiskBlock          AlertType = "RISK_BLOCK"
	AlertModelDrift         AlertType = "MODEL_DRIFT"
	AlertDataStale          AlertType = "DATA_STALE"
)

// AlertSeverity orders alerts for display and routing.
type AlertSeverity string

const (
	SeverityInfo     AlertSeverity = "INFO"
	SeverityNotice   AlertSeverity = "NOTICE"
	SeverityWarning  AlertSeverity = "WARNING"
	SeverityCritical AlertSeverity = "CRITICAL"
)

// Alert is an emitted notification. DedupKey is what makes alerting idempotent
// across redelivery and replay (ADR-010, §36).
type Alert struct {
	ID        string        `json:"id"`
	DedupKey  string        `json:"dedup_key"`
	Type      AlertType     `json:"type"`
	Severity  AlertSeverity `json:"severity"`
	Ticker    Ticker        `json:"ticker,omitempty"`
	CreatedAt time.Time     `json:"created_at"`

	Title   string `json:"title"`
	Message string `json:"message"`

	// Structured payload. The dashboard renders from these, never by parsing
	// Message, so displayed numbers are always authoritative.
	Before map[string]float64 `json:"before,omitempty"`
	After  map[string]float64 `json:"after,omitempty"`

	Regime        Regime    `json:"regime,omitempty"`
	RiskLevel     RiskLevel `json:"risk_level,omitempty"`
	Status        string    `json:"status,omitempty"` // e.g. "PAPER-TRADING WATCH"
	SignalID      string    `json:"signal_id,omitempty"`
	PredictionID  string    `json:"prediction_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Disclaimer    string    `json:"disclaimer"`
}

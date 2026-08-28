package domain

import "time"

// ModelStage controls how a registered model participates in serving.
type ModelStage string

const (
	StageShadow  ModelStage = "shadow" // scored, recorded, never served
	StageCanary  ModelStage = "canary" // served to a stable fraction of symbols
	StageActive  ModelStage = "active" // serving
	StageRetired ModelStage = "retired"
)

// ModelFamily enumerates the inference implementations available in Go.
type ModelFamily string

const (
	FamilyLogistic ModelFamily = "multinomial_logistic"
	FamilyStumps   ModelFamily = "gradient_boosted_stumps"
)

// ModelVersion is the registry record for one trained artifact.
type ModelVersion struct {
	ID          string      `json:"id"`
	ModelID     string      `json:"model_id"`
	Version     string      `json:"version"`
	Family      ModelFamily `json:"family"`
	Stage       ModelStage  `json:"stage"`
	ArtifactURI string      `json:"artifact_uri"`
	ArtifactSHA string      `json:"artifact_sha"`

	Features    []string `json:"features"`
	Horizon     Horizon  `json:"horizon"`
	FlatBandBps float64  `json:"flat_band_bps"`

	TrainStart time.Time `json:"train_start"`
	TrainEnd   time.Time `json:"train_end"`
	ValidStart time.Time `json:"valid_start"`
	ValidEnd   time.Time `json:"valid_end"`
	TestStart  time.Time `json:"test_start"`
	TestEnd    time.Time `json:"test_end"`

	TrainedAt      time.Time  `json:"trained_at"`
	RegisteredAt   time.Time  `json:"registered_at"`
	PromotedAt     *time.Time `json:"promoted_at,omitempty"`
	TrainingCommit string     `json:"training_commit,omitempty"`

	Hyperparameters map[string]float64 `json:"hyperparameters,omitempty"`
	CanaryFraction  float64            `json:"canary_fraction,omitempty"`

	OutOfSample ModelEvaluation            `json:"out_of_sample"`
	ByRegime    map[Regime]ModelEvaluation `json:"by_regime,omitempty"`
	Notes       string                     `json:"notes,omitempty"`
}

// ConfusionMatrix counts predicted-vs-actual across the three outcome classes.
type ConfusionMatrix struct {
	// Counts[predicted][actual]
	Counts map[Outcome]map[Outcome]int `json:"counts"`
	Total  int                         `json:"total"`
}

// NewConfusionMatrix returns an initialised, zeroed matrix.
func NewConfusionMatrix() ConfusionMatrix {
	c := ConfusionMatrix{Counts: map[Outcome]map[Outcome]int{}}
	for _, p := range AllOutcomes {
		c.Counts[p] = map[Outcome]int{}
		for _, a := range AllOutcomes {
			c.Counts[p][a] = 0
		}
	}
	return c
}

// Add records one observation.
func (c *ConfusionMatrix) Add(predicted, actual Outcome) {
	if c.Counts == nil {
		*c = NewConfusionMatrix()
	}
	c.Counts[predicted][actual]++
	c.Total++
}

// Precision is TP / (TP + FP) for one class.
func (c ConfusionMatrix) Precision(o Outcome) float64 {
	tp := c.Counts[o][o]
	den := 0
	for _, a := range AllOutcomes {
		den += c.Counts[o][a]
	}
	if den == 0 {
		return 0
	}
	return float64(tp) / float64(den)
}

// Recall is TP / (TP + FN) for one class.
func (c ConfusionMatrix) Recall(o Outcome) float64 {
	tp := c.Counts[o][o]
	den := 0
	for _, p := range AllOutcomes {
		den += c.Counts[p][o]
	}
	if den == 0 {
		return 0
	}
	return float64(tp) / float64(den)
}

// F1 is the harmonic mean of precision and recall.
func (c ConfusionMatrix) F1(o Outcome) float64 {
	p, r := c.Precision(o), c.Recall(o)
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// Accuracy is the fraction of correct predictions.
func (c ConfusionMatrix) Accuracy() float64 {
	if c.Total == 0 {
		return 0
	}
	correct := 0
	for _, o := range AllOutcomes {
		correct += c.Counts[o][o]
	}
	return float64(correct) / float64(c.Total)
}

// CalibrationBin is one bucket of a reliability diagram.
type CalibrationBin struct {
	Lower         float64 `json:"lower"`
	Upper         float64 `json:"upper"`
	Count         int     `json:"count"`
	MeanPredicted float64 `json:"mean_predicted"`
	Observed      float64 `json:"observed_frequency"`
	Deviation     float64 `json:"deviation"`
}

// ModelEvaluation is the measured performance of a model over a window.
type ModelEvaluation struct {
	ModelVersion string    `json:"model_version"`
	Window       string    `json:"window"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Horizon      Horizon   `json:"horizon"`
	Regime       Regime    `json:"regime,omitempty"`

	Samples    int     `json:"samples"`
	Accuracy   float64 `json:"accuracy"`
	BaseRate   float64 `json:"base_rate"`
	Lift       float64 `json:"lift"`
	BrierScore float64 `json:"brier_score"`
	BrierSkill float64 `json:"brier_skill_score"`
	LogLoss    float64 `json:"log_loss"`

	PrecisionUp   float64 `json:"precision_up"`
	RecallUp      float64 `json:"recall_up"`
	PrecisionDown float64 `json:"precision_down"`
	RecallDown    float64 `json:"recall_down"`
	MacroF1       float64 `json:"macro_f1"`

	Confusion   ConfusionMatrix  `json:"confusion"`
	Calibration []CalibrationBin `json:"calibration,omitempty"`
	// MaxCalibrationDeviation is the promotion gate in the governance charter.
	MaxCalibrationDeviation  float64 `json:"max_calibration_deviation"`
	ExpectedCalibrationError float64 `json:"expected_calibration_error"`

	ComputedAt time.Time `json:"computed_at"`
}

// DriftSeverity grades a detected drift.
type DriftSeverity string

const (
	DriftNone     DriftSeverity = "none"
	DriftMild     DriftSeverity = "mild"
	DriftModerate DriftSeverity = "moderate"
	DriftSevere   DriftSeverity = "severe"
)

// DriftReport is the output of the drift detector.
type DriftReport struct {
	ModelVersion string        `json:"model_version"`
	ComputedAt   time.Time     `json:"computed_at"`
	Severity     DriftSeverity `json:"severity"`

	// FeatureDrift is population stability index per feature. PSI > 0.25 is the
	// conventional "significant shift" threshold.
	FeatureDrift    map[string]float64 `json:"feature_drift"`
	MaxFeaturePSI   float64            `json:"max_feature_psi"`
	DriftedFeatures []string           `json:"drifted_features,omitempty"`

	// PredictionDrift is the KL divergence of the recent prediction
	// distribution from the reference window.
	PredictionDrift float64 `json:"prediction_drift"`

	// Performance degradation.
	ReferenceAccuracy float64 `json:"reference_accuracy"`
	RecentAccuracy    float64 `json:"recent_accuracy"`
	AccuracyDelta     float64 `json:"accuracy_delta"`
	ReferenceBrier    float64 `json:"reference_brier"`
	RecentBrier       float64 `json:"recent_brier"`

	// RegimeDegradation flags regimes where performance has fallen below the
	// base rate, which is the practically important failure.
	RegimeDegradation map[Regime]float64 `json:"regime_degradation,omitempty"`

	Samples        int      `json:"samples"`
	Recommendation string   `json:"recommendation"`
	Reasons        []string `json:"reasons,omitempty"`
}

// ModelHealth is the dashboard-facing summary.
type ModelHealth struct {
	ModelID      string     `json:"model_id"`
	ModelVersion string     `json:"model_version"`
	Stage        ModelStage `json:"stage"`
	AsOf         time.Time  `json:"as_of"`

	Accuracy    float64       `json:"accuracy"`
	Calibration float64       `json:"calibration"` // 1 - ECE, higher is better
	BrierScore  float64       `json:"brier_score"`
	Drift       DriftSeverity `json:"drift"`

	HighVolatilityAccuracy float64 `json:"high_volatility_accuracy"`
	TrendingAccuracy       float64 `json:"trending_accuracy"`
	RangeAccuracy          float64 `json:"range_accuracy"`

	Samples         int       `json:"samples"`
	Healthy         bool      `json:"healthy"`
	Issues          []string  `json:"issues,omitempty"`
	LastEvaluatedAt time.Time `json:"last_evaluated_at"`

	// Provisional marks a health record derived from the model's own offline
	// test split rather than from live resolved predictions.
	//
	// It exists to resolve a real bootstrap problem: a newly promoted model has
	// no live outcomes yet, and refusing to serve until it does means it never
	// gets any. The measurement is still a measurement — it was made on data
	// the model never saw — but it was made under a different distribution, so
	// the record says so and is replaced by live evidence as soon as enough
	// predictions have resolved.
	Provisional bool `json:"provisional,omitempty"`
}

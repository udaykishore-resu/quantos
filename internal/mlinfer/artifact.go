// Package mlinfer loads versioned model artifacts and performs inference
// in-process (ADR-006).
//
// The artifact is plain, inspectable JSON. That is deliberate: a model whose
// weights can be read, diffed and checksummed is one whose behaviour can be
// audited, and inference that is a pure function of (artifact, feature vector)
// is one whose historical output can be recomputed exactly.
package mlinfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// ArtifactSchemaVersion is the format version this package understands.
const ArtifactSchemaVersion = 1

// Artifact is a trained model exported by ml/training.
type Artifact struct {
	SchemaVersion int                `json:"schema_version"`
	ModelID       string             `json:"model_id"`
	Version       string             `json:"version"`
	Family        domain.ModelFamily `json:"family"`
	Horizon       domain.Horizon     `json:"horizon"`
	FlatBandBps   float64            `json:"flat_band_bps"`

	// Features is the ordered feature list. Order is part of the contract:
	// the same names in a different order is a different model.
	Features []string `json:"features"`
	// Mean and Std standardise the input vector. Both must match len(Features).
	Mean []float64 `json:"mean"`
	Std  []float64 `json:"std"`

	// Multinomial logistic parameters: one coefficient row per class, in the
	// order of domain.AllOutcomes.
	Coefficients [][]float64 `json:"coefficients,omitempty"`
	Intercepts   []float64   `json:"intercepts,omitempty"`

	// Gradient-boosted stumps: additive score per class.
	Stumps    [][]Stump `json:"stumps,omitempty"`
	BaseScore []float64 `json:"base_score,omitempty"`

	// Calibration is applied after the raw distribution is formed.
	Calibration Calibration `json:"calibration"`

	Metadata Metadata `json:"metadata"`

	// Digest is computed at load time over the file bytes, not stored in it.
	Digest string `json:"-"`
	Path   string `json:"-"`
}

// Stump is a depth-one decision tree.
type Stump struct {
	Feature   int     `json:"f"`
	Threshold float64 `json:"t"`
	Left      float64 `json:"l"`
	Right     float64 `json:"r"`
}

// Calibration post-processes raw probabilities so that a stated 0.7 means
// roughly 70% empirically. An uncalibrated classifier's "probability" is not a
// probability, and the risk engine thresholds on it.
type Calibration struct {
	// Method is "none", "temperature" or "vector".
	Method      string  `json:"method"`
	Temperature float64 `json:"temperature,omitempty"`
	// Scale and Shift apply per class for vector scaling.
	Scale []float64 `json:"scale,omitempty"`
	Shift []float64 `json:"shift,omitempty"`
	// ECE and MaxDeviation are recorded from validation, for the promotion gate.
	ECE          float64 `json:"ece,omitempty"`
	MaxDeviation float64 `json:"max_deviation,omitempty"`
}

// Metadata records how the artifact was produced.
type Metadata struct {
	TrainedAt  time.Time `json:"trained_at"`
	TrainStart time.Time `json:"train_start"`
	TrainEnd   time.Time `json:"train_end"`
	ValidStart time.Time `json:"valid_start"`
	ValidEnd   time.Time `json:"valid_end"`
	TestStart  time.Time `json:"test_start"`
	TestEnd    time.Time `json:"test_end"`
	Samples    int       `json:"samples"`

	// The model's own report card, measured on a test split it never saw.
	//
	// These are required, not decorative. Without them the runtime has no way
	// to know whether a freshly promoted model is better than a constant guess,
	// and "we have not measured it yet" would either block the model forever or
	// let it serve unmeasured. Reporting the base rate alongside the accuracy
	// is the part that makes the number mean anything.
	TestSamples  int     `json:"test_samples"`
	TestAccuracy float64 `json:"test_accuracy"`
	TestBaseRate float64 `json:"test_base_rate"`
	TestBrier    float64 `json:"test_brier,omitempty"`
	// TestDirectionalAUC is the model's measured ability to separate UP from
	// DOWN on the test split, ignoring FLAT. 0.5 is a coin flip.
	//
	// It is reported separately from accuracy because a three-class model can
	// score well by being good at one question and useless at another: telling
	// a quiet tape from a moving one is not the same skill as telling up from
	// down, and only the second is what a directional signal rests on. Absent
	// or 0 means unmeasured, which the prediction engine treats as no skill.
	TestDirectionalAUC float64            `json:"test_directional_auc,omitempty"`
	TrainingCommit     string             `json:"training_commit,omitempty"`
	Hyperparams        map[string]float64 `json:"hyperparameters,omitempty"`
	Notes              string             `json:"notes,omitempty"`
	// Generator identifies what produced the artifact, so a synthetic
	// bootstrap model is never mistaken for a fitted one.
	Generator string `json:"generator,omitempty"`
}

// Validate checks the artifact's internal consistency. It is strict on purpose:
// a silently misaligned feature vector produces confident nonsense, which is
// worse than a refusal to start (ADR-006).
func (a *Artifact) Validate() error {
	var errs []string
	add := func(f string, v ...any) { errs = append(errs, fmt.Sprintf(f, v...)) }

	if a.SchemaVersion != ArtifactSchemaVersion {
		add("schema_version %d is not supported (this build understands %d)", a.SchemaVersion, ArtifactSchemaVersion)
	}
	if a.ModelID == "" || a.Version == "" {
		add("model_id and version are required")
	}
	n := len(a.Features)
	if n == 0 {
		add("features must not be empty")
	}
	if len(a.Mean) != n || len(a.Std) != n {
		add("mean (%d) and std (%d) must both match the feature count (%d)", len(a.Mean), len(a.Std), n)
	}
	for i, s := range a.Std {
		if s <= 0 {
			add("std[%d] must be positive, got %g", i, s)
		}
	}
	switch a.Family {
	case domain.FamilyLogistic:
		if len(a.Coefficients) != len(domain.AllOutcomes) {
			add("coefficients must have %d rows, got %d", len(domain.AllOutcomes), len(a.Coefficients))
		}
		for i, row := range a.Coefficients {
			if len(row) != n {
				add("coefficients[%d] has %d entries, expected %d", i, len(row), n)
			}
		}
		if len(a.Intercepts) != len(domain.AllOutcomes) {
			add("intercepts must have %d entries, got %d", len(domain.AllOutcomes), len(a.Intercepts))
		}
	case domain.FamilyStumps:
		if len(a.Stumps) != len(domain.AllOutcomes) {
			add("stumps must have %d class groups, got %d", len(domain.AllOutcomes), len(a.Stumps))
		}
		for ci, group := range a.Stumps {
			for si, s := range group {
				if s.Feature < 0 || s.Feature >= n {
					add("stumps[%d][%d].f=%d out of range for %d features", ci, si, s.Feature, n)
				}
			}
		}
		if len(a.BaseScore) != len(domain.AllOutcomes) {
			add("base_score must have %d entries", len(domain.AllOutcomes))
		}
	default:
		add("unknown family %q", a.Family)
	}
	switch a.Calibration.Method {
	case "", "none":
	case "temperature":
		if a.Calibration.Temperature <= 0 {
			add("calibration temperature must be positive")
		}
	case "vector":
		if len(a.Calibration.Scale) != len(domain.AllOutcomes) || len(a.Calibration.Shift) != len(domain.AllOutcomes) {
			add("vector calibration requires scale and shift of length %d", len(domain.AllOutcomes))
		}
	default:
		add("unknown calibration method %q", a.Calibration.Method)
	}
	if a.FlatBandBps <= 0 {
		add("flat_band_bps must be positive: without it the classes are undefined")
	}
	// An artifact that does not report how it scored on held-out data cannot be
	// served. The alternative is a model whose accuracy nobody has ever
	// measured making probability claims to a user, which is the failure this
	// whole platform is built to avoid.
	if a.Metadata.TestSamples <= 0 {
		add("metadata.test_samples is required: an unmeasured model may not serve")
	}
	if a.Metadata.TestAccuracy <= 0 || a.Metadata.TestAccuracy > 1 {
		add("metadata.test_accuracy must be a proportion in (0, 1], got %g", a.Metadata.TestAccuracy)
	}
	if a.Metadata.TestBaseRate <= 0 || a.Metadata.TestBaseRate > 1 {
		add("metadata.test_base_rate must be a proportion in (0, 1]: accuracy without "+
			"the constant-guess benchmark is not a measurement, got %g", a.Metadata.TestBaseRate)
	}
	if len(errs) > 0 {
		return fmt.Errorf("model artifact invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// LoadArtifact reads and validates an artifact, computing its digest.
func LoadArtifact(path string) (*Artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mlinfer: read %s: %w", path, err)
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("mlinfer: parse %s: %w", path, err)
	}
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("mlinfer: %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	a.Digest = hex.EncodeToString(sum[:])
	a.Path = path
	return &a, nil
}

// Model is a loaded, ready-to-serve artifact.
type Model struct {
	art *Artifact
	// index maps a feature name to its position, so callers can pass a snapshot
	// rather than a positional vector.
	index map[string]int
}

// NewModel wraps a validated artifact.
func NewModel(a *Artifact) *Model {
	idx := make(map[string]int, len(a.Features))
	for i, f := range a.Features {
		idx[f] = i
	}
	return &Model{art: a, index: idx}
}

// Artifact returns the underlying artifact.
func (m *Model) Artifact() *Artifact { return m.art }

// ID returns "model_id@version".
func (m *Model) ID() string { return m.art.ModelID + "@" + m.art.Version }

// Features returns the ordered feature list.
func (m *Model) Features() []string { return m.art.Features }

// Predict runs inference on a feature snapshot.
//
// It returns an error rather than a default distribution when a feature is
// missing or cold: guessing here is how a model silently starts serving a
// different function from the one that was trained.
func (m *Model) Predict(snap domain.FeatureSnapshot) (domain.Distribution, []domain.FeatureContribution, error) {
	vec, err := snap.Vector(m.art.Features)
	if err != nil {
		return domain.Distribution{}, nil, err
	}
	return m.PredictVector(vec)
}

// PredictVector runs inference on a raw, ordered feature vector.
func (m *Model) PredictVector(vec []float64) (domain.Distribution, []domain.FeatureContribution, error) {
	a := m.art
	if len(vec) != len(a.Features) {
		return domain.Distribution{}, nil, fmt.Errorf("mlinfer: expected %d features, got %d", len(a.Features), len(vec))
	}
	z := make([]float64, len(vec))
	for i, v := range vec {
		z[i] = (v - a.Mean[i]) / a.Std[i]
		// Clip standardised inputs. An extreme outlier — a bad tick that slipped
		// through validation, or a genuine six-sigma move — should not be
		// extrapolated far outside the training distribution.
		z[i] = domain.Clamp(z[i], -6, 6)
	}

	var scores []float64
	var contribs []domain.FeatureContribution
	switch a.Family {
	case domain.FamilyLogistic:
		scores = make([]float64, len(domain.AllOutcomes))
		for c := range domain.AllOutcomes {
			s := a.Intercepts[c]
			for i, zi := range z {
				s += a.Coefficients[c][i] * zi
			}
			scores[c] = s
		}
		// Attribution against the FLAT class: the contribution of a feature to
		// "up rather than flat" is the interpretable quantity.
		up, flat := 0, 1
		for i, zi := range z {
			w := a.Coefficients[up][i] - a.Coefficients[flat][i]
			contribs = append(contribs, domain.FeatureContribution{
				Feature: a.Features[i], Value: vec[i], Weight: w, Contribution: w * zi,
			})
		}
	case domain.FamilyStumps:
		scores = append([]float64(nil), a.BaseScore...)
		perFeature := make([]float64, len(z))
		for c, group := range a.Stumps {
			for _, s := range group {
				v := s.Left
				if z[s.Feature] > s.Threshold {
					v = s.Right
				}
				scores[c] += v
				if c == 0 {
					perFeature[s.Feature] += v
				} else if c == 1 {
					perFeature[s.Feature] -= v
				}
			}
		}
		for i := range z {
			contribs = append(contribs, domain.FeatureContribution{
				Feature: a.Features[i], Value: vec[i], Weight: 0, Contribution: perFeature[i],
			})
		}
	default:
		return domain.Distribution{}, nil, fmt.Errorf("mlinfer: unsupported family %q", a.Family)
	}

	dist := softmax(scores)
	dist = a.Calibration.apply(dist)
	sort.SliceStable(contribs, func(i, j int) bool {
		return math.Abs(contribs[i].Contribution) > math.Abs(contribs[j].Contribution)
	})
	return dist, contribs, nil
}

func softmax(scores []float64) domain.Distribution {
	max := scores[0]
	for _, s := range scores[1:] {
		if s > max {
			max = s
		}
	}
	exp := make([]float64, len(scores))
	sum := 0.0
	for i, s := range scores {
		exp[i] = math.Exp(s - max)
		sum += exp[i]
	}
	if sum == 0 {
		return domain.NewDistribution(1, 1, 1)
	}
	return domain.NewDistribution(exp[0], exp[1], exp[2])
}

// apply post-processes a distribution according to the calibration method.
func (c Calibration) apply(d domain.Distribution) domain.Distribution {
	switch c.Method {
	case "temperature":
		if c.Temperature > 0 && c.Temperature != 1 {
			return d.Sharpen(c.Temperature)
		}
		return d
	case "vector":
		p := []float64{d.Up, d.Flat, d.Down}
		out := make([]float64, 3)
		for i := range p {
			// Operate in log space so the transform stays a valid reweighting.
			lp := math.Log(math.Max(p[i], 1e-12))
			out[i] = math.Exp(c.Scale[i]*lp + c.Shift[i])
		}
		return domain.NewDistribution(out[0], out[1], out[2])
	default:
		return d
	}
}

// Registry holds loaded models and implements the shadow/canary/active staging
// described in ADR-006.
type Registry struct {
	mu       sync.RWMutex
	models   map[string]*Model // "model_id@version"
	active   map[string]*Model // model_id -> active model
	canary   map[string]*Model
	shadow   map[string][]*Model
	fraction map[string]float64
}

// NewRegistry returns an empty model registry.
func NewRegistry() *Registry {
	return &Registry{
		models: map[string]*Model{}, active: map[string]*Model{},
		canary: map[string]*Model{}, shadow: map[string][]*Model{},
		fraction: map[string]float64{},
	}
}

// Register adds a model at a stage.
func (r *Registry) Register(m *Model, stage domain.ModelStage, canaryFraction float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[m.ID()] = m
	id := m.art.ModelID
	switch stage {
	case domain.StageActive:
		r.active[id] = m
	case domain.StageCanary:
		r.canary[id] = m
		r.fraction[id] = domain.Clamp01(canaryFraction)
	case domain.StageShadow:
		r.shadow[id] = append(r.shadow[id], m)
	}
}

// Select returns the model that should serve a given symbol.
//
// Canary routing is keyed by a hash of the ticker so a symbol does not flap
// between models within a session — which would make its prediction history
// uninterpretable.
func (r *Registry) Select(modelID string, ticker domain.Ticker) (*Model, domain.ModelStage, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	can, hasCanary := r.canary[modelID]
	act, hasActive := r.active[modelID]
	if hasCanary && r.fraction[modelID] > 0 {
		if stableFraction(ticker) < r.fraction[modelID] {
			return can, domain.StageCanary, true
		}
	}
	if hasActive {
		return act, domain.StageActive, true
	}
	if hasCanary {
		return can, domain.StageCanary, true
	}
	return nil, "", false
}

// Shadows returns the shadow models for a model id.
func (r *Registry) Shadows(modelID string) []*Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*Model(nil), r.shadow[modelID]...)
}

// All returns every registered model, sorted by id.
func (r *Registry) All() []*Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Model, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Promote moves a model to active, demoting the incumbent to retired.
func (r *Registry) Promote(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.models[id]
	if !ok {
		return fmt.Errorf("mlinfer: unknown model %q", id)
	}
	r.active[m.art.ModelID] = m
	delete(r.canary, m.art.ModelID)
	return nil
}

func stableFraction(t domain.Ticker) float64 {
	sum := sha256.Sum256([]byte(t))
	v := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	return float64(v) / float64(^uint32(0))
}

// LoadDir loads every *.json artifact in dir that is not a *.meta.json or
// *.golden.json sidecar. The newest version of each model id becomes active and
// the rest are registered as shadows, which gives a sensible default without
// requiring a database in embedded mode.
func LoadDir(dir string) (*Registry, []error) {
	reg := NewRegistry()
	var errs []error
	entries, err := os.ReadDir(dir)
	if err != nil {
		return reg, []error{fmt.Errorf("mlinfer: read %s: %w", dir, err)}
	}
	names := []string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") {
			continue
		}
		if strings.HasSuffix(n, ".meta.json") || strings.HasSuffix(n, ".golden.json") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	byModel := map[string][]*Model{}
	for _, n := range names {
		a, err := LoadArtifact(filepath.Join(dir, n))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m := NewModel(a)
		byModel[a.ModelID] = append(byModel[a.ModelID], m)
	}
	for id, list := range byModel {
		sort.Slice(list, func(i, j int) bool { return list[i].art.Version < list[j].art.Version })
		for i, m := range list {
			stage := domain.StageShadow
			if i == len(list)-1 {
				stage = domain.StageActive
			}
			reg.Register(m, stage, 0)
		}
		_ = id
	}
	return reg, errs
}

// ProvisionalHealth turns an artifact's offline report card into a health
// record, so a newly promoted model can serve before any live prediction has
// resolved.
//
// It is deliberately strict in one direction only: a model that did not beat
// its own base rate offline is marked unhealthy here and will be treated as
// such by the risk engine. A model that did beat it is allowed to start, and
// the first real evaluation sweep replaces this record with live evidence.
func (a *Artifact) ProvisionalHealth(stage domain.ModelStage, now time.Time) domain.ModelHealth {
	h := domain.ModelHealth{
		ModelID: a.ModelID, ModelVersion: a.Version, Stage: stage, AsOf: now,
		Accuracy: a.Metadata.TestAccuracy, BrierScore: a.Metadata.TestBrier,
		Calibration: 1 - a.Calibration.ECE, Drift: domain.DriftNone,
		Samples: 0, Provisional: true,
		LastEvaluatedAt: a.Metadata.TrainedAt,
		Healthy:         true,
	}
	if a.Metadata.TestAccuracy <= a.Metadata.TestBaseRate {
		h.Healthy = false
		h.Issues = append(h.Issues, fmt.Sprintf(
			"offline test accuracy %.4f does not beat the base rate %.4f: the model is no better than a constant guess",
			a.Metadata.TestAccuracy, a.Metadata.TestBaseRate))
	}
	if a.Calibration.MaxDeviation > 0.10 {
		h.Healthy = false
		h.Issues = append(h.Issues, fmt.Sprintf(
			"offline maximum calibration deviation %.4f exceeds the 0.10 promotion gate", a.Calibration.MaxDeviation))
	}
	return h
}

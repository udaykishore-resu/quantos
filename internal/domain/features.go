package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TechnicalFeature is one named, timestamped numeric feature.
type TechnicalFeature struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	// AsOf is the domain timestamp of the newest input that produced this
	// value. The leakage guard compares this against the decision time.
	AsOf time.Time `json:"as_of"`
	// Window is the lookback in bars, 0 for stateless features.
	Window int `json:"window,omitempty"`
	// Warm reports whether the underlying indicator has seen enough data to be
	// meaningful. Cold features are never fed to models.
	Warm bool `json:"warm"`
}

// FeatureSnapshot is the complete, immutable record of everything a downstream
// model saw for one symbol at one instant. It is the unit of reproducibility:
// a prediction stores the snapshot's Hash, and a stored snapshot plus a model
// artifact digest is sufficient to recompute the prediction exactly.
type FeatureSnapshot struct {
	Ticker   Ticker             `json:"ticker"`
	AsOf     time.Time          `json:"as_of"`
	Interval Interval           `json:"interval"`
	Values   map[string]float64 `json:"values"`
	Warm     map[string]bool    `json:"warm"`
	// Price context carried alongside so consumers never need a second lookup.
	Last      float64 `json:"last"`
	Bid       float64 `json:"bid"`
	Ask       float64 `json:"ask"`
	Volume    float64 `json:"volume"`
	SpreadBps float64 `json:"spread_bps"`
	// Stale is set by the freshness gate; stale snapshots may be displayed but
	// may never produce a new signal (governance rule G-5).
	Stale         bool      `json:"stale"`
	StaleReason   string    `json:"stale_reason,omitempty"`
	LastTickAt    time.Time `json:"last_tick_at"`
	SchemaVersion int       `json:"schema_version"`
	Hash          string    `json:"hash"`
}

// Get returns a feature value and whether it is present and warm.
func (s FeatureSnapshot) Get(name string) (float64, bool) {
	v, ok := s.Values[name]
	if !ok {
		return 0, false
	}
	if warm, has := s.Warm[name]; has && !warm {
		return v, false
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// MustGet returns the value or fallback when absent or cold.
func (s FeatureSnapshot) MustGet(name string, fallback float64) float64 {
	if v, ok := s.Get(name); ok {
		return v
	}
	return fallback
}

// Names returns feature names in deterministic order.
func (s FeatureSnapshot) Names() []string {
	out := make([]string, 0, len(s.Values))
	for k := range s.Values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Vector extracts features in the exact order given, reporting the first
// missing or cold name. Model inference uses this so that feature ordering can
// never silently drift (ADR-006).
func (s FeatureSnapshot) Vector(names []string) ([]float64, error) {
	out := make([]float64, len(names))
	for i, n := range names {
		v, ok := s.Get(n)
		if !ok {
			return nil, fmt.Errorf("feature %q missing or cold in snapshot for %s", n, s.Ticker)
		}
		out[i] = v
	}
	return out, nil
}

// ComputeHash produces a stable content hash of the snapshot's decision-relevant
// contents. Float formatting is fixed to 10 significant digits so that the hash
// is stable across platforms while remaining sensitive to real changes.
func (s *FeatureSnapshot) ComputeHash() string {
	var b strings.Builder
	b.WriteString(string(s.Ticker))
	b.WriteByte('|')
	b.WriteString(s.AsOf.UTC().Format(time.RFC3339Nano))
	b.WriteByte('|')
	b.WriteString(string(s.Interval))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(s.SchemaVersion))
	for _, n := range s.Names() {
		b.WriteByte('|')
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(strconv.FormatFloat(s.Values[n], 'g', 10, 64))
		if !s.Warm[n] {
			b.WriteString("~cold")
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Finalize computes and stores the hash. Callers must not mutate a snapshot
// after Finalize.
func (s *FeatureSnapshot) Finalize() {
	if s.SchemaVersion == 0 {
		s.SchemaVersion = FeatureSchemaVersion
	}
	s.Hash = s.ComputeHash()
}

// MaxFeatureLookbackBars is the longest window any feature in the canonical set
// depends on (the 200-period moving average).
//
// It is here so that a warm-up shorter than the longest lookback can be
// rejected as a configuration error instead of failing silently. A platform
// warmed for fewer bars than its slowest indicator needs runs with that
// indicator permanently cold: every rule referencing it is skipped for ever,
// the strategies that depend on those rules never fire, and nothing reports a
// problem — the platform simply produces nothing and looks healthy doing it.
const MaxFeatureLookbackBars = 200

// FeatureSchemaVersion is bumped whenever the meaning of an existing feature
// name changes. Adding a new feature does not require a bump.
const FeatureSchemaVersion = 1

// Canonical feature names. Keeping them as constants prevents the classic
// "rsi_14" vs "RSI14" divergence between the rule engine and the model.
const (
	FeatLast         = "last"
	FeatSMA20        = "sma_20"
	FeatSMA50        = "sma_50"
	FeatSMA200       = "sma_200"
	FeatEMA9         = "ema_9"
	FeatEMA21        = "ema_21"
	FeatVWAP         = "vwap"
	FeatVWAPDist     = "vwap_dist_bps"
	FeatRSI14        = "rsi_14"
	FeatMACD         = "macd"
	FeatMACDSignal   = "macd_signal"
	FeatMACDHist     = "macd_hist"
	FeatATR14        = "atr_14"
	FeatATRPct       = "atr_pct"
	FeatBBUpper      = "bb_upper"
	FeatBBLower      = "bb_lower"
	FeatBBWidth      = "bb_width"
	FeatBBPercentB   = "bb_percent_b"
	FeatADX14        = "adx_14"
	FeatPlusDI       = "plus_di_14"
	FeatMinusDI      = "minus_di_14"
	FeatRelStrength  = "rel_strength_20"
	FeatMomentum10   = "momentum_10"
	FeatMomentum60   = "momentum_60"
	FeatRetLog1      = "ret_log_1"
	FeatRetLog5      = "ret_log_5"
	FeatVolRatio     = "volume_ratio_20"
	FeatVolZScore    = "volume_z_20"
	FeatRealizedVol  = "realized_vol_20"
	FeatVolOfVol     = "vol_of_vol_20"
	FeatSupport      = "support"
	FeatResistance   = "resistance"
	FeatDistSupport  = "dist_support_bps"
	FeatDistResist   = "dist_resistance_bps"
	FeatBreakoutUp   = "breakout_up"
	FeatBreakoutDown = "breakout_down"
	FeatGapPct       = "gap_pct"
	FeatTrendSlope   = "trend_slope_20"
	FeatZScore20     = "zscore_20"
)

// ModelFeatureSet is the ordered feature list consumed by the shipped direction
// model. mlinfer asserts an artifact declares exactly this list.
var ModelFeatureSet = []string{
	FeatRetLog1,
	FeatRetLog5,
	FeatMomentum10,
	FeatMomentum60,
	FeatRSI14,
	FeatMACDHist,
	FeatVWAPDist,
	FeatBBPercentB,
	FeatADX14,
	FeatRelStrength,
	FeatVolRatio,
	FeatVolZScore,
	FeatATRPct,
	FeatRealizedVol,
	FeatZScore20,
	FeatTrendSlope,
}

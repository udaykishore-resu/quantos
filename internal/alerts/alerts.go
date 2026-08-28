// Package alerts detects notable transitions and emits deduplicated alerts.
//
// Every alert carries a deterministic DedupKey derived from what it is about
// and a time bucket, so redelivery and replay cannot produce a duplicate
// (requirement §36). The structured Before/After payload is what the dashboard
// renders; the Message string is for humans and is never parsed.
package alerts

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Config parameterises alert generation.
type Config struct {
	// DedupBucket collapses repeated firings of the same condition.
	DedupBucket time.Duration
	// Cooldown suppresses the same alert type for the same symbol.
	Cooldown time.Duration
	// MaxPerTickerPerHour is a hard ceiling that protects the user from a
	// symbol that oscillates around a threshold.
	MaxPerTickerPerHour int

	ProbabilityDelta   float64
	VolumeAcceleration float64
	VolatilitySpike    float64

	// Enabled restricts which alert types fire. Empty means all.
	Enabled []domain.AlertType

	Clock   obs.Clock
	Metrics *obs.Metrics
}

// DefaultConfig returns sensible parameters.
func DefaultConfig() Config {
	return Config{
		DedupBucket: time.Minute, Cooldown: 5 * time.Minute,
		MaxPerTickerPerHour: 12, ProbabilityDelta: 0.08,
		VolumeAcceleration: 2.5, VolatilitySpike: 2.0,
		Clock: obs.SystemClock{},
	}
}

// Input is the current and previous state an evaluation compares.
type Input struct {
	Ticker domain.Ticker
	Now    time.Time

	Snapshot       domain.FeatureSnapshot
	PrevSnapshot   *domain.FeatureSnapshot
	Prediction     *domain.Prediction
	PrevPrediction *domain.Prediction
	Regime         domain.MarketRegime
	Risk           *domain.RiskAssessment
	Signal         *domain.Signal
	News           *domain.NewsEvent
	Invalidated    *domain.Signal
	Drift          *domain.DriftReport
	Stale          bool
	StaleReason    string
	CorrelationID  string
}

// Engine detects conditions and emits alerts.
type Engine struct {
	cfg Config

	mu      sync.Mutex
	lastAt  map[string]time.Time // ticker|type -> last emission
	hourly  map[domain.Ticker][]time.Time
	emitted map[string]bool // dedup key -> emitted
	enabled map[domain.AlertType]bool
}

// NewEngine builds an alert engine.
func NewEngine(cfg Config) *Engine {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.DedupBucket <= 0 {
		cfg.DedupBucket = time.Minute
	}
	e := &Engine{
		cfg: cfg, lastAt: map[string]time.Time{},
		hourly: map[domain.Ticker][]time.Time{}, emitted: map[string]bool{},
	}
	if len(cfg.Enabled) > 0 {
		e.enabled = map[domain.AlertType]bool{}
		for _, t := range cfg.Enabled {
			e.enabled[t] = true
		}
	}
	return e
}

// Evaluate returns every alert the current state warrants, already deduplicated
// and rate limited.
func (e *Engine) Evaluate(in Input) []domain.Alert {
	now := in.Now
	if now.IsZero() {
		now = e.cfg.Clock.Now()
	}
	var out []domain.Alert
	add := func(a domain.Alert) {
		if a.Type == "" {
			return
		}
		if e.enabled != nil && !e.enabled[a.Type] {
			return
		}
		a.CreatedAt = now
		a.Ticker = in.Ticker
		a.Regime = in.Regime.Regime
		a.CorrelationID = in.CorrelationID
		a.Disclaimer = domain.Disclaimer
		if in.Risk != nil {
			a.RiskLevel = in.Risk.Level
		}
		if a.DedupKey == "" {
			a.DedupKey = bus.EffectKey("alert", string(in.Ticker), string(a.Type), bus.BucketTime(now, e.cfg.DedupBucket))
		}
		a.ID = bus.NewEventID(now)
		if e.admit(a, now) {
			out = append(out, a)
		}
	}

	// --- Data staleness ----------------------------------------------------
	if in.Stale {
		add(domain.Alert{
			Type: domain.AlertDataStale, Severity: domain.SeverityWarning,
			Title:   fmt.Sprintf("%s market data is stale", in.Ticker),
			Message: in.StaleReason + ". Signal generation is suspended for this instrument.",
			Status:  "SUSPENDED",
		})
		// Nothing else is trustworthy while data is stale, so stop here.
		return out
	}

	// --- VWAP crossover ----------------------------------------------------
	if in.PrevSnapshot != nil {
		prev, okP := in.PrevSnapshot.Get(domain.FeatVWAPDist)
		cur, okC := in.Snapshot.Get(domain.FeatVWAPDist)
		if okP && okC && prev*cur < 0 {
			dir := "above"
			sev := domain.SeverityNotice
			if cur < 0 {
				dir = "below"
				sev = domain.SeverityWarning
			}
			add(domain.Alert{
				Type: domain.AlertVWAPCross, Severity: sev,
				Title:   fmt.Sprintf("%s crossed %s session VWAP", in.Ticker, dir),
				Message: fmt.Sprintf("Distance from VWAP moved from %.0f bps to %.0f bps.", prev, cur),
				Before:  map[string]float64{"vwap_dist_bps": prev},
				After:   map[string]float64{"vwap_dist_bps": cur},
				Status:  "PAPER-TRADING WATCH",
			})
		}
	}

	// --- Volume acceleration ----------------------------------------------
	if vr, ok := in.Snapshot.Get(domain.FeatVolRatio); ok && vr >= e.cfg.VolumeAcceleration {
		add(domain.Alert{
			Type: domain.AlertVolumeAcceleration, Severity: domain.SeverityNotice,
			Title:   fmt.Sprintf("%s volume acceleration", in.Ticker),
			Message: fmt.Sprintf("Volume is %.1f times its twenty-period average.", vr),
			After:   map[string]float64{"volume_ratio_20": vr},
			Status:  "PAPER-TRADING WATCH",
		})
	}

	// --- Breakout ----------------------------------------------------------
	if up, ok := in.Snapshot.Get(domain.FeatBreakoutUp); ok && up > 0 {
		add(domain.Alert{
			Type: domain.AlertBreakout, Severity: domain.SeverityNotice,
			Title:   fmt.Sprintf("%s broke above its twenty-bar range", in.Ticker),
			Message: "Close beyond the prior twenty-bar high with volume confirmation.",
			After: map[string]float64{
				"breakout_up":     1,
				"volume_ratio_20": in.Snapshot.MustGet(domain.FeatVolRatio, 0),
			},
			Status: "PAPER-TRADING WATCH",
		})
	}
	if dn, ok := in.Snapshot.Get(domain.FeatBreakoutDown); ok && dn > 0 {
		add(domain.Alert{
			Type: domain.AlertBreakout, Severity: domain.SeverityWarning,
			Title:   fmt.Sprintf("%s broke below its twenty-bar range", in.Ticker),
			Message: "Close beyond the prior twenty-bar low with volume confirmation.",
			After: map[string]float64{
				"breakout_down":   1,
				"volume_ratio_20": in.Snapshot.MustGet(domain.FeatVolRatio, 0),
			},
			Status: "PAPER-TRADING WATCH",
		})
	}

	// --- Volatility spike --------------------------------------------------
	if in.PrevSnapshot != nil {
		prev, okP := in.PrevSnapshot.Get(domain.FeatRealizedVol)
		cur, okC := in.Snapshot.Get(domain.FeatRealizedVol)
		if okP && okC && prev > 0 && cur/prev >= e.cfg.VolatilitySpike {
			add(domain.Alert{
				Type: domain.AlertVolatilitySpike, Severity: domain.SeverityWarning,
				Title:   fmt.Sprintf("%s volatility spike", in.Ticker),
				Message: fmt.Sprintf("Realised volatility rose from %.2f to %.2f.", prev, cur),
				Before:  map[string]float64{"realized_vol_20": prev},
				After:   map[string]float64{"realized_vol_20": cur},
				Status:  "RISK REVIEW",
			})
		}
	}

	// --- Momentum shift ----------------------------------------------------
	if in.PrevSnapshot != nil {
		prev, okP := in.PrevSnapshot.Get(domain.FeatMACDHist)
		cur, okC := in.Snapshot.Get(domain.FeatMACDHist)
		if okP && okC && prev*cur < 0 {
			dir := "positive"
			if cur < 0 {
				dir = "negative"
			}
			add(domain.Alert{
				Type: domain.AlertMomentumShift, Severity: domain.SeverityInfo,
				Title:   fmt.Sprintf("%s momentum turned %s", in.Ticker, dir),
				Message: fmt.Sprintf("MACD histogram moved from %.2f to %.2f.", prev, cur),
				Before:  map[string]float64{"macd_hist": prev},
				After:   map[string]float64{"macd_hist": cur},
			})
		}
	}

	// --- Probability shift -------------------------------------------------
	if in.Prediction != nil && in.PrevPrediction != nil {
		cur := in.Prediction.Primary()
		prev, ok := in.PrevPrediction.Scenario(cur.Horizon)
		if ok {
			delta := cur.Dist.Up - prev.Dist.Up
			if absf(delta) >= e.cfg.ProbabilityDelta {
				sev := domain.SeverityNotice
				if absf(delta) >= e.cfg.ProbabilityDelta*2 {
					sev = domain.SeverityWarning
				}
				add(domain.Alert{
					Type: domain.AlertProbabilityShift, Severity: sev,
					Title: fmt.Sprintf("%s %s upside probability moved", in.Ticker, cur.Horizon),
					Message: fmt.Sprintf("Probability of an upward outcome moved from %.2f to %.2f over the %s horizon.",
						prev.Dist.Up, cur.Dist.Up, cur.Horizon),
					Before:       map[string]float64{"p_up": prev.Dist.Up, "confidence": prev.Confidence},
					After:        map[string]float64{"p_up": cur.Dist.Up, "confidence": cur.Confidence},
					PredictionID: in.Prediction.ID,
					Status:       "PAPER-TRADING WATCH",
				})
			}
		}
	}

	// --- News --------------------------------------------------------------
	if n := in.News; n != nil && n.Material() {
		sev := domain.SeverityNotice
		if n.Materiality == domain.MaterialityHigh {
			sev = domain.SeverityWarning
		}
		add(domain.Alert{
			Type: domain.AlertNewsEvent, Severity: sev,
			DedupKey: bus.EffectKey("alert", "news", n.ID),
			Title:    fmt.Sprintf("%s: %s news", in.Ticker, n.Category),
			Message:  n.Headline,
			After: map[string]float64{
				"sentiment_score":   n.SentimentScore,
				"materiality_score": n.MaterialityScore,
				"confidence":        n.Confidence,
			},
			Status: "NEWS REVIEW",
		})
	}

	// --- Signal lifecycle --------------------------------------------------
	if s := in.Signal; s != nil {
		add(domain.Alert{
			Type: domain.AlertSignalGenerated, Severity: domain.SeverityNotice,
			DedupKey: bus.EffectKey("alert", "signal", s.ID),
			Title:    fmt.Sprintf("%s %s paper signal", in.Ticker, s.Side),
			Message: fmt.Sprintf("Strategy %s produced a %s paper signal with strength %.1f and confidence %.2f.",
				s.StrategyID, s.Side, s.Strength, s.Confidence),
			After: map[string]float64{
				"strength": s.Strength, "confidence": s.Confidence,
				"entry_reference": s.EntryReference, "stop_reference": s.StopReference,
			},
			SignalID: s.ID, PredictionID: s.PredictionID,
			Status: "PAPER-TRADING SIGNAL",
		})
	}
	if s := in.Invalidated; s != nil {
		add(domain.Alert{
			Type: domain.AlertSignalInvalidated, Severity: domain.SeverityWarning,
			DedupKey: bus.EffectKey("alert", "invalidated", s.ID),
			Title:    fmt.Sprintf("%s signal invalidated", in.Ticker),
			Message:  fmt.Sprintf("%s: %s", s.InvalidationKind, s.InvalidationNote),
			SignalID: s.ID,
			Status:   "INVALIDATED",
		})
	}

	// --- Risk block --------------------------------------------------------
	if r := in.Risk; r != nil && r.Decision == domain.RiskBlock {
		add(domain.Alert{
			Type: domain.AlertRiskBlock, Severity: domain.SeverityWarning,
			Title:   fmt.Sprintf("%s blocked by the risk engine", in.Ticker),
			Message: joinFirst(r.Blockers, 2),
			After:   map[string]float64{"risk_score": r.Score},
			Status:  "BLOCKED",
		})
	}

	// --- Regime change -----------------------------------------------------
	if in.Regime.Changed {
		add(domain.Alert{
			Type: domain.AlertRegimeChange, Severity: domain.SeverityWarning,
			DedupKey: bus.EffectKey("alert", "regime", string(in.Regime.Regime), bus.BucketTime(now, e.cfg.DedupBucket)),
			Title:    fmt.Sprintf("Market regime changed to %s", in.Regime.Regime),
			Message: fmt.Sprintf("Regime moved from %s to %s at confidence %.2f.",
				in.Regime.PreviousRegime, in.Regime.Regime, in.Regime.Confidence),
			Before: map[string]float64{},
			After: map[string]float64{
				"confidence": in.Regime.Confidence,
				"volatility": in.Regime.Volatility,
				"breadth":    in.Regime.Breadth,
			},
			Status: "REGIME CHANGE",
		})
	}

	// --- Model drift -------------------------------------------------------
	if d := in.Drift; d != nil && d.Severity != domain.DriftNone && d.Severity != domain.DriftMild {
		sev := domain.SeverityWarning
		if d.Severity == domain.DriftSevere {
			sev = domain.SeverityCritical
		}
		add(domain.Alert{
			Type: domain.AlertModelDrift, Severity: sev,
			DedupKey: bus.EffectKey("alert", "drift", d.ModelVersion, string(d.Severity), bus.BucketTime(now, time.Hour)),
			Title:    fmt.Sprintf("Model %s drift: %s", d.ModelVersion, d.Severity),
			Message:  d.Recommendation,
			After: map[string]float64{
				"max_feature_psi":  d.MaxFeaturePSI,
				"accuracy_delta":   d.AccuracyDelta,
				"prediction_drift": d.PredictionDrift,
			},
			Status: "MODEL HEALTH",
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) > severityRank(out[j].Severity)
	})
	return out
}

// admit applies dedup, cooldown and the hourly ceiling.
func (e *Engine) admit(a domain.Alert, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.emitted[a.DedupKey] {
		e.count("duplicate", a)
		return false
	}
	key := string(a.Ticker) + "|" + string(a.Type)
	if last, ok := e.lastAt[key]; ok && now.Sub(last) < e.cfg.Cooldown && !alwaysEmit(a.Type) {
		e.count("cooldown", a)
		return false
	}
	if e.cfg.MaxPerTickerPerHour > 0 {
		times := e.hourly[a.Ticker]
		kept := times[:0]
		for _, t := range times {
			if now.Sub(t) < time.Hour {
				kept = append(kept, t)
			}
		}
		if len(kept) >= e.cfg.MaxPerTickerPerHour && !alwaysEmit(a.Type) {
			e.hourly[a.Ticker] = kept
			e.count("hourly_cap", a)
			return false
		}
		e.hourly[a.Ticker] = append(kept, now)
	}
	e.emitted[a.DedupKey] = true
	e.lastAt[key] = now
	if len(e.emitted) > 1<<16 {
		e.emitted = map[string]bool{a.DedupKey: true}
	}
	if e.cfg.Metrics != nil {
		e.cfg.Metrics.AlertsGenerated.Inc(string(a.Type), string(a.Severity))
	}
	return true
}

func (e *Engine) count(reason string, a domain.Alert) {
	if e.cfg.Metrics != nil {
		e.cfg.Metrics.AlertsSuppressed.Inc(string(a.Type), reason)
	}
}

// alwaysEmit lists the alert types that bypass cooldown and the hourly cap.
// These are lifecycle facts, not observations: suppressing them would leave a
// user with a signal they never saw invalidated.
func alwaysEmit(t domain.AlertType) bool {
	switch t {
	case domain.AlertSignalGenerated, domain.AlertSignalInvalidated,
		domain.AlertRegimeChange, domain.AlertModelDrift, domain.AlertDataStale:
		return true
	}
	return false
}

func severityRank(s domain.AlertSeverity) int {
	switch s {
	case domain.SeverityCritical:
		return 3
	case domain.SeverityWarning:
		return 2
	case domain.SeverityNotice:
		return 1
	default:
		return 0
	}
}

func joinFirst(ss []string, n int) string {
	if len(ss) == 0 {
		return "no reason recorded"
	}
	if len(ss) > n {
		ss = ss[:n]
	}
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

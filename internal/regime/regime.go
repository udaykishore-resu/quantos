// Package regime classifies the prevailing market environment.
//
// The regime is not a forecast. It is a description of observed conditions that
// conditions which signals the platform treats as meaningful: a mean-reversion
// rule that works in a range is actively harmful in a strong trend, and the
// rule engine uses the regime to decide which rules are even eligible.
package regime

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Config parameterises regime detection.
type Config struct {
	// Benchmarks drive trend and momentum; the first is the primary index.
	Benchmarks []domain.Ticker
	// VolatilityRef is the implied-volatility reference (VIX).
	VolatilityRef domain.Ticker
	// RatesRef and DollarRef provide the risk-appetite cross-checks.
	RatesRef  domain.Ticker
	DollarRef domain.Ticker
	CreditRef domain.Ticker

	MinConfidence    float64
	Hysteresis       time.Duration
	VIXHigh          float64
	VIXLow           float64
	TrendThreshold   float64
	BreadthThreshold float64

	Clock obs.Clock
}

// DefaultConfig returns sensible detection parameters.
func DefaultConfig() Config {
	return Config{
		Benchmarks:       []domain.Ticker{"SPY", "QQQ"},
		VolatilityRef:    "VIX",
		RatesRef:         "TLT",
		DollarRef:        "DXY",
		CreditRef:        "HYG",
		MinConfidence:    0.45,
		Hysteresis:       5 * time.Minute,
		VIXHigh:          24,
		VIXLow:           14,
		TrendThreshold:   0.0004,
		BreadthThreshold: 0.15,
		Clock:            obs.SystemClock{},
	}
}

// Input is everything the engine needs for one classification. Assembling it is
// the caller's job, which keeps the engine a pure function of its inputs and
// therefore trivially testable and reproducible.
type Input struct {
	AsOf      time.Time
	Snapshots map[domain.Ticker]domain.FeatureSnapshot
	// VIXLevel is the implied-volatility index level. When the reference symbol
	// is present in Snapshots this is derived from it.
	VIXLevel float64
	// Breadth is (advancers - decliners) / total, in [-1, 1].
	Breadth float64
	// Volume is market-wide volume relative to its own 20-period average.
	VolumeRatio float64
	// Sectors carries relative sector performance for rotation analysis.
	Sectors []domain.SectorPerformance
}

// Engine classifies market regimes with hysteresis.
type Engine struct {
	cfg Config

	mu           sync.RWMutex
	current      domain.MarketRegime
	candidate    domain.Regime
	candidateAt  time.Time
	lastChangeAt time.Time
	history      []domain.MarketRegime
}

// NewEngine builds a regime engine.
func NewEngine(cfg Config) *Engine {
	if len(cfg.Benchmarks) == 0 {
		cfg.Benchmarks = []domain.Ticker{"SPY"}
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.VIXHigh <= cfg.VIXLow {
		cfg.VIXHigh, cfg.VIXLow = 24, 14
	}
	return &Engine{cfg: cfg, current: domain.MarketRegime{Regime: domain.RegimeUndefined}}
}

// Version identifies the classification logic, and is stamped on every output
// so a historical regime can be attributed to the code that produced it.
const Version = "regime-v1"

// Classify produces a regime from the given inputs. It is deterministic: the
// same Input always yields the same output apart from the hysteresis state,
// which is carried explicitly on the engine.
func (e *Engine) Classify(in Input) domain.MarketRegime {
	primary := e.cfg.Benchmarks[0]
	ps, hasPrimary := in.Snapshots[primary]

	inputs := map[string]float64{}
	var evidence []string

	// --- Trend --------------------------------------------------------------
	trend, trendConf := 0.0, 0.0
	if hasPrimary {
		slope := ps.MustGet(domain.FeatTrendSlope, 0) / 10000 // back to fraction
		sma20 := ps.MustGet(domain.FeatSMA20, 0)
		sma50 := ps.MustGet(domain.FeatSMA50, 0)
		last := ps.MustGet(domain.FeatLast, 0)
		adx := ps.MustGet(domain.FeatADX14, 0)
		mom := ps.MustGet(domain.FeatMomentum60, 0)

		inputs["primary_slope"] = slope
		inputs["primary_adx"] = adx
		inputs["primary_momentum_60"] = mom
		if sma20 > 0 && sma50 > 0 && last > 0 {
			inputs["sma20_over_sma50"] = sma20/sma50 - 1
			inputs["price_over_sma50"] = last/sma50 - 1
		}

		// Trend strength combines slope direction with ADX magnitude, because
		// slope alone cannot distinguish a drift from a trend.
		dirScore := math.Tanh(slope / math.Max(e.cfg.TrendThreshold, 1e-9))
		strength := domain.Clamp01((adx - 15) / 25) // ADX 15 -> 0, 40 -> 1
		trend = dirScore * strength
		trendConf = strength
		if sma20 > 0 && sma50 > 0 {
			if (sma20 > sma50 && trend > 0) || (sma20 < sma50 && trend < 0) {
				trendConf = math.Min(1, trendConf+0.2)
				evidence = append(evidence, fmt.Sprintf("%s 20/50 moving averages agree with the trend direction", primary))
			}
		}
		if adx >= 25 {
			evidence = append(evidence, fmt.Sprintf("%s ADX at %.1f indicates a directional market", primary, adx))
		} else if adx > 0 {
			evidence = append(evidence, fmt.Sprintf("%s ADX at %.1f is below the trending threshold of 25", primary, adx))
		}
	}

	// Confirmation from the secondary benchmark.
	confirm := 0.0
	if len(e.cfg.Benchmarks) > 1 {
		if qs, ok := in.Snapshots[e.cfg.Benchmarks[1]]; ok {
			qslope := qs.MustGet(domain.FeatTrendSlope, 0) / 10000
			inputs["secondary_slope"] = qslope
			if qslope*trend > 0 {
				confirm = 0.15
				evidence = append(evidence, fmt.Sprintf("%s confirms the direction of %s", e.cfg.Benchmarks[1], primary))
			} else if qslope*trend < 0 {
				confirm = -0.15
				evidence = append(evidence, fmt.Sprintf("%s diverges from %s", e.cfg.Benchmarks[1], primary))
			}
		}
	}

	// --- Volatility ---------------------------------------------------------
	vix := in.VIXLevel
	if vix == 0 {
		if vs, ok := in.Snapshots[e.cfg.VolatilityRef]; ok {
			vix = vs.MustGet(domain.FeatLast, 0)
		}
	}
	realized := 0.0
	if hasPrimary {
		realized = ps.MustGet(domain.FeatRealizedVol, 0)
	}
	inputs["vix"] = vix
	inputs["realized_vol"] = realized

	// Normalise volatility to 0..1 using the configured VIX band, falling back
	// to realised volatility when no implied series is available.
	var volNorm float64
	switch {
	case vix > 0:
		volNorm = domain.Clamp01((vix - e.cfg.VIXLow) / (e.cfg.VIXHigh*1.8 - e.cfg.VIXLow))
	case realized > 0:
		volNorm = domain.Clamp01((realized - 0.10) / 0.50)
	}
	if vix > 0 {
		switch {
		case vix >= e.cfg.VIXHigh:
			evidence = append(evidence, fmt.Sprintf("implied volatility at %.1f is above the high threshold of %.1f", vix, e.cfg.VIXHigh))
		case vix <= e.cfg.VIXLow:
			evidence = append(evidence, fmt.Sprintf("implied volatility at %.1f is below the calm threshold of %.1f", vix, e.cfg.VIXLow))
		}
	}

	// --- Breadth and participation -----------------------------------------
	breadth := domain.Clamp(in.Breadth, -1, 1)
	inputs["breadth"] = breadth
	inputs["volume_ratio"] = in.VolumeRatio
	if math.Abs(breadth) >= e.cfg.BreadthThreshold {
		dir := "positive"
		if breadth < 0 {
			dir = "negative"
		}
		evidence = append(evidence, fmt.Sprintf("market breadth is %s at %.0f%% net advancers", dir, breadth*100))
	}

	// --- Risk appetite ------------------------------------------------------
	risk := 0.0
	n := 0
	if cs, ok := in.Snapshots[e.cfg.CreditRef]; ok {
		v := cs.MustGet(domain.FeatMomentum10, 0)
		inputs["credit_momentum"] = v
		risk += math.Tanh(v * 60)
		n++
	}
	if rs, ok := in.Snapshots[e.cfg.RatesRef]; ok {
		v := rs.MustGet(domain.FeatMomentum10, 0)
		inputs["rates_momentum"] = v
		// Long-duration Treasuries rallying is typically a flight to safety.
		risk += -math.Tanh(v * 60)
		n++
	}
	if ds, ok := in.Snapshots[e.cfg.DollarRef]; ok {
		v := ds.MustGet(domain.FeatMomentum10, 0)
		inputs["dollar_momentum"] = v
		risk += -math.Tanh(v * 80)
		n++
	}
	if n > 0 {
		risk /= float64(n)
	}
	risk = domain.Clamp(risk*0.6+breadth*0.4, -1, 1)
	inputs["risk_appetite"] = risk

	// --- Range / breakout / reversal ---------------------------------------
	bbWidth, percentB, rsi := 0.0, 0.5, 50.0
	breakoutUp, breakoutDown := 0.0, 0.0
	if hasPrimary {
		bbWidth = ps.MustGet(domain.FeatBBWidth, 0)
		percentB = ps.MustGet(domain.FeatBBPercentB, 0.5)
		rsi = ps.MustGet(domain.FeatRSI14, 50)
		breakoutUp = ps.MustGet(domain.FeatBreakoutUp, 0)
		breakoutDown = ps.MustGet(domain.FeatBreakoutDown, 0)
	}
	inputs["bb_width"] = bbWidth
	inputs["bb_percent_b"] = percentB
	inputs["rsi"] = rsi

	// --- Score every candidate ---------------------------------------------
	scores := map[domain.Regime]float64{}
	t := trend + confirm

	scores[domain.RegimeBullTrend] = domain.Clamp01(math.Max(0, t)*0.75+math.Max(0, breadth)*0.25) * (1 - 0.3*volNorm)
	scores[domain.RegimeBearTrend] = domain.Clamp01(math.Max(0, -t)*0.75+math.Max(0, -breadth)*0.25) * (0.7 + 0.3*volNorm)
	scores[domain.RegimeRange] = domain.Clamp01((1-math.Abs(t))*0.6 + (1-domain.Clamp01(bbWidth/0.06))*0.2 + (1-volNorm)*0.2)
	scores[domain.RegimeHighVol] = domain.Clamp01(volNorm*1.15 - 0.15)
	scores[domain.RegimeLowVol] = domain.Clamp01((1-volNorm)*0.9 - 0.15)
	scores[domain.RegimeBreakout] = domain.Clamp01(math.Max(breakoutUp, breakoutDown)*0.6 + domain.Clamp01(bbWidth/0.05)*0.2 + domain.Clamp01(in.VolumeRatio/2)*0.2)
	// Reversal: momentum and oscillator disagree at an extreme.
	rev := 0.0
	if rsi > 70 && t < 0 {
		rev = domain.Clamp01((rsi - 70) / 20)
	} else if rsi < 30 && t > 0 {
		rev = domain.Clamp01((30 - rsi) / 20)
	}
	scores[domain.RegimeReversal] = rev
	scores[domain.RegimeRiskOn] = domain.Clamp01(math.Max(0, risk)*0.8 + math.Max(0, breadth)*0.2)
	scores[domain.RegimeRiskOff] = domain.Clamp01(math.Max(0, -risk)*0.8 + math.Max(0, -breadth)*0.2)

	// Volatility dominance: when volatility is extreme the environment *is*
	// high-volatility regardless of direction, because that is what changes how
	// every downstream threshold should behave.
	if volNorm > 0.8 {
		scores[domain.RegimeHighVol] = math.Max(scores[domain.RegimeHighVol], 0.85)
	}

	best, second := rank(scores)
	conf := scores[best]
	// Confidence is the margin over the runner-up as much as the absolute
	// score: a 0.55 winner beating a 0.54 runner-up is not a confident call.
	margin := scores[best] - scores[second]
	conf = domain.Clamp01(0.6*conf + 0.4*domain.Clamp01(margin*2.5))

	out := domain.MarketRegime{
		Regime:         best,
		Confidence:     conf,
		Secondary:      second,
		SecondaryScore: scores[second],
		Volatility:     volNorm,
		Breadth:        breadth,
		Momentum:       domain.Clamp(t, -1, 1),
		RiskAppetite:   risk,
		Liquidity:      domain.Clamp01(in.VolumeRatio / 2),
		Inputs:         inputs,
		Scores:         scores,
		Evidence:       evidence,
		AsOf:           in.AsOf,
		Version:        Version,
	}
	return e.applyHysteresis(out)
}

// applyHysteresis suppresses flapping: a new regime must persist for the
// configured duration, or arrive with clearly higher confidence, before it
// replaces the incumbent. Flapping regimes are worse than a slightly stale one
// because every downstream threshold keys off the regime.
func (e *Engine) applyHysteresis(next domain.MarketRegime) domain.MarketRegime {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := next.AsOf
	if now.IsZero() {
		now = e.cfg.Clock.Now()
	}
	cur := e.current

	if cur.Regime == domain.RegimeUndefined {
		next.Changed = true
		next.PreviousRegime = domain.RegimeUndefined
		e.current = next
		e.lastChangeAt = now
		e.candidate = next.Regime
		e.candidateAt = now
		e.record(next)
		return next
	}

	if next.Regime == cur.Regime {
		e.candidate = next.Regime
		e.candidateAt = now
		next.Changed = false
		next.PreviousRegime = cur.PreviousRegime
		next.StableFor = now.Sub(e.lastChangeAt)
		e.current = next
		return next
	}

	if e.candidate != next.Regime {
		e.candidate = next.Regime
		e.candidateAt = now
	}
	held := now.Sub(e.candidateAt)
	decisive := next.Confidence >= math.Min(0.9, cur.Confidence+0.25)

	if held < e.cfg.Hysteresis && !decisive {
		// Report the incumbent, but surface the pending candidate so operators
		// can see a change forming rather than being surprised by it.
		out := cur
		out.AsOf = next.AsOf
		out.Inputs = next.Inputs
		out.Scores = next.Scores
		out.Volatility = next.Volatility
		out.Breadth = next.Breadth
		out.Momentum = next.Momentum
		out.RiskAppetite = next.RiskAppetite
		out.Secondary = next.Regime
		out.SecondaryScore = next.Confidence
		out.Changed = false
		out.StableFor = now.Sub(e.lastChangeAt)
		out.Evidence = append(append([]string(nil), next.Evidence...),
			fmt.Sprintf("candidate regime %s pending for %s (hysteresis %s)",
				next.Regime, held.Truncate(time.Second), e.cfg.Hysteresis))
		e.current = out
		return out
	}

	next.Changed = true
	next.PreviousRegime = cur.Regime
	next.StableFor = 0
	e.current = next
	e.lastChangeAt = now
	e.record(next)
	return next
}

func (e *Engine) record(r domain.MarketRegime) {
	e.history = append(e.history, r)
	if len(e.history) > 512 {
		e.history = e.history[len(e.history)-512:]
	}
}

// Current returns the regime in force.
func (e *Engine) Current() domain.MarketRegime {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.current
}

// History returns recorded regime changes, oldest first.
func (e *Engine) History() []domain.MarketRegime {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]domain.MarketRegime(nil), e.history...)
}

func rank(scores map[domain.Regime]float64) (domain.Regime, domain.Regime) {
	type kv struct {
		r domain.Regime
		v float64
	}
	list := make([]kv, 0, len(scores))
	for r, v := range scores {
		list = append(list, kv{r, v})
	}
	// Sort by score descending, then by name so ties are deterministic.
	sort.Slice(list, func(i, j int) bool {
		if list[i].v == list[j].v {
			return list[i].r < list[j].r
		}
		return list[i].v > list[j].v
	})
	if len(list) == 0 {
		return domain.RegimeUndefined, domain.RegimeUndefined
	}
	if len(list) == 1 {
		return list[0].r, list[0].r
	}
	return list[0].r, list[1].r
}

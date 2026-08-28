// Package relationship models how the market's anchors move relative to one
// another: correlations and their changes, sector rotation, risk-on/risk-off
// posture and divergences.
//
// The engine reports what it measures and nothing more. It does not claim to
// know *why* a correlation changed, and it never asserts institutional intent.
package relationship

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/features"
)

// Pair names a relationship whose typical behaviour is known well enough that a
// departure from it is worth surfacing.
type Pair struct {
	A        domain.Ticker
	B        domain.Ticker
	Expected float64 // typical correlation sign/magnitude
	Note     string
}

// DefaultPairs are the cross-asset relationships QuantOS monitors. Expected
// values are coarse priors used only to flag divergence, never to trade.
var DefaultPairs = []Pair{
	{"SPY", "QQQ", 0.90, "large-cap and technology beta normally move together"},
	{"SPY", "IWM", 0.80, "large and small caps normally move together"},
	{"SPY", "HYG", 0.65, "equities and high-yield credit normally move together"},
	{"SPY", "TLT", -0.30, "equities and long-duration Treasuries are usually inversely related"},
	{"SPY", "VIX", -0.75, "index level and implied volatility are strongly inversely related"},
	{"GLD", "DXY", -0.45, "gold and the dollar are usually inversely related"},
	{"XLK", "SPY", 0.88, "technology is the dominant index weight"},
	{"XLU", "SPY", 0.45, "utilities are defensive and less correlated in stress"},
}

// Config parameterises the relationship engine.
type Config struct {
	Window       int // observations used for correlation
	ChangeWindow int // observations used for the "recent" correlation
	Pairs        []Pair
	SectorETFs   []domain.Ticker
	Benchmark    domain.Ticker
	// DivergenceThreshold is the absolute correlation gap that counts.
	DivergenceThreshold float64
}

// DefaultConfig returns sensible parameters.
func DefaultConfig() Config {
	return Config{
		Window:              60,
		ChangeWindow:        20,
		Pairs:               DefaultPairs,
		Benchmark:           "SPY",
		DivergenceThreshold: 0.35,
	}
}

// ReturnSource supplies recent log returns per symbol. features.Engine
// satisfies it, which avoids duplicating price history.
type ReturnSource interface {
	Returns(t domain.Ticker, n int) []float64
}

// Engine computes the cross-asset picture.
type Engine struct {
	cfg Config

	mu     sync.RWMutex
	latest domain.RelationshipState
}

// NewEngine builds a relationship engine.
func NewEngine(cfg Config) *Engine {
	if cfg.Window <= 0 {
		cfg.Window = 60
	}
	if cfg.ChangeWindow <= 0 || cfg.ChangeWindow > cfg.Window {
		cfg.ChangeWindow = cfg.Window / 3
	}
	if len(cfg.Pairs) == 0 {
		cfg.Pairs = DefaultPairs
	}
	if cfg.Benchmark == "" {
		cfg.Benchmark = "SPY"
	}
	if cfg.DivergenceThreshold <= 0 {
		cfg.DivergenceThreshold = 0.35
	}
	return &Engine{cfg: cfg}
}

// Update recomputes the relationship state.
func (e *Engine) Update(asOf time.Time, src ReturnSource, snaps map[domain.Ticker]domain.FeatureSnapshot) domain.RelationshipState {
	st := domain.RelationshipState{
		AsOf:              asOf,
		Correlation:       map[string]float64{},
		CorrelationChange: map[string]float64{},
	}

	for _, p := range e.cfg.Pairs {
		a := src.Returns(p.A, e.cfg.Window)
		b := src.Returns(p.B, e.cfg.Window)
		n := min(len(a), len(b))
		if n < 10 {
			continue
		}
		full := features.Correlation(a[len(a)-n:], b[len(b)-n:])
		key := string(p.A) + "|" + string(p.B)
		st.Correlation[key] = round3(full)

		if n > e.cfg.ChangeWindow {
			recent := features.Correlation(a[len(a)-e.cfg.ChangeWindow:], b[len(b)-e.cfg.ChangeWindow:])
			st.CorrelationChange[key] = round3(recent - full)
			if math.Abs(recent-p.Expected) >= e.cfg.DivergenceThreshold {
				st.Divergences = append(st.Divergences, domain.Divergence{
					A: string(p.A), B: string(p.B),
					Expected: p.Expected, Observed: round3(recent),
					Magnitude: round3(math.Abs(recent - p.Expected)),
					Note: fmt.Sprintf("%s: observed %.2f over the last %d observations against a typical %.2f",
						p.Note, recent, e.cfg.ChangeWindow, p.Expected),
				})
			}
		}
	}
	sort.Slice(st.Divergences, func(i, j int) bool { return st.Divergences[i].Magnitude > st.Divergences[j].Magnitude })

	st.Sectors = e.sectorPerformance(snaps)
	st.Rotation, st.RiskOnScore = e.rotation(st.Sectors, snaps)
	if len(st.Sectors) >= 2 {
		st.Notes = append(st.Notes, fmt.Sprintf("leading sector %s (%.2f%% over 20 bars), lagging sector %s (%.2f%%)",
			st.Sectors[0].Sector, st.Sectors[0].Return20D*100,
			st.Sectors[len(st.Sectors)-1].Sector, st.Sectors[len(st.Sectors)-1].Return20D*100))
	}

	e.mu.Lock()
	e.latest = st
	e.mu.Unlock()
	return st
}

// sectorETFName maps a sector ETF to its human sector label.
var sectorETFName = map[domain.Ticker]string{
	"XLK": "Technology", "XLF": "Financials", "XLE": "Energy",
	"XLV": "Health Care", "XLI": "Industrials", "XLY": "Consumer Discretionary",
	"XLP": "Consumer Staples", "XLU": "Utilities", "XLB": "Materials",
	"XLRE": "Real Estate", "XLC": "Communication Services",
}

// defensiveSectors are the ones that typically outperform when risk appetite falls.
var defensiveSectors = map[string]bool{
	"Utilities": true, "Consumer Staples": true, "Health Care": true, "Real Estate": true,
}

func (e *Engine) sectorPerformance(snaps map[domain.Ticker]domain.FeatureSnapshot) []domain.SectorPerformance {
	etfs := e.cfg.SectorETFs
	if len(etfs) == 0 {
		for t := range sectorETFName {
			etfs = append(etfs, t)
		}
		sort.Slice(etfs, func(i, j int) bool { return etfs[i] < etfs[j] })
	}
	bench := snaps[e.cfg.Benchmark]
	benchMom := bench.MustGet(domain.FeatMomentum60, 0)

	out := []domain.SectorPerformance{}
	for _, t := range etfs {
		s, ok := snaps[t]
		if !ok {
			continue
		}
		p := domain.SectorPerformance{
			Sector:      sectorETFName[t],
			ETF:         t,
			Return1D:    s.MustGet(domain.FeatRetLog1, 0),
			Return5D:    s.MustGet(domain.FeatRetLog5, 0),
			Return20D:   s.MustGet(domain.FeatMomentum10, 0),
			RelStrength: s.MustGet(domain.FeatMomentum60, 0) - benchMom,
		}
		if p.Sector == "" {
			p.Sector = string(t)
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RelStrength == out[j].RelStrength {
			return out[i].Sector < out[j].Sector
		}
		return out[i].RelStrength > out[j].RelStrength
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// rotation classifies leadership as cyclical or defensive and scores risk
// appetite from the spread between the two groups.
func (e *Engine) rotation(sectors []domain.SectorPerformance, snaps map[domain.Ticker]domain.FeatureSnapshot) (string, float64) {
	if len(sectors) < 4 {
		return "INDETERMINATE", 0
	}
	var defSum, cycSum float64
	var defN, cycN int
	for _, s := range sectors {
		if defensiveSectors[s.Sector] {
			defSum += s.RelStrength
			defN++
		} else {
			cycSum += s.RelStrength
			cycN++
		}
	}
	if defN == 0 || cycN == 0 {
		return "INDETERMINATE", 0
	}
	spread := cycSum/float64(cycN) - defSum/float64(defN)

	// Credit and duration corroborate the sector read.
	corroboration := 0.0
	if hyg, ok := snaps["HYG"]; ok {
		corroboration += math.Tanh(hyg.MustGet(domain.FeatMomentum10, 0) * 60)
	}
	if tlt, ok := snaps["TLT"]; ok {
		corroboration -= math.Tanh(tlt.MustGet(domain.FeatMomentum10, 0) * 60)
	}
	score := domain.Clamp(math.Tanh(spread*40)*0.7+corroboration*0.3, -1, 1)

	switch {
	case score > 0.30:
		return "DEFENSIVE_TO_CYCLICAL", score
	case score < -0.30:
		return "CYCLICAL_TO_DEFENSIVE", score
	default:
		return "MIXED", score
	}
}

// Latest returns the last computed state.
func (e *Engine) Latest() domain.RelationshipState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.latest
}

func round3(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1000) / 1000
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

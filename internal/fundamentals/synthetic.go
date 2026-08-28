package fundamentals

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// SyntheticStore generates deterministic, plausible fundamentals for the
// development universe.
//
// This is FIXTURE DATA. It exists so the fundamental, valuation and scoring
// engines have something coherent to operate on without a paid filings feed.
// It is not, and must never be presented as, reported financials: every value
// is derived from a hash of the ticker. Swap in a real Store implementation and
// nothing else in the platform changes.
type SyntheticStore struct {
	seed     int64
	asOfBase time.Time

	mu    sync.RWMutex
	cache map[domain.Ticker][]domain.Fundamental
}

// NewSyntheticStore builds a generator anchored at a reference date.
func NewSyntheticStore(seed int64, asOf time.Time) *SyntheticStore {
	return &SyntheticStore{seed: seed, asOfBase: asOf, cache: map[domain.Ticker][]domain.Fundamental{}}
}

// Periods implements Store, returning reports filed on or before asOf.
func (s *SyntheticStore) Periods(ticker domain.Ticker, asOf time.Time, limit int) ([]domain.Fundamental, error) {
	s.mu.RLock()
	all, ok := s.cache[ticker]
	s.mu.RUnlock()
	if !ok {
		all = s.generate(ticker)
		s.mu.Lock()
		s.cache[ticker] = all
		s.mu.Unlock()
	}
	out := make([]domain.Fundamental, 0, len(all))
	for _, f := range all {
		// Point-in-time: a report is invisible until it is filed.
		if !asOf.IsZero() && f.FiledAt.After(asOf) {
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PeriodEnd.After(out[j].PeriodEnd) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *SyntheticStore) generate(t domain.Ticker) []domain.Fundamental {
	h := fnv.New64a()
	_, _ = h.Write([]byte(t))
	rng := rand.New(rand.NewSource(s.seed ^ int64(h.Sum64())))

	// Company archetype: growth, quality and leverage are drawn once and then
	// held stable across periods, with noise, so ratios behave like a business
	// rather than like a random walk.
	growth := 0.02 + rng.Float64()*0.28
	grossMargin := 0.25 + rng.Float64()*0.50
	opMarginRatio := 0.15 + rng.Float64()*0.55 // share of gross margin retained
	leverage := rng.Float64() * 1.8
	shares := 100e6 * (0.5 + rng.Float64()*20)
	revenue := 2e8 * (0.5 + rng.Float64()*40)

	const periods = 13 // ~3 years of quarters plus one for YoY at the oldest
	out := make([]domain.Fundamental, 0, periods)
	// Walk forward from the oldest period so growth compounds naturally.
	rev := revenue / math.Pow(1+growth/4, periods)
	for i := periods - 1; i >= 0; i-- {
		end := s.asOfBase.AddDate(0, -3*i, 0)
		filed := end.AddDate(0, 0, 28)
		noise := 1 + (rng.Float64()-0.5)*0.06
		rev *= (1 + growth/4) * noise

		gp := rev * grossMargin * (1 + (rng.Float64()-0.5)*0.04)
		op := gp * opMarginRatio
		ni := op * 0.79
		ocf := ni * (1.05 + rng.Float64()*0.25)
		capex := rev * (0.03 + rng.Float64()*0.05)
		equity := rev * (0.8 + rng.Float64()*1.2)
		debt := equity * leverage
		f := domain.Fundamental{
			Ticker:             t,
			PeriodEnd:          end,
			FiledAt:            filed,
			Period:             fmt.Sprintf("Q%d-%d", quarterOf(end), end.Year()),
			Currency:           "USD",
			Revenue:            round2(rev),
			GrossProfit:        round2(gp),
			OperatingIncome:    round2(op),
			NetIncome:          round2(ni),
			EPS:                round4(ni / shares),
			FreeCashFlow:       round2(ocf - capex),
			OperatingCashFlow:  round2(ocf),
			CapEx:              round2(capex),
			TotalAssets:        round2(equity + debt + rev*0.4),
			TotalEquity:        round2(equity),
			TotalDebt:          round2(debt),
			Cash:               round2(rev * (0.1 + rng.Float64()*0.3)),
			CurrentAssets:      round2(rev * (0.4 + rng.Float64()*0.5)),
			CurrentLiabilities: round2(rev * (0.2 + rng.Float64()*0.3)),
			SharesDiluted:      shares,
			Source:             "synthetic-fixture",
		}
		f.InvestedCapital = f.TotalEquity + f.TotalDebt - f.Cash
		out = append(out, f)
	}
	// Fill year-over-year fields where a comparison exists.
	sort.SliceStable(out, func(i, j int) bool { return out[i].PeriodEnd.After(out[j].PeriodEnd) })
	for i := range out {
		if i+4 < len(out) {
			prev := out[i+4]
			if prev.Revenue > 0 {
				out[i].RevenueYoY = round4(out[i].Revenue/prev.Revenue - 1)
			}
			if prev.EPS != 0 {
				out[i].EPSYoY = round4(out[i].EPS/math.Abs(prev.EPS) - sign(prev.EPS))
			}
		}
	}
	return out
}

func quarterOf(t time.Time) int { return (int(t.Month())-1)/3 + 1 }

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

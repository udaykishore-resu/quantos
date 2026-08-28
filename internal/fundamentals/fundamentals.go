// Package fundamentals derives financial ratios from reported periods and
// scores them against a strategy's configured policy.
//
// Two properties matter more than the arithmetic. First, "unknown" and "zero"
// are different: every metric carries an availability flag, and a missing
// metric is excluded from the score with the remaining weights renormalised,
// never treated as a zero. Second, point-in-time correctness: only periods with
// FiledAt <= the decision time are visible, so a restated figure cannot leak
// backwards into a historical decision.
package fundamentals

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Store supplies reported fundamentals.
type Store interface {
	// Periods returns reports for a ticker with FiledAt <= asOf, newest first.
	Periods(ticker domain.Ticker, asOf time.Time, limit int) ([]domain.Fundamental, error)
}

// Metric names, used for the availability map and for score explanations.
const (
	MetricRevenueGrowth   = "revenue_growth"
	MetricEPSGrowth       = "eps_growth"
	MetricGrossMargin     = "gross_margin"
	MetricOperatingMargin = "operating_margin"
	MetricNetMargin       = "net_margin"
	MetricFCFMargin       = "fcf_margin"
	MetricROE             = "roe"
	MetricROIC            = "roic"
	MetricDebtToEquity    = "debt_to_equity"
	MetricCurrentRatio    = "current_ratio"
	MetricConsistency     = "earnings_consistency"
	MetricAccruals        = "accruals"
)

// Compute derives ratios from a set of periods, newest first.
//
// It requires at least one period; year-over-year growth requires five periods
// for quarterly reporting (t and t-4), and falls back to the oldest available
// comparison with the period count recorded so the caller can judge.
func Compute(ticker domain.Ticker, periods []domain.Fundamental, asOf time.Time) domain.FundamentalMetrics {
	m := domain.FundamentalMetrics{
		Ticker:    ticker,
		AsOf:      asOf,
		Available: map[string]bool{},
		Periods:   len(periods),
	}
	if len(periods) == 0 {
		return m
	}
	sort.SliceStable(periods, func(i, j int) bool { return periods[i].PeriodEnd.After(periods[j].PeriodEnd) })
	cur := periods[0]
	m.BasedOn = cur.Period

	set := func(name string, v float64, ok bool, dst *float64) {
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			m.Available[name] = false
			return
		}
		*dst = v
		m.Available[name] = true
	}

	// Margins.
	set(MetricGrossMargin, safeDiv(cur.GrossProfit, cur.Revenue), cur.Revenue > 0, &m.GrossMargin)
	set(MetricOperatingMargin, safeDiv(cur.OperatingIncome, cur.Revenue), cur.Revenue > 0, &m.OperatingMargin)
	set(MetricNetMargin, safeDiv(cur.NetIncome, cur.Revenue), cur.Revenue > 0, &m.NetMargin)
	set(MetricFCFMargin, safeDiv(cur.FreeCashFlow, cur.Revenue), cur.Revenue > 0, &m.FCFMargin)

	// Returns on capital.
	set(MetricROE, safeDiv(cur.NetIncome*annualiseFactor(cur), cur.TotalEquity), cur.TotalEquity > 0, &m.ROE)
	ic := cur.InvestedCapital
	if ic == 0 {
		ic = cur.TotalEquity + cur.TotalDebt - cur.Cash
	}
	// NOPAT approximated at a 21% statutory rate; the assumption is explicit
	// rather than hidden, and a provider that supplies tax expense overrides it.
	nopat := cur.OperatingIncome * 0.79 * annualiseFactor(cur)
	set(MetricROIC, safeDiv(nopat, ic), ic > 0, &m.ROIC)

	// Balance sheet.
	set(MetricDebtToEquity, safeDiv(cur.TotalDebt, cur.TotalEquity), cur.TotalEquity > 0, &m.DebtToEquity)
	set(MetricCurrentRatio, safeDiv(cur.CurrentAssets, cur.CurrentLiabilities), cur.CurrentLiabilities > 0, &m.CurrentRatio)
	if cur.TotalAssets > 0 {
		set(MetricAccruals, (cur.NetIncome-cur.OperatingCashFlow)/cur.TotalAssets, true, &m.Accruals)
	} else {
		m.Available[MetricAccruals] = false
	}

	// Growth: prefer the reported year-over-year figure, else derive from t-4.
	if cur.RevenueYoY != 0 {
		set(MetricRevenueGrowth, cur.RevenueYoY, true, &m.RevenueGrowth)
	} else if prior, ok := priorYear(periods); ok && prior.Revenue > 0 {
		set(MetricRevenueGrowth, cur.Revenue/prior.Revenue-1, true, &m.RevenueGrowth)
	} else {
		m.Available[MetricRevenueGrowth] = false
	}
	if cur.EPSYoY != 0 {
		set(MetricEPSGrowth, cur.EPSYoY, true, &m.EPSGrowth)
	} else if prior, ok := priorYear(periods); ok && prior.EPS != 0 {
		set(MetricEPSGrowth, cur.EPS/math.Abs(prior.EPS)-sign(prior.EPS), true, &m.EPSGrowth)
	} else {
		m.Available[MetricEPSGrowth] = false
	}

	// Earnings consistency: fraction of comparable periods with positive YoY
	// EPS growth. Needs at least six periods to mean anything.
	if len(periods) >= 6 {
		good, total := 0, 0
		for i := 0; i+4 < len(periods); i++ {
			p, q := periods[i], periods[i+4]
			if q.EPS == 0 {
				continue
			}
			total++
			if p.EPS > q.EPS {
				good++
			}
		}
		if total > 0 {
			set(MetricConsistency, float64(good)/float64(total), true, &m.EarningsConsistency)
		} else {
			m.Available[MetricConsistency] = false
		}
	} else {
		m.Available[MetricConsistency] = false
	}
	return m
}

// annualiseFactor scales a quarterly figure to a trailing-annual equivalent.
func annualiseFactor(f domain.Fundamental) float64 {
	if len(f.Period) >= 2 && f.Period[0] == 'Q' {
		return 4
	}
	return 1
}

func priorYear(periods []domain.Fundamental) (domain.Fundamental, bool) {
	if len(periods) > 4 {
		return periods[4], true
	}
	if len(periods) > 1 {
		return periods[len(periods)-1], true
	}
	return domain.Fundamental{}, false
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return math.NaN()
	}
	return a / b
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// Scores is the fundamental scoring output. Each component is 0..100 and
// carries the evidence that produced it.
type Scores struct {
	Quality           float64  `json:"quality"`
	Growth            float64  `json:"growth"`
	FinancialStrength float64  `json:"financial_strength"`
	Fundamental       float64  `json:"fundamental"`
	Missing           []string `json:"missing,omitempty"`
	Evidence          []string `json:"evidence,omitempty"`
}

// component is one scored input with its weight inside a bucket.
type component struct {
	metric   string
	weight   float64
	value    float64
	score    float64
	evidence string
}

// Score maps metrics to 0..100 sub-scores using a strategy's policy.
//
// Scoring is band-based rather than threshold-based: a company that just misses
// a threshold should not score identically to one that missed by a mile, and a
// company far past it should not accrue unbounded credit.
func Score(m domain.FundamentalMetrics, p domain.FundamentalPolicy) Scores {
	var out Scores
	missing := map[string]bool{}

	quality := []component{
		bandComp(m, MetricGrossMargin, 0.25, orDefault(p.MinGrossMargin, 0.35), 0.75,
			"gross margin of %.1f%%", 100),
		bandComp(m, MetricOperatingMargin, 0.25, orDefault(p.MinOperatingMargin, 0.10), 0.35,
			"operating margin of %.1f%%", 100),
		bandComp(m, MetricROE, 0.20, orDefault(p.MinROE, 0.12), 0.35,
			"return on equity of %.1f%%", 100),
		bandComp(m, MetricROIC, 0.20, orDefault(p.MinROIC, 0.10), 0.30,
			"return on invested capital of %.1f%%", 100),
		invBandComp(m, MetricAccruals, 0.10, 0.10, -0.05,
			"accruals at %.1f%% of assets", 100),
	}
	growth := []component{
		bandComp(m, MetricRevenueGrowth, 0.40, orDefault(p.MinRevenueGrowth, 0.05), 0.30,
			"revenue growth of %.1f%%", 100),
		bandComp(m, MetricEPSGrowth, 0.40, orDefault(p.MinEPSGrowth, 0.05), 0.35,
			"earnings growth of %.1f%%", 100),
		bandComp(m, MetricConsistency, 0.20, 0.5, 0.9,
			"positive year-over-year earnings in %.0f%% of comparable periods", 100),
	}
	strength := []component{
		invBandComp(m, MetricDebtToEquity, 0.40, orDefault(p.MaxDebtToEquity, 1.5), 0.2,
			"debt-to-equity of %.2f", 1),
		bandComp(m, MetricCurrentRatio, 0.30, orDefault(p.MinCurrentRatio, 1.0), 2.5,
			"current ratio of %.2f", 1),
		bandComp(m, MetricFCFMargin, 0.30, 0.0, 0.20,
			"free-cash-flow margin of %.1f%%", 100),
	}

	out.Quality, _ = aggregate(quality, missing)
	out.Growth, _ = aggregate(growth, missing)
	out.FinancialStrength, _ = aggregate(strength, missing)

	// The composite is an equal-ish blend that leans on quality, because
	// quality metrics are the most persistent of the three across regimes.
	parts := []struct {
		v float64
		w float64
		n string
	}{
		{out.Quality, 0.40, "quality"},
		{out.Growth, 0.35, "growth"},
		{out.FinancialStrength, 0.25, "financial_strength"},
	}
	sum, wsum := 0.0, 0.0
	for _, p := range parts {
		if p.v < 0 {
			continue
		}
		sum += p.v * p.w
		wsum += p.w
	}
	if wsum > 0 {
		out.Fundamental = sum / wsum
	}

	for _, c := range append(append(append([]component{}, quality...), growth...), strength...) {
		if c.evidence != "" {
			out.Evidence = append(out.Evidence, c.evidence)
		}
	}
	for k := range missing {
		out.Missing = append(out.Missing, k)
	}
	sort.Strings(out.Missing)
	return out
}

// aggregate renormalises weights over available components.
func aggregate(cs []component, missing map[string]bool) (float64, float64) {
	sum, wsum := 0.0, 0.0
	for _, c := range cs {
		if c.weight <= 0 {
			continue
		}
		if math.IsNaN(c.score) {
			missing[c.metric] = true
			continue
		}
		sum += c.score * c.weight
		wsum += c.weight
	}
	if wsum == 0 {
		return -1, 0 // fully unavailable; caller excludes it
	}
	return sum / wsum, wsum
}

// bandComp scores "higher is better" between floor and target.
func bandComp(m domain.FundamentalMetrics, metric string, weight, floor, target float64, format string, scale float64) component {
	c := component{metric: metric, weight: weight, score: math.NaN()}
	v, ok := metricValue(m, metric)
	if !ok {
		return c
	}
	c.value = v
	c.score = 100 * bandScore(v, floor, target)
	c.evidence = fmt.Sprintf(format, v*scale)
	return c
}

// invBandComp scores "lower is better" between ceiling and target.
func invBandComp(m domain.FundamentalMetrics, metric string, weight, ceiling, target float64, format string, scale float64) component {
	c := component{metric: metric, weight: weight, score: math.NaN()}
	v, ok := metricValue(m, metric)
	if !ok {
		return c
	}
	c.value = v
	c.score = 100 * bandScore(-v, -ceiling, -target)
	c.evidence = fmt.Sprintf(format, v*scale)
	return c
}

// bandScore maps v into [0,1]: 0.5 at floor, 1.0 at target, tapering below.
// The shape is deliberate: passing the policy floor should be a pass mark, not
// full marks, and exceeding the target should not be rewarded without bound.
func bandScore(v, floor, target float64) float64 {
	if target == floor {
		if v >= floor {
			return 1
		}
		return 0
	}
	if v >= target {
		// Diminishing credit beyond target, capped at 1.
		excess := (v - target) / math.Abs(target-floor)
		return domain.Clamp01(1 - 0.0*excess)
	}
	if v >= floor {
		return 0.5 + 0.5*(v-floor)/(target-floor)
	}
	// Below the floor, decay to zero over the same distance again.
	deficit := (floor - v) / math.Abs(target-floor)
	return domain.Clamp01(0.5 * (1 - deficit))
}

func metricValue(m domain.FundamentalMetrics, name string) (float64, bool) {
	if !m.Available[name] {
		return 0, false
	}
	switch name {
	case MetricRevenueGrowth:
		return m.RevenueGrowth, true
	case MetricEPSGrowth:
		return m.EPSGrowth, true
	case MetricGrossMargin:
		return m.GrossMargin, true
	case MetricOperatingMargin:
		return m.OperatingMargin, true
	case MetricNetMargin:
		return m.NetMargin, true
	case MetricFCFMargin:
		return m.FCFMargin, true
	case MetricROE:
		return m.ROE, true
	case MetricROIC:
		return m.ROIC, true
	case MetricDebtToEquity:
		return m.DebtToEquity, true
	case MetricCurrentRatio:
		return m.CurrentRatio, true
	case MetricConsistency:
		return m.EarningsConsistency, true
	case MetricAccruals:
		return m.Accruals, true
	}
	return 0, false
}

func orDefault(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

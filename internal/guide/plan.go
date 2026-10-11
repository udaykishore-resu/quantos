package guide

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// PlanInput is a savings goal as the owner states it.
type PlanInput struct {
	Capital        float64 `json:"capital"`
	Monthly        float64 `json:"monthly_contribution"`
	Years          int     `json:"years"`
	AnnualIncome   float64 `json:"target_annual_income"`
	WithdrawalRate float64 `json:"withdrawal_rate"`
	// Haircut is subtracted from every sampled annual return. The sample is
	// one history, and 2010–2026 US large caps were a kind one; the haircut
	// is how the reader says "assume less".
	Haircut   float64       `json:"return_haircut"`
	Benchmark domain.Ticker `json:"benchmark"`
	Paths     int           `json:"paths"`
	Block     int           `json:"block_months"`
	Seed      int64         `json:"seed"`
}

// PlanYear is the distribution of wealth after a given number of years.
type PlanYear struct {
	Year        int     `json:"year"`
	Contributed float64 `json:"contributed_to_date"`
	P10         float64 `json:"p10"`
	P25         float64 `json:"p25"`
	P50         float64 `json:"p50"`
	P75         float64 `json:"p75"`
	P90         float64 `json:"p90"`
	// ProbSustainable is the share of paths whose wealth supports the income
	// at the withdrawal rate; ProbFromReturns the share that would supply it
	// from the sample's average return alone, with no growth left over.
	ProbSustainable float64 `json:"prob_sustainable_target"`
	ProbFromReturns float64 `json:"prob_income_from_returns"`
	Fixed           float64 `json:"fixed_rate_projection"`
}

// PlanResult is the planner's output.
type PlanResult struct {
	Input              PlanInput  `json:"input"`
	SampleFrom         time.Time  `json:"sample_from"`
	SampleTo           time.Time  `json:"sample_to"`
	SampleMonths       int        `json:"sample_months"`
	SampleAnnualReturn float64    `json:"sample_annual_return"`
	SampleAnnualVol    float64    `json:"sample_annual_vol"`
	SampleWorstYear    float64    `json:"sample_worst_12m"`
	UsedAnnualReturn   float64    `json:"used_annual_return"`
	TargetSustainable  float64    `json:"target_capital_sustainable"`
	TargetFromReturns  float64    `json:"target_capital_from_returns"`
	Years              []PlanYear `json:"years"`
	MedianYearSust     int        `json:"median_year_sustainable"`
	MedianYearReturns  int        `json:"median_year_from_returns"`
	Verdict            string     `json:"verdict"`
	Notes              []string   `json:"notes"`
	Disclaimer         string     `json:"disclaimer"`
}

// MonthlyReturns reduces a daily series to month-end-to-month-end simple
// returns.
func MonthlyReturns(bars []domain.Candle) (rets []float64, from, to time.Time) {
	if len(bars) < 2 {
		return nil, from, to
	}
	type me struct {
		t  time.Time
		px float64
	}
	var ends []me
	for i, c := range bars {
		last := i == len(bars)-1 || bars[i+1].Start.Month() != c.Start.Month() || bars[i+1].Start.Year() != c.Start.Year()
		if last {
			ends = append(ends, me{c.Start, c.Close})
		}
	}
	for i := 1; i < len(ends); i++ {
		if ends[i-1].px > 0 {
			rets = append(rets, ends[i].px/ends[i-1].px-1)
		}
	}
	if len(ends) > 1 {
		from, to = ends[0].t, ends[len(ends)-1].t
	}
	return rets, from, to
}

// Plan runs a block-bootstrap Monte Carlo of wealth paths.
//
// Why bootstrap rather than a normal distribution: the sample's fat tails and
// volatility clustering survive block resampling and die in a mean-and-sigma
// summary, and the tails are what decide whether a plan survives. Why blocks
// of twelve months: long enough to carry a drawdown and a recovery, short
// enough to recombine into paths the sample never contained.
func Plan(monthly []float64, from, to time.Time, in PlanInput) (PlanResult, error) {
	if len(monthly) < 36 {
		return PlanResult{}, fmt.Errorf("guide: need at least three years of monthly returns, have %d months", len(monthly))
	}
	if in.Years <= 0 {
		in.Years = 20
	}
	if in.Paths <= 0 {
		in.Paths = 10000
	}
	if in.Block <= 0 {
		in.Block = 12
	}
	if in.WithdrawalRate <= 0 {
		in.WithdrawalRate = 0.04
	}
	if in.Seed == 0 {
		in.Seed = 20260827
	}
	res := PlanResult{Input: in, SampleFrom: from, SampleTo: to, SampleMonths: len(monthly), Disclaimer: domain.Disclaimer}

	// Sample statistics.
	g := 1.0
	for _, r := range monthly {
		g *= 1 + r
	}
	years := float64(len(monthly)) / 12
	res.SampleAnnualReturn = math.Pow(g, 1/years) - 1
	m, sd := meanStd(monthly)
	_ = m
	res.SampleAnnualVol = sd * math.Sqrt(12)
	worst := math.Inf(1)
	for i := 0; i+12 <= len(monthly); i++ {
		w := 1.0
		for _, r := range monthly[i : i+12] {
			w *= 1 + r
		}
		worst = math.Min(worst, w-1)
	}
	res.SampleWorstYear = worst
	res.UsedAnnualReturn = res.SampleAnnualReturn - in.Haircut
	monthlyHaircut := math.Pow(1+in.Haircut, 1.0/12) - 1

	if in.AnnualIncome > 0 {
		res.TargetSustainable = in.AnnualIncome / in.WithdrawalRate
		if res.UsedAnnualReturn > 0 {
			res.TargetFromReturns = in.AnnualIncome / res.UsedAnnualReturn
		}
	}

	// Simulate.
	months := in.Years * 12
	rng := lcg(uint64(in.Seed))
	wealth := make([][]float64, in.Years) // per year: wealth across paths
	for y := range wealth {
		wealth[y] = make([]float64, in.Paths)
	}
	for p := 0; p < in.Paths; p++ {
		w := in.Capital
		mIdx := 0
		for mIdx < months {
			start := int(rng() * float64(len(monthly)))
			for k := 0; k < in.Block && mIdx < months; k++ {
				r := monthly[(start+k)%len(monthly)] - monthlyHaircut
				w = w*(1+r) + in.Monthly
				mIdx++
				if mIdx%12 == 0 {
					wealth[mIdx/12-1][p] = w
				}
			}
		}
	}
	fixedMonthly := math.Pow(1+res.UsedAnnualReturn, 1.0/12) - 1
	fixed := in.Capital
	for y := 0; y < in.Years; y++ {
		ws := wealth[y]
		sort.Float64s(ws)
		q := func(p float64) float64 { return ws[int(math.Min(float64(len(ws)-1), math.Floor(p*float64(len(ws)))))] }
		for k := 0; k < 12; k++ {
			fixed = fixed*(1+fixedMonthly) + in.Monthly
		}
		py := PlanYear{
			Year: y + 1, Contributed: in.Capital + in.Monthly*12*float64(y+1),
			P10: q(0.10), P25: q(0.25), P50: q(0.50), P75: q(0.75), P90: q(0.90), Fixed: fixed,
		}
		if res.TargetSustainable > 0 {
			py.ProbSustainable = share(ws, res.TargetSustainable)
			if res.MedianYearSust == 0 && py.P50 >= res.TargetSustainable {
				res.MedianYearSust = y + 1
			}
		}
		if res.TargetFromReturns > 0 {
			py.ProbFromReturns = share(ws, res.TargetFromReturns)
			if res.MedianYearReturns == 0 && py.P50 >= res.TargetFromReturns {
				res.MedianYearReturns = y + 1
			}
		}
		res.Years = append(res.Years, py)
	}

	res.Notes = append(res.Notes,
		fmt.Sprintf("returns are block-bootstrapped (%d-month blocks, %d paths) from %s monthly returns %s..%s: %.1f%% a year with %.1f%% volatility, worst twelve months %.1f%%",
			in.Block, in.Paths, in.Benchmark, from.Format("2006-01"), to.Format("2006-01"), res.SampleAnnualReturn*100, res.SampleAnnualVol*100, res.SampleWorstYear*100),
		fmt.Sprintf("a haircut of %.1f%% a year is applied to every sampled return, giving %.1f%%; the sample period was a strong one for US equities and the haircut is the reader's allowance for that", in.Haircut*100, res.UsedAnnualReturn*100),
		"contributions are constant in nominal dollars; nothing here is adjusted for inflation or tax",
		"'sustainable' means the income is the withdrawal rate times wealth, which leaves the capital to keep growing; 'from returns' means spending the whole expected return, which leaves nothing for the bad years",
	)
	res.Verdict = verdict(res)
	return res, nil
}

func verdict(r PlanResult) string {
	if r.TargetSustainable == 0 {
		return fmt.Sprintf("median wealth after %d years is $%s, with a one-in-ten chance of less than $%s", r.Input.Years, money(r.Years[len(r.Years)-1].P50), money(r.Years[len(r.Years)-1].P10))
	}
	in := r.Input
	daily := in.AnnualIncome / 250
	s := fmt.Sprintf("$%.0f a day is $%s a year. Sustained at a %.0f%% withdrawal that needs $%s of capital; drawn from the expected return alone it needs $%s and leaves no margin. ",
		daily, money(in.AnnualIncome), in.WithdrawalRate*100, money(r.TargetSustainable), money(r.TargetFromReturns))
	switch {
	case r.MedianYearSust > 0:
		s += fmt.Sprintf("Starting from $%s and adding $%s a month, the median path reaches the sustainable figure in year %d", money(in.Capital), money(in.Monthly), r.MedianYearSust)
		if r.MedianYearReturns > 0 {
			s += fmt.Sprintf(" and the from-returns figure in year %d", r.MedianYearReturns)
		}
		last := r.Years[r.MedianYearSust-1]
		s += fmt.Sprintf("; in that year one path in ten is below $%s.", money(last.P10))
	case r.MedianYearReturns > 0:
		s += fmt.Sprintf("Starting from $%s and adding $%s a month, the median path reaches the from-returns figure in year %d but not the sustainable one within %d years.",
			money(in.Capital), money(in.Monthly), r.MedianYearReturns, in.Years)
	default:
		s += fmt.Sprintf("Starting from $%s and adding $%s a month, the median path reaches neither within %d years; the levers are the contribution and the horizon, not the return.",
			money(in.Capital), money(in.Monthly), in.Years)
	}
	return s
}

func share(sorted []float64, target float64) float64 {
	i := sort.SearchFloat64s(sorted, target)
	return float64(len(sorted)-i) / float64(len(sorted))
}

// lcg is a small deterministic uniform generator so plans reproduce exactly
// from their seed, independent of math/rand's version.
func lcg(seed uint64) func() float64 {
	state := seed*6364136223846793005 + 1442695040888963407
	return func() float64 {
		state = state*6364136223846793005 + 1442695040888963407
		return float64(state>>11) / float64(uint64(1)<<53)
	}
}

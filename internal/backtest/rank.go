package backtest

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// The ranking sleeve is a different kind of strategy from the per-bar
// pipeline: once a month it sorts the universe on a formation signal, holds
// the top slice for a month, and re-sorts. There are no thresholds to tune
// and nothing is fitted, which is why the whole history is out of sample
// and why it is the strategy type the academic record actually supports.
//
// Three honest caveats are built in rather than footnoted: the universe is
// membership-gated at each formation date, costs are charged on every
// rebalance at the configured model and at twice it, and the long-short
// spread between the top and bottom slices is reported alongside the
// long-only sleeve, because a positive spread with a long-only sleeve that
// still loses to the index is the usual result and worth seeing plainly.

// RankSignal names a formation rule.
type RankSignal string

const (
	// Momentum12_1 is the twelve-month return skipping the most recent month,
	// the canonical formation (Jegadeesh & Titman 1993).
	Momentum12_1 RankSignal = "momentum-12-1"
	// Momentum6_1 is the six-month version.
	Momentum6_1 RankSignal = "momentum-6-1"
	// MomentumVolAdj divides the twelve-one return by trailing volatility,
	// which ranks a steady riser above a lottery ticket.
	MomentumVolAdj RankSignal = "momentum-voladj"
	// LowVol is minus trailing volatility: the defensive sleeve.
	LowVol RankSignal = "low-vol"
)

// RankConfig parameterises a run.
type RankConfig struct {
	Signal     RankSignal
	Series     map[domain.Ticker][]domain.Candle
	Candidates []domain.Ticker
	Benchmark  domain.Ticker
	Member     func(domain.Ticker, time.Time) bool // nil: everything is a member
	Start, End time.Time
	// TopFraction is the slice held, e.g. 0.1 for a decile. MinNames and
	// MaxNames bound the count.
	TopFraction float64
	MinNames    int
	MaxNames    int
	// InverseVol weights names by 1/vol instead of equally.
	InverseVol bool
	// VolTarget scales the whole sleeve's exposure by min(1, target/vol of
	// the sleeve's trailing return); zero disables.
	VolTarget    float64
	StartingCash float64
	Costs        domain.CostModel
	CostMultiple float64
	RiskFree     float64
	// Trials is the number of signal variants the caller ran, for the
	// deflated Sharpe.
	Trials int
}

// RankHolding is one name in one month's slice.
type RankHolding struct {
	Ticker domain.Ticker `json:"ticker"`
	Weight float64       `json:"weight"`
	Score  float64       `json:"score"`
}

// RankMonth is one rebalance.
type RankMonth struct {
	At        time.Time     `json:"at"`
	Universe  int           `json:"universe"`
	Holdings  []RankHolding `json:"holdings"`
	Turnover  float64       `json:"turnover"`
	Cost      float64       `json:"cost"`
	Exposure  float64       `json:"exposure"`
	Return    float64       `json:"return"`     // sleeve, this month, net
	Benchmark float64       `json:"benchmark"`  // benchmark, same month
	Spread    float64       `json:"long_short"` // top slice minus bottom slice, gross
}

// RankResult is a run.
type RankResult struct {
	Signal        RankSignal             `json:"signal"`
	Start         time.Time              `json:"start"`
	End           time.Time              `json:"end"`
	Months        []RankMonth            `json:"months"`
	Metrics       domain.BacktestMetrics `json:"metrics"`
	Benchmark     domain.BacktestMetrics `json:"benchmark_metrics"`
	ExcessMonthly float64                `json:"excess_monthly_mean"`
	ExcessTStat   float64                `json:"excess_t_stat"`
	HitRate       float64                `json:"months_beating_benchmark"`
	SpreadMonthly float64                `json:"long_short_monthly_mean"`
	SpreadTStat   float64                `json:"long_short_t_stat"`
	AvgTurnover   float64                `json:"avg_monthly_turnover"`
	TotalCosts    float64                `json:"total_costs"`
	WorstYear     float64                `json:"worst_calendar_year"`
	ByYear        []RankYear             `json:"by_year"`
	Current       []RankHolding          `json:"current_slice"`
	Notes         []string               `json:"notes"`
}

// RankYear is one calendar year.
type RankYear struct {
	Year      int     `json:"year"`
	Sleeve    float64 `json:"sleeve"`
	Benchmark float64 `json:"benchmark"`
}

// RunRank executes the sleeve.
func RunRank(cfg RankConfig) (RankResult, error) {
	if cfg.Signal == "" {
		cfg.Signal = Momentum12_1
	}
	if cfg.Benchmark == "" {
		cfg.Benchmark = "SPY"
	}
	if cfg.TopFraction <= 0 {
		cfg.TopFraction = 0.10
	}
	if cfg.MinNames <= 0 {
		cfg.MinNames = 5
	}
	if cfg.MaxNames <= 0 {
		cfg.MaxNames = 20
	}
	if cfg.StartingCash <= 0 {
		cfg.StartingCash = 100_000
	}
	if cfg.Costs.ParticipationCap <= 0 {
		cfg.Costs = domain.DefaultCostModel()
	}
	if cfg.CostMultiple <= 0 {
		cfg.CostMultiple = 1
	}
	member := cfg.Member
	if member == nil {
		member = func(domain.Ticker, time.Time) bool { return true }
	}
	bench := cfg.Series[cfg.Benchmark]
	if len(bench) < 300 {
		return RankResult{}, fmt.Errorf("rank: benchmark %s needs at least a year of history", cfg.Benchmark)
	}

	// Calendar and aligned closes, as in guide.Assess.
	var dates []time.Time
	for _, c := range bench {
		if (cfg.Start.IsZero() || !c.Start.Before(cfg.Start.AddDate(-1, -1, 0))) && (cfg.End.IsZero() || !c.Start.After(cfg.End)) {
			dates = append(dates, c.Start)
		}
	}
	idx := make(map[time.Time]int, len(dates))
	for i, d := range dates {
		idx[d] = i
	}
	closes := map[domain.Ticker][]float64{}
	for _, t := range append([]domain.Ticker{cfg.Benchmark}, cfg.Candidates...) {
		s := cfg.Series[t]
		if len(s) == 0 {
			continue
		}
		row := make([]float64, len(dates))
		for i := range row {
			row[i] = math.NaN()
		}
		for _, c := range s {
			if i, ok := idx[c.Start]; ok {
				row[i] = c.Close
			}
		}
		closes[t] = row
	}

	// Month-end indices.
	var ends []int
	for i := 0; i < len(dates); i++ {
		if i == len(dates)-1 || dates[i+1].Month() != dates[i].Month() {
			ends = append(ends, i)
		}
	}
	perSide := (cfg.Costs.CommissionBps + cfg.Costs.SpreadCostBps + cfg.Costs.SlippageBps) * cfg.CostMultiple / 10000

	res := RankResult{Signal: cfg.Signal}
	equity := cfg.StartingCash
	shares := map[domain.Ticker]float64{}
	var curve []domain.EquityPoint

	for mi := 0; mi < len(ends)-1; mi++ {
		t := ends[mi]
		at := dates[t]
		if !cfg.Start.IsZero() && at.Before(cfg.Start) {
			continue
		}
		// Formation: needs 13 months of history (252 + 21 bars).
		if t < 273 {
			continue
		}
		var pool []rankCand
		for _, tk := range cfg.Candidates {
			row, ok := closes[tk]
			if !ok || !member(tk, at) {
				continue
			}
			score, vol, ok := formation(cfg.Signal, row, t)
			if !ok {
				continue
			}
			pool = append(pool, rankCand{tk, score, vol})
		}
		if len(pool) < 2*cfg.MinNames {
			continue
		}
		sort.Slice(pool, func(i, j int) bool { return pool[i].score > pool[j].score })
		n := int(math.Round(float64(len(pool)) * cfg.TopFraction))
		n = max(cfg.MinNames, min(cfg.MaxNames, n))
		top, bottom := pool[:n], pool[len(pool)-n:]

		// Weights.
		w := map[domain.Ticker]float64{}
		sum := 0.0
		for _, c := range top {
			x := 1.0
			if cfg.InverseVol && c.vol > 0 {
				x = 1 / c.vol
			}
			w[c.t] = x
			sum += x
		}
		exposure := 1.0
		if cfg.VolTarget > 0 && mi > 0 {
			// Trailing 63-day vol of the sleeve's own realised returns.
			if sv := sleeveVol(curve, 63); sv > cfg.VolTarget {
				exposure = cfg.VolTarget / sv
			}
		}
		for k := range w {
			w[k] = w[k] / sum * exposure
		}

		// Mark, rebalance, charge costs.
		mark := func(i int) float64 {
			v := 0.0
			for tk, sh := range shares {
				if px := closes[tk][i]; !math.IsNaN(px) {
					v += sh * px
				}
			}
			return v
		}
		cash := equity - mark(t)
		turnover := 0.0
		newShares := map[domain.Ticker]float64{}
		for tk, wt := range w {
			px := closes[tk][t]
			if math.IsNaN(px) || px <= 0 {
				continue
			}
			target := equity * wt
			cur := shares[tk] * px
			turnover += math.Abs(target - cur)
			newShares[tk] = target / px
		}
		for tk, sh := range shares {
			if _, keep := w[tk]; !keep {
				if px := closes[tk][t]; !math.IsNaN(px) {
					turnover += math.Abs(sh * px)
				}
			}
		}
		cost := turnover * perSide
		shares = newShares
		invested := 0.0
		for tk, sh := range shares {
			invested += sh * closes[tk][t]
		}
		cash = equity - invested - cost
		equity -= cost

		// Hold to the next month end, marking daily.
		next := ends[mi+1]
		startEq := equity
		for i := t + 1; i <= next; i++ {
			eq := cash
			for tk, sh := range shares {
				px := closes[tk][i]
				if math.IsNaN(px) {
					px = lastKnown(closes[tk], i)
				}
				eq += sh * px
			}
			equity = eq
			curve = append(curve, domain.EquityPoint{At: dates[i], Equity: equity, Cash: cash, Exposure: exposure})
		}
		bRet := closes[cfg.Benchmark][next]/closes[cfg.Benchmark][t] - 1
		spread := sliceReturn(top, closes, t, next) - sliceReturn(bottom, closes, t, next)

		month := RankMonth{At: at, Universe: len(pool), Turnover: turnover / startEq, Cost: cost, Exposure: exposure,
			Return: equity/startEq - 1, Benchmark: bRet, Spread: spread}
		for _, c := range top {
			month.Holdings = append(month.Holdings, RankHolding{Ticker: c.t, Weight: w[c.t], Score: c.score})
		}
		res.Months = append(res.Months, month)
		res.TotalCosts += cost
	}
	if len(res.Months) < 12 {
		return res, fmt.Errorf("rank: only %d rebalances inside the window; need at least a year", len(res.Months))
	}
	res.Start, res.End = res.Months[0].At, curve[len(curve)-1].At
	res.Metrics = ComputeMetrics(curve, nil, cfg.RiskFree)
	res.Metrics.TotalCosts = round2(res.TotalCosts)
	if cfg.Trials > 1 {
		res.Metrics.TrialsConsidered = cfg.Trials
		res.Metrics.DeflatedSharpe = DeflatedSharpe(res.Metrics.Sharpe, cfg.Trials, len(toDaily(curve)))
	}

	// Benchmark over the same span.
	var bcurve []domain.EquityPoint
	b0 := closes[cfg.Benchmark][idx[res.Months[0].At]]
	for i := idx[res.Months[0].At]; i < len(dates) && !dates[i].After(res.End); i++ {
		if px := closes[cfg.Benchmark][i]; !math.IsNaN(px) {
			bcurve = append(bcurve, domain.EquityPoint{At: dates[i], Equity: cfg.StartingCash * px / b0, Exposure: 1})
		}
	}
	res.Benchmark = ComputeMetrics(bcurve, nil, cfg.RiskFree)

	// Monthly statistics.
	var ex, sp []float64
	hits := 0
	turn := 0.0
	byYear := map[int][2]float64{}
	for _, m := range res.Months {
		ex = append(ex, m.Return-m.Benchmark)
		sp = append(sp, m.Spread)
		if m.Return > m.Benchmark {
			hits++
		}
		turn += m.Turnover
		y := byYear[m.At.Year()]
		if y == [2]float64{} {
			y = [2]float64{1, 1}
		}
		y[0] *= 1 + m.Return
		y[1] *= 1 + m.Benchmark
		byYear[m.At.Year()] = y
	}
	res.ExcessMonthly, res.ExcessTStat = meanT(ex)
	res.SpreadMonthly, res.SpreadTStat = meanT(sp)
	res.HitRate = float64(hits) / float64(len(res.Months))
	res.AvgTurnover = turn / float64(len(res.Months))
	years := make([]int, 0, len(byYear))
	for y := range byYear {
		years = append(years, y)
	}
	sort.Ints(years)
	res.WorstYear = math.Inf(1)
	for _, y := range years {
		v := byYear[y]
		res.ByYear = append(res.ByYear, RankYear{Year: y, Sleeve: v[0] - 1, Benchmark: v[1] - 1})
		res.WorstYear = math.Min(res.WorstYear, v[0]-1)
	}
	res.Current = res.Months[len(res.Months)-1].Holdings

	res.Notes = append(res.Notes,
		fmt.Sprintf("formation %s, top %.0f%% of the membership-gated universe (%d–%d names), rebalanced at each month end, held one month", cfg.Signal, cfg.TopFraction*100, cfg.MinNames, cfg.MaxNames),
		fmt.Sprintf("costs of %.1f bps per side (%.1fx the configured model) charged on every rebalance; the long-short spread is gross", perSide*10000, cfg.CostMultiple),
		"nothing is fitted: the formation window and the slice are fixed in advance, so every month is out of sample; the deflated Sharpe accounts for the number of variants tried",
		"survivorship: names absent from the data are absent, not losing; membership gating removes the pre-inclusion bias only",
	)
	if cfg.InverseVol {
		res.Notes = append(res.Notes, "names are weighted by inverse trailing volatility")
	}
	if cfg.VolTarget > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("the sleeve's exposure is scaled by min(1, %.0f%% / its trailing 63-day volatility)", cfg.VolTarget*100))
	}
	return res, nil
}

// formation returns the signal score and trailing volatility at index t.
func formation(sig RankSignal, row []float64, t int) (score, vol float64, ok bool) {
	px := func(i int) float64 {
		if i < 0 || i >= len(row) {
			return math.NaN()
		}
		return row[i]
	}
	// Trailing 126-day volatility from log returns.
	var rets []float64
	for i := t - 125; i <= t; i++ {
		a, b := px(i-1), px(i)
		if !math.IsNaN(a) && !math.IsNaN(b) && a > 0 {
			rets = append(rets, math.Log(b/a))
		}
	}
	if len(rets) < 100 {
		return 0, 0, false
	}
	m := 0.0
	for _, r := range rets {
		m += r
	}
	m /= float64(len(rets))
	ss := 0.0
	for _, r := range rets {
		ss += (r - m) * (r - m)
	}
	vol = math.Sqrt(ss/float64(len(rets)-1)) * math.Sqrt(252)
	ret := func(lookback int) (float64, bool) {
		a, b := px(t-lookback), px(t-21)
		if math.IsNaN(a) || math.IsNaN(b) || a <= 0 {
			return 0, false
		}
		return b/a - 1, true
	}
	switch sig {
	case Momentum12_1:
		r, ok := ret(252)
		return r, vol, ok
	case Momentum6_1:
		r, ok := ret(126)
		return r, vol, ok
	case MomentumVolAdj:
		r, ok := ret(252)
		if !ok || vol <= 0 {
			return 0, vol, false
		}
		return r / vol, vol, true
	case LowVol:
		return -vol, vol, true
	}
	return 0, 0, false
}

// rankCand is one name at a formation date.
type rankCand struct {
	t     domain.Ticker
	score float64
	vol   float64
}

// sliceReturn is the equal-weight gross return of a slice over a month.
func sliceReturn(slice []rankCand, closes map[domain.Ticker][]float64, t, next int) float64 {
	sum, n := 0.0, 0
	for _, c := range slice {
		a, b := closes[c.t][t], closes[c.t][next]
		if math.IsNaN(b) {
			b = lastKnown(closes[c.t], next)
		}
		if !math.IsNaN(a) && !math.IsNaN(b) && a > 0 {
			sum += b/a - 1
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func lastKnown(row []float64, i int) float64 {
	for ; i >= 0; i-- {
		if !math.IsNaN(row[i]) {
			return row[i]
		}
	}
	return math.NaN()
}

func sleeveVol(curve []domain.EquityPoint, n int) float64 {
	if len(curve) < n+1 {
		return 0
	}
	var rets []float64
	for i := len(curve) - n; i < len(curve); i++ {
		if curve[i-1].Equity > 0 {
			rets = append(rets, curve[i].Equity/curve[i-1].Equity-1)
		}
	}
	_, sd := meanStd(rets)
	return sd * math.Sqrt(252)
}

func meanT(v []float64) (mean, t float64) {
	if len(v) < 2 {
		return 0, 0
	}
	m, sd := meanStd(v)
	if sd == 0 {
		return m, 0
	}
	return m, m / (sd / math.Sqrt(float64(len(v))))
}

// Package backtest is the deterministic event-driven simulator described in
// ADR-007.
//
// It replays historical bars into the *production* engines via
// internal/pipeline, so what is backtested is what runs. Determinism is
// structural: a simulated clock, an explicit seed, sorted event ordering and a
// single-threaded core. Bias prevention is enforced rather than documented: the
// leakage guard aborts a run rather than warning about it.
package backtest

import (
	"math"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// tradingDaysPerYear is the annualisation constant used throughout. It is
// stated once, here, rather than being re-derived inconsistently.
const tradingDaysPerYear = 252

// ComputeMetrics derives the standard performance report from an equity curve
// and a trade list.
//
// The equity curve drives the return statistics and the trade list drives the
// trade statistics; conflating them is a common source of nonsense numbers
// (for example a "win rate" computed from daily equity changes).
func ComputeMetrics(curve []domain.EquityPoint, trades []domain.BacktestTrade, riskFreeAnnual float64) domain.BacktestMetrics {
	var m domain.BacktestMetrics
	if len(curve) < 2 {
		return m
	}
	sort.SliceStable(curve, func(i, j int) bool { return curve[i].At.Before(curve[j].At) })

	start, end := curve[0].Equity, curve[len(curve)-1].Equity
	if start > 0 {
		m.TotalReturn = round4(end/start - 1)
	}

	// Sample the curve to daily observations before computing risk statistics.
	// Annualising minute-level standard deviation by sqrt(252) is a classic way
	// to produce a Sharpe ratio that means nothing.
	daily := toDaily(curve)
	rets := returnsOf(daily)

	years := curve[len(curve)-1].At.Sub(curve[0].At).Hours() / 24 / 365.25
	if years > 0 && start > 0 && end > 0 {
		m.CAGR = round4(math.Pow(end/start, 1/years) - 1)
	}

	if len(rets) > 1 {
		mean, sd := meanStd(rets)
		rfDaily := riskFreeAnnual / tradingDaysPerYear
		m.Volatility = round4(sd * math.Sqrt(tradingDaysPerYear))
		if sd > 0 {
			m.Sharpe = round4((mean - rfDaily) / sd * math.Sqrt(tradingDaysPerYear))
		}
		if dd := downsideDeviation(rets, rfDaily); dd > 0 {
			m.Sortino = round4((mean - rfDaily) / dd * math.Sqrt(tradingDaysPerYear))
		}
	}

	maxDD, ddDays := drawdown(curve)
	m.MaxDrawdown = round4(maxDD)
	m.MaxDrawdownDays = round2(ddDays)
	if maxDD > 0 {
		m.Calmar = round4(m.CAGR / maxDD)
	}

	exposure := 0.0
	for _, p := range curve {
		if p.Equity > 0 {
			exposure += p.Exposure / p.Equity
		}
	}
	m.AvgExposure = round4(exposure / float64(len(curve)))

	// Trade statistics.
	m.Trades = len(trades)
	if len(trades) > 0 {
		wins, grossWin, grossLoss, totalPnL, hold, costs := 0, 0.0, 0.0, 0.0, 0.0, 0.0
		for _, t := range trades {
			totalPnL += t.PnL
			costs += t.Costs
			hold += t.HoldPeriod.Hours()
			if t.PnL > 0 {
				wins++
				grossWin += t.PnL
			} else {
				grossLoss += -t.PnL
			}
		}
		m.WinRate = round4(float64(wins) / float64(len(trades)))
		if grossLoss > 0 {
			m.ProfitFactor = round4(grossWin / grossLoss)
		} else if grossWin > 0 {
			m.ProfitFactor = math.Inf(1)
		}
		m.Expectancy = round4(totalPnL / float64(len(trades)))
		m.AvgHoldHours = round2(hold / float64(len(trades)))
		m.TotalCosts = round2(costs)

		// Turnover: traded notional against average equity, annualised.
		traded := 0.0
		for _, t := range trades {
			traded += math.Abs(t.EntryPrice*t.Quantity) + math.Abs(t.ExitPrice*t.Quantity)
		}
		avgEquity := 0.0
		for _, p := range curve {
			avgEquity += p.Equity
		}
		avgEquity /= float64(len(curve))
		if avgEquity > 0 && years > 0 {
			m.Turnover = round2(traded / avgEquity / years)
		}
	}
	return m
}

// DeflatedSharpe adjusts an observed Sharpe ratio for the number of
// configurations that were tried.
//
// Reporting it is not optional politeness: with enough parameter sweeps a
// random strategy produces an impressive Sharpe, and a backtest report that
// omits the trial count is not a measurement.
func DeflatedSharpe(observed float64, trials int, samples int) float64 {
	if trials <= 1 || samples <= 1 {
		return round4(observed)
	}
	// Expected maximum of `trials` standard normals (Bailey & López de Prado's
	// approximation), scaled to the estimator's standard error.
	const euler = 0.5772156649
	n := float64(trials)
	expectedMax := (1-euler)*qNorm(1-1/n) + euler*qNorm(1-1/(n*math.E))
	se := math.Sqrt(1 / float64(samples-1))
	return round4(observed - expectedMax*se)
}

// qNorm is the inverse normal CDF (Acklam's rational approximation), accurate
// to about 1.15e-9 in the central region.
func qNorm(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00}
	const plow, phigh = 0.02425, 1 - 0.02425

	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > phigh:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	default:
		q := p - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	}
}

// toDaily reduces an equity curve to one observation per UTC day, taking the
// last sample of each day.
func toDaily(curve []domain.EquityPoint) []domain.EquityPoint {
	if len(curve) == 0 {
		return nil
	}
	var out []domain.EquityPoint
	currentDay := curve[0].At.UTC().Format("2006-01-02")
	last := curve[0]
	for _, p := range curve[1:] {
		day := p.At.UTC().Format("2006-01-02")
		if day != currentDay {
			out = append(out, last)
			currentDay = day
		}
		last = p
	}
	out = append(out, last)
	// A backtest shorter than two days has no daily statistics; fall back to
	// the raw curve so Sharpe is computed from *something* rather than nothing,
	// and let the sample count speak for itself.
	if len(out) < 3 {
		return curve
	}
	return out
}

func returnsOf(curve []domain.EquityPoint) []float64 {
	out := make([]float64, 0, len(curve))
	for i := 1; i < len(curve); i++ {
		if curve[i-1].Equity > 0 {
			out = append(out, curve[i].Equity/curve[i-1].Equity-1)
		}
	}
	return out
}

func meanStd(v []float64) (float64, float64) {
	if len(v) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	mean := sum / float64(len(v))
	if len(v) < 2 {
		return mean, 0
	}
	varsum := 0.0
	for _, x := range v {
		varsum += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(varsum / float64(len(v)-1))
}

func downsideDeviation(v []float64, target float64) float64 {
	sum, n := 0.0, 0
	for _, x := range v {
		if x < target {
			d := x - target
			sum += d * d
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(n))
}

func drawdown(curve []domain.EquityPoint) (float64, float64) {
	peak, maxDD := curve[0].Equity, 0.0
	var peakAt time.Time = curve[0].At
	var worstDuration float64
	for _, p := range curve {
		if p.Equity > peak {
			peak, peakAt = p.Equity, p.At
		}
		if peak > 0 {
			dd := (peak - p.Equity) / peak
			if dd > maxDD {
				maxDD = dd
			}
			if dd > 0 {
				d := p.At.Sub(peakAt).Hours() / 24
				if d > worstDuration {
					worstDuration = d
				}
			}
		}
	}
	return maxDD, worstDuration
}

// PartitionByRegime splits trades by the regime in force at entry and computes
// metrics for each. This is the answer to product question 9 — which strategies
// work under which conditions — and it is why every trade carries its regime.
func PartitionByRegime(curve []domain.EquityPoint, trades []domain.BacktestTrade, riskFree float64) map[domain.Regime]domain.BacktestMetrics {
	byRegime := map[domain.Regime][]domain.BacktestTrade{}
	for _, t := range trades {
		byRegime[t.Regime] = append(byRegime[t.Regime], t)
	}
	out := map[domain.Regime]domain.BacktestMetrics{}
	for rg, ts := range byRegime {
		if rg == "" {
			continue
		}
		m := ComputeMetrics(curve, ts, riskFree)
		// Return statistics from the full curve are not attributable to one
		// regime, so blank them and keep only the trade statistics.
		m.TotalReturn, m.CAGR, m.Sharpe, m.Sortino, m.Calmar = 0, 0, 0, 0, 0
		m.MaxDrawdown, m.MaxDrawdownDays, m.Volatility, m.AvgExposure = 0, 0, 0, 0
		out[rg] = m
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10000) / 10000
}

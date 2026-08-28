// Package valuation computes multiples and places them in historical and
// sector context.
//
// A multiple in isolation says very little: a P/E of 30 is cheap for one
// business and expensive for another. The engine therefore reports both the raw
// multiple and its percentile against the instrument's own history and its
// sector, and the score leans on the percentiles.
package valuation

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Multiple names for the availability map.
const (
	MultiplePE         = "pe"
	MultipleForwardPE  = "forward_pe"
	MultiplePEG        = "peg"
	MultipleEVEBITDA   = "ev_to_ebitda"
	MultiplePriceToFCF = "price_to_fcf"
	MultiplePriceSales = "price_to_sales"
	MultiplePriceBook  = "price_to_book"
)

// History supplies an instrument's own trailing multiples, so a percentile can
// be computed. An empty history simply means the percentile is unavailable.
type History interface {
	TrailingPE(ticker domain.Ticker, asOf time.Time) []float64
}

// SectorStats supplies peer-group multiples for the sector percentile.
type SectorStats interface {
	SectorPEs(sector string, asOf time.Time) []float64
}

// Input is everything the engine needs.
type Input struct {
	Stock   domain.Stock
	Price   float64
	AsOf    time.Time
	Periods []domain.Fundamental
	Metrics domain.FundamentalMetrics
	History History
	Sector  SectorStats
	// ForwardEPS is an optional analyst-consensus figure. When absent, forward
	// multiples are simply unavailable rather than estimated silently.
	ForwardEPS float64
}

// Compute derives valuation multiples and a 0..100 score.
func Compute(in Input, p domain.FundamentalPolicy) domain.ValuationMetrics {
	v := domain.ValuationMetrics{
		Ticker:    in.Stock.Ticker,
		AsOf:      in.AsOf,
		Price:     in.Price,
		Available: map[string]bool{},
	}
	if len(in.Periods) == 0 || in.Price <= 0 {
		v.Explanation = "valuation unavailable: no reported periods or no price"
		return v
	}

	ttm := trailing(in.Periods, 4)
	shares := in.Periods[0].SharesDiluted
	if shares <= 0 && in.Stock.SharesOut > 0 {
		shares = in.Stock.SharesOut
	}
	marketCap := in.Price * shares
	netDebt := in.Periods[0].TotalDebt - in.Periods[0].Cash
	ev := marketCap + netDebt

	set := func(name string, val float64, ok bool, dst *float64) {
		if !ok || math.IsNaN(val) || math.IsInf(val, 0) || val <= 0 {
			v.Available[name] = false
			return
		}
		*dst = round2(val)
		v.Available[name] = true
	}

	set(MultiplePE, in.Price/ttm.eps, ttm.eps > 0, &v.PE)
	set(MultipleForwardPE, in.Price/in.ForwardEPS, in.ForwardEPS > 0, &v.ForwardPE)
	set(MultiplePriceSales, marketCap/ttm.revenue, ttm.revenue > 0 && shares > 0, &v.PriceToSales)
	set(MultiplePriceToFCF, marketCap/ttm.fcf, ttm.fcf > 0 && shares > 0, &v.PriceToFCF)
	set(MultiplePriceBook, marketCap/in.Periods[0].TotalEquity, in.Periods[0].TotalEquity > 0 && shares > 0, &v.PriceToBook)

	// EBITDA approximated as operating income plus an assumed 4% of revenue in
	// depreciation, because the fixture data has no D&A line. The assumption is
	// stated in the explanation rather than buried.
	ebitda := ttm.operating + ttm.revenue*0.04
	set(MultipleEVEBITDA, ev/ebitda, ebitda > 0 && shares > 0, &v.EVToEBITDA)

	// PEG only makes sense with positive growth and a positive P/E.
	if v.Available[MultiplePE] && in.Metrics.Available["eps_growth"] && in.Metrics.EPSGrowth > 0.01 {
		set(MultiplePEG, v.PE/(in.Metrics.EPSGrowth*100), true, &v.PEG)
	} else {
		v.Available[MultiplePEG] = false
	}

	if ttm.eps > 0 {
		v.EarningsYield = round4(ttm.eps / in.Price)
	}
	if ttm.fcf > 0 && marketCap > 0 {
		v.FCFYield = round4(ttm.fcf / marketCap)
	}

	// Percentiles.
	if in.History != nil && v.Available[MultiplePE] {
		hist := in.History.TrailingPE(in.Stock.Ticker, in.AsOf)
		if len(hist) >= 8 {
			v.HistoricalPercentile = round4(percentileOf(hist, v.PE))
			v.HistoryPeriods = len(hist)
		}
	}
	if in.Sector != nil && v.Available[MultiplePE] {
		peers := in.Sector.SectorPEs(in.Stock.Sector, in.AsOf)
		if len(peers) >= 5 {
			v.SectorPercentile = round4(percentileOf(peers, v.PE))
		}
	}

	v.Score, v.Explanation = score(v, in, p)
	return v
}

type ttmAgg struct {
	revenue   float64
	operating float64
	net       float64
	eps       float64
	fcf       float64
	periods   int
}

func trailing(periods []domain.Fundamental, n int) ttmAgg {
	var a ttmAgg
	for i := 0; i < n && i < len(periods); i++ {
		p := periods[i]
		a.revenue += p.Revenue
		a.operating += p.OperatingIncome
		a.net += p.NetIncome
		a.eps += p.EPS
		a.fcf += p.FreeCashFlow
		a.periods++
	}
	return a
}

// score maps multiples to 0..100 where higher means cheaper relative to
// context. It is deliberately percentile-first: absolute multiple thresholds
// are the fastest way to build a value screen that never buys anything in a
// growth market and buys everything in a bear one.
func score(v domain.ValuationMetrics, in Input, p domain.FundamentalPolicy) (float64, string) {
	type part struct {
		score  float64
		weight float64
		note   string
	}
	var parts []part

	if v.HistoryPeriods > 0 {
		s := 100 * (1 - v.HistoricalPercentile)
		parts = append(parts, part{s, 0.35, fmt.Sprintf(
			"trades at the %.0fth percentile of its own trailing P/E history over %d periods",
			v.HistoricalPercentile*100, v.HistoryPeriods)})
	}
	if v.SectorPercentile > 0 {
		s := 100 * (1 - v.SectorPercentile)
		parts = append(parts, part{s, 0.20, fmt.Sprintf(
			"trades at the %.0fth percentile of %s sector P/E", v.SectorPercentile*100, in.Stock.Sector)})
	}
	if v.Available[MultiplePEG] {
		maxPEG := p.MaxPEG
		if maxPEG == 0 {
			maxPEG = 2.0
		}
		s := 100 * domain.Clamp01((maxPEG-v.PEG)/maxPEG)
		parts = append(parts, part{s, 0.20, fmt.Sprintf("PEG of %.2f against a policy maximum of %.2f", v.PEG, maxPEG)})
	}
	if v.Available[MultiplePE] {
		maxPE := p.MaxPE
		if maxPE == 0 {
			maxPE = 35
		}
		s := 100 * domain.Clamp01((maxPE*1.5-v.PE)/(maxPE*1.5-maxPE*0.3))
		parts = append(parts, part{s, 0.15, fmt.Sprintf("P/E of %.1f against a policy maximum of %.0f", v.PE, maxPE)})
	}
	if v.Available[MultipleEVEBITDA] {
		maxEV := p.MaxEVToEBITDA
		if maxEV == 0 {
			maxEV = 18
		}
		s := 100 * domain.Clamp01((maxEV*1.5-v.EVToEBITDA)/(maxEV*1.5-maxEV*0.3))
		parts = append(parts, part{s, 0.10, fmt.Sprintf("EV/EBITDA of %.1f (D&A assumed at 4%% of revenue)", v.EVToEBITDA)})
	}
	if v.FCFYield > 0 {
		s := 100 * domain.Clamp01(v.FCFYield/0.08)
		parts = append(parts, part{s, 0.15, fmt.Sprintf("free-cash-flow yield of %.1f%%", v.FCFYield*100)})
	}

	if len(parts) == 0 {
		return 0, "valuation unavailable: no multiple could be computed from the available reports"
	}
	sum, wsum := 0.0, 0.0
	notes := make([]string, 0, len(parts))
	for _, p := range parts {
		sum += p.score * p.weight
		wsum += p.weight
		notes = append(notes, p.note)
	}
	out := sum / wsum

	// A strategy that explicitly does not prefer cheapness (a momentum or
	// quality-growth strategy) should not be penalised for holding expensive
	// compounders, so the score is flattened toward neutral rather than
	// inverted, which would be a different claim entirely.
	if !p.PreferLowValuation {
		out = 50 + (out-50)*0.5
	}

	return math.Round(out*10) / 10, strings.Join(notes, "; ")
}

func percentileOf(sample []float64, v float64) float64 {
	if len(sample) == 0 {
		return 0
	}
	s := append([]float64(nil), sample...)
	sort.Float64s(s)
	count := 0
	for _, x := range s {
		if x <= v {
			count++
		}
	}
	return float64(count) / float64(len(s))
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// Package options derives options-surface intelligence where data is available.
//
// Two rules govern this package, both from the governance charter (G-10):
//
//  1. Every metric carries a data-quality marker and a confidence, and
//     confidence is bounded by data quality. A partial surface can never report
//     high confidence.
//  2. The package never asserts institutional positioning or intent. Open
//     interest and put/call ratios are observations about contracts, not about
//     who holds them or why. The Caveats field states this explicitly so that
//     downstream prose cannot quietly upgrade an observation into a claim.
package options

import (
	"math"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/features"
)

// Contract is one option in the observed chain.
type Contract struct {
	Strike       float64
	Expiry       time.Time
	Call         bool
	ImpliedVol   float64
	OpenInterest float64
	Volume       float64
	Delta        float64
	Gamma        float64
	Theta        float64
	Vega         float64
}

// Chain is the observed surface for one underlying at one instant.
type Chain struct {
	Ticker    domain.Ticker
	AsOf      time.Time
	Spot      float64
	Contracts []Contract
	// Source records where the chain came from, which feeds data quality.
	Source string
}

// Engine computes options metrics and maintains the implied-volatility history
// needed for IV rank.
type Engine struct {
	ivHistory map[domain.Ticker]*features.Window
	window    int
}

// NewEngine builds an options engine retaining `window` IV observations per
// symbol (252 trading days is the conventional IV-rank lookback).
func NewEngine(window int) *Engine {
	if window <= 0 {
		window = 252
	}
	return &Engine{ivHistory: map[domain.Ticker]*features.Window{}, window: window}
}

// Compute derives metrics from a chain. An empty or thin chain yields
// DataQuality "none"/"partial" and a correspondingly capped confidence.
func (e *Engine) Compute(c Chain) domain.OptionsMetrics {
	m := domain.OptionsMetrics{Ticker: c.Ticker, AsOf: c.AsOf, DataQuality: "none"}
	m.Caveats = []string{
		"open interest and volume describe contracts outstanding and traded; they do not identify who holds a position or why",
		"implied volatility is derived from observed prices and is not a forecast",
	}
	if len(c.Contracts) == 0 {
		m.Confidence = 0
		return m
	}

	var (
		callOI, putOI             float64
		callVol, putVol           float64
		ivWeighted, ivW           float64
		delta, gamma, theta, vega float64
		nearAtm                   int
		call25, put25             float64
		have25c, have25p          bool
	)
	for _, k := range c.Contracts {
		if k.Call {
			callOI += k.OpenInterest
			callVol += k.Volume
		} else {
			putOI += k.OpenInterest
			putVol += k.Volume
		}
		// Weight implied volatility toward at-the-money, where the surface is
		// most reliable and least distorted by wide quotes on far strikes.
		if c.Spot > 0 && k.ImpliedVol > 0 {
			moneyness := math.Abs(k.Strike/c.Spot - 1)
			w := math.Exp(-math.Pow(moneyness/0.05, 2))
			ivWeighted += k.ImpliedVol * w
			ivW += w
			if moneyness < 0.02 {
				nearAtm++
			}
		}
		delta += k.Delta * k.OpenInterest
		gamma += k.Gamma * k.OpenInterest
		theta += k.Theta * k.OpenInterest
		vega += k.Vega * k.OpenInterest

		// 25-delta skew inputs.
		if k.ImpliedVol > 0 {
			if k.Call && math.Abs(k.Delta-0.25) < 0.06 {
				call25, have25c = k.ImpliedVol, true
			}
			if !k.Call && math.Abs(k.Delta+0.25) < 0.06 {
				put25, have25p = k.ImpliedVol, true
			}
		}
	}

	if ivW > 0 {
		m.ImpliedVol = round4(ivWeighted / ivW)
	}
	m.OpenInterest = callOI + putOI
	m.Volume = callVol + putVol
	if callOI > 0 {
		m.PutCallRatio = round4(putOI / callOI)
	}
	m.Delta, m.Gamma, m.Theta, m.Vega = round4(delta), round4(gamma), round4(theta), round4(vega)
	if have25c && have25p {
		m.SkewPct = round4(put25 - call25)
	}

	// IV rank against this symbol's own trailing implied volatility.
	if m.ImpliedVol > 0 {
		w, ok := e.ivHistory[c.Ticker]
		if !ok {
			w = features.NewWindow(e.window)
			e.ivHistory[c.Ticker] = w
		}
		if w.Len() >= 20 {
			lo, hi := w.Min(), w.Max()
			if hi > lo {
				m.IVRank = round4(domain.Clamp01((m.ImpliedVol - lo) / (hi - lo)))
			}
			m.IVPercentile = round4(w.RankOf(m.ImpliedVol))
		}
		w.Push(m.ImpliedVol)
	}

	// Data quality drives the confidence ceiling.
	switch {
	case len(c.Contracts) >= 40 && nearAtm >= 2 && m.OpenInterest > 0:
		m.DataQuality = "good"
		m.Confidence = 0.80
	case len(c.Contracts) >= 10 && m.OpenInterest > 0:
		m.DataQuality = "partial"
		m.Confidence = 0.45
		m.Caveats = append(m.Caveats, "chain is thin: fewer than 40 contracts observed, so surface metrics are indicative only")
	default:
		m.DataQuality = "partial"
		m.Confidence = 0.20
		m.Caveats = append(m.Caveats, "chain is very thin: metrics should not be relied upon")
	}
	if e.ivHistory[c.Ticker] == nil || e.ivHistory[c.Ticker].Len() < 20 {
		m.Caveats = append(m.Caveats, "IV rank requires at least 20 observations of history and is unavailable")
		m.Confidence = math.Min(m.Confidence, 0.5)
	}
	return m
}

// Empty returns an explicitly-unavailable metrics record, which is what the
// platform uses when no options provider is configured. It is deliberately not
// a zero value: "we have no options data" is information the dashboard shows.
func Empty(t domain.Ticker, asOf time.Time) domain.OptionsMetrics {
	return domain.OptionsMetrics{
		Ticker: t, AsOf: asOf, DataQuality: "none", Confidence: 0,
		Caveats: []string{"no options data source is configured for this deployment"},
	}
}

func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10000) / 10000
}

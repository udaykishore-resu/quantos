package backtest

import (
	"math"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Why this file exists: a replayed history has bars and no quotes, and the
// risk engine refuses to pass a spread check it cannot evaluate — correctly,
// since an unmeasured risk is not an absent one. Without a quote every
// candidate in every backtest is WATCH_ONLY and the backtester can never
// trade, which is a silent way for a platform to look prudent.
//
// What was tried first, and why it is not here: the Corwin–Schultz high-low
// and Abdi–Ranaldo close-high-low estimators recover a bid-ask spread from
// daily bars. Both take a square root of a windowed average, so on a liquid
// name whose true spread is a few basis points the estimate is dominated by
// sampling noise and reads tens of basis points — enough to put every large
// cap on WATCH_ONLY for a reason that is not real. Daily bars cannot resolve
// a 3 bps spread, and pretending otherwise is a measurement in name only.
//
// What is here instead is a liquidity proxy: the spread implied by traded
// notional, measured from the bars themselves. It is a stated assumption —
// documented, monotone and deliberately conservative — rather than a noisy
// estimate dressed as data. It separates the illiquid names the check exists
// to catch from the ones it should pass, and it never goes below the cost
// model's own spread assumption, so the risk check is never looser than the
// cost the paper broker already charges.

// notionalWindow is the number of bars of close×volume averaged.
const notionalWindow = 20

// spreadFromNotional maps average daily traded notional (USD) to a quoted
// spread in basis points of the mid:
//
//	spread ≈ 2 + 300 / sqrt(notional in $M)
//
// which puts a $10B/day name at ~5 bps, $1B at ~11, $100M at ~32 (the risk
// engine's soft limit is 25) and $10M at ~97 (hard limit 80). The constants
// are an assumption about US large- and mid-cap equities, not a fit; the
// shape, with spread falling as the inverse square root of activity, is the
// standard one.
func spreadFromNotional(notional float64) float64 {
	if notional <= 0 || math.IsNaN(notional) {
		return 0
	}
	return 2 + 300/math.Sqrt(notional/1e6)
}

// spreadEstimator keeps a rolling traded-notional window per instrument.
type spreadEstimator struct {
	notional map[domain.Ticker][]float64
}

func newSpreadEstimator() *spreadEstimator {
	return &spreadEstimator{notional: map[domain.Ticker][]float64{}}
}

// observe folds one more bar in and returns the implied spread in bps, or
// zero when the bar carries no volume (an index level, say), which the
// caller floors.
func (e *spreadEstimator) observe(cur domain.Candle) float64 {
	if cur.Close <= 0 || cur.Volume <= 0 {
		return 0
	}
	h := append(e.notional[cur.Ticker], cur.Close*cur.Volume)
	if len(h) > notionalWindow {
		h = h[len(h)-notionalWindow:]
	}
	e.notional[cur.Ticker] = h
	sum := 0.0
	for _, v := range h {
		sum += v
	}
	return spreadFromNotional(sum / float64(len(h)))
}

// syntheticQuote builds the two-sided quote a replayed bar implies: last at
// the close, bid and ask a half-spread either side, timestamped at the bar
// close so staleness is measured from the bar rather than from nothing. The
// floor is the cost model's own spread assumption.
func syntheticQuote(cur domain.Candle, spread, floorBps float64) domain.Quote {
	if spread < floorBps {
		spread = floorBps
	}
	half := cur.Close * spread / 10000 / 2
	return domain.Quote{
		Ticker: cur.Ticker, Bid: cur.Close - half, Ask: cur.Close + half, Last: cur.Close,
		Volume: cur.Volume, Timestamp: cur.End, Source: "replay-liquidity-proxy",
	}
}

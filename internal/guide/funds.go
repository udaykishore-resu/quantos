package guide

import "github.com/udaykishoreresu/quantos/internal/domain"

// Index funds a personal book commonly holds, with enough about each to treat
// it as what it is: a basket, not a single name.
//
// Without this, a fund outside the research universe is marked as a stock of
// unknown sector, and a book that is half S&P 500 fails the single-name cap on
// its most diversified holding.

// LookThroughAsOf dates the constituent weights below. They are approximate
// and they drift: the largest S&P 500 weights move by a point or more in a
// year. They are good enough to answer "how much of my money rides on this
// company", which is the question they exist for, and not good enough for
// anything that needs the second decimal.
const LookThroughAsOf = "2026-06 (approximate)"

// FundWeight is one constituent of a fund.
type FundWeight struct {
	Ticker domain.Ticker
	Weight float64
}

var sp500Top = []FundWeight{
	{"NVDA", 0.075}, {"MSFT", 0.065}, {"AAPL", 0.060}, {"AMZN", 0.040}, {"META", 0.030},
	{"AVGO", 0.025}, {"GOOGL", 0.022}, {"GOOG", 0.018}, {"TSLA", 0.018}, {"BRK.B", 0.016},
	{"JPM", 0.015}, {"LLY", 0.013}, {"V", 0.011}, {"XOM", 0.009}, {"UNH", 0.008},
}

var nasdaq100Top = []FundWeight{
	{"NVDA", 0.090}, {"MSFT", 0.080}, {"AAPL", 0.075}, {"AMZN", 0.055}, {"AVGO", 0.050},
	{"META", 0.035}, {"TSLA", 0.030}, {"GOOGL", 0.025}, {"GOOG", 0.024}, {"COST", 0.025},
	{"NFLX", 0.020},
}

// Total-market funds hold the S&P 500 at roughly 85% of their weight.
func scaled(ws []FundWeight, k float64) []FundWeight {
	out := make([]FundWeight, len(ws))
	for i, w := range ws {
		out[i] = FundWeight{w.Ticker, w.Weight * k}
	}
	return out
}

var totalMarketTop = scaled(sp500Top, 0.85)

type fund struct {
	name, sector string
	top          []FundWeight
}

var knownFunds = map[domain.Ticker]fund{
	"SPY":  {"SPDR S&P 500 ETF", "Broad Market", sp500Top},
	"VOO":  {"Vanguard S&P 500 ETF", "Broad Market", sp500Top},
	"IVV":  {"iShares Core S&P 500 ETF", "Broad Market", sp500Top},
	"SPLG": {"SPDR Portfolio S&P 500 ETF", "Broad Market", sp500Top},
	"VTI":  {"Vanguard Total Stock Market ETF", "Broad Market", totalMarketTop},
	"ITOT": {"iShares Core S&P Total U.S. Stock Market ETF", "Broad Market", totalMarketTop},
	"SCHB": {"Schwab U.S. Broad Market ETF", "Broad Market", totalMarketTop},
	"QQQ":  {"Invesco QQQ Trust", "Nasdaq 100", nasdaq100Top},
	"QQQM": {"Invesco NASDAQ 100 ETF", "Nasdaq 100", nasdaq100Top},
	"RSP":  {"Invesco S&P 500 Equal Weight ETF", "Broad Market", nil},
	"VT":   {"Vanguard Total World Stock ETF", "Global", scaled(sp500Top, 0.55)},
	"VXUS": {"Vanguard Total International Stock ETF", "International", nil},
	"VEA":  {"Vanguard FTSE Developed Markets ETF", "International", nil},
	"VWO":  {"Vanguard FTSE Emerging Markets ETF", "International", nil},
	"BND":  {"Vanguard Total Bond Market ETF", "Bonds", nil},
	"AGG":  {"iShares Core U.S. Aggregate Bond ETF", "Bonds", nil},
	"SCHD": {"Schwab U.S. Dividend Equity ETF", "Broad Market", nil},
	"VUG":  {"Vanguard Growth ETF", "Large Growth", nil},
	"VGT":  {"Vanguard Information Technology ETF", "Technology", nil},
}

// KnownFund describes a common index fund, if t is one.
func KnownFund(t domain.Ticker) (domain.Stock, bool) {
	f, ok := knownFunds[t]
	if !ok {
		return domain.Stock{}, false
	}
	return domain.Stock{Ticker: t, Company: f.name, Sector: f.sector, AssetClass: domain.AssetETF}, true
}

// FundHoldings returns the fund's largest constituents, or nil when they are
// not tracked (equal-weight, international and bond funds hold no single
// company at a weight that matters here).
func FundHoldings(t domain.Ticker) []FundWeight { return knownFunds[t].top }

// Exposure is how much of the book rides on one company, counting the shares
// held directly and the share of every index fund that holds it.
type Exposure struct {
	Ticker   domain.Ticker   `json:"ticker"`
	Direct   float64         `json:"direct"`
	ViaFunds float64         `json:"via_funds"`
	Total    float64         `json:"total"`
	Funds    []domain.Ticker `json:"funds,omitempty"`
}

// PersonalCaps are thresholds for a personal book of index funds and a few
// stocks, as opposed to the strategy's caps, which assume a twenty-name
// signal book. They are not advice; they are where the report starts asking
// whether a concentration is deliberate.
//
//   - one company, counted through funds: 25% fails, 15% warns
//   - one sector across single names: 40% fails, 30% warns
//   - a peak-to-trough fall at today's weights over the last year: 35% / 25%
func PersonalCaps() Caps {
	return Caps{
		MaxPosition: 0.25, SoftPosition: 0.15,
		MaxSector: 0.40, SoftSector: 0.30,
		MaxCorrelated: 0.50, SoftCorrelated: 0.35,
		MaxDrawdown: 0.35, DrawdownWarn: 0.25,
		CorrelationThreshold: 0.60,
	}
}

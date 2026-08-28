package domain

import "time"

// Fundamental is one reporting period's financial snapshot.
//
// PeriodEnd and FiledAt are both required: only rows with FiledAt <= decision
// time are visible to a backtest, which is how restatement bias is avoided
// (ADR-007).
type Fundamental struct {
	Ticker    Ticker    `json:"ticker"`
	PeriodEnd time.Time `json:"period_end"`
	FiledAt   time.Time `json:"filed_at"`
	Period    string    `json:"period"` // "Q1-2026", "FY2025"
	Currency  string    `json:"currency"`

	Revenue           float64 `json:"revenue"`
	RevenueYoY        float64 `json:"revenue_yoy"`
	GrossProfit       float64 `json:"gross_profit"`
	OperatingIncome   float64 `json:"operating_income"`
	NetIncome         float64 `json:"net_income"`
	EPS               float64 `json:"eps"`
	EPSYoY            float64 `json:"eps_yoy"`
	FreeCashFlow      float64 `json:"free_cash_flow"`
	OperatingCashFlow float64 `json:"operating_cash_flow"`
	CapEx             float64 `json:"capex"`

	TotalAssets        float64 `json:"total_assets"`
	TotalEquity        float64 `json:"total_equity"`
	TotalDebt          float64 `json:"total_debt"`
	Cash               float64 `json:"cash"`
	CurrentAssets      float64 `json:"current_assets"`
	CurrentLiabilities float64 `json:"current_liabilities"`
	InvestedCapital    float64 `json:"invested_capital"`

	SharesDiluted float64 `json:"shares_diluted"`
	Restated      bool    `json:"restated"`
	Source        string  `json:"source"`
}

// FundamentalMetrics is the derived ratio set. Every field is a pointer-free
// float with a companion presence flag in Available, because "0" and "unknown"
// are different and conflating them silently corrupts scores.
type FundamentalMetrics struct {
	Ticker  Ticker    `json:"ticker"`
	AsOf    time.Time `json:"as_of"`
	BasedOn string    `json:"based_on"` // period label

	RevenueGrowth   float64 `json:"revenue_growth"`
	EPSGrowth       float64 `json:"eps_growth"`
	GrossMargin     float64 `json:"gross_margin"`
	OperatingMargin float64 `json:"operating_margin"`
	NetMargin       float64 `json:"net_margin"`
	FCFMargin       float64 `json:"fcf_margin"`
	ROE             float64 `json:"roe"`
	ROIC            float64 `json:"roic"`
	DebtToEquity    float64 `json:"debt_to_equity"`
	NetDebtToEBITDA float64 `json:"net_debt_to_ebitda"`
	CurrentRatio    float64 `json:"current_ratio"`
	InterestCover   float64 `json:"interest_coverage"`
	// EarningsConsistency is the fraction of the last N periods with positive
	// year-over-year EPS growth, in [0,1].
	EarningsConsistency float64 `json:"earnings_consistency"`
	// Accruals is (net income - operating cash flow) / total assets. High
	// positive accruals are a well-documented earnings-quality warning.
	Accruals float64 `json:"accruals"`

	Available map[string]bool `json:"available"`
	Periods   int             `json:"periods"`
}

// Has reports whether a named metric was computable.
func (m FundamentalMetrics) Has(name string) bool { return m.Available[name] }

// ValuationMetrics holds multiples and their historical context.
type ValuationMetrics struct {
	Ticker Ticker    `json:"ticker"`
	AsOf   time.Time `json:"as_of"`
	Price  float64   `json:"price"`

	PE            float64 `json:"pe"`
	ForwardPE     float64 `json:"forward_pe"`
	PEG           float64 `json:"peg"`
	EVToEBITDA    float64 `json:"ev_to_ebitda"`
	PriceToFCF    float64 `json:"price_to_fcf"`
	PriceToSales  float64 `json:"price_to_sales"`
	PriceToBook   float64 `json:"price_to_book"`
	EarningsYield float64 `json:"earnings_yield"`
	FCFYield      float64 `json:"fcf_yield"`

	// Percentile of the primary multiple against the instrument's own history,
	// in [0,1]. 0.9 means "more expensive than 90% of its own history".
	HistoricalPercentile float64 `json:"historical_percentile"`
	SectorPercentile     float64 `json:"sector_percentile"`
	HistoryPeriods       int     `json:"history_periods"`

	Available map[string]bool `json:"available"`

	Score       float64 `json:"valuation_score"`
	Explanation string  `json:"valuation_explanation"`
}

// OptionsMetrics captures the options surface where data is available.
// Every field carries an explicit confidence and data-quality marker because
// options data is frequently sparse and unreliable (governance rule G-10).
type OptionsMetrics struct {
	Ticker Ticker    `json:"ticker"`
	AsOf   time.Time `json:"as_of"`

	ImpliedVol   float64 `json:"implied_vol"`
	IVRank       float64 `json:"iv_rank"` // 0..1 against trailing 52w
	IVPercentile float64 `json:"iv_percentile"`
	PutCallRatio float64 `json:"put_call_ratio"`
	OpenInterest float64 `json:"open_interest"`
	Volume       float64 `json:"volume"`
	SkewPct      float64 `json:"skew_25d"`

	// Aggregate greeks across the observed chain.
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`

	// DataQuality is one of "none", "partial", "good". Confidence is bounded by
	// it: a "partial" surface can never report high confidence.
	DataQuality string  `json:"data_quality"`
	Confidence  float64 `json:"confidence"`
	// Caveats explicitly lists what cannot be inferred, so downstream prose
	// cannot claim institutional positioning from open interest.
	Caveats []string `json:"caveats,omitempty"`
}

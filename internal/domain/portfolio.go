package domain

import "time"

// OrderSide is the direction of a simulated order.
type OrderSide string

const (
	OrderBuy  OrderSide = "BUY"
	OrderSell OrderSide = "SELL"
)

// OrderType enumerates supported simulated order types.
type OrderType string

const (
	OrderMarket OrderType = "MARKET"
	OrderLimit  OrderType = "LIMIT"
	OrderStop   OrderType = "STOP"
)

// OrderStatus is the lifecycle state of a simulated order.
type OrderStatus string

const (
	OrderPending   OrderStatus = "PENDING"
	OrderFilled    OrderStatus = "FILLED"
	OrderPartial   OrderStatus = "PARTIALLY_FILLED"
	OrderCancelled OrderStatus = "CANCELLED"
	OrderRejected  OrderStatus = "REJECTED"
)

// PaperOrder is a simulated order. There is no real-money counterpart anywhere
// in this repository (governance rule G-8).
type PaperOrder struct {
	ID          string `json:"id"`
	PortfolioID string `json:"portfolio_id"`
	// IdempotencyKey makes order submission safe under replay and retry.
	IdempotencyKey string    `json:"idempotency_key"`
	Ticker         Ticker    `json:"ticker"`
	Side           OrderSide `json:"side"`
	Type           OrderType `json:"type"`
	Quantity       float64   `json:"quantity"`
	LimitPrice     float64   `json:"limit_price,omitempty"`
	StopPrice      float64   `json:"stop_price,omitempty"`
	TimeInForce    string    `json:"time_in_force"`

	Status       OrderStatus `json:"status"`
	FilledQty    float64     `json:"filled_qty"`
	AvgFillPrice float64     `json:"avg_fill_price"`
	Commission   float64     `json:"commission"`
	SlippageBps  float64     `json:"slippage_bps"`
	RejectReason string      `json:"reject_reason,omitempty"`

	SignalID  string    `json:"signal_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Fills     []Fill    `json:"fills,omitempty"`
}

// Fill is a single simulated execution.
type Fill struct {
	Quantity    float64   `json:"quantity"`
	Price       float64   `json:"price"`
	Commission  float64   `json:"commission"`
	SlippageBps float64   `json:"slippage_bps"`
	At          time.Time `json:"at"`
}

// PaperPosition is an open simulated position.
type PaperPosition struct {
	PortfolioID   string    `json:"portfolio_id"`
	Ticker        Ticker    `json:"ticker"`
	Quantity      float64   `json:"quantity"` // negative for short
	AvgPrice      float64   `json:"avg_price"`
	MarketPrice   float64   `json:"market_price"`
	MarketValue   float64   `json:"market_value"`
	CostBasis     float64   `json:"cost_basis"`
	UnrealizedPnL float64   `json:"unrealized_pnl"`
	RealizedPnL   float64   `json:"realized_pnl"`
	OpenedAt      time.Time `json:"opened_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	SignalID      string    `json:"signal_id,omitempty"`
	Sector        string    `json:"sector,omitempty"`
}

// Portfolio is the simulated account.
type Portfolio struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Owner    string    `json:"owner"`
	Currency string    `json:"currency"`
	AsOf     time.Time `json:"as_of"`

	StartingCash  float64 `json:"starting_cash"`
	Cash          float64 `json:"cash"`
	Equity        float64 `json:"equity"`
	MarketValue   float64 `json:"market_value"`
	RealizedPnL   float64 `json:"realized_pnl"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
	TotalPnL      float64 `json:"total_pnl"`
	ReturnPct     float64 `json:"return_pct"`

	GrossExposure float64 `json:"gross_exposure"`
	NetExposure   float64 `json:"net_exposure"`
	LongExposure  float64 `json:"long_exposure"`
	ShortExposure float64 `json:"short_exposure"`
	Leverage      float64 `json:"leverage"`

	PeakEquity     float64            `json:"peak_equity"`
	Drawdown       float64            `json:"drawdown"`
	MaxDrawdown    float64            `json:"max_drawdown"`
	SectorExposure map[string]float64 `json:"sector_exposure,omitempty"`

	Positions []PaperPosition `json:"positions,omitempty"`
	Paper     bool            `json:"paper"` // always true
}

// EquityPoint is one sample of the equity curve.
type EquityPoint struct {
	At       time.Time `json:"at"`
	Equity   float64   `json:"equity"`
	Cash     float64   `json:"cash"`
	Drawdown float64   `json:"drawdown"`
	Exposure float64   `json:"exposure"`
}

// BacktestStatus is the lifecycle of a backtest run.
type BacktestStatus string

const (
	BacktestQueued    BacktestStatus = "QUEUED"
	BacktestRunning   BacktestStatus = "RUNNING"
	BacktestCompleted BacktestStatus = "COMPLETED"
	BacktestFailed    BacktestStatus = "FAILED"
	BacktestAborted   BacktestStatus = "ABORTED"
)

// BacktestRun is one deterministic historical simulation.
type BacktestRun struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Status     BacktestStatus `json:"status"`
	StrategyID string         `json:"strategy_id"`
	Universe   []Ticker       `json:"universe"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	Interval   Interval       `json:"interval"`

	// Determinism inputs. Given these, the run reproduces byte-for-byte.
	Seed         int64  `json:"seed"`
	ConfigHash   string `json:"config_hash"`
	ModelVersion string `json:"model_version"`
	CodeVersion  string `json:"code_version"`

	StartingCash float64   `json:"starting_cash"`
	Costs        CostModel `json:"costs"`

	Metrics       BacktestMetrics            `json:"metrics"`
	RegimeMetrics map[Regime]BacktestMetrics `json:"regime_metrics,omitempty"`
	EquityCurve   []EquityPoint              `json:"equity_curve,omitempty"`
	Trades        []BacktestTrade            `json:"trades,omitempty"`

	CreatedAt       time.Time `json:"created_at"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	Error           string    `json:"error,omitempty"`
	EventsProcessed int64     `json:"events_processed"`
	ResultHash      string    `json:"result_hash,omitempty"`
}

// CostModel parameterises transaction costs and slippage.
type CostModel struct {
	CommissionBps      float64 `json:"commission_bps"`
	CommissionPerShare float64 `json:"commission_per_share"`
	MinCommission      float64 `json:"min_commission"`
	SpreadCostBps      float64 `json:"spread_cost_bps"`
	SlippageModel      string  `json:"slippage_model"` // "fixed" | "sqrt_impact"
	SlippageBps        float64 `json:"slippage_bps"`
	ImpactCoef         float64 `json:"impact_coefficient"`
	BorrowRateAnnual   float64 `json:"borrow_rate_annual"`
	ParticipationCap   float64 `json:"participation_cap"` // fraction of bar volume
}

// DefaultCostModel is deliberately pessimistic. Optimistic cost assumptions are
// the most common way a backtest lies.
func DefaultCostModel() CostModel {
	return CostModel{
		CommissionBps:    1.0,
		MinCommission:    0.0,
		SpreadCostBps:    2.0,
		SlippageModel:    "sqrt_impact",
		SlippageBps:      1.5,
		ImpactCoef:       0.35,
		BorrowRateAnnual: 0.03,
		ParticipationCap: 0.05,
	}
}

// BacktestTrade is one round trip.
type BacktestTrade struct {
	Ticker     Ticker        `json:"ticker"`
	Side       Side          `json:"side"`
	EntryAt    time.Time     `json:"entry_at"`
	ExitAt     time.Time     `json:"exit_at"`
	EntryPrice float64       `json:"entry_price"`
	ExitPrice  float64       `json:"exit_price"`
	Quantity   float64       `json:"quantity"`
	PnL        float64       `json:"pnl"`
	ReturnPct  float64       `json:"return_pct"`
	Costs      float64       `json:"costs"`
	HoldPeriod time.Duration `json:"hold_period"`
	Regime     Regime        `json:"regime"`
	SignalID   string        `json:"signal_id,omitempty"`
	ExitReason string        `json:"exit_reason"`
}

// BacktestMetrics is the standard performance report.
type BacktestMetrics struct {
	TotalReturn     float64 `json:"total_return"`
	CAGR            float64 `json:"cagr"`
	Sharpe          float64 `json:"sharpe"`
	Sortino         float64 `json:"sortino"`
	Calmar          float64 `json:"calmar"`
	MaxDrawdown     float64 `json:"max_drawdown"`
	MaxDrawdownDays float64 `json:"max_drawdown_days"`
	Volatility      float64 `json:"volatility"`
	WinRate         float64 `json:"win_rate"`
	ProfitFactor    float64 `json:"profit_factor"`
	Expectancy      float64 `json:"expectancy"`
	Turnover        float64 `json:"turnover"`
	AvgExposure     float64 `json:"avg_exposure"`
	Trades          int     `json:"trades"`
	AvgHoldHours    float64 `json:"avg_hold_hours"`
	TotalCosts      float64 `json:"total_costs"`
	// DeflatedSharpe adjusts for the number of configurations tried; reported
	// on sweeps so that a lucky parameter set is visibly discounted.
	DeflatedSharpe   float64 `json:"deflated_sharpe,omitempty"`
	TrialsConsidered int     `json:"trials_considered,omitempty"`
}

// Package paper is the simulated broker and portfolio.
//
// There is no real-money counterpart anywhere in this repository. `Broker` is
// the only order sink, it has no network client, and guardRealMoney refuses to
// start if an operator sets the environment variable that a real implementation
// would need (governance rule G-8).
package paper

import (
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

func init() { guardRealMoney() }

// guardRealMoney makes the paper-only guarantee load-bearing rather than
// aspirational. If someone sets the flag a real-execution build would need,
// the process refuses to start rather than quietly ignoring it.
func guardRealMoney() {
	if v, ok := os.LookupEnv("QUANTOS_ALLOW_REAL_MONEY"); ok && v != "" && v != "0" && v != "false" {
		panic("QuantOS: QUANTOS_ALLOW_REAL_MONEY is set, but this build contains no " +
			"real-money execution path and never will. Unset the variable. " +
			"See docs/security/governance.md rule G-8.")
	}
}

// Config parameterises the simulated broker.
type Config struct {
	StartingCash float64
	MaxPositions int
	MaxWeight    float64
	AllowShort   bool
	Costs        domain.CostModel
	// FillDelay models the gap between decision and execution. Zero fills on
	// the next mark, which is already one bar later than the decision.
	FillDelay time.Duration
	Clock     obs.Clock
	Metrics   *obs.Metrics
	// Rand supplies the randomness used by the slippage model. Supplying it
	// explicitly is what keeps backtests reproducible.
	Rand func() float64
}

// DefaultConfig returns the shipped paper-trading parameters.
func DefaultConfig() Config {
	return Config{
		StartingCash: 100_000, MaxPositions: 20, MaxWeight: 0.10,
		AllowShort: false, Costs: domain.DefaultCostModel(),
		Clock: obs.SystemClock{},
	}
}

// Broker is a simulated execution venue and portfolio accountant.
type Broker struct {
	cfg Config

	mu        sync.RWMutex
	portfolio domain.Portfolio
	positions map[domain.Ticker]*domain.PaperPosition
	orders    map[string]*domain.PaperOrder
	// idempotency maps an idempotency key to the order it created.
	idempotency map[string]string
	pending     []*domain.PaperOrder
	marks       map[domain.Ticker]float64
	sectors     map[domain.Ticker]string
	equityCurve []domain.EquityPoint
	trades      []domain.BacktestTrade
	// openLots tracks entry context so a round trip can be reported.
	openLots map[domain.Ticker]lot
}

type lot struct {
	at       time.Time
	price    float64
	qty      float64
	signalID string
	regime   domain.Regime
}

// NewBroker builds a simulated broker.
func NewBroker(cfg Config, id, name string) *Broker {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.StartingCash <= 0 {
		cfg.StartingCash = 100_000
	}
	if cfg.Costs.ParticipationCap <= 0 {
		cfg.Costs = domain.DefaultCostModel()
	}
	if cfg.Rand == nil {
		// Deterministic by default: no unseeded randomness anywhere on a path
		// that a backtest can reach (ADR-007).
		cfg.Rand = func() float64 { return 0.5 }
	}
	now := cfg.Clock.Now()
	return &Broker{
		cfg: cfg,
		portfolio: domain.Portfolio{
			ID: id, Name: name, Currency: "USD", AsOf: now,
			StartingCash: cfg.StartingCash, Cash: cfg.StartingCash,
			Equity: cfg.StartingCash, PeakEquity: cfg.StartingCash,
			Paper: true, SectorExposure: map[string]float64{},
		},
		positions:   map[domain.Ticker]*domain.PaperPosition{},
		orders:      map[string]*domain.PaperOrder{},
		idempotency: map[string]string{},
		marks:       map[domain.Ticker]float64{},
		sectors:     map[domain.Ticker]string{},
		openLots:    map[domain.Ticker]lot{},
	}
}

// SetSector records an instrument's sector for exposure accounting.
func (b *Broker) SetSector(t domain.Ticker, sector string) {
	b.mu.Lock()
	b.sectors[t] = sector
	b.mu.Unlock()
}

// OrderRequest is a submission.
type OrderRequest struct {
	IdempotencyKey string
	Ticker         domain.Ticker
	Side           domain.OrderSide
	Type           domain.OrderType
	Quantity       float64
	LimitPrice     float64
	StopPrice      float64
	SignalID       string
	Sector         string
	Now            time.Time
}

// Submit places a simulated order.
//
// Idempotency is enforced here rather than by the caller: replaying a
// signal.generated event must never open a second position (requirement §36).
func (b *Broker) Submit(req OrderRequest) (domain.PaperOrder, error) {
	now := req.Now
	if now.IsZero() {
		now = b.cfg.Clock.Now()
	}
	if req.Quantity <= 0 {
		return domain.PaperOrder{}, fmt.Errorf("paper: quantity must be positive")
	}
	if req.Side == domain.OrderSell && !b.cfg.AllowShort {
		b.mu.RLock()
		pos, held := b.positions[req.Ticker]
		b.mu.RUnlock()
		if !held || pos.Quantity < req.Quantity {
			return domain.PaperOrder{}, fmt.Errorf("paper: short selling is disabled and the position is insufficient")
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if req.IdempotencyKey != "" {
		if id, seen := b.idempotency[req.IdempotencyKey]; seen {
			return *b.orders[id], nil
		}
	}
	if req.Sector != "" {
		b.sectors[req.Ticker] = req.Sector
	}

	o := &domain.PaperOrder{
		ID:             bus.NewEventID(now),
		PortfolioID:    b.portfolio.ID,
		IdempotencyKey: req.IdempotencyKey,
		Ticker:         req.Ticker,
		Side:           req.Side,
		Type:           req.Type,
		Quantity:       req.Quantity,
		LimitPrice:     req.LimitPrice,
		StopPrice:      req.StopPrice,
		TimeInForce:    "DAY",
		Status:         domain.OrderPending,
		SignalID:       req.SignalID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if o.Type == "" {
		o.Type = domain.OrderMarket
	}
	b.orders[o.ID] = o
	if req.IdempotencyKey != "" {
		b.idempotency[req.IdempotencyKey] = o.ID
	}
	b.pending = append(b.pending, o)
	return *o, nil
}

// Mark updates prices, attempts to fill pending orders and revalues the book.
//
// Fills happen on the *next* mark after submission, never on the price that was
// visible at decision time. That one rule is the difference between a simulated
// portfolio and a fantasy (ADR-007).
func (b *Broker) Mark(prices map[domain.Ticker]float64, volumes map[domain.Ticker]float64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if now.IsZero() {
		now = b.cfg.Clock.Now()
	}
	for t, p := range prices {
		if p > 0 {
			b.marks[t] = p
		}
	}

	remaining := b.pending[:0]
	for _, o := range b.pending {
		px, ok := b.marks[o.Ticker]
		if !ok || px <= 0 {
			remaining = append(remaining, o)
			continue
		}
		if b.cfg.FillDelay > 0 && now.Sub(o.CreatedAt) < b.cfg.FillDelay {
			remaining = append(remaining, o)
			continue
		}
		switch o.Type {
		case domain.OrderLimit:
			if (o.Side == domain.OrderBuy && px > o.LimitPrice) ||
				(o.Side == domain.OrderSell && px < o.LimitPrice) {
				remaining = append(remaining, o)
				continue
			}
		case domain.OrderStop:
			if (o.Side == domain.OrderBuy && px < o.StopPrice) ||
				(o.Side == domain.OrderSell && px > o.StopPrice) {
				remaining = append(remaining, o)
				continue
			}
		}
		if !b.fill(o, px, volumes[o.Ticker], now) {
			remaining = append(remaining, o)
		}
	}
	b.pending = remaining
	b.revalue(now)
}

// fill executes an order with costs and slippage, returning false when it could
// not be filled at all.
func (b *Broker) fill(o *domain.PaperOrder, px, barVolume float64, now time.Time) bool {
	qty := o.Quantity - o.FilledQty
	if qty <= 0 {
		o.Status = domain.OrderFilled
		return true
	}
	// Participation cap: an order larger than a slice of the bar's volume is
	// filled partially, which is what actually happens and what stops a
	// backtest from claiming a size it could never have traded.
	if barVolume > 0 && b.cfg.Costs.ParticipationCap > 0 {
		maxQty := barVolume * b.cfg.Costs.ParticipationCap
		if qty > maxQty {
			qty = maxQty
		}
	}
	if qty <= 0 {
		return false
	}

	slipBps := b.slippageBps(qty, barVolume, px)
	fillPx := px
	if o.Side == domain.OrderBuy {
		fillPx = px * (1 + slipBps/10000)
	} else {
		fillPx = px * (1 - slipBps/10000)
	}

	notional := fillPx * qty
	commission := notional * b.cfg.Costs.CommissionBps / 10000
	if b.cfg.Costs.CommissionPerShare > 0 {
		commission += b.cfg.Costs.CommissionPerShare * qty
	}
	if commission < b.cfg.Costs.MinCommission {
		commission = b.cfg.Costs.MinCommission
	}

	if o.Side == domain.OrderBuy {
		cost := notional + commission
		if cost > b.portfolio.Cash {
			// Scale down to what the account can actually afford rather than
			// silently going negative.
			affordable := (b.portfolio.Cash - commission) / fillPx
			if affordable <= 0 {
				o.Status = domain.OrderRejected
				o.RejectReason = "insufficient simulated cash"
				o.UpdatedAt = now
				return true
			}
			qty = affordable
			notional = fillPx * qty
			commission = notional * b.cfg.Costs.CommissionBps / 10000
		}
		b.portfolio.Cash -= notional + commission
	} else {
		b.portfolio.Cash += notional - commission
	}

	o.Fills = append(o.Fills, domain.Fill{
		Quantity: round4(qty), Price: round4(fillPx),
		Commission: round4(commission), SlippageBps: round2(slipBps), At: now,
	})
	prevQty := o.FilledQty
	o.FilledQty += qty
	o.AvgFillPrice = round4((o.AvgFillPrice*prevQty + fillPx*qty) / o.FilledQty)
	o.Commission = round4(o.Commission + commission)
	o.SlippageBps = round2(slipBps)
	o.UpdatedAt = now
	if o.FilledQty >= o.Quantity-1e-9 {
		o.Status = domain.OrderFilled
	} else {
		o.Status = domain.OrderPartial
	}

	b.applyToPosition(o, qty, fillPx, commission, now)
	return o.Status == domain.OrderFilled || o.Status == domain.OrderRejected
}

// slippageBps applies the configured model. The square-root impact model is the
// standard k·σ·sqrt(Q/ADV) form; the fixed model is there for comparison.
func (b *Broker) slippageBps(qty, barVolume, px float64) float64 {
	base := b.cfg.Costs.SlippageBps + b.cfg.Costs.SpreadCostBps/2
	if b.cfg.Costs.SlippageModel != "sqrt_impact" || barVolume <= 0 {
		return base
	}
	participation := qty / barVolume
	impact := b.cfg.Costs.ImpactCoef * 100 * math.Sqrt(math.Max(participation, 0))
	return base + impact
}

func (b *Broker) applyToPosition(o *domain.PaperOrder, qty, px, commission float64, now time.Time) {
	pos, ok := b.positions[o.Ticker]
	if !ok {
		pos = &domain.PaperPosition{
			PortfolioID: b.portfolio.ID, Ticker: o.Ticker,
			OpenedAt: now, SignalID: o.SignalID, Sector: b.sectors[o.Ticker],
		}
		b.positions[o.Ticker] = pos
	}
	signed := qty
	if o.Side == domain.OrderSell {
		signed = -qty
	}
	prevQty := pos.Quantity
	newQty := prevQty + signed

	switch {
	case prevQty == 0 || sameSign(prevQty, signed):
		// Opening or adding.
		pos.AvgPrice = round4((pos.AvgPrice*math.Abs(prevQty) + px*qty) / math.Abs(newQty))
		if prevQty == 0 {
			pos.OpenedAt = now
			b.openLots[o.Ticker] = lot{at: now, price: px, qty: qty, signalID: o.SignalID}
		}
	default:
		// Reducing or closing: realise P&L on the closed portion.
		closed := math.Min(math.Abs(signed), math.Abs(prevQty))
		direction := 1.0
		if prevQty < 0 {
			direction = -1
		}
		realized := (px - pos.AvgPrice) * closed * direction
		pos.RealizedPnL = round4(pos.RealizedPnL + realized - commission)
		b.portfolio.RealizedPnL = round4(b.portfolio.RealizedPnL + realized - commission)

		if l, ok := b.openLots[o.Ticker]; ok {
			side := domain.SideLong
			if prevQty < 0 {
				side = domain.SideShort
			}
			ret := 0.0
			if l.price > 0 {
				ret = (px - l.price) / l.price * direction
			}
			b.trades = append(b.trades, domain.BacktestTrade{
				Ticker: o.Ticker, Side: side, EntryAt: l.at, ExitAt: now,
				EntryPrice: round4(l.price), ExitPrice: round4(px), Quantity: round4(closed),
				PnL: round4(realized - commission), ReturnPct: round4(ret),
				Costs: round4(commission), HoldPeriod: now.Sub(l.at),
				Regime: l.regime, SignalID: l.signalID, ExitReason: "order",
			})
		}
		if math.Abs(newQty) < 1e-9 {
			newQty = 0
			delete(b.openLots, o.Ticker)
		}
	}
	pos.Quantity = round4(newQty)
	pos.UpdatedAt = now
	if pos.Quantity == 0 {
		pos.AvgPrice = 0
		pos.MarketValue = 0
		pos.UnrealizedPnL = 0
	}
}

func sameSign(a, b float64) bool { return (a >= 0 && b >= 0) || (a < 0 && b < 0) }

// revalue recomputes portfolio aggregates from marks.
func (b *Broker) revalue(now time.Time) {
	mv, long, short := 0.0, 0.0, 0.0
	unrealized := 0.0
	sectors := map[string]float64{}

	for t, pos := range b.positions {
		if pos.Quantity == 0 {
			continue
		}
		px := b.marks[t]
		if px <= 0 {
			px = pos.AvgPrice
		}
		pos.MarketPrice = round4(px)
		pos.MarketValue = round4(pos.Quantity * px)
		pos.CostBasis = round4(pos.Quantity * pos.AvgPrice)
		pos.UnrealizedPnL = round4(pos.MarketValue - pos.CostBasis)
		unrealized += pos.UnrealizedPnL
		mv += pos.MarketValue
		if pos.Quantity > 0 {
			long += pos.MarketValue
		} else {
			short += -pos.MarketValue
		}
		if s := b.sectors[t]; s != "" {
			sectors[s] += math.Abs(pos.MarketValue)
		}
	}

	p := &b.portfolio
	p.AsOf = now
	p.MarketValue = round4(mv)
	p.UnrealizedPnL = round4(unrealized)
	p.Equity = round4(p.Cash + mv)
	p.TotalPnL = round4(p.Equity - p.StartingCash)
	if p.StartingCash > 0 {
		p.ReturnPct = round4(p.TotalPnL / p.StartingCash)
	}
	p.LongExposure, p.ShortExposure = round4(long), round4(short)
	p.GrossExposure = round4(long + short)
	p.NetExposure = round4(long - short)
	if p.Equity > 0 {
		p.Leverage = round4(p.GrossExposure / p.Equity)
		p.SectorExposure = map[string]float64{}
		for s, v := range sectors {
			p.SectorExposure[s] = round4(v / p.Equity)
		}
	}
	if p.Equity > p.PeakEquity {
		p.PeakEquity = p.Equity
	}
	if p.PeakEquity > 0 {
		p.Drawdown = round4((p.PeakEquity - p.Equity) / p.PeakEquity)
		if p.Drawdown > p.MaxDrawdown {
			p.MaxDrawdown = p.Drawdown
		}
	}
	b.equityCurve = append(b.equityCurve, domain.EquityPoint{
		At: now, Equity: p.Equity, Cash: p.Cash, Drawdown: p.Drawdown, Exposure: p.GrossExposure,
	})
}

// Portfolio returns a snapshot including positions.
func (b *Broker) Portfolio() domain.Portfolio {
	b.mu.RLock()
	defer b.mu.RUnlock()
	p := b.portfolio
	p.SectorExposure = copyMap(b.portfolio.SectorExposure)
	p.Positions = b.positionsLocked()
	return p
}

func (b *Broker) positionsLocked() []domain.PaperPosition {
	out := make([]domain.PaperPosition, 0, len(b.positions))
	for _, p := range b.positions {
		if p.Quantity == 0 {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ticker < out[j].Ticker })
	return out
}

// Positions returns the open positions.
func (b *Broker) Positions() []domain.PaperPosition {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.positionsLocked()
}

// Orders returns every order, newest first.
func (b *Broker) Orders() []domain.PaperOrder {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]domain.PaperOrder, 0, len(b.orders))
	for _, o := range b.orders {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// EquityCurve returns the recorded equity samples.
func (b *Broker) EquityCurve() []domain.EquityPoint {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]domain.EquityPoint(nil), b.equityCurve...)
}

// Trades returns completed round trips.
func (b *Broker) Trades() []domain.BacktestTrade {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]domain.BacktestTrade(nil), b.trades...)
}

// SizeForWeight converts a target portfolio weight into a share quantity at the
// current mark, respecting the configured maximum weight.
func (b *Broker) SizeForWeight(t domain.Ticker, weight float64) float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	px := b.marks[t]
	if px <= 0 || b.portfolio.Equity <= 0 {
		return 0
	}
	w := math.Min(weight, b.cfg.MaxWeight)
	if w <= 0 {
		return 0
	}
	return math.Floor(b.portfolio.Equity * w / px)
}

// CanOpen reports whether a new position is permitted under the position count
// limit. It is a broker-level guard; the risk engine's caps are separate and
// stricter.
func (b *Broker) CanOpen(t domain.Ticker) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, held := b.positions[t]; held && b.positions[t].Quantity != 0 {
		return true
	}
	open := 0
	for _, p := range b.positions {
		if p.Quantity != 0 {
			open++
		}
	}
	return open < b.cfg.MaxPositions
}

func copyMap(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// Package execution routes QuantOS suggestions to a broker, one human approval
// at a time.
//
// The flow is: a suggestion becomes a Ticket (PROPOSED); the owner approves or
// rejects it; an approval is re-checked against every guardrail at the moment
// it is given — expiry, price drift, the kill switch, the model-health gate,
// per-order and per-position limits, the daily loss limit — and only then is a
// limit order sent to the Venue. Nothing in this package submits an order
// without an Approve call, and nothing approves on the owner's behalf.
//
// Governance rule G-8 still holds: the only Venue shipped is Alpaca's *paper*
// API, and NewAlpacaPaper refuses Alpaca's live host. Pointing this at real
// money is a deliberate change to G-8 and to that constructor, not a config
// flag.
package execution

import (
	"context"
	"errors"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Account is the venue's view of the account.
type Account struct {
	Equity      float64 `json:"equity"`
	LastEquity  float64 `json:"last_equity"` // equity at the previous close
	Cash        float64 `json:"cash"`
	BuyingPower float64 `json:"buying_power"`
	Currency    string  `json:"currency"`
	// Blocked is true when the venue itself will not accept orders.
	Blocked bool   `json:"blocked"`
	Status  string `json:"status"`
	Paper   bool   `json:"paper"`
}

// DayPnLPct is today's change in equity as a fraction of the previous close.
func (a Account) DayPnLPct() float64 {
	if a.LastEquity <= 0 {
		return 0
	}
	return (a.Equity - a.LastEquity) / a.LastEquity
}

// Position is one holding at the venue.
type Position struct {
	Ticker        domain.Ticker `json:"ticker"`
	Quantity      float64       `json:"quantity"` // negative for short
	AvgPrice      float64       `json:"avg_price"`
	MarketPrice   float64       `json:"market_price"`
	MarketValue   float64       `json:"market_value"`
	UnrealizedPnL float64       `json:"unrealized_pnl"`
}

// OrderRequest is what the desk sends to a venue. Every order is a limit
// order: a market order on an approval that may be minutes old would fill at
// whatever the price has become, which is the opposite of what was approved.
type OrderRequest struct {
	// ClientOrderID makes submission idempotent: the desk uses the ticket ID,
	// so a retried approval can never place a second order.
	ClientOrderID string
	Ticker        domain.Ticker
	Side          domain.OrderSide
	Quantity      float64
	LimitPrice    float64
	TimeInForce   string // "day" unless set
}

// Order is the venue's record of an order.
type Order struct {
	ID            string             `json:"id"`
	ClientOrderID string             `json:"client_order_id"`
	Ticker        domain.Ticker      `json:"ticker"`
	Side          domain.OrderSide   `json:"side"`
	Quantity      float64            `json:"quantity"`
	LimitPrice    float64            `json:"limit_price"`
	Status        domain.OrderStatus `json:"status"`
	// VenueStatus is the venue's own status string, kept verbatim for audit.
	VenueStatus  string    `json:"venue_status"`
	FilledQty    float64   `json:"filled_qty"`
	AvgFillPrice float64   `json:"avg_fill_price"`
	SubmittedAt  time.Time `json:"submitted_at"`
}

// Terminal reports whether the order can no longer change.
func (o Order) Terminal() bool {
	switch o.Status {
	case domain.OrderFilled, domain.OrderCancelled, domain.OrderRejected:
		return true
	}
	return false
}

// Venue is a broker the desk can route approved orders to.
type Venue interface {
	Name() string
	// Paper reports whether orders at this venue move real money. The desk
	// refuses to run against a venue that answers false (rule G-8).
	Paper() bool
	Account(ctx context.Context) (Account, error)
	Positions(ctx context.Context) ([]Position, error)
	Submit(ctx context.Context, req OrderRequest) (Order, error)
	// OrderByClientID looks an order up by the ID the desk assigned. It returns
	// ErrOrderNotFound when the venue has no such order.
	OrderByClientID(ctx context.Context, clientOrderID string) (Order, error)
	// CancelAll cancels every open order. It backs the kill switch.
	CancelAll(ctx context.Context) error
}

// ErrOrderNotFound is returned by OrderByClientID for an unknown order.
var ErrOrderNotFound = errors.New("execution: order not found at venue")

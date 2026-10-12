package execution

import (
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// TicketStatus is the lifecycle of an order ticket.
//
//	PROPOSED ──approve──▶ APPROVED ──guardrails ok──▶ SUBMITTED ──▶ FILLED
//	    │                     │                           │      ╲─▶ PARTIALLY_FILLED
//	    │                     ├──guardrail fails──▶ BLOCKED│      ╲─▶ CANCELLED
//	    │                     └──venue error──────▶ FAILED ╲────────▶ VENUE_REJECTED
//	    ├──reject──▶ REJECTED
//	    ├──ttl─────▶ EXPIRED
//	    └──halt────▶ CANCELLED
type TicketStatus string

// Proposal sources.
const (
	SourceSignal = "signal"
	SourceManual = "manual"
)

const (
	TicketProposed      TicketStatus = "PROPOSED"
	TicketApproved      TicketStatus = "APPROVED" // transient: checks running
	TicketSubmitted     TicketStatus = "SUBMITTED"
	TicketPartial       TicketStatus = "PARTIALLY_FILLED"
	TicketFilled        TicketStatus = "FILLED"
	TicketRejected      TicketStatus = "REJECTED" // by the owner
	TicketBlocked       TicketStatus = "BLOCKED"  // by a guardrail at approval time
	TicketExpired       TicketStatus = "EXPIRED"
	TicketCancelled     TicketStatus = "CANCELLED"
	TicketVenueRejected TicketStatus = "VENUE_REJECTED"
	TicketFailed        TicketStatus = "FAILED"
)

// Open reports whether the ticket still has an order working at the venue.
func (s TicketStatus) Open() bool { return s == TicketSubmitted || s == TicketPartial }

// Proposal is a suggestion QuantOS wants the owner to decide on.
type Proposal struct {
	Ticker   domain.Ticker    `json:"ticker"`
	Side     domain.OrderSide `json:"side"`
	Quantity float64          `json:"quantity"`
	// LimitPrice is the worst price the owner is agreeing to.
	LimitPrice float64 `json:"limit_price"`
	// ReferencePrice is the market price the suggestion was made at. Approval
	// is refused if the market has moved too far from it since.
	ReferencePrice float64 `json:"reference_price"`

	// Source is "signal" for a proposal generated from a QuantOS signal and
	// "manual" for one the owner typed in. Only signal proposals are subject
	// to the model-health gate: a trade the owner thought of themselves is not
	// the model's claim.
	Source string `json:"source"`

	// Rationale is shown to the owner verbatim. A suggestion that cannot say
	// why it exists is not put in front of anyone.
	Rationale  string  `json:"rationale"`
	SignalID   string  `json:"signal_id,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// TicketEvent is one entry in a ticket's audit trail.
type TicketEvent struct {
	At     time.Time    `json:"at"`
	Status TicketStatus `json:"status"`
	By     string       `json:"by"`
	Reason string       `json:"reason,omitempty"`
}

// Ticket is a proposal plus everything that happened to it.
type Ticket struct {
	ID string `json:"id"`
	Proposal

	Status       TicketStatus `json:"status"`
	StatusReason string       `json:"status_reason,omitempty"`
	Venue        string       `json:"venue"`
	Paper        bool         `json:"paper"`

	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	DecidedBy string    `json:"decided_by,omitempty"`
	DecidedAt time.Time `json:"decided_at,omitempty"`

	// Checks records what each guardrail saw at approval time, so a fill can
	// always be traced back to the numbers that allowed it.
	Checks []Check `json:"checks,omitempty"`
	Order  *Order  `json:"order,omitempty"`

	History []TicketEvent `json:"history"`
}

// Notional is the most the order can cost (or raise).
func (t Ticket) Notional() float64 { return t.Quantity * t.LimitPrice }

// Check is the outcome of one guardrail.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

func (t *Ticket) transition(at time.Time, s TicketStatus, by, reason string) {
	t.Status = s
	t.StatusReason = reason
	t.History = append(t.History, TicketEvent{At: at, Status: s, By: by, Reason: reason})
}

func (t Ticket) clone() Ticket {
	c := t
	c.History = append([]TicketEvent(nil), t.History...)
	c.Checks = append([]Check(nil), t.Checks...)
	if t.Order != nil {
		o := *t.Order
		c.Order = &o
	}
	return c
}

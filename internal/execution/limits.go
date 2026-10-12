package execution

import (
	"errors"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Limits are the guardrails every approval is checked against. They live in
// the service, not the UI, so no client can skip them.
type Limits struct {
	// Allowlist is the set of tickers the desk will trade. It is required:
	// an empty allowlist trades nothing (fail closed).
	Allowlist []domain.Ticker
	// MaxOrderNotional caps quantity × limit price for a single order.
	MaxOrderNotional float64
	// MaxPositionWeight caps a position, after a buy, as a fraction of equity.
	MaxPositionWeight float64
	// MaxDailyLossPct blocks new buys once the account is down this fraction
	// on the day. Sells are still allowed: they reduce risk.
	MaxDailyLossPct float64
	// MaxDriftBps refuses an approval when the market has moved this far from
	// the price the suggestion was made at.
	MaxDriftBps float64
	// TicketTTL is how long a proposal can wait for a decision.
	TicketTTL time.Duration
	// AllowShort permits sells larger than the position held. Off by default.
	AllowShort bool
}

// DefaultLimits are deliberately small. They are a starting point for a
// paper account, not a recommendation for any real one.
func DefaultLimits() Limits {
	return Limits{
		MaxOrderNotional:  1_000,
		MaxPositionWeight: 0.10,
		MaxDailyLossPct:   0.02,
		MaxDriftBps:       50,
		TicketTTL:         15 * time.Minute,
	}
}

// Validate rejects a limit set that would make a guardrail meaningless.
func (l Limits) Validate() error {
	switch {
	case len(l.Allowlist) == 0:
		return errors.New("execution: limits need a non-empty ticker allowlist")
	case l.MaxOrderNotional <= 0:
		return errors.New("execution: MaxOrderNotional must be positive")
	case l.MaxPositionWeight <= 0 || l.MaxPositionWeight > 1:
		return errors.New("execution: MaxPositionWeight must be in (0, 1]")
	case l.MaxDailyLossPct <= 0 || l.MaxDailyLossPct >= 1:
		return errors.New("execution: MaxDailyLossPct must be in (0, 1)")
	case l.MaxDriftBps <= 0:
		return errors.New("execution: MaxDriftBps must be positive")
	case l.TicketTTL <= 0:
		return errors.New("execution: TicketTTL must be positive")
	}
	return nil
}

func (l Limits) allows(t domain.Ticker) bool {
	for _, a := range l.Allowlist {
		if domain.NormalizeTicker(string(a)) == t {
			return true
		}
	}
	return false
}

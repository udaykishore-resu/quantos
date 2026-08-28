package signal

import (
	"fmt"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// InvalidationCheck is the outcome of evaluating one condition.
type InvalidationCheck struct {
	SignalID  string                  `json:"signal_id"`
	Kind      domain.InvalidationKind `json:"kind"`
	Triggered bool                    `json:"triggered"`
	Note      string                  `json:"note"`
	Value     float64                 `json:"value"`
	Threshold float64                 `json:"threshold"`
}

// EvalInput is the current state a signal is re-evaluated against.
type EvalInput struct {
	Snapshot   domain.FeatureSnapshot
	Regime     domain.MarketRegime
	Prediction *domain.Prediction
	Risk       *domain.RiskAssessment
	// MaterialNews is set when a high-materiality event contradicting the
	// signal's direction has arrived.
	MaterialNews *domain.NewsEvent
	Now          time.Time
}

// Evaluate checks every invalidation condition on a signal and returns the
// first that triggered, if any.
//
// Returning the first rather than all of them is deliberate: the *reason* a
// signal died is a single fact for the audit trail, and evaluating in
// declaration order makes that reason deterministic and reproducible.
func Evaluate(s domain.Signal, in EvalInput) (InvalidationCheck, bool) {
	now := in.Now
	if now.IsZero() {
		now = in.Snapshot.AsOf
	}
	price := in.Snapshot.MustGet(domain.FeatLast, in.Snapshot.Last)

	for _, c := range s.Invalidations {
		chk := InvalidationCheck{SignalID: s.ID, Kind: c.Kind, Threshold: c.Threshold}
		switch c.Kind {
		case domain.InvalidPriceBelow:
			chk.Value = price
			if price > 0 && c.Threshold > 0 && price < c.Threshold {
				chk.Triggered = true
				chk.Note = fmt.Sprintf("price %.4f fell below the invalidation level %.4f", price, c.Threshold)
			}
		case domain.InvalidPriceAbove:
			chk.Value = price
			if price > 0 && c.Threshold > 0 && price > c.Threshold {
				chk.Triggered = true
				chk.Note = fmt.Sprintf("price %.4f rose above the invalidation level %.4f", price, c.Threshold)
			}
		case domain.InvalidVWAPCross:
			vd, ok := in.Snapshot.Get(domain.FeatVWAPDist)
			if !ok {
				continue
			}
			chk.Value = vd
			// Direction-aware: a long thesis dies on a loss of VWAP, a short
			// thesis on a reclaim of it.
			if (s.Side == domain.SideLong && vd < 0) || (s.Side == domain.SideShort && vd > 0) {
				chk.Triggered = true
				chk.Note = fmt.Sprintf("price crossed to the wrong side of session VWAP (%.0f bps)", vd)
			}
		case domain.InvalidRegimeChange:
			if c.RegimeFrom != "" && in.Regime.Regime != "" && in.Regime.Regime != c.RegimeFrom {
				chk.Triggered = true
				chk.Note = fmt.Sprintf("market regime changed from %s to %s", c.RegimeFrom, in.Regime.Regime)
			}
		case domain.InvalidVolatilityAbove:
			v, ok := in.Snapshot.Get(domain.FeatRealizedVol)
			if !ok {
				continue
			}
			chk.Value = v
			if c.Threshold > 0 && v > c.Threshold {
				chk.Triggered = true
				chk.Note = fmt.Sprintf("realised volatility %.2f exceeded the invalidation threshold %.2f", v, c.Threshold)
			}
		case domain.InvalidConfidenceBelow:
			if in.Prediction == nil {
				continue
			}
			conf := scenarioFor(*in.Prediction, s.Horizon).Confidence
			chk.Value = conf
			if c.Threshold > 0 && conf < c.Threshold {
				chk.Triggered = true
				chk.Note = fmt.Sprintf("model confidence fell to %.2f, below the invalidation threshold %.2f", conf, c.Threshold)
			}
		case domain.InvalidRiskVeto:
			if in.Risk != nil && in.Risk.Decision == domain.RiskBlock {
				chk.Triggered = true
				chk.Note = "risk engine now blocks this instrument: " + firstOr(in.Risk.Blockers, "no reason recorded")
			}
		case domain.InvalidTimeExpiry:
			deadline := c.Deadline
			if deadline.IsZero() {
				deadline = s.ExpiresAt
			}
			if !deadline.IsZero() && !now.Before(deadline) {
				chk.Triggered = true
				chk.Note = fmt.Sprintf("signal horizon elapsed at %s", deadline.UTC().Format(time.RFC3339))
			}
		case domain.InvalidNewsMaterial:
			if in.MaterialNews == nil {
				continue
			}
			n := *in.MaterialNews
			adverse := (s.Side == domain.SideLong && n.SentimentScore < -0.3) ||
				(s.Side == domain.SideShort && n.SentimentScore > 0.3)
			if n.Material() && adverse {
				chk.Triggered = true
				chk.Value = n.SentimentScore
				chk.Note = fmt.Sprintf("material %s news arrived with %s sentiment: %s",
					n.Category, n.Sentiment, n.Headline)
			}
		}
		if chk.Triggered {
			return chk, true
		}
	}

	// A risk BLOCK invalidates regardless of whether the strategy declared the
	// condition. The veto outranks the strategy's own list (ADR-009).
	if in.Risk != nil && in.Risk.Decision == domain.RiskBlock {
		return InvalidationCheck{
			SignalID: s.ID, Kind: domain.InvalidRiskVeto, Triggered: true,
			Note: "risk engine blocked the instrument: " + firstOr(in.Risk.Blockers, "no reason recorded"),
		}, true
	}
	// Hard expiry backstop.
	if !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt) {
		return InvalidationCheck{
			SignalID: s.ID, Kind: domain.InvalidTimeExpiry, Triggered: true,
			Note: "signal expiry reached",
		}, true
	}
	return InvalidationCheck{SignalID: s.ID}, false
}

// Sweep evaluates every active signal and invalidates those whose conditions
// have triggered, returning the invalidated signals.
func (e *Engine) Sweep(lookup func(domain.Ticker) EvalInput, now time.Time) []domain.Signal {
	var out []domain.Signal
	for _, s := range e.Active() {
		in := lookup(s.Ticker)
		if in.Now.IsZero() {
			in.Now = now
		}
		chk, triggered := Evaluate(s, in)
		if !triggered {
			continue
		}
		if inv, ok := e.Invalidate(s.ID, chk.Kind, chk.Note, in.Now); ok {
			out = append(out, inv)
		}
	}
	return out
}

func firstOr(ss []string, def string) string {
	if len(ss) == 0 {
		return def
	}
	return ss[0]
}

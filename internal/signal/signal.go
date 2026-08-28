// Package signal turns an approved opportunity into a paper-trading signal and
// then keeps watching it.
//
// Two guarantees are enforced here rather than documented:
//
//   - A signal cannot exist without an allowing risk assessment. The only way
//     to build one is domain.NewSignal, which takes the assessment and refuses
//     anything other than ALLOW_PAPER_SIGNAL (ADR-009).
//   - A signal cannot exist without invalidation conditions, and the engine
//     re-evaluates them on every feature update, regime change and risk
//     re-assessment (governance rule G-6).
package signal

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/rules"
)

// Config parameterises signal generation.
type Config struct {
	MaxActive    int
	MaxPerTicker int
	Cooldown     time.Duration
	DefaultTTL   time.Duration
	Clock        obs.Clock
	Metrics      *obs.Metrics
}

// DefaultConfig returns sensible parameters.
func DefaultConfig() Config {
	return Config{
		MaxActive: 50, MaxPerTicker: 1, Cooldown: 30 * time.Minute,
		DefaultTTL: 4 * time.Hour, Clock: obs.SystemClock{},
	}
}

// Input is everything needed to consider emitting a signal.
type Input struct {
	Stock       domain.Stock
	Snapshot    domain.FeatureSnapshot
	Rules       rules.Result
	Score       domain.StockScore
	Prediction  domain.Prediction
	Opportunity domain.OpportunityScore
	Risk        domain.RiskAssessment
	Regime      domain.MarketRegime
	Strategy    domain.Strategy
	// CorrelationID ties the signal to the causal chain that produced it.
	CorrelationID string
	Now           time.Time
}

// Rejection explains why no signal was emitted. Rejections are recorded, not
// discarded: the rate and reason mix is how the platform's selectivity is
// measured and tuned.
type Rejection struct {
	Ticker domain.Ticker `json:"ticker"`
	Reason string        `json:"reason"`
	Stage  string        `json:"stage"`
	At     time.Time     `json:"at"`
}

// Engine generates and tracks signals.
type Engine struct {
	cfg Config

	mu       sync.RWMutex
	active   map[string]domain.Signal   // id -> signal
	byTicker map[domain.Ticker][]string // ticker -> signal ids
	lastAt   map[string]time.Time       // ticker|strategy -> last emission
}

// NewEngine builds a signal engine.
func NewEngine(cfg Config) *Engine {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = 50
	}
	if cfg.MaxPerTicker <= 0 {
		cfg.MaxPerTicker = 1
	}
	return &Engine{
		cfg:      cfg,
		active:   map[string]domain.Signal{},
		byTicker: map[domain.Ticker][]string{},
		lastAt:   map[string]time.Time{},
	}
}

// Generate evaluates the policy gates and, if they pass, constructs a signal.
//
// The order of the gates is deliberate: cheap deterministic policy checks come
// first, and the risk veto is enforced by construction at the end, so no path
// exists that produces a signal without it.
func (e *Engine) Generate(in Input) (domain.Signal, *Rejection, error) {
	now := in.Now
	if now.IsZero() {
		now = e.cfg.Clock.Now()
	}
	reject := func(stage, format string, a ...any) (domain.Signal, *Rejection, error) {
		return domain.Signal{}, &Rejection{
			Ticker: in.Stock.Ticker, Stage: stage,
			Reason: fmt.Sprintf(format, a...), At: now,
		}, nil
	}
	pol := in.Strategy.Policy

	// Governance rule G-5: stale data suspends new signals, unconditionally.
	if in.Snapshot.Stale {
		return reject("freshness", "market data is stale: %s", in.Snapshot.StaleReason)
	}
	side := in.Opportunity.Bias
	if side == domain.SideFlat {
		return reject("direction", "no directional bias: edge is inside the neutral band")
	}
	if side == domain.SideShort && !pol.AllowShort {
		return reject("direction", "strategy %s does not permit short signals", in.Strategy.ID)
	}
	if in.Rules.Side != side {
		return reject("agreement", "rule engine bias (%s) disagrees with the prediction bias (%s)", in.Rules.Side, side)
	}
	if absScore(in.Rules.Score) < pol.MinRuleScore {
		return reject("rule_score", "rule score %.1f is below the strategy minimum of %.1f", in.Rules.Score, pol.MinRuleScore)
	}
	if in.Score.FinalScore < pol.MinFinalScore {
		return reject("composite_score", "composite score %.1f is below the strategy minimum of %.1f", in.Score.FinalScore, pol.MinFinalScore)
	}
	scenario := scenarioFor(in.Prediction, pol.Horizon)
	if scenario.Confidence < pol.MinConfidence {
		return reject("confidence", "model confidence %.2f is below the strategy minimum of %.2f", scenario.Confidence, pol.MinConfidence)
	}
	if absScore(scenario.Dist.DirectionalEdge()*100)/100 < pol.MinDirectionalEdge {
		return reject("edge", "directional edge %.3f is below the strategy minimum of %.3f",
			scenario.Dist.DirectionalEdge(), pol.MinDirectionalEdge)
	}
	if in.Opportunity.RiskReward < pol.MinRiskReward {
		return reject("risk_reward", "risk/reward of %.2f is below the strategy minimum of %.2f",
			in.Opportunity.RiskReward, pol.MinRiskReward)
	}

	key := string(in.Stock.Ticker) + "|" + in.Strategy.ID
	e.mu.RLock()
	last, hadLast := e.lastAt[key]
	activeCount := len(e.active)
	tickerCount := len(e.byTicker[in.Stock.Ticker])
	e.mu.RUnlock()

	cooldown := e.cfg.Cooldown
	if pol.CooldownMinutes > 0 {
		cooldown = time.Duration(pol.CooldownMinutes) * time.Minute
	}
	if hadLast && now.Sub(last) < cooldown {
		return reject("cooldown", "within the %s cooldown for %s (last signal %s ago)",
			cooldown, in.Strategy.ID, now.Sub(last).Truncate(time.Second))
	}
	if activeCount >= e.cfg.MaxActive {
		return reject("capacity", "at the platform limit of %d active signals", e.cfg.MaxActive)
	}
	if tickerCount >= e.cfg.MaxPerTicker {
		return reject("capacity", "already holding %d active signal(s) for %s", tickerCount, in.Stock.Ticker)
	}

	price := in.Snapshot.MustGet(domain.FeatLast, in.Snapshot.Last)
	atr := in.Snapshot.MustGet(domain.FeatATR14, 0)
	if price <= 0 {
		return reject("reference", "no usable reference price on the snapshot")
	}

	stop, target := price, price
	switch side {
	case domain.SideLong:
		stop = price - atr*orDefault(pol.StopATRMultiple, 1.5)
		target = price + atr*orDefault(pol.TargetATRMultiple, 2.5)
	case domain.SideShort:
		stop = price + atr*orDefault(pol.StopATRMultiple, 1.5)
		target = price - atr*orDefault(pol.TargetATRMultiple, 2.5)
	}

	ttl := e.cfg.DefaultTTL
	if pol.Horizon != "" {
		ttl = pol.Horizon.Duration()
	}
	weight := domain.Clamp(in.Opportunity.Score/100*pol.MaxWeight, 0, pol.MaxWeight)

	draft := domain.SignalDraft{
		ID:              bus.NewEventID(now),
		Ticker:          in.Stock.Ticker,
		CreatedAt:       now,
		ExpiresAt:       now.Add(ttl),
		Side:            side,
		Strength:        in.Opportunity.Score,
		Confidence:      scenario.Confidence,
		Horizon:         pol.Horizon,
		EntryReference:  round4(price),
		StopReference:   round4(stop),
		TargetReference: round4(target),
		SuggestedWeight: round4(weight),
		Invalidations:   BuildInvalidations(in.Strategy, price, atr, scenario.Confidence, in.Regime.Regime, now),
		StrategyID:      in.Strategy.ID,
		RulesFired:      ruleIDs(in.Rules),
		Evidence:        in.Rules.Evidence,
		CounterEvidence: in.Rules.CounterEvidence,
		PredictionID:    in.Prediction.ID,
		ScoreRef:        in.Score.ConfigHash,
		FeatureHash:     in.Snapshot.Hash,
		Regime:          in.Regime.Regime,
		ModelVersion:    in.Prediction.ModelVersion,
		ConfigHash:      in.Strategy.Hash,
		CorrelationID:   in.CorrelationID,
	}

	// The risk veto is enforced here, by construction. There is no branch that
	// skips it.
	sig, err := domain.NewSignal(draft, in.Risk)
	if err != nil {
		return domain.Signal{}, &Rejection{
			Ticker: in.Stock.Ticker, Stage: "risk", Reason: err.Error(), At: now,
		}, nil
	}

	e.mu.Lock()
	e.active[sig.ID] = sig
	e.byTicker[sig.Ticker] = append(e.byTicker[sig.Ticker], sig.ID)
	e.lastAt[key] = now
	e.mu.Unlock()

	if e.cfg.Metrics != nil {
		e.cfg.Metrics.SignalsGenerated.Inc(in.Strategy.ID, string(side))
	}
	return sig, nil, nil
}

// BuildInvalidations instantiates a strategy's invalidation templates against
// live values. Every signal gets at least a time expiry, because a signal that
// can never expire is a position nobody is watching.
func BuildInvalidations(s domain.Strategy, price, atr, confidence float64, rg domain.Regime, now time.Time) []domain.InvalidationCondition {
	out := make([]domain.InvalidationCondition, 0, len(s.Invalidation)+1)
	for _, t := range s.Invalidation {
		c := domain.InvalidationCondition{Kind: t.Kind, Description: t.Description}
		switch t.Kind {
		case domain.InvalidPriceBelow:
			c.Threshold = round4(price - atr*orDefault(t.Multiple, 1.5))
		case domain.InvalidPriceAbove:
			c.Threshold = round4(price + atr*orDefault(t.Multiple, 1.5))
		case domain.InvalidVolatilityAbove:
			c.Threshold = orDefault(t.Value, 0.9)
		case domain.InvalidConfidenceBelow:
			c.Threshold = orDefault(t.Value, confidence*0.75)
		case domain.InvalidRegimeChange:
			c.RegimeFrom = rg
		case domain.InvalidTimeExpiry:
			mins := t.Minutes
			if mins <= 0 {
				mins = 240
			}
			c.Deadline = now.Add(time.Duration(mins) * time.Minute)
		case domain.InvalidVWAPCross, domain.InvalidNewsMaterial, domain.InvalidRiskVeto:
			// No numeric threshold; evaluated structurally.
		}
		out = append(out, c)
	}
	hasExpiry := false
	for _, c := range out {
		if c.Kind == domain.InvalidTimeExpiry {
			hasExpiry = true
		}
	}
	if !hasExpiry {
		out = append(out, domain.InvalidationCondition{
			Kind: domain.InvalidTimeExpiry, Deadline: now.Add(4 * time.Hour),
			Description: "Default four-hour expiry: no signal is allowed to live indefinitely",
		})
	}
	return out
}

// Active returns the live signals, newest first.
func (e *Engine) Active() []domain.Signal {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]domain.Signal, 0, len(e.active))
	for _, s := range e.active {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// ActiveFor returns live signals for one symbol.
func (e *Engine) ActiveFor(t domain.Ticker) []domain.Signal {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := []domain.Signal{}
	for _, id := range e.byTicker[t] {
		if s, ok := e.active[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Get returns a signal by id.
func (e *Engine) Get(id string) (domain.Signal, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.active[id]
	return s, ok
}

// Attach records an externally-restored signal, used when rebuilding state from
// the store after a restart.
func (e *Engine) Attach(s domain.Signal) {
	if s.Status != domain.SignalActive {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active[s.ID] = s
	e.byTicker[s.Ticker] = append(e.byTicker[s.Ticker], s.ID)
}

// Invalidate marks a signal invalid and removes it from the active set.
func (e *Engine) Invalidate(id string, kind domain.InvalidationKind, note string, at time.Time) (domain.Signal, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.active[id]
	if !ok {
		return domain.Signal{}, false
	}
	s.Status = domain.SignalInvalidated
	s.InvalidatedAt = &at
	s.InvalidationKind = kind
	s.InvalidationNote = note
	delete(e.active, id)
	ids := e.byTicker[s.Ticker][:0]
	for _, x := range e.byTicker[s.Ticker] {
		if x != id {
			ids = append(ids, x)
		}
	}
	e.byTicker[s.Ticker] = ids
	if e.cfg.Metrics != nil {
		e.cfg.Metrics.SignalsInvalidated.Inc(string(kind))
	}
	return s, true
}

func ruleIDs(r rules.Result) []string {
	out := make([]string, 0, len(r.Fired))
	for _, h := range r.Fired {
		out = append(out, h.RuleID)
	}
	return out
}

func scenarioFor(p domain.Prediction, h domain.Horizon) domain.ScenarioPrediction {
	if s, ok := p.Scenario(h); ok {
		return s
	}
	return p.Primary()
}

func absScore(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func orDefault(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

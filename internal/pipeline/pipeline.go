// Package pipeline is the composition root for the decision path.
//
// It wires the engines together in the fixed order from ADR-005 and exposes a
// single entry point, OnBar. The services drive it from Kafka; the backtester
// drives it from historical bars; the demo runner drives it from the simulator.
// All three therefore execute *the same* code, which is what makes a backtest a
// measurement of this system rather than of a parallel implementation.
package pipeline

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/alerts"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/features"
	"github.com/udaykishoreresu/quantos/internal/fundamentals"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/opportunity"
	"github.com/udaykishoreresu/quantos/internal/paper"
	"github.com/udaykishoreresu/quantos/internal/predict"
	"github.com/udaykishoreresu/quantos/internal/regime"
	"github.com/udaykishoreresu/quantos/internal/relationship"
	"github.com/udaykishoreresu/quantos/internal/risk"
	"github.com/udaykishoreresu/quantos/internal/rules"
	"github.com/udaykishoreresu/quantos/internal/scoring"
	"github.com/udaykishoreresu/quantos/internal/signal"
	"github.com/udaykishoreresu/quantos/internal/universe"
	"github.com/udaykishoreresu/quantos/internal/valuation"
)

// Deps are the engines the pipeline orchestrates. Constructing them is the
// caller's job, which keeps the pipeline free of configuration parsing and
// makes every engine independently substitutable in tests.
type Deps struct {
	Universe     *universe.Universe
	Features     *features.Engine
	Regime       *regime.Engine
	Relationship *relationship.Engine
	Rules        *rules.Engine
	Strategies   *rules.Registry
	Predict      *predict.Engine
	Risk         *risk.Engine
	Signals      *signal.Engine
	Alerts       *alerts.Engine
	Broker       *paper.Broker
	Fundamentals fundamentals.Store
	Clock        obs.Clock
	Metrics      *obs.Metrics
}

// Options tune pipeline behaviour that is not owned by a single engine.
type Options struct {
	// StrategyID selects the strategy used for scoring and signal policy.
	StrategyID string
	// MacroTickers are the symbols fed to the regime and relationship engines
	// rather than evaluated as candidates.
	MacroTickers []domain.Ticker
	// TradePaper submits paper orders for generated signals.
	TradePaper bool
	// FundamentalRefresh bounds how often fundamentals are recomputed per
	// symbol; they change quarterly, so recomputing per bar is waste.
	FundamentalRefresh time.Duration
}

// Result is everything one bar produced. Nil pointers mean "this stage did not
// produce anything", which is a normal outcome, not an error.
type Result struct {
	Ticker      domain.Ticker
	StrategyID  string
	Snapshot    domain.FeatureSnapshot
	Regime      domain.MarketRegime
	Rules       rules.Result
	Prediction  *domain.Prediction
	Score       *domain.StockScore
	Risk        *domain.RiskAssessment
	Opportunity *domain.OpportunityScore
	Signal      *domain.Signal
	Rejection   *signal.Rejection
	Alerts      []domain.Alert
	Invalidated []domain.Signal
	// Stock, Valuation and Events are carried out of the pipeline so that a
	// caller can build an explanation from a finished result without asking
	// the pipeline to produce prose. The pipeline has no way to reach a
	// language model; see ADR-005 and tests/integration/architecture_test.go.
	Stock     domain.Stock
	Valuation *domain.ValuationMetrics
	Events    []domain.CorporateEvent
	// Macro is true when the bar was market context rather than a candidate.
	Macro bool
}

// Pipeline runs the decision path.
type Pipeline struct {
	deps Deps
	opts Options

	mu        sync.RWMutex
	snapshots map[domain.Ticker]domain.FeatureSnapshot
	prevSnaps map[domain.Ticker]domain.FeatureSnapshot
	prevPreds map[domain.Ticker]domain.Prediction
	fundCache map[domain.Ticker]fundCacheEntry
	events    map[domain.Ticker][]domain.CorporateEvent
	news      map[domain.Ticker]domain.NewsEvent
	lastPrice map[domain.Ticker]float64
	lastVol   map[domain.Ticker]float64
	regime    domain.MarketRegime
	macro     map[domain.Ticker]bool
	health    *domain.ModelHealth
	drift     *domain.DriftReport
	barsSeen  int64
}

type fundCacheEntry struct {
	at        time.Time
	metrics   domain.FundamentalMetrics
	scores    fundamentals.Scores
	valuation domain.ValuationMetrics
}

// New builds a pipeline.
func New(deps Deps, opts Options) *Pipeline {
	if deps.Clock == nil {
		deps.Clock = obs.SystemClock{}
	}
	if opts.FundamentalRefresh <= 0 {
		opts.FundamentalRefresh = 6 * time.Hour
	}
	p := &Pipeline{
		deps:      deps,
		opts:      opts,
		snapshots: map[domain.Ticker]domain.FeatureSnapshot{},
		prevSnaps: map[domain.Ticker]domain.FeatureSnapshot{},
		prevPreds: map[domain.Ticker]domain.Prediction{},
		fundCache: map[domain.Ticker]fundCacheEntry{},
		events:    map[domain.Ticker][]domain.CorporateEvent{},
		news:      map[domain.Ticker]domain.NewsEvent{},
		lastPrice: map[domain.Ticker]float64{},
		lastVol:   map[domain.Ticker]float64{},
		macro:     map[domain.Ticker]bool{},
	}
	for _, t := range opts.MacroTickers {
		p.macro[t] = true
	}
	return p
}

// SetEvents records the corporate-event calendar for a symbol.
func (p *Pipeline) SetEvents(t domain.Ticker, events []domain.CorporateEvent) {
	p.mu.Lock()
	p.events[t] = events
	p.mu.Unlock()
}

// SetNews records the most recent material news event for a symbol.
func (p *Pipeline) SetNews(n domain.NewsEvent) {
	p.mu.Lock()
	p.news[n.Ticker] = n
	p.mu.Unlock()
}

// SetModelHealth records the current model health, which the risk engine reads.
func (p *Pipeline) SetModelHealth(h *domain.ModelHealth, d *domain.DriftReport) {
	p.mu.Lock()
	p.health, p.drift = h, d
	p.mu.Unlock()
}

// Regime returns the regime currently in force.
func (p *Pipeline) Regime() domain.MarketRegime {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.regime
}

// Snapshot returns the latest snapshot for a symbol.
func (p *Pipeline) Snapshot(t domain.Ticker) (domain.FeatureSnapshot, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s, ok := p.snapshots[t]
	return s, ok
}

// Snapshots returns every current snapshot.
func (p *Pipeline) Snapshots() map[domain.Ticker]domain.FeatureSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[domain.Ticker]domain.FeatureSnapshot, len(p.snapshots))
	for k, v := range p.snapshots {
		out[k] = v
	}
	return out
}

// OnQuote forwards a quote to the feature engine and updates marks.
func (p *Pipeline) OnQuote(q domain.Quote) {
	p.deps.Features.OnQuote(q)
	p.mu.Lock()
	if q.Last > 0 {
		p.lastPrice[q.Ticker] = q.Last
	}
	p.mu.Unlock()
}

// OnBar runs the full decision path for one completed bar.
//
// The order below is the pipeline from ADR-005 and must not be rearranged:
// features → regime → rules → prediction → risk → scoring → opportunity →
// signal → explanation. The risk gate sits before signal construction, and the
// explanation sits after everything numeric is final.
func (p *Pipeline) OnBar(ctx context.Context, c domain.Candle) Result {
	now := p.deps.Clock.Now()
	res := Result{Ticker: c.Ticker}

	// 1. Features.
	snap := p.deps.Features.OnCandle(c)
	res.Snapshot = snap

	p.mu.Lock()
	p.barsSeen++
	if prev, ok := p.snapshots[c.Ticker]; ok {
		p.prevSnaps[c.Ticker] = prev
	}
	p.snapshots[c.Ticker] = snap
	p.lastPrice[c.Ticker] = c.Close
	p.lastVol[c.Ticker] = c.Volume
	isMacro := p.macro[c.Ticker]
	p.mu.Unlock()

	// 2. Regime, recomputed when a market-context symbol updates. Recomputing
	//    on every symbol's bar would be wasteful and would make the regime
	//    depend on arrival order.
	if isMacro {
		p.updateRegime(c, now)
		res.Macro = true
		res.Regime = p.Regime()
		return res
	}
	rg := p.Regime()
	res.Regime = rg

	stock, ok := p.deps.Universe.Get(c.Ticker)
	if !ok {
		stock = domain.Stock{Ticker: c.Ticker}
	}
	res.Stock = stock

	strat, ok := p.selectStrategy(rg.Regime)
	if !ok {
		return res
	}
	res.StrategyID = strat.ID

	// 3. Deterministic rules.
	res.Rules = p.deps.Rules.Evaluate(strat, snap, rg.Regime)

	// 4. Prediction.
	pred, err := p.deps.Predict.Predict(ctx, predict.Input{
		Snapshot: snap, Rules: res.Rules, Regime: rg,
	})
	if err == nil {
		res.Prediction = &pred
	}

	// 5. Fundamentals and valuation (cached; they change quarterly).
	fund, val := p.fundamentalsFor(stock, snap, strat, now)
	res.Valuation = val

	// 6. Risk. Note this runs *before* scoring, because the score's risk
	//    component consumes the assessment.
	p.mu.RLock()
	events := p.events[c.Ticker]
	health := p.health
	newsEv, hasNews := p.news[c.Ticker]
	p.mu.RUnlock()
	res.Events = events

	side := domain.SideFlat
	if res.Prediction != nil {
		e := res.Prediction.Primary().Dist.DirectionalEdge()
		switch {
		case e > 0.02:
			side = domain.SideLong
		case e < -0.02:
			side = domain.SideShort
		}
	}
	var portfolio *domain.Portfolio
	if p.deps.Broker != nil {
		pf := p.deps.Broker.Portfolio()
		portfolio = &pf
	}
	assessment := p.deps.Risk.Assess(risk.Input{
		Ticker: c.Ticker, Stock: stock, Snapshot: snap, Prediction: res.Prediction,
		Regime: rg, Portfolio: portfolio, Events: events, Health: health,
		ProposedWeight: strat.Policy.MaxWeight, Side: side, Now: now,
	})
	res.Risk = &assessment

	// 7. Composite score.
	sectorPerf := p.sectorFor(stock.Sector)
	score := scoring.Score(scoring.Input{
		Stock: stock, Snapshot: snap, Rules: res.Rules, Regime: rg,
		Risk: &assessment, Fundamental: fund, Valuation: val,
		Sector: sectorPerf, Events: events, Strategy: strat, Now: now,
	})
	res.Score = &score

	// 8. Opportunity.
	if res.Prediction != nil {
		opp := opportunity.Evaluate(opportunity.Input{
			Stock: stock, Score: score, Prediction: *res.Prediction,
			Risk: assessment, Regime: rg, Snapshot: snap, Strategy: strat, Now: now,
		})
		res.Opportunity = &opp

		// 9. Signal. domain.NewSignal enforces the risk veto.
		sig, rej, err := p.deps.Signals.Generate(signal.Input{
			Stock: stock, Snapshot: snap, Rules: res.Rules, Score: score,
			Prediction: *res.Prediction, Opportunity: opp, Risk: assessment,
			Regime: rg, Strategy: strat, Now: now,
			CorrelationID: snap.Hash,
		})
		switch {
		case err != nil:
			res.Rejection = &signal.Rejection{Ticker: c.Ticker, Stage: "error", Reason: err.Error(), At: now}
		case rej != nil:
			res.Rejection = rej
		default:
			res.Signal = &sig
			if p.opts.TradePaper && p.deps.Broker != nil {
				p.submitPaperOrder(sig, now)
			}
		}
	}

	// 10. Invalidation sweep for this symbol's active signals.
	res.Invalidated = p.sweepTicker(c.Ticker, snap, rg, res.Prediction, &assessment, newsEv, hasNews, now)

	// 11. Alerts.
	p.mu.RLock()
	prevSnap, hasPrev := p.prevSnaps[c.Ticker]
	prevPred, hasPrevPred := p.prevPreds[c.Ticker]
	drift := p.drift
	p.mu.RUnlock()

	ai := alerts.Input{
		Ticker: c.Ticker, Now: now, Snapshot: snap, Regime: rg,
		Risk: &assessment, Signal: res.Signal, Drift: drift,
		Stale: snap.Stale, StaleReason: snap.StaleReason,
		CorrelationID: snap.Hash,
	}
	if hasPrev {
		ai.PrevSnapshot = &prevSnap
	}
	if res.Prediction != nil {
		ai.Prediction = res.Prediction
	}
	if hasPrevPred {
		ai.PrevPrediction = &prevPred
	}
	if hasNews {
		ai.News = &newsEv
	}
	if len(res.Invalidated) > 0 {
		ai.Invalidated = &res.Invalidated[0]
	}
	res.Alerts = p.deps.Alerts.Evaluate(ai)

	if res.Prediction != nil {
		p.mu.Lock()
		p.prevPreds[c.Ticker] = *res.Prediction
		p.mu.Unlock()
	}
	return res
}

// selectStrategy picks the strategy to evaluate in the current regime.
//
// The configured default is used whenever it applies. When it does not — the
// momentum strategy in a range, say — the pipeline falls back to the first
// enabled strategy that does, in lexical order so the choice is reproducible.
//
// The alternative, evaluating the configured strategy regardless, is worse than
// it looks: the rule engine correctly refuses to run a trend strategy in a
// range, so every symbol comes back with no evidence, no score and a perfectly
// symmetric prediction, and the platform silently produces nothing for as long
// as the regime lasts. Choosing a strategy suited to the regime is the whole
// point of classifying the regime in the first place.
func (p *Pipeline) selectStrategy(rg domain.Regime) (domain.Strategy, bool) {
	if s, ok := p.deps.Strategies.Get(p.opts.StrategyID); ok && s.AppliesTo(rg) {
		return s, true
	}
	all := p.deps.Strategies.All() // Registry.All returns a sorted copy.
	for _, s := range all {
		if s.Enabled && s.AppliesTo(rg) {
			return s, true
		}
	}
	// Nothing matches the regime. Return the configured strategy anyway so the
	// rule engine records an explicit "not enabled in this regime" skip rather
	// than the pipeline dropping the bar with no explanation at all.
	if s, ok := p.deps.Strategies.Get(p.opts.StrategyID); ok {
		return s, true
	}
	if len(all) > 0 {
		return all[0], true
	}
	return domain.Strategy{}, false
}

func (p *Pipeline) submitPaperOrder(sig domain.Signal, now time.Time) {
	if !p.deps.Broker.CanOpen(sig.Ticker) {
		return
	}
	qty := p.deps.Broker.SizeForWeight(sig.Ticker, sig.SuggestedWeight)
	if qty <= 0 {
		return
	}
	side := domain.OrderBuy
	if sig.Side == domain.SideShort {
		side = domain.OrderSell
	}
	stock, _ := p.deps.Universe.Get(sig.Ticker)
	_, _ = p.deps.Broker.Submit(paper.OrderRequest{
		// The idempotency key is the signal id: replaying signal.generated
		// cannot open a second position (requirement §36).
		IdempotencyKey: "signal:" + sig.ID,
		Ticker:         sig.Ticker,
		Side:           side,
		Type:           domain.OrderMarket,
		Quantity:       qty,
		SignalID:       sig.ID,
		Sector:         stock.Sector,
		Now:            now,
	})
}

func (p *Pipeline) sweepTicker(t domain.Ticker, snap domain.FeatureSnapshot, rg domain.MarketRegime,
	pred *domain.Prediction, assessment *domain.RiskAssessment, news domain.NewsEvent, hasNews bool, now time.Time) []domain.Signal {

	active := p.deps.Signals.ActiveFor(t)
	if len(active) == 0 {
		return nil
	}
	in := signal.EvalInput{Snapshot: snap, Regime: rg, Prediction: pred, Risk: assessment, Now: now}
	if hasNews && news.Material() {
		in.MaterialNews = &news
	}
	var out []domain.Signal
	for _, s := range active {
		chk, triggered := signal.Evaluate(s, in)
		if !triggered {
			continue
		}
		if inv, ok := p.deps.Signals.Invalidate(s.ID, chk.Kind, chk.Note, now); ok {
			out = append(out, inv)
			// Close any paper position the signal opened.
			if p.opts.TradePaper && p.deps.Broker != nil {
				p.closePaperPosition(inv, now)
			}
		}
	}
	return out
}

func (p *Pipeline) closePaperPosition(sig domain.Signal, now time.Time) {
	for _, pos := range p.deps.Broker.Positions() {
		if pos.Ticker != sig.Ticker || pos.Quantity == 0 {
			continue
		}
		side := domain.OrderSell
		qty := pos.Quantity
		if qty < 0 {
			side, qty = domain.OrderBuy, -qty
		}
		_, _ = p.deps.Broker.Submit(paper.OrderRequest{
			IdempotencyKey: "close:" + sig.ID,
			Ticker:         sig.Ticker, Side: side, Type: domain.OrderMarket,
			Quantity: qty, SignalID: sig.ID, Now: now,
		})
	}
}

func (p *Pipeline) updateRegime(c domain.Candle, now time.Time) {
	snaps := p.Snapshots()
	vix := 0.0
	if s, ok := snaps["VIX"]; ok {
		vix = s.MustGet(domain.FeatLast, 0)
	}
	breadth, volRatio := p.breadth(snaps)

	rg := p.deps.Regime.Classify(regime.Input{
		AsOf: c.End, Snapshots: snaps, VIXLevel: vix,
		Breadth: breadth, VolumeRatio: volRatio,
	})
	p.mu.Lock()
	p.regime = rg
	p.mu.Unlock()

	if p.deps.Relationship != nil {
		p.deps.Relationship.Update(c.End, p.deps.Features, snaps)
	}
	_ = now
}

// breadth computes the advance/decline balance and average volume ratio across
// non-macro symbols.
func (p *Pipeline) breadth(snaps map[domain.Ticker]domain.FeatureSnapshot) (float64, float64) {
	adv, dec := 0, 0
	volSum, volN := 0.0, 0.0
	keys := make([]domain.Ticker, 0, len(snaps))
	for t := range snaps {
		keys = append(keys, t)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, t := range keys {
		if p.macro[t] {
			continue
		}
		s := snaps[t]
		if r, ok := s.Get(domain.FeatRetLog1); ok {
			switch {
			case r > 0:
				adv++
			case r < 0:
				dec++
			}
		}
		if v, ok := s.Get(domain.FeatVolRatio); ok {
			volSum += v
			volN++
		}
	}
	breadth := 0.0
	if adv+dec > 0 {
		breadth = float64(adv-dec) / float64(adv+dec)
	}
	volRatio := 1.0
	if volN > 0 {
		volRatio = volSum / volN
	}
	return breadth, volRatio
}

func (p *Pipeline) sectorFor(sector string) *domain.SectorPerformance {
	if p.deps.Relationship == nil || sector == "" {
		return nil
	}
	for _, s := range p.deps.Relationship.Latest().Sectors {
		if s.Sector == sector {
			out := s
			return &out
		}
	}
	return nil
}

func (p *Pipeline) fundamentalsFor(stock domain.Stock, snap domain.FeatureSnapshot,
	strat domain.Strategy, now time.Time) (*fundamentals.Scores, *domain.ValuationMetrics) {

	if p.deps.Fundamentals == nil || stock.AssetClass == domain.AssetETF || stock.AssetClass == domain.AssetIndex {
		return nil, nil
	}
	p.mu.RLock()
	entry, ok := p.fundCache[stock.Ticker]
	p.mu.RUnlock()
	if ok && now.Sub(entry.at) < p.opts.FundamentalRefresh {
		s, v := entry.scores, entry.valuation
		return &s, &v
	}

	periods, err := p.deps.Fundamentals.Periods(stock.Ticker, now, 13)
	if err != nil || len(periods) == 0 {
		return nil, nil
	}
	metrics := fundamentals.Compute(stock.Ticker, periods, now)
	scores := fundamentals.Score(metrics, strat.Fundamental)
	val := valuation.Compute(valuation.Input{
		Stock: stock, Price: snap.MustGet(domain.FeatLast, snap.Last), AsOf: now,
		Periods: periods, Metrics: metrics,
	}, strat.Fundamental)

	p.mu.Lock()
	p.fundCache[stock.Ticker] = fundCacheEntry{at: now, metrics: metrics, scores: scores, valuation: val}
	p.mu.Unlock()
	return &scores, &val
}

// Marks returns the last prices and volumes, for the paper broker.
func (p *Pipeline) Marks() (map[domain.Ticker]float64, map[domain.Ticker]float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	prices := make(map[domain.Ticker]float64, len(p.lastPrice))
	vols := make(map[domain.Ticker]float64, len(p.lastVol))
	for k, v := range p.lastPrice {
		prices[k] = v
	}
	for k, v := range p.lastVol {
		vols[k] = v
	}
	return prices, vols
}

// BarsProcessed returns how many bars the pipeline has seen.
func (p *Pipeline) BarsProcessed() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.barsSeen
}

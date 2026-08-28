// Package api exposes the REST surface and the read model behind it.
//
// The read model (State) is deliberately separate from the engines. The
// pipeline produces immutable results; State records the latest of each kind
// per instrument and answers queries from that. Keeping them apart means an API
// read can never perturb the decision path, and a slow dashboard query cannot
// add latency to signal generation.
package api

import (
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/analyst"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/pipeline"
)

// InstrumentView is everything the dashboard shows for one instrument.
type InstrumentView struct {
	Stock       domain.Stock             `json:"stock"`
	Snapshot    *domain.FeatureSnapshot  `json:"features,omitempty"`
	Score       *domain.StockScore       `json:"score,omitempty"`
	Prediction  *domain.Prediction       `json:"prediction,omitempty"`
	Risk        *domain.RiskAssessment   `json:"risk,omitempty"`
	Opportunity *domain.OpportunityScore `json:"opportunity,omitempty"`
	Valuation   *domain.ValuationMetrics `json:"valuation,omitempty"`
	Options     *domain.OptionsMetrics   `json:"options,omitempty"`
	Signals     []domain.Signal          `json:"signals,omitempty"`
	News        []domain.NewsEvent       `json:"news,omitempty"`
	Events      []domain.CorporateEvent  `json:"events,omitempty"`
	Report      *analyst.Report          `json:"explanation,omitempty"`
	Evidence    []string                 `json:"evidence,omitempty"`
	Counter     []string                 `json:"counter_evidence,omitempty"`
	UpdatedAt   time.Time                `json:"updated_at"`
}

// State is the in-memory read model.
type State struct {
	mu sync.RWMutex

	snapshots   map[domain.Ticker]domain.FeatureSnapshot
	scores      map[domain.Ticker]domain.StockScore
	predictions map[domain.Ticker]domain.Prediction
	risks       map[domain.Ticker]domain.RiskAssessment
	opps        map[domain.Ticker]domain.OpportunityScore
	reports     map[domain.Ticker]analyst.Report
	evidence    map[domain.Ticker][]string
	counter     map[domain.Ticker][]string
	news        map[domain.Ticker][]domain.NewsEvent
	events      map[domain.Ticker][]domain.CorporateEvent
	rejections  map[domain.Ticker]string
	updated     map[domain.Ticker]time.Time

	regime domain.MarketRegime
	rel    domain.RelationshipState
	health domain.ModelHealth
	drift  domain.DriftReport

	maxNewsPerTicker int
}

// NewState builds an empty read model.
func NewState() *State {
	return &State{
		snapshots:        map[domain.Ticker]domain.FeatureSnapshot{},
		scores:           map[domain.Ticker]domain.StockScore{},
		predictions:      map[domain.Ticker]domain.Prediction{},
		risks:            map[domain.Ticker]domain.RiskAssessment{},
		opps:             map[domain.Ticker]domain.OpportunityScore{},
		reports:          map[domain.Ticker]analyst.Report{},
		evidence:         map[domain.Ticker][]string{},
		counter:          map[domain.Ticker][]string{},
		news:             map[domain.Ticker][]domain.NewsEvent{},
		events:           map[domain.Ticker][]domain.CorporateEvent{},
		rejections:       map[domain.Ticker]string{},
		updated:          map[domain.Ticker]time.Time{},
		maxNewsPerTicker: 25,
	}
}

// SetReport records an AI explanation for a symbol.
//
// It is a separate entry point from Observe because the explanation is produced
// outside the decision pipeline: the pipeline cannot reach a language model at
// all, so the narrative arrives here on its own path (ADR-005).
func (s *State) SetReport(t domain.Ticker, rep analyst.Report) {
	s.mu.Lock()
	s.reports[t] = rep
	s.mu.Unlock()
}

// Rejections returns, per symbol, the stage and reason the platform declined to
// emit a signal.
//
// It is exported because "produced nothing" and "declined for these reasons"
// look identical from outside and mean entirely different things.
func (s *State) Rejections() map[domain.Ticker]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.Ticker]string, len(s.rejections))
	for k, v := range s.rejections {
		out[k] = v
	}
	return out
}

// Observe records a pipeline result.
func (s *State) Observe(res pipeline.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := res.Ticker
	s.snapshots[t] = res.Snapshot
	s.updated[t] = res.Snapshot.AsOf
	if res.Regime.Regime != "" {
		s.regime = res.Regime
	}
	if res.Score != nil {
		s.scores[t] = *res.Score
	}
	if res.Prediction != nil {
		s.predictions[t] = *res.Prediction
	}
	if res.Risk != nil {
		s.risks[t] = *res.Risk
	}
	if res.Opportunity != nil {
		s.opps[t] = *res.Opportunity
	}
	if len(res.Rules.Evidence) > 0 {
		s.evidence[t] = res.Rules.Evidence
	}
	if len(res.Rules.CounterEvidence) > 0 {
		s.counter[t] = res.Rules.CounterEvidence
	}
	if res.Rejection != nil {
		s.rejections[t] = res.Rejection.Stage + ": " + res.Rejection.Reason
	} else if res.Signal != nil {
		delete(s.rejections, t)
	}
}

// SetRelationship records the cross-asset state.
func (s *State) SetRelationship(r domain.RelationshipState) {
	s.mu.Lock()
	s.rel = r
	s.mu.Unlock()
}

// SetModelHealth records model health and drift.
func (s *State) SetModelHealth(h domain.ModelHealth, d domain.DriftReport) {
	s.mu.Lock()
	s.health, s.drift = h, d
	s.mu.Unlock()
}

// AddNews records a news event against its instrument.
func (s *State) AddNews(n domain.NewsEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append([]domain.NewsEvent{n}, s.news[n.Ticker]...)
	if len(list) > s.maxNewsPerTicker {
		list = list[:s.maxNewsPerTicker]
	}
	s.news[n.Ticker] = list
}

// SetEvents records the corporate-event calendar for an instrument.
func (s *State) SetEvents(t domain.Ticker, events []domain.CorporateEvent) {
	s.mu.Lock()
	s.events[t] = events
	s.mu.Unlock()
}

// Regime returns the current market regime.
func (s *State) Regime() domain.MarketRegime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.regime
}

// Relationship returns the cross-asset state.
func (s *State) Relationship() domain.RelationshipState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rel
}

// ModelHealth returns model health and drift.
func (s *State) ModelHealth() (domain.ModelHealth, domain.DriftReport) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health, s.drift
}

// View assembles the instrument view.
func (s *State) View(stock domain.Stock, signals []domain.Signal) InstrumentView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := stock.Ticker
	v := InstrumentView{Stock: stock, Signals: signals, UpdatedAt: s.updated[t]}
	if x, ok := s.snapshots[t]; ok {
		v.Snapshot = &x
	}
	if x, ok := s.scores[t]; ok {
		v.Score = &x
	}
	if x, ok := s.predictions[t]; ok {
		v.Prediction = &x
	}
	if x, ok := s.risks[t]; ok {
		v.Risk = &x
	}
	if x, ok := s.opps[t]; ok {
		v.Opportunity = &x
	}
	if x, ok := s.reports[t]; ok {
		v.Report = &x
	}
	v.Evidence = append([]string(nil), s.evidence[t]...)
	v.Counter = append([]string(nil), s.counter[t]...)
	v.News = append([]domain.NewsEvent(nil), s.news[t]...)
	v.Events = append([]domain.CorporateEvent(nil), s.events[t]...)
	return v
}

// Ranked returns instruments ordered by opportunity score.
type Ranked struct {
	Ticker         domain.Ticker         `json:"ticker"`
	Company        string                `json:"company"`
	Sector         string                `json:"sector"`
	Last           float64               `json:"last"`
	Score          float64               `json:"score"`
	Opportunity    float64               `json:"opportunity"`
	Classification domain.Classification `json:"classification"`
	Bias           domain.Side           `json:"bias"`
	Confidence     float64               `json:"confidence"`
	Risk           domain.RiskLevel      `json:"risk"`
	RiskDecision   domain.RiskDecision   `json:"risk_decision"`
	Probability    domain.Distribution   `json:"probability"`
	Stale          bool                  `json:"stale"`
	Rejection      string                `json:"rejection,omitempty"`
	UpdatedAt      time.Time             `json:"updated_at"`
}

// Rank returns the ranked instrument list.
//
// Ordering is opportunity descending with the ticker as a tiebreak, so the same
// state always produces the same list — a UI that reshuffles equal-scoring rows
// on every poll is a UI nobody trusts.
func (s *State) Rank(lookup func(domain.Ticker) (domain.Stock, bool), sector string, limit int) []Ranked {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Ranked, 0, len(s.scores))
	for t, sc := range s.scores {
		stock := domain.Stock{Ticker: t}
		if lookup != nil {
			if x, ok := lookup(t); ok {
				stock = x
			}
		}
		if sector != "" && stock.Sector != sector {
			continue
		}
		r := Ranked{
			Ticker: t, Company: stock.Company, Sector: stock.Sector,
			Score: sc.FinalScore, Classification: sc.Classification,
			UpdatedAt: s.updated[t], Rejection: s.rejections[t],
		}
		if snap, ok := s.snapshots[t]; ok {
			r.Last = snap.MustGet(domain.FeatLast, snap.Last)
			r.Stale = snap.Stale
		}
		if o, ok := s.opps[t]; ok {
			r.Opportunity, r.Bias, r.Confidence, r.Risk = o.Score, o.Bias, o.Confidence, o.Risk
		}
		if rk, ok := s.risks[t]; ok {
			r.RiskDecision = rk.Decision
		}
		if p, ok := s.predictions[t]; ok {
			r.Probability = p.Primary().Dist
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Opportunity == out[j].Opportunity {
			return out[i].Ticker < out[j].Ticker
		}
		return out[i].Opportunity > out[j].Opportunity
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Snapshots returns a copy of the current feature snapshots.
func (s *State) Snapshots() map[domain.Ticker]domain.FeatureSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.Ticker]domain.FeatureSnapshot, len(s.snapshots))
	for k, v := range s.snapshots {
		out[k] = v
	}
	return out
}

// Scores returns a copy of the current scores.
func (s *State) Scores() map[domain.Ticker]domain.StockScore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.Ticker]domain.StockScore, len(s.scores))
	for k, v := range s.scores {
		out[k] = v
	}
	return out
}

// Predictions returns a copy of the current predictions.
func (s *State) Predictions() map[domain.Ticker]domain.Prediction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.Ticker]domain.Prediction, len(s.predictions))
	for k, v := range s.predictions {
		out[k] = v
	}
	return out
}

// Opportunities returns a copy of the current opportunity scores.
func (s *State) Opportunities() map[domain.Ticker]domain.OpportunityScore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.Ticker]domain.OpportunityScore, len(s.opps))
	for k, v := range s.opps {
		out[k] = v
	}
	return out
}

// Risks returns a copy of the current risk assessments.
func (s *State) Risks() map[domain.Ticker]domain.RiskAssessment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.Ticker]domain.RiskAssessment, len(s.risks))
	for k, v := range s.risks {
		out[k] = v
	}
	return out
}

// Evidence returns the rule evidence per instrument.
func (s *State) Evidence() (map[domain.Ticker][]string, map[domain.Ticker][]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ev := make(map[domain.Ticker][]string, len(s.evidence))
	for k, v := range s.evidence {
		ev[k] = append([]string(nil), v...)
	}
	ct := make(map[domain.Ticker][]string, len(s.counter))
	for k, v := range s.counter {
		ct[k] = append([]string(nil), v...)
	}
	return ev, ct
}

// AllNews returns every retained news event, newest first.
func (s *State) AllNews(limit int) []domain.NewsEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.NewsEvent
	for _, list := range s.news {
		out = append(out, list...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].ID < out[j].ID
		}
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// AllEvents returns the corporate-event calendar.
func (s *State) AllEvents() map[domain.Ticker][]domain.CorporateEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.Ticker][]domain.CorporateEvent, len(s.events))
	for k, v := range s.events {
		out[k] = append([]domain.CorporateEvent(nil), v...)
	}
	return out
}

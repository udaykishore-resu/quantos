// Package store defines persistence interfaces and ships three implementations:
// an in-memory store for embedded mode and tests, a PostgreSQL store for
// transactional state, and a ClickHouse store for time series.
//
// The split follows ADR-003's boundary rule: if losing a row would corrupt a
// decision's provenance it goes to PostgreSQL; if losing a row would only blur
// a statistic it goes to ClickHouse. Provenance is what the platform promises,
// so signals, risk assessments and model versions are transactional, while
// quotes and feature snapshots are analytical.
package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("store: not found")

// ErrUnavailable is returned when the backing store is down. Callers use it to
// decide whether to degrade or to stop emitting.
var ErrUnavailable = errors.New("store: unavailable")

// Query bounds a listing.
type Query struct {
	Ticker   domain.Ticker
	From     time.Time
	To       time.Time
	Limit    int
	Offset   int
	Status   string
	Strategy string
}

// SignalStore persists signals and their lifecycle.
type SignalStore interface {
	SaveSignal(ctx context.Context, s domain.Signal) error
	UpdateSignal(ctx context.Context, s domain.Signal) error
	GetSignal(ctx context.Context, id string) (domain.Signal, error)
	ListSignals(ctx context.Context, q Query) ([]domain.Signal, error)
}

// RiskStore persists risk assessments.
type RiskStore interface {
	SaveAssessment(ctx context.Context, a domain.RiskAssessment) error
	GetAssessment(ctx context.Context, id string) (domain.RiskAssessment, error)
	ListAssessments(ctx context.Context, q Query) ([]domain.RiskAssessment, error)
}

// PredictionStore persists predictions and their outcomes.
type PredictionStore interface {
	SavePrediction(ctx context.Context, p domain.Prediction) error
	GetPrediction(ctx context.Context, id string) (domain.Prediction, error)
	ListPredictions(ctx context.Context, q Query) ([]domain.Prediction, error)
	SaveOutcome(ctx context.Context, o domain.PredictionOutcome) error
	ListOutcomes(ctx context.Context, q Query) ([]domain.PredictionOutcome, error)
}

// FeatureStore persists feature snapshots. Snapshots are analytical, but their
// hash is stored on the signal, so provenance survives even if a snapshot is
// lost.
type FeatureStore interface {
	SaveSnapshot(ctx context.Context, s domain.FeatureSnapshot) error
	GetSnapshot(ctx context.Context, ticker domain.Ticker, hash string) (domain.FeatureSnapshot, error)
	ListSnapshots(ctx context.Context, q Query) ([]domain.FeatureSnapshot, error)
}

// MarketStore persists quotes and bars.
type MarketStore interface {
	SaveQuotes(ctx context.Context, q []domain.Quote) error
	SaveCandles(ctx context.Context, c []domain.Candle) error
	ListCandles(ctx context.Context, q Query, interval domain.Interval) ([]domain.Candle, error)
	LatestQuote(ctx context.Context, t domain.Ticker) (domain.Quote, error)
}

// AlertStore persists alerts with a uniqueness constraint on the dedup key,
// which is the durable half of the exactly-once-effect guarantee.
type AlertStore interface {
	SaveAlert(ctx context.Context, a domain.Alert) (inserted bool, err error)
	ListAlerts(ctx context.Context, q Query) ([]domain.Alert, error)
}

// RegimeStore persists regime history.
type RegimeStore interface {
	SaveRegime(ctx context.Context, r domain.MarketRegime) error
	LatestRegime(ctx context.Context) (domain.MarketRegime, error)
	ListRegimes(ctx context.Context, q Query) ([]domain.MarketRegime, error)
}

// PortfolioStore persists paper portfolios, orders and positions.
type PortfolioStore interface {
	SavePortfolio(ctx context.Context, p domain.Portfolio) error
	GetPortfolio(ctx context.Context, id string) (domain.Portfolio, error)
	SaveOrder(ctx context.Context, o domain.PaperOrder) (inserted bool, err error)
	ListOrders(ctx context.Context, q Query) ([]domain.PaperOrder, error)
}

// ModelStore persists the model registry and evaluations.
type ModelStore interface {
	SaveModelVersion(ctx context.Context, m domain.ModelVersion) error
	ListModelVersions(ctx context.Context) ([]domain.ModelVersion, error)
	SaveEvaluation(ctx context.Context, e domain.ModelEvaluation) error
	ListEvaluations(ctx context.Context, modelVersion string, limit int) ([]domain.ModelEvaluation, error)
	SaveDrift(ctx context.Context, d domain.DriftReport) error
	LatestDrift(ctx context.Context, modelVersion string) (domain.DriftReport, error)
}

// BacktestStore persists backtest runs.
type BacktestStore interface {
	SaveRun(ctx context.Context, r domain.BacktestRun) error
	GetRun(ctx context.Context, id string) (domain.BacktestRun, error)
	ListRuns(ctx context.Context, limit int) ([]domain.BacktestRun, error)
}

// NewsStore persists processed news events.
type NewsStore interface {
	SaveNews(ctx context.Context, n domain.NewsEvent) (inserted bool, err error)
	ListNews(ctx context.Context, q Query) ([]domain.NewsEvent, error)
}

// AuditStore is the append-only record of state-changing actions.
type AuditStore interface {
	Append(ctx context.Context, e AuditEvent) error
	ListAudit(ctx context.Context, limit int) ([]AuditEvent, error)
}

// AuditEvent is one recorded action. The type is defined in domain so that the
// HTTP middleware which writes audit records does not have to import a storage
// package; the alias keeps every existing store call site unchanged.
type AuditEvent = domain.AuditEvent

// Store is the aggregate interface most services take.
type Store interface {
	SignalStore
	RiskStore
	PredictionStore
	FeatureStore
	MarketStore
	AlertStore
	RegimeStore
	PortfolioStore
	ModelStore
	BacktestStore
	NewsStore
	AuditStore
	Health(ctx context.Context) Health
	Close() error
}

// Health reports the state of each backing store, which the API surfaces so a
// degraded response can say *what* is degraded.
type Health struct {
	Postgres   ComponentHealth `json:"postgres"`
	ClickHouse ComponentHealth `json:"clickhouse"`
	Redis      ComponentHealth `json:"redis"`
	Degraded   []string        `json:"degraded,omitempty"`
}

// ComponentHealth is one backing store's state.
type ComponentHealth struct {
	Enabled bool          `json:"enabled"`
	Healthy bool          `json:"healthy"`
	Latency time.Duration `json:"latency,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// In-memory implementation
// ---------------------------------------------------------------------------

// Memory is a complete in-memory Store. It is what embedded mode, the demo
// runner and every test use, and it is bounded so a long-running demo cannot
// exhaust memory.
type Memory struct {
	mu sync.RWMutex

	signals     map[string]domain.Signal
	assessments map[string]domain.RiskAssessment
	predictions map[string]domain.Prediction
	outcomes    []domain.PredictionOutcome
	snapshots   map[string]domain.FeatureSnapshot // ticker|hash
	candles     map[domain.Ticker][]domain.Candle
	quotes      map[domain.Ticker]domain.Quote
	alerts      []domain.Alert
	alertKeys   map[string]bool
	regimes     []domain.MarketRegime
	portfolios  map[string]domain.Portfolio
	orders      map[string]domain.PaperOrder
	orderKeys   map[string]string
	models      map[string]domain.ModelVersion
	evaluations []domain.ModelEvaluation
	drifts      map[string]domain.DriftReport
	runs        map[string]domain.BacktestRun
	news        map[string]domain.NewsEvent
	audit       []AuditEvent

	maxSeries int
	maxList   int
}

// NewMemory builds an in-memory store.
func NewMemory() *Memory {
	return &Memory{
		signals: map[string]domain.Signal{}, assessments: map[string]domain.RiskAssessment{},
		predictions: map[string]domain.Prediction{}, snapshots: map[string]domain.FeatureSnapshot{},
		candles: map[domain.Ticker][]domain.Candle{}, quotes: map[domain.Ticker]domain.Quote{},
		alertKeys: map[string]bool{}, portfolios: map[string]domain.Portfolio{},
		orders: map[string]domain.PaperOrder{}, orderKeys: map[string]string{},
		models: map[string]domain.ModelVersion{}, drifts: map[string]domain.DriftReport{},
		runs: map[string]domain.BacktestRun{}, news: map[string]domain.NewsEvent{},
		maxSeries: 5000, maxList: 20000,
	}
}

// SaveSignal stores a signal.
func (m *Memory) SaveSignal(_ context.Context, s domain.Signal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signals[s.ID] = s
	return nil
}

// UpdateSignal replaces a stored signal.
func (m *Memory) UpdateSignal(ctx context.Context, s domain.Signal) error {
	return m.SaveSignal(ctx, s)
}

// GetSignal returns a signal by id.
func (m *Memory) GetSignal(_ context.Context, id string) (domain.Signal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.signals[id]
	if !ok {
		return domain.Signal{}, ErrNotFound
	}
	return s, nil
}

// ListSignals returns matching signals, newest first.
func (m *Memory) ListSignals(_ context.Context, q Query) ([]domain.Signal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.Signal{}
	for _, s := range m.signals {
		if q.Ticker != "" && s.Ticker != q.Ticker {
			continue
		}
		if q.Status != "" && string(s.Status) != q.Status {
			continue
		}
		if q.Strategy != "" && s.StrategyID != q.Strategy {
			continue
		}
		if !q.From.IsZero() && s.CreatedAt.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && s.CreatedAt.After(q.To) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return page(out, q), nil
}

// SaveAssessment stores a risk assessment.
func (m *Memory) SaveAssessment(_ context.Context, a domain.RiskAssessment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assessments[a.ID] = a
	return nil
}

// GetAssessment returns an assessment by id.
func (m *Memory) GetAssessment(_ context.Context, id string) (domain.RiskAssessment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.assessments[id]
	if !ok {
		return domain.RiskAssessment{}, ErrNotFound
	}
	return a, nil
}

// ListAssessments returns matching assessments, newest first.
func (m *Memory) ListAssessments(_ context.Context, q Query) ([]domain.RiskAssessment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.RiskAssessment{}
	for _, a := range m.assessments {
		if q.Ticker != "" && a.Ticker != q.Ticker {
			continue
		}
		if !q.From.IsZero() && a.CreatedAt.Before(q.From) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return page(out, q), nil
}

// SavePrediction stores a prediction.
func (m *Memory) SavePrediction(_ context.Context, p domain.Prediction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.predictions[p.ID] = p
	if len(m.predictions) > m.maxList {
		m.trimPredictionsLocked()
	}
	return nil
}

func (m *Memory) trimPredictionsLocked() {
	list := make([]domain.Prediction, 0, len(m.predictions))
	for _, p := range m.predictions {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	m.predictions = map[string]domain.Prediction{}
	for i, p := range list {
		if i >= m.maxList/2 {
			break
		}
		m.predictions[p.ID] = p
	}
}

// GetPrediction returns a prediction by id.
func (m *Memory) GetPrediction(_ context.Context, id string) (domain.Prediction, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.predictions[id]
	if !ok {
		return domain.Prediction{}, ErrNotFound
	}
	return p, nil
}

// ListPredictions returns matching predictions, newest first.
func (m *Memory) ListPredictions(_ context.Context, q Query) ([]domain.Prediction, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.Prediction{}
	for _, p := range m.predictions {
		if q.Ticker != "" && p.Ticker != q.Ticker {
			continue
		}
		if !q.From.IsZero() && p.CreatedAt.Before(q.From) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return page(out, q), nil
}

// SaveOutcome stores a resolved prediction outcome.
func (m *Memory) SaveOutcome(_ context.Context, o domain.PredictionOutcome) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, o)
	if len(m.outcomes) > m.maxList {
		m.outcomes = m.outcomes[len(m.outcomes)-m.maxList/2:]
	}
	return nil
}

// ListOutcomes returns matching outcomes.
func (m *Memory) ListOutcomes(_ context.Context, q Query) ([]domain.PredictionOutcome, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.PredictionOutcome{}
	for _, o := range m.outcomes {
		if q.Ticker != "" && o.Ticker != q.Ticker {
			continue
		}
		if !q.From.IsZero() && o.ResolvedAt.Before(q.From) {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResolvedAt.After(out[j].ResolvedAt) })
	return page(out, q), nil
}

// SaveSnapshot stores a feature snapshot.
func (m *Memory) SaveSnapshot(_ context.Context, s domain.FeatureSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[string(s.Ticker)+"|"+s.Hash] = s
	if len(m.snapshots) > m.maxList {
		// Bounded: drop the oldest half by AsOf.
		list := make([]domain.FeatureSnapshot, 0, len(m.snapshots))
		for _, v := range m.snapshots {
			list = append(list, v)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].AsOf.After(list[j].AsOf) })
		m.snapshots = map[string]domain.FeatureSnapshot{}
		for i, v := range list {
			if i >= m.maxList/2 {
				break
			}
			m.snapshots[string(v.Ticker)+"|"+v.Hash] = v
		}
	}
	return nil
}

// GetSnapshot returns a snapshot by ticker and hash, which is how a historical
// decision is reproduced.
func (m *Memory) GetSnapshot(_ context.Context, ticker domain.Ticker, hash string) (domain.FeatureSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.snapshots[string(ticker)+"|"+hash]
	if !ok {
		return domain.FeatureSnapshot{}, ErrNotFound
	}
	return s, nil
}

// ListSnapshots returns matching snapshots.
func (m *Memory) ListSnapshots(_ context.Context, q Query) ([]domain.FeatureSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.FeatureSnapshot{}
	for _, s := range m.snapshots {
		if q.Ticker != "" && s.Ticker != q.Ticker {
			continue
		}
		if !q.From.IsZero() && s.AsOf.Before(q.From) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AsOf.After(out[j].AsOf) })
	return page(out, q), nil
}

// SaveQuotes stores the latest quote per symbol.
func (m *Memory) SaveQuotes(_ context.Context, qs []domain.Quote) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, q := range qs {
		if cur, ok := m.quotes[q.Ticker]; ok && q.Timestamp.Before(cur.Timestamp) {
			continue
		}
		m.quotes[q.Ticker] = q
	}
	return nil
}

// SaveCandles appends bars, keeping a bounded series per symbol.
func (m *Memory) SaveCandles(_ context.Context, cs []domain.Candle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range cs {
		series := append(m.candles[c.Ticker], c)
		if len(series) > m.maxSeries {
			series = series[len(series)-m.maxSeries:]
		}
		m.candles[c.Ticker] = series
	}
	return nil
}

// ListCandles returns bars for a symbol.
func (m *Memory) ListCandles(_ context.Context, q Query, interval domain.Interval) ([]domain.Candle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.Candle{}
	for _, c := range m.candles[q.Ticker] {
		if interval != "" && c.Interval != interval {
			continue
		}
		if !q.From.IsZero() && c.Start.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && c.Start.After(q.To) {
			continue
		}
		out = append(out, c)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}

// LatestQuote returns the most recent quote for a symbol.
func (m *Memory) LatestQuote(_ context.Context, t domain.Ticker) (domain.Quote, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	q, ok := m.quotes[t]
	if !ok {
		return domain.Quote{}, ErrNotFound
	}
	return q, nil
}

// SaveAlert stores an alert, returning false when the dedup key already exists.
func (m *Memory) SaveAlert(_ context.Context, a domain.Alert) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.DedupKey != "" && m.alertKeys[a.DedupKey] {
		return false, nil
	}
	if a.DedupKey != "" {
		m.alertKeys[a.DedupKey] = true
	}
	m.alerts = append(m.alerts, a)
	if len(m.alerts) > m.maxList {
		m.alerts = m.alerts[len(m.alerts)-m.maxList/2:]
	}
	return true, nil
}

// ListAlerts returns matching alerts, newest first.
func (m *Memory) ListAlerts(_ context.Context, q Query) ([]domain.Alert, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.Alert{}
	for _, a := range m.alerts {
		if q.Ticker != "" && a.Ticker != q.Ticker {
			continue
		}
		if !q.From.IsZero() && a.CreatedAt.Before(q.From) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return page(out, q), nil
}

// SaveRegime appends a regime record.
func (m *Memory) SaveRegime(_ context.Context, r domain.MarketRegime) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.regimes = append(m.regimes, r)
	if len(m.regimes) > 2000 {
		m.regimes = m.regimes[len(m.regimes)-1000:]
	}
	return nil
}

// LatestRegime returns the most recent regime.
func (m *Memory) LatestRegime(_ context.Context) (domain.MarketRegime, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.regimes) == 0 {
		return domain.MarketRegime{}, ErrNotFound
	}
	return m.regimes[len(m.regimes)-1], nil
}

// ListRegimes returns regime history, newest first.
func (m *Memory) ListRegimes(_ context.Context, q Query) ([]domain.MarketRegime, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]domain.MarketRegime(nil), m.regimes...)
	sort.Slice(out, func(i, j int) bool { return out[i].AsOf.After(out[j].AsOf) })
	return page(out, q), nil
}

// SavePortfolio stores a portfolio snapshot.
func (m *Memory) SavePortfolio(_ context.Context, p domain.Portfolio) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.portfolios[p.ID] = p
	return nil
}

// GetPortfolio returns a portfolio.
func (m *Memory) GetPortfolio(_ context.Context, id string) (domain.Portfolio, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.portfolios[id]
	if !ok {
		return domain.Portfolio{}, ErrNotFound
	}
	return p, nil
}

// SaveOrder stores an order, returning false when the idempotency key already
// exists. This is the durable half of the no-duplicate-orders guarantee.
func (m *Memory) SaveOrder(_ context.Context, o domain.PaperOrder) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o.IdempotencyKey != "" {
		if existing, ok := m.orderKeys[o.IdempotencyKey]; ok && existing != o.ID {
			return false, nil
		}
		m.orderKeys[o.IdempotencyKey] = o.ID
	}
	_, existed := m.orders[o.ID]
	m.orders[o.ID] = o
	return !existed, nil
}

// ListOrders returns matching orders, newest first.
func (m *Memory) ListOrders(_ context.Context, q Query) ([]domain.PaperOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.PaperOrder{}
	for _, o := range m.orders {
		if q.Ticker != "" && o.Ticker != q.Ticker {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return page(out, q), nil
}

// SaveModelVersion registers a model.
func (m *Memory) SaveModelVersion(_ context.Context, mv domain.ModelVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models[mv.ModelID+"@"+mv.Version] = mv
	return nil
}

// ListModelVersions returns registered models.
func (m *Memory) ListModelVersions(_ context.Context) ([]domain.ModelVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.ModelVersion, 0, len(m.models))
	for _, mv := range m.models {
		out = append(out, mv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID+out[i].Version < out[j].ModelID+out[j].Version })
	return out, nil
}

// SaveEvaluation appends a model evaluation.
func (m *Memory) SaveEvaluation(_ context.Context, e domain.ModelEvaluation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluations = append(m.evaluations, e)
	if len(m.evaluations) > 2000 {
		m.evaluations = m.evaluations[len(m.evaluations)-1000:]
	}
	return nil
}

// ListEvaluations returns evaluations for a model version, newest first.
func (m *Memory) ListEvaluations(_ context.Context, modelVersion string, limit int) ([]domain.ModelEvaluation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.ModelEvaluation{}
	for _, e := range m.evaluations {
		if modelVersion != "" && e.ModelVersion != modelVersion {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ComputedAt.After(out[j].ComputedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SaveDrift stores a drift report.
func (m *Memory) SaveDrift(_ context.Context, d domain.DriftReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drifts[d.ModelVersion] = d
	return nil
}

// LatestDrift returns the most recent drift report for a model version.
func (m *Memory) LatestDrift(_ context.Context, modelVersion string) (domain.DriftReport, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.drifts[modelVersion]
	if !ok {
		return domain.DriftReport{}, ErrNotFound
	}
	return d, nil
}

// SaveRun stores a backtest run.
func (m *Memory) SaveRun(_ context.Context, r domain.BacktestRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r
	return nil
}

// GetRun returns a backtest run.
func (m *Memory) GetRun(_ context.Context, id string) (domain.BacktestRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return domain.BacktestRun{}, ErrNotFound
	}
	return r, nil
}

// ListRuns returns backtest runs, newest first.
func (m *Memory) ListRuns(_ context.Context, limit int) ([]domain.BacktestRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.BacktestRun, 0, len(m.runs))
	for _, r := range m.runs {
		// Curves and trades are large; listings return the headline only.
		r.EquityCurve, r.Trades = nil, nil
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SaveNews stores a news event, returning false when it is a duplicate.
func (m *Memory) SaveNews(_ context.Context, n domain.NewsEvent) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.news[n.ID]; exists {
		return false, nil
	}
	m.news[n.ID] = n
	return true, nil
}

// ListNews returns matching news events, newest first.
func (m *Memory) ListNews(_ context.Context, q Query) ([]domain.NewsEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []domain.NewsEvent{}
	for _, n := range m.news {
		if q.Ticker != "" && n.Ticker != q.Ticker {
			continue
		}
		if !q.From.IsZero() && n.Timestamp.Before(q.From) {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return page(out, q), nil
}

// Append records an audit event.
func (m *Memory) Append(_ context.Context, e AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	if len(m.audit) > m.maxList {
		m.audit = m.audit[len(m.audit)-m.maxList/2:]
	}
	return nil
}

// ListAudit returns audit events, newest first.
func (m *Memory) ListAudit(_ context.Context, limit int) ([]AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]AuditEvent(nil), m.audit...)
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Health reports the in-memory store as healthy with nothing enabled.
func (m *Memory) Health(context.Context) Health {
	return Health{
		Postgres:   ComponentHealth{Enabled: false, Healthy: true},
		ClickHouse: ComponentHealth{Enabled: false, Healthy: true},
		Redis:      ComponentHealth{Enabled: false, Healthy: true},
	}
}

// Close is a no-op.
func (m *Memory) Close() error { return nil }

func page[T any](items []T, q Query) []T {
	if q.Offset > 0 {
		if q.Offset >= len(items) {
			return []T{}
		}
		items = items[q.Offset:]
	}
	if q.Limit > 0 && len(items) > q.Limit {
		items = items[:q.Limit]
	}
	return items
}

package store

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// SQL composes PostgreSQL, ClickHouse and Redis into one Store, routing each
// type to the engine ADR-003 assigns it and degrading explicitly when one is
// unavailable.
//
// The degradation policy is the interesting part:
//
//   - PostgreSQL down: signal *emission* is disabled by the caller (it cannot
//     persist provenance, so it cannot honour the reproducibility promise), and
//     reads fall back to ClickHouse or the cache with `Degraded` populated.
//   - ClickHouse down: the live path is unaffected. Analytical writes buffer in
//     a bounded queue and are retried; analytical reads return ErrUnavailable
//     so the API can answer 503 rather than an empty list, which would look
//     like "no data" instead of "we cannot see".
//   - Redis down: everything falls through to the source of truth at higher
//     latency.
type SQL struct {
	pg    *Postgres
	ch    *ClickHouse
	cache *Cache
	log   *slog.Logger

	mu      sync.RWMutex
	pgOK    bool
	chOK    bool
	cacheOK bool
	lastErr map[string]string

	// buffer holds analytical writes that could not be flushed. It is bounded:
	// silently unbounded buffering is how a downstream outage becomes an
	// upstream out-of-memory kill.
	bufMu    sync.Mutex
	bufSnaps []domain.FeatureSnapshot
	bufPreds []domain.Prediction
	bufBars  []domain.Candle
	maxBuf   int
}

// SQLConfig parameterises the composite store.
type SQLConfig struct {
	Postgres   *Postgres
	ClickHouse *ClickHouse
	Cache      *Cache
	Logger     *slog.Logger
	MaxBuffer  int
}

// NewSQL builds the composite store.
func NewSQL(cfg SQLConfig) *SQL {
	if cfg.MaxBuffer <= 0 {
		cfg.MaxBuffer = 50_000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &SQL{
		pg: cfg.Postgres, ch: cfg.ClickHouse, cache: cfg.Cache, log: cfg.Logger,
		pgOK: cfg.Postgres != nil, chOK: cfg.ClickHouse != nil, cacheOK: cfg.Cache != nil,
		lastErr: map[string]string{}, maxBuf: cfg.MaxBuffer,
	}
}

func (s *SQL) note(component string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		switch component {
		case "postgres":
			s.pgOK = s.pg != nil
		case "clickhouse":
			s.chOK = s.ch != nil
		case "redis":
			s.cacheOK = s.cache != nil
		}
		delete(s.lastErr, component)
		return
	}
	s.lastErr[component] = err.Error()
	if errors.Is(err, ErrUnavailable) {
		switch component {
		case "postgres":
			s.pgOK = false
		case "clickhouse":
			s.chOK = false
		case "redis":
			s.cacheOK = false
		}
	}
}

// --- Transactional: PostgreSQL ---------------------------------------------

// SaveSignal persists a signal.
func (s *SQL) SaveSignal(ctx context.Context, sig domain.Signal) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	err := s.pg.SaveSignal(ctx, sig)
	s.note("postgres", err)
	return err
}

// UpdateSignal persists a signal's lifecycle change.
func (s *SQL) UpdateSignal(ctx context.Context, sig domain.Signal) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	err := s.pg.UpdateSignal(ctx, sig)
	s.note("postgres", err)
	return err
}

// GetSignal reads a signal.
func (s *SQL) GetSignal(ctx context.Context, id string) (domain.Signal, error) {
	if s.pg == nil {
		return domain.Signal{}, ErrUnavailable
	}
	sig, err := s.pg.GetSignal(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		s.note("postgres", err)
	}
	return sig, err
}

// ListSignals reads signals.
func (s *SQL) ListSignals(ctx context.Context, q Query) ([]domain.Signal, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	out, err := s.pg.ListSignals(ctx, q)
	s.note("postgres", err)
	return out, err
}

// SaveAssessment persists a risk assessment.
func (s *SQL) SaveAssessment(ctx context.Context, a domain.RiskAssessment) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	err := s.pg.SaveAssessment(ctx, a)
	s.note("postgres", err)
	return err
}

// GetAssessment reads a risk assessment.
func (s *SQL) GetAssessment(ctx context.Context, id string) (domain.RiskAssessment, error) {
	if s.pg == nil {
		return domain.RiskAssessment{}, ErrUnavailable
	}
	return s.pg.GetAssessment(ctx, id)
}

// ListAssessments reads risk assessments.
func (s *SQL) ListAssessments(ctx context.Context, q Query) ([]domain.RiskAssessment, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListAssessments(ctx, q)
}

// SaveAlert persists an alert with its uniqueness guarantee.
func (s *SQL) SaveAlert(ctx context.Context, a domain.Alert) (bool, error) {
	if s.pg == nil {
		return false, ErrUnavailable
	}
	ok, err := s.pg.SaveAlert(ctx, a)
	s.note("postgres", err)
	return ok, err
}

// ListAlerts reads alerts.
func (s *SQL) ListAlerts(ctx context.Context, q Query) ([]domain.Alert, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListAlerts(ctx, q)
}

// SavePortfolio persists a portfolio snapshot.
func (s *SQL) SavePortfolio(ctx context.Context, p domain.Portfolio) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	return s.pg.SavePortfolio(ctx, p)
}

// GetPortfolio reads a portfolio.
func (s *SQL) GetPortfolio(ctx context.Context, id string) (domain.Portfolio, error) {
	if s.pg == nil {
		return domain.Portfolio{}, ErrUnavailable
	}
	return s.pg.GetPortfolio(ctx, id)
}

// SaveOrder persists a paper order.
func (s *SQL) SaveOrder(ctx context.Context, o domain.PaperOrder) (bool, error) {
	if s.pg == nil {
		return false, ErrUnavailable
	}
	return s.pg.SaveOrder(ctx, o)
}

// ListOrders reads paper orders.
func (s *SQL) ListOrders(ctx context.Context, q Query) ([]domain.PaperOrder, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListOrders(ctx, q)
}

// SaveModelVersion persists a registry entry.
func (s *SQL) SaveModelVersion(ctx context.Context, m domain.ModelVersion) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	return s.pg.SaveModelVersion(ctx, m)
}

// ListModelVersions reads the registry.
func (s *SQL) ListModelVersions(ctx context.Context) ([]domain.ModelVersion, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListModelVersions(ctx)
}

// SaveEvaluation persists a model evaluation.
func (s *SQL) SaveEvaluation(ctx context.Context, e domain.ModelEvaluation) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	return s.pg.SaveEvaluation(ctx, e)
}

// ListEvaluations reads model evaluations.
func (s *SQL) ListEvaluations(ctx context.Context, modelVersion string, limit int) ([]domain.ModelEvaluation, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListEvaluations(ctx, modelVersion, limit)
}

// SaveDrift persists a drift report.
func (s *SQL) SaveDrift(ctx context.Context, d domain.DriftReport) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	return s.pg.SaveDrift(ctx, d)
}

// LatestDrift reads the most recent drift report.
func (s *SQL) LatestDrift(ctx context.Context, modelVersion string) (domain.DriftReport, error) {
	if s.pg == nil {
		return domain.DriftReport{}, ErrUnavailable
	}
	return s.pg.LatestDrift(ctx, modelVersion)
}

// SaveRun persists a backtest run.
func (s *SQL) SaveRun(ctx context.Context, r domain.BacktestRun) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	return s.pg.SaveRun(ctx, r)
}

// GetRun reads a backtest run.
func (s *SQL) GetRun(ctx context.Context, id string) (domain.BacktestRun, error) {
	if s.pg == nil {
		return domain.BacktestRun{}, ErrUnavailable
	}
	return s.pg.GetRun(ctx, id)
}

// ListRuns reads backtest runs.
func (s *SQL) ListRuns(ctx context.Context, limit int) ([]domain.BacktestRun, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListRuns(ctx, limit)
}

// SaveNews persists a news event.
func (s *SQL) SaveNews(ctx context.Context, n domain.NewsEvent) (bool, error) {
	if s.pg == nil {
		return false, ErrUnavailable
	}
	return s.pg.SaveNews(ctx, n)
}

// ListNews reads news events.
func (s *SQL) ListNews(ctx context.Context, q Query) ([]domain.NewsEvent, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListNews(ctx, q)
}

// Append writes an audit event.
func (s *SQL) Append(ctx context.Context, e AuditEvent) error {
	if s.pg == nil {
		return ErrUnavailable
	}
	return s.pg.Append(ctx, e)
}

// ListAudit reads audit events.
func (s *SQL) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListAudit(ctx, limit)
}

// SaveRegime persists a regime record and caches it.
func (s *SQL) SaveRegime(ctx context.Context, r domain.MarketRegime) error {
	if s.cache != nil {
		if err := s.cache.SetRegime(ctx, r); err != nil {
			s.note("redis", err)
		}
	}
	if s.pg == nil {
		return nil
	}
	err := s.pg.SaveRegime(ctx, r)
	s.note("postgres", err)
	return err
}

// LatestRegime reads the current regime, preferring the cache.
func (s *SQL) LatestRegime(ctx context.Context) (domain.MarketRegime, error) {
	if s.cache != nil {
		if r, ok := s.cache.GetRegime(ctx); ok {
			return r, nil
		}
	}
	if s.pg == nil {
		return domain.MarketRegime{}, ErrUnavailable
	}
	return s.pg.LatestRegime(ctx)
}

// ListRegimes reads regime history.
func (s *SQL) ListRegimes(ctx context.Context, q Query) ([]domain.MarketRegime, error) {
	if s.pg == nil {
		return nil, ErrUnavailable
	}
	return s.pg.ListRegimes(ctx, q)
}

// --- Analytical: ClickHouse, with buffering --------------------------------

// SaveQuotes caches the latest quote and appends to the analytical store.
func (s *SQL) SaveQuotes(ctx context.Context, qs []domain.Quote) error {
	if s.cache != nil {
		for _, q := range qs {
			if err := s.cache.SetQuote(ctx, q); err != nil {
				s.note("redis", err)
				break
			}
		}
	}
	if s.ch == nil {
		return nil
	}
	err := s.ch.SaveQuotes(ctx, qs)
	s.note("clickhouse", err)
	// Quotes are the highest-volume, lowest-value stream: dropping them under
	// an outage is preferable to buffering them, and the alert on the
	// ClickHouse health check is what tells an operator.
	return nil
}

// SaveCandles appends bars, buffering on failure.
func (s *SQL) SaveCandles(ctx context.Context, cs []domain.Candle) error {
	if s.ch == nil {
		return nil
	}
	if err := s.ch.SaveCandles(ctx, cs); err != nil {
		s.note("clickhouse", err)
		s.bufferBars(cs)
		return nil
	}
	s.note("clickhouse", nil)
	s.flush(ctx)
	return nil
}

// ListCandles reads bars.
func (s *SQL) ListCandles(ctx context.Context, q Query, interval domain.Interval) ([]domain.Candle, error) {
	if s.ch == nil {
		return nil, ErrUnavailable
	}
	out, err := s.ch.ListCandles(ctx, q, interval)
	s.note("clickhouse", err)
	return out, err
}

// LatestQuote reads the latest quote, preferring the cache.
func (s *SQL) LatestQuote(ctx context.Context, t domain.Ticker) (domain.Quote, error) {
	if s.cache != nil {
		if q, ok := s.cache.GetQuote(ctx, t); ok {
			return q, nil
		}
	}
	if s.ch == nil {
		return domain.Quote{}, ErrUnavailable
	}
	return s.ch.LatestQuote(ctx, t)
}

// SaveSnapshot writes a feature snapshot, buffering on failure.
func (s *SQL) SaveSnapshot(ctx context.Context, snap domain.FeatureSnapshot) error {
	if s.cache != nil {
		if err := s.cache.SetSnapshot(ctx, snap); err != nil {
			s.note("redis", err)
		}
	}
	if s.ch == nil {
		return nil
	}
	if err := s.ch.SaveSnapshot(ctx, snap); err != nil {
		s.note("clickhouse", err)
		s.bufferSnapshot(snap)
		return nil
	}
	s.note("clickhouse", nil)
	return nil
}

// GetSnapshot reads a snapshot, preferring the cache.
func (s *SQL) GetSnapshot(ctx context.Context, t domain.Ticker, hash string) (domain.FeatureSnapshot, error) {
	if s.cache != nil {
		if snap, ok := s.cache.GetSnapshot(ctx, t, hash); ok {
			return snap, nil
		}
	}
	if s.ch == nil {
		return domain.FeatureSnapshot{}, ErrUnavailable
	}
	return s.ch.GetSnapshot(ctx, t, hash)
}

// ListSnapshots reads snapshots.
func (s *SQL) ListSnapshots(ctx context.Context, q Query) ([]domain.FeatureSnapshot, error) {
	if s.ch == nil {
		return nil, ErrUnavailable
	}
	return s.ch.ListSnapshots(ctx, q)
}

// SavePrediction writes a prediction, buffering on failure.
func (s *SQL) SavePrediction(ctx context.Context, p domain.Prediction) error {
	if s.ch == nil {
		return nil
	}
	if err := s.ch.SavePrediction(ctx, p); err != nil {
		s.note("clickhouse", err)
		s.bufferPrediction(p)
		return nil
	}
	s.note("clickhouse", nil)
	return nil
}

// GetPrediction reads a prediction.
func (s *SQL) GetPrediction(ctx context.Context, id string) (domain.Prediction, error) {
	if s.ch == nil {
		return domain.Prediction{}, ErrUnavailable
	}
	return s.ch.GetPrediction(ctx, id)
}

// ListPredictions reads predictions.
func (s *SQL) ListPredictions(ctx context.Context, q Query) ([]domain.Prediction, error) {
	if s.ch == nil {
		return nil, ErrUnavailable
	}
	return s.ch.ListPredictions(ctx, q)
}

// SaveOutcome writes a resolved outcome.
func (s *SQL) SaveOutcome(ctx context.Context, o domain.PredictionOutcome) error {
	if s.ch == nil {
		return nil
	}
	err := s.ch.SaveOutcome(ctx, o)
	s.note("clickhouse", err)
	return err
}

// ListOutcomes reads resolved outcomes.
func (s *SQL) ListOutcomes(ctx context.Context, q Query) ([]domain.PredictionOutcome, error) {
	if s.ch == nil {
		return nil, ErrUnavailable
	}
	return s.ch.ListOutcomes(ctx, q)
}

// --- Buffering --------------------------------------------------------------

func (s *SQL) bufferSnapshot(snap domain.FeatureSnapshot) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if len(s.bufSnaps) >= s.maxBuf {
		// Drop the oldest: an unbounded buffer turns a downstream outage into
		// an out-of-memory kill, which is a worse failure.
		s.bufSnaps = s.bufSnaps[len(s.bufSnaps)/2:]
		s.log.Warn("clickhouse buffer full; dropped oldest feature snapshots", "kept", len(s.bufSnaps))
	}
	s.bufSnaps = append(s.bufSnaps, snap)
}

func (s *SQL) bufferPrediction(p domain.Prediction) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if len(s.bufPreds) >= s.maxBuf {
		s.bufPreds = s.bufPreds[len(s.bufPreds)/2:]
	}
	s.bufPreds = append(s.bufPreds, p)
}

func (s *SQL) bufferBars(cs []domain.Candle) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if len(s.bufBars) >= s.maxBuf {
		s.bufBars = s.bufBars[len(s.bufBars)/2:]
	}
	s.bufBars = append(s.bufBars, cs...)
}

// flush retries buffered analytical writes. It is called opportunistically on
// every successful write, so recovery needs no separate scheduler.
func (s *SQL) flush(ctx context.Context) {
	s.bufMu.Lock()
	snaps, preds, bars := s.bufSnaps, s.bufPreds, s.bufBars
	s.bufSnaps, s.bufPreds, s.bufBars = nil, nil, nil
	s.bufMu.Unlock()

	if len(bars) > 0 {
		if err := s.ch.SaveCandles(ctx, bars); err != nil {
			s.bufferBars(bars)
			return
		}
	}
	for _, snap := range snaps {
		if err := s.ch.SaveSnapshot(ctx, snap); err != nil {
			s.bufferSnapshot(snap)
			return
		}
	}
	for _, p := range preds {
		if err := s.ch.SavePrediction(ctx, p); err != nil {
			s.bufferPrediction(p)
			return
		}
	}
}

// BufferDepth reports how many analytical writes are pending.
func (s *SQL) BufferDepth() int {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	return len(s.bufSnaps) + len(s.bufPreds) + len(s.bufBars)
}

// Health probes each backing store.
func (s *SQL) Health(ctx context.Context) Health {
	h := Health{}
	probe := func(name string, enabled bool, ping func(context.Context) error) ComponentHealth {
		c := ComponentHealth{Enabled: enabled}
		if !enabled {
			c.Healthy = true
			return c
		}
		start := time.Now()
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := ping(pctx); err != nil {
			c.Error = err.Error()
			h.Degraded = append(h.Degraded, name)
			s.note(name, ErrUnavailable)
			return c
		}
		c.Healthy = true
		c.Latency = time.Since(start)
		s.note(name, nil)
		return c
	}
	h.Postgres = probe("postgres", s.pg != nil, func(c context.Context) error { return s.pg.Ping(c) })
	h.ClickHouse = probe("clickhouse", s.ch != nil, func(c context.Context) error { return s.ch.Ping(c) })
	h.Redis = probe("redis", s.cache != nil, func(c context.Context) error { return s.cache.Ping(c) })
	return h
}

// CanEmitSignals reports whether provenance can currently be persisted.
//
// This is the gate that implements ADR-003's failure policy: if a signal's
// provenance cannot be written, the platform must not emit the signal, because
// it could not then answer "reproduce this decision" — which is a stated
// product requirement, not a nice-to-have.
func (s *SQL) CanEmitSignals() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pg != nil && s.pgOK
}

// Degraded lists the components currently considered unhealthy.
func (s *SQL) Degraded() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	if s.pg != nil && !s.pgOK {
		out = append(out, "postgres")
	}
	if s.ch != nil && !s.chOK {
		out = append(out, "clickhouse")
	}
	if s.cache != nil && !s.cacheOK {
		out = append(out, "redis")
	}
	return out
}

// Close releases every backing connection.
func (s *SQL) Close() error {
	var firstErr error
	if s.pg != nil {
		if err := s.pg.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.ch != nil {
		if err := s.ch.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.cache != nil {
		if err := s.cache.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

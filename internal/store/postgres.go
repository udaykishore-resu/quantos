package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Postgres is the transactional store: everything whose loss would corrupt a
// decision's provenance (ADR-003).
//
// Two design choices are worth stating. First, the large nested structures
// (risk checks, invalidation conditions, equity curves) are stored as JSONB
// rather than normalised into child tables: they are written once, read whole,
// and never queried field-by-field, so normalising them would buy nothing and
// cost a join on every read. Second, the idempotency guarantees are enforced by
// unique indexes rather than by application logic, because an index survives a
// cache wipe, a redeploy and a careless refactor.
type Postgres struct {
	db      *sql.DB
	timeout time.Duration
}

// PostgresConfig parameterises the connection.
type PostgresConfig struct {
	DSN          string
	MaxOpenConns int
	MaxIdleConns int
	ConnTimeout  time.Duration
	QueryTimeout time.Duration
}

// OpenPostgres connects and verifies the connection.
func OpenPostgres(ctx context.Context, cfg PostgresConfig) (*Postgres, error) {
	if cfg.DSN == "" {
		return nil, errors.New("store: postgres DSN is required")
	}
	db, err := sql.Open("postgres", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: open postgres: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	db.SetConnMaxLifetime(30 * time.Minute)

	if cfg.ConnTimeout <= 0 {
		cfg.ConnTimeout = 5 * time.Second
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 5 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, cfg.ConnTimeout)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: postgres ping: %v", ErrUnavailable, err)
	}
	return &Postgres{db: db, timeout: cfg.QueryTimeout}, nil
}

func (p *Postgres) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.timeout)
}

// SaveSignal inserts a signal. The primary key makes replay idempotent.
func (p *Postgres) SaveSignal(ctx context.Context, s domain.Signal) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	inv, err := json.Marshal(s.Invalidations)
	if err != nil {
		return err
	}
	ev, _ := json.Marshal(s.Evidence)
	cev, _ := json.Marshal(s.CounterEvidence)
	rules, _ := json.Marshal(s.RulesFired)

	_, err = p.db.ExecContext(ctx, `
INSERT INTO signals (
  id, ticker, created_at, expires_at, side, status, strength, confidence, horizon,
  entry_reference, stop_reference, target_reference, suggested_weight,
  invalidations, strategy_id, rules_fired, evidence, counter_evidence,
  prediction_id, risk_assessment_id, feature_hash, regime, model_version,
  config_hash, correlation_id, explanation, explanation_source
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
ON CONFLICT (id) DO NOTHING`,
		s.ID, string(s.Ticker), s.CreatedAt, s.ExpiresAt, string(s.Side), string(s.Status),
		s.Strength, s.Confidence, string(s.Horizon),
		s.EntryReference, s.StopReference, s.TargetReference, s.SuggestedWeight,
		inv, s.StrategyID, rules, ev, cev,
		nullable(s.PredictionID), nullable(s.RiskAssessmentID), s.FeatureHash,
		string(s.Regime), s.ModelVersion, s.ConfigHash, nullable(s.CorrelationID),
		nullable(s.Explanation), nullable(s.ExplanationSource))
	if err != nil {
		return fmt.Errorf("store: save signal %s: %w", s.ID, err)
	}
	return nil
}

// UpdateSignal writes the lifecycle fields.
func (p *Postgres) UpdateSignal(ctx context.Context, s domain.Signal) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `
UPDATE signals SET status=$2, invalidated_at=$3, invalidation_kind=$4,
  invalidation_note=$5, explanation=$6, explanation_source=$7
WHERE id=$1`,
		s.ID, string(s.Status), s.InvalidatedAt, nullable(string(s.InvalidationKind)),
		nullable(s.InvalidationNote), nullable(s.Explanation), nullable(s.ExplanationSource))
	if err != nil {
		return fmt.Errorf("store: update signal %s: %w", s.ID, err)
	}
	return nil
}

const signalColumns = `id, ticker, created_at, expires_at, side, status, strength, confidence,
 horizon, entry_reference, stop_reference, target_reference, suggested_weight,
 invalidations, strategy_id, rules_fired, evidence, counter_evidence,
 COALESCE(prediction_id,''), COALESCE(risk_assessment_id,''), feature_hash, regime,
 model_version, config_hash, COALESCE(correlation_id,''), invalidated_at,
 COALESCE(invalidation_kind,''), COALESCE(invalidation_note,''),
 COALESCE(explanation,''), COALESCE(explanation_source,'')`

func scanSignal(rows interface{ Scan(...any) error }) (domain.Signal, error) {
	var s domain.Signal
	var inv, ev, cev, rules []byte
	var side, status, horizon, regime, invKind string
	err := rows.Scan(&s.ID, &s.Ticker, &s.CreatedAt, &s.ExpiresAt, &side, &status,
		&s.Strength, &s.Confidence, &horizon, &s.EntryReference, &s.StopReference,
		&s.TargetReference, &s.SuggestedWeight, &inv, &s.StrategyID, &rules, &ev, &cev,
		&s.PredictionID, &s.RiskAssessmentID, &s.FeatureHash, &regime,
		&s.ModelVersion, &s.ConfigHash, &s.CorrelationID, &s.InvalidatedAt,
		&invKind, &s.InvalidationNote, &s.Explanation, &s.ExplanationSource)
	if err != nil {
		return s, err
	}
	s.Side, s.Status = domain.Side(side), domain.SignalStatus(status)
	s.Horizon, s.Regime = domain.Horizon(horizon), domain.Regime(regime)
	s.InvalidationKind = domain.InvalidationKind(invKind)
	_ = json.Unmarshal(inv, &s.Invalidations)
	_ = json.Unmarshal(ev, &s.Evidence)
	_ = json.Unmarshal(cev, &s.CounterEvidence)
	_ = json.Unmarshal(rules, &s.RulesFired)
	s.Disclaimer = domain.Disclaimer
	return s, nil
}

// GetSignal reads one signal.
func (p *Postgres) GetSignal(ctx context.Context, id string) (domain.Signal, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	row := p.db.QueryRowContext(ctx, `SELECT `+signalColumns+` FROM signals WHERE id=$1`, id)
	s, err := scanSignal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Signal{}, ErrNotFound
	}
	return s, err
}

// ListSignals reads matching signals.
func (p *Postgres) ListSignals(ctx context.Context, q Query) ([]domain.Signal, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT `+signalColumns+` FROM signals
WHERE ($1='' OR ticker=$1) AND ($2='' OR status=$2) AND ($3='' OR strategy_id=$3)
  AND ($4::timestamptz IS NULL OR created_at >= $4)
  AND ($5::timestamptz IS NULL OR created_at <= $5)
ORDER BY created_at DESC, id DESC
LIMIT $6 OFFSET $7`,
		string(q.Ticker), q.Status, q.Strategy, nullTime(q.From), nullTime(q.To),
		limitOr(q.Limit, 200), q.Offset)
	if err != nil {
		return nil, fmt.Errorf("store: list signals: %w", err)
	}
	defer rows.Close()
	var out []domain.Signal
	for rows.Next() {
		s, err := scanSignal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SaveAssessment inserts a risk assessment with its per-check detail.
func (p *Postgres) SaveAssessment(ctx context.Context, a domain.RiskAssessment) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	checks, _ := json.Marshal(a.Checks)
	blockers, _ := json.Marshal(a.Blockers)
	warnings, _ := json.Marshal(a.Warnings)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO risk_assessments (id, ticker, created_at, decision, level, score, checks,
  blockers, warnings, config_hash, regime, feature_hash, prediction_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (id) DO NOTHING`,
		a.ID, string(a.Ticker), a.CreatedAt, string(a.Decision), string(a.Level), a.Score,
		checks, blockers, warnings, a.ConfigHash, string(a.Regime), a.FeatureHash,
		nullable(a.PredictionID))
	if err != nil {
		return fmt.Errorf("store: save assessment %s: %w", a.ID, err)
	}
	return nil
}

// GetAssessment reads one assessment.
func (p *Postgres) GetAssessment(ctx context.Context, id string) (domain.RiskAssessment, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	var a domain.RiskAssessment
	var checks, blockers, warnings []byte
	var decision, level, regime string
	err := p.db.QueryRowContext(ctx, `
SELECT id, ticker, created_at, decision, level, score, checks, blockers, warnings,
  config_hash, regime, feature_hash, COALESCE(prediction_id,'')
FROM risk_assessments WHERE id=$1`, id).
		Scan(&a.ID, &a.Ticker, &a.CreatedAt, &decision, &level, &a.Score,
			&checks, &blockers, &warnings, &a.ConfigHash, &regime, &a.FeatureHash, &a.PredictionID)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.Decision, a.Level, a.Regime = domain.RiskDecision(decision), domain.RiskLevel(level), domain.Regime(regime)
	_ = json.Unmarshal(checks, &a.Checks)
	_ = json.Unmarshal(blockers, &a.Blockers)
	_ = json.Unmarshal(warnings, &a.Warnings)
	return a, nil
}

// ListAssessments reads matching assessments.
func (p *Postgres) ListAssessments(ctx context.Context, q Query) ([]domain.RiskAssessment, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, ticker, created_at, decision, level, score, checks, blockers, warnings,
  config_hash, regime, feature_hash, COALESCE(prediction_id,'')
FROM risk_assessments
WHERE ($1='' OR ticker=$1) AND ($2::timestamptz IS NULL OR created_at >= $2)
ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
		string(q.Ticker), nullTime(q.From), limitOr(q.Limit, 200), q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RiskAssessment
	for rows.Next() {
		var a domain.RiskAssessment
		var checks, blockers, warnings []byte
		var decision, level, regime string
		if err := rows.Scan(&a.ID, &a.Ticker, &a.CreatedAt, &decision, &level, &a.Score,
			&checks, &blockers, &warnings, &a.ConfigHash, &regime, &a.FeatureHash, &a.PredictionID); err != nil {
			return nil, err
		}
		a.Decision, a.Level, a.Regime = domain.RiskDecision(decision), domain.RiskLevel(level), domain.Regime(regime)
		_ = json.Unmarshal(checks, &a.Checks)
		_ = json.Unmarshal(blockers, &a.Blockers)
		_ = json.Unmarshal(warnings, &a.Warnings)
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveAlert inserts an alert. The unique index on dedup_key is what makes
// duplicate alerts impossible even after a Redis wipe (ADR-010).
func (p *Postgres) SaveAlert(ctx context.Context, a domain.Alert) (bool, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	before, _ := json.Marshal(a.Before)
	after, _ := json.Marshal(a.After)
	res, err := p.db.ExecContext(ctx, `
INSERT INTO alerts (id, dedup_key, type, severity, ticker, created_at, title, message,
  before_values, after_values, regime, risk_level, status, signal_id, prediction_id, correlation_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (dedup_key) DO NOTHING`,
		a.ID, a.DedupKey, string(a.Type), string(a.Severity), string(a.Ticker), a.CreatedAt,
		a.Title, a.Message, before, after, string(a.Regime), string(a.RiskLevel), a.Status,
		nullable(a.SignalID), nullable(a.PredictionID), nullable(a.CorrelationID))
	if err != nil {
		return false, fmt.Errorf("store: save alert: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListAlerts reads matching alerts.
func (p *Postgres) ListAlerts(ctx context.Context, q Query) ([]domain.Alert, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, dedup_key, type, severity, ticker, created_at, title, message,
  before_values, after_values, regime, risk_level, status,
  COALESCE(signal_id,''), COALESCE(prediction_id,''), COALESCE(correlation_id,'')
FROM alerts
WHERE ($1='' OR ticker=$1) AND ($2::timestamptz IS NULL OR created_at >= $2)
ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
		string(q.Ticker), nullTime(q.From), limitOr(q.Limit, 200), q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Alert
	for rows.Next() {
		var a domain.Alert
		var before, after []byte
		var typ, sev, regime, level string
		if err := rows.Scan(&a.ID, &a.DedupKey, &typ, &sev, &a.Ticker, &a.CreatedAt,
			&a.Title, &a.Message, &before, &after, &regime, &level, &a.Status,
			&a.SignalID, &a.PredictionID, &a.CorrelationID); err != nil {
			return nil, err
		}
		a.Type, a.Severity = domain.AlertType(typ), domain.AlertSeverity(sev)
		a.Regime, a.RiskLevel = domain.Regime(regime), domain.RiskLevel(level)
		_ = json.Unmarshal(before, &a.Before)
		_ = json.Unmarshal(after, &a.After)
		a.Disclaimer = domain.Disclaimer
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveOrder inserts a paper order. The unique index on idempotency_key is the
// durable no-duplicate-orders guarantee.
func (p *Postgres) SaveOrder(ctx context.Context, o domain.PaperOrder) (bool, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	fills, _ := json.Marshal(o.Fills)
	res, err := p.db.ExecContext(ctx, `
INSERT INTO paper_orders (id, portfolio_id, idempotency_key, ticker, side, type, quantity,
  limit_price, stop_price, time_in_force, status, filled_qty, avg_fill_price, commission,
  slippage_bps, reject_reason, signal_id, created_at, updated_at, fills)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
ON CONFLICT (idempotency_key) DO UPDATE SET
  status=EXCLUDED.status, filled_qty=EXCLUDED.filled_qty,
  avg_fill_price=EXCLUDED.avg_fill_price, commission=EXCLUDED.commission,
  updated_at=EXCLUDED.updated_at, fills=EXCLUDED.fills
RETURNING (xmax = 0) AS inserted`,
		o.ID, o.PortfolioID, o.IdempotencyKey, string(o.Ticker), string(o.Side), string(o.Type),
		o.Quantity, o.LimitPrice, o.StopPrice, o.TimeInForce, string(o.Status), o.FilledQty,
		o.AvgFillPrice, o.Commission, o.SlippageBps, nullable(o.RejectReason),
		nullable(o.SignalID), o.CreatedAt, o.UpdatedAt, fills)
	if err != nil {
		return false, fmt.Errorf("store: save order: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListOrders reads matching orders.
func (p *Postgres) ListOrders(ctx context.Context, q Query) ([]domain.PaperOrder, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, portfolio_id, idempotency_key, ticker, side, type, quantity, limit_price,
  stop_price, time_in_force, status, filled_qty, avg_fill_price, commission, slippage_bps,
  COALESCE(reject_reason,''), COALESCE(signal_id,''), created_at, updated_at, fills
FROM paper_orders
WHERE ($1='' OR ticker=$1)
ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		string(q.Ticker), limitOr(q.Limit, 200), q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaperOrder
	for rows.Next() {
		var o domain.PaperOrder
		var fills []byte
		var side, typ, status string
		if err := rows.Scan(&o.ID, &o.PortfolioID, &o.IdempotencyKey, &o.Ticker, &side, &typ,
			&o.Quantity, &o.LimitPrice, &o.StopPrice, &o.TimeInForce, &status, &o.FilledQty,
			&o.AvgFillPrice, &o.Commission, &o.SlippageBps, &o.RejectReason, &o.SignalID,
			&o.CreatedAt, &o.UpdatedAt, &fills); err != nil {
			return nil, err
		}
		o.Side, o.Type, o.Status = domain.OrderSide(side), domain.OrderType(typ), domain.OrderStatus(status)
		_ = json.Unmarshal(fills, &o.Fills)
		out = append(out, o)
	}
	return out, rows.Err()
}

// SavePortfolio upserts a portfolio snapshot.
func (p *Postgres) SavePortfolio(ctx context.Context, pf domain.Portfolio) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	positions, _ := json.Marshal(pf.Positions)
	sectors, _ := json.Marshal(pf.SectorExposure)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO portfolios (id, name, owner, currency, as_of, starting_cash, cash, equity,
  market_value, realized_pnl, unrealized_pnl, total_pnl, return_pct, gross_exposure,
  net_exposure, leverage, peak_equity, drawdown, max_drawdown, sector_exposure, positions)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
ON CONFLICT (id) DO UPDATE SET
  as_of=EXCLUDED.as_of, cash=EXCLUDED.cash, equity=EXCLUDED.equity,
  market_value=EXCLUDED.market_value, realized_pnl=EXCLUDED.realized_pnl,
  unrealized_pnl=EXCLUDED.unrealized_pnl, total_pnl=EXCLUDED.total_pnl,
  return_pct=EXCLUDED.return_pct, gross_exposure=EXCLUDED.gross_exposure,
  net_exposure=EXCLUDED.net_exposure, leverage=EXCLUDED.leverage,
  peak_equity=EXCLUDED.peak_equity, drawdown=EXCLUDED.drawdown,
  max_drawdown=EXCLUDED.max_drawdown, sector_exposure=EXCLUDED.sector_exposure,
  positions=EXCLUDED.positions`,
		pf.ID, pf.Name, pf.Owner, pf.Currency, pf.AsOf, pf.StartingCash, pf.Cash, pf.Equity,
		pf.MarketValue, pf.RealizedPnL, pf.UnrealizedPnL, pf.TotalPnL, pf.ReturnPct,
		pf.GrossExposure, pf.NetExposure, pf.Leverage, pf.PeakEquity, pf.Drawdown,
		pf.MaxDrawdown, sectors, positions)
	return err
}

// GetPortfolio reads a portfolio.
func (p *Postgres) GetPortfolio(ctx context.Context, id string) (domain.Portfolio, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	var pf domain.Portfolio
	var positions, sectors []byte
	err := p.db.QueryRowContext(ctx, `
SELECT id, name, owner, currency, as_of, starting_cash, cash, equity, market_value,
  realized_pnl, unrealized_pnl, total_pnl, return_pct, gross_exposure, net_exposure,
  leverage, peak_equity, drawdown, max_drawdown, sector_exposure, positions
FROM portfolios WHERE id=$1`, id).
		Scan(&pf.ID, &pf.Name, &pf.Owner, &pf.Currency, &pf.AsOf, &pf.StartingCash, &pf.Cash,
			&pf.Equity, &pf.MarketValue, &pf.RealizedPnL, &pf.UnrealizedPnL, &pf.TotalPnL,
			&pf.ReturnPct, &pf.GrossExposure, &pf.NetExposure, &pf.Leverage, &pf.PeakEquity,
			&pf.Drawdown, &pf.MaxDrawdown, &sectors, &positions)
	if errors.Is(err, sql.ErrNoRows) {
		return pf, ErrNotFound
	}
	if err != nil {
		return pf, err
	}
	_ = json.Unmarshal(positions, &pf.Positions)
	_ = json.Unmarshal(sectors, &pf.SectorExposure)
	pf.Paper = true
	return pf, nil
}

// SaveModelVersion upserts a registry entry.
func (p *Postgres) SaveModelVersion(ctx context.Context, m domain.ModelVersion) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	features, _ := json.Marshal(m.Features)
	hyper, _ := json.Marshal(m.Hyperparameters)
	oos, _ := json.Marshal(m.OutOfSample)
	byRegime, _ := json.Marshal(m.ByRegime)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO model_versions (id, model_id, version, family, stage, artifact_uri, artifact_sha,
  features, horizon, flat_band_bps, train_start, train_end, valid_start, valid_end,
  test_start, test_end, trained_at, registered_at, promoted_at, training_commit,
  hyperparameters, canary_fraction, out_of_sample, by_regime, notes)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
ON CONFLICT (model_id, version) DO UPDATE SET
  stage=EXCLUDED.stage, promoted_at=EXCLUDED.promoted_at,
  canary_fraction=EXCLUDED.canary_fraction, out_of_sample=EXCLUDED.out_of_sample,
  by_regime=EXCLUDED.by_regime, notes=EXCLUDED.notes`,
		m.ID, m.ModelID, m.Version, string(m.Family), string(m.Stage), m.ArtifactURI, m.ArtifactSHA,
		features, string(m.Horizon), m.FlatBandBps, m.TrainStart, m.TrainEnd, m.ValidStart,
		m.ValidEnd, m.TestStart, m.TestEnd, m.TrainedAt, m.RegisteredAt, m.PromotedAt,
		nullable(m.TrainingCommit), hyper, m.CanaryFraction, oos, byRegime, nullable(m.Notes))
	return err
}

// ListModelVersions reads the registry.
func (p *Postgres) ListModelVersions(ctx context.Context) ([]domain.ModelVersion, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, model_id, version, family, stage, artifact_uri, artifact_sha, features,
  horizon, flat_band_bps, trained_at, registered_at, out_of_sample, by_regime,
  COALESCE(notes,'')
FROM model_versions ORDER BY model_id, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModelVersion
	for rows.Next() {
		var m domain.ModelVersion
		var features, oos, byRegime []byte
		var family, stage, horizon string
		if err := rows.Scan(&m.ID, &m.ModelID, &m.Version, &family, &stage, &m.ArtifactURI,
			&m.ArtifactSHA, &features, &horizon, &m.FlatBandBps, &m.TrainedAt,
			&m.RegisteredAt, &oos, &byRegime, &m.Notes); err != nil {
			return nil, err
		}
		m.Family, m.Stage, m.Horizon = domain.ModelFamily(family), domain.ModelStage(stage), domain.Horizon(horizon)
		_ = json.Unmarshal(features, &m.Features)
		_ = json.Unmarshal(oos, &m.OutOfSample)
		_ = json.Unmarshal(byRegime, &m.ByRegime)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveEvaluation appends a model evaluation.
func (p *Postgres) SaveEvaluation(ctx context.Context, e domain.ModelEvaluation) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	payload, _ := json.Marshal(e)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO model_evaluations (model_version, window_name, regime, horizon, samples,
  accuracy, base_rate, brier_score, log_loss, computed_at, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		e.ModelVersion, e.Window, string(e.Regime), string(e.Horizon), e.Samples,
		e.Accuracy, e.BaseRate, e.BrierScore, e.LogLoss, e.ComputedAt, payload)
	return err
}

// ListEvaluations reads evaluations for a model version.
func (p *Postgres) ListEvaluations(ctx context.Context, modelVersion string, limit int) ([]domain.ModelEvaluation, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT payload FROM model_evaluations
WHERE ($1='' OR model_version=$1) ORDER BY computed_at DESC LIMIT $2`,
		modelVersion, limitOr(limit, 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModelEvaluation
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var e domain.ModelEvaluation
		if err := json.Unmarshal(payload, &e); err == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// SaveDrift stores a drift report.
func (p *Postgres) SaveDrift(ctx context.Context, d domain.DriftReport) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	payload, _ := json.Marshal(d)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO model_drift (model_version, computed_at, severity, max_feature_psi,
  prediction_drift, accuracy_delta, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		d.ModelVersion, d.ComputedAt, string(d.Severity), d.MaxFeaturePSI,
		d.PredictionDrift, d.AccuracyDelta, payload)
	return err
}

// LatestDrift reads the most recent drift report.
func (p *Postgres) LatestDrift(ctx context.Context, modelVersion string) (domain.DriftReport, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	var payload []byte
	err := p.db.QueryRowContext(ctx, `
SELECT payload FROM model_drift WHERE ($1='' OR model_version=$1)
ORDER BY computed_at DESC LIMIT 1`, modelVersion).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DriftReport{}, ErrNotFound
	}
	if err != nil {
		return domain.DriftReport{}, err
	}
	var d domain.DriftReport
	return d, json.Unmarshal(payload, &d)
}

// SaveRun stores a backtest run.
func (p *Postgres) SaveRun(ctx context.Context, r domain.BacktestRun) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	payload, _ := json.Marshal(r)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO backtest_runs (id, name, status, strategy_id, start_at, end_at, seed,
  config_hash, model_version, code_version, created_at, completed_at, result_hash, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, completed_at=EXCLUDED.completed_at,
  result_hash=EXCLUDED.result_hash, payload=EXCLUDED.payload`,
		r.ID, r.Name, string(r.Status), r.StrategyID, r.Start, r.End, r.Seed,
		r.ConfigHash, r.ModelVersion, r.CodeVersion, r.CreatedAt, nullTimeValue(r.CompletedAt),
		r.ResultHash, payload)
	return err
}

// GetRun reads a backtest run.
func (p *Postgres) GetRun(ctx context.Context, id string) (domain.BacktestRun, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	var payload []byte
	err := p.db.QueryRowContext(ctx, `SELECT payload FROM backtest_runs WHERE id=$1`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.BacktestRun{}, ErrNotFound
	}
	if err != nil {
		return domain.BacktestRun{}, err
	}
	var r domain.BacktestRun
	return r, json.Unmarshal(payload, &r)
}

// ListRuns reads backtest runs without their curves.
func (p *Postgres) ListRuns(ctx context.Context, limit int) ([]domain.BacktestRun, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, name, status, strategy_id, start_at, end_at, seed, config_hash,
  model_version, code_version, created_at, result_hash
FROM backtest_runs ORDER BY created_at DESC LIMIT $1`, limitOr(limit, 50))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.BacktestRun
	for rows.Next() {
		var r domain.BacktestRun
		var status string
		if err := rows.Scan(&r.ID, &r.Name, &status, &r.StrategyID, &r.Start, &r.End,
			&r.Seed, &r.ConfigHash, &r.ModelVersion, &r.CodeVersion, &r.CreatedAt, &r.ResultHash); err != nil {
			return nil, err
		}
		r.Status = domain.BacktestStatus(status)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveNews inserts a news event.
func (p *Postgres) SaveNews(ctx context.Context, n domain.NewsEvent) (bool, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	matched, _ := json.Marshal(n.Matched)
	res, err := p.db.ExecContext(ctx, `
INSERT INTO news_events (id, ticker, timestamp, ingested_at, source, headline, url,
  category, sentiment, sentiment_score, materiality, materiality_score, confidence,
  affected_sector, expected_horizon, stage, matched_terms, llm_model, llm_agreed, cluster_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
ON CONFLICT (id) DO NOTHING`,
		n.ID, string(n.Ticker), n.Timestamp, n.IngestedAt, n.Source, n.Headline, nullable(n.URL),
		string(n.Category), string(n.Sentiment), n.SentimentScore, string(n.Materiality),
		n.MaterialityScore, n.Confidence, nullable(n.AffectedSector), string(n.ExpectedHorizon),
		n.Stage, matched, nullable(n.LLMModel), n.LLMAgreed, n.ClusterID)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// ListNews reads matching news events.
func (p *Postgres) ListNews(ctx context.Context, q Query) ([]domain.NewsEvent, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, ticker, timestamp, ingested_at, source, headline, COALESCE(url,''), category,
  sentiment, sentiment_score, materiality, materiality_score, confidence,
  COALESCE(affected_sector,''), expected_horizon, stage, matched_terms,
  COALESCE(llm_model,''), llm_agreed, cluster_id
FROM news_events
WHERE ($1='' OR ticker=$1) AND ($2::timestamptz IS NULL OR timestamp >= $2)
ORDER BY timestamp DESC LIMIT $3 OFFSET $4`,
		string(q.Ticker), nullTime(q.From), limitOr(q.Limit, 100), q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.NewsEvent
	for rows.Next() {
		var n domain.NewsEvent
		var matched []byte
		var cat, sent, mat, horizon string
		if err := rows.Scan(&n.ID, &n.Ticker, &n.Timestamp, &n.IngestedAt, &n.Source,
			&n.Headline, &n.URL, &cat, &sent, &n.SentimentScore, &mat, &n.MaterialityScore,
			&n.Confidence, &n.AffectedSector, &horizon, &n.Stage, &matched,
			&n.LLMModel, &n.LLMAgreed, &n.ClusterID); err != nil {
			return nil, err
		}
		n.Category, n.Sentiment = domain.NewsCategory(cat), domain.Sentiment(sent)
		n.Materiality, n.ExpectedHorizon = domain.Materiality(mat), domain.Horizon(horizon)
		_ = json.Unmarshal(matched, &n.Matched)
		out = append(out, n)
	}
	return out, rows.Err()
}

// Append writes an audit event. The table has no UPDATE or DELETE grant for the
// application role, which is what makes it append-only in practice rather than
// by convention.
func (p *Postgres) Append(ctx context.Context, e AuditEvent) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	detail, _ := json.Marshal(e.Detail)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO audit_log (id, at, principal, action, resource, outcome, request_id, trace_id, detail, remote_ip)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.ID, e.At, e.Principal, e.Action, e.Resource, e.Outcome,
		nullable(e.RequestID), nullable(e.TraceID), detail, nullable(e.RemoteIP))
	return err
}

// ListAudit reads audit events.
func (p *Postgres) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, at, principal, action, resource, outcome, COALESCE(request_id,''),
  COALESCE(trace_id,''), detail, COALESCE(remote_ip,'')
FROM audit_log ORDER BY at DESC LIMIT $1`, limitOr(limit, 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var detail []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Principal, &e.Action, &e.Resource, &e.Outcome,
			&e.RequestID, &e.TraceID, &detail, &e.RemoteIP); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SaveRegime appends a regime record.
func (p *Postgres) SaveRegime(ctx context.Context, r domain.MarketRegime) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	payload, _ := json.Marshal(r)
	_, err := p.db.ExecContext(ctx, `
INSERT INTO regimes (as_of, regime, confidence, volatility, breadth, momentum, changed, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (as_of) DO NOTHING`,
		r.AsOf, string(r.Regime), r.Confidence, r.Volatility, r.Breadth, r.Momentum, r.Changed, payload)
	return err
}

// LatestRegime reads the most recent regime.
func (p *Postgres) LatestRegime(ctx context.Context) (domain.MarketRegime, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	var payload []byte
	err := p.db.QueryRowContext(ctx, `SELECT payload FROM regimes ORDER BY as_of DESC LIMIT 1`).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MarketRegime{}, ErrNotFound
	}
	if err != nil {
		return domain.MarketRegime{}, err
	}
	var r domain.MarketRegime
	return r, json.Unmarshal(payload, &r)
}

// ListRegimes reads regime history.
func (p *Postgres) ListRegimes(ctx context.Context, q Query) ([]domain.MarketRegime, error) {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `
SELECT payload FROM regimes WHERE ($1::timestamptz IS NULL OR as_of >= $1)
ORDER BY as_of DESC LIMIT $2`, nullTime(q.From), limitOr(q.Limit, 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MarketRegime
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var r domain.MarketRegime
		if err := json.Unmarshal(payload, &r); err == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// Ping verifies the connection.
func (p *Postgres) Ping(ctx context.Context) error {
	ctx, cancel := p.ctx(ctx)
	defer cancel()
	return p.db.PingContext(ctx)
}

// Close releases the pool.
func (p *Postgres) Close() error { return p.db.Close() }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullTimeValue(t time.Time) any { return nullTime(t) }

func limitOr(v, def int) int {
	if v <= 0 {
		return def
	}
	if v > 5000 {
		return 5000
	}
	return v
}

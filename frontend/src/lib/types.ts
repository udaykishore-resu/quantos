/**
 * TypeScript mirror of `internal/domain`, `internal/api` and `internal/httpx`.
 *
 * Every field here corresponds to a JSON tag in the Go source. Nothing is
 * invented and nothing is renamed: when the Go type says `risk_decision` with
 * the value `WATCH_ONLY`, so does this file. Where Go emits a zero value for a
 * field that means "not measured" (a nil slice serialised as `null`, or a zero
 * `time.Time` serialised as `0001-01-01T00:00:00Z`), the type is widened here
 * and the caller is expected to treat it as absent — see `lib/format.ts`.
 */

// --- Envelope (internal/httpx/server.go) ------------------------------------

export interface Meta {
  count?: number;
  limit?: number;
  offset?: number;
  /** Go emits `0001-01-01T00:00:00Z` when unset; use `isZeroTime`. */
  as_of?: string;
  stale?: boolean;
  stale_reason?: string;
}

export interface Envelope<T> {
  data: T;
  meta?: Meta;
  degraded?: string[];
  disclaimer: string;
  request_id?: string;
  served_at: string;
}

export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    detail?: string;
    request_id?: string;
  };
  disclaimer: string;
}

// --- Core (internal/domain/core.go) -----------------------------------------

export type Ticker = string;

export type Side = 'LONG' | 'SHORT' | 'FLAT';

export type Exchange = 'NYSE' | 'NASDAQ' | 'AMEX' | 'ARCA' | 'OTHER';

export type AssetClass =
  | 'EQUITY'
  | 'ETF'
  | 'INDEX'
  | 'FUTURE'
  | 'FX'
  | 'RATE'
  | 'COMMODITY';

export type Interval = '1m' | '5m' | '15m' | '1h' | '1d';

export interface Stock {
  ticker: Ticker;
  company: string;
  sector: string;
  industry: string;
  exchange: Exchange;
  asset_class: AssetClass;
  currency: string;
  market_cap: number;
  shares_outstanding: number;
  avg_volume_30d: number;
  /** Simulator fixture level. Never a quote — see the Go comment. */
  reference_price?: number;
  avg_notional_30d: number;
  beta: number;
  listed_at: string;
  delisted_at?: string | null;
  tags?: string[] | null;
}

export interface Candle {
  ticker: Ticker;
  interval: Interval;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
  vwap: number;
  trades: number;
  start: string;
  end: string;
  adjusted: boolean;
  source: string;
  partial: boolean;
  seq: number;
}

export type MarketSession =
  | 'CLOSED'
  | 'PRE_MARKET'
  | 'OPEN'
  | 'AFTER_HOURS'
  | 'HOLIDAY';

export type CorporateEventType =
  | 'EARNINGS'
  | 'DIVIDEND'
  | 'SPLIT'
  | 'GUIDANCE'
  | 'MERGER'
  | 'OFFERING'
  | 'INDEX_ADD'
  | 'INDEX_DROP'
  | 'MACRO';

export interface CorporateEvent {
  id: string;
  ticker: Ticker;
  type: CorporateEventType;
  scheduled_at: string;
  confirmed: boolean;
  detail?: string;
  source: string;
}

export interface AuditEvent {
  id: string;
  at: string;
  principal: string;
  action: string;
  resource: string;
  outcome: string;
  request_id?: string;
  trace_id?: string;
  detail?: Record<string, string> | null;
  remote_ip?: string;
}

// --- Features (internal/domain/features.go) ---------------------------------

export interface FeatureSnapshot {
  ticker: Ticker;
  as_of: string;
  interval: Interval;
  values: Record<string, number> | null;
  /** A feature absent from `warm`, or `warm[name] === false`, is cold. */
  warm: Record<string, boolean> | null;
  last: number;
  bid: number;
  ask: number;
  volume: number;
  spread_bps: number;
  stale: boolean;
  stale_reason?: string;
  last_tick_at: string;
  schema_version: number;
  hash: string;
}

/** The ordered feature list the shipped direction model consumes. */
export const MODEL_FEATURE_SET: readonly string[] = [
  'ret_log_1',
  'ret_log_5',
  'momentum_10',
  'momentum_60',
  'rsi_14',
  'macd_hist',
  'vwap_dist_bps',
  'bb_percent_b',
  'adx_14',
  'rel_strength_20',
  'volume_ratio_20',
  'volume_z_20',
  'atr_pct',
  'realized_vol_20',
  'zscore_20',
  'trend_slope_20',
];

// --- Regime (internal/domain/regime.go) -------------------------------------

export type Regime =
  | 'BULL_TREND'
  | 'BEAR_TREND'
  | 'RANGE'
  | 'HIGH_VOLATILITY'
  | 'LOW_VOLATILITY'
  | 'BREAKOUT'
  | 'REVERSAL'
  | 'RISK_ON'
  | 'RISK_OFF'
  | 'UNDEFINED';

export const ALL_REGIMES: readonly Regime[] = [
  'BULL_TREND',
  'BEAR_TREND',
  'RANGE',
  'HIGH_VOLATILITY',
  'LOW_VOLATILITY',
  'BREAKOUT',
  'REVERSAL',
  'RISK_ON',
  'RISK_OFF',
];

export interface MarketRegime {
  regime: Regime | '';
  confidence: number;
  secondary?: Regime | '';
  secondary_score?: number;
  volatility: number;
  breadth: number;
  momentum: number;
  risk_appetite: number;
  liquidity: number;
  inputs: Record<string, number> | null;
  scores: Partial<Record<Regime, number>> | null;
  evidence?: string[] | null;
  as_of: string;
  version: string;
  changed: boolean;
  previous_regime?: Regime | '';
  /** Go `time.Duration` marshals as an integer count of nanoseconds. */
  stable_for: number;
}

export interface SectorPerformance {
  sector: string;
  etf?: Ticker;
  return_1d: number;
  return_5d: number;
  return_20d: number;
  rel_strength: number;
  breadth: number;
  rank: number;
}

export interface Divergence {
  a: string;
  b: string;
  expected_correlation: number;
  observed_correlation: number;
  magnitude: number;
  note: string;
}

export interface RelationshipState {
  as_of: string;
  correlation: Record<string, number> | null;
  correlation_change: Record<string, number> | null;
  sectors: SectorPerformance[] | null;
  rotation: string;
  risk_on_score: number;
  divergences?: Divergence[] | null;
  notes?: string[] | null;
}

// --- Prediction (internal/domain/prediction.go) -----------------------------

export type Outcome = 'UP' | 'FLAT' | 'DOWN';
export const ALL_OUTCOMES: readonly Outcome[] = ['UP', 'FLAT', 'DOWN'];

export type Horizon = '15m' | '60m' | '1d' | '5d';

export interface Distribution {
  up: number;
  flat: number;
  down: number;
}

export interface ScenarioPrediction {
  horizon: Horizon;
  distribution: Distribution;
  /** Half-width of the FLAT class in bps. Without it "UP 0.63" is meaningless. */
  flat_band_bps: number;
  expected_move_bps: number;
  confidence: number;
  uncertainty: number;
}

export type PredictionSource = 'model' | 'rules-fallback' | 'blend';

export interface FeatureContribution {
  feature: string;
  value: number;
  weight: number;
  contribution: number;
}

export interface Prediction {
  id: string;
  ticker: Ticker;
  created_at: string;
  scenarios: ScenarioPrediction[] | null;
  model_id: string;
  model_version: string;
  artifact_sha?: string;
  source: PredictionSource;
  feature_hash: string;
  feature_as_of: string;
  regime: Regime | '';
  regime_confidence: number;
  rule_prior: Distribution;
  model_output: Distribution;
  blend_weight: number;
  contributions?: FeatureContribution[] | null;
  degraded?: string[] | null;
  disclaimer: string;
}

export interface PredictionOutcome {
  prediction_id: string;
  ticker: Ticker;
  horizon: Horizon;
  created_at: string;
  resolved_at: string;
  entry_price: number;
  exit_price: number;
  return_bps: number;
  predicted: Outcome;
  actual: Outcome;
  correct: boolean;
  distribution: Distribution;
  brier_score: number;
  log_loss: number;
  regime: Regime | '';
  model_version: string;
  risk_decision: RiskDecision | '';
}

// --- Risk (internal/domain/risk.go) -----------------------------------------

export type RiskDecision = 'ALLOW_PAPER_SIGNAL' | 'WATCH_ONLY' | 'BLOCK';

export type RiskLevel = 'LOW' | 'MEDIUM' | 'HIGH' | 'EXTREME';

export type CheckStatus = 'PASS' | 'WARN' | 'FAIL' | 'SKIP';

export interface RiskCheck {
  name: string;
  status: CheckStatus;
  decision: RiskDecision | '';
  value: number;
  threshold: number;
  reason: string;
  skipped_reason?: string;
}

export interface RiskAssessment {
  id: string;
  ticker: Ticker;
  created_at: string;
  decision: RiskDecision | '';
  level: RiskLevel | '';
  /** 0..100, higher = riskier. */
  score: number;
  checks: RiskCheck[] | null;
  blockers?: string[] | null;
  warnings?: string[] | null;
  config_hash: string;
  regime: Regime | '';
  feature_hash?: string;
  prediction_id?: string;
  evaluated_at: string;
}

/** Canonical risk check names, from internal/domain/risk.go. */
export const RISK_CHECK_NAMES: readonly string[] = [
  'data_freshness',
  'liquidity',
  'spread',
  'volatility',
  'event_proximity',
  'model_confidence',
  'model_health',
  'market_regime',
  'portfolio_concentration',
  'correlation_exposure',
  'portfolio_drawdown',
];

// --- Signal (internal/domain/signal.go) -------------------------------------

export type Classification =
  | 'STRONG_WATCH'
  | 'BULLISH_SETUP'
  | 'NEUTRAL'
  | 'HIGH_RISK'
  | 'AVOID';

export interface StockScore {
  ticker: Ticker;
  created_at: string;
  fundamental_score: number;
  growth_score: number;
  quality_score: number;
  valuation_score: number;
  technical_score: number;
  momentum_score: number;
  sector_score: number;
  market_regime_score: number;
  /** higher = safer */
  risk_score: number;
  /** higher = safer */
  event_risk_score: number;
  final_score: number;
  classification: Classification | '';
  weights: Record<string, number> | null;
  strategy_id: string;
  config_hash: string;
  /** Sub-scores that could not be computed; weights are renormalised, not zeroed. */
  missing?: string[] | null;
  evidence?: string[] | null;
  disclaimer: string;
}

export interface OpportunityScore {
  ticker: Ticker;
  created_at: string;
  score: number;
  bias: Side | '';
  confidence: number;
  risk: RiskLevel | '';
  components: Record<string, number> | null;
  risk_reward: number;
  expected_move_bps: number;
  stop_distance_bps: number;
  target_distance_bps: number;
  prediction_id?: string;
  score_ref?: string;
  regime: Regime | '';
  disclaimer: string;
}

export type SignalStatus = 'ACTIVE' | 'INVALIDATED' | 'EXPIRED' | 'COMPLETED';

export type InvalidationKind =
  | 'PRICE_BELOW'
  | 'PRICE_ABOVE'
  | 'VWAP_CROSS'
  | 'REGIME_CHANGE'
  | 'VOLATILITY_ABOVE'
  | 'CONFIDENCE_BELOW'
  | 'RISK_VETO'
  | 'TIME_EXPIRY'
  | 'MATERIAL_NEWS';

export interface InvalidationCondition {
  kind: InvalidationKind;
  threshold?: number;
  regime_from?: Regime | '';
  deadline?: string;
  description: string;
}

export interface Signal {
  id: string;
  ticker: Ticker;
  created_at: string;
  expires_at: string;
  side: Side;
  status: SignalStatus;
  /** 0..100 */
  strength: number;
  confidence: number;
  horizon: Horizon;
  entry_reference: number;
  stop_reference: number;
  target_reference: number;
  /** fraction of paper equity */
  suggested_weight: number;
  invalidations: InvalidationCondition[] | null;
  // Provenance chain.
  strategy_id: string;
  rules_fired: string[] | null;
  evidence: string[] | null;
  counter_evidence?: string[] | null;
  prediction_id: string;
  risk_assessment_id: string;
  score_ref?: string;
  feature_hash: string;
  regime: Regime | '';
  model_version: string;
  config_hash: string;
  correlation_id: string;
  // Lifecycle.
  invalidated_at?: string | null;
  invalidation_kind?: InvalidationKind | '';
  invalidation_note?: string;
  explanation?: string;
  explanation_source?: string;
  disclaimer: string;
}

// --- News and alerts (internal/domain/news.go) ------------------------------

export type NewsCategory =
  | 'EARNINGS'
  | 'GUIDANCE'
  | 'M_AND_A'
  | 'REGULATORY'
  | 'LEGAL'
  | 'PRODUCT'
  | 'MANAGEMENT'
  | 'ANALYST_ACTION'
  | 'MACRO'
  | 'GENERAL';

export type Sentiment = 'positive' | 'neutral' | 'negative';
export type Materiality = 'high' | 'medium' | 'low';

export interface NewsEvent {
  id: string;
  /** "__MACRO__" for market-wide items. */
  ticker: Ticker;
  timestamp: string;
  ingested_at: string;
  source: string;
  headline: string;
  url?: string;
  category: NewsCategory;
  sentiment: Sentiment;
  sentiment_score: number;
  materiality: Materiality;
  materiality_score: number;
  confidence: number;
  affected_sector?: string;
  expected_horizon: Horizon;
  stage: string;
  matched_terms?: string[] | null;
  llm_model?: string;
  llm_agreed?: boolean;
  deduplicated?: boolean;
  cluster_id?: string;
}

export type AlertType =
  | 'VWAP_CROSS'
  | 'VOLUME_ACCELERATION'
  | 'BREAKOUT'
  | 'MOMENTUM_SHIFT'
  | 'REGIME_CHANGE'
  | 'VOLATILITY_SPIKE'
  | 'NEWS_EVENT'
  | 'PROBABILITY_SHIFT'
  | 'SIGNAL_GENERATED'
  | 'SIGNAL_INVALIDATED'
  | 'RISK_BLOCK'
  | 'MODEL_DRIFT'
  | 'DATA_STALE';

export type AlertSeverity = 'INFO' | 'NOTICE' | 'WARNING' | 'CRITICAL';

export interface Alert {
  id: string;
  dedup_key: string;
  type: AlertType;
  severity: AlertSeverity;
  ticker?: Ticker;
  created_at: string;
  title: string;
  message: string;
  before?: Record<string, number> | null;
  after?: Record<string, number> | null;
  regime?: Regime | '';
  risk_level?: RiskLevel | '';
  status?: string;
  signal_id?: string;
  prediction_id?: string;
  correlation_id?: string;
  disclaimer: string;
}

// --- Model (internal/domain/model.go) ---------------------------------------

export type ModelStage = 'shadow' | 'canary' | 'active' | 'retired';
export type ModelFamily = 'multinomial_logistic' | 'gradient_boosted_stumps';
export type DriftSeverity = 'none' | 'mild' | 'moderate' | 'severe';

export interface ConfusionMatrix {
  /** Counts[predicted][actual] */
  counts: Partial<Record<Outcome, Partial<Record<Outcome, number>>>> | null;
  total: number;
}

export interface CalibrationBin {
  lower: number;
  upper: number;
  count: number;
  mean_predicted: number;
  observed_frequency: number;
  deviation: number;
}

export interface ModelEvaluation {
  model_version: string;
  window: string;
  start: string;
  end: string;
  horizon: Horizon | '';
  regime?: Regime | '';
  samples: number;
  accuracy: number;
  /** The constant-guess benchmark. Accuracy is never shown without it. */
  base_rate: number;
  lift: number;
  brier_score: number;
  brier_skill_score: number;
  log_loss: number;
  precision_up: number;
  recall_up: number;
  precision_down: number;
  recall_down: number;
  macro_f1: number;
  confusion: ConfusionMatrix;
  calibration?: CalibrationBin[] | null;
  max_calibration_deviation: number;
  expected_calibration_error: number;
  computed_at: string;
}

export interface ModelVersion {
  id: string;
  model_id: string;
  version: string;
  family: ModelFamily | '';
  stage: ModelStage | '';
  artifact_uri: string;
  artifact_sha: string;
  features: string[] | null;
  horizon: Horizon | '';
  flat_band_bps: number;
  train_start: string;
  train_end: string;
  valid_start: string;
  valid_end: string;
  test_start: string;
  test_end: string;
  trained_at: string;
  registered_at: string;
  promoted_at?: string | null;
  training_commit?: string;
  hyperparameters?: Record<string, number> | null;
  canary_fraction?: number;
  out_of_sample: ModelEvaluation;
  by_regime?: Partial<Record<Regime, ModelEvaluation>> | null;
  notes?: string;
}

export interface DriftReport {
  model_version: string;
  computed_at: string;
  severity: DriftSeverity | '';
  /** Population stability index per feature. PSI > 0.25 is a significant shift. */
  feature_drift: Record<string, number> | null;
  max_feature_psi: number;
  drifted_features?: string[] | null;
  prediction_drift: number;
  reference_accuracy: number;
  recent_accuracy: number;
  accuracy_delta: number;
  reference_brier: number;
  recent_brier: number;
  regime_degradation?: Partial<Record<Regime, number>> | null;
  samples: number;
  recommendation: string;
  reasons?: string[] | null;
}

export interface ModelHealth {
  model_id: string;
  model_version: string;
  stage: ModelStage | '';
  as_of: string;
  accuracy: number;
  /** 1 - ECE, higher is better. */
  calibration: number;
  brier_score: number;
  drift: DriftSeverity | '';
  high_volatility_accuracy: number;
  trending_accuracy: number;
  range_accuracy: number;
  samples: number;
  healthy: boolean;
  issues?: string[] | null;
  last_evaluated_at: string;
  /** Derived from the offline test split, not from live resolved predictions. */
  provisional?: boolean;
}

/** internal/evaluation/evaluation.go */
export interface VetoCost {
  blocked: number;
  watch_only: number;
  allowed: number;
  blocked_accuracy: number;
  allowed_accuracy: number;
  blocked_mean_return_bps: number;
  allowed_mean_return_bps: number;
  interpretation: string;
}

/** GET /api/v1/model-health — assembled inline by api.modelHealth. */
export interface ModelHealthResponse {
  health: ModelHealth;
  drift: DriftReport;
  overall?: ModelEvaluation;
  by_regime?: Partial<Record<Regime, ModelEvaluation>> | null;
  veto_cost?: VetoCost;
  pending?: number;
}

/** GET /api/v1/evaluations */
export interface EvaluationsResponse {
  outcomes: PredictionOutcome[] | null;
  evaluation: ModelEvaluation;
  calibration: CalibrationBin[] | null;
}

/** GET /api/v1/models — the inline `modelInfo` struct in api.listModels. */
export interface ModelInfo {
  id: string;
  version: string;
  family: string;
  horizon: string;
  features: string[] | null;
  artifact_sha: string;
}

// --- Strategy (internal/domain/strategy.go) ---------------------------------

export type Comparator =
  | 'gt'
  | 'gte'
  | 'lt'
  | 'lte'
  | 'between'
  | 'outside'
  | 'cross_up'
  | 'cross_down'
  | 'is_true'
  | 'is_false';

export interface Condition {
  feature: string;
  op: Comparator;
  value: number;
  value2?: number;
  other?: string;
  scale?: number;
  optional?: boolean;
  describe?: string;
}

export interface Rule {
  id: string;
  description: string;
  side: Side;
  weight: number;
  all?: Condition[] | null;
  any?: Condition[] | null;
  none?: Condition[] | null;
  regimes?: Regime[] | null;
  /** A counter rule argues against the strategy's thesis when it fires. */
  counter?: boolean;
  enabled?: boolean | null;
}

export interface ScoreWeights {
  fundamental: number;
  growth: number;
  quality: number;
  valuation: number;
  technical: number;
  momentum: number;
  sector: number;
  market_regime: number;
  risk: number;
  event_risk: number;
}

export interface SignalPolicy {
  min_rule_score: number;
  min_final_score: number;
  min_confidence: number;
  min_directional_edge: number;
  min_risk_reward: number;
  horizon: Horizon;
  stop_atr_multiple: number;
  target_atr_multiple: number;
  max_weight: number;
  cooldown_minutes: number;
  allow_short: boolean;
}

export interface InvalidationTemplate {
  kind: InvalidationKind;
  ref?: string;
  multiple?: number;
  value?: number;
  minutes?: number;
  description: string;
}

export interface Strategy {
  id: string;
  name: string;
  version: string;
  description: string;
  enabled: boolean;
  universe?: string[] | null;
  interval: Interval;
  regimes?: Regime[] | null;
  rules: Rule[] | null;
  weights: ScoreWeights;
  policy: SignalPolicy;
  invalidation: InvalidationTemplate[] | null;
  source_path?: string;
  hash?: string;
  modified_at?: string;
}

// --- Portfolio and backtest (internal/domain/portfolio.go) ------------------

export type OrderSide = 'BUY' | 'SELL';
export type OrderType = 'MARKET' | 'LIMIT' | 'STOP';
export type OrderStatus =
  | 'PENDING'
  | 'FILLED'
  | 'PARTIALLY_FILLED'
  | 'CANCELLED'
  | 'REJECTED';

export interface Fill {
  quantity: number;
  price: number;
  commission: number;
  slippage_bps: number;
  at: string;
}

export interface PaperOrder {
  id: string;
  portfolio_id: string;
  idempotency_key: string;
  ticker: Ticker;
  side: OrderSide;
  type: OrderType;
  quantity: number;
  limit_price?: number;
  stop_price?: number;
  time_in_force: string;
  status: OrderStatus;
  filled_qty: number;
  avg_fill_price: number;
  commission: number;
  slippage_bps: number;
  reject_reason?: string;
  signal_id?: string;
  created_at: string;
  updated_at: string;
  fills?: Fill[] | null;
}

export interface PaperPosition {
  portfolio_id: string;
  ticker: Ticker;
  /** negative for short */
  quantity: number;
  avg_price: number;
  market_price: number;
  market_value: number;
  cost_basis: number;
  unrealized_pnl: number;
  realized_pnl: number;
  opened_at: string;
  updated_at: string;
  signal_id?: string;
  sector?: string;
}

export interface Portfolio {
  id: string;
  name: string;
  owner: string;
  currency: string;
  as_of: string;
  starting_cash: number;
  cash: number;
  equity: number;
  market_value: number;
  realized_pnl: number;
  unrealized_pnl: number;
  total_pnl: number;
  return_pct: number;
  gross_exposure: number;
  net_exposure: number;
  long_exposure: number;
  short_exposure: number;
  leverage: number;
  peak_equity: number;
  drawdown: number;
  max_drawdown: number;
  sector_exposure?: Record<string, number> | null;
  positions?: PaperPosition[] | null;
  /** Always true. There is no real-money counterpart in this platform. */
  paper: boolean;
}

export interface EquityPoint {
  at: string;
  equity: number;
  cash: number;
  drawdown: number;
  exposure: number;
}

/** GET /api/v1/paper/portfolio — assembled inline by api.portfolio. */
export interface PortfolioResponse {
  portfolio: Portfolio;
  equity_curve: EquityPoint[] | null;
  trades: BacktestTrade[] | null;
}

export type BacktestStatus =
  | 'QUEUED'
  | 'RUNNING'
  | 'COMPLETED'
  | 'FAILED'
  | 'ABORTED';

export interface CostModel {
  commission_bps: number;
  commission_per_share: number;
  min_commission: number;
  spread_cost_bps: number;
  slippage_model: string;
  slippage_bps: number;
  impact_coefficient: number;
  borrow_rate_annual: number;
  /** fraction of bar volume */
  participation_cap: number;
}

export interface BacktestTrade {
  ticker: Ticker;
  side: Side;
  entry_at: string;
  exit_at: string;
  entry_price: number;
  exit_price: number;
  quantity: number;
  pnl: number;
  return_pct: number;
  costs: number;
  /** Go `time.Duration`: nanoseconds. */
  hold_period: number;
  regime: Regime | '';
  signal_id?: string;
  exit_reason: string;
}

export interface BacktestMetrics {
  total_return: number;
  cagr: number;
  sharpe: number;
  sortino: number;
  calmar: number;
  max_drawdown: number;
  max_drawdown_days: number;
  volatility: number;
  win_rate: number;
  profit_factor: number;
  expectancy: number;
  turnover: number;
  avg_exposure: number;
  trades: number;
  avg_hold_hours: number;
  total_costs: number;
  /** Only meaningful alongside `trials_considered`. */
  deflated_sharpe?: number;
  trials_considered?: number;
}

export interface BacktestRun {
  id: string;
  name: string;
  status: BacktestStatus;
  strategy_id: string;
  universe: Ticker[] | null;
  start: string;
  end: string;
  interval: Interval;
  seed: number;
  config_hash: string;
  model_version: string;
  code_version: string;
  starting_cash: number;
  costs: CostModel;
  metrics: BacktestMetrics;
  regime_metrics?: Partial<Record<Regime, BacktestMetrics>> | null;
  equity_curve?: EquityPoint[] | null;
  trades?: BacktestTrade[] | null;
  created_at: string;
  started_at?: string;
  completed_at?: string;
  error?: string;
  events_processed: number;
  result_hash?: string;
}

export interface BacktestRequest {
  name: string;
  strategy_id: string;
  /** RFC3339 */
  start: string;
  /** RFC3339 */
  end: string;
  interval?: Interval;
  seed?: number;
  starting_cash?: number;
  tickers?: string[];
}

// --- Analyst (internal/analyst/analyst.go) ----------------------------------

export interface GroundingReport {
  [key: string]: unknown;
}

export interface AnalystReport {
  ticker: Ticker;
  generated_at: string;
  summary: string;
  supporting_evidence: string[] | null;
  counter_evidence: string[] | null;
  uncertainty: string;
  risks: string[] | null;
  scenarios: string[] | null;
  source: string;
  model?: string;
  /** Why an LLM narrative was discarded. Surfaced, never hidden. */
  rejected?: string;
  disclaimer: string;
  grounding?: GroundingReport | null;
}

// --- API read model (internal/api/state.go) ---------------------------------

export interface ValuationMetrics {
  ticker: Ticker;
  as_of: string;
  price: number;
  pe: number;
  forward_pe: number;
  peg: number;
  ev_to_ebitda: number;
  price_to_fcf: number;
  price_to_sales: number;
  price_to_book: number;
  earnings_yield: number;
  fcf_yield: number;
  historical_percentile: number;
  sector_percentile: number;
  history_periods: number;
  available: Record<string, boolean> | null;
  valuation_score: number;
  valuation_explanation: string;
}

export interface OptionsMetrics {
  ticker: Ticker;
  as_of: string;
  implied_vol: number;
  iv_rank: number;
  iv_percentile: number;
  put_call_ratio: number;
  open_interest: number;
  volume: number;
  skew_25d: number;
  delta: number;
  gamma: number;
  theta: number;
  vega: number;
  /** "none" | "partial" | "good" */
  data_quality: string;
  confidence: number;
  caveats?: string[] | null;
}

export interface InstrumentView {
  stock: Stock;
  features?: FeatureSnapshot | null;
  score?: StockScore | null;
  prediction?: Prediction | null;
  risk?: RiskAssessment | null;
  opportunity?: OpportunityScore | null;
  valuation?: ValuationMetrics | null;
  options?: OptionsMetrics | null;
  signals?: Signal[] | null;
  news?: NewsEvent[] | null;
  events?: CorporateEvent[] | null;
  explanation?: AnalystReport | null;
  evidence?: string[] | null;
  counter_evidence?: string[] | null;
  updated_at: string;
}

/** GET /api/v1/stocks — api.Ranked. */
export interface Ranked {
  ticker: Ticker;
  company: string;
  sector: string;
  last: number;
  score: number;
  opportunity: number;
  classification: Classification | '';
  bias: Side | '';
  confidence: number;
  risk: RiskLevel | '';
  risk_decision: RiskDecision | '';
  probability: Distribution;
  stale: boolean;
  /** Why no signal was emitted for this instrument, when that is known. */
  rejection?: string;
  updated_at: string;
}

/** GET /api/v1/market/status — assembled inline by api.marketStatus. */
export interface MarketStatusResponse {
  regime: Regime | '';
  regime_confidence: number;
  instruments: number;
  stale_instruments: number;
  as_of: string;
}

/** GET /api/v1/signals/{id}/provenance — api.Provenance. */
export interface Provenance {
  signal: Signal;
  feature_snapshot?: FeatureSnapshot | null;
  feature_snapshot_error?: string;
  prediction?: Prediction | null;
  risk_assessment?: RiskAssessment | null;
  regime?: MarketRegime | null;
  model_version?: ModelVersion | null;
  strategy?: Strategy | null;
  outcomes?: PredictionOutcome[] | null;
  reproducible: boolean;
  /** Inputs that could not be retrieved. Non-empty means not reproducible. */
  missing?: string[] | null;
  explanation?: string;
  note: string;
}

// --- Brief (internal/brief/brief.go) ----------------------------------------

export interface BriefOpportunity {
  ticker: Ticker;
  company: string;
  sector: string;
  score: number;
  classification: Classification | '';
  bias: Side | '';
  probability: Distribution;
  horizon: Horizon | '';
  confidence: number;
  risk: RiskLevel | '';
  risk_decision: RiskDecision | '';
  reasons: string[] | null;
  counter_signals: string[] | null;
  important_events?: string[] | null;
}

export interface MarketOverview {
  as_of: string;
  regime: MarketRegime;
  benchmarks: Record<string, number> | null;
  vix: number;
  breadth: number;
  sector_rotation: SectorPerformance[] | null;
  rotation: string;
  divergences?: Divergence[] | null;
  notes?: string[] | null;
}

export interface PredictionSummary {
  resolved: number;
  accuracy: number;
  base_rate: number;
  brier_score: number;
  calibration: number;
  best_regime?: string;
  worst_regime?: string;
  veto_cost: VetoCost;
  interpretation: string;
}

export interface ModelConfidence {
  model_version: string;
  stage: ModelStage | '';
  healthy: boolean;
  drift: DriftSeverity | '';
  issues?: string[] | null;
  serving_from_fallback: boolean;
}

export interface Brief {
  id: string;
  generated_at: string;
  trading_day: string;
  title: string;
  market_overview: MarketOverview;
  major_macro_events: NewsEvent[] | null;
  sector_leaders: SectorPerformance[] | null;
  top_opportunities: BriefOpportunity[] | null;
  high_risk: BriefOpportunity[] | null;
  important_earnings: CorporateEvent[] | null;
  prediction_summary: PredictionSummary;
  model_confidence: ModelConfidence;
  risk_warnings: string[] | null;
  narrative: string;
  narrative_source: string;
  disclaimer: string;
}

// --- Health (internal/api/api.go) -------------------------------------------

export interface StoreComponentHealth {
  enabled: boolean;
  healthy: boolean;
  error?: string;
  [key: string]: unknown;
}

export interface HealthResponse {
  status: string;
  version: string;
  mode: string;
  uptime: string;
  degraded?: string[] | null;
  store?: Record<string, StoreComponentHealth> | null;
  stream_clients: number;
  timestamp: string;
}

/**
 * Readiness as the dashboard needs it. `/readyz` answers 200 with
 * `{"status":"ready"}` or 503 with an error envelope whose detail explains what
 * is unavailable, so the UI derives both facts from one call.
 */
export interface Readiness {
  ready: boolean;
  reason: string;
  degraded: string[];
}

// --- Stream (internal/httpx/sse.go) -----------------------------------------

/** The dashboard's subscription vocabulary, from internal/httpx/sse.go. */
export const STREAM_TOPICS = [
  'quote',
  'regime',
  'signal',
  'signal_invalidated',
  'alert',
  'prediction',
  'portfolio',
  'health',
] as const;

export type StreamTopic = (typeof STREAM_TOPICS)[number];

/**
 * Control events the hub emits out of band. They carry no `id:` field, so they
 * never advance Last-Event-ID.
 */
export type StreamControlEvent = 'resync' | 'overflow';

export interface StreamControlPayload {
  reason: string;
}

export const DISCLAIMER =
  'QuantOS is an educational research and paper-trading platform. ' +
  'Output is probabilistic, may be wrong, and is not financial advice. ' +
  'No real-money orders are placed.';

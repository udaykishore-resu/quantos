// Package config loads and validates QuantOS configuration.
//
// Precedence, lowest to highest: built-in defaults, YAML file, environment
// variables. Every loaded configuration is hashed, and that hash is stamped on
// every risk assessment, score and signal, so a historical decision can be
// re-derived even after the configuration changes (requirement §50).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Mode selects the infrastructure profile (see docs/architecture/overview.md).
type Mode string

const (
	ModeEmbedded Mode = "embedded" // in-process bus, in-memory stores
	ModeCompose  Mode = "compose"  // local Kafka/Postgres/Redis/ClickHouse
	ModeCluster  Mode = "cluster"  // MSK/RDS/ElastiCache
)

// Config is the root configuration object.
type Config struct {
	Service string `yaml:"service"`
	Env     string `yaml:"env"`
	Version string `yaml:"version"`
	Mode    Mode   `yaml:"mode"`

	HTTP       HTTPConfig       `yaml:"http"`
	Log        LogConfig        `yaml:"log"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
	Bus        BusConfig        `yaml:"bus"`
	Store      StoreConfig      `yaml:"store"`
	Market     MarketConfig     `yaml:"market"`
	Features   FeatureConfig    `yaml:"features"`
	Regime     RegimeConfig     `yaml:"regime"`
	Predict    PredictConfig    `yaml:"predict"`
	Risk       RiskConfig       `yaml:"risk"`
	Signals    SignalConfig     `yaml:"signals"`
	Alerts     AlertConfig      `yaml:"alerts"`
	Paper      PaperConfig      `yaml:"paper"`
	News       NewsConfig       `yaml:"news"`
	LLM        LLMConfig        `yaml:"llm"`
	Auth       AuthConfig       `yaml:"auth"`
	Universe   UniverseConfig   `yaml:"universe"`
	Strategies StrategyConfig   `yaml:"strategies"`
	Evaluation EvaluationConfig `yaml:"evaluation"`

	// Hash is computed at load time over the effective configuration.
	Hash string `yaml:"-"`
	// SourcePath records where the file came from, for the /health payload.
	SourcePath string `yaml:"-"`
}

// HTTPConfig parameterises the API server.
type HTTPConfig struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
	CORSOrigins     []string      `yaml:"cors_origins"`
	RateLimitRPS    float64       `yaml:"rate_limit_rps"`
	RateLimitBurst  int           `yaml:"rate_limit_burst"`
	SSEBuffer       int           `yaml:"sse_buffer"`
	SSEReplay       int           `yaml:"sse_replay"`
}

// LogConfig parameterises logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// TelemetryConfig parameterises tracing and metrics.
type TelemetryConfig struct {
	OTLPEndpoint string        `yaml:"otlp_endpoint"`
	MetricsAddr  string        `yaml:"metrics_addr"`
	SampleRatio  float64       `yaml:"sample_ratio"`
	FlushEvery   time.Duration `yaml:"flush_every"`
}

// BusConfig selects and parameterises the event bus driver.
type BusConfig struct {
	Driver      string        `yaml:"driver"` // memory|wal|kafka
	Synchronous bool          `yaml:"synchronous"`
	Dir         string        `yaml:"dir"`
	Brokers     []string      `yaml:"brokers"`
	GroupPrefix string        `yaml:"group_prefix"`
	MaxEventAge time.Duration `yaml:"max_event_age"`
	MaxRetries  int           `yaml:"max_retries"`
	Buffer      int           `yaml:"buffer"`
	DedupTTL    time.Duration `yaml:"dedup_ttl"`
}

// StoreConfig selects and parameterises persistence.
type StoreConfig struct {
	Driver             string        `yaml:"driver"` // memory|sql
	PostgresDSN        string        `yaml:"postgres_dsn"`
	ClickHouseURL      string        `yaml:"clickhouse_url"`
	ClickHouseDB       string        `yaml:"clickhouse_db"`
	ClickHouseUser     string        `yaml:"clickhouse_user"`
	ClickHousePassword string        `yaml:"clickhouse_password"`
	RedisAddr          string        `yaml:"redis_addr"`
	RedisDB            int           `yaml:"redis_db"`
	MaxOpenConns       int           `yaml:"max_open_conns"`
	MaxIdleConns       int           `yaml:"max_idle_conns"`
	ConnTimeout        time.Duration `yaml:"conn_timeout"`
	Retention          time.Duration `yaml:"retention"`
}

// MarketConfig parameterises data ingestion.
type MarketConfig struct {
	Provider         string          `yaml:"provider"` // sim|replay|composite
	Providers        []string        `yaml:"providers"`
	Seed             int64           `yaml:"seed"`
	TickInterval     time.Duration   `yaml:"tick_interval"`
	BarInterval      domain.Interval `yaml:"bar_interval"`
	StalenessSoft    time.Duration   `yaml:"staleness_soft"`
	StalenessHard    time.Duration   `yaml:"staleness_hard"`
	MaxGapPct        float64         `yaml:"max_gap_pct"`
	MaxSpreadBps     float64         `yaml:"max_spread_bps"`
	ReplayDir        string          `yaml:"replay_dir"`
	ReplaySpeed      float64         `yaml:"replay_speed"`
	RequestTimeout   time.Duration   `yaml:"request_timeout"`
	CacheTTL         time.Duration   `yaml:"cache_ttl"`
	RateLimitPerSec  float64         `yaml:"rate_limit_per_sec"`
	CircuitThreshold int             `yaml:"circuit_threshold"`
	CircuitCooldown  time.Duration   `yaml:"circuit_cooldown"`
}

// FeatureConfig parameterises the feature engine.
type FeatureConfig struct {
	Interval       domain.Interval `yaml:"interval"`
	Warmup         int             `yaml:"warmup_bars"`
	MaxHistory     int             `yaml:"max_history_bars"`
	Benchmark      domain.Ticker   `yaml:"benchmark"`
	VWAPResetDaily bool            `yaml:"vwap_reset_daily"`
}

// RegimeConfig parameterises regime detection.
type RegimeConfig struct {
	Benchmarks       []domain.Ticker `yaml:"benchmarks"`
	VolatilityRef    domain.Ticker   `yaml:"volatility_ref"`
	MinConfidence    float64         `yaml:"min_confidence"`
	Hysteresis       time.Duration   `yaml:"hysteresis"`
	VIXHigh          float64         `yaml:"vix_high"`
	VIXLow           float64         `yaml:"vix_low"`
	TrendThreshold   float64         `yaml:"trend_threshold"`
	BreadthThreshold float64         `yaml:"breadth_threshold"`
}

// PredictConfig parameterises the prediction engine.
type PredictConfig struct {
	ArtifactDir             string           `yaml:"artifact_dir"`
	ModelID                 string           `yaml:"model_id"`
	Horizons                []domain.Horizon `yaml:"horizons"`
	FlatBandBps             float64          `yaml:"flat_band_bps"`
	BlendReferenceLift      float64          `yaml:"blend_reference_lift"`
	DirectionalReferenceAUC float64          `yaml:"directional_reference_auc"`
	BlendWeight             float64          `yaml:"blend_weight"`
	FallbackCeiling         float64          `yaml:"fallback_confidence_ceiling"`
	CanaryFraction          float64          `yaml:"canary_fraction"`
	MaxLatency              time.Duration    `yaml:"max_latency"`
}

// RiskConfig holds every risk threshold. It is the most safety-critical block
// in the file, so each field is explicit rather than a generic map.
type RiskConfig struct {
	StalenessSoft          time.Duration   `yaml:"staleness_soft"`
	StalenessHard          time.Duration   `yaml:"staleness_hard"`
	MinLiquidity           float64         `yaml:"min_liquidity"`
	PreferredLiquidity     float64         `yaml:"preferred_liquidity"`
	MaxSpreadSoft          float64         `yaml:"max_spread_soft_bps"`
	MaxSpreadHard          float64         `yaml:"max_spread_hard_bps"`
	MaxVolSoft             float64         `yaml:"max_vol_soft"`
	MaxVolHard             float64         `yaml:"max_vol_hard"`
	EarningsWatch          time.Duration   `yaml:"earnings_watch_window"`
	EarningsBlock          time.Duration   `yaml:"earnings_block_window"`
	MinConfidence          float64         `yaml:"min_confidence"`
	MinRegimeConfidence    float64         `yaml:"min_regime_confidence"`
	BlockedRegimes         []domain.Regime `yaml:"blocked_regimes"`
	MaxPositionWeight      float64         `yaml:"max_position_weight"`
	SoftPositionWeight     float64         `yaml:"soft_position_weight"`
	MaxSectorWeight        float64         `yaml:"max_sector_weight"`
	SoftSectorWeight       float64         `yaml:"soft_sector_weight"`
	MaxCorrelatedExposure  float64         `yaml:"max_correlated_exposure"`
	SoftCorrelatedExposure float64         `yaml:"soft_correlated_exposure"`
	MaxDrawdown            float64         `yaml:"max_drawdown"`
	DrawdownWarn           float64         `yaml:"drawdown_warn"`
	VetoRateAlert          float64         `yaml:"veto_rate_alert"`
}

// SignalConfig parameterises signal generation and invalidation.
type SignalConfig struct {
	DefaultStrategy   string        `yaml:"default_strategy"`
	MaxActive         int           `yaml:"max_active"`
	MaxPerTicker      int           `yaml:"max_per_ticker"`
	Cooldown          time.Duration `yaml:"cooldown"`
	DefaultTTL        time.Duration `yaml:"default_ttl"`
	InvalidationSweep time.Duration `yaml:"invalidation_sweep"`
}

// AlertConfig parameterises the alert engine.
type AlertConfig struct {
	DedupBucket         time.Duration `yaml:"dedup_bucket"`
	Cooldown            time.Duration `yaml:"cooldown"`
	MaxPerTickerPerHour int           `yaml:"max_per_ticker_per_hour"`
	ProbabilityDelta    float64       `yaml:"probability_delta"`
	VolumeAcceleration  float64       `yaml:"volume_acceleration"`
	VolatilitySpike     float64       `yaml:"volatility_spike"`
	Enabled             []string      `yaml:"enabled"`
}

// PaperConfig parameterises the simulated broker.
type PaperConfig struct {
	StartingCash float64          `yaml:"starting_cash"`
	MaxPositions int              `yaml:"max_positions"`
	MaxWeight    float64          `yaml:"max_weight"`
	AllowShort   bool             `yaml:"allow_short"`
	Costs        domain.CostModel `yaml:"costs"`
	FillDelay    time.Duration    `yaml:"fill_delay"`
	MarkInterval time.Duration    `yaml:"mark_interval"`
}

// NewsConfig parameterises the news pipeline.
type NewsConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Sources         []string      `yaml:"sources"`
	PollInterval    time.Duration `yaml:"poll_interval"`
	DedupWindow     time.Duration `yaml:"dedup_window"`
	MinMateriality  float64       `yaml:"min_materiality"`
	UseLLM          bool          `yaml:"use_llm"`
	MaxLLMPerMinute int           `yaml:"max_llm_per_minute"`
	FixturePath     string        `yaml:"fixture_path"`
}

// LLMConfig parameterises the explanation layer.
type LLMConfig struct {
	Provider     string        `yaml:"provider"` // mock|anthropic
	Model        string        `yaml:"model"`
	APIKeyEnv    string        `yaml:"api_key_env"`
	BaseURL      string        `yaml:"base_url"`
	MaxTokens    int           `yaml:"max_tokens"`
	Temperature  float64       `yaml:"temperature"`
	Timeout      time.Duration `yaml:"timeout"`
	CacheTTL     time.Duration `yaml:"cache_ttl"`
	MaxPerMinute int           `yaml:"max_per_minute"`
	FailOpen     bool          `yaml:"fail_open"`
}

// AuthConfig parameterises authentication and authorisation.
type AuthConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Issuer        string        `yaml:"issuer"`
	Audience      string        `yaml:"audience"`
	JWKSURL       string        `yaml:"jwks_url"`
	HMACSecretEnv string        `yaml:"hmac_secret_env"`
	TokenTTL      time.Duration `yaml:"token_ttl"`
	DevUsers      []DevUser     `yaml:"dev_users"`
}

// DevUser is a locally-configured principal, used only in embedded/compose mode.
type DevUser struct {
	Subject  string   `yaml:"subject"`
	Password string   `yaml:"password"`
	Roles    []string `yaml:"roles"`
}

// UniverseConfig selects the tradable universe.
type UniverseConfig struct {
	Source     string   `yaml:"source"` // builtin|file
	Path       string   `yaml:"path"`
	Include    []string `yaml:"include"`
	Exclude    []string `yaml:"exclude"`
	Watchlist  []string `yaml:"watchlist"`
	MaxSymbols int      `yaml:"max_symbols"`
}

// StrategyConfig points at the YAML strategy directory.
type StrategyConfig struct {
	Dir     string   `yaml:"dir"`
	Enabled []string `yaml:"enabled"`
}

// EvaluationConfig parameterises outcome evaluation and drift detection.
type EvaluationConfig struct {
	SweepInterval     time.Duration `yaml:"sweep_interval"`
	CalibrationBins   int           `yaml:"calibration_bins"`
	DriftWindow       int           `yaml:"drift_window"`
	DriftReference    int           `yaml:"drift_reference"`
	PSIWarn           float64       `yaml:"psi_warn"`
	PSIAlert          float64       `yaml:"psi_alert"`
	AccuracyDropWarn  float64       `yaml:"accuracy_drop_warn"`
	AccuracyDropAlert float64       `yaml:"accuracy_drop_alert"`
	MinSamples        int           `yaml:"min_samples"`
}

// Default returns the built-in configuration. It is a complete, runnable
// configuration: `make demo` uses it unmodified.
func Default() Config {
	return Config{
		Service: "quantos",
		Env:     "local",
		Version: "0.1.0",
		Mode:    ModeEmbedded,
		HTTP: HTTPConfig{
			Addr:            ":8080",
			ReadTimeout:     10 * time.Second,
			WriteTimeout:    30 * time.Second,
			IdleTimeout:     120 * time.Second,
			ShutdownTimeout: 15 * time.Second,
			MaxBodyBytes:    1 << 20,
			CORSOrigins:     []string{"http://localhost:3000"},
			RateLimitRPS:    50,
			RateLimitBurst:  100,
			SSEBuffer:       256,
			SSEReplay:       1024,
		},
		Log:       LogConfig{Level: "info", Format: "json"},
		Telemetry: TelemetryConfig{MetricsAddr: ":9090", SampleRatio: 1.0, FlushEvery: 5 * time.Second},
		Bus: BusConfig{
			Driver:      "memory",
			GroupPrefix: "quantos",
			MaxEventAge: 5 * time.Minute,
			MaxRetries:  2,
			Buffer:      8192,
			DedupTTL:    48 * time.Hour,
			Dir:         "./.quantos/wal",
		},
		Store: StoreConfig{
			Driver:       "memory",
			MaxOpenConns: 25,
			MaxIdleConns: 5,
			ConnTimeout:  5 * time.Second,
			ClickHouseDB: "quantos",
			Retention:    90 * 24 * time.Hour,
		},
		Market: MarketConfig{
			Provider:         "sim",
			Seed:             20260827,
			TickInterval:     time.Second,
			BarInterval:      domain.Interval1m,
			StalenessSoft:    30 * time.Second,
			StalenessHard:    2 * time.Minute,
			MaxGapPct:        0.25,
			MaxSpreadBps:     500,
			ReplaySpeed:      0,
			RequestTimeout:   5 * time.Second,
			CacheTTL:         2 * time.Second,
			RateLimitPerSec:  50,
			CircuitThreshold: 5,
			CircuitCooldown:  30 * time.Second,
		},
		Features: FeatureConfig{
			Interval: domain.Interval1m,
			// Long enough for the 200-period average to be warm; see
			// domain.MaxFeatureLookbackBars.
			Warmup:         260,
			MaxHistory:     500,
			Benchmark:      "SPY",
			VWAPResetDaily: true,
		},
		Regime: RegimeConfig{
			Benchmarks:       []domain.Ticker{"SPY", "QQQ"},
			VolatilityRef:    "VIX",
			MinConfidence:    0.45,
			Hysteresis:       5 * time.Minute,
			VIXHigh:          24,
			VIXLow:           14,
			TrendThreshold:   0.0004,
			BreadthThreshold: 0.15,
		},
		Predict: PredictConfig{
			ArtifactDir:             "ml/artifacts",
			ModelID:                 "direction-3class",
			Horizons:                domain.DefaultHorizons,
			FlatBandBps:             15,
			BlendWeight:             0.6,
			BlendReferenceLift:      0.05,
			DirectionalReferenceAUC: 0.03,
			FallbackCeiling:         0.55,
			CanaryFraction:          0,
			MaxLatency:              900 * time.Millisecond,
		},
		Risk: RiskConfig{
			StalenessSoft:          30 * time.Second,
			StalenessHard:          2 * time.Minute,
			MinLiquidity:           2_000_000,
			PreferredLiquidity:     20_000_000,
			MaxSpreadSoft:          25,
			MaxSpreadHard:          80,
			MaxVolSoft:             0.45,
			MaxVolHard:             0.90,
			EarningsWatch:          72 * time.Hour,
			EarningsBlock:          24 * time.Hour,
			MinConfidence:          0.30,
			MinRegimeConfidence:    0.35,
			BlockedRegimes:         nil,
			MaxPositionWeight:      0.10,
			SoftPositionWeight:     0.07,
			MaxSectorWeight:        0.35,
			SoftSectorWeight:       0.25,
			MaxCorrelatedExposure:  0.50,
			SoftCorrelatedExposure: 0.35,
			MaxDrawdown:            0.20,
			DrawdownWarn:           0.12,
			VetoRateAlert:          0.80,
		},
		Signals: SignalConfig{
			DefaultStrategy:   "momentum",
			MaxActive:         50,
			MaxPerTicker:      1,
			Cooldown:          30 * time.Minute,
			DefaultTTL:        4 * time.Hour,
			InvalidationSweep: 15 * time.Second,
		},
		Alerts: AlertConfig{
			DedupBucket:         time.Minute,
			Cooldown:            5 * time.Minute,
			MaxPerTickerPerHour: 12,
			ProbabilityDelta:    0.08,
			VolumeAcceleration:  2.5,
			VolatilitySpike:     2.0,
		},
		Paper: PaperConfig{
			StartingCash: 100_000,
			MaxPositions: 20,
			MaxWeight:    0.10,
			AllowShort:   false,
			Costs:        domain.DefaultCostModel(),
			FillDelay:    0,
			MarkInterval: 5 * time.Second,
		},
		News: NewsConfig{
			Enabled:         true,
			PollInterval:    30 * time.Second,
			DedupWindow:     6 * time.Hour,
			MinMateriality:  0.3,
			UseLLM:          true,
			MaxLLMPerMinute: 20,
			FixturePath:     "testdata/news",
		},
		LLM: LLMConfig{
			Provider:     "mock",
			Model:        "claude-sonnet-4-5",
			APIKeyEnv:    "ANTHROPIC_API_KEY",
			BaseURL:      "https://api.anthropic.com",
			MaxTokens:    1024,
			Temperature:  0.2,
			Timeout:      20 * time.Second,
			CacheTTL:     30 * time.Minute,
			MaxPerMinute: 60,
			FailOpen:     true,
		},
		Auth: AuthConfig{
			Enabled:       true,
			Issuer:        "quantos-local",
			Audience:      "quantos-api",
			HMACSecretEnv: "QUANTOS_JWT_SECRET",
			TokenTTL:      12 * time.Hour,
			DevUsers: []DevUser{
				{Subject: "demo", Password: "demo", Roles: []string{"viewer", "analyst"}},
				{Subject: "operator", Password: "operator", Roles: []string{"viewer", "analyst", "operator"}},
			},
		},
		Universe:   UniverseConfig{Source: "builtin", MaxSymbols: 120},
		Strategies: StrategyConfig{Dir: "strategies"},
		Evaluation: EvaluationConfig{
			SweepInterval:     30 * time.Second,
			CalibrationBins:   10,
			DriftWindow:       500,
			DriftReference:    2000,
			PSIWarn:           0.15,
			PSIAlert:          0.25,
			AccuracyDropWarn:  0.05,
			AccuracyDropAlert: 0.10,
			MinSamples:        100,
		},
	}
}

// Load reads the default configuration, overlays a YAML file when path is
// non-empty and the file exists, then applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("config: parse %s: %w", path, err)
			}
			cfg.SourcePath = path
		case os.IsNotExist(err):
			// A missing file is not an error: defaults are complete.
		default:
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	cfg.Hash = cfg.ComputeHash()
	return cfg, nil
}

// applyEnv overlays QUANTOS_-prefixed environment variables. Only the settings
// that differ between deployments are exposed this way; everything else belongs
// in the file, where it is reviewable.
func applyEnv(c *Config) {
	set := func(key string, fn func(string)) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			fn(v)
		}
	}
	set("QUANTOS_MODE", func(v string) { c.Mode = Mode(v) })
	set("QUANTOS_ENV", func(v string) { c.Env = v })
	set("QUANTOS_SERVICE", func(v string) { c.Service = v })
	set("QUANTOS_VERSION", func(v string) { c.Version = v })
	set("QUANTOS_HTTP_ADDR", func(v string) { c.HTTP.Addr = v })
	set("QUANTOS_METRICS_ADDR", func(v string) { c.Telemetry.MetricsAddr = v })
	set("QUANTOS_LOG_LEVEL", func(v string) { c.Log.Level = v })
	set("QUANTOS_LOG_FORMAT", func(v string) { c.Log.Format = v })
	set("QUANTOS_OTLP_ENDPOINT", func(v string) { c.Telemetry.OTLPEndpoint = v })
	set("QUANTOS_BUS_DRIVER", func(v string) { c.Bus.Driver = v })
	set("QUANTOS_BUS_DIR", func(v string) { c.Bus.Dir = v })
	set("QUANTOS_KAFKA_BROKERS", func(v string) { c.Bus.Brokers = splitList(v) })
	set("QUANTOS_STORE_DRIVER", func(v string) { c.Store.Driver = v })
	set("QUANTOS_POSTGRES_DSN", func(v string) { c.Store.PostgresDSN = v })
	set("QUANTOS_CLICKHOUSE_URL", func(v string) { c.Store.ClickHouseURL = v })
	set("QUANTOS_CLICKHOUSE_USER", func(v string) { c.Store.ClickHouseUser = v })
	set("QUANTOS_CLICKHOUSE_PASSWORD", func(v string) { c.Store.ClickHousePassword = v })
	set("QUANTOS_REDIS_ADDR", func(v string) { c.Store.RedisAddr = v })
	set("QUANTOS_MARKET_PROVIDER", func(v string) { c.Market.Provider = v })
	set("QUANTOS_LLM_PROVIDER", func(v string) { c.LLM.Provider = v })
	set("QUANTOS_LLM_MODEL", func(v string) { c.LLM.Model = v })
	set("QUANTOS_AUTH_ENABLED", func(v string) { c.Auth.Enabled = truthy(v) })
	set("QUANTOS_STRATEGY_DIR", func(v string) { c.Strategies.Dir = v })
	set("QUANTOS_ARTIFACT_DIR", func(v string) { c.Predict.ArtifactDir = v })
	set("QUANTOS_SEED", func(v string) {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.Market.Seed = n
		}
	})
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Validate rejects configurations that would produce unsafe behaviour. It is
// deliberately strict about the risk block: a mis-specified threshold there is
// how a veto silently becomes a suggestion.
func (c Config) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	switch c.Mode {
	case ModeEmbedded, ModeCompose, ModeCluster:
	default:
		add("mode %q is not one of embedded|compose|cluster", c.Mode)
	}
	switch c.Bus.Driver {
	case "memory", "wal", "kafka":
	default:
		add("bus.driver %q is not one of memory|wal|kafka", c.Bus.Driver)
	}
	if c.Bus.Driver == "kafka" && len(c.Bus.Brokers) == 0 {
		add("bus.driver=kafka requires bus.brokers")
	}
	if c.Bus.Driver == "wal" && c.Bus.Dir == "" {
		add("bus.driver=wal requires bus.dir")
	}
	if c.Features.Warmup > 0 && c.Features.Warmup < domain.MaxFeatureLookbackBars {
		add("features.warmup_bars is %d but the slowest feature needs %d bars of history: "+
			"every rule that references it would be skipped for ever",
			c.Features.Warmup, domain.MaxFeatureLookbackBars)
	}
	switch c.Store.Driver {
	case "memory", "sql":
	default:
		add("store.driver %q is not one of memory|sql", c.Store.Driver)
	}
	if c.Store.Driver == "sql" && c.Store.PostgresDSN == "" {
		add("store.driver=sql requires store.postgres_dsn")
	}

	if c.Risk.StalenessHard <= c.Risk.StalenessSoft {
		add("risk.staleness_hard (%s) must exceed risk.staleness_soft (%s)", c.Risk.StalenessHard, c.Risk.StalenessSoft)
	}
	if c.Risk.MaxSpreadHard <= c.Risk.MaxSpreadSoft {
		add("risk.max_spread_hard_bps must exceed risk.max_spread_soft_bps")
	}
	if c.Risk.MaxVolHard <= c.Risk.MaxVolSoft {
		add("risk.max_vol_hard must exceed risk.max_vol_soft")
	}
	if c.Risk.PreferredLiquidity < c.Risk.MinLiquidity {
		add("risk.preferred_liquidity must be >= risk.min_liquidity")
	}
	if c.Risk.EarningsWatch < c.Risk.EarningsBlock {
		add("risk.earnings_watch_window must be >= risk.earnings_block_window")
	}
	if c.Risk.MinConfidence < 0 || c.Risk.MinConfidence > 1 {
		add("risk.min_confidence must be in [0,1]")
	}
	if c.Risk.MaxDrawdown <= 0 || c.Risk.MaxDrawdown > 1 {
		add("risk.max_drawdown must be in (0,1]")
	}
	if c.Risk.DrawdownWarn >= c.Risk.MaxDrawdown {
		add("risk.drawdown_warn must be below risk.max_drawdown")
	}
	if c.Risk.MaxPositionWeight <= 0 || c.Risk.MaxPositionWeight > 1 {
		add("risk.max_position_weight must be in (0,1]")
	}
	for _, r := range c.Risk.BlockedRegimes {
		if !validRegime(r) {
			add("risk.blocked_regimes contains unknown regime %q", r)
		}
	}

	if c.Predict.BlendWeight < 0 || c.Predict.BlendWeight > 1 {
		add("predict.blend_weight must be in [0,1]")
	}
	if c.Predict.FallbackCeiling < 0 || c.Predict.FallbackCeiling > 1 {
		add("predict.fallback_confidence_ceiling must be in [0,1]")
	}
	if c.Predict.FlatBandBps <= 0 {
		add("predict.flat_band_bps must be positive: without it the outcome classes are undefined")
	}
	if len(c.Predict.Horizons) == 0 {
		add("predict.horizons must not be empty")
	}

	if c.Paper.StartingCash <= 0 {
		add("paper.starting_cash must be positive")
	}
	if c.Paper.Costs.ParticipationCap <= 0 || c.Paper.Costs.ParticipationCap > 1 {
		add("paper.costs.participation_cap must be in (0,1]")
	}
	switch c.LLM.Provider {
	case "mock", "anthropic", "":
	default:
		add("llm.provider %q is not one of mock|anthropic", c.LLM.Provider)
	}
	if c.Mode == ModeCluster && !c.Auth.Enabled {
		add("auth.enabled must be true in cluster mode")
	}
	if len(errs) > 0 {
		return fmt.Errorf("config invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func validRegime(r domain.Regime) bool {
	for _, x := range domain.AllRegimes {
		if x == r {
			return true
		}
	}
	return false
}

// ComputeHash returns a stable hash of the effective configuration. Secrets are
// never part of the hash input because they are never part of the struct: only
// the *name* of the environment variable is stored.
func (c Config) ComputeHash() string {
	c.Hash = ""
	c.SourcePath = ""
	data, err := yaml.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// Secret reads a secret from the environment variable named by key. Secrets are
// never stored in the configuration struct, never logged and never serialised.
func Secret(key string) string { return os.Getenv(key) }

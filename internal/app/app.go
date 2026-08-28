// Package app is the composition root.
//
// Every service binary is a thin main() over this package: it parses flags,
// builds an App from configuration, and runs the roles it is responsible for.
// Keeping construction in one place means the embedded demo, the compose stack
// and the cluster deployment wire the *same* objects together, so a behaviour
// that works in `make demo` is the behaviour that ships.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/alerts"
	"github.com/udaykishoreresu/quantos/internal/analyst"
	"github.com/udaykishoreresu/quantos/internal/api"
	"github.com/udaykishoreresu/quantos/internal/auth"
	"github.com/udaykishoreresu/quantos/internal/backtest"
	"github.com/udaykishoreresu/quantos/internal/brief"
	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/config"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/evaluation"
	"github.com/udaykishoreresu/quantos/internal/features"
	"github.com/udaykishoreresu/quantos/internal/fundamentals"
	"github.com/udaykishoreresu/quantos/internal/httpx"
	"github.com/udaykishoreresu/quantos/internal/llm"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
	"github.com/udaykishoreresu/quantos/internal/mlinfer"
	"github.com/udaykishoreresu/quantos/internal/news"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/paper"
	"github.com/udaykishoreresu/quantos/internal/pipeline"
	"github.com/udaykishoreresu/quantos/internal/predict"
	"github.com/udaykishoreresu/quantos/internal/regime"
	"github.com/udaykishoreresu/quantos/internal/relationship"
	"github.com/udaykishoreresu/quantos/internal/risk"
	"github.com/udaykishoreresu/quantos/internal/rules"
	"github.com/udaykishoreresu/quantos/internal/signal"
	"github.com/udaykishoreresu/quantos/internal/store"
	"github.com/udaykishoreresu/quantos/internal/universe"
)

// App holds every constructed component.
type App struct {
	Cfg     config.Config
	Log     *slog.Logger
	Metrics *obs.Metrics
	Tracer  *obs.Tracer
	Clock   obs.Clock

	Universe  *universe.Universe
	Sim       *marketdata.Sim
	Provider  marketdata.Provider
	Validator *marketdata.Validator

	Features     *features.Engine
	Regime       *regime.Engine
	Relationship *relationship.Engine
	Rules        *rules.Engine
	Strategies   *rules.Registry
	Models       *mlinfer.Registry
	Predict      *predict.Engine
	Risk         *risk.Engine
	Signals      *signal.Engine
	Alerts       *alerts.Engine
	Analyst      *analyst.Analyst
	LLM          llm.Provider
	News         *news.Pipeline
	NewsSource   *news.SyntheticSource
	Broker       *paper.Broker
	Evaluator    *evaluation.Scheduler
	Drift        *evaluation.Detector
	Fundamentals fundamentals.Store

	Bus      bus.Bus
	Store    store.Store
	SQL      *store.SQL
	Deduper  bus.Deduper
	Pipeline *pipeline.Pipeline
	State    *api.State
	Hub      *httpx.Hub
	Auth     *auth.Authenticator
	Brief    *brief.Generator
	Backtest *backtest.Engine
	Runner   *api.BacktestRunner
	Server   *httpx.Server
	API      *api.API

	closers []func() error
	mu      sync.Mutex
	// degraded records subsystems that could not be constructed or have failed.
	degraded map[string]bool
	// explain enables the AI narrative on emitted signals. It gates a step that
	// runs after the decision is final; it can never change one.
	explain bool
	// modelHealth is refreshed by the evaluation loop.
	health domain.ModelHealth
	drift  domain.DriftReport
}

// Options tune construction for a particular binary.
type Options struct {
	// Clock lets a test or the demo runner drive time.
	Clock obs.Clock
	// Now seeds the simulator's start time.
	Now time.Time
	// WithHTTP builds the API server. Consumer-only services set it false.
	WithHTTP bool
	// TradePaper enables paper order submission from generated signals.
	TradePaper bool
	// Explain enables the AI analyst on generated signals.
	Explain bool
}

// New constructs the application graph.
//
// Construction is fail-soft for optional subsystems and fail-fast for the ones
// whose absence would make output wrong: a missing strategy file is fatal, a
// missing model artifact is a degradation to the rule prior.
func New(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
	if opts.Clock == nil {
		opts.Clock = obs.SystemClock{}
	}
	if opts.Now.IsZero() {
		opts.Now = opts.Clock.Now()
	}
	log := obs.NewLogger(obs.LogConfig{
		Level: cfg.Log.Level, Format: cfg.Log.Format,
		Service: cfg.Service, Version: cfg.Version, Env: cfg.Env,
	})
	metrics := obs.NewMetrics()
	tracer := obs.NewTracer(obs.TracerConfig{
		Service: cfg.Service, Version: cfg.Version, Env: cfg.Env,
		Endpoint: cfg.Telemetry.OTLPEndpoint, FlushEvery: cfg.Telemetry.FlushEvery,
		Clock: opts.Clock,
	})

	a := &App{
		Cfg: cfg, Log: log, Metrics: metrics, Tracer: tracer, Clock: opts.Clock,
		degraded: map[string]bool{}, State: api.NewState(),
	}
	a.closers = append(a.closers, func() error { tracer.Close(); return nil })

	// --- Universe ----------------------------------------------------------
	u := universe.Builtin()
	if cfg.Universe.Source == "file" && cfg.Universe.Path != "" {
		loaded, err := universe.LoadFile(cfg.Universe.Path)
		if err != nil {
			return nil, fmt.Errorf("app: load universe: %w", err)
		}
		u = loaded
	}
	a.Universe = u.Apply(cfg.Universe.Include, cfg.Universe.Exclude, cfg.Universe.MaxSymbols)
	// Market-context symbols must always be present, whatever the filter did:
	// without them there is no regime and no relative strength.
	for _, t := range universe.MacroTickers() {
		if s, ok := u.Get(t); ok {
			a.Universe.Add(s)
		}
	}
	for _, t := range universe.SectorETFs() {
		if s, ok := u.Get(t); ok {
			a.Universe.Add(s)
		}
	}
	log.Info("universe loaded", "instruments", a.Universe.Len(), "sectors", len(a.Universe.Sectors()))

	// --- Strategies (fatal on error) --------------------------------------
	reg, err := rules.LoadDir(cfg.Strategies.Dir)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a.Strategies = reg
	log.Info("strategies loaded", "count", reg.Len(), "dir", cfg.Strategies.Dir)

	// --- Market data -------------------------------------------------------
	switch cfg.Market.Provider {
	case "replay":
		rp := marketdata.NewReplay(cfg.Market.ReplayDir, cfg.Market.BarInterval)
		if err := rp.Load(); err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
		a.Provider = rp
	default:
		// The simulated tick interval must match the interval the ingest loop
		// drives it at, or simulated and real time diverge; see Sim.AdvanceTo.
		simStart := opts.Now
		if simStart.IsZero() {
			simStart = opts.Clock.Now()
		}
		simCfg := marketdata.DefaultSimConfig(cfg.Market.Seed, simStart, a.Universe.All())
		if cfg.Market.TickInterval > 0 {
			simCfg.TickInterval = cfg.Market.TickInterval
		}
		sim := marketdata.NewSim(simCfg)
		a.Sim, a.Provider = sim, sim
	}
	a.Validator = marketdata.NewValidator(a.Provider, marketdata.ValidatorConfig{
		MaxGapPct: cfg.Market.MaxGapPct, MaxSpreadBps: cfg.Market.MaxSpreadBps,
		StalenessSoft: cfg.Market.StalenessSoft, StalenessHard: cfg.Market.StalenessHard,
		Clock: opts.Clock, Metrics: metrics,
		OnReject: func(rej marketdata.Rejection) {
			log.Warn("market data rejected", "ticker", rej.Ticker, "kind", rej.Kind,
				"reason", rej.Reason, "detail", rej.Detail)
		},
	})

	// --- Engines -----------------------------------------------------------
	a.Features = features.NewEngine(features.Config{
		Interval: cfg.Features.Interval, Warmup: cfg.Features.Warmup,
		MaxHistory: cfg.Features.MaxHistory, Benchmark: cfg.Features.Benchmark,
		VWAPResetDaily: cfg.Features.VWAPResetDaily,
		StalenessSoft:  cfg.Market.StalenessSoft, StalenessHard: cfg.Market.StalenessHard,
		Clock: opts.Clock, Metrics: metrics,
	})
	a.Regime = regime.NewEngine(regime.Config{
		Benchmarks: cfg.Regime.Benchmarks, VolatilityRef: cfg.Regime.VolatilityRef,
		RatesRef: "TLT", DollarRef: "DXY", CreditRef: "HYG",
		MinConfidence: cfg.Regime.MinConfidence, Hysteresis: cfg.Regime.Hysteresis,
		VIXHigh: cfg.Regime.VIXHigh, VIXLow: cfg.Regime.VIXLow,
		TrendThreshold: cfg.Regime.TrendThreshold, BreadthThreshold: cfg.Regime.BreadthThreshold,
		Clock: opts.Clock,
	})
	relCfg := relationship.DefaultConfig()
	relCfg.SectorETFs = universe.SectorETFs()
	a.Relationship = relationship.NewEngine(relCfg)
	a.Rules = rules.NewEngine()

	// --- Models ------------------------------------------------------------
	a.Models = mlinfer.NewRegistry()
	if cfg.Predict.ArtifactDir != "" {
		if _, err := os.Stat(cfg.Predict.ArtifactDir); err == nil {
			r, errs := mlinfer.LoadDir(cfg.Predict.ArtifactDir)
			for _, e := range errs {
				log.Warn("model artifact could not be loaded", "error", e)
				metrics.ModelLoadFailures.Inc(cfg.Predict.ModelID, "load_error")
			}
			a.Models = r
		}
	}
	if len(a.Models.All()) == 0 {
		a.markDegraded("model")
		log.Warn("no model artifacts loaded; predictions will use the deterministic rule prior",
			"artifact_dir", cfg.Predict.ArtifactDir)
	} else if m, stage, ok := a.Models.Select(cfg.Predict.ModelID, ""); ok {
		// Seed provisional health from the artifact's own held-out evaluation.
		// Without this a freshly promoted model is unmeasured, the risk engine
		// refuses to act on it, no prediction ever resolves, and it stays
		// unmeasured for ever. The record is replaced by live evidence at the
		// first evaluation sweep.
		a.health = m.Artifact().ProvisionalHealth(stage, opts.Clock.Now())
		log.Info("model serving on its offline evaluation",
			"model", m.ID(), "accuracy", a.health.Accuracy,
			"base_rate", m.Artifact().Metadata.TestBaseRate, "healthy", a.health.Healthy)
	}
	a.Predict = predict.NewEngine(predict.Config{
		ModelID: cfg.Predict.ModelID, Horizons: cfg.Predict.Horizons,
		FlatBandBps: cfg.Predict.FlatBandBps, BlendWeight: cfg.Predict.BlendWeight,
		BlendReferenceLift:      cfg.Predict.BlendReferenceLift,
		DirectionalReferenceAUC: cfg.Predict.DirectionalReferenceAUC,
		FallbackCeiling:         cfg.Predict.FallbackCeiling, Clock: opts.Clock, Metrics: metrics,
	}, a.Models)

	a.Risk = risk.NewEngine(risk.Config{
		StalenessSoft: cfg.Risk.StalenessSoft, StalenessHard: cfg.Risk.StalenessHard,
		MinLiquidity: cfg.Risk.MinLiquidity, PreferredLiquidity: cfg.Risk.PreferredLiquidity,
		MaxSpreadSoft: cfg.Risk.MaxSpreadSoft, MaxSpreadHard: cfg.Risk.MaxSpreadHard,
		MaxVolSoft: cfg.Risk.MaxVolSoft, MaxVolHard: cfg.Risk.MaxVolHard,
		EarningsWatch: cfg.Risk.EarningsWatch, EarningsBlock: cfg.Risk.EarningsBlock,
		MinConfidence: cfg.Risk.MinConfidence, MinRegimeConfidence: cfg.Risk.MinRegimeConfidence,
		BlockedRegimes:    cfg.Risk.BlockedRegimes,
		MaxPositionWeight: cfg.Risk.MaxPositionWeight, SoftPositionWeight: cfg.Risk.SoftPositionWeight,
		MaxSectorWeight: cfg.Risk.MaxSectorWeight, SoftSectorWeight: cfg.Risk.SoftSectorWeight,
		MaxCorrelatedExposure: cfg.Risk.MaxCorrelatedExposure, SoftCorrelatedExposure: cfg.Risk.SoftCorrelatedExposure,
		MaxDrawdown: cfg.Risk.MaxDrawdown, DrawdownWarn: cfg.Risk.DrawdownWarn,
		ConfigHash: cfg.Hash, Clock: opts.Clock, Metrics: metrics,
	})
	a.Signals = signal.NewEngine(signal.Config{
		MaxActive: cfg.Signals.MaxActive, MaxPerTicker: cfg.Signals.MaxPerTicker,
		Cooldown: cfg.Signals.Cooldown, DefaultTTL: cfg.Signals.DefaultTTL,
		Clock: opts.Clock, Metrics: metrics,
	})
	a.Alerts = alerts.NewEngine(alerts.Config{
		DedupBucket: cfg.Alerts.DedupBucket, Cooldown: cfg.Alerts.Cooldown,
		MaxPerTickerPerHour: cfg.Alerts.MaxPerTickerPerHour,
		ProbabilityDelta:    cfg.Alerts.ProbabilityDelta,
		VolumeAcceleration:  cfg.Alerts.VolumeAcceleration,
		VolatilitySpike:     cfg.Alerts.VolatilitySpike,
		Clock:               opts.Clock, Metrics: metrics,
	})

	// --- LLM and analyst ---------------------------------------------------
	a.LLM = a.buildLLM(cfg, opts.Clock, metrics, log)
	a.Analyst = analyst.New(analyst.Config{
		MaxTokens: cfg.LLM.MaxTokens, Temperature: cfg.LLM.Temperature,
		Timeout: cfg.LLM.Timeout, Clock: opts.Clock, Metrics: metrics,
		StrictGrounding: true,
	}, a.LLM)
	a.Brief = brief.New(a.Analyst)

	// --- News --------------------------------------------------------------
	a.News = news.NewPipeline(news.Config{
		DedupWindow: cfg.News.DedupWindow, MinMateriality: cfg.News.MinMateriality,
		UseLLM: cfg.News.UseLLM, Clock: opts.Clock, Metrics: metrics,
	}, a.LLM)
	if cfg.News.Enabled {
		a.NewsSource = news.NewSyntheticSource(cfg.Market.Seed, a.Universe.All(), 0.4, opts.Clock.Now)
	}

	// --- Paper broker ------------------------------------------------------
	a.Broker = paper.NewBroker(paper.Config{
		StartingCash: cfg.Paper.StartingCash, MaxPositions: cfg.Paper.MaxPositions,
		MaxWeight: cfg.Paper.MaxWeight, AllowShort: cfg.Paper.AllowShort,
		Costs: cfg.Paper.Costs, FillDelay: cfg.Paper.FillDelay,
		Clock: opts.Clock, Metrics: metrics,
	}, "paper-default", "QuantOS Paper Portfolio")
	for _, s := range a.Universe.All() {
		a.Broker.SetSector(s.Ticker, s.Sector)
	}

	// --- Evaluation --------------------------------------------------------
	a.Evaluator = evaluation.NewScheduler(opts.Clock, metrics, 100_000)
	driftCfg := evaluation.DefaultDriftConfig()
	driftCfg.Window = cfg.Evaluation.DriftWindow
	driftCfg.Reference = cfg.Evaluation.DriftReference
	driftCfg.PSIWarn, driftCfg.PSIAlert = cfg.Evaluation.PSIWarn, cfg.Evaluation.PSIAlert
	driftCfg.AccuracyDropWarn = cfg.Evaluation.AccuracyDropWarn
	driftCfg.AccuracyDropAlert = cfg.Evaluation.AccuracyDropAlert
	driftCfg.MinSamples = cfg.Evaluation.MinSamples
	driftCfg.Bins = cfg.Evaluation.CalibrationBins
	driftCfg.Clock, driftCfg.Metrics = opts.Clock, metrics
	a.Drift = evaluation.NewDetector(driftCfg)

	a.Fundamentals = fundamentals.NewSyntheticStore(cfg.Market.Seed, opts.Now)

	// --- Store and bus -----------------------------------------------------
	if err := a.buildStore(ctx, cfg, log); err != nil {
		return nil, err
	}
	if err := a.buildBus(cfg, metrics, log); err != nil {
		return nil, err
	}

	// --- Pipeline ----------------------------------------------------------
	a.Pipeline = pipeline.New(pipeline.Deps{
		Universe: a.Universe, Features: a.Features, Regime: a.Regime,
		Relationship: a.Relationship, Rules: a.Rules, Strategies: a.Strategies,
		Predict: a.Predict, Risk: a.Risk, Signals: a.Signals, Alerts: a.Alerts,
		Broker: a.Broker, Fundamentals: a.Fundamentals,
		Clock: opts.Clock, Metrics: metrics,
	}, pipeline.Options{
		StrategyID:   cfg.Signals.DefaultStrategy,
		MacroTickers: universe.MacroTickers(),
		TradePaper:   opts.TradePaper,
	})
	// The analyst runs outside the pipeline, on finished results only.
	a.explain = opts.Explain

	// Publish the provisional health computed at model load, now that the
	// pipeline exists to receive it.
	if a.health.ModelVersion != "" {
		a.Pipeline.SetModelHealth(&a.health, nil)
		a.State.SetModelHealth(a.health, domain.DriftReport{})
	}

	// --- Backtesting -------------------------------------------------------
	a.Backtest = backtest.NewEngine(a.newBacktestPipeline)
	a.Runner = api.NewBacktestRunner(api.RunnerConfig{
		Engine: a.Backtest, Store: a.Store, Bars: a.historicalBars,
		Clock: opts.Clock, Logger: log, Concurrency: 2,
		CodeVersion: cfg.Version, ConfigHash: cfg.Hash,
	})

	// --- Auth and HTTP -----------------------------------------------------
	authCfg := auth.Config{
		Enabled: cfg.Auth.Enabled, Issuer: cfg.Auth.Issuer, Audience: cfg.Auth.Audience,
		JWKSURL: cfg.Auth.JWKSURL, TokenTTL: cfg.Auth.TokenTTL,
		HMACSecret: config.Secret(cfg.Auth.HMACSecretEnv),
	}
	if authCfg.HMACSecret == "" && cfg.Mode != config.ModeCluster {
		// Development convenience: derive a stable per-run secret so the local
		// stack works without ceremony. Cluster mode refuses to do this.
		authCfg.HMACSecret = "quantos-local-development-secret-" + cfg.Hash
		log.Warn("no JWT secret configured; generated a development secret",
			"env", cfg.Auth.HMACSecretEnv, "mode", cfg.Mode)
	}
	for _, u := range cfg.Auth.DevUsers {
		authCfg.Users = append(authCfg.Users, auth.User{Subject: u.Subject, Password: u.Password, Roles: u.Roles})
	}
	authn, err := auth.New(authCfg)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a.Auth = authn

	a.Hub = httpx.NewHub(httpx.HubConfig{
		ReplaySize: cfg.HTTP.SSEReplay, ClientBuffer: cfg.HTTP.SSEBuffer,
		Clock: opts.Clock, Metrics: metrics,
	})

	if opts.WithHTTP {
		a.buildHTTP(cfg, log, metrics)
	}
	return a, nil
}

func (a *App) buildLLM(cfg config.Config, clock obs.Clock, metrics *obs.Metrics, log *slog.Logger) llm.Provider {
	var provider llm.Provider
	switch cfg.LLM.Provider {
	case "anthropic":
		key := config.Secret(cfg.LLM.APIKeyEnv)
		if key == "" {
			log.Warn("anthropic provider selected but no API key is set; using the deterministic mock",
				"env", cfg.LLM.APIKeyEnv)
			a.markDegraded("llm")
			provider = llm.NewMock(clock)
		} else {
			provider = llm.NewAnthropic(llm.AnthropicConfig{
				APIKey: key, Model: cfg.LLM.Model, BaseURL: cfg.LLM.BaseURL, Timeout: cfg.LLM.Timeout,
			})
		}
	default:
		provider = llm.NewMock(clock)
	}
	provider = llm.NewBreaker(provider, 5, time.Minute, clock)
	return llm.NewCached(provider, cfg.LLM.CacheTTL, cfg.LLM.MaxPerMinute, clock, metrics)
}

func (a *App) buildStore(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.Store.Driver != "sql" {
		a.Store = store.NewMemory()
		return nil
	}
	var pg *store.Postgres
	var ch *store.ClickHouse
	var cache *store.Cache

	if cfg.Store.PostgresDSN != "" {
		p, err := store.OpenPostgres(ctx, store.PostgresConfig{
			DSN: cfg.Store.PostgresDSN, MaxOpenConns: cfg.Store.MaxOpenConns,
			MaxIdleConns: cfg.Store.MaxIdleConns, ConnTimeout: cfg.Store.ConnTimeout,
		})
		if err != nil {
			// PostgreSQL is where provenance lives. Starting without it is
			// allowed — the platform degrades to read-only — but it is loud.
			log.Error("postgres unavailable; signal emission will be disabled until it returns", "error", err)
			a.markDegraded("postgres")
		} else {
			pg = p
			a.closers = append(a.closers, p.Close)
		}
	}
	if cfg.Store.ClickHouseURL != "" {
		c, err := store.OpenClickHouse(ctx, store.ClickHouseConfig{
			URL: cfg.Store.ClickHouseURL, Database: cfg.Store.ClickHouseDB,
			User: cfg.Store.ClickHouseUser, Password: cfg.Store.ClickHousePassword,
		})
		if err != nil {
			log.Warn("clickhouse unavailable; analytics will be buffered", "error", err)
			a.markDegraded("clickhouse")
		} else {
			ch = c
			a.closers = append(a.closers, c.Close)
		}
	}
	if cfg.Store.RedisAddr != "" {
		rc, err := store.OpenCache(ctx, store.CacheConfig{
			Addr: cfg.Store.RedisAddr, DB: cfg.Store.RedisDB, TTL: 5 * time.Minute,
		})
		if err != nil {
			log.Warn("redis unavailable; falling through to the source of truth", "error", err)
			a.markDegraded("redis")
		} else {
			cache = rc
			a.closers = append(a.closers, rc.Close)
		}
	}
	sqlStore := store.NewSQL(store.SQLConfig{Postgres: pg, ClickHouse: ch, Cache: cache, Logger: log})
	a.SQL, a.Store = sqlStore, sqlStore
	if cache != nil {
		a.Deduper = cache
	}
	return nil
}

func (a *App) buildBus(cfg config.Config, metrics *obs.Metrics, log *slog.Logger) error {
	if a.Deduper == nil {
		a.Deduper = bus.NewMemoryDeduper(a.Clock, 1<<20)
	}
	switch cfg.Bus.Driver {
	case "wal":
		b, err := bus.NewWALBus(bus.WALOptions{
			Dir: cfg.Bus.Dir, Metrics: metrics, Clock: a.Clock,
			MaxRetries: cfg.Bus.MaxRetries,
			OnError: func(topic, group string, e bus.Envelope, err error) {
				log.Error("event handler failed", "topic", topic, "group", group,
					"event_id", e.EventID, "error", err)
			},
		})
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
		a.Bus = b
		a.closers = append(a.closers, b.Close)
	case "kafka":
		// The Kafka driver lives in drivers/kafka as a separate module so the
		// core build has no broker dependency. Building it in is a deliberate
		// deployment step, documented in drivers/kafka/README.md.
		return errors.New("app: bus.driver=kafka requires the drivers/kafka module; " +
			"run `make kafka-driver` or use bus.driver=wal")
	default:
		b := bus.NewMemoryBus(bus.MemoryOptions{
			Buffer: cfg.Bus.Buffer, Metrics: metrics, MaxRetries: cfg.Bus.MaxRetries,
			Synchronous: cfg.Bus.Synchronous,
			OnError: func(topic, group string, e bus.Envelope, err error) {
				log.Error("event handler failed", "topic", topic, "group", group,
					"event_id", e.EventID, "error", err)
			},
		})
		a.Bus = b
		a.closers = append(a.closers, b.Close)
	}
	return nil
}

func (a *App) buildHTTP(cfg config.Config, log *slog.Logger, metrics *obs.Metrics) {
	srv := httpx.NewServer(httpx.ServerConfig{
		Addr: cfg.HTTP.Addr, ReadTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout, IdleTimeout: cfg.HTTP.IdleTimeout,
		ShutdownTimeout: cfg.HTTP.ShutdownTimeout, MaxBodyBytes: cfg.HTTP.MaxBodyBytes,
		CORSOrigins: cfg.HTTP.CORSOrigins, Version: cfg.Version,
		Metrics: metrics, Clock: a.Clock,
	})
	r := srv.Router()
	r.Use(httpx.Correlation(a.Tracer))
	r.Use(httpx.SecurityHeaders)
	r.Use(httpx.CORS(cfg.HTTP.CORSOrigins))
	r.Use(httpx.Logging(log))
	r.Use(httpx.Metrics(metrics, nil))
	r.Use(httpx.MaxBody(cfg.HTTP.MaxBodyBytes))

	var limiter httpx.Limiter = httpx.NewLocalLimiter(a.Clock)
	if c, ok := a.Deduper.(*store.Cache); ok {
		limiter = c
	}
	// The limiter is handed to the API rather than mounted here. Mounted at the
	// root it would run before authentication, see no principal, and quietly
	// key every request on the client address — which puts every user behind a
	// load balancer in one bucket and lets one caller exhaust it for everyone.
	// The API mounts it inside the authenticated group, where the principal
	// exists.
	r.Use(httpx.Audit(a.Store, a.Clock))

	a.API = api.New(api.Deps{
		State: a.State, Store: a.Store, Universe: a.Universe, Signals: a.Signals,
		Broker: a.Broker, Strategies: a.Strategies, Models: a.Models,
		Evaluator: a.Evaluator, Brief: a.Brief, Hub: a.Hub, Auth: a.Auth,
		Backtests: a.Runner, Clock: a.Clock, Metrics: metrics, Logger: log,
		Version: cfg.Version, Mode: string(cfg.Mode),
		Limiter: limiter, RateLimitRPS: cfg.HTTP.RateLimitRPS, RateLimitBurst: cfg.HTTP.RateLimitBurst,
		Degraded: a.Degraded,
		Ready:    a.ready,
	})
	a.API.Mount(r)
	a.Server = srv
}

// newBacktestPipeline builds an isolated pipeline for a backtest run. Fresh
// engines per run is what stops one run's warm indicators leaking into the next.
func (a *App) newBacktestPipeline(clock *obs.SimClock, broker *paper.Broker) *pipeline.Pipeline {
	feat := features.NewEngine(features.Config{
		Interval: a.Cfg.Features.Interval, Warmup: a.Cfg.Features.Warmup,
		MaxHistory: a.Cfg.Features.MaxHistory, Benchmark: a.Cfg.Features.Benchmark,
		VWAPResetDaily: a.Cfg.Features.VWAPResetDaily, Clock: clock,
	})
	rg := regime.NewEngine(regime.Config{
		Benchmarks: a.Cfg.Regime.Benchmarks, VolatilityRef: a.Cfg.Regime.VolatilityRef,
		RatesRef: "TLT", DollarRef: "DXY", CreditRef: "HYG",
		MinConfidence: a.Cfg.Regime.MinConfidence, Hysteresis: a.Cfg.Regime.Hysteresis,
		VIXHigh: a.Cfg.Regime.VIXHigh, VIXLow: a.Cfg.Regime.VIXLow,
		TrendThreshold: a.Cfg.Regime.TrendThreshold, BreadthThreshold: a.Cfg.Regime.BreadthThreshold,
		Clock: clock,
	})
	relCfg := relationship.DefaultConfig()
	relCfg.SectorETFs = universe.SectorETFs()

	riskCfg := risk.DefaultConfig()
	riskCfg.Clock = clock
	riskCfg.ConfigHash = a.Cfg.Hash

	return pipeline.New(pipeline.Deps{
		Universe: a.Universe, Features: feat, Regime: rg,
		Relationship: relationship.NewEngine(relCfg), Rules: rules.NewEngine(),
		Strategies: a.Strategies,
		Predict: predict.NewEngine(predict.Config{
			ModelID: a.Cfg.Predict.ModelID, Horizons: a.Cfg.Predict.Horizons,
			FlatBandBps: a.Cfg.Predict.FlatBandBps, BlendWeight: a.Cfg.Predict.BlendWeight,
			FallbackCeiling: a.Cfg.Predict.FallbackCeiling, Clock: clock,
		}, a.Models),
		Risk: risk.NewEngine(riskCfg),
		Signals: signal.NewEngine(signal.Config{
			MaxActive: a.Cfg.Signals.MaxActive, MaxPerTicker: a.Cfg.Signals.MaxPerTicker,
			Cooldown: a.Cfg.Signals.Cooldown, DefaultTTL: a.Cfg.Signals.DefaultTTL, Clock: clock,
		}),
		Alerts:       alerts.NewEngine(alerts.Config{Clock: clock}),
		Broker:       broker,
		Fundamentals: a.Fundamentals,
		Clock:        clock,
	}, pipeline.Options{
		StrategyID:   a.Cfg.Signals.DefaultStrategy,
		MacroTickers: universe.MacroTickers(),
		TradePaper:   true,
	})
}

// historicalBars supplies bars to a backtest, from the store when available and
// from the simulator otherwise.
func (a *App) historicalBars(ctx context.Context, tickers []domain.Ticker, start, end time.Time, interval domain.Interval) ([]domain.Candle, error) {
	if len(tickers) == 0 {
		for _, s := range a.Universe.All() {
			tickers = append(tickers, s.Ticker)
		}
	}
	sort.Slice(tickers, func(i, j int) bool { return tickers[i] < tickers[j] })

	var out []domain.Candle
	if a.Store != nil {
		for _, t := range tickers {
			bars, err := a.Store.ListCandles(ctx, store.Query{Ticker: t, From: start, To: end, Limit: 5000}, interval)
			if err == nil {
				out = append(out, bars...)
			}
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	if a.Provider == nil {
		return nil, errors.New("no historical data source available")
	}
	return a.Provider.GetHistoricalBars(ctx, marketdata.BarRequest{
		Tickers: tickers, Interval: interval, Start: start, End: end, Limit: 5000, Adjusted: true,
	})
}

func (a *App) markDegraded(name string) {
	a.mu.Lock()
	a.degraded[name] = true
	a.mu.Unlock()
	if a.Metrics != nil {
		a.Metrics.DegradedComponents.Set(1, name)
	}
}

// ClearDegraded marks a subsystem healthy again.
func (a *App) ClearDegraded(name string) {
	a.mu.Lock()
	delete(a.degraded, name)
	a.mu.Unlock()
	if a.Metrics != nil {
		a.Metrics.DegradedComponents.Set(0, name)
	}
}

// Degraded lists currently-degraded subsystems.
func (a *App) Degraded() []string {
	a.mu.Lock()
	out := make([]string, 0, len(a.degraded))
	for k := range a.degraded {
		out = append(out, k)
	}
	a.mu.Unlock()
	if a.SQL != nil {
		out = append(out, a.SQL.Degraded()...)
	}
	sort.Strings(out)
	return dedupeStrings(out)
}

// CanEmitSignals reports whether provenance can be persisted. When it cannot,
// the platform stops emitting rather than emitting something it could not later
// explain (ADR-003).
func (a *App) CanEmitSignals() bool {
	if a.SQL == nil {
		return true // in-memory store is always writable
	}
	return a.SQL.CanEmitSignals()
}

func (a *App) ready() (bool, string) {
	if a.Universe.Len() == 0 {
		return false, "universe is empty"
	}
	if a.Strategies.Len() == 0 {
		return false, "no strategies loaded"
	}
	if !a.CanEmitSignals() {
		return false, "provenance store is unavailable; signal emission is suspended"
	}
	return true, ""
}

// SetModelHealth records the current health, feeding the risk engine and the API.
func (a *App) SetModelHealth(h domain.ModelHealth, d domain.DriftReport) {
	a.mu.Lock()
	a.health, a.drift = h, d
	a.mu.Unlock()
	a.Pipeline.SetModelHealth(&h, &d)
	a.State.SetModelHealth(h, d)
}

// ModelHealth returns the recorded health.
func (a *App) ModelHealth() (domain.ModelHealth, domain.DriftReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.health, a.drift
}

// Close releases every resource in reverse construction order.
func (a *App) Close() error {
	var firstErr error
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ArtifactPath resolves a model artifact path relative to the configured dir.
func (a *App) ArtifactPath(name string) string {
	return filepath.Join(a.Cfg.Predict.ArtifactDir, name)
}

func dedupeStrings(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0]
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

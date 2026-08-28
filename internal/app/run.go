package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/analyst"
	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/evaluation"
	"github.com/udaykishoreresu/quantos/internal/httpx"
	"github.com/udaykishoreresu/quantos/internal/news"
	"github.com/udaykishoreresu/quantos/internal/pipeline"
	"github.com/udaykishoreresu/quantos/internal/signal"
)

// Role names one responsibility a binary can take on. A single-process
// deployment runs them all; the compose and cluster deployments split them
// across services, which is the only difference between the topologies.
type Role string

const (
	RoleIngest     Role = "ingest"     // market data producer
	RoleEngine     Role = "engine"     // features → … → signals
	RoleNews       Role = "news"       // news pipeline
	RoleEvaluation Role = "evaluation" // outcome scoring and drift
	RoleAPI        Role = "api"        // HTTP edge
	RoleSweeper    Role = "sweeper"    // signal invalidation sweep
)

// AllRoles is the single-process configuration.
var AllRoles = []Role{RoleIngest, RoleEngine, RoleNews, RoleEvaluation, RoleAPI, RoleSweeper}

// Run starts the requested roles and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context, roles ...Role) error {
	if len(roles) == 0 {
		roles = AllRoles
	}
	want := map[Role]bool{}
	for _, r := range roles {
		want[r] = true
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(roles)+2)

	// Consumers must be registered before the bus starts.
	if want[RoleEngine] {
		if err := a.registerEngineConsumers(); err != nil {
			return err
		}
	}
	if want[RoleAPI] {
		if err := a.registerStreamConsumers(); err != nil {
			return err
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.Bus.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("bus: %w", err)
		}
	}()

	if want[RoleIngest] {
		wg.Add(1)
		go func() { defer wg.Done(); a.runIngest(ctx) }()
	}
	if want[RoleNews] && a.NewsSource != nil {
		wg.Add(1)
		go func() { defer wg.Done(); a.runNews(ctx) }()
	}
	if want[RoleEvaluation] {
		wg.Add(1)
		go func() { defer wg.Done(); a.runEvaluation(ctx) }()
	}
	if want[RoleSweeper] {
		wg.Add(1)
		go func() { defer wg.Done(); a.runSweeper(ctx) }()
	}
	if want[RoleAPI] && a.Server != nil {
		if err := a.Server.Start(); err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		a.Log.Info("http server listening", "addr", a.Cfg.HTTP.Addr, "mode", a.Cfg.Mode)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	if a.Server != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), a.Cfg.HTTP.ShutdownTimeout)
		defer cancel()
		if err := a.Server.Shutdown(shutCtx); err != nil {
			a.Log.Error("http shutdown", "error", err)
		}
	}
	wg.Wait()
	return nil
}

// --- Ingest -----------------------------------------------------------------

// runIngest drives the market-data producer.
//
// Every tick is validated before publication, and a symbol whose data has gone
// stale publishes to market.stale so downstream consumers suspend rather than
// act on old prices (governance rule G-5).
func (a *App) runIngest(ctx context.Context) {
	builder := bus.NewBuilder(a.Cfg.Service+"/ingest", a.Clock)
	interval := a.Cfg.Market.TickInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Warm the indicators before publishing, so the first observed bar is not
	// a cold one. This is not cheating: it is the same warm-up a live process
	// performs by reading history at start-up.
	if a.Sim != nil && a.Cfg.Features.Warmup > 0 {
		// Rewind first, so the warm-up ends on the present instead of leaving
		// the feed that far in the future. See Sim.Rewind.
		a.Sim.Rewind(time.Duration(a.Cfg.Features.Warmup*60) * interval)
		warm := a.Sim.Warmup(a.Cfg.Features.Warmup * 60)
		for _, c := range warm {
			a.Pipeline.OnBar(ctx, c)
		}
		a.Log.Info("simulator warmed", "bars", len(warm))
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if a.Sim == nil {
			continue
		}
		// Slave the simulated feed to the real clock: see Sim.AdvanceTo. Only
		// the most recent quote per symbol is published, because a quote from
		// several seconds ago is not news, but every completed bar is, since
		// bars are what the decision path consumes.
		quotes, bars := a.Sim.AdvanceTo(a.Clock.Now(), 600)

		accepted := make([]domain.Quote, 0, len(quotes))
		for _, q := range quotes {
			if err := a.Validator.ValidateQuote(q); err != nil {
				a.publishRejected(ctx, builder, q.Ticker, err)
				continue
			}
			accepted = append(accepted, q)
		}
		if len(accepted) > 0 {
			a.publishQuotes(ctx, builder, accepted)
		}
		for _, c := range bars {
			if err := a.Validator.ValidateCandle(c); err != nil {
				a.publishRejected(ctx, builder, c.Ticker, err)
				continue
			}
			env, err := builder.New(ctx, bus.TopicBars, string(c.Ticker), c.End, c)
			if err != nil {
				continue
			}
			if err := a.Bus.Publish(ctx, bus.TopicBars, env); err != nil {
				a.Metrics.BusErrors.Inc(bus.TopicBars, "publish")
			}
		}
		a.checkStaleness(ctx, builder)
	}
}

func (a *App) publishQuotes(ctx context.Context, b *bus.Builder, quotes []domain.Quote) {
	envs := make([]bus.Envelope, 0, len(quotes))
	for _, q := range quotes {
		env, err := b.New(ctx, bus.TopicQuotes, string(q.Ticker), q.Timestamp, q)
		if err != nil {
			continue
		}
		envs = append(envs, env)
	}
	if err := a.Bus.PublishBatch(ctx, bus.TopicQuotes, envs); err != nil {
		a.Metrics.BusErrors.Inc(bus.TopicQuotes, "publish")
	}
	if a.Store != nil {
		_ = a.Store.SaveQuotes(ctx, quotes)
	}
}

func (a *App) publishRejected(ctx context.Context, b *bus.Builder, t domain.Ticker, err error) {
	env, berr := b.New(ctx, bus.TopicRejected, string(t), a.Clock.Now(), map[string]string{
		"ticker": string(t), "reason": err.Error(),
	})
	if berr == nil {
		_ = a.Bus.Publish(ctx, bus.TopicRejected, env)
	}
}

// checkStaleness publishes market.stale for symbols whose data has aged out.
func (a *App) checkStaleness(ctx context.Context, b *bus.Builder) {
	for _, s := range a.Universe.All() {
		f := a.Validator.Freshness(s.Ticker)
		if !f.Stale() {
			continue
		}
		env, err := b.New(ctx, bus.TopicStale, string(s.Ticker), a.Clock.Now(), map[string]any{
			"ticker": s.Ticker, "age_seconds": f.Age.Seconds(),
			"reason": f.Reason, "hard": f.Hard,
		})
		if err == nil {
			_ = a.Bus.Publish(ctx, bus.TopicStale, env)
		}
	}
}

// --- Engine -----------------------------------------------------------------

// registerEngineConsumers wires the decision path onto the bus.
//
// Every handler is wrapped in bus.Idempotent so redelivery and replay cannot
// duplicate an effect, and in bus.MaxAgeFilter so consumer lag turns into
// silence rather than into stale action (ADR-002, ADR-010).
func (a *App) registerEngineConsumers() error {
	group := a.Cfg.Bus.GroupPrefix + ".engine"

	quoteHandler := bus.Idempotent(a.Deduper, bus.IdempotentOptions{
		Group: group, TTL: a.Cfg.Bus.DedupTTL, Metrics: a.Metrics, Topic: bus.TopicQuotes,
	}, func(ctx context.Context, e bus.Envelope) error {
		var q domain.Quote
		if err := e.Decode(&q); err != nil {
			return err
		}
		a.Pipeline.OnQuote(q)
		return nil
	})
	if err := a.Bus.Subscribe(bus.TopicQuotes, group, quoteHandler); err != nil {
		return err
	}

	barHandler := bus.MaxAgeFilter(a.Clock, a.Cfg.Bus.MaxEventAge,
		func(e bus.Envelope) {
			a.Log.Warn("dropping stale bar: consumer lag exceeds max_event_age",
				"event_id", e.EventID, "ticker", e.PartitionKey,
				"age", a.Clock.Now().Sub(e.OccurredAt).String())
		},
		bus.Idempotent(a.Deduper, bus.IdempotentOptions{
			Group: group, TTL: a.Cfg.Bus.DedupTTL, Metrics: a.Metrics, Topic: bus.TopicBars,
		}, func(ctx context.Context, e bus.Envelope) error {
			var c domain.Candle
			if err := e.Decode(&c); err != nil {
				return err
			}
			return a.onBar(ctx, e, c)
		}))
	return a.Bus.Subscribe(bus.TopicBars, group, barHandler)
}

// onBar runs the decision path for one bar and publishes everything it produced.
func (a *App) onBar(ctx context.Context, cause bus.Envelope, c domain.Candle) error {
	start := time.Now()
	res := a.Pipeline.OnBar(ctx, c)
	a.State.Observe(res)

	// The AI explanation is produced here, outside the pipeline, and strictly
	// after the decision is final. The pipeline package has no import edge to
	// the analyst or the LLM, so no narrative can influence a number; this is
	// the second half of that arrangement, and the only thing it writes back is
	// prose (ADR-005).
	a.explainSignal(ctx, res)

	builder := bus.NewBuilder(a.Cfg.Service+"/engine", a.Clock)
	emit := func(topic string, key string, at time.Time, payload any) {
		env, err := builder.Caused(ctx, cause, topic, key, at, payload)
		if err != nil {
			return
		}
		if err := a.Bus.Publish(ctx, topic, env); err != nil {
			a.Metrics.BusErrors.Inc(topic, "publish")
		}
	}

	if a.Store != nil {
		_ = a.Store.SaveCandles(ctx, []domain.Candle{c})
		_ = a.Store.SaveSnapshot(ctx, res.Snapshot)
	}
	a.Drift.ObserveFeatures(res.Snapshot)
	emit(bus.TopicFeatures, string(c.Ticker), res.Snapshot.AsOf, res.Snapshot)

	if res.Macro {
		if res.Regime.Changed || res.Regime.Regime != "" {
			if a.Store != nil {
				_ = a.Store.SaveRegime(ctx, res.Regime)
			}
			a.State.SetRelationship(a.Relationship.Latest())
			emit(bus.TopicRegime, "__MARKET__", res.Regime.AsOf, res.Regime)
		}
		return nil
	}

	if res.Prediction != nil {
		a.Drift.ObservePrediction(*res.Prediction)
		if a.Store != nil {
			_ = a.Store.SavePrediction(ctx, *res.Prediction)
		}
		decision := domain.RiskWatchOnly
		if res.Risk != nil {
			decision = res.Risk.Decision
		}
		a.Evaluator.Track(*res.Prediction, res.Snapshot.MustGet(domain.FeatLast, res.Snapshot.Last), decision)
		emit(bus.TopicPrediction, string(c.Ticker), res.Prediction.CreatedAt, res.Prediction)
	}
	if res.Risk != nil {
		if a.Store != nil {
			_ = a.Store.SaveAssessment(ctx, *res.Risk)
		}
		emit(bus.TopicRisk, string(c.Ticker), res.Risk.CreatedAt, res.Risk)
	}

	if res.Signal != nil {
		// Provenance gate: if the signal cannot be persisted, it must not be
		// published, because the platform could not later explain it.
		if !a.CanEmitSignals() {
			a.Log.Error("suppressing signal: provenance store unavailable",
				"ticker", c.Ticker, "signal_id", res.Signal.ID)
			a.Signals.Invalidate(res.Signal.ID, domain.InvalidRiskVeto,
				"provenance store unavailable at emission time", a.Clock.Now())
		} else {
			if a.Store != nil {
				if err := a.Store.SaveSignal(ctx, *res.Signal); err != nil {
					a.Log.Error("could not persist signal", "id", res.Signal.ID, "error", err)
				}
			}
			emit(bus.TopicSignal, res.Signal.ID, res.Signal.CreatedAt, res.Signal)
			a.Metrics.SignalLatency.Observe(time.Since(start).Seconds())
		}
	}
	for _, inv := range res.Invalidated {
		if a.Store != nil {
			_ = a.Store.UpdateSignal(ctx, inv)
		}
		emit(bus.TopicSignalInvalidated, inv.ID, a.Clock.Now(), inv)
	}
	for _, al := range res.Alerts {
		if a.Store != nil {
			inserted, err := a.Store.SaveAlert(ctx, al)
			if err == nil && !inserted {
				// The durable unique index caught a duplicate the in-memory
				// dedup missed, which is exactly its job.
				a.Metrics.AlertsSuppressed.Inc(string(al.Type), "durable_dedup")
				continue
			}
		}
		emit(bus.TopicAlert, al.ID, al.CreatedAt, al)
	}

	a.Metrics.MarketEventLatency.Observe(time.Since(start).Seconds(), "bar")
	a.Metrics.MarketEventsProcessed.Inc("bar", a.Provider.Name())
	return nil
}

// --- Stream -----------------------------------------------------------------

// registerStreamConsumers forwards events to connected dashboards.
func (a *App) registerStreamConsumers() error {
	group := a.Cfg.Bus.GroupPrefix + ".stream"
	sub := func(topic, name string) error {
		return a.Bus.Subscribe(topic, group, func(ctx context.Context, e bus.Envelope) error {
			var payload any
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				return nil
			}
			a.Hub.Publish(name, payload)
			return nil
		})
	}
	for _, pair := range []struct{ topic, name string }{
		{bus.TopicRegime, httpx.StreamRegime},
		{bus.TopicSignal, httpx.StreamSignal},
		{bus.TopicSignalInvalidated, httpx.StreamInvalidated},
		{bus.TopicAlert, httpx.StreamAlert},
		{bus.TopicPrediction, httpx.StreamPrediction},
	} {
		if err := sub(pair.topic, pair.name); err != nil {
			return err
		}
	}
	// Quotes are throttled: a dashboard does not need every tick of every
	// symbol, and an unthrottled fan-out is how an SSE hub becomes the
	// bottleneck.
	var lastQuoteFlush time.Time
	pending := map[domain.Ticker]domain.Quote{}
	var mu sync.Mutex
	return a.Bus.Subscribe(bus.TopicQuotes, group, func(ctx context.Context, e bus.Envelope) error {
		var q domain.Quote
		if err := e.Decode(&q); err != nil {
			return nil
		}
		mu.Lock()
		pending[q.Ticker] = q
		now := a.Clock.Now()
		if now.Sub(lastQuoteFlush) < 500*time.Millisecond {
			mu.Unlock()
			return nil
		}
		lastQuoteFlush = now
		batch := make([]domain.Quote, 0, len(pending))
		for _, v := range pending {
			batch = append(batch, v)
		}
		pending = map[domain.Ticker]domain.Quote{}
		mu.Unlock()
		a.Hub.Publish(httpx.StreamQuotes, batch)
		return nil
	})
}

// --- News -------------------------------------------------------------------

func (a *App) runNews(ctx context.Context) {
	interval := a.Cfg.News.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	builder := bus.NewBuilder(a.Cfg.Service+"/news", a.Clock)
	since := a.Clock.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		articles, err := a.NewsSource.Fetch(ctx, since)
		since = a.Clock.Now()
		if err != nil {
			a.Log.Warn("news fetch failed", "error", err)
			continue
		}
		for _, ev := range a.News.Process(ctx, articles) {
			if a.Store != nil {
				if inserted, err := a.Store.SaveNews(ctx, ev); err == nil && !inserted {
					continue
				}
			}
			a.State.AddNews(ev)
			a.Pipeline.SetNews(ev)
			env, err := builder.New(ctx, bus.TopicNews, string(ev.Ticker), ev.Timestamp, ev)
			if err == nil {
				_ = a.Bus.Publish(ctx, bus.TopicNews, env)
			}
		}
	}
}

// InjectNews queues a headline into the synthetic wire, which is how the demo
// scenarios drive a news-triggered change.
func (a *App) InjectNews(article news.Article) {
	if a.NewsSource != nil {
		a.NewsSource.Inject(article)
	}
}

// --- Evaluation -------------------------------------------------------------

// runEvaluation resolves elapsed predictions and recomputes drift.
func (a *App) runEvaluation(ctx context.Context) {
	interval := a.Cfg.Evaluation.SweepInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	builder := bus.NewBuilder(a.Cfg.Service+"/evaluation", a.Clock)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := a.Clock.Now()
		outcomes := a.Evaluator.Resolve(now, func(t domain.Ticker) (float64, bool) {
			snap, ok := a.Pipeline.Snapshot(t)
			if !ok {
				return 0, false
			}
			// A stale price is not a resolution price: scoring against it would
			// manufacture outcomes out of missing data.
			if snap.Stale {
				return 0, false
			}
			return snap.MustGet(domain.FeatLast, snap.Last), true
		})

		for _, o := range outcomes {
			a.Drift.ObserveOutcome(o)
			if a.Store != nil {
				_ = a.Store.SaveOutcome(ctx, o)
			}
			env, err := builder.New(ctx, bus.TopicEvaluated, o.PredictionID, o.ResolvedAt, o)
			if err == nil {
				_ = a.Bus.Publish(ctx, bus.TopicEvaluated, env)
			}
		}

		resolved := a.Evaluator.Resolved()
		if len(resolved) == 0 {
			continue
		}
		modelVersion := "rules-fallback"
		if models := a.Models.All(); len(models) > 0 {
			modelVersion = models[len(models)-1].Artifact().Version
		}
		overall := evaluation.Evaluate(resolved, "rolling", a.Cfg.Evaluation.CalibrationBins)
		byRegime := evaluation.ByRegime(resolved, a.Cfg.Evaluation.CalibrationBins, a.Cfg.Evaluation.MinSamples/4)
		drift := a.Drift.Report(modelVersion)
		health := evaluation.Health(a.Cfg.Predict.ModelID, modelVersion, domain.StageActive,
			overall, byRegime, drift, now)

		a.SetModelHealth(health, drift)
		if a.Store != nil {
			_ = a.Store.SaveEvaluation(ctx, overall)
			_ = a.Store.SaveDrift(ctx, drift)
		}
		if drift.Severity == domain.DriftModerate || drift.Severity == domain.DriftSevere {
			env, err := builder.New(ctx, bus.TopicDrift, modelVersion, now, drift)
			if err == nil {
				_ = a.Bus.Publish(ctx, bus.TopicDrift, env)
			}
			a.Log.Warn("model drift detected", "severity", drift.Severity,
				"recommendation", drift.Recommendation, "reasons", drift.Reasons)
		}
		a.Hub.Publish(httpx.StreamHealth, map[string]any{"health": health, "drift": drift})
	}
}

// --- Sweeper ----------------------------------------------------------------

// runSweeper re-evaluates invalidation conditions and marks the paper book.
//
// It exists separately from the bar handler because a signal must be
// invalidated by the passage of time and by a regime change, not only by its
// own instrument printing a new bar.
func (a *App) runSweeper(ctx context.Context) {
	interval := a.Cfg.Signals.InvalidationSweep
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	builder := bus.NewBuilder(a.Cfg.Service+"/sweeper", a.Clock)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := a.Clock.Now()
		prices, volumes := a.Pipeline.Marks()
		a.Broker.Mark(prices, volumes, now)
		if a.Store != nil {
			_ = a.Store.SavePortfolio(ctx, a.Broker.Portfolio())
		}
		a.Hub.Publish(httpx.StreamPortfolio, a.Broker.Portfolio())

		regimeNow := a.Pipeline.Regime()
		invalidated := a.Signals.Sweep(func(t domain.Ticker) signal.EvalInput {
			snap, _ := a.Pipeline.Snapshot(t)
			risks := a.State.Risks()
			var ra *domain.RiskAssessment
			if x, ok := risks[t]; ok {
				ra = &x
			}
			preds := a.State.Predictions()
			var pr *domain.Prediction
			if x, ok := preds[t]; ok {
				pr = &x
			}
			return signal.EvalInput{
				Snapshot: snap, Regime: regimeNow, Prediction: pr, Risk: ra, Now: now,
			}
		}, now)

		for _, inv := range invalidated {
			if a.Store != nil {
				_ = a.Store.UpdateSignal(ctx, inv)
			}
			env, err := builder.New(ctx, bus.TopicSignalInvalidated, inv.ID, now, inv)
			if err == nil {
				_ = a.Bus.Publish(ctx, bus.TopicSignalInvalidated, env)
			}
			a.Log.Info("signal invalidated", "id", inv.ID, "ticker", inv.Ticker,
				"kind", inv.InvalidationKind, "note", inv.InvalidationNote)
		}
	}
}

// StoreHealthy reports whether the persistent store is reachable.
func (a *App) StoreHealthy(ctx context.Context) bool {
	if a.Store == nil {
		return false
	}
	h := a.Store.Health(ctx)
	return len(h.Degraded) == 0
}

// PipelineResult re-exports the pipeline result type for consumers that only
// import app.
type PipelineResult = pipeline.Result

// RegisterConsumers wires the bus consumers for the given roles without
// starting any goroutines. The demo runner and the integration tests use it to
// drive the platform deterministically: publish an event, and the handlers run
// inline on the synchronous in-memory bus.
func (a *App) RegisterConsumers(roles ...Role) error {
	if len(roles) == 0 {
		roles = AllRoles
	}
	want := map[Role]bool{}
	for _, r := range roles {
		want[r] = true
	}
	if want[RoleEngine] {
		if err := a.registerEngineConsumers(); err != nil {
			return err
		}
	}
	if want[RoleAPI] && a.Hub != nil {
		if err := a.registerStreamConsumers(); err != nil {
			return err
		}
	}
	return nil
}

// PublishBar publishes one bar as if it had come from ingestion, including
// validation. It is the entry point the demo runner and the failure tests use.
func (a *App) PublishBar(ctx context.Context, c domain.Candle) error {
	if err := a.Validator.ValidateCandle(c); err != nil {
		return err
	}
	b := bus.NewBuilder(a.Cfg.Service+"/ingest", a.Clock)
	env, err := b.New(ctx, bus.TopicBars, string(c.Ticker), c.End, c)
	if err != nil {
		return err
	}
	return a.Bus.Publish(ctx, bus.TopicBars, env)
}

// PublishQuote publishes one validated quote.
func (a *App) PublishQuote(ctx context.Context, q domain.Quote) error {
	if err := a.Validator.ValidateQuote(q); err != nil {
		return err
	}
	b := bus.NewBuilder(a.Cfg.Service+"/ingest", a.Clock)
	env, err := b.New(ctx, bus.TopicQuotes, string(q.Ticker), q.Timestamp, q)
	if err != nil {
		return err
	}
	if a.Store != nil {
		_ = a.Store.SaveQuotes(ctx, []domain.Quote{q})
	}
	return a.Bus.Publish(ctx, bus.TopicQuotes, env)
}

// EvaluateOnce runs one evaluation sweep, resolving elapsed predictions and
// recomputing drift. It is the deterministic counterpart of runEvaluation.
func (a *App) EvaluateOnce(ctx context.Context, now time.Time) []domain.PredictionOutcome {
	outcomes := a.Evaluator.Resolve(now, func(t domain.Ticker) (float64, bool) {
		snap, ok := a.Pipeline.Snapshot(t)
		if !ok || snap.Stale {
			return 0, false
		}
		return snap.MustGet(domain.FeatLast, snap.Last), true
	})
	for _, o := range outcomes {
		a.Drift.ObserveOutcome(o)
		if a.Store != nil {
			_ = a.Store.SaveOutcome(ctx, o)
		}
	}
	resolved := a.Evaluator.Resolved()
	if len(resolved) == 0 {
		return outcomes
	}
	modelVersion := "rules-fallback"
	if models := a.Models.All(); len(models) > 0 {
		modelVersion = models[len(models)-1].Artifact().Version
	}
	overall := evaluation.Evaluate(resolved, "rolling", a.Cfg.Evaluation.CalibrationBins)
	byRegime := evaluation.ByRegime(resolved, a.Cfg.Evaluation.CalibrationBins, 10)
	drift := a.Drift.Report(modelVersion)
	a.SetModelHealth(evaluation.Health(a.Cfg.Predict.ModelID, modelVersion,
		domain.StageActive, overall, byRegime, drift, now), drift)
	return outcomes
}

// SweepOnce runs one invalidation sweep and marks the paper book.
func (a *App) SweepOnce(ctx context.Context, now time.Time) []domain.Signal {
	prices, volumes := a.Pipeline.Marks()
	a.Broker.Mark(prices, volumes, now)
	rg := a.Pipeline.Regime()
	invalidated := a.Signals.Sweep(func(t domain.Ticker) signal.EvalInput {
		snap, _ := a.Pipeline.Snapshot(t)
		var ra *domain.RiskAssessment
		if x, ok := a.State.Risks()[t]; ok {
			ra = &x
		}
		var pr *domain.Prediction
		if x, ok := a.State.Predictions()[t]; ok {
			pr = &x
		}
		return signal.EvalInput{Snapshot: snap, Regime: rg, Prediction: pr, Risk: ra, Now: now}
	}, now)
	for _, inv := range invalidated {
		if a.Store != nil {
			_ = a.Store.UpdateSignal(ctx, inv)
		}
	}
	return invalidated
}

// ProcessNewsOnce fetches and processes one batch of news synchronously.
func (a *App) ProcessNewsOnce(ctx context.Context, since time.Time) []domain.NewsEvent {
	if a.NewsSource == nil {
		return nil
	}
	articles, err := a.NewsSource.Fetch(ctx, since)
	if err != nil {
		return nil
	}
	events := a.News.Process(ctx, articles)
	for _, ev := range events {
		if a.Store != nil {
			_, _ = a.Store.SaveNews(ctx, ev)
		}
		a.State.AddNews(ev)
		a.Pipeline.SetNews(ev)
	}
	return events
}

// explainSignal attaches a narrative to a signal that has already been made.
//
// Everything it touches is descriptive: the signal's explanation text and the
// read model's report cache. If the analyst fails, or produces prose that fails
// grounding, the signal is unaffected — it simply carries the deterministic
// template explanation instead.
func (a *App) explainSignal(ctx context.Context, res pipeline.Result) {
	if !a.explain || a.Analyst == nil || res.Signal == nil {
		return
	}
	snap := res.Snapshot
	rep := a.Analyst.Analyze(ctx, analyst.Input{
		Stock: res.Stock, AsOf: res.Signal.CreatedAt, Regime: res.Regime,
		Score: res.Score, Prediction: res.Prediction, Opportunity: res.Opportunity,
		Risk: res.Risk, Signal: res.Signal, Snapshot: &snap,
		Valuation: res.Valuation, Evidence: res.Rules.Evidence,
		CounterEvidence: res.Rules.CounterEvidence, Events: res.Events,
	})
	res.Signal.Explanation = rep.Summary
	res.Signal.ExplanationSource = string(rep.Source)
	a.State.SetReport(res.Ticker, rep)
}

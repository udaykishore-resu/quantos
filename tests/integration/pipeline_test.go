package integration

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/config"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// start is a fixed Tuesday inside US market hours. Every test in this file
// drives a simulated clock from it, so nothing here depends on when it runs.
var start = time.Date(2026, 3, 10, 14, 0, 0, 0, time.UTC)

// newApp builds a fully wired platform on in-memory infrastructure, with the
// repository's real configuration and real strategy files.
//
// Using the shipped configuration is deliberate: a test that invents its own
// thresholds proves the code works under thresholds nobody ships.
func newApp(t *testing.T, clock obs.Clock) *app.App {
	t.Helper()
	root := repoRoot(t)
	cfg, err := config.Load(root + "/config/quantos.yaml")
	if err != nil {
		t.Fatalf("configuration: %v", err)
	}
	cfg.Strategies.Dir = root + "/strategies"
	cfg.Predict.ArtifactDir = root + "/ml/artifacts"
	cfg.Store.Driver = "memory"
	cfg.Bus.Driver = "memory"
	// Synchronous delivery: a handler runs inside Publish, so a test can assert
	// on the result of a bar immediately after publishing it instead of racing
	// a background worker.
	cfg.Bus.Synchronous = true
	cfg.LLM.Provider = "mock"
	cfg.Auth.Enabled = false
	cfg.Log.Level = "error"
	cfg.HTTP.Addr = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("configuration: %v", err)
	}
	cfg.Hash = cfg.ComputeHash()

	a, err := app.New(context.Background(), cfg, app.Options{
		Clock: clock, Now: start, WithHTTP: false, TradePaper: true,
	})
	if err != nil {
		t.Fatalf("start-up: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.RegisterConsumers(app.RoleEngine); err != nil {
		t.Fatalf("consumers: %v", err)
	}
	return a
}

// warmPlatform warms the indicators the same way the ingest loop does: rewind
// the simulator by the span the warm-up will consume, so that when it finishes
// the feed sits on the present rather than hours ahead of it.
//
// Getting this wrong is not subtle in effect and is invisible in the logs: a
// feed ahead of the clock has every tick rejected as future-dated, and the
// platform runs cleanly while producing nothing at all.
func warmPlatform(a *app.App) {
	// A little past the longest lookback, so nothing is marginal.
	const ticks = (domain.MaxFeatureLookbackBars + 30) * 60
	a.Sim.Rewind(ticks * time.Second)
	// The warm-up bars are fed straight to the pipeline rather than published:
	// they are history being replayed to prime the indicators, not events the
	// rest of the platform should react to.
	for _, c := range a.Sim.Warmup(ticks) {
		a.Pipeline.OnBar(context.Background(), c)
	}
}

// drive advances the simulator and the clock together for n ticks, publishing
// everything the simulator produces through the real event bus.
func drive(t *testing.T, a *app.App, clock *obs.SimClock, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		quotes, bars := a.Sim.Tick()
		clock.Advance(time.Second)
		// PublishQuote and PublishBar validate internally; a rejected tick is a
		// normal event on a simulated feed, not a test failure.
		for _, q := range quotes {
			_ = a.PublishQuote(ctx, q)
		}
		for _, c := range bars {
			_ = a.PublishBar(ctx, c)
		}
	}
}

// TestTheWholeDecisionPathRunsOnRealConfiguration walks a bar from the
// simulator through every stage and asserts the pipeline produced a coherent,
// fully attributed result rather than an empty one.
func TestTheWholeDecisionPathRunsOnRealConfiguration(t *testing.T) {
	clock := obs.NewSimClock(start)
	a := newApp(t, clock)

	// Warm the indicators past the slowest lookback, then run long enough for
	// the regime engine to classify.
	warmPlatform(a)
	drive(t, a, clock, 900)

	snap, ok := a.Pipeline.Snapshot("AAPL")
	if !ok {
		t.Fatal("no feature snapshot was produced for AAPL")
	}
	if snap.Hash == "" {
		t.Fatal("the snapshot carries no content hash, so no decision made from it is reproducible")
	}
	cold := 0
	for name := range snap.Values {
		if warm, has := snap.Warm[name]; has && !warm {
			cold++
		}
	}
	if cold > 0 {
		t.Errorf("%d features are still cold after warming past the %d-bar longest lookback; "+
			"every rule referencing them is being skipped", cold, domain.MaxFeatureLookbackBars)
	}

	rg := a.Pipeline.Regime()
	if rg.Regime == "" {
		t.Fatal("no market regime was classified")
	}

	preds := a.State.Predictions()
	if len(preds) == 0 {
		t.Fatal("the pipeline produced no predictions at all")
	}
	for tk, p := range preds {
		if len(p.Scenarios) == 0 {
			t.Fatalf("%s: a prediction with no scenarios", tk)
		}
		sc := p.Primary()
		sum := sc.Dist.Up + sc.Dist.Flat + sc.Dist.Down
		if sum < 0.999 || sum > 1.001 {
			t.Fatalf("%s: the distribution sums to %.6f", tk, sum)
		}
		if p.ModelVersion == "" {
			t.Fatalf("%s: a prediction with no model version cannot be reproduced", tk)
		}
		if p.FeatureHash == "" {
			t.Fatalf("%s: a prediction that does not name its feature snapshot cannot be explained", tk)
		}
		if sc.FlatBandBps <= 0 {
			t.Fatalf("%s: a three-class prediction with no flat band: the classes mean nothing", tk)
		}
		break
	}
}

// TestEveryEmittedSignalCarriesItsProvenance is the product's central claim,
// checked against whatever the platform actually emitted.
//
// A run that produces no signal is a legitimate outcome — a quiet tape has no
// setups — so this asserts a property of the signals that exist rather than
// requiring that some exist.
func TestEveryEmittedSignalCarriesItsProvenance(t *testing.T) {
	clock := obs.NewSimClock(start)
	a := newApp(t, clock)
	warmPlatform(a)
	// A trending tape, so the signal path is actually exercised.
	if sc, ok := namedBullScenario(); ok {
		a.Sim.AddScenario(sc)
	}
	drive(t, a, clock, 1800)

	signals := a.Signals.Active()
	t.Logf("the run produced %d active signals", len(signals))

	// A run with no signals is legitimate — a tape with no setups should
	// produce none — but silence and refusal are different things. Every
	// candidate the platform declined must say at which stage and why, or this
	// test would pass just as happily against a pipeline that does nothing.
	rejections := a.State.Rejections()
	if len(signals) == 0 && len(rejections) == 0 {
		t.Fatal("the platform produced neither a signal nor a single recorded rejection: " +
			"it is not declining to act, it is not acting at all")
	}
	for tk, reason := range rejections {
		if reason == "" {
			t.Fatalf("%s was rejected with no reason recorded", tk)
		}
	}

	for _, s := range signals {
		if s.ID == "" || s.Ticker == "" {
			t.Fatalf("a signal with no identity: %+v", s)
		}
		if s.RiskAssessmentID == "" {
			t.Fatalf("%s: does not name the risk assessment that allowed it; the veto would be "+
				"unauditable after the fact", s.ID)
		}
		if s.PredictionID == "" {
			t.Fatalf("%s: does not name the prediction it rests on", s.ID)
		}
		if len(s.Invalidations) == 0 {
			t.Fatalf("%s: no invalidation conditions; nothing could ever falsify this signal", s.ID)
		}
		if s.FeatureHash == "" {
			t.Fatalf("%s: does not name the feature snapshot it was computed from", s.ID)
		}
		if s.StrategyID == "" || s.ConfigHash == "" {
			t.Fatalf("%s: does not name the strategy or configuration that produced it", s.ID)
		}
		if s.ModelVersion == "" {
			t.Fatalf("%s: does not name the model version behind its prediction", s.ID)
		}
		if len(s.Evidence) == 0 {
			t.Fatalf("%s: no evidence recorded", s.ID)
		}
		if s.Disclaimer != domain.Disclaimer {
			t.Fatalf("%s: does not carry the research disclaimer", s.ID)
		}
		if s.Status != domain.SignalActive {
			t.Fatalf("%s: is in the active set with status %s", s.ID, s.Status)
		}
	}
}

// TestPredictionsAreTrackedAndScored checks that the evaluation loop closes: a
// prediction made is a prediction that gets marked, including one the risk
// engine refused to act on.
func TestPredictionsAreTrackedAndScored(t *testing.T) {
	clock := obs.NewSimClock(start)
	a := newApp(t, clock)
	warmPlatform(a)
	drive(t, a, clock, 600)

	if a.Evaluator.PendingCount() == 0 {
		t.Fatal("no prediction was registered for evaluation; the platform would never learn it was wrong")
	}

	// Advance past the shortest horizon so the earliest predictions resolve.
	drive(t, a, clock, 16*60)
	outcomes := a.EvaluateOnce(context.Background(), clock.Now())
	if len(outcomes) == 0 {
		t.Fatal("no prediction resolved after advancing past its horizon")
	}
	for _, o := range outcomes {
		if o.Predicted == "" || o.Actual == "" {
			t.Fatalf("an outcome with no verdict: %+v", o)
		}
		if o.BrierScore < 0 || o.BrierScore > 2 {
			t.Fatalf("Brier score %.4f is outside its range", o.BrierScore)
		}
		if o.ModelVersion == "" {
			t.Fatal("an outcome that does not say which model produced it teaches nothing")
		}
	}
	if a.Evaluator.Discarded() > 0 {
		t.Logf("%d predictions were discarded as unresolvable, which is the honest "+
			"outcome when a sweep runs late", a.Evaluator.Discarded())
	}
}

// TestTheSamePlatformTwiceProducesTheSameDecisions is the reproducibility
// claim. Two independently constructed platforms, given the same seed and the
// same simulated clock, must agree on every feature hash and every prediction.
func TestTheSamePlatformTwiceProducesTheSameDecisions(t *testing.T) {
	run := func() (map[domain.Ticker]string, map[domain.Ticker]domain.Distribution) {
		clock := obs.NewSimClock(start)
		a := newApp(t, clock)
		warmPlatform(a)
		drive(t, a, clock, 600)

		hashes := map[domain.Ticker]string{}
		dists := map[domain.Ticker]domain.Distribution{}
		for tk, p := range a.State.Predictions() {
			dists[tk] = p.Primary().Dist
		}
		for tk := range dists {
			if snap, ok := a.Pipeline.Snapshot(tk); ok {
				hashes[tk] = snap.Hash
			}
		}
		return hashes, dists
	}

	h1, d1 := run()
	h2, d2 := run()

	if len(h1) == 0 {
		t.Fatal("the first run produced nothing to compare")
	}
	if len(h1) != len(h2) {
		t.Fatalf("the two runs covered different symbol sets: %d and %d", len(h1), len(h2))
	}
	for tk, h := range h1 {
		if h2[tk] != h {
			t.Fatalf("%s: feature hash differs between identical runs (%s vs %s); "+
				"nothing downstream of this is reproducible", tk, h, h2[tk])
		}
	}
	for tk, d := range d1 {
		o := d2[tk]
		if d.Up != o.Up || d.Flat != o.Flat || d.Down != o.Down {
			t.Fatalf("%s: prediction differs between identical runs: %+v vs %+v", tk, d, o)
		}
	}
}

// namedBullScenario returns the shipped trending scenario, so the integration
// test exercises the same market conditions an operator can ask for with
// `quantos run --scenario bull`.
func namedBullScenario() (marketdata.Scenario, bool) {
	return marketdata.NamedScenario("bull")
}

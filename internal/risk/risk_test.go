package risk

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

var now = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

func engine() *Engine {
	cfg := DefaultConfig()
	cfg.Clock = obs.NewSimClock(now)
	cfg.ConfigHash = "test-config"
	return NewEngine(cfg)
}

// healthy builds an input that should pass every check, so each test can spoil
// exactly one thing and see the verdict change.
func healthy() Input {
	snap := domain.FeatureSnapshot{
		Ticker:     "AAPL",
		AsOf:       now,
		LastTickAt: now.Add(-2 * time.Second),
		SpreadBps:  4,
		Last:       230,
		Values: map[string]float64{
			domain.FeatLast:        230,
			domain.FeatRealizedVol: 0.22,
			domain.FeatATRPct:      0.004,
		},
		Warm: map[string]bool{
			domain.FeatLast: true, domain.FeatRealizedVol: true, domain.FeatATRPct: true,
		},
	}
	pred := domain.Prediction{
		ID: "p1", Ticker: "AAPL", Source: domain.SourceModel, ModelVersion: "v1",
		Scenarios: []domain.ScenarioPrediction{{
			Horizon:    domain.Horizon60m,
			Dist:       domain.NewDistribution(0.60, 0.25, 0.15),
			Confidence: domain.NewDistribution(0.60, 0.25, 0.15).Confidence(),
		}},
	}
	return Input{
		Ticker: "AAPL",
		Stock: domain.Stock{
			Ticker: "AAPL", Sector: "Technology",
			AvgNotional: 12_000_000_000, AvgVolume: 55_000_000,
		},
		Snapshot:   snap,
		Prediction: &pred,
		Regime:     domain.MarketRegime{Regime: domain.RegimeBullTrend, Confidence: 0.8},
		Portfolio: &domain.Portfolio{
			ID: "p", Equity: 100_000, PeakEquity: 100_000,
			SectorExposure: map[string]float64{"Technology": 0.05},
		},
		Health:         &domain.ModelHealth{ModelVersion: "v1", Healthy: true, Drift: domain.DriftNone},
		ProposedWeight: 0.03,
		Side:           domain.SideLong,
		Now:            now,
	}
}

func TestHealthyInputIsAllowed(t *testing.T) {
	a := engine().Assess(healthy())
	if a.Decision != domain.RiskAllowPaperSignal {
		t.Fatalf("a healthy input was not allowed: %s — blockers=%v warnings=%v",
			a.Decision, a.Blockers, a.Warnings)
	}
	if a.ConfigHash != "test-config" {
		t.Fatal("the assessment does not record the configuration it was made under")
	}
	if len(a.Checks) != 11 {
		t.Fatalf("expected eleven checks, got %d", len(a.Checks))
	}
}

// TestCanaryBlocks is the regression guard from ADR-009: known-bad inputs must
// still be blocked under the shipped configuration. If threshold tuning ever
// makes the veto vacuous, this fails.
func TestCanaryBlocks(t *testing.T) {
	cases := map[string]func(*Input){
		"stale market data": func(in *Input) {
			in.Snapshot.LastTickAt = now.Add(-10 * time.Minute)
		},
		"illiquid instrument": func(in *Input) {
			in.Stock.AvgNotional = 100_000
		},
		"absurd spread": func(in *Input) {
			in.Snapshot.SpreadBps = 500
		},
		"extreme volatility": func(in *Input) {
			in.Snapshot.Values[domain.FeatRealizedVol] = 1.5
		},
		"earnings inside the blocking window": func(in *Input) {
			in.Events = []domain.CorporateEvent{{
				Ticker: "AAPL", Type: domain.EventEarnings,
				ScheduledAt: now.Add(6 * time.Hour), Confirmed: true,
			}}
		},
		"severe model drift": func(in *Input) {
			in.Health = &domain.ModelHealth{ModelVersion: "v1", Drift: domain.DriftSevere}
		},
		"position weight over the hard cap": func(in *Input) {
			in.ProposedWeight = 0.5
		},
		"sector weight over the hard cap": func(in *Input) {
			in.Portfolio.SectorExposure["Technology"] = 0.40
		},
		"drawdown kill switch": func(in *Input) {
			in.Portfolio.Drawdown = 0.25
		},
		"correlated exposure over the hard cap": func(in *Input) {
			in.CorrelatedExposure = 0.6
		},
		"long into a high-volatility risk-off tape": func(in *Input) {
			in.Regime = domain.MarketRegime{Regime: domain.RegimeRiskOff, Confidence: 0.9, Volatility: 0.85}
		},
	}
	e := engine()
	for name, spoil := range cases {
		in := healthy()
		spoil(&in)
		a := e.Assess(in)
		if a.Decision != domain.RiskBlock {
			t.Errorf("%s: expected BLOCK, got %s (blockers=%v warnings=%v)",
				name, a.Decision, a.Blockers, a.Warnings)
		}
		if len(a.Blockers) == 0 {
			t.Errorf("%s: blocked without recording a reason", name)
		}
	}
}

func TestWatchOnlyConditions(t *testing.T) {
	cases := map[string]func(*Input){
		"below the confidence floor": func(in *Input) {
			in.Prediction.Scenarios[0].Confidence = 0.05
		},
		"soft staleness": func(in *Input) {
			in.Snapshot.LastTickAt = now.Add(-45 * time.Second)
		},
		"soft spread": func(in *Input) {
			in.Snapshot.SpreadBps = 40
		},
		"moderate drift": func(in *Input) {
			in.Health = &domain.ModelHealth{ModelVersion: "v1", Drift: domain.DriftModerate}
		},
		"low regime confidence": func(in *Input) {
			in.Regime.Confidence = 0.1
		},
	}
	e := engine()
	for name, spoil := range cases {
		in := healthy()
		spoil(&in)
		a := e.Assess(in)
		if a.Decision != domain.RiskWatchOnly {
			t.Errorf("%s: expected WATCH_ONLY, got %s", name, a.Decision)
		}
	}
}

// TestMostSevereWins is the property that keeps the veto from degrading into a
// suggestion: a single BLOCK must dominate any number of passes.
func TestMostSevereWins(t *testing.T) {
	in := healthy()
	in.Snapshot.SpreadBps = 500 // BLOCK
	in.Regime.Confidence = 0.1  // WATCH_ONLY
	a := engine().Assess(in)
	if a.Decision != domain.RiskBlock {
		t.Fatalf("a BLOCK was averaged away into %s", a.Decision)
	}
}

// TestSkippedChecksAreNotPasses covers the "we cannot evaluate it, so we do not
// get to assume it is absent" rule.
func TestSkippedChecksAreNotPasses(t *testing.T) {
	in := healthy()
	in.Portfolio = nil
	a := engine().Assess(in)
	if a.Decision == domain.RiskAllowPaperSignal {
		t.Fatal("with no portfolio state the concentration, correlation and drawdown " +
			"checks cannot run, so the verdict must not be ALLOW")
	}
	skipped := 0
	for _, c := range a.Checks {
		if c.Status == domain.CheckSkip {
			skipped++
			if c.Skipped == "" {
				t.Fatalf("check %s was skipped without saying why", c.Name)
			}
		}
	}
	if skipped < 3 {
		t.Fatalf("expected at least three skipped checks, got %d", skipped)
	}
}

// TestNoModelYetIsNotADeadlock guards the fix for a real defect: treating "no
// model health record" as an unevaluated risk meant the platform could never
// emit its first signal.
func TestNoModelYetIsNotADeadlock(t *testing.T) {
	in := healthy()
	in.Health = nil
	in.Prediction.Source = domain.SourceRulesFallback
	a := engine().Assess(in)
	if a.Decision != domain.RiskAllowPaperSignal {
		t.Fatalf("with no trained model and a rule-prior prediction the verdict was %s; "+
			"the platform would never emit a first signal (blockers=%v warnings=%v)",
			a.Decision, a.Blockers, a.Warnings)
	}
}

func TestServingModelWithoutHealthIsWatchOnly(t *testing.T) {
	in := healthy()
	in.Health = nil
	in.Prediction.Source = domain.SourceModel
	if a := engine().Assess(in); a.Decision != domain.RiskWatchOnly {
		t.Fatalf("an unmeasured serving model produced %s; its accuracy is unknown", a.Decision)
	}
}

// TestProvisionalHealthLetsANewModelStart covers the bootstrap that the
// unmeasured-model rule would otherwise make impossible: a freshly promoted
// model has no live outcomes, and refusing to act on it means it never gets
// any. An offline result measured on data the model never saw is a real
// measurement, so it is allowed to start on that.
func TestProvisionalHealthLetsANewModelStart(t *testing.T) {
	in := healthy()
	in.Health = &domain.ModelHealth{
		ModelVersion: "v1", Healthy: true, Drift: domain.DriftNone,
		Provisional: true, Accuracy: 0.42, Samples: 0,
	}
	a := engine().Assess(in)
	if a.Decision != domain.RiskAllowPaperSignal {
		t.Fatalf("a model with a passing offline evaluation produced %s (blockers=%v warnings=%v)",
			a.Decision, a.Blockers, a.Warnings)
	}
	for _, c := range a.Checks {
		if c.Name == domain.CheckModelHealth && !strings.Contains(c.Reason, "offline") {
			t.Fatalf("the check does not say the accuracy is offline-measured: %q", c.Reason)
		}
	}
}

// TestProvisionalHealthStillBlocksAFailingModel is the other half: the
// bootstrap allowance is not a free pass. A model that did not beat its own
// base rate offline is worse than a constant guess and must not serve.
func TestProvisionalHealthStillBlocksAFailingModel(t *testing.T) {
	in := healthy()
	in.Health = &domain.ModelHealth{
		ModelVersion: "v1", Healthy: false, Drift: domain.DriftNone, Provisional: true,
		Issues: []string{"offline test accuracy 0.3100 does not beat the base rate 0.3900"},
	}
	if a := engine().Assess(in); a.Decision != domain.RiskBlock {
		t.Fatalf("a model that failed its own offline evaluation produced %s", a.Decision)
	}
}

func TestBlockedRegimeConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clock = obs.NewSimClock(now)
	cfg.BlockedRegimes = []domain.Regime{domain.RegimeBullTrend}
	a := NewEngine(cfg).Assess(healthy())
	if a.Decision != domain.RiskBlock {
		t.Fatalf("a configured blocked regime did not block: %s", a.Decision)
	}
}

// TestDecisionIsNeverDerivableFromScore documents that the numeric score is
// descriptive only: two assessments with similar scores can carry opposite
// decisions, so no consumer may threshold on the score.
func TestDecisionIsNeverDerivableFromScore(t *testing.T) {
	e := engine()
	blocked := healthy()
	blocked.Snapshot.SpreadBps = 500
	b := e.Assess(blocked)
	a := e.Assess(healthy())
	if b.Decision != domain.RiskBlock || a.Decision != domain.RiskAllowPaperSignal {
		t.Fatal("test setup failed")
	}
	if b.Score < a.Score {
		t.Fatal("a blocked assessment scored lower than an allowed one, which would be surprising")
	}
}

func TestSummariseMeasuresTheVeto(t *testing.T) {
	e := engine()
	var assessments []domain.RiskAssessment
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		in := healthy()
		if rng.Intn(3) == 0 {
			in.Snapshot.SpreadBps = 500
		}
		assessments = append(assessments, e.Assess(in))
	}
	s := Summarise(assessments)
	if s.Total != 200 {
		t.Fatalf("summary counted %d assessments", s.Total)
	}
	if s.Blocked == 0 || s.Allowed == 0 {
		t.Fatalf("expected a mix of verdicts, got blocked=%d allowed=%d", s.Blocked, s.Allowed)
	}
	if s.TopBlockReason != domain.CheckSpread {
		t.Fatalf("top block reason is %q, expected %q", s.TopBlockReason, domain.CheckSpread)
	}
	if s.VetoRate <= 0 || s.VetoRate >= 1 {
		t.Fatalf("veto rate %f is not a proportion", s.VetoRate)
	}
}

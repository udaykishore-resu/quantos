package rules

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

var at = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

func snap(values map[string]float64, cold ...string) domain.FeatureSnapshot {
	s := domain.FeatureSnapshot{
		Ticker: "AAPL", AsOf: at, Interval: domain.Interval1m,
		Values: values, Warm: map[string]bool{},
	}
	for name := range values {
		s.Warm[name] = true
	}
	for _, name := range cold {
		s.Warm[name] = false
	}
	return s
}

func strategy(rules ...domain.Rule) domain.Strategy {
	return domain.Strategy{ID: "test", Name: "Test", Hash: "hash", Rules: rules}
}

func TestAConjunctionRequiresEveryCondition(t *testing.T) {
	r := domain.Rule{
		ID: "trend", Side: domain.SideLong, Weight: 1,
		All: []domain.Condition{
			{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 55},
			{Feature: domain.FeatADX14, Op: domain.CmpGT, Value: 25},
		},
	}
	e := NewEngine()
	both := e.Evaluate(strategy(r), snap(map[string]float64{
		domain.FeatRSI14: 60, domain.FeatADX14: 30,
	}), domain.RegimeBullTrend)
	if len(both.Fired) != 1 {
		t.Fatalf("a satisfied conjunction did not fire: %+v", both)
	}

	e = NewEngine()
	one := e.Evaluate(strategy(r), snap(map[string]float64{
		domain.FeatRSI14: 60, domain.FeatADX14: 10,
	}), domain.RegimeBullTrend)
	if len(one.Fired) != 0 {
		t.Fatal("a rule fired with one of its two conditions unmet")
	}
	if len(one.Skipped) != 0 {
		t.Fatal("an evaluated-and-false rule was reported as skipped")
	}
}

// TestColdFeaturesSkipRatherThanFalsify is the distinction the whole
// explanation layer depends on: "the condition was false" and "we could not
// evaluate the condition" are different facts and must not be conflated.
func TestColdFeaturesSkipRatherThanFalsify(t *testing.T) {
	r := domain.Rule{
		ID: "trend", Side: domain.SideLong, Weight: 1,
		All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 55}},
	}
	res := NewEngine().Evaluate(strategy(r), snap(
		map[string]float64{domain.FeatRSI14: 60}, domain.FeatRSI14,
	), domain.RegimeBullTrend)

	if len(res.Fired) != 0 {
		t.Fatal("a rule fired on a cold indicator that has not warmed up")
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("a cold feature produced %d skip records, expected one", len(res.Skipped))
	}
	if !strings.Contains(res.Skipped[0], "cold") && !strings.Contains(res.Skipped[0], "unavailable") {
		t.Fatalf("the skip record does not explain itself: %q", res.Skipped[0])
	}
	if res.Score != 0 {
		t.Fatalf("an unevaluable rule contributed %.2f to the score", res.Score)
	}
}

func TestMissingFeatureIsSkippedNotAssumedZero(t *testing.T) {
	r := domain.Rule{
		ID: "vol", Side: domain.SideLong, Weight: 1,
		All: []domain.Condition{{Feature: domain.FeatATRPct, Op: domain.CmpLT, Value: 0.05}},
	}
	res := NewEngine().Evaluate(strategy(r), snap(map[string]float64{domain.FeatRSI14: 60}), domain.RegimeBullTrend)
	if len(res.Fired) != 0 {
		t.Fatal("an absent feature was treated as zero, which satisfied a `less than` test")
	}
	if len(res.Skipped) != 1 {
		t.Fatal("the absent feature was not recorded as a skip")
	}
}

func TestCrossUpNeedsAPriorSnapshot(t *testing.T) {
	r := domain.Rule{
		ID: "cross", Side: domain.SideLong, Weight: 1,
		All: []domain.Condition{{
			Feature: domain.FeatEMA9, Op: domain.CmpCrossUp, Other: domain.FeatEMA21,
		}},
	}
	e := NewEngine()
	// First observation: below. There is no prior snapshot, so the crossover
	// test cannot be evaluated at all.
	first := e.Evaluate(strategy(r), snap(map[string]float64{
		domain.FeatEMA9: 99, domain.FeatEMA21: 100,
	}), domain.RegimeBullTrend)
	if len(first.Skipped) != 1 {
		t.Fatalf("a crossover was evaluated with no history: %+v", first)
	}

	// Second observation: now above. The cross happened between the two.
	second := e.Evaluate(strategy(r), snap(map[string]float64{
		domain.FeatEMA9: 101, domain.FeatEMA21: 100,
	}), domain.RegimeBullTrend)
	if len(second.Fired) != 1 {
		t.Fatalf("a genuine crossover did not fire: %+v", second)
	}

	// Third observation: still above, but nothing crossed this bar.
	third := e.Evaluate(strategy(r), snap(map[string]float64{
		domain.FeatEMA9: 102, domain.FeatEMA21: 100,
	}), domain.RegimeBullTrend)
	if len(third.Fired) != 0 {
		t.Fatal("a crossover fired again while merely remaining above; every bar " +
			"of an uptrend would be a fresh entry signal")
	}
}

func TestRegimeGatingSkipsInapplicableRules(t *testing.T) {
	r := domain.Rule{
		ID: "trend-only", Side: domain.SideLong, Weight: 1,
		Regimes: []domain.Regime{domain.RegimeBullTrend},
		All:     []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}},
	}
	s := strategy(r)
	values := map[string]float64{domain.FeatRSI14: 60}

	inTrend := NewEngine().Evaluate(s, snap(values), domain.RegimeBullTrend)
	if len(inTrend.Fired) != 1 {
		t.Fatal("a regime-gated rule did not fire inside its regime")
	}
	inRange := NewEngine().Evaluate(s, snap(values), domain.RegimeRange)
	if len(inRange.Fired) != 0 {
		t.Fatal("a trend rule fired in a range: this is the 'applying indicators blindly' failure")
	}
	if inRange.EligibleWeight != 0 {
		t.Fatalf("an inapplicable rule still counted %.2f toward the eligible weight, "+
			"which would depress confidence for a reason unrelated to the evidence",
			inRange.EligibleWeight)
	}
}

func TestStrategyRegimeGateShortCircuits(t *testing.T) {
	s := strategy(domain.Rule{
		ID: "any", Side: domain.SideLong, Weight: 1,
		All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}},
	})
	s.Regimes = []domain.Regime{domain.RegimeBullTrend}
	res := NewEngine().Evaluate(s, snap(map[string]float64{domain.FeatRSI14: 60}), domain.RegimeHighVol)
	if len(res.Fired) != 0 || res.Score != 0 {
		t.Fatal("a strategy disabled in this regime still produced a score")
	}
	if len(res.Skipped) == 0 {
		t.Fatal("the strategy-level skip was not recorded")
	}
}

// TestCounterEvidenceSubtractsAndIsSurfaced covers the requirement that the
// platform argues against itself: a counter rule must reduce the score and
// appear in the explanation, not be quietly dropped.
func TestCounterEvidenceSubtractsAndIsSurfaced(t *testing.T) {
	s := strategy(
		domain.Rule{
			ID: "momentum", Side: domain.SideLong, Weight: 2,
			All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 55}},
		},
		domain.Rule{
			ID: "stretched", Side: domain.SideLong, Weight: 1, Counter: true,
			Description: "RSI is stretched",
			All:         []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 75}},
		},
	)
	e := NewEngine()
	clean := e.Evaluate(s, snap(map[string]float64{domain.FeatRSI14: 60}), domain.RegimeBullTrend)
	e = NewEngine()
	stretched := e.Evaluate(s, snap(map[string]float64{domain.FeatRSI14: 85}), domain.RegimeBullTrend)

	if stretched.Score >= clean.Score {
		t.Fatalf("counter-evidence did not reduce the score: %.2f with, %.2f without",
			stretched.Score, clean.Score)
	}
	if len(stretched.CounterFired) != 1 || len(stretched.CounterEvidence) == 0 {
		t.Fatal("the counter rule fired but was not surfaced as counter-evidence")
	}
	for _, h := range stretched.Fired {
		if h.Counter {
			t.Fatal("a counter rule was counted as supporting evidence")
		}
	}
}

func TestConfidenceIsCoverageNotConviction(t *testing.T) {
	s := strategy(
		domain.Rule{ID: "a", Side: domain.SideLong, Weight: 1,
			All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}}},
		domain.Rule{ID: "b", Side: domain.SideLong, Weight: 1,
			All: []domain.Condition{{Feature: domain.FeatADX14, Op: domain.CmpGT, Value: 25}}},
		domain.Rule{ID: "c", Side: domain.SideLong, Weight: 2,
			All: []domain.Condition{{Feature: domain.FeatMACDHist, Op: domain.CmpGT, Value: 0}}},
	)
	res := NewEngine().Evaluate(s, snap(map[string]float64{
		domain.FeatRSI14: 60, domain.FeatADX14: 10, domain.FeatMACDHist: 0.4,
	}), domain.RegimeBullTrend)

	// Rules a and c fired: weight 3 of an eligible 4.
	if math.Abs(res.Confidence-0.75) > 1e-9 {
		t.Fatalf("confidence %.4f, expected the fired share of eligible weight (0.75)", res.Confidence)
	}
	if res.EligibleWeight != 4 || res.FiredWeight != 3 {
		t.Fatalf("eligible=%.1f fired=%.1f", res.EligibleWeight, res.FiredWeight)
	}
	if res.Side != domain.SideLong {
		t.Fatalf("side %s", res.Side)
	}
}

func TestScoreIsBoundedAndSigned(t *testing.T) {
	s := strategy(
		domain.Rule{ID: "long", Side: domain.SideLong, Weight: 1,
			All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}}},
		domain.Rule{ID: "short", Side: domain.SideShort, Weight: 3,
			All: []domain.Condition{{Feature: domain.FeatMACDHist, Op: domain.CmpLT, Value: 0}}},
	)
	res := NewEngine().Evaluate(s, snap(map[string]float64{
		domain.FeatRSI14: 60, domain.FeatMACDHist: -0.4,
	}), domain.RegimeBullTrend)
	if res.Side != domain.SideShort {
		t.Fatalf("the heavier short evidence did not set the side: %s (score %.2f)", res.Side, res.Score)
	}
	if res.Score < -100 || res.Score > 100 {
		t.Fatalf("score %.2f is outside [-100, 100]", res.Score)
	}
}

func TestDisabledRulesAreInert(t *testing.T) {
	off := false
	s := strategy(domain.Rule{
		ID: "a", Side: domain.SideLong, Weight: 1, Enabled: &off,
		All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}},
	})
	res := NewEngine().Evaluate(s, snap(map[string]float64{domain.FeatRSI14: 60}), domain.RegimeBullTrend)
	if len(res.Fired) != 0 || res.EligibleWeight != 0 {
		t.Fatal("a disabled rule was evaluated")
	}
}

// --- loader -----------------------------------------------------------------

func TestValidateRejectsAnUnknownFeature(t *testing.T) {
	err := Validate(domain.Strategy{
		ID: "bad", Name: "Bad",
		Rules: []domain.Rule{{
			ID: "r", Side: domain.SideLong, Weight: 1,
			All: []domain.Condition{{Feature: "rsi_fourteen", Op: domain.CmpGT, Value: 50}},
		}},
	})
	if err == nil {
		t.Fatal("a typo'd feature name was accepted; the rule would silently never fire")
	}
	if !strings.Contains(err.Error(), "rsi_fourteen") {
		t.Fatalf("the error does not name the offending feature: %v", err)
	}
}

func TestValidateRejectsStructuralMistakes(t *testing.T) {
	cases := map[string]domain.Strategy{
		"no id": {Name: "x", Rules: []domain.Rule{{ID: "r", Side: domain.SideLong, Weight: 1,
			All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}}}}},
		"no rules": {ID: "x", Name: "x"},
		"rule with no conditions": {ID: "x", Name: "x", Rules: []domain.Rule{
			{ID: "r", Side: domain.SideLong, Weight: 1}}},
		"unknown operator": {ID: "x", Name: "x", Rules: []domain.Rule{{
			ID: "r", Side: domain.SideLong, Weight: 1,
			All: []domain.Condition{{Feature: domain.FeatRSI14, Op: "approximately", Value: 50}}}}},
		"unknown regime": {ID: "x", Name: "x", Regimes: []domain.Regime{"SIDEWAYS_ISH"},
			Rules: []domain.Rule{{ID: "r", Side: domain.SideLong, Weight: 1,
				All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}}}}},
		"duplicate rule ids": {ID: "x", Name: "x", Rules: []domain.Rule{
			{ID: "r", Side: domain.SideLong, Weight: 1,
				All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}}},
			{ID: "r", Side: domain.SideLong, Weight: 1,
				All: []domain.Condition{{Feature: domain.FeatADX14, Op: domain.CmpGT, Value: 20}}},
		}},
	}
	for name, s := range cases {
		if err := Validate(s); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}

// TestShippedStrategiesAreValid keeps the repository's own configuration
// honest: a strategy file that references a feature the engine never computes
// is a rule that can never fire, and nothing else would notice.
func TestShippedStrategiesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "strategies")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no strategies directory: %v", err)
	}
	reg, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("the shipped strategies do not load: %v", err)
	}
	if reg.Len() == 0 {
		t.Fatal("no strategies were loaded")
	}
	seen := map[string]bool{}
	for _, s := range reg.All() {
		if s.Hash == "" {
			t.Errorf("strategy %s has no configuration hash, so signals cannot cite the "+
				"exact configuration that produced them", s.ID)
		}
		if seen[s.Hash] {
			t.Errorf("strategy %s shares a hash with another file", s.ID)
		}
		seen[s.Hash] = true
	}
}

func TestRegistryRejectsDuplicateStrategyIDs(t *testing.T) {
	r := NewRegistry()
	s := strategy(domain.Rule{ID: "r", Side: domain.SideLong, Weight: 1,
		All: []domain.Condition{{Feature: domain.FeatRSI14, Op: domain.CmpGT, Value: 50}}})
	if err := r.Add(s); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(s); err == nil {
		t.Fatal("a duplicate strategy id was accepted")
	}
}

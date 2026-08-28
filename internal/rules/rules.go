// Package rules evaluates configuration-driven strategies against feature
// snapshots.
//
// No strategy logic lives in Go source (requirement §37): a rule is a named
// conjunction of typed conditions over feature names, loaded from YAML, and the
// engine is a pure evaluator. That is what makes strategies reviewable by
// someone who does not read Go, versionable in git, and diffable in a pull
// request.
package rules

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// RuleHit records that a rule fired, with the evidence that made it fire.
type RuleHit struct {
	RuleID      string      `json:"rule_id"`
	Description string      `json:"description"`
	Side        domain.Side `json:"side"`
	Weight      float64     `json:"weight"`
	Counter     bool        `json:"counter"`
	Evidence    []string    `json:"evidence"`
}

// Result is the outcome of evaluating a strategy against one snapshot.
type Result struct {
	StrategyID string        `json:"strategy_id"`
	Ticker     domain.Ticker `json:"ticker"`
	Side       domain.Side   `json:"side"`
	// Score is the net directional rule score in [-100, 100]: positive is long,
	// negative is short. It is a *rule* score, not a probability, and the
	// prediction engine treats it as a prior, never as an output.
	Score float64 `json:"score"`
	// Confidence is derived from how much of the strategy's available weight
	// actually fired, which is a meaningful and bounded quantity — unlike a
	// number invented to look like a probability.
	Confidence float64 `json:"confidence"`

	Fired           []RuleHit `json:"fired"`
	CounterFired    []RuleHit `json:"counter_fired,omitempty"`
	Skipped         []string  `json:"skipped,omitempty"`
	Evidence        []string  `json:"evidence"`
	CounterEvidence []string  `json:"counter_evidence,omitempty"`

	EligibleWeight float64       `json:"eligible_weight"`
	FiredWeight    float64       `json:"fired_weight"`
	Regime         domain.Regime `json:"regime"`
	ConfigHash     string        `json:"config_hash"`
}

// Engine evaluates strategies. It retains the previous snapshot per symbol so
// that crossover conditions can be expressed.
type Engine struct {
	mu   sync.RWMutex
	prev map[domain.Ticker]domain.FeatureSnapshot
}

// NewEngine builds a rule engine.
func NewEngine() *Engine {
	return &Engine{prev: map[domain.Ticker]domain.FeatureSnapshot{}}
}

// Evaluate runs a strategy against a snapshot in a given regime.
//
// Rules whose Regimes list excludes the current regime are not evaluated at
// all: this is how the platform avoids applying indicators blindly (§9). They
// are reported in Skipped so the exclusion is visible rather than silent.
func (e *Engine) Evaluate(s domain.Strategy, snap domain.FeatureSnapshot, rg domain.Regime) Result {
	e.mu.RLock()
	prev, hasPrev := e.prev[snap.Ticker]
	e.mu.RUnlock()

	res := Result{
		StrategyID: s.ID,
		Ticker:     snap.Ticker,
		Regime:     rg,
		ConfigHash: s.Hash,
		Side:       domain.SideFlat,
	}

	if len(s.Regimes) > 0 && !containsRegime(s.Regimes, rg) {
		res.Skipped = append(res.Skipped, fmt.Sprintf("strategy %s is not enabled in regime %s", s.ID, rg))
		e.remember(snap)
		return res
	}

	long, short := 0.0, 0.0
	for _, r := range s.Rules {
		if !r.Active() {
			continue
		}
		if !r.AppliesTo(rg) {
			res.Skipped = append(res.Skipped, fmt.Sprintf("rule %s not applicable in regime %s", r.ID, rg))
			continue
		}
		res.EligibleWeight += math.Abs(r.Weight)

		ok, evidence, missing := evalRule(r, snap, prev, hasPrev)
		if len(missing) > 0 {
			res.Skipped = append(res.Skipped,
				fmt.Sprintf("rule %s skipped: features unavailable or cold: %s", r.ID, strings.Join(missing, ", ")))
			continue
		}
		if !ok {
			continue
		}
		hit := RuleHit{
			RuleID: r.ID, Description: r.Description, Side: r.Side,
			Weight: r.Weight, Counter: r.Counter, Evidence: evidence,
		}
		if r.Counter {
			res.CounterFired = append(res.CounterFired, hit)
			res.CounterEvidence = append(res.CounterEvidence, describeHit(hit))
			// Counter-evidence subtracts from the side it argues against.
			switch r.Side {
			case domain.SideLong:
				long -= math.Abs(r.Weight)
			case domain.SideShort:
				short -= math.Abs(r.Weight)
			}
			continue
		}
		res.Fired = append(res.Fired, hit)
		res.Evidence = append(res.Evidence, describeHit(hit))
		res.FiredWeight += math.Abs(r.Weight)
		switch r.Side {
		case domain.SideLong:
			long += r.Weight
		case domain.SideShort:
			short += r.Weight
		}
	}

	net := long - short
	if res.EligibleWeight > 0 {
		res.Score = domain.Clamp(net/res.EligibleWeight*100, -100, 100)
		res.Confidence = domain.Clamp01(res.FiredWeight / res.EligibleWeight)
	}
	switch {
	case res.Score > 0:
		res.Side = domain.SideLong
	case res.Score < 0:
		res.Side = domain.SideShort
	}
	sort.SliceStable(res.Fired, func(i, j int) bool { return res.Fired[i].Weight > res.Fired[j].Weight })
	e.remember(snap)
	return res
}

func (e *Engine) remember(snap domain.FeatureSnapshot) {
	e.mu.Lock()
	e.prev[snap.Ticker] = snap
	e.mu.Unlock()
}

// Previous exposes the retained prior snapshot, which the alert engine uses to
// describe transitions.
func (e *Engine) Previous(t domain.Ticker) (domain.FeatureSnapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.prev[t]
	return s, ok
}

func describeHit(h RuleHit) string {
	if len(h.Evidence) == 0 {
		return h.Description
	}
	return fmt.Sprintf("%s (%s)", h.Description, strings.Join(h.Evidence, "; "))
}

// evalRule returns whether the rule fired, the evidence, and any features that
// were unavailable. A rule with an unavailable required feature does not fire
// and does not count against the strategy — it is skipped, because "we could
// not tell" is different from "the condition was false".
func evalRule(r domain.Rule, snap, prev domain.FeatureSnapshot, hasPrev bool) (bool, []string, []string) {
	var evidence, missing []string

	for _, c := range r.All {
		ok, ev, miss := evalCondition(c, snap, prev, hasPrev)
		if miss != "" {
			if !c.Optional {
				missing = append(missing, miss)
			}
			continue
		}
		if !ok {
			return false, nil, nil
		}
		evidence = append(evidence, ev)
	}
	if len(missing) > 0 {
		return false, nil, missing
	}

	if len(r.Any) > 0 {
		anyOK := false
		anyMissing := 0
		for _, c := range r.Any {
			ok, ev, miss := evalCondition(c, snap, prev, hasPrev)
			if miss != "" {
				anyMissing++
				continue
			}
			if ok {
				anyOK = true
				evidence = append(evidence, ev)
			}
		}
		if !anyOK {
			if anyMissing == len(r.Any) {
				return false, nil, []string{fmt.Sprintf("all `any` conditions of %s unavailable", r.ID)}
			}
			return false, nil, nil
		}
	}

	for _, c := range r.None {
		ok, _, miss := evalCondition(c, snap, prev, hasPrev)
		if miss != "" {
			if !c.Optional {
				return false, nil, []string{miss}
			}
			continue
		}
		if ok {
			return false, nil, nil
		}
	}
	return true, evidence, nil
}

// evalCondition evaluates one predicate. The third return value names a
// missing feature, and is empty when evaluation succeeded.
func evalCondition(c domain.Condition, snap, prev domain.FeatureSnapshot, hasPrev bool) (bool, string, string) {
	v, ok := snap.Get(c.Feature)
	if !ok {
		return false, "", c.Feature
	}

	// Resolve the right-hand side: a literal, or another feature with a scale.
	rhs := c.Value
	rhsDesc := formatNum(c.Value)
	if c.Other != "" {
		ov, ook := snap.Get(c.Other)
		if !ook {
			return false, "", c.Other
		}
		scale := c.Scale
		if scale == 0 {
			scale = 1
		}
		rhs = ov * scale
		if scale == 1 {
			rhsDesc = fmt.Sprintf("%s (%s)", c.Other, formatNum(ov))
		} else {
			rhsDesc = fmt.Sprintf("%.4g × %s (%s)", scale, c.Other, formatNum(rhs))
		}
	}

	describe := func(op string) string {
		if c.Describe != "" {
			return c.Describe
		}
		return fmt.Sprintf("%s %s %s is %s", c.Feature, op, rhsDesc, formatNum(v))
	}

	switch c.Op {
	case domain.CmpGT:
		return v > rhs, describe("above"), ""
	case domain.CmpGTE:
		return v >= rhs, describe("at or above"), ""
	case domain.CmpLT:
		return v < rhs, describe("below"), ""
	case domain.CmpLTE:
		return v <= rhs, describe("at or below"), ""
	case domain.CmpBetween:
		lo, hi := math.Min(c.Value, c.Value2), math.Max(c.Value, c.Value2)
		d := c.Describe
		if d == "" {
			d = fmt.Sprintf("%s is %s, within [%s, %s]", c.Feature, formatNum(v), formatNum(lo), formatNum(hi))
		}
		return v >= lo && v <= hi, d, ""
	case domain.CmpOutside:
		lo, hi := math.Min(c.Value, c.Value2), math.Max(c.Value, c.Value2)
		d := c.Describe
		if d == "" {
			d = fmt.Sprintf("%s is %s, outside [%s, %s]", c.Feature, formatNum(v), formatNum(lo), formatNum(hi))
		}
		return v < lo || v > hi, d, ""
	case domain.CmpTrue:
		return v != 0, describe("is set"), ""
	case domain.CmpFalse:
		return v == 0, describe("is not set"), ""
	case domain.CmpCrossUp, domain.CmpCrossDown:
		if !hasPrev {
			return false, "", c.Feature + " (no prior snapshot for a crossover test)"
		}
		pv, pok := prev.Get(c.Feature)
		if !pok {
			return false, "", c.Feature + " (prior value unavailable)"
		}
		prhs := c.Value
		if c.Other != "" {
			pov, pook := prev.Get(c.Other)
			if !pook {
				return false, "", c.Other + " (prior value unavailable)"
			}
			scale := c.Scale
			if scale == 0 {
				scale = 1
			}
			prhs = pov * scale
		}
		if c.Op == domain.CmpCrossUp {
			d := c.Describe
			if d == "" {
				d = fmt.Sprintf("%s crossed above %s", c.Feature, rhsDesc)
			}
			return pv <= prhs && v > rhs, d, ""
		}
		d := c.Describe
		if d == "" {
			d = fmt.Sprintf("%s crossed below %s", c.Feature, rhsDesc)
		}
		return pv >= prhs && v < rhs, d, ""
	default:
		return false, "", fmt.Sprintf("%s (unknown operator %q)", c.Feature, c.Op)
	}
}

func formatNum(v float64) string {
	switch {
	case v == math.Trunc(v) && math.Abs(v) < 1e6:
		return fmt.Sprintf("%.0f", v)
	case math.Abs(v) < 1:
		return fmt.Sprintf("%.4f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func containsRegime(list []domain.Regime, r domain.Regime) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}

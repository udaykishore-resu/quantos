package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Registry holds the loaded strategies. It is the single place strategies enter
// the system, which makes "which strategy produced this signal, at which
// version" answerable.
type Registry struct {
	mu         sync.RWMutex
	strategies map[string]domain.Strategy
	order      []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{strategies: map[string]domain.Strategy{}}
}

// LoadDir reads every .yaml/.yml file in dir. Files are loaded in sorted order
// so that the registry is deterministic. Validation failures are fatal: a
// malformed strategy silently degrading to "no rules fire" would be worse than
// a startup error.
func LoadDir(dir string) (*Registry, error) {
	r := NewRegistry()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("strategies: read %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		s, err := LoadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		if err := r.Add(s); err != nil {
			return nil, err
		}
	}
	if r.Len() == 0 {
		return nil, fmt.Errorf("strategies: no strategy files found in %s", dir)
	}
	return r, nil
}

// LoadFile parses and validates one strategy file.
func LoadFile(path string) (domain.Strategy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Strategy{}, fmt.Errorf("strategy %s: read: %w", path, err)
	}
	var modified time.Time
	if fi, err := os.Stat(path); err == nil {
		modified = fi.ModTime().UTC()
	}
	var s domain.Strategy
	if err := yaml.Unmarshal(data, &s); err != nil {
		return domain.Strategy{}, fmt.Errorf("strategy %s: parse: %w", path, err)
	}
	s.SourcePath = path
	sum := sha256.Sum256(data)
	s.Hash = hex.EncodeToString(sum[:])[:16]
	s.ModifiedAt = modified
	if err := Validate(s); err != nil {
		return domain.Strategy{}, fmt.Errorf("strategy %s: %w", path, err)
	}
	return s, nil
}

// Add registers a strategy.
func (r *Registry) Add(s domain.Strategy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.strategies[s.ID]; dup {
		return fmt.Errorf("strategies: duplicate id %q", s.ID)
	}
	r.strategies[s.ID] = s
	r.order = append(r.order, s.ID)
	sort.Strings(r.order)
	return nil
}

// Get returns a strategy by id.
func (r *Registry) Get(id string) (domain.Strategy, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.strategies[id]
	return s, ok
}

// All returns every strategy in deterministic order.
func (r *Registry) All() []domain.Strategy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.Strategy, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.strategies[id])
	}
	return out
}

// Enabled returns enabled strategies, optionally filtered by an allow-list.
func (r *Registry) Enabled(allow []string) []domain.Strategy {
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a] = true
	}
	out := []domain.Strategy{}
	for _, s := range r.All() {
		if !s.Enabled {
			continue
		}
		if len(allowed) > 0 && !allowed[s.ID] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Len returns the number of registered strategies.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// knownFeatures is the set of feature names a condition may reference. A typo
// in a YAML file would otherwise become a rule that silently never fires, which
// is the worst possible failure mode for a strategy definition.
var knownFeatures = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{
		domain.FeatLast, domain.FeatSMA20, domain.FeatSMA50, domain.FeatSMA200,
		domain.FeatEMA9, domain.FeatEMA21, domain.FeatVWAP, domain.FeatVWAPDist,
		domain.FeatRSI14, domain.FeatMACD, domain.FeatMACDSignal, domain.FeatMACDHist,
		domain.FeatATR14, domain.FeatATRPct, domain.FeatBBUpper, domain.FeatBBLower,
		domain.FeatBBWidth, domain.FeatBBPercentB, domain.FeatADX14, domain.FeatPlusDI,
		domain.FeatMinusDI, domain.FeatRelStrength, domain.FeatMomentum10,
		domain.FeatMomentum60, domain.FeatRetLog1, domain.FeatRetLog5,
		domain.FeatVolRatio, domain.FeatVolZScore, domain.FeatRealizedVol,
		domain.FeatVolOfVol, domain.FeatSupport, domain.FeatResistance,
		domain.FeatDistSupport, domain.FeatDistResist, domain.FeatBreakoutUp,
		domain.FeatBreakoutDown, domain.FeatGapPct, domain.FeatTrendSlope,
		domain.FeatZScore20,
	} {
		m[n] = true
	}
	return m
}()

// KnownFeature reports whether a feature name is produced by the feature engine.
func KnownFeature(n string) bool { return knownFeatures[n] }

var validOps = map[domain.Comparator]bool{
	domain.CmpGT: true, domain.CmpGTE: true, domain.CmpLT: true, domain.CmpLTE: true,
	domain.CmpBetween: true, domain.CmpOutside: true, domain.CmpCrossUp: true,
	domain.CmpCrossDown: true, domain.CmpTrue: true, domain.CmpFalse: true,
}

// Validate rejects malformed strategies with a precise, actionable message.
func Validate(s domain.Strategy) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if s.ID == "" {
		add("id is required")
	}
	if s.Version == "" {
		add("version is required: a strategy without a version cannot be attributed to a historical signal")
	}
	if len(s.Rules) == 0 {
		add("at least one rule is required")
	}
	if len(s.Invalidation) == 0 {
		add("at least one invalidation template is required (governance rule G-6)")
	}
	seen := map[string]bool{}
	for i, r := range s.Rules {
		where := fmt.Sprintf("rule[%d]", i)
		if r.ID == "" {
			add("%s: id is required", where)
		} else {
			if seen[r.ID] {
				add("%s: duplicate rule id %q", where, r.ID)
			}
			seen[r.ID] = true
			where = "rule " + r.ID
		}
		if r.Side != domain.SideLong && r.Side != domain.SideShort {
			add("%s: side must be LONG or SHORT, got %q", where, r.Side)
		}
		if r.Weight <= 0 {
			add("%s: weight must be positive (direction comes from side, not sign)", where)
		}
		if len(r.All)+len(r.Any)+len(r.None) == 0 {
			add("%s: must have at least one condition", where)
		}
		for _, rg := range r.Regimes {
			if !validRegime(rg) {
				add("%s: unknown regime %q", where, rg)
			}
		}
		checkConds := func(kind string, cs []domain.Condition) {
			for j, c := range cs {
				cw := fmt.Sprintf("%s.%s[%d]", where, kind, j)
				if c.Feature == "" {
					add("%s: feature is required", cw)
					continue
				}
				if !knownFeatures[c.Feature] {
					add("%s: unknown feature %q (see internal/domain/features.go for the produced set)", cw, c.Feature)
				}
				if !validOps[c.Op] {
					add("%s: unknown operator %q", cw, c.Op)
				}
				if c.Other != "" && !knownFeatures[c.Other] {
					add("%s: unknown comparison feature %q", cw, c.Other)
				}
				if (c.Op == domain.CmpBetween || c.Op == domain.CmpOutside) && c.Value == c.Value2 {
					add("%s: %s requires two distinct bounds", cw, c.Op)
				}
			}
		}
		checkConds("all", r.All)
		checkConds("any", r.Any)
		checkConds("none", r.None)
	}
	for _, rg := range s.Regimes {
		if !validRegime(rg) {
			add("unknown regime %q in strategy regimes", rg)
		}
	}
	for i, inv := range s.Invalidation {
		if inv.Kind == "" {
			add("invalidation[%d]: kind is required", i)
		}
		if inv.Description == "" {
			add("invalidation[%d]: description is required; it is shown to users", i)
		}
	}
	w := s.Weights.Map()
	total := 0.0
	for _, v := range w {
		if v < 0 {
			add("score weights must be non-negative")
			break
		}
		total += v
	}
	if total <= 0 {
		add("score weights must not all be zero")
	}
	if s.Policy.MinConfidence < 0 || s.Policy.MinConfidence > 1 {
		add("policy.min_confidence must be in [0,1]")
	}
	if s.Policy.StopATRMultiple <= 0 {
		add("policy.stop_atr_multiple must be positive: a signal without a stop reference has no risk/reward")
	}
	if s.Policy.TargetATRMultiple <= 0 {
		add("policy.target_atr_multiple must be positive")
	}
	if s.Policy.MaxWeight <= 0 || s.Policy.MaxWeight > 1 {
		add("policy.max_weight must be in (0,1]")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid strategy:\n  - %s", strings.Join(errs, "\n  - "))
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

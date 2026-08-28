package analyst

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Fact is one grounded value the narrative is permitted to cite.
type Fact struct {
	Key     string
	Value   float64
	Display string
	// Unit is informational: "bps", "%", "x", "usd", "count", "".
	Unit string
	// Numeric is false for facts whose value is categorical (a regime name, a
	// decision) and which therefore contribute no permitted numbers.
	Numeric bool
}

// FactSet is the complete set of values a narrative may reference.
type FactSet struct {
	facts map[string]Fact
	// allowed is the flattened set of permitted numeric values, including the
	// derived forms documented in Ground.
	allowed []float64
}

// NewFactSet returns an empty set.
func NewFactSet() *FactSet { return &FactSet{facts: map[string]Fact{}} }

// Add records a numeric fact.
func (f *FactSet) Add(key string, value float64, display, unit string) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	f.facts[key] = Fact{Key: key, Value: value, Display: display, Unit: unit, Numeric: true}
}

// AddText records a categorical fact, which contributes no permitted numbers.
func (f *FactSet) AddText(key, display string) {
	f.facts[key] = Fact{Key: key, Display: display}
}

// Keys returns fact keys in deterministic order.
func (f *FactSet) Keys() []string { return sortedKeys(f.facts) }

// Display returns a fact's rendered value.
func (f *FactSet) Display(key string) string { return f.facts[key].Display }

// Get returns a fact.
func (f *FactSet) Get(key string) (Fact, bool) {
	v, ok := f.facts[key]
	return v, ok
}

// Len returns the number of facts.
func (f *FactSet) Len() int { return len(f.facts) }

// numbers returns every numeric fact value.
func (f *FactSet) numbers() []float64 {
	out := make([]float64, 0, len(f.facts))
	for _, k := range f.Keys() {
		if f.facts[k].Numeric {
			out = append(out, f.facts[k].Value)
		}
	}
	return out
}

// BuildFacts flattens an analyst Input into the permitted fact set.
//
// This function is the trust boundary: a number the narrative cites must appear
// here, so anything omitted here is, by construction, something the model may
// not mention numerically.
func BuildFacts(in Input) *FactSet {
	f := NewFactSet()

	f.AddText("regime", string(in.Regime.Regime))
	f.Add("regime_confidence", round2(in.Regime.Confidence), fmt.Sprintf("%.2f", in.Regime.Confidence), "")
	f.Add("regime_volatility", round2(in.Regime.Volatility), fmt.Sprintf("%.2f", in.Regime.Volatility), "")
	f.Add("regime_breadth", round2(in.Regime.Breadth), fmt.Sprintf("%.2f", in.Regime.Breadth), "")
	f.Add("regime_momentum", round2(in.Regime.Momentum), fmt.Sprintf("%.2f", in.Regime.Momentum), "")

	if s := in.Score; s != nil {
		f.Add("composite_score", s.FinalScore, fmt.Sprintf("%.1f out of 100", s.FinalScore), "")
		f.AddText("classification", string(s.Classification))
		f.Add("technical_score", s.TechnicalScore, fmt.Sprintf("%.1f", s.TechnicalScore), "")
		f.Add("momentum_score", s.MomentumScore, fmt.Sprintf("%.1f", s.MomentumScore), "")
		f.Add("fundamental_score", s.FundamentalScore, fmt.Sprintf("%.1f", s.FundamentalScore), "")
		f.Add("quality_score", s.QualityScore, fmt.Sprintf("%.1f", s.QualityScore), "")
		f.Add("growth_score", s.GrowthScore, fmt.Sprintf("%.1f", s.GrowthScore), "")
		f.Add("valuation_score", s.ValuationScore, fmt.Sprintf("%.1f", s.ValuationScore), "")
		f.Add("sector_score", s.SectorScore, fmt.Sprintf("%.1f", s.SectorScore), "")
		f.Add("risk_score", s.RiskScore, fmt.Sprintf("%.1f", s.RiskScore), "")
		f.Add("event_risk_score", s.EventRiskScore, fmt.Sprintf("%.1f", s.EventRiskScore), "")
		if len(s.Missing) > 0 {
			f.AddText("unavailable_components", strings.Join(s.Missing, ", "))
		}
	}

	if p := in.Prediction; p != nil {
		f.AddText("model_version", p.ModelVersion)
		f.AddText("prediction_source", string(p.Source))
		for _, sc := range p.Scenarios {
			pre := "p_" + string(sc.Horizon) + "_"
			f.Add(pre+"up", round2(sc.Dist.Up), fmt.Sprintf("%.0f%%", sc.Dist.Up*100), "%")
			f.Add(pre+"flat", round2(sc.Dist.Flat), fmt.Sprintf("%.0f%%", sc.Dist.Flat*100), "%")
			f.Add(pre+"down", round2(sc.Dist.Down), fmt.Sprintf("%.0f%%", sc.Dist.Down*100), "%")
			f.Add(pre+"confidence", round2(sc.Confidence), fmt.Sprintf("%.2f", sc.Confidence), "")
			f.Add(pre+"flat_band_bps", sc.FlatBandBps, fmt.Sprintf("%.0f bps", sc.FlatBandBps), "bps")
			f.Add(pre+"expected_move_bps", round1(sc.ExpectedMoveBps), fmt.Sprintf("%.1f bps", sc.ExpectedMoveBps), "bps")
		}
		for _, c := range p.TopContributions(5) {
			f.Add("contribution_"+c.Feature, round4(c.Contribution),
				fmt.Sprintf("%.4f (feature value %.4f)", c.Contribution, c.Value), "")
		}
	}

	if o := in.Opportunity; o != nil {
		f.Add("opportunity_score", o.Score, fmt.Sprintf("%.1f out of 100", o.Score), "")
		f.AddText("bias", string(o.Bias))
		f.Add("opportunity_confidence", round2(o.Confidence), fmt.Sprintf("%.2f", o.Confidence), "")
		f.Add("risk_reward", round2(o.RiskReward), fmt.Sprintf("%.2f to 1", o.RiskReward), "x")
		f.Add("stop_distance_bps", round1(o.StopDistance), fmt.Sprintf("%.1f bps", o.StopDistance), "bps")
		f.Add("target_distance_bps", round1(o.TargetDistance), fmt.Sprintf("%.1f bps", o.TargetDistance), "bps")
		f.AddText("risk_level", string(o.Risk))
	}

	if r := in.Risk; r != nil {
		f.AddText("risk_decision", string(r.Decision))
		f.Add("risk_assessment_score", r.Score, fmt.Sprintf("%.0f out of 100", r.Score), "")
		for _, c := range r.Checks {
			if c.Status == domain.CheckSkip {
				continue
			}
			f.Add("check_"+c.Name, round4(c.Value), fmt.Sprintf("%.4g against threshold %.4g", c.Value, c.Threshold), "")
			f.Add("threshold_"+c.Name, round4(c.Threshold), fmt.Sprintf("%.4g", c.Threshold), "")
		}
	}

	if s := in.Signal; s != nil {
		f.AddText("signal_side", string(s.Side))
		f.Add("signal_strength", s.Strength, fmt.Sprintf("%.1f", s.Strength), "")
		f.Add("signal_confidence", round2(s.Confidence), fmt.Sprintf("%.2f", s.Confidence), "")
		f.Add("entry_reference", s.EntryReference, fmt.Sprintf("%.4f", s.EntryReference), "usd")
		f.Add("stop_reference", s.StopReference, fmt.Sprintf("%.4f", s.StopReference), "usd")
		f.Add("target_reference", s.TargetReference, fmt.Sprintf("%.4f", s.TargetReference), "usd")
		f.Add("suggested_weight", round4(s.SuggestedWeight), fmt.Sprintf("%.2f%% of paper equity", s.SuggestedWeight*100), "%")
	}

	if snap := in.Snapshot; snap != nil {
		for _, name := range snap.Names() {
			v, ok := snap.Get(name)
			if !ok {
				continue
			}
			f.Add("feature_"+name, round4(v), fmt.Sprintf("%.4g", v), "")
		}
		f.Add("spread_bps", round2(snap.SpreadBps), fmt.Sprintf("%.2f bps", snap.SpreadBps), "bps")
	}

	if v := in.Valuation; v != nil {
		if v.Available["pe"] {
			f.Add("pe", v.PE, fmt.Sprintf("%.2f", v.PE), "x")
		}
		if v.Available["peg"] {
			f.Add("peg", v.PEG, fmt.Sprintf("%.2f", v.PEG), "x")
		}
		if v.Available["ev_to_ebitda"] {
			f.Add("ev_to_ebitda", v.EVToEBITDA, fmt.Sprintf("%.2f", v.EVToEBITDA), "x")
		}
		if v.HistoryPeriods > 0 {
			f.Add("valuation_history_percentile", round2(v.HistoricalPercentile),
				fmt.Sprintf("%.0fth percentile of its own history", v.HistoricalPercentile*100), "%")
		}
	}

	if o := in.Options; o != nil && o.DataQuality != "none" {
		f.Add("implied_vol", round4(o.ImpliedVol), fmt.Sprintf("%.2f", o.ImpliedVol), "")
		f.Add("iv_rank", round2(o.IVRank), fmt.Sprintf("%.0f%%", o.IVRank*100), "%")
		f.Add("put_call_ratio", round2(o.PutCallRatio), fmt.Sprintf("%.2f", o.PutCallRatio), "x")
		f.AddText("options_data_quality", o.DataQuality)
	}

	for i, n := range in.News {
		f.AddText(fmt.Sprintf("news_%d_headline", i+1), n.Headline)
		f.AddText(fmt.Sprintf("news_%d_category", i+1), string(n.Category))
		f.Add(fmt.Sprintf("news_%d_confidence", i+1), round2(n.Confidence), fmt.Sprintf("%.2f", n.Confidence), "")
	}
	for i, e := range in.Events {
		f.AddText(fmt.Sprintf("event_%d", i+1),
			fmt.Sprintf("%s scheduled %s", e.Type, e.ScheduledAt.UTC().Format("2006-01-02 15:04Z")))
	}

	f.allowed = f.derive()
	return f
}

// derive expands the permitted numbers with the forms a careful writer would
// legitimately use: percentages of ratios, common roundings, absolute values,
// and differences between two supplied values.
func (f *FactSet) derive() []float64 {
	base := f.numbers()
	seen := map[float64]bool{}
	out := []float64{}
	add := func(v float64) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		k := math.Round(v*1e6) / 1e6
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, v)
	}
	for _, v := range base {
		add(v)
		add(-v)
		add(math.Abs(v))
		add(v * 100)
		add(v / 100)
		for _, d := range []int{0, 1, 2} {
			p := math.Pow(10, float64(d))
			add(math.Round(v*p) / p)
			add(math.Round(v*100*p) / p)
		}
	}
	// Differences between pairs: "the 0.63 up probability against 0.16 down is
	// a 0.47 spread" is a legitimate derivation.
	for i := 0; i < len(base); i++ {
		for j := i + 1; j < len(base); j++ {
			d := base[i] - base[j]
			add(d)
			add(-d)
			add(d * 100)
			add(math.Round(d*100) / 100)
			add(math.Round(d * 100))
		}
	}
	// Small counting integers: "three risk checks failed".
	for i := 0; i <= 20; i++ {
		add(float64(i))
	}
	// Scale denominators.
	for _, d := range scaleDenominators {
		add(d)
	}
	return out
}

// GroundingReport is the validator's verdict.
type GroundingReport struct {
	OK         bool     `json:"ok"`
	Checked    int      `json:"numbers_checked"`
	Ungrounded []string `json:"ungrounded,omitempty"`
	FactCount  int      `json:"fact_count"`
	Tolerance  float64  `json:"tolerance"`
}

// Summary renders a short human explanation.
func (g GroundingReport) Summary() string {
	if g.OK {
		return fmt.Sprintf("all %d numeric tokens matched the structured record", g.Checked)
	}
	return fmt.Sprintf("%d of %d numeric tokens are not present in the structured record: %s",
		len(g.Ungrounded), g.Checked, strings.Join(g.Ungrounded, ", "))
}

// numberToken matches a signed decimal with optional thousands separators and
// an optional trailing percent sign.
var numberToken = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?%?`)

// isoTimestamp matches timestamps so their digits are not treated as claims.
var isoTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2})?(?:\.\d+)?Z?)?`)

// horizonToken matches horizon labels like "15m", "60m", "1d", "5d", which are
// identifiers rather than quantities.
var horizonToken = regexp.MustCompile(`\b\d+(?:m|h|d|min|mins|minute|minutes|hour|hours|day|days)\b`)

// versionToken matches semantic versions such as "v1.2.0" or "1.2.0". A model
// version is a name, not a measurement, and its digits are not claims.
var versionToken = regexp.MustCompile(`\bv?\d+\.\d+(?:\.\d+)+\b`)

// hashToken matches content hashes and ids, whose hex digits are likewise not
// claims about the market.
var hashToken = regexp.MustCompile(`\b[0-9a-f]{8,}\b`)

// identifierToken matches names that happen to contain digits — feature keys
// like rsi_14, scenario keys like p_60m_up, model ids like direction-3class.
// A key is a label, not a measurement: scanning its digits as claims would
// reject every narrative that names the fields it is describing.
var identifierToken = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_.-]*[0-9][A-Za-z0-9_.-]*\b`)

// scaleDenominators are the universal denominators a writer uses to express a
// score or a percentage: "72.5 out of 100", "0.42 of 1". They carry no claim of
// their own, so allowing them is not a hole in the validator.
var scaleDenominators = []float64{1, 100, 1000, 10000}

// Ground checks every numeric token in text against the fact set.
//
// Tolerance is deliberately generous on the absolute side (0.005) so that a
// value rendered to two decimals matches its source, and tight on the relative
// side (1e-4) so that a large number cannot drift.
func Ground(text string, facts *FactSet) GroundingReport {
	rep := GroundingReport{OK: true, FactCount: facts.Len(), Tolerance: 0.005}
	if facts.allowed == nil {
		facts.allowed = facts.derive()
	}

	// Remove tokens that are identifiers, not quantities. Order matters:
	// timestamps and versions must go before the generic number scan, or their
	// components are read as claims.
	clean := isoTimestamp.ReplaceAllString(text, " ")
	clean = versionToken.ReplaceAllString(clean, " ")
	clean = hashToken.ReplaceAllString(clean, " ")
	clean = identifierToken.ReplaceAllString(clean, " ")
	clean = horizonToken.ReplaceAllString(clean, " ")

	for _, tok := range numberToken.FindAllString(clean, -1) {
		raw := strings.TrimSuffix(tok, "%")
		raw = strings.ReplaceAll(raw, ",", "")
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		rep.Checked++
		if !matches(v, facts.allowed, rep.Tolerance) {
			rep.OK = false
			rep.Ungrounded = append(rep.Ungrounded, tok)
		}
	}
	sort.Strings(rep.Ungrounded)
	rep.Ungrounded = dedupeStrings(rep.Ungrounded)
	if len(rep.Ungrounded) > 8 {
		rep.Ungrounded = rep.Ungrounded[:8]
	}
	return rep
}

func matches(v float64, allowed []float64, tol float64) bool {
	for _, a := range allowed {
		if math.Abs(a-v) <= tol {
			return true
		}
		if a != 0 && math.Abs((a-v)/a) <= 1e-4 {
			return true
		}
	}
	return false
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

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

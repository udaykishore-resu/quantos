// Package risk implements the risk engine described in ADR-009.
//
// The engine holds unconditional veto authority. It returns one of three
// decisions and resolution is most-severe-wins: there is no averaging, no
// weighting and no override, because averaging is exactly how a veto degrades
// into a suggestion. `domain.NewSignal` refuses to construct a signal without
// an allowing assessment, so the guarantee is structural rather than a matter
// of every caller remembering to check.
package risk

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Config holds every threshold. Thresholds live in configuration and the config
// hash is stamped on each assessment, so a historical veto can be re-derived
// even after the thresholds change.
type Config struct {
	StalenessSoft time.Duration
	StalenessHard time.Duration

	MinLiquidity       float64 // average daily notional, USD
	PreferredLiquidity float64

	MaxSpreadSoft float64 // basis points
	MaxSpreadHard float64

	MaxVolSoft float64 // annualised realised volatility
	MaxVolHard float64

	EarningsWatch time.Duration
	EarningsBlock time.Duration

	MinConfidence       float64
	MinRegimeConfidence float64
	BlockedRegimes      []domain.Regime

	MaxPositionWeight  float64
	SoftPositionWeight float64
	MaxSectorWeight    float64
	SoftSectorWeight   float64

	MaxCorrelatedExposure  float64
	SoftCorrelatedExposure float64

	MaxDrawdown  float64
	DrawdownWarn float64

	ConfigHash string
	Clock      obs.Clock
	Metrics    *obs.Metrics
}

// DefaultConfig returns the shipped thresholds.
func DefaultConfig() Config {
	return Config{
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
		MaxPositionWeight:      0.10,
		SoftPositionWeight:     0.07,
		MaxSectorWeight:        0.35,
		SoftSectorWeight:       0.25,
		MaxCorrelatedExposure:  0.50,
		SoftCorrelatedExposure: 0.35,
		MaxDrawdown:            0.20,
		DrawdownWarn:           0.12,
		Clock:                  obs.SystemClock{},
	}
}

// Input is everything the engine evaluates. Fields that are absent produce a
// SKIP result on the corresponding check, recorded explicitly — a check that
// could not run is never silently treated as a pass.
type Input struct {
	Ticker     domain.Ticker
	Stock      domain.Stock
	Snapshot   domain.FeatureSnapshot
	Prediction *domain.Prediction
	Regime     domain.MarketRegime
	Portfolio  *domain.Portfolio
	Events     []domain.CorporateEvent
	Health     *domain.ModelHealth
	// ProposedWeight is the fraction of equity the candidate signal would use.
	ProposedWeight float64
	// CorrelatedExposure is the portfolio's existing exposure to instruments
	// correlated with this one, as a fraction of equity.
	CorrelatedExposure float64
	// Side is the direction under consideration; some checks are directional.
	Side domain.Side
	Now  time.Time
}

// Engine evaluates risk.
type Engine struct{ cfg Config }

// NewEngine builds a risk engine.
func NewEngine(cfg Config) *Engine {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	return &Engine{cfg: cfg}
}

// Assess runs every check and resolves the verdict.
func (e *Engine) Assess(in Input) domain.RiskAssessment {
	now := in.Now
	if now.IsZero() {
		now = e.cfg.Clock.Now()
	}
	checks := []domain.RiskCheck{
		e.checkFreshness(in, now),
		e.checkLiquidity(in),
		e.checkSpread(in),
		e.checkVolatility(in),
		e.checkEventRisk(in, now),
		e.checkConfidence(in),
		e.checkModelHealth(in),
		e.checkRegime(in),
		e.checkConcentration(in),
		e.checkCorrelation(in),
		e.checkDrawdown(in),
	}

	a := domain.RiskAssessment{
		ID:          bus.NewEventID(now),
		Ticker:      in.Ticker,
		CreatedAt:   now,
		EvaluatedAt: now,
		Checks:      checks,
		ConfigHash:  e.cfg.ConfigHash,
		Regime:      in.Regime.Regime,
		FeatureHash: in.Snapshot.Hash,
	}
	if in.Prediction != nil {
		a.PredictionID = in.Prediction.ID
	}

	// Most severe wins. No averaging, deliberately.
	decision := domain.RiskAllowPaperSignal
	for _, c := range checks {
		if c.Decision.Severity() > decision.Severity() {
			decision = c.Decision
		}
		switch c.Status {
		case domain.CheckFail:
			a.Blockers = append(a.Blockers, c.Reason)
		case domain.CheckWarn:
			a.Warnings = append(a.Warnings, c.Reason)
		}
	}
	a.Decision = decision
	a.Score, a.Level = e.grade(checks)

	if e.cfg.Metrics != nil {
		reason := "none"
		if len(a.Blockers) > 0 {
			reason = firstReasonName(checks, domain.CheckFail)
		} else if len(a.Warnings) > 0 {
			reason = firstReasonName(checks, domain.CheckWarn)
		}
		e.cfg.Metrics.RiskDecisions.Inc(string(a.Decision), reason)
	}
	return a
}

func firstReasonName(checks []domain.RiskCheck, status domain.CheckStatus) string {
	for _, c := range checks {
		if c.Status == status {
			return c.Name
		}
	}
	return "none"
}

// grade produces a 0..100 risk score for display. It is descriptive only: the
// Decision is what is enforced, and no consumer may derive a decision from the
// score.
func (e *Engine) grade(checks []domain.RiskCheck) (float64, domain.RiskLevel) {
	score, n := 0.0, 0
	for _, c := range checks {
		switch c.Status {
		case domain.CheckFail:
			score += 100
			n++
		case domain.CheckWarn:
			score += 55
			n++
		case domain.CheckPass:
			score += 15
			n++
		}
	}
	if n == 0 {
		return 50, domain.RiskMedium
	}
	s := score / float64(n)
	switch {
	case s >= 70:
		return math.Round(s), domain.RiskExtreme
	case s >= 50:
		return math.Round(s), domain.RiskHigh
	case s >= 30:
		return math.Round(s), domain.RiskMedium
	default:
		return math.Round(s), domain.RiskLow
	}
}

func pass(name string, v, thr float64, reason string) domain.RiskCheck {
	return domain.RiskCheck{Name: name, Status: domain.CheckPass, Decision: domain.RiskAllowPaperSignal, Value: v, Threshold: thr, Reason: reason}
}

func warn(name string, v, thr float64, reason string) domain.RiskCheck {
	return domain.RiskCheck{Name: name, Status: domain.CheckWarn, Decision: domain.RiskWatchOnly, Value: v, Threshold: thr, Reason: reason}
}

func fail(name string, v, thr float64, reason string) domain.RiskCheck {
	return domain.RiskCheck{Name: name, Status: domain.CheckFail, Decision: domain.RiskBlock, Value: v, Threshold: thr, Reason: reason}
}

func skip(name, why string) domain.RiskCheck {
	// A skipped check is WATCH_ONLY, not ALLOW. If we cannot evaluate a risk we
	// do not get to assume it is absent.
	return domain.RiskCheck{Name: name, Status: domain.CheckSkip, Decision: domain.RiskWatchOnly, Skipped: why,
		Reason: fmt.Sprintf("%s could not be evaluated: %s", name, why)}
}

func (e *Engine) checkFreshness(in Input, now time.Time) domain.RiskCheck {
	if in.Snapshot.LastTickAt.IsZero() {
		return skip(domain.CheckFreshness, "no market data timestamp on the snapshot")
	}
	age := now.Sub(in.Snapshot.LastTickAt)
	secs := age.Seconds()
	switch {
	case age >= e.cfg.StalenessHard:
		return fail(domain.CheckFreshness, secs, e.cfg.StalenessHard.Seconds(),
			fmt.Sprintf("market data is %s old, beyond the hard staleness threshold of %s", age.Truncate(time.Second), e.cfg.StalenessHard))
	case age >= e.cfg.StalenessSoft:
		return warn(domain.CheckFreshness, secs, e.cfg.StalenessSoft.Seconds(),
			fmt.Sprintf("market data is %s old, beyond the soft staleness threshold of %s", age.Truncate(time.Second), e.cfg.StalenessSoft))
	}
	return pass(domain.CheckFreshness, secs, e.cfg.StalenessSoft.Seconds(),
		fmt.Sprintf("market data is current (%s old)", age.Truncate(time.Second)))
}

func (e *Engine) checkLiquidity(in Input) domain.RiskCheck {
	notional := in.Stock.AvgNotional
	if notional <= 0 {
		return skip(domain.CheckLiquidity, "no average traded notional available for the instrument")
	}
	switch {
	case notional < e.cfg.MinLiquidity:
		return fail(domain.CheckLiquidity, notional, e.cfg.MinLiquidity,
			fmt.Sprintf("average daily notional of $%.1fM is below the $%.1fM minimum", notional/1e6, e.cfg.MinLiquidity/1e6))
	case notional < e.cfg.PreferredLiquidity:
		return warn(domain.CheckLiquidity, notional, e.cfg.PreferredLiquidity,
			fmt.Sprintf("average daily notional of $%.1fM is below the $%.1fM preferred level", notional/1e6, e.cfg.PreferredLiquidity/1e6))
	}
	return pass(domain.CheckLiquidity, notional, e.cfg.PreferredLiquidity,
		fmt.Sprintf("average daily notional of $%.1fM", notional/1e6))
}

func (e *Engine) checkSpread(in Input) domain.RiskCheck {
	s := in.Snapshot.SpreadBps
	if s <= 0 {
		return skip(domain.CheckSpread, "no two-sided quote available")
	}
	switch {
	case s > e.cfg.MaxSpreadHard:
		return fail(domain.CheckSpread, s, e.cfg.MaxSpreadHard,
			fmt.Sprintf("quoted spread of %.1f bps exceeds the hard limit of %.0f bps", s, e.cfg.MaxSpreadHard))
	case s > e.cfg.MaxSpreadSoft:
		return warn(domain.CheckSpread, s, e.cfg.MaxSpreadSoft,
			fmt.Sprintf("quoted spread of %.1f bps exceeds the soft limit of %.0f bps", s, e.cfg.MaxSpreadSoft))
	}
	return pass(domain.CheckSpread, s, e.cfg.MaxSpreadSoft, fmt.Sprintf("quoted spread of %.1f bps", s))
}

func (e *Engine) checkVolatility(in Input) domain.RiskCheck {
	v, ok := in.Snapshot.Get(domain.FeatRealizedVol)
	if !ok {
		return skip(domain.CheckVolatility, "realised volatility is not yet warm")
	}
	switch {
	case v > e.cfg.MaxVolHard:
		return fail(domain.CheckVolatility, v, e.cfg.MaxVolHard,
			fmt.Sprintf("annualised realised volatility of %.0f%% exceeds the hard limit of %.0f%%", v*100, e.cfg.MaxVolHard*100))
	case v > e.cfg.MaxVolSoft:
		return warn(domain.CheckVolatility, v, e.cfg.MaxVolSoft,
			fmt.Sprintf("annualised realised volatility of %.0f%% exceeds the soft limit of %.0f%%", v*100, e.cfg.MaxVolSoft*100))
	}
	return pass(domain.CheckVolatility, v, e.cfg.MaxVolSoft,
		fmt.Sprintf("annualised realised volatility of %.0f%%", v*100))
}

// checkEventRisk blocks around scheduled events. Earnings are the canonical
// case: the model's training distribution does not contain the discontinuity an
// earnings release introduces, so its probabilities are not meaningful there.
func (e *Engine) checkEventRisk(in Input, now time.Time) domain.RiskCheck {
	if len(in.Events) == 0 {
		return pass(domain.CheckEventRisk, 0, e.cfg.EarningsBlock.Hours(), "no scheduled events within the monitored window")
	}
	var nearest *domain.CorporateEvent
	for i := range in.Events {
		ev := in.Events[i]
		if ev.ScheduledAt.Before(now) {
			continue
		}
		if nearest == nil || ev.ScheduledAt.Before(nearest.ScheduledAt) {
			nearest = &in.Events[i]
		}
	}
	if nearest == nil {
		return pass(domain.CheckEventRisk, 0, e.cfg.EarningsBlock.Hours(), "no upcoming scheduled events")
	}
	until := nearest.ScheduledAt.Sub(now)
	hrs := until.Hours()
	material := nearest.Type == domain.EventEarnings || nearest.Type == domain.EventGuidance || nearest.Type == domain.EventMerger
	switch {
	case material && until <= e.cfg.EarningsBlock:
		return fail(domain.CheckEventRisk, hrs, e.cfg.EarningsBlock.Hours(),
			fmt.Sprintf("%s scheduled in %.1f hours, inside the %.0f-hour blocking window", strings.ToLower(string(nearest.Type)), hrs, e.cfg.EarningsBlock.Hours()))
	case material && until <= e.cfg.EarningsWatch:
		return warn(domain.CheckEventRisk, hrs, e.cfg.EarningsWatch.Hours(),
			fmt.Sprintf("%s scheduled in %.1f hours, inside the %.0f-hour watch window", strings.ToLower(string(nearest.Type)), hrs, e.cfg.EarningsWatch.Hours()))
	}
	return pass(domain.CheckEventRisk, hrs, e.cfg.EarningsWatch.Hours(),
		fmt.Sprintf("next scheduled event (%s) is %.1f hours away", strings.ToLower(string(nearest.Type)), hrs))
}

func (e *Engine) checkConfidence(in Input) domain.RiskCheck {
	if in.Prediction == nil {
		return skip(domain.CheckConfidence, "no prediction supplied")
	}
	s := in.Prediction.Primary()
	if s.Confidence < e.cfg.MinConfidence {
		return warn(domain.CheckConfidence, s.Confidence, e.cfg.MinConfidence,
			fmt.Sprintf("model confidence of %.2f is below the %.2f minimum for a paper signal", s.Confidence, e.cfg.MinConfidence))
	}
	return pass(domain.CheckConfidence, s.Confidence, e.cfg.MinConfidence,
		fmt.Sprintf("model confidence of %.2f", s.Confidence))
}

func (e *Engine) checkModelHealth(in Input) domain.RiskCheck {
	if in.Health == nil {
		// Distinguish "no model to be unhealthy" from "a model is serving and
		// we have not measured it". The first is a known, bounded state: the
		// prediction came from the deterministic rule prior, whose confidence
		// is already capped, so treating it as an unevaluated risk would
		// deadlock the platform — it could never emit its first signal.
		if in.Prediction != nil && in.Prediction.Source == domain.SourceRulesFallback {
			return pass(domain.CheckModelHealth, 0, 3,
				"no trained model is serving; predictions come from the deterministic rule prior with a capped confidence")
		}
		return warn(domain.CheckModelHealth, 0, 3,
			"a model is serving but has no health record yet: its accuracy and calibration are unmeasured")
	}
	if in.Health.Provisional {
		// The model has an offline report card but no live evidence yet. That
		// is a genuine measurement — made on data the model never saw — so it
		// is allowed to serve, and the record says plainly which kind of
		// measurement it is. Live outcomes replace it as soon as enough
		// predictions resolve.
		if !in.Health.Healthy {
			return fail(domain.CheckModelHealth, 2, 3, fmt.Sprintf(
				"model %s failed its own offline evaluation: %s",
				in.Health.ModelVersion, strings.Join(in.Health.Issues, "; ")))
		}
		return pass(domain.CheckModelHealth, 1, 3, fmt.Sprintf(
			"model %s is serving on its offline test result (accuracy %.3f); no live predictions have resolved yet",
			in.Health.ModelVersion, in.Health.Accuracy))
	}
	switch in.Health.Drift {
	case domain.DriftSevere:
		return fail(domain.CheckModelHealth, 3, 3,
			fmt.Sprintf("model %s is in severe drift; its probabilities are not currently trustworthy", in.Health.ModelVersion))
	case domain.DriftModerate:
		return warn(domain.CheckModelHealth, 2, 3,
			fmt.Sprintf("model %s is in moderate drift", in.Health.ModelVersion))
	}
	if !in.Health.Healthy {
		return warn(domain.CheckModelHealth, 1, 3,
			fmt.Sprintf("model %s is flagged unhealthy: %s", in.Health.ModelVersion, strings.Join(in.Health.Issues, "; ")))
	}
	return pass(domain.CheckModelHealth, 0, 3, fmt.Sprintf("model %s is healthy", in.Health.ModelVersion))
}

func (e *Engine) checkRegime(in Input) domain.RiskCheck {
	for _, r := range e.cfg.BlockedRegimes {
		if r == in.Regime.Regime {
			return fail(domain.CheckRegime, 0, 0,
				fmt.Sprintf("market regime %s is on the blocked list", in.Regime.Regime))
		}
	}
	// A long setup in a confirmed risk-off, high-volatility market is exactly
	// the situation where the model is least reliable.
	if in.Side == domain.SideLong && in.Regime.Regime == domain.RegimeRiskOff && in.Regime.Volatility > 0.7 {
		return fail(domain.CheckRegime, in.Regime.Volatility, 0.7,
			fmt.Sprintf("long setup in a risk-off regime with volatility at %.0f%% of the normalised range", in.Regime.Volatility*100))
	}
	if in.Regime.Confidence < e.cfg.MinRegimeConfidence {
		return warn(domain.CheckRegime, in.Regime.Confidence, e.cfg.MinRegimeConfidence,
			fmt.Sprintf("regime classification confidence of %.2f is below the %.2f minimum", in.Regime.Confidence, e.cfg.MinRegimeConfidence))
	}
	return pass(domain.CheckRegime, in.Regime.Confidence, e.cfg.MinRegimeConfidence,
		fmt.Sprintf("regime %s at confidence %.2f", in.Regime.Regime, in.Regime.Confidence))
}

func (e *Engine) checkConcentration(in Input) domain.RiskCheck {
	if in.Portfolio == nil {
		return skip(domain.CheckConcentration, "no portfolio state available")
	}
	if in.Portfolio.Equity <= 0 {
		return skip(domain.CheckConcentration, "portfolio equity is zero")
	}
	existing := 0.0
	for _, p := range in.Portfolio.Positions {
		if p.Ticker == in.Ticker {
			existing += math.Abs(p.MarketValue) / in.Portfolio.Equity
		}
	}
	proposed := existing + in.ProposedWeight
	sector := in.Portfolio.SectorExposure[in.Stock.Sector] + in.ProposedWeight

	switch {
	case proposed > e.cfg.MaxPositionWeight:
		return fail(domain.CheckConcentration, proposed, e.cfg.MaxPositionWeight,
			fmt.Sprintf("resulting position weight of %.1f%% would exceed the %.1f%% hard cap", proposed*100, e.cfg.MaxPositionWeight*100))
	case sector > e.cfg.MaxSectorWeight:
		return fail(domain.CheckConcentration, sector, e.cfg.MaxSectorWeight,
			fmt.Sprintf("resulting %s sector weight of %.1f%% would exceed the %.1f%% hard cap", in.Stock.Sector, sector*100, e.cfg.MaxSectorWeight*100))
	case proposed > e.cfg.SoftPositionWeight:
		return warn(domain.CheckConcentration, proposed, e.cfg.SoftPositionWeight,
			fmt.Sprintf("resulting position weight of %.1f%% exceeds the %.1f%% soft cap", proposed*100, e.cfg.SoftPositionWeight*100))
	case sector > e.cfg.SoftSectorWeight:
		return warn(domain.CheckConcentration, sector, e.cfg.SoftSectorWeight,
			fmt.Sprintf("resulting %s sector weight of %.1f%% exceeds the %.1f%% soft cap", in.Stock.Sector, sector*100, e.cfg.SoftSectorWeight*100))
	}
	return pass(domain.CheckConcentration, proposed, e.cfg.SoftPositionWeight,
		fmt.Sprintf("resulting position weight of %.1f%%", proposed*100))
}

func (e *Engine) checkCorrelation(in Input) domain.RiskCheck {
	if in.Portfolio == nil {
		return skip(domain.CheckCorrelation, "no portfolio state available")
	}
	x := in.CorrelatedExposure + in.ProposedWeight
	switch {
	case x > e.cfg.MaxCorrelatedExposure:
		return fail(domain.CheckCorrelation, x, e.cfg.MaxCorrelatedExposure,
			fmt.Sprintf("correlation-adjusted exposure of %.1f%% would exceed the %.1f%% hard cap", x*100, e.cfg.MaxCorrelatedExposure*100))
	case x > e.cfg.SoftCorrelatedExposure:
		return warn(domain.CheckCorrelation, x, e.cfg.SoftCorrelatedExposure,
			fmt.Sprintf("correlation-adjusted exposure of %.1f%% exceeds the %.1f%% soft cap", x*100, e.cfg.SoftCorrelatedExposure*100))
	}
	return pass(domain.CheckCorrelation, x, e.cfg.SoftCorrelatedExposure,
		fmt.Sprintf("correlation-adjusted exposure of %.1f%%", x*100))
}

// checkDrawdown is the portfolio kill switch.
func (e *Engine) checkDrawdown(in Input) domain.RiskCheck {
	if in.Portfolio == nil {
		return skip(domain.CheckDrawdown, "no portfolio state available")
	}
	d := in.Portfolio.Drawdown
	switch {
	case d >= e.cfg.MaxDrawdown:
		return fail(domain.CheckDrawdown, d, e.cfg.MaxDrawdown,
			fmt.Sprintf("portfolio drawdown of %.1f%% has reached the %.1f%% kill switch; no new signals until it recovers", d*100, e.cfg.MaxDrawdown*100))
	case d >= e.cfg.DrawdownWarn:
		return warn(domain.CheckDrawdown, d, e.cfg.DrawdownWarn,
			fmt.Sprintf("portfolio drawdown of %.1f%% exceeds the %.1f%% warning level", d*100, e.cfg.DrawdownWarn*100))
	}
	return pass(domain.CheckDrawdown, d, e.cfg.DrawdownWarn, fmt.Sprintf("portfolio drawdown of %.1f%%", d*100))
}

// VetoStats summarises the cost of the veto over a set of assessments, which is
// what makes the risk engine falsifiable rather than superstitious (ADR-009).
type VetoStats struct {
	Total          int                         `json:"total"`
	Allowed        int                         `json:"allowed"`
	WatchOnly      int                         `json:"watch_only"`
	Blocked        int                         `json:"blocked"`
	VetoRate       float64                     `json:"veto_rate"`
	ByCheck        map[string]int              `json:"by_check"`
	TopBlockReason string                      `json:"top_block_reason,omitempty"`
	ByDecision     map[domain.RiskDecision]int `json:"by_decision"`
}

// Summarise aggregates assessments into veto statistics.
func Summarise(assessments []domain.RiskAssessment) VetoStats {
	s := VetoStats{ByCheck: map[string]int{}, ByDecision: map[domain.RiskDecision]int{}}
	for _, a := range assessments {
		s.Total++
		s.ByDecision[a.Decision]++
		switch a.Decision {
		case domain.RiskAllowPaperSignal:
			s.Allowed++
		case domain.RiskWatchOnly:
			s.WatchOnly++
		case domain.RiskBlock:
			s.Blocked++
		}
		for _, c := range a.Checks {
			if c.Status == domain.CheckFail {
				s.ByCheck[c.Name]++
			}
		}
	}
	if s.Total > 0 {
		s.VetoRate = float64(s.Blocked+s.WatchOnly) / float64(s.Total)
	}
	type kv struct {
		k string
		v int
	}
	list := make([]kv, 0, len(s.ByCheck))
	for k, v := range s.ByCheck {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v == list[j].v {
			return list[i].k < list[j].k
		}
		return list[i].v > list[j].v
	})
	if len(list) > 0 {
		s.TopBlockReason = list[0].k
	}
	return s
}

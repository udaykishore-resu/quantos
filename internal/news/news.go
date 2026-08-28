// Package news turns raw headlines into typed, bounded events.
//
// The order is fixed and matters (ADR-005, requirement §12): deterministic
// preprocessing runs first and always; a language model may only refine the
// result afterwards, and only into the same typed schema. If the model is
// unavailable, disagrees implausibly, or returns something unparseable, the
// deterministic classification stands. The raw article text never reaches the
// decision path — only the typed fields do, and only through
// NewsEvent.SignedImpact.
package news

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/llm"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Article is a raw input item.
type Article struct {
	ID        string        `json:"id"`
	Ticker    domain.Ticker `json:"ticker"`
	Headline  string        `json:"headline"`
	Body      string        `json:"body,omitempty"`
	Source    string        `json:"source"`
	URL       string        `json:"url,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
	Sector    string        `json:"sector,omitempty"`
}

// Source supplies articles.
type Source interface {
	Name() string
	Fetch(ctx context.Context, since time.Time) ([]Article, error)
}

// Config parameterises the pipeline.
type Config struct {
	DedupWindow    time.Duration
	MinMateriality float64
	UseLLM         bool
	Clock          obs.Clock
	Metrics        *obs.Metrics
}

// DefaultConfig returns sensible parameters.
func DefaultConfig() Config {
	return Config{
		DedupWindow: 6 * time.Hour, MinMateriality: 0.3,
		UseLLM: true, Clock: obs.SystemClock{},
	}
}

// Pipeline processes articles into events.
type Pipeline struct {
	cfg      Config
	provider llm.Provider

	mu      sync.Mutex
	seen    map[string]time.Time // cluster key -> first seen
	lastRun time.Time
}

// NewPipeline builds a news pipeline. A nil provider disables enrichment; the
// deterministic stage still runs and the platform still works.
func NewPipeline(cfg Config, p llm.Provider) *Pipeline {
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.DedupWindow <= 0 {
		cfg.DedupWindow = 6 * time.Hour
	}
	return &Pipeline{cfg: cfg, provider: p, seen: map[string]time.Time{}}
}

// ---------------------------------------------------------------------------
// Deterministic stage
// ---------------------------------------------------------------------------

type keywordRule struct {
	category domain.NewsCategory
	// materiality is the base score for this category.
	materiality float64
	terms       []string
}

// categoryRules map headline vocabulary to a category and a base materiality.
// The list is explicit and auditable, which a learned classifier would not be.
var categoryRules = []keywordRule{
	{domain.NewsEarnings, 0.90, []string{"earnings", "eps", "quarterly results", "q1 results", "q2 results", "q3 results", "q4 results", "beats estimates", "misses estimates", "reports revenue"}},
	{domain.NewsGuidance, 0.85, []string{"guidance", "outlook", "forecast", "raises outlook", "cuts outlook", "warns", "pre-announce"}},
	{domain.NewsMA, 0.95, []string{"acquire", "acquisition", "merger", "takeover", "buyout", "to be acquired", "bid for", "stake in"}},
	{domain.NewsRegulatory, 0.70, []string{"sec ", "regulator", "antitrust", "investigation", "probe", "fda", "approval", "recall", "sanction"}},
	{domain.NewsLegal, 0.60, []string{"lawsuit", "sues", "settlement", "court", "verdict", "class action", "patent dispute"}},
	{domain.NewsManagement, 0.55, []string{"ceo", "cfo", "chief executive", "resigns", "steps down", "appoints", "names new"}},
	{domain.NewsAnalyst, 0.45, []string{"upgrade", "downgrade", "price target", "initiates coverage", "reiterates", "overweight", "underweight"}},
	{domain.NewsProduct, 0.40, []string{"launch", "unveils", "announces product", "partnership", "contract win", "deal with"}},
	{domain.NewsMacroNews, 0.65, []string{"fed", "federal reserve", "inflation", "cpi", "jobs report", "nonfarm", "rate decision", "gdp", "tariff"}},
}

// Sentiment lexicons. Deliberately small and specific to finance: a general
// sentiment model would score "beats" and "misses" as ordinary English.
var positiveTerms = map[string]float64{
	"beats": 0.7, "beat": 0.6, "tops": 0.6, "raises": 0.6, "raised": 0.6,
	"upgrade": 0.7, "upgraded": 0.7, "record": 0.5, "surges": 0.6, "soars": 0.7,
	"approval": 0.6, "approved": 0.6, "wins": 0.5, "win": 0.4, "expands": 0.4,
	"strong": 0.5, "growth": 0.4, "outperform": 0.6, "overweight": 0.5,
	"buyback": 0.5, "dividend increase": 0.6, "acquires": 0.3, "partnership": 0.4,
	"exceeded": 0.6, "accelerating": 0.5, "profitable": 0.5,
}

var negativeTerms = map[string]float64{
	"misses": -0.7, "miss": -0.6, "cuts": -0.6, "cut": -0.5, "downgrade": -0.7,
	"downgraded": -0.7, "plunges": -0.8, "falls": -0.4, "warns": -0.7,
	"warning": -0.6, "investigation": -0.6, "probe": -0.6, "lawsuit": -0.5,
	"recall": -0.7, "resigns": -0.5, "steps down": -0.4, "layoffs": -0.5,
	"weak": -0.5, "declines": -0.5, "underperform": -0.6, "underweight": -0.5,
	"delays": -0.5, "delay": -0.4, "halts": -0.6, "bankruptcy": -0.9,
	"fraud": -0.9, "restatement": -0.8, "shortfall": -0.6, "slowing": -0.5,
}

// negation flips the polarity of a following term within a short window, which
// is the single highest-value correction a lexicon approach can make.
var negationTerms = []string{"not ", "no ", "without ", "fails to ", "denies ", "rejects "}

var wordSplit = regexp.MustCompile(`[^a-z0-9$%.-]+`)

// Classify performs the deterministic stage. It is a pure function: the same
// article always produces the same event, which is what makes news-driven
// behaviour reproducible in backtests.
func Classify(a Article, now time.Time) domain.NewsEvent {
	text := strings.ToLower(a.Headline + " " + a.Body)

	category, base := domain.NewsGeneral, 0.25
	var matched []string
	for _, r := range categoryRules {
		for _, t := range r.terms {
			if strings.Contains(text, t) {
				if r.materiality > base {
					category, base = r.category, r.materiality
				}
				matched = append(matched, t)
			}
		}
	}

	score, hits := lexiconSentiment(text)
	matched = append(matched, hits...)

	sentiment := domain.SentimentNeutral
	switch {
	case score > 0.15:
		sentiment = domain.SentimentPositive
	case score < -0.15:
		sentiment = domain.SentimentNegative
	}

	// Materiality combines the category's base with the strength of the
	// sentiment signal: a strongly-worded analyst note matters more than a
	// neutral one, but never as much as an acquisition.
	materiality := domain.Clamp01(base*0.75 + absf(score)*0.25)
	mLabel := domain.MaterialityLow
	switch {
	case materiality >= 0.7:
		mLabel = domain.MaterialityHigh
	case materiality >= 0.4:
		mLabel = domain.MaterialityMedium
	}

	// Confidence reflects how much evidence the classifier actually found.
	// A single weak keyword match should not produce a confident label.
	confidence := domain.Clamp(0.35+0.12*float64(len(matched)), 0, 0.85)
	if category == domain.NewsGeneral {
		confidence = domain.Clamp(confidence*0.6, 0, 0.5)
	}

	horizon := domain.Horizon60m
	switch category {
	case domain.NewsEarnings, domain.NewsGuidance, domain.NewsMA:
		horizon = domain.Horizon1d
	case domain.NewsRegulatory, domain.NewsLegal:
		horizon = domain.Horizon5d
	case domain.NewsAnalyst, domain.NewsProduct:
		horizon = domain.Horizon60m
	case domain.NewsMacroNews:
		horizon = domain.Horizon1d
	}

	id := a.ID
	if id == "" {
		id = bus.NewEventID(now)
	}
	return domain.NewsEvent{
		ID:               id,
		Ticker:           a.Ticker,
		Timestamp:        a.Timestamp,
		IngestedAt:       now,
		Source:           a.Source,
		Headline:         a.Headline,
		URL:              a.URL,
		Category:         category,
		Sentiment:        sentiment,
		SentimentScore:   round2(score),
		Materiality:      mLabel,
		MaterialityScore: round2(materiality),
		Confidence:       round2(confidence),
		AffectedSector:   a.Sector,
		ExpectedHorizon:  horizon,
		Stage:            "deterministic",
		Matched:          dedupe(matched),
		ClusterID:        clusterKey(a),
	}
}

func lexiconSentiment(text string) (float64, []string) {
	words := wordSplit.Split(text, -1)
	score, n := 0.0, 0
	var hits []string
	for i, w := range words {
		apply := func(v float64, term string) {
			neg := false
			// Look back three tokens for a negation.
			for j := i - 3; j < i; j++ {
				if j < 0 || j >= len(words) {
					continue
				}
				for _, t := range negationTerms {
					if strings.TrimSpace(t) == words[j] {
						neg = true
					}
				}
			}
			if neg {
				v = -v * 0.8
				term = "not " + term
			}
			score += v
			n++
			hits = append(hits, term)
		}
		if v, ok := positiveTerms[w]; ok {
			apply(v, w)
		}
		if v, ok := negativeTerms[w]; ok {
			apply(v, w)
		}
	}
	// Multi-word phrases the tokeniser cannot see.
	for phrase, v := range map[string]float64{
		"steps down": -0.4, "dividend increase": 0.6, "class action": -0.5,
		"price target": 0.0, "beats estimates": 0.8, "misses estimates": -0.8,
	} {
		if strings.Contains(text, phrase) {
			score += v
			n++
			hits = append(hits, phrase)
		}
	}
	if n == 0 {
		return 0, nil
	}
	// Average rather than sum, so a long article is not automatically extreme.
	return domain.Clamp(score/float64(n), -1, 1), hits
}

// clusterKey groups near-duplicate headlines from different outlets. Normalised
// significant words plus the ticker is crude but effective, and crucially it is
// deterministic, so the same duplicate is suppressed on every replay.
func clusterKey(a Article) string {
	words := wordSplit.Split(strings.ToLower(a.Headline), -1)
	keep := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < 4 || stopWords[w] {
			continue
		}
		keep = append(keep, w)
	}
	sort.Strings(keep)
	if len(keep) > 8 {
		keep = keep[:8]
	}
	sum := sha256.Sum256([]byte(string(a.Ticker) + "|" + strings.Join(keep, " ")))
	return hex.EncodeToString(sum[:8])
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"from": true, "this": true, "will": true, "says": true, "after": true,
	"amid": true, "over": true, "into": true, "than": true, "more": true,
	"shares": true, "stock": true, "company": true, "reports": true,
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

// Process classifies articles, suppresses duplicates and optionally enriches
// with a language model.
func (p *Pipeline) Process(ctx context.Context, articles []Article) []domain.NewsEvent {
	now := p.cfg.Clock.Now()
	out := make([]domain.NewsEvent, 0, len(articles))

	// Deterministic order so replay produces identical output.
	sorted := append([]Article(nil), articles...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Timestamp.Equal(sorted[j].Timestamp) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	for _, a := range sorted {
		ev := Classify(a, now)

		p.mu.Lock()
		first, dup := p.seen[ev.ClusterID]
		if dup && now.Sub(first) < p.cfg.DedupWindow {
			p.mu.Unlock()
			ev.Deduplicated = true
			continue
		}
		p.seen[ev.ClusterID] = now
		p.gcLocked(now)
		p.mu.Unlock()

		if ev.MaterialityScore < p.cfg.MinMateriality {
			// Retained but not enriched: below the bar for spending a model call.
			out = append(out, ev)
			continue
		}
		if p.cfg.UseLLM && p.provider != nil {
			if enriched, ok := p.enrich(ctx, a, ev); ok {
				ev = enriched
			}
		}
		out = append(out, ev)
	}
	p.mu.Lock()
	p.lastRun = now
	p.mu.Unlock()
	return out
}

func (p *Pipeline) gcLocked(now time.Time) {
	if len(p.seen) < 4096 {
		return
	}
	for k, t := range p.seen {
		if now.Sub(t) > p.cfg.DedupWindow {
			delete(p.seen, k)
		}
	}
}

// enrichSchema is the only shape the model may return. Anything else is
// discarded and the deterministic classification stands.
type enrichSchema struct {
	Sentiment      string  `json:"sentiment"`
	SentimentScore float64 `json:"sentiment_score"`
	Materiality    string  `json:"materiality"`
	Confidence     float64 `json:"confidence"`
	Category       string  `json:"category"`
	Reason         string  `json:"reason"`
}

const enrichSystem = `You classify financial news headlines into a fixed schema.

Return ONLY a JSON object, no prose, with exactly these keys:
{"sentiment":"positive|neutral|negative","sentiment_score":-1.0..1.0,
 "materiality":"high|medium|low","confidence":0.0..1.0,
 "category":"EARNINGS|GUIDANCE|M_AND_A|REGULATORY|LEGAL|PRODUCT|MANAGEMENT|ANALYST_ACTION|MACRO|GENERAL",
 "reason":"one short sentence"}

Rules:
- Judge the effect on the named company only.
- "materiality" is how much this could plausibly move the instrument, not how interesting it is.
- If the headline is ambiguous, return neutral with a low confidence. Do not guess.
- Never predict a price move. Never give advice. Return only the JSON object.`

// enrich asks the model to refine the classification, then reconciles.
//
// Reconciliation is conservative: the model can move the labels, but a large
// disagreement lowers confidence rather than overriding the deterministic
// result. That keeps a hallucinating or injected model from manufacturing a
// high-materiality event out of a routine headline.
func (p *Pipeline) enrich(ctx context.Context, a Article, det domain.NewsEvent) (domain.NewsEvent, bool) {
	req := llm.Request{
		System: enrichSystem,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: fmt.Sprintf(
			"Company ticker: %s\n\n%s",
			a.Ticker, llm.UntrustedBlock(a.Source, a.Headline))}},
		MaxTokens:   200,
		Temperature: 0,
		Purpose:     "news_classification",
	}
	resp, err := p.provider.Complete(ctx, req)
	if err != nil {
		return det, false
	}
	var out enrichSchema
	if err := json.Unmarshal([]byte(extractJSON(resp.Text)), &out); err != nil {
		return det, false
	}
	if !validSentiment(out.Sentiment) || !validMateriality(out.Materiality) {
		return det, false
	}

	ev := det
	ev.Stage = "llm-enriched"
	ev.LLMModel = resp.Model

	disagreement := absf(out.SentimentScore - det.SentimentScore)
	ev.LLMAgreed = disagreement < 0.5 && out.Materiality == string(det.Materiality)

	// Blend rather than replace, and cap the model's influence.
	ev.SentimentScore = round2(domain.Clamp(0.5*det.SentimentScore+0.5*domain.Clamp(out.SentimentScore, -1, 1), -1, 1))
	switch {
	case ev.SentimentScore > 0.15:
		ev.Sentiment = domain.SentimentPositive
	case ev.SentimentScore < -0.15:
		ev.Sentiment = domain.SentimentNegative
	default:
		ev.Sentiment = domain.SentimentNeutral
	}
	// Materiality may only move one step, and never above the category's cap.
	ev.Materiality = reconcileMateriality(det.Materiality, domain.Materiality(out.Materiality))
	ev.MaterialityScore = round2(materialityScore(ev.Materiality))

	if ev.LLMAgreed {
		ev.Confidence = round2(domain.Clamp(det.Confidence*1.15, 0, 0.92))
	} else {
		// Disagreement is information: it means the case is genuinely unclear.
		ev.Confidence = round2(domain.Clamp(det.Confidence*0.7, 0, 0.6))
	}
	return ev, true
}

func reconcileMateriality(det, model domain.Materiality) domain.Materiality {
	rank := map[domain.Materiality]int{domain.MaterialityLow: 0, domain.MaterialityMedium: 1, domain.MaterialityHigh: 2}
	inv := map[int]domain.Materiality{0: domain.MaterialityLow, 1: domain.MaterialityMedium, 2: domain.MaterialityHigh}
	d, m := rank[det], rank[model]
	switch {
	case m > d:
		return inv[minInt(d+1, 2)]
	case m < d:
		return inv[maxInt(d-1, 0)]
	default:
		return det
	}
}

func materialityScore(m domain.Materiality) float64 {
	switch m {
	case domain.MaterialityHigh:
		return 0.8
	case domain.MaterialityMedium:
		return 0.55
	default:
		return 0.25
	}
}

func validSentiment(s string) bool {
	switch domain.Sentiment(s) {
	case domain.SentimentPositive, domain.SentimentNeutral, domain.SentimentNegative:
		return true
	}
	return false
}

func validMateriality(s string) bool {
	switch domain.Materiality(s) {
	case domain.MaterialityHigh, domain.MaterialityMedium, domain.MaterialityLow:
		return true
	}
	return false
}

// extractJSON pulls the first balanced JSON object out of a response, so a model
// that wraps its answer in prose or a code fence is still usable.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return "{}"
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return "{}"
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func round2(v float64) float64 { return float64(int64(v*100+sign(v)*0.5)) / 100 }

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

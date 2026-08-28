package news

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// SyntheticSource generates deterministic headlines for the development
// universe.
//
// These are FIXTURE headlines about real ticker symbols. They are obviously
// synthetic ("QuantOS Sim Wire" is the source), they are never presented as
// reported news, and the platform labels every event with its source. They
// exist so the news pipeline, the materiality gate and the news-driven
// invalidation path can be exercised and demonstrated without a paid feed.
type SyntheticSource struct {
	seed  int64
	rate  float64 // expected headlines per symbol per hour
	clock func() time.Time

	mu       sync.Mutex
	universe []domain.Stock
	rng      *rand.Rand
	counter  int
	// injected are scenario-driven headlines queued by the demo runner.
	injected []Article
}

// NewSyntheticSource builds a generator.
func NewSyntheticSource(seed int64, universe []domain.Stock, ratePerSymbolPerHour float64, now func() time.Time) *SyntheticSource {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &SyntheticSource{
		seed: seed, rate: ratePerSymbolPerHour, clock: now,
		universe: universe, rng: rand.New(rand.NewSource(seed)),
	}
}

// Name implements Source.
func (s *SyntheticSource) Name() string { return "quantos-sim-wire" }

// Inject queues a specific headline, which is how demo scenarios drive a
// news-triggered prediction change.
func (s *SyntheticSource) Inject(a Article) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Timestamp.IsZero() {
		a.Timestamp = s.clock()
	}
	if a.Source == "" {
		a.Source = s.Name()
	}
	if a.ID == "" {
		s.counter++
		a.ID = fmt.Sprintf("inj-%d-%d", s.seed, s.counter)
	}
	s.injected = append(s.injected, a)
}

// templates are (headline format, weight) pairs spanning the categories the
// classifier recognises, so the pipeline sees a realistic mix.
var templates = []struct {
	format string
	weight int
}{
	{"%s beats estimates on quarterly results as revenue growth accelerates", 3},
	{"%s misses estimates; management warns on the outlook for next quarter", 3},
	{"%s raises full-year guidance after a strong quarter", 2},
	{"%s cuts guidance, citing slowing demand", 2},
	{"Analysts upgrade %s to overweight and lift the price target", 3},
	{"Analysts downgrade %s to underweight on valuation concerns", 3},
	{"%s announces a partnership to expand its product line", 2},
	{"%s unveils a new product at its annual event", 2},
	{"Regulator opens an investigation into %s over disclosure practices", 1},
	{"%s settles a class action lawsuit without admitting liability", 1},
	{"%s names a new chief financial officer; the incumbent steps down", 1},
	{"%s announces a buyback and a dividend increase", 2},
	{"Reports suggest %s is in talks to acquire a smaller competitor", 1},
	{"%s shares in focus as sector peers report results", 3},
}

// Fetch implements Source, returning injected headlines plus a Poisson-ish
// sample of generated ones.
func (s *SyntheticSource) Fetch(_ context.Context, since time.Time) ([]Article, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	out := s.injected
	s.injected = nil

	if s.rate <= 0 || len(s.universe) == 0 {
		return out, nil
	}
	elapsed := now.Sub(since).Hours()
	if since.IsZero() || elapsed <= 0 || elapsed > 24 {
		elapsed = 1.0 / 60 // one minute's worth on the first call
	}
	expected := s.rate * elapsed * float64(len(s.universe))
	n := int(expected)
	if s.rng.Float64() < expected-float64(n) {
		n++
	}
	for i := 0; i < n && i < 25; i++ {
		st := s.universe[s.rng.Intn(len(s.universe))]
		tmpl := weightedTemplate(s.rng)
		s.counter++
		out = append(out, Article{
			ID:        fmt.Sprintf("sim-%d-%d", s.seed, s.counter),
			Ticker:    st.Ticker,
			Headline:  fmt.Sprintf(tmpl, st.Company),
			Source:    s.Name(),
			Timestamp: now,
			Sector:    st.Sector,
		})
	}
	return out, nil
}

func weightedTemplate(rng *rand.Rand) string {
	total := 0
	for _, t := range templates {
		total += t.weight
	}
	x := rng.Intn(total)
	for _, t := range templates {
		x -= t.weight
		if x < 0 {
			return t.format
		}
	}
	return templates[0].format
}

// StableID produces a deterministic article id from its content, which lets a
// fixture file be replayed without duplicate suppression misfiring.
func StableID(a Article) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(string(a.Ticker) + a.Headline + a.Timestamp.UTC().Format(time.RFC3339)))
	return fmt.Sprintf("%016x", h.Sum64())
}

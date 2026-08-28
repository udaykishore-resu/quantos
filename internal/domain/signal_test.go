package domain

import (
	"errors"
	"math"
	"math/rand"
	"testing"
	"time"
)

// TestSignalRequiresAllowingRiskAssessment is the single most important test in
// the repository. ADR-009 promises that the risk engine's veto is structural,
// not conventional: if this test can be made to fail, the promise is broken.
func TestSignalRequiresAllowingRiskAssessment(t *testing.T) {
	draft := validDraft()

	for _, tc := range []struct {
		decision RiskDecision
		wantErr  bool
	}{
		{RiskAllowPaperSignal, false},
		{RiskWatchOnly, true},
		{RiskBlock, true},
		{RiskDecision("SOMETHING_ELSE"), true},
		{RiskDecision(""), true},
	} {
		_, err := NewSignal(draft, RiskAssessment{ID: "ra-1", Decision: tc.decision})
		if tc.wantErr && !errors.Is(err, ErrRiskVeto) {
			t.Fatalf("decision %q: expected ErrRiskVeto, got %v", tc.decision, err)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("decision %q: unexpected error %v", tc.decision, err)
		}
	}
}

// TestSignalVetoProperty is the randomised counterpart: across a large number
// of arbitrary drafts and assessments, a non-allowing decision must never
// produce a signal.
func TestSignalVetoProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	decisions := []RiskDecision{RiskAllowPaperSignal, RiskWatchOnly, RiskBlock}

	for i := 0; i < 100_000; i++ {
		d := validDraft()
		d.Strength = rng.Float64() * 200
		d.Confidence = rng.Float64() * 2
		d.SuggestedWeight = rng.Float64() * 3
		if rng.Intn(2) == 0 {
			d.Side = SideShort
		}
		decision := decisions[rng.Intn(len(decisions))]
		sig, err := NewSignal(d, RiskAssessment{ID: "ra", Decision: decision})

		switch decision {
		case RiskAllowPaperSignal:
			if err != nil {
				t.Fatalf("iteration %d: allow produced an error: %v", i, err)
			}
			if sig.Status != SignalActive {
				t.Fatalf("iteration %d: allowed signal is not active", i)
			}
			// Bounds must hold whatever the draft claimed.
			if sig.Strength < 0 || sig.Strength > 100 {
				t.Fatalf("iteration %d: strength %f escaped [0,100]", i, sig.Strength)
			}
			if sig.Confidence < 0 || sig.Confidence > 1 {
				t.Fatalf("iteration %d: confidence %f escaped [0,1]", i, sig.Confidence)
			}
			if sig.SuggestedWeight < 0 || sig.SuggestedWeight > 1 {
				t.Fatalf("iteration %d: weight %f escaped [0,1]", i, sig.SuggestedWeight)
			}
		default:
			if err == nil {
				t.Fatalf("iteration %d: decision %q produced a signal", i, decision)
			}
			if sig.ID != "" {
				t.Fatalf("iteration %d: a vetoed signal was partially constructed", i)
			}
		}
	}
}

// TestSignalRequiresInvalidationConditions covers governance rule G-6: a signal
// nobody is watching for failure is not allowed to exist.
func TestSignalRequiresInvalidationConditions(t *testing.T) {
	d := validDraft()
	d.Invalidations = nil
	if _, err := NewSignal(d, allowing()); !errors.Is(err, ErrNoInvalidation) {
		t.Fatalf("expected ErrNoInvalidation, got %v", err)
	}
}

func TestSignalRejectsFlatSide(t *testing.T) {
	d := validDraft()
	d.Side = SideFlat
	if _, err := NewSignal(d, allowing()); err == nil {
		t.Fatal("a FLAT signal was accepted; direction must be LONG or SHORT")
	}
}

func TestSignalCarriesDisclaimer(t *testing.T) {
	s, err := NewSignal(validDraft(), allowing())
	if err != nil {
		t.Fatal(err)
	}
	if s.Disclaimer != Disclaimer {
		t.Fatal("the research disclaimer is not attached to the signal (governance rule G-9)")
	}
	if s.RiskAssessmentID != "ra-allow" {
		t.Fatal("the signal does not record which assessment allowed it")
	}
}

func validDraft() SignalDraft {
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	return SignalDraft{
		ID:        "sig-1",
		Ticker:    "AAPL",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
		Side:      SideLong,
		Horizon:   Horizon60m,
		Invalidations: []InvalidationCondition{
			{Kind: InvalidTimeExpiry, Deadline: now.Add(time.Hour), Description: "expiry"},
		},
	}
}

func allowing() RiskAssessment {
	return RiskAssessment{ID: "ra-allow", Decision: RiskAllowPaperSignal}
}

// --- Distribution -----------------------------------------------------------

func TestDistributionNormalises(t *testing.T) {
	for _, tc := range [][3]float64{
		{1, 1, 1}, {2, 0, 0}, {0.6, 0.3, 0.1}, {100, 50, 25},
		{0, 0, 0}, {-1, 2, 1}, {math.NaN(), 1, 1}, {math.Inf(1), 1, 1},
	} {
		d := NewDistribution(tc[0], tc[1], tc[2])
		if err := d.Valid(); err != nil {
			t.Fatalf("input %v produced an invalid distribution: %v", tc, err)
		}
	}
}

func TestDistributionDegenerateInputIsMaximumEntropy(t *testing.T) {
	d := NewDistribution(0, 0, 0)
	if math.Abs(d.Up-1.0/3) > 1e-9 {
		t.Fatal("a zero-weight distribution must be maximum entropy: with no information, " +
			"the honest answer is that every outcome is equally likely")
	}
}

func TestConfidenceIsMonotoneInMaxProbability(t *testing.T) {
	low := NewDistribution(0.34, 0.33, 0.33)
	mid := NewDistribution(0.55, 0.25, 0.20)
	high := NewDistribution(0.90, 0.05, 0.05)

	if !(low.Confidence() < mid.Confidence() && mid.Confidence() < high.Confidence()) {
		t.Fatalf("confidence is not monotone: %.4f, %.4f, %.4f",
			low.Confidence(), mid.Confidence(), high.Confidence())
	}
	if low.Confidence() > 0.02 {
		t.Fatalf("a near-uniform distribution reported confidence %.4f", low.Confidence())
	}
	if high.Confidence() < 0.8 {
		t.Fatalf("a peaked distribution reported confidence %.4f", high.Confidence())
	}
}

// TestSpecExampleConfidenceIsUsable guards the definition against regressing to
// normalised entropy, which would make the requirements' own worked example
// unusable against any sensible threshold.
func TestSpecExampleConfidenceIsUsable(t *testing.T) {
	d := NewDistribution(0.63, 0.21, 0.16)
	if c := d.Confidence(); c < 0.35 || c > 0.55 {
		t.Fatalf("the documented example distribution reports confidence %.3f, "+
			"which no reasonable gate could use", c)
	}
}

func TestBrierScoreBounds(t *testing.T) {
	perfect := NewDistribution(1, 0, 0)
	if b := BrierScore(perfect, OutcomeUp); b != 0 {
		t.Fatalf("a perfect forecast scored %f, expected 0", b)
	}
	worst := NewDistribution(1, 0, 0)
	if b := BrierScore(worst, OutcomeDown); math.Abs(b-2) > 1e-9 {
		t.Fatalf("the worst possible forecast scored %f, expected 2", b)
	}
	uniform := NewDistribution(1, 1, 1)
	if b := BrierScore(uniform, OutcomeUp); math.Abs(b-(4.0/9+1.0/9+1.0/9)) > 1e-9 {
		t.Fatalf("uniform forecast Brier score is %f", b)
	}
}

func TestArgmaxTiesResolveToFlat(t *testing.T) {
	d := NewDistribution(1, 1, 1)
	if o, _ := d.Argmax(); o != OutcomeFlat {
		t.Fatalf("a three-way tie resolved to %s; FLAT is the conservative choice", o)
	}
}

func TestDirectionalEdgeDistinguishesShapes(t *testing.T) {
	a := NewDistribution(0.40, 0.35, 0.25)
	b := NewDistribution(0.40, 0.20, 0.40)
	if a.DirectionalEdge() <= b.DirectionalEdge() {
		t.Fatal("directional edge must separate distributions that share an argmax")
	}
}

func TestBlendIsCommutativeAtHalf(t *testing.T) {
	a := NewDistribution(0.6, 0.3, 0.1)
	b := NewDistribution(0.1, 0.3, 0.6)
	ab, ba := a.Blend(b, 0.5), b.Blend(a, 0.5)
	if math.Abs(ab.Up-ba.Up) > 1e-12 || math.Abs(ab.Down-ba.Down) > 1e-12 {
		t.Fatal("a half-and-half blend is not symmetric")
	}
}

// --- Snapshot hashing -------------------------------------------------------

func TestFeatureSnapshotHashIsStableAndSensitive(t *testing.T) {
	mk := func(rsi float64) FeatureSnapshot {
		s := FeatureSnapshot{
			Ticker: "AAPL", AsOf: time.Unix(1700000000, 0).UTC(), Interval: Interval1m,
			Values: map[string]float64{"rsi_14": rsi, "sma_20": 100},
			Warm:   map[string]bool{"rsi_14": true, "sma_20": true},
		}
		s.Finalize()
		return s
	}
	a, b, c := mk(55), mk(55), mk(55.0001)
	if a.Hash != b.Hash {
		t.Fatal("identical snapshots hashed differently; reproducibility depends on this")
	}
	if a.Hash == c.Hash {
		t.Fatal("a changed feature value did not change the hash")
	}
	if len(a.Hash) != 64 {
		t.Fatalf("expected a sha256 hex digest, got %d characters", len(a.Hash))
	}
}

func TestFeatureSnapshotColdValuesAreNotReadable(t *testing.T) {
	s := FeatureSnapshot{
		Values: map[string]float64{"rsi_14": 55},
		Warm:   map[string]bool{"rsi_14": false},
	}
	if _, ok := s.Get("rsi_14"); ok {
		t.Fatal("a cold feature was readable; models must never consume one")
	}
	if v := s.MustGet("rsi_14", 42); v != 42 {
		t.Fatalf("MustGet returned %v for a cold feature instead of the fallback", v)
	}
}

func TestVectorFailsClosedOnMissingFeature(t *testing.T) {
	s := FeatureSnapshot{
		Values: map[string]float64{"a": 1},
		Warm:   map[string]bool{"a": true},
	}
	if _, err := s.Vector([]string{"a", "b"}); err == nil {
		t.Fatal("a missing feature must be an error, not a silently substituted zero")
	}
}

func TestQuoteValidation(t *testing.T) {
	base := Quote{Ticker: "AAPL", Bid: 100, Ask: 100.05, BidSize: 300, AskSize: 200,
		Last: 100.02, Timestamp: time.Now()}
	if err := base.Valid(); err != nil {
		t.Fatalf("a good quote was rejected: %v", err)
	}
	crossed := base
	crossed.Bid, crossed.Ask = 100.10, 100.00
	if err := crossed.Valid(); err == nil {
		t.Fatal("a crossed book was accepted")
	}
	negative := base
	negative.Last = -1
	if err := negative.Valid(); err == nil {
		t.Fatal("a negative price was accepted")
	}
	// A two-sided book with no size is not a market you could trade against,
	// and the spread it implies is fiction — which the risk engine would then
	// read as a tight, liquid quote.
	sizeless := base
	sizeless.BidSize, sizeless.AskSize = 0, 0
	if err := sizeless.Valid(); err == nil {
		t.Fatal("a quote with no size on either side was accepted")
	}
	oneSided := base
	oneSided.AskSize = 0
	if err := oneSided.Valid(); err == nil {
		t.Fatal("a quote with size on only one side was accepted")
	}
}

func TestCandleValidation(t *testing.T) {
	start := time.Now()
	good := Candle{Ticker: "AAPL", Open: 100, High: 101, Low: 99, Close: 100.5,
		Volume: 1000, Start: start, End: start.Add(time.Minute)}
	if err := good.Valid(); err != nil {
		t.Fatalf("a good candle was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Candle){
		"high below low":   func(c *Candle) { c.High, c.Low = 99, 101 },
		"high below close": func(c *Candle) { c.High = 100 },
		"low above open":   func(c *Candle) { c.Low = 100.4 },
		"negative volume":  func(c *Candle) { c.Volume = -1 },
		"end before start": func(c *Candle) { c.End = c.Start.Add(-time.Minute) },
	} {
		bad := good
		mutate(&bad)
		if err := bad.Valid(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestNormalizeTicker(t *testing.T) {
	for _, in := range []string{"brk-b", "BRK/B", " brk.b ", "BRK.B"} {
		if got := NormalizeTicker(in); got != "BRK.B" {
			t.Fatalf("NormalizeTicker(%q) = %q", in, got)
		}
	}
}

func TestStockPointInTimeMembership(t *testing.T) {
	listed := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	delisted := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s := Stock{Ticker: "OLD", ListedAt: listed, DelistedAt: &delisted}

	if s.Active(listed.AddDate(-1, 0, 0)) {
		t.Fatal("an instrument was active before it listed")
	}
	if !s.Active(time.Date(2017, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("an instrument was inactive during its listed life")
	}
	if s.Active(delisted) {
		t.Fatal("an instrument was still active on its delisting date; this is how survivorship bias enters")
	}
}

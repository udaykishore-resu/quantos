package features

import (
	"math"
	"testing"
)

const eps = 1e-9

func approx(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.10f, want %.10f (tolerance %g)", what, got, want, tol)
	}
}

func TestWindowStatistics(t *testing.T) {
	w := NewWindow(5)
	for _, v := range []float64{2, 4, 4, 4, 5} {
		w.Push(v)
	}
	approx(t, w.Mean(), 3.8, eps, "mean")
	// Population variance of {2,4,4,4,5} = 0.96
	approx(t, w.Variance(), 0.96, 1e-9, "variance")
	approx(t, w.StdDev(), math.Sqrt(0.96), 1e-9, "stddev")
	if !w.Full() {
		t.Fatal("a window of five given five values is not full")
	}
	if w.Last() != 5 {
		t.Fatalf("Last() = %v", w.Last())
	}
	if w.At(0) != 5 || w.At(4) != 2 {
		t.Fatalf("At() is not newest-first: At(0)=%v At(4)=%v", w.At(0), w.At(4))
	}
	// Eviction: pushing a sixth value drops the oldest.
	w.Push(10)
	approx(t, w.Mean(), (4+4+4+5+10)/5.0, eps, "mean after eviction")
	if w.Min() != 4 || w.Max() != 10 {
		t.Fatalf("Min=%v Max=%v after eviction", w.Min(), w.Max())
	}
}

func TestWindowIgnoresNonFiniteValues(t *testing.T) {
	w := NewWindow(3)
	w.Push(1)
	w.Push(math.NaN())
	w.Push(math.Inf(1))
	w.Push(3)
	if w.Len() != 2 {
		t.Fatalf("non-finite values were admitted: length %d", w.Len())
	}
	approx(t, w.Mean(), 2, eps, "mean")
}

func TestWindowPercentile(t *testing.T) {
	w := NewWindow(5)
	for _, v := range []float64{10, 20, 30, 40, 50} {
		w.Push(v)
	}
	approx(t, w.Percentile(0), 10, eps, "p0")
	approx(t, w.Percentile(1), 50, eps, "p100")
	approx(t, w.Percentile(0.5), 30, eps, "p50")
	approx(t, w.Percentile(0.25), 20, eps, "p25")
	approx(t, w.RankOf(30), 0.6, eps, "rank of 30")
}

func TestSMAMatchesArithmeticMean(t *testing.T) {
	s := NewSMA(3)
	s.Update(1)
	s.Update(2)
	if s.Warm() {
		t.Fatal("an SMA is warm before its window is full")
	}
	v := s.Update(3)
	approx(t, v, 2, eps, "sma(1,2,3)")
	if !s.Warm() {
		t.Fatal("an SMA of three given three values is not warm")
	}
	approx(t, s.Update(4), 3, eps, "sma(2,3,4)")
}

// TestEMASeeding checks the deliberate choice to seed with an SMA rather than
// with the first value, which would bias every early reading toward it.
func TestEMASeeding(t *testing.T) {
	e := NewEMA(3)
	e.Update(10)
	e.Update(20)
	seeded := e.Update(30)
	approx(t, seeded, 20, eps, "seeded EMA equals the SMA of the seeding window")
	if !e.Warm() {
		t.Fatal("the EMA is not warm after its seeding period")
	}
	// alpha = 2/(3+1) = 0.5; next value 40 -> 20 + 0.5*(40-20) = 30
	approx(t, e.Update(40), 30, eps, "EMA after seeding")
}

// TestRSIKnownSeries uses Wilder's original worked example. Getting RSI subtly
// wrong is easy and invisible, so it is worth pinning to a published series.
func TestRSIKnownSeries(t *testing.T) {
	closes := []float64{
		44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42,
		45.84, 46.08, 45.89, 46.03, 45.61, 46.28, 46.28,
	}
	r := NewRSI(14)
	var last float64
	for _, c := range closes {
		last = r.Update(c)
	}
	if !r.Warm() {
		t.Fatal("RSI is not warm after fifteen closes")
	}
	// Wilder's example gives ~70.53 at this point.
	approx(t, last, 70.53, 0.6, "RSI(14)")
}

func TestRSIBounds(t *testing.T) {
	up := NewRSI(14)
	for i := 0; i < 40; i++ {
		up.Update(100 + float64(i))
	}
	approx(t, up.Value(), 100, eps, "RSI of a monotone rise")

	down := NewRSI(14)
	for i := 0; i < 40; i++ {
		down.Update(100 - float64(i))
	}
	if down.Value() > 1 {
		t.Fatalf("RSI of a monotone fall is %.4f, expected ~0", down.Value())
	}
}

func TestATRTrueRangeIncludesGaps(t *testing.T) {
	a := NewATR(3)
	// A gap up: the true range must span from the previous close, not just the
	// bar's own high-low, or every gap is invisible to volatility measurement.
	a.Update(10, 9, 9.5)   // TR = 1
	a.Update(20, 19, 19.5) // TR = max(1, |20-9.5|, |19-9.5|) = 10.5
	a.Update(21, 20, 20.5) // TR = max(1, |21-19.5|, |20-19.5|) = 1.5
	if !a.Warm() {
		t.Fatal("ATR is not warm after three bars")
	}
	approx(t, a.Value(), (1+10.5+1.5)/3, 1e-9, "ATR seeding average")
}

func TestBollingerBands(t *testing.T) {
	b := NewBollinger(5, 2)
	for _, v := range []float64{10, 12, 14, 16, 18} {
		b.Update(v)
	}
	up, mid, lo := b.Values()
	approx(t, mid, 14, eps, "middle band")
	sd := math.Sqrt(8) // population stddev of {10,12,14,16,18}
	approx(t, up, 14+2*sd, 1e-9, "upper band")
	approx(t, lo, 14-2*sd, 1e-9, "lower band")
	approx(t, b.PercentB(mid), 0.5, 1e-9, "%B at the middle band")
	approx(t, b.PercentB(up), 1, 1e-9, "%B at the upper band")
}

func TestBollingerDegenerateSeries(t *testing.T) {
	b := NewBollinger(5, 2)
	for i := 0; i < 5; i++ {
		b.Update(100)
	}
	// Zero variance: %B must not divide by zero.
	if v := b.PercentB(100); math.IsNaN(v) || math.IsInf(v, 0) {
		t.Fatalf("%%B on a flat series produced %v", v)
	}
}

func TestADXRisesInATrendAndFallsInChop(t *testing.T) {
	trend := NewADX(14)
	price := 100.0
	for i := 0; i < 60; i++ {
		price += 1
		trend.Update(price+0.5, price-0.5, price)
	}
	trendADX, plusDI, minusDI := trend.Values()
	if !trend.Warm() {
		t.Fatal("ADX is not warm after sixty bars")
	}
	if trendADX < 40 {
		t.Fatalf("ADX in a clean uptrend is only %.1f", trendADX)
	}
	if plusDI <= minusDI {
		t.Fatalf("+DI (%.1f) should exceed -DI (%.1f) in an uptrend", plusDI, minusDI)
	}

	chop := NewADX(14)
	base := 100.0
	for i := 0; i < 60; i++ {
		if i%2 == 0 {
			base += 0.5
		} else {
			base -= 0.5
		}
		chop.Update(base+0.2, base-0.2, base)
	}
	chopADX, _, _ := chop.Values()
	if chopADX >= trendADX {
		t.Fatalf("ADX did not distinguish a trend (%.1f) from chop (%.1f)", trendADX, chopADX)
	}
}

func TestVWAPWeightsByVolumeAndResetsOnSession(t *testing.T) {
	v := NewVWAP()
	v.Update(10, 100, "day-1")
	v.Update(20, 300, "day-1")
	// (10*100 + 20*300) / 400 = 17.5
	approx(t, v.Value(), 17.5, eps, "VWAP")
	if !v.Warm() {
		t.Fatal("VWAP is not warm after accumulating volume")
	}
	v.Update(50, 100, "day-2")
	approx(t, v.Value(), 50, eps, "VWAP after a session reset")
}

func TestVWAPIgnoresZeroVolume(t *testing.T) {
	v := NewVWAP()
	v.Update(10, 0, "d")
	if v.Warm() {
		t.Fatal("VWAP became warm on zero volume")
	}
	if v.Value() != 0 {
		t.Fatalf("VWAP with no volume is %v, expected 0", v.Value())
	}
}

func TestMACDCrossoverBehaviour(t *testing.T) {
	m := NewMACD(12, 26, 9)
	price := 100.0
	for i := 0; i < 60; i++ {
		price += 0.5
		m.Update(price)
	}
	macd, _, hist := m.Values()
	if macd <= 0 || hist <= 0 {
		t.Fatalf("MACD in a steady uptrend: macd=%.4f hist=%.4f", macd, hist)
	}
	for i := 0; i < 80; i++ {
		price -= 0.8
		m.Update(price)
	}
	macd, _, _ = m.Values()
	if macd >= 0 {
		t.Fatalf("MACD did not turn negative in a downtrend: %.4f", macd)
	}
}

func TestLinearSlope(t *testing.T) {
	approx(t, LinearSlope([]float64{1, 2, 3, 4, 5}), 1, eps, "unit slope")
	approx(t, LinearSlope([]float64{5, 4, 3, 2, 1}), -1, eps, "negative slope")
	approx(t, LinearSlope([]float64{3, 3, 3}), 0, eps, "flat slope")
	approx(t, LinearSlope([]float64{7}), 0, eps, "single point")
}

func TestCorrelation(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	approx(t, Correlation(a, a), 1, 1e-9, "self-correlation")
	approx(t, Correlation(a, []float64{5, 4, 3, 2, 1}), -1, 1e-9, "anti-correlation")
	approx(t, Correlation(a, []float64{1, 1, 1, 1, 1}), 0, 1e-9, "correlation with a constant")
	if Correlation(a, []float64{1, 2}) != 0 {
		t.Fatal("mismatched lengths should yield zero rather than panicking")
	}
}

func TestZScore(t *testing.T) {
	approx(t, ZScore(110, 100, 5), 2, eps, "z-score")
	if ZScore(110, 100, 0) != 0 {
		t.Fatal("a zero standard deviation must not produce an infinite z-score")
	}
}

// BenchmarkIndicatorUpdate documents the per-bar cost of the whole indicator
// set, which is what the 100 ms market-event SLO is spent on.
func BenchmarkIndicatorUpdate(b *testing.B) {
	sma := NewSMA(20)
	ema := NewEMA(21)
	rsi := NewRSI(14)
	macd := NewMACD(12, 26, 9)
	atr := NewATR(14)
	bb := NewBollinger(20, 2)
	adx := NewADX(14)
	price := 100.0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		price += float64(i%7) * 0.01
		sma.Update(price)
		ema.Update(price)
		rsi.Update(price)
		macd.Update(price)
		atr.Update(price+0.5, price-0.5, price)
		bb.Update(price)
		adx.Update(price+0.5, price-0.5, price)
	}
}

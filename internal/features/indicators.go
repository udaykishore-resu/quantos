// Package features computes the deterministic technical feature set that every
// downstream engine consumes.
//
// All indicators are incremental: they hold O(window) state and update in O(1)
// per bar. That matters because the platform recomputes features for hundreds
// of symbols on every bar and the SLO for market-event processing is a P99 of
// 100 ms end to end.
//
// Every indicator tracks warmth explicitly. A cold indicator returns its
// current value *and* Warm() == false, and the snapshot marks it cold so that
// models and rules refuse to use it rather than silently consuming a value
// derived from three data points.
package features

import (
	"math"
	"sort"
)

// Window is a fixed-size circular buffer of float64 with running sum and
// sum-of-squares, so mean and standard deviation are O(1).
type Window struct {
	buf    []float64
	n      int
	idx    int
	filled bool
	sum    float64
	sumSq  float64
}

// NewWindow returns a window of the given size.
func NewWindow(size int) *Window {
	if size < 1 {
		size = 1
	}
	return &Window{buf: make([]float64, size)}
}

// Push adds a value, evicting the oldest when full.
func (w *Window) Push(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	if w.filled {
		old := w.buf[w.idx]
		w.sum -= old
		w.sumSq -= old * old
	} else {
		w.n++
	}
	w.buf[w.idx] = v
	w.sum += v
	w.sumSq += v * v
	w.idx = (w.idx + 1) % len(w.buf)
	if w.idx == 0 {
		w.filled = true
	}
}

// Len returns the number of samples held.
func (w *Window) Len() int {
	if w.filled {
		return len(w.buf)
	}
	return w.n
}

// Full reports whether the window has seen at least `size` samples.
func (w *Window) Full() bool { return w.filled }

// Mean returns the arithmetic mean.
func (w *Window) Mean() float64 {
	n := w.Len()
	if n == 0 {
		return 0
	}
	return w.sum / float64(n)
}

// Variance returns the population variance.
func (w *Window) Variance() float64 {
	n := float64(w.Len())
	if n < 2 {
		return 0
	}
	m := w.sum / n
	v := w.sumSq/n - m*m
	if v < 0 {
		return 0 // floating-point noise near zero variance
	}
	return v
}

// StdDev returns the population standard deviation.
func (w *Window) StdDev() float64 { return math.Sqrt(w.Variance()) }

// Sum returns the running sum.
func (w *Window) Sum() float64 { return w.sum }

// Last returns the most recently pushed value.
func (w *Window) Last() float64 {
	if w.Len() == 0 {
		return 0
	}
	i := (w.idx - 1 + len(w.buf)) % len(w.buf)
	return w.buf[i]
}

// At returns the i-th most recent value (0 = latest).
func (w *Window) At(i int) float64 {
	if i < 0 || i >= w.Len() {
		return 0
	}
	idx := (w.idx - 1 - i + 2*len(w.buf)) % len(w.buf)
	return w.buf[idx]
}

// Values returns the contents oldest-first.
func (w *Window) Values() []float64 {
	n := w.Len()
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[n-1-i] = w.At(i)
	}
	return out
}

// Min returns the minimum value held.
func (w *Window) Min() float64 {
	n := w.Len()
	if n == 0 {
		return 0
	}
	m := w.At(0)
	for i := 1; i < n; i++ {
		if v := w.At(i); v < m {
			m = v
		}
	}
	return m
}

// Max returns the maximum value held.
func (w *Window) Max() float64 {
	n := w.Len()
	if n == 0 {
		return 0
	}
	m := w.At(0)
	for i := 1; i < n; i++ {
		if v := w.At(i); v > m {
			m = v
		}
	}
	return m
}

// Percentile returns the linear-interpolated percentile p in [0,1].
func (w *Window) Percentile(p float64) float64 {
	n := w.Len()
	if n == 0 {
		return 0
	}
	vals := w.Values()
	sort.Float64s(vals)
	if p <= 0 {
		return vals[0]
	}
	if p >= 1 {
		return vals[n-1]
	}
	pos := p * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return vals[lo]
	}
	frac := pos - float64(lo)
	return vals[lo]*(1-frac) + vals[hi]*frac
}

// RankOf returns the fraction of held values less than or equal to v.
func (w *Window) RankOf(v float64) float64 {
	n := w.Len()
	if n == 0 {
		return 0
	}
	count := 0
	for i := 0; i < n; i++ {
		if w.At(i) <= v {
			count++
		}
	}
	return float64(count) / float64(n)
}

// SMA is a simple moving average.
type SMA struct{ w *Window }

// NewSMA returns a simple moving average over n periods.
func NewSMA(n int) *SMA { return &SMA{w: NewWindow(n)} }

// Update pushes a value and returns the current average.
func (s *SMA) Update(v float64) float64 { s.w.Push(v); return s.w.Mean() }

// Value returns the current average.
func (s *SMA) Value() float64 { return s.w.Mean() }

// Warm reports whether the window is full.
func (s *SMA) Warm() bool { return s.w.Full() }

// EMA is an exponential moving average seeded with an SMA over the first n
// samples, which avoids the large start-up bias of seeding with the first value.
type EMA struct {
	alpha float64
	value float64
	seed  *Window
	warm  bool
	n     int
}

// NewEMA returns an EMA over n periods.
func NewEMA(n int) *EMA {
	if n < 1 {
		n = 1
	}
	return &EMA{alpha: 2 / (float64(n) + 1), seed: NewWindow(n), n: n}
}

// Update pushes a value and returns the current EMA.
func (e *EMA) Update(v float64) float64 {
	if !e.warm {
		e.seed.Push(v)
		e.value = e.seed.Mean()
		if e.seed.Full() {
			e.warm = true
		}
		return e.value
	}
	e.value += e.alpha * (v - e.value)
	return e.value
}

// Value returns the current EMA.
func (e *EMA) Value() float64 { return e.value }

// Warm reports whether the seeding period has elapsed.
func (e *EMA) Warm() bool { return e.warm }

// RSI is Wilder's relative strength index.
type RSI struct {
	n        int
	avgGain  float64
	avgLoss  float64
	prev     float64
	hasPrev  bool
	count    int
	seedGain float64
	seedLoss float64
	value    float64
}

// NewRSI returns an RSI over n periods.
func NewRSI(n int) *RSI {
	if n < 2 {
		n = 2
	}
	return &RSI{n: n, value: 50}
}

// Update pushes a close price and returns the current RSI in [0,100].
func (r *RSI) Update(close float64) float64 {
	if !r.hasPrev {
		r.prev, r.hasPrev = close, true
		return r.value
	}
	change := close - r.prev
	r.prev = close
	gain, loss := math.Max(change, 0), math.Max(-change, 0)
	r.count++
	switch {
	case r.count <= r.n:
		r.seedGain += gain
		r.seedLoss += loss
		if r.count == r.n {
			r.avgGain = r.seedGain / float64(r.n)
			r.avgLoss = r.seedLoss / float64(r.n)
		}
	default:
		// Wilder smoothing.
		r.avgGain = (r.avgGain*float64(r.n-1) + gain) / float64(r.n)
		r.avgLoss = (r.avgLoss*float64(r.n-1) + loss) / float64(r.n)
	}
	if r.count < r.n {
		return r.value
	}
	if r.avgLoss == 0 {
		r.value = 100
		return r.value
	}
	rs := r.avgGain / r.avgLoss
	r.value = 100 - 100/(1+rs)
	return r.value
}

// Value returns the current RSI.
func (r *RSI) Value() float64 { return r.value }

// Warm reports whether enough changes have been observed.
func (r *RSI) Warm() bool { return r.count >= r.n }

// MACD is the moving-average convergence/divergence indicator.
type MACD struct {
	fast, slow *EMA
	signal     *EMA
	macd       float64
	sig        float64
	hist       float64
}

// NewMACD returns a MACD with the given fast, slow and signal periods.
func NewMACD(fast, slow, signal int) *MACD {
	return &MACD{fast: NewEMA(fast), slow: NewEMA(slow), signal: NewEMA(signal)}
}

// Update pushes a close price and returns (macd, signal, histogram).
func (m *MACD) Update(close float64) (float64, float64, float64) {
	f := m.fast.Update(close)
	s := m.slow.Update(close)
	m.macd = f - s
	m.sig = m.signal.Update(m.macd)
	m.hist = m.macd - m.sig
	return m.macd, m.sig, m.hist
}

// Values returns the current (macd, signal, histogram).
func (m *MACD) Values() (float64, float64, float64) { return m.macd, m.sig, m.hist }

// Warm reports whether both EMAs and the signal line are seeded.
func (m *MACD) Warm() bool { return m.slow.Warm() && m.signal.Warm() }

// ATR is Wilder's average true range.
type ATR struct {
	n       int
	value   float64
	prev    float64
	hasPrev bool
	count   int
	seed    float64
}

// NewATR returns an ATR over n periods.
func NewATR(n int) *ATR {
	if n < 1 {
		n = 1
	}
	return &ATR{n: n}
}

// Update pushes a bar and returns the current ATR.
func (a *ATR) Update(high, low, close float64) float64 {
	tr := high - low
	if a.hasPrev {
		tr = math.Max(tr, math.Max(math.Abs(high-a.prev), math.Abs(low-a.prev)))
	}
	a.prev, a.hasPrev = close, true
	a.count++
	if a.count <= a.n {
		a.seed += tr
		if a.count == a.n {
			a.value = a.seed / float64(a.n)
		}
		return a.value
	}
	a.value = (a.value*float64(a.n-1) + tr) / float64(a.n)
	return a.value
}

// Value returns the current ATR.
func (a *ATR) Value() float64 { return a.value }

// Warm reports whether the seeding period has elapsed.
func (a *ATR) Warm() bool { return a.count >= a.n }

// Bollinger holds a moving average and its standard-deviation bands.
type Bollinger struct {
	w    *Window
	mult float64
}

// NewBollinger returns Bollinger bands over n periods at mult standard deviations.
func NewBollinger(n int, mult float64) *Bollinger {
	if mult <= 0 {
		mult = 2
	}
	return &Bollinger{w: NewWindow(n), mult: mult}
}

// Update pushes a close price and returns (upper, middle, lower).
func (b *Bollinger) Update(close float64) (float64, float64, float64) {
	b.w.Push(close)
	return b.Values()
}

// Values returns the current (upper, middle, lower).
func (b *Bollinger) Values() (float64, float64, float64) {
	m, sd := b.w.Mean(), b.w.StdDev()
	return m + b.mult*sd, m, m - b.mult*sd
}

// PercentB locates a price within the bands: 0 at the lower band, 1 at the upper.
func (b *Bollinger) PercentB(price float64) float64 {
	up, _, lo := b.Values()
	if up-lo <= 0 {
		return 0.5
	}
	return (price - lo) / (up - lo)
}

// Width is the band width as a fraction of the middle band, the standard
// squeeze/expansion measure.
func (b *Bollinger) Width() float64 {
	up, mid, lo := b.Values()
	if mid == 0 {
		return 0
	}
	return (up - lo) / mid
}

// Warm reports whether the window is full.
func (b *Bollinger) Warm() bool { return b.w.Full() }

// ADX is Wilder's average directional index with its +DI/-DI components.
type ADX struct {
	n                     int
	prevHigh, prevLow     float64
	prevClose             float64
	hasPrev               bool
	smTR, smPlus, smMinus float64
	count                 int
	adx                   float64
	plusDI, minusDI       float64
	dxSeed                float64
	dxCount               int
}

// NewADX returns an ADX over n periods.
func NewADX(n int) *ADX {
	if n < 2 {
		n = 2
	}
	return &ADX{n: n}
}

// Update pushes a bar and returns (adx, +DI, -DI).
func (a *ADX) Update(high, low, close float64) (float64, float64, float64) {
	if !a.hasPrev {
		a.prevHigh, a.prevLow, a.prevClose, a.hasPrev = high, low, close, true
		return a.adx, a.plusDI, a.minusDI
	}
	up := high - a.prevHigh
	down := a.prevLow - low
	plusDM, minusDM := 0.0, 0.0
	if up > down && up > 0 {
		plusDM = up
	}
	if down > up && down > 0 {
		minusDM = down
	}
	tr := math.Max(high-low, math.Max(math.Abs(high-a.prevClose), math.Abs(low-a.prevClose)))
	a.prevHigh, a.prevLow, a.prevClose = high, low, close
	a.count++

	if a.count <= a.n {
		a.smTR += tr
		a.smPlus += plusDM
		a.smMinus += minusDM
		if a.count < a.n {
			return a.adx, a.plusDI, a.minusDI
		}
	} else {
		a.smTR = a.smTR - a.smTR/float64(a.n) + tr
		a.smPlus = a.smPlus - a.smPlus/float64(a.n) + plusDM
		a.smMinus = a.smMinus - a.smMinus/float64(a.n) + minusDM
	}
	if a.smTR == 0 {
		return a.adx, a.plusDI, a.minusDI
	}
	a.plusDI = 100 * a.smPlus / a.smTR
	a.minusDI = 100 * a.smMinus / a.smTR
	den := a.plusDI + a.minusDI
	dx := 0.0
	if den > 0 {
		dx = 100 * math.Abs(a.plusDI-a.minusDI) / den
	}
	a.dxCount++
	if a.dxCount <= a.n {
		a.dxSeed += dx
		if a.dxCount == a.n {
			a.adx = a.dxSeed / float64(a.n)
		}
	} else {
		a.adx = (a.adx*float64(a.n-1) + dx) / float64(a.n)
	}
	return a.adx, a.plusDI, a.minusDI
}

// Values returns the current (adx, +DI, -DI).
func (a *ADX) Values() (float64, float64, float64) { return a.adx, a.plusDI, a.minusDI }

// Warm reports whether the ADX smoothing period has elapsed.
func (a *ADX) Warm() bool { return a.dxCount >= a.n }

// VWAP is a volume-weighted average price with optional session reset.
type VWAP struct {
	notional   float64
	volume     float64
	sessionKey string
}

// NewVWAP returns a VWAP accumulator.
func NewVWAP() *VWAP { return &VWAP{} }

// Update accumulates a bar. sessionKey resets the accumulator when it changes,
// which is how a daily VWAP is expressed without special-casing the calendar.
func (v *VWAP) Update(typical, volume float64, sessionKey string) float64 {
	if sessionKey != v.sessionKey {
		v.notional, v.volume, v.sessionKey = 0, 0, sessionKey
	}
	if volume > 0 {
		v.notional += typical * volume
		v.volume += volume
	}
	return v.Value()
}

// Value returns the current VWAP, or 0 before any volume is seen.
func (v *VWAP) Value() float64 {
	if v.volume <= 0 {
		return 0
	}
	return v.notional / v.volume
}

// Warm reports whether any volume has been accumulated.
func (v *VWAP) Warm() bool { return v.volume > 0 }

// LinearSlope returns the least-squares slope of y against its index, expressed
// per period. It is used for a robust trend measure that is less jumpy than a
// two-point return.
func LinearSlope(y []float64) float64 {
	n := len(y)
	if n < 2 {
		return 0
	}
	var sx, sy, sxy, sxx float64
	for i, v := range y {
		x := float64(i)
		sx += x
		sy += v
		sxy += x * v
		sxx += x * x
	}
	fn := float64(n)
	den := fn*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (fn*sxy - sx*sy) / den
}

// ZScore returns (v - mean) / stddev, or 0 when the sample is degenerate.
func ZScore(v, mean, sd float64) float64 {
	if sd <= 0 {
		return 0
	}
	return (v - mean) / sd
}

// Correlation returns Pearson's r between two equal-length series.
func Correlation(a, b []float64) float64 {
	n := len(a)
	if n != len(b) || n < 2 {
		return 0
	}
	var sa, sb float64
	for i := 0; i < n; i++ {
		sa += a[i]
		sb += b[i]
	}
	ma, mb := sa/float64(n), sb/float64(n)
	var num, da, db float64
	for i := 0; i < n; i++ {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	den := math.Sqrt(da * db)
	if den == 0 {
		return 0
	}
	return num / den
}

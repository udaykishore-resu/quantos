package features

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Config parameterises the feature engine.
type Config struct {
	Interval domain.Interval
	// Warmup is the number of bars before a symbol is considered usable.
	Warmup int
	// MaxHistory bounds retained bars per symbol.
	MaxHistory int
	// Benchmark drives relative-strength calculation.
	Benchmark domain.Ticker
	// VWAPResetDaily resets VWAP at each UTC session boundary.
	VWAPResetDaily bool
	// StalenessSoft/Hard mark a snapshot stale, which suspends new signals.
	StalenessSoft time.Duration
	StalenessHard time.Duration
	Clock         obs.Clock
	Metrics       *obs.Metrics
}

// DefaultConfig returns sensible feature-engine parameters.
func DefaultConfig() Config {
	return Config{
		Interval:       domain.Interval1m,
		Warmup:         50,
		MaxHistory:     500,
		Benchmark:      "SPY",
		VWAPResetDaily: true,
		StalenessSoft:  30 * time.Second,
		StalenessHard:  2 * time.Minute,
		Clock:          obs.SystemClock{},
	}
}

// Engine computes feature snapshots incrementally.
//
// It is safe for concurrent use across symbols; per-symbol state is guarded so
// that a partitioned consumer (one goroutine per Kafka partition) can update
// disjoint symbols in parallel without contention on a single lock.
type Engine struct {
	cfg Config

	mu      sync.RWMutex
	symbols map[domain.Ticker]*symbol

	benchMu   sync.RWMutex
	benchRet  *Window
	benchLast float64
}

type symbol struct {
	mu sync.Mutex

	ticker domain.Ticker
	bars   int

	sma20, sma50, sma200 *SMA
	ema9, ema21          *EMA
	rsi14                *RSI
	macd                 *MACD
	atr14                *ATR
	bb                   *Bollinger
	adx14                *ADX
	vwap                 *VWAP

	closes   *Window
	highs    *Window
	lows     *Window
	volumes  *Window
	logRets  *Window
	ranges   *Window
	volShort *Window

	lastClose  float64
	prevClose  float64
	lastVWAP   float64
	prevVWAP   float64
	sessionDay string

	lastBarAt  time.Time
	lastTickAt time.Time
	lastQuote  domain.Quote

	// pivots retained for support/resistance
	pivotHighs []float64
	pivotLows  []float64
}

// NewEngine builds a feature engine.
func NewEngine(cfg Config) *Engine {
	if cfg.Interval == "" {
		cfg.Interval = domain.Interval1m
	}
	if cfg.Warmup <= 0 {
		cfg.Warmup = 50
	}
	if cfg.MaxHistory < cfg.Warmup {
		cfg.MaxHistory = cfg.Warmup * 4
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	return &Engine{
		cfg:      cfg,
		symbols:  map[domain.Ticker]*symbol{},
		benchRet: NewWindow(60),
	}
}

func newSymbol(t domain.Ticker, cfg Config) *symbol {
	return &symbol{
		ticker:   t,
		sma20:    NewSMA(20),
		sma50:    NewSMA(50),
		sma200:   NewSMA(200),
		ema9:     NewEMA(9),
		ema21:    NewEMA(21),
		rsi14:    NewRSI(14),
		macd:     NewMACD(12, 26, 9),
		atr14:    NewATR(14),
		bb:       NewBollinger(20, 2),
		adx14:    NewADX(14),
		vwap:     NewVWAP(),
		closes:   NewWindow(cfg.MaxHistory),
		highs:    NewWindow(cfg.MaxHistory),
		lows:     NewWindow(cfg.MaxHistory),
		volumes:  NewWindow(60),
		logRets:  NewWindow(60),
		ranges:   NewWindow(20),
		volShort: NewWindow(20),
	}
}

func (e *Engine) symbol(t domain.Ticker) *symbol {
	e.mu.RLock()
	s, ok := e.symbols[t]
	e.mu.RUnlock()
	if ok {
		return s
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok = e.symbols[t]; ok {
		return s
	}
	s = newSymbol(t, e.cfg)
	e.symbols[t] = s
	return s
}

// OnQuote records the latest top-of-book. Quotes refine the price context on a
// snapshot but never create one: features are bar-driven so that a decision is
// always tied to a completed, immutable observation.
func (e *Engine) OnQuote(q domain.Quote) {
	s := e.symbol(q.Ticker)
	s.mu.Lock()
	s.lastQuote = q
	s.lastTickAt = q.Timestamp
	s.mu.Unlock()
}

// OnCandle folds a completed bar into the symbol's state and returns a finalised
// snapshot. The returned snapshot is immutable and hash-addressed.
func (e *Engine) OnCandle(c domain.Candle) domain.FeatureSnapshot {
	start := time.Now()
	if e.cfg.Metrics != nil {
		defer func() { e.cfg.Metrics.FeatureLatency.Observe(time.Since(start).Seconds()) }()
	}
	if c.Ticker == e.cfg.Benchmark {
		e.updateBenchmark(c)
	}
	s := e.symbol(c.Ticker)
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bars++
	s.prevClose = s.lastClose
	s.lastClose = c.Close
	s.lastBarAt = c.End
	if s.lastTickAt.IsZero() || c.End.After(s.lastTickAt) {
		s.lastTickAt = c.End
	}

	s.sma20.Update(c.Close)
	s.sma50.Update(c.Close)
	s.sma200.Update(c.Close)
	s.ema9.Update(c.Close)
	s.ema21.Update(c.Close)
	s.rsi14.Update(c.Close)
	s.macd.Update(c.Close)
	s.atr14.Update(c.High, c.Low, c.Close)
	s.bb.Update(c.Close)
	s.adx14.Update(c.High, c.Low, c.Close)

	sessionKey := ""
	if e.cfg.VWAPResetDaily {
		sessionKey = c.Start.UTC().Format("2006-01-02")
	}
	s.prevVWAP = s.lastVWAP
	s.lastVWAP = s.vwap.Update(c.TypicalPrice(), c.Volume, sessionKey)
	s.sessionDay = sessionKey

	s.closes.Push(c.Close)
	s.highs.Push(c.High)
	s.lows.Push(c.Low)
	s.volumes.Push(c.Volume)
	s.volShort.Push(c.Volume)
	s.ranges.Push(c.Range())
	if s.prevClose > 0 {
		s.logRets.Push(math.Log(c.Close / s.prevClose))
	}
	s.updatePivots()

	return e.buildSnapshot(s, c)
}

func (e *Engine) updateBenchmark(c domain.Candle) {
	e.benchMu.Lock()
	defer e.benchMu.Unlock()
	if e.benchLast > 0 {
		e.benchRet.Push(math.Log(c.Close / e.benchLast))
	}
	e.benchLast = c.Close
}

// benchmarkReturn returns the cumulative benchmark log return over n periods.
func (e *Engine) benchmarkReturn(n int) (float64, bool) {
	e.benchMu.RLock()
	defer e.benchMu.RUnlock()
	if e.benchRet.Len() < n {
		return 0, false
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += e.benchRet.At(i)
	}
	return sum, true
}

// updatePivots maintains simple 5-bar fractal pivots, which give support and
// resistance levels that survive noise better than rolling min/max alone.
func (s *symbol) updatePivots() {
	const k = 2 // bars either side
	if s.highs.Len() < 2*k+1 {
		return
	}
	// The candidate pivot is k bars back.
	cHigh, cLow := s.highs.At(k), s.lows.At(k)
	isHigh, isLow := true, true
	for i := 0; i <= 2*k; i++ {
		if i == k {
			continue
		}
		if s.highs.At(i) > cHigh {
			isHigh = false
		}
		if s.lows.At(i) < cLow {
			isLow = false
		}
	}
	if isHigh {
		s.pivotHighs = appendCapped(s.pivotHighs, cHigh, 24)
	}
	if isLow {
		s.pivotLows = appendCapped(s.pivotLows, cLow, 24)
	}
}

func appendCapped(s []float64, v float64, max int) []float64 {
	s = append(s, v)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// nearestBelow returns the highest level strictly below price.
func nearestBelow(levels []float64, price float64) (float64, bool) {
	best, found := 0.0, false
	for _, l := range levels {
		if l < price && (!found || l > best) {
			best, found = l, true
		}
	}
	return best, found
}

// nearestAbove returns the lowest level strictly above price.
func nearestAbove(levels []float64, price float64) (float64, bool) {
	best, found := 0.0, false
	for _, l := range levels {
		if l > price && (!found || l < best) {
			best, found = l, true
		}
	}
	return best, found
}

func (e *Engine) buildSnapshot(s *symbol, c domain.Candle) domain.FeatureSnapshot {
	vals := make(map[string]float64, 40)
	warm := make(map[string]bool, 40)
	put := func(name string, v float64, w bool) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			v, w = 0, false
		}
		vals[name] = v
		warm[name] = w
	}

	price := c.Close
	put(domain.FeatLast, price, true)
	put(domain.FeatSMA20, s.sma20.Value(), s.sma20.Warm())
	put(domain.FeatSMA50, s.sma50.Value(), s.sma50.Warm())
	put(domain.FeatSMA200, s.sma200.Value(), s.sma200.Warm())
	put(domain.FeatEMA9, s.ema9.Value(), s.ema9.Warm())
	put(domain.FeatEMA21, s.ema21.Value(), s.ema21.Warm())

	vw := s.vwap.Value()
	put(domain.FeatVWAP, vw, s.vwap.Warm())
	if vw > 0 {
		put(domain.FeatVWAPDist, (price-vw)/vw*10000, s.vwap.Warm())
	} else {
		put(domain.FeatVWAPDist, 0, false)
	}

	put(domain.FeatRSI14, s.rsi14.Value(), s.rsi14.Warm())
	m, sig, hist := s.macd.Values()
	put(domain.FeatMACD, m, s.macd.Warm())
	put(domain.FeatMACDSignal, sig, s.macd.Warm())
	// Normalise the histogram by price so it is comparable across symbols; a
	// raw MACD histogram on a $900 stock and a $20 stock are not the same unit.
	if price > 0 {
		put(domain.FeatMACDHist, hist/price*10000, s.macd.Warm())
	} else {
		put(domain.FeatMACDHist, 0, false)
	}

	atr := s.atr14.Value()
	put(domain.FeatATR14, atr, s.atr14.Warm())
	if price > 0 {
		put(domain.FeatATRPct, atr/price, s.atr14.Warm())
	} else {
		put(domain.FeatATRPct, 0, false)
	}

	up, _, lo := s.bb.Values()
	put(domain.FeatBBUpper, up, s.bb.Warm())
	put(domain.FeatBBLower, lo, s.bb.Warm())
	put(domain.FeatBBWidth, s.bb.Width(), s.bb.Warm())
	put(domain.FeatBBPercentB, s.bb.PercentB(price), s.bb.Warm())

	adx, pdi, mdi := s.adx14.Values()
	put(domain.FeatADX14, adx, s.adx14.Warm())
	put(domain.FeatPlusDI, pdi, s.adx14.Warm())
	put(domain.FeatMinusDI, mdi, s.adx14.Warm())

	// Returns and momentum.
	put(domain.FeatRetLog1, s.logRets.Last(), s.logRets.Len() >= 1)
	put(domain.FeatRetLog5, sumLast(s.logRets, 5), s.logRets.Len() >= 5)
	put(domain.FeatMomentum10, momentum(s.closes, 10), s.closes.Len() > 10)
	put(domain.FeatMomentum60, momentum(s.closes, 60), s.closes.Len() > 60)

	// Relative strength against the benchmark over 20 bars.
	if br, ok := e.benchmarkReturn(20); ok && s.logRets.Len() >= 20 {
		put(domain.FeatRelStrength, sumLast(s.logRets, 20)-br, true)
	} else {
		put(domain.FeatRelStrength, 0, false)
	}

	// Volume behaviour.
	vmean := s.volumes.Mean()
	if vmean > 0 {
		put(domain.FeatVolRatio, c.Volume/vmean, s.volumes.Len() >= 20)
	} else {
		put(domain.FeatVolRatio, 0, false)
	}
	put(domain.FeatVolZScore, ZScore(c.Volume, s.volShort.Mean(), s.volShort.StdDev()), s.volShort.Full())

	// Volatility: annualised realised vol from log returns at this bar size.
	barsPerYear := 252 * 6.5 * 3600 / e.cfg.Interval.Duration().Seconds()
	rv := s.logRets.StdDev() * math.Sqrt(barsPerYear)
	put(domain.FeatRealizedVol, rv, s.logRets.Len() >= 20)
	put(domain.FeatVolOfVol, volOfVol(s.logRets), s.logRets.Len() >= 40)

	// Support and resistance from fractal pivots, falling back to rolling
	// extremes when too few pivots have formed.
	support, hasSup := nearestBelow(s.pivotLows, price)
	if !hasSup && s.lows.Len() > 0 {
		support, hasSup = s.lows.Min(), s.lows.Len() >= 20
	}
	resistance, hasRes := nearestAbove(s.pivotHighs, price)
	if !hasRes && s.highs.Len() > 0 {
		resistance, hasRes = s.highs.Max(), s.highs.Len() >= 20
	}
	put(domain.FeatSupport, support, hasSup)
	put(domain.FeatResistance, resistance, hasRes)
	if hasSup && price > 0 {
		put(domain.FeatDistSupport, (price-support)/price*10000, true)
	} else {
		put(domain.FeatDistSupport, 0, false)
	}
	if hasRes && price > 0 {
		put(domain.FeatDistResist, (resistance-price)/price*10000, true)
	} else {
		put(domain.FeatDistResist, 0, false)
	}

	// Breakout: a close beyond the prior 20-bar extreme on above-average volume.
	// Requiring volume confirmation is what separates a breakout from a wick.
	priorHigh, priorLow := priorExtremes(s.highs, s.lows, 20)
	volConfirm := vmean > 0 && c.Volume > 1.5*vmean
	breakUp, breakDown := 0.0, 0.0
	if s.highs.Len() > 20 && volConfirm && price > priorHigh {
		breakUp = 1
	}
	if s.lows.Len() > 20 && volConfirm && price < priorLow {
		breakDown = 1
	}
	put(domain.FeatBreakoutUp, breakUp, s.highs.Len() > 20)
	put(domain.FeatBreakoutDown, breakDown, s.lows.Len() > 20)

	// Gap from the previous bar's close.
	if s.prevClose > 0 {
		put(domain.FeatGapPct, (c.Open-s.prevClose)/s.prevClose, true)
	} else {
		put(domain.FeatGapPct, 0, false)
	}

	// Trend slope over 20 bars, normalised by price so it is unit-free.
	slope := 0.0
	if s.closes.Len() >= 20 && price > 0 {
		slope = LinearSlope(lastN(s.closes, 20)) / price * 10000
	}
	put(domain.FeatTrendSlope, slope, s.closes.Len() >= 20)
	put(domain.FeatZScore20, ZScore(price, s.sma20.Value(), stdLastN(s.closes, 20)), s.closes.Len() >= 20)

	now := e.cfg.Clock.Now()
	snap := domain.FeatureSnapshot{
		Ticker:     c.Ticker,
		AsOf:       c.End,
		Interval:   e.cfg.Interval,
		Values:     vals,
		Warm:       warm,
		Last:       price,
		Bid:        s.lastQuote.Bid,
		Ask:        s.lastQuote.Ask,
		Volume:     c.Volume,
		SpreadBps:  spreadOf(s.lastQuote),
		LastTickAt: s.lastTickAt,
	}
	if age := now.Sub(s.lastTickAt); e.cfg.StalenessHard > 0 && age >= e.cfg.StalenessHard {
		snap.Stale = true
		snap.StaleReason = "no market data for " + age.Truncate(time.Second).String()
	} else if e.cfg.StalenessSoft > 0 && age >= e.cfg.StalenessSoft {
		snap.Stale = true
		snap.StaleReason = "market data ageing: " + age.Truncate(time.Second).String()
	}
	// Warm-up is deliberately *not* folded into Stale: staleness is about data
	// arrival, warmth is about indicator readiness. They have different
	// remedies and different consumers, so they stay separate signals — the
	// per-feature Warm map carries warmth.
	snap.Finalize()
	if e.cfg.Metrics != nil {
		e.cfg.Metrics.FeatureSnapshots.Inc(string(c.Ticker))
	}
	return snap
}

func spreadOf(q domain.Quote) float64 {
	s := q.SpreadBps()
	if math.IsNaN(s) {
		return 0
	}
	return s
}

func sumLast(w *Window, n int) float64 {
	if w.Len() < n {
		n = w.Len()
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += w.At(i)
	}
	return sum
}

func momentum(closes *Window, n int) float64 {
	if closes.Len() <= n {
		return 0
	}
	old := closes.At(n)
	if old <= 0 {
		return 0
	}
	return closes.At(0)/old - 1
}

func lastN(w *Window, n int) []float64 {
	if w.Len() < n {
		n = w.Len()
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[n-1-i] = w.At(i)
	}
	return out
}

func stdLastN(w *Window, n int) float64 {
	v := lastN(w, n)
	if len(v) < 2 {
		return 0
	}
	mean := 0.0
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	varsum := 0.0
	for _, x := range v {
		varsum += (x - mean) * (x - mean)
	}
	return math.Sqrt(varsum / float64(len(v)))
}

// volOfVol is the standard deviation of rolling 10-bar volatility, a simple
// measure of how unstable the volatility regime is.
func volOfVol(rets *Window) float64 {
	n := rets.Len()
	if n < 40 {
		return 0
	}
	var vols []float64
	for start := 0; start+10 <= 40; start += 5 {
		seg := make([]float64, 10)
		for i := 0; i < 10; i++ {
			seg[i] = rets.At(start + i)
		}
		vols = append(vols, stdOf(seg))
	}
	return stdOf(vols)
}

func stdOf(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	mean := 0.0
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	s := 0.0
	for _, x := range v {
		s += (x - mean) * (x - mean)
	}
	return math.Sqrt(s / float64(len(v)))
}

// priorExtremes returns the highest high and lowest low over the n bars ending
// one bar before the current one — excluding the current bar is what makes a
// breakout test meaningful rather than tautological.
func priorExtremes(highs, lows *Window, n int) (float64, float64) {
	hi, lo := math.Inf(-1), math.Inf(1)
	for i := 1; i <= n && i < highs.Len(); i++ {
		hi = math.Max(hi, highs.At(i))
	}
	for i := 1; i <= n && i < lows.Len(); i++ {
		lo = math.Min(lo, lows.At(i))
	}
	if math.IsInf(hi, 0) {
		hi = 0
	}
	if math.IsInf(lo, 0) {
		lo = 0
	}
	return hi, lo
}

// Warm reports whether a symbol has seen at least the configured warm-up bars.
// Unknown symbols are never warm.
func (e *Engine) Warm(t domain.Ticker) bool {
	e.mu.RLock()
	s, ok := e.symbols[t]
	e.mu.RUnlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bars >= e.cfg.Warmup
}

// Bars returns how many bars a symbol has been fed.
func (e *Engine) Bars(t domain.Ticker) int {
	e.mu.RLock()
	s, ok := e.symbols[t]
	e.mu.RUnlock()
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bars
}

// Tracked returns every symbol the engine holds state for, sorted.
func (e *Engine) Tracked() []domain.Ticker {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]domain.Ticker, 0, len(e.symbols))
	for t := range e.symbols {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Returns exposes a symbol's recent log returns, used by the relationship
// engine for correlation without duplicating history.
func (e *Engine) Returns(t domain.Ticker, n int) []float64 {
	e.mu.RLock()
	s, ok := e.symbols[t]
	e.mu.RUnlock()
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return lastN(s.logRets, n)
}

// LastClose returns the most recent close for a symbol.
func (e *Engine) LastClose(t domain.Ticker) (float64, bool) {
	e.mu.RLock()
	s, ok := e.symbols[t]
	e.mu.RUnlock()
	if !ok {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastClose, s.lastClose > 0
}

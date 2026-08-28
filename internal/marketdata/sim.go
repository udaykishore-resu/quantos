package marketdata

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Sim is a deterministic market simulator.
//
// It exists so the whole platform runs, demonstrably and reproducibly, with no
// paid data feed. The process is intentionally simple but not naive: a common
// market factor with stochastic volatility and fat-tailed jumps, per-symbol
// beta and idiosyncratic risk, an intraday volume smile, and a synthetic VIX
// derived from the market factor's own volatility, so that regime detection has
// something real to detect.
//
// Determinism: each symbol owns a *rand.Rand seeded from (seed, ticker), and
// symbols are stepped in sorted order. The output therefore depends only on the
// seed and the step sequence — never on map iteration order or goroutine
// scheduling. This is what makes the demo scenarios reproducible.
//
// Nothing here is a claim about real markets. No performance number derived
// from simulated data means anything, and the platform labels it as simulated
// through Capabilities().Simulated.
type Sim struct {
	cfg   SimConfig
	clock obs.Clock

	mu      sync.RWMutex
	symbols []domain.Stock
	state   map[domain.Ticker]*symState
	market  marketState
	now     time.Time
	seq     uint64

	scenarios []activeScenario
	events    []domain.CorporateEvent
}

// SimConfig parameterises the simulator.
type SimConfig struct {
	Seed         int64
	Start        time.Time
	Interval     domain.Interval
	TickInterval time.Duration
	Universe     []domain.Stock
	// AnnualDrift and AnnualVol are the market factor's baseline parameters.
	AnnualDrift float64
	AnnualVol   float64
	// JumpIntensity is the expected number of jumps per year.
	JumpIntensity float64
	JumpScale     float64
	// VolMeanReversion controls how fast volatility returns to target.
	VolMeanReversion float64
	VolOfVol         float64
	Clock            obs.Clock
}

// symKind distinguishes instruments whose price process is not a stock's.
type symKind int

const (
	kindEquity symKind = iota
	// kindVolIndex tracks the market's own volatility rather than a price:
	// treating VIX as an ordinary stock would give the regime engine a random
	// number where it expects an implied-volatility level.
	kindVolIndex
	// kindDollar and kindRates are low-volatility macro series with a negative
	// beta to the market factor.
	kindDollar
	kindRates
)

type symState struct {
	kind      symKind
	stock     domain.Stock
	rng       *rand.Rand
	price     float64
	prevClose float64
	vol       float64 // annualised idiosyncratic vol
	volTarget float64
	beta      float64
	alpha     float64
	baseVol   float64 // baseline per-bar volume

	// current bar accumulation
	bar       domain.Candle
	barOpen   bool
	notional  float64
	barVolume float64

	lastQuote domain.Quote
	spreadBps float64
}

type marketState struct {
	rng       *rand.Rand
	level     float64
	vol       float64
	volTarget float64
	drift     float64
	vix       float64
	breadth   float64
	advancers int
	decliners int
}

type activeScenario struct {
	Scenario
	startedAt time.Time
	applied   bool
}

// Scenario perturbs the simulation, which is how the demo scenarios in §46 are
// driven without hand-editing data files.
type Scenario struct {
	Name string
	// Tickers empty means "all symbols".
	Tickers []domain.Ticker
	// DriftShift is added to the annualised drift of the market factor.
	DriftShift float64
	// VolMultiplier scales volatility targets.
	VolMultiplier float64
	// VolumeMultiplier scales traded volume.
	VolumeMultiplier float64
	// GapPct applies a one-off proportional price jump when the scenario starts.
	GapPct float64
	// BreadthShift biases the advance/decline balance in [-1, 1].
	BreadthShift float64
	// Duration bounds the scenario; zero means "until removed".
	Duration time.Duration
	// SpreadMultiplier widens quoted spreads, which is how the risk engine's
	// spread check gets exercised.
	SpreadMultiplier float64
}

// DefaultSimConfig returns sensible baseline parameters.
func DefaultSimConfig(seed int64, start time.Time, universe []domain.Stock) SimConfig {
	return SimConfig{
		Seed:             seed,
		Start:            start,
		Interval:         domain.Interval1m,
		TickInterval:     time.Second,
		Universe:         universe,
		AnnualDrift:      0.06,
		AnnualVol:        0.16,
		JumpIntensity:    30,
		JumpScale:        0.010,
		VolMeanReversion: 6.0,
		VolOfVol:         0.55,
	}
}

func symbolSeed(seed int64, t domain.Ticker) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(t))
	return seed ^ int64(h.Sum64())
}

// NewSim builds a simulator over the given universe.
func NewSim(cfg SimConfig) *Sim {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = time.Second
	}
	if cfg.Interval == "" {
		cfg.Interval = domain.Interval1m
	}
	if cfg.AnnualVol <= 0 {
		cfg.AnnualVol = 0.16
	}
	if cfg.VolMeanReversion <= 0 {
		cfg.VolMeanReversion = 6.0
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	if cfg.Start.IsZero() {
		cfg.Start = cfg.Clock.Now()
	}
	s := &Sim{
		cfg:   cfg,
		clock: cfg.Clock,
		state: map[domain.Ticker]*symState{},
		now:   cfg.Start.UTC().Truncate(cfg.TickInterval),
	}
	s.market = marketState{
		rng:       rand.New(rand.NewSource(cfg.Seed)),
		level:     100,
		vol:       cfg.AnnualVol,
		volTarget: cfg.AnnualVol,
		drift:     cfg.AnnualDrift,
		vix:       cfg.AnnualVol * 100,
	}
	sorted := append([]domain.Stock(nil), cfg.Universe...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Ticker < sorted[j].Ticker })
	s.symbols = sorted
	for _, st := range sorted {
		s.state[st.Ticker] = newSymState(st, cfg)
	}
	return s
}

func newSymState(st domain.Stock, cfg SimConfig) *symState {
	rng := rand.New(rand.NewSource(symbolSeed(cfg.Seed, st.Ticker)))
	beta := st.Beta
	if beta == 0 {
		beta = 0.7 + rng.Float64()*0.9
	}
	// Start from the instrument's reference level when the universe supplies
	// one, so the simulated book shows recognisable prices; otherwise draw a
	// stable per-symbol level so a restart with the same seed reproduces it.
	price := st.ReferencePrice
	if price <= 0 {
		price = 20 + rng.Float64()*380
	}
	idioVol := 0.12 + rng.Float64()*0.35

	kind := kindEquity
	switch st.Ticker {
	case "VIX":
		kind, idioVol = kindVolIndex, 0.9
	case "DXY":
		kind, idioVol = kindDollar, 0.07
	case "TLT", "IEF":
		kind, idioVol = kindRates, 0.12
	}
	// Broad-market and sector ETFs are diversified, so their idiosyncratic
	// component is a fraction of a single name's.
	if st.AssetClass == domain.AssetETF || st.AssetClass == domain.AssetIndex {
		idioVol *= 0.35
	}
	baseVolume := st.AvgVolume
	if baseVolume <= 0 {
		baseVolume = 200_000 + rng.Float64()*3_000_000
	}
	return &symState{
		kind:      kind,
		stock:     st,
		rng:       rng,
		price:     price,
		prevClose: price,
		vol:       idioVol,
		volTarget: idioVol,
		beta:      beta,
		alpha:     (rng.Float64() - 0.5) * 0.04,
		baseVol:   baseVolume / 390, // per one-minute bar over a 6.5h session
		spreadBps: 2 + rng.Float64()*10,
	}
}

// Name implements Provider.
func (s *Sim) Name() string { return "sim" }

// Capabilities implements Provider. Simulated is true so that no consumer can
// mistake this feed for real data.
func (s *Sim) Capabilities() Capabilities {
	return Capabilities{
		Intervals:       []domain.Interval{domain.Interval1m, domain.Interval5m, domain.Interval15m, domain.Interval1h, domain.Interval1d},
		MaxHistory:      365 * 24 * time.Hour,
		Quotes:          true,
		Trades:          true,
		Streaming:       true,
		CorporateEvents: true,
		Adjusted:        true,
		Simulated:       true,
		RateLimitPerSec: 0,
	}
}

// Now returns the simulator's current time.
func (s *Sim) Now() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.now
}

// AddScenario schedules a perturbation from the simulator's current time.
func (s *Sim) AddScenario(sc Scenario) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarios = append(s.scenarios, activeScenario{Scenario: sc, startedAt: s.now})
}

// ClearScenarios removes all active perturbations.
func (s *Sim) ClearScenarios() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarios = nil
}

// ScheduleEvent registers a corporate event, which the risk engine's
// earnings-proximity check consumes.
func (s *Sim) ScheduleEvent(e domain.CorporateEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// Rewind moves the simulator's clock back by d.
//
// It exists for one purpose: a consumer that is about to warm up indicators by
// replaying d worth of simulated time needs to start that far in the past, so
// that when the warm-up finishes the feed is at the present rather than d
// ahead of it. A feed running ahead of the clock the validator reads has every
// quote rejected as future-dated, and the platform then looks healthy while
// emitting nothing.
func (s *Sim) Rewind(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.now = s.now.Add(-d).UTC().Truncate(s.cfg.TickInterval)
	s.mu.Unlock()
}

// AdvanceTo steps the simulation forward until its clock reaches target,
// returning everything produced along the way.
//
// A live process must use this rather than a fixed number of Tick calls. The
// ingest loop is driven by a wall-clock ticker, and any tick that takes longer
// than the interval to process makes the simulated feed fall behind real time.
// Advancing by a fixed step lets that gap accumulate until the freshness gate
// declares the whole universe stale — a failure that looks like a market-data
// outage and is really a scheduling artefact. Slaving the simulated clock to
// the real one keeps the two together no matter how uneven the loop is.
//
// maxSteps bounds the catch-up so a long pause (a laptop suspending, a
// debugger) replays a bounded amount of history instead of blocking the loop
// for minutes. Skipping ahead is the right answer there: the platform should
// resume from the present, not grind through a backlog nobody will act on.
func (s *Sim) AdvanceTo(target time.Time, maxSteps int) ([]domain.Quote, []domain.Candle) {
	if maxSteps <= 0 {
		maxSteps = 600
	}
	s.mu.RLock()
	behind := target.Sub(s.now)
	interval := s.cfg.TickInterval
	s.mu.RUnlock()

	steps := int(behind / interval)
	if steps < 1 {
		steps = 1
	}
	if steps > maxSteps {
		s.mu.Lock()
		// Jump the clock rather than replaying: the skipped interval never
		// reaches a consumer, so nothing downstream is silently fed data it
		// will treat as current.
		s.now = target.Add(-time.Duration(maxSteps) * interval).UTC().Truncate(interval)
		s.mu.Unlock()
		steps = maxSteps
	}

	var quotes []domain.Quote
	var bars []domain.Candle
	for i := 0; i < steps; i++ {
		q, b := s.Tick()
		quotes, bars = q, append(bars, b...)
	}
	return quotes, bars
}

// Tick advances the simulation by one tick interval and returns the quotes
// produced and any bars that completed on this tick.
func (s *Sim) Tick() ([]domain.Quote, []domain.Candle) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.now = s.now.Add(s.cfg.TickInterval)
	dt := s.cfg.TickInterval.Seconds() / (252 * 6.5 * 3600) // in years

	sc := s.effectiveScenario()
	s.stepMarket(dt, sc)

	quotes := make([]domain.Quote, 0, len(s.symbols))
	var closed []domain.Candle
	adv, dec := 0, 0

	for _, st := range s.symbols {
		sym := s.state[st.Ticker]
		applies := sc.applies(st.Ticker)
		r := s.stepSymbol(sym, dt, sc, applies)
		if r > 0 {
			adv++
		} else if r < 0 {
			dec++
		}
		s.seq++
		q := s.makeQuote(sym, sc, applies)
		quotes = append(quotes, q)
		if c, ok := s.accumulate(sym, q); ok {
			closed = append(closed, c)
		}
	}
	s.market.advancers, s.market.decliners = adv, dec
	if adv+dec > 0 {
		s.market.breadth = float64(adv-dec) / float64(adv+dec)
	}
	return quotes, closed
}

// effectiveScenario collapses active scenarios into one perturbation.
func (s *Sim) effectiveScenario() combinedScenario {
	out := combinedScenario{VolMultiplier: 1, VolumeMultiplier: 1, SpreadMultiplier: 1}
	kept := s.scenarios[:0]
	for i := range s.scenarios {
		a := &s.scenarios[i]
		if a.Duration > 0 && s.now.Sub(a.startedAt) > a.Duration {
			continue
		}
		kept = append(kept, *a)
		out.DriftShift += a.DriftShift
		out.BreadthShift += a.BreadthShift
		if a.VolMultiplier > 0 {
			out.VolMultiplier *= a.VolMultiplier
		}
		if a.VolumeMultiplier > 0 {
			out.VolumeMultiplier *= a.VolumeMultiplier
		}
		if a.SpreadMultiplier > 0 {
			out.SpreadMultiplier *= a.SpreadMultiplier
		}
		if !a.applied && a.GapPct != 0 {
			out.GapPct += a.GapPct
			out.gapTickers = append(out.gapTickers, a.Tickers...)
			out.gapAll = out.gapAll || len(a.Tickers) == 0
			s.scenarios[i].applied = true
		}
		if len(a.Tickers) > 0 {
			out.tickers = append(out.tickers, a.Tickers...)
		} else {
			out.all = true
		}
	}
	s.scenarios = kept
	return out
}

type combinedScenario struct {
	DriftShift       float64
	VolMultiplier    float64
	VolumeMultiplier float64
	SpreadMultiplier float64
	BreadthShift     float64
	GapPct           float64
	tickers          []domain.Ticker
	gapTickers       []domain.Ticker
	all              bool
	gapAll           bool
}

func (c combinedScenario) applies(t domain.Ticker) bool {
	if c.all {
		return true
	}
	for _, x := range c.tickers {
		if x == t {
			return true
		}
	}
	return false
}

func (c combinedScenario) gapApplies(t domain.Ticker) bool {
	if c.GapPct == 0 {
		return false
	}
	if c.gapAll {
		return true
	}
	for _, x := range c.gapTickers {
		if x == t {
			return true
		}
	}
	return false
}

func (s *Sim) stepMarket(dt float64, sc combinedScenario) {
	m := &s.market
	target := s.cfg.AnnualVol * sc.VolMultiplier
	// Ornstein-Uhlenbeck volatility with a lognormal-ish shock.
	m.vol += s.cfg.VolMeanReversion*(target-m.vol)*dt + s.cfg.VolOfVol*m.vol*math.Sqrt(dt)*m.rng.NormFloat64()
	m.vol = domain.Clamp(m.vol, 0.03, 2.5)

	drift := s.cfg.AnnualDrift + sc.DriftShift
	ret := drift*dt + m.vol*math.Sqrt(dt)*m.rng.NormFloat64()
	if m.rng.Float64() < s.cfg.JumpIntensity*dt {
		ret += s.cfg.JumpScale * m.rng.NormFloat64() * 3
	}
	m.level *= math.Exp(ret)
	m.drift = drift

	// A synthetic VIX: annualised market vol, scaled, with the usual inverse
	// relationship to returns and a floor.
	implied := m.vol * 100 * (1 + 0.5*math.Max(0, -ret*50))
	m.vix += 0.2 * (implied - m.vix)
	m.vix = domain.Clamp(m.vix, 8, 90)
}

func (s *Sim) stepSymbol(sym *symState, dt float64, sc combinedScenario, applies bool) float64 {
	// The volatility index is not a traded price process: it tracks the market
	// factor's own volatility, which is what the regime engine expects to read.
	if sym.kind == kindVolIndex {
		prev := sym.price
		sym.price += 0.35 * (s.market.vix - sym.price)
		sym.price = domain.Clamp(sym.price*(1+0.01*sym.rng.NormFloat64()), 8, 95)
		if prev <= 0 {
			return 0
		}
		return math.Log(sym.price / prev)
	}

	volMul := 1.0
	drift := 0.0
	if applies {
		volMul = sc.VolMultiplier
		// A scenario's breadth shift is expressed as additional drift, because
		// breadth is measured downstream from advancers and decliners: without
		// this, a "market turns bullish" scenario would move the index and
		// leave participation flat, which is not what a broadening rally is.
		drift = sc.DriftShift + sc.BreadthShift*6
	}
	target := sym.volTarget * volMul
	sym.vol += s.cfg.VolMeanReversion*(target-sym.vol)*dt + s.cfg.VolOfVol*sym.vol*math.Sqrt(dt)*sym.rng.NormFloat64()
	sym.vol = domain.Clamp(sym.vol, 0.03, 3.0)

	marketRet := math.Log(s.market.level / 100)
	_ = marketRet
	// Market factor increment this step, recovered from the market's own move.
	mktInc := s.market.drift*dt + s.market.vol*math.Sqrt(dt)*0 // deterministic part
	idio := sym.vol * math.Sqrt(dt) * sym.rng.NormFloat64()
	beta := sym.beta
	switch sym.kind {
	case kindDollar, kindRates:
		// Macro series move against equity risk appetite.
		beta = -math.Abs(beta)
	}
	ret := beta*(mktInc+s.marketShock(dt)) + sym.alpha*dt + idio + drift*dt
	if sym.rng.Float64() < s.cfg.JumpIntensity*dt*0.5 {
		ret += s.cfg.JumpScale * sym.rng.NormFloat64() * 2
	}
	if sc.gapApplies(sym.stock.Ticker) {
		ret += math.Log(1 + sc.GapPct)
	}
	sym.price *= math.Exp(ret)
	sym.price = math.Max(sym.price, 0.5)
	return ret
}

// marketShock returns the stochastic part of the market factor's last step, so
// that symbols inherit correlated moves.
func (s *Sim) marketShock(dt float64) float64 {
	return s.market.vol * math.Sqrt(dt) * s.market.rng.NormFloat64() * 0.35
}

func (s *Sim) makeQuote(sym *symState, sc combinedScenario, applies bool) domain.Quote {
	spreadMul := 1.0
	volMul := 1.0
	if applies {
		spreadMul = sc.SpreadMultiplier
		volMul = sc.VolumeMultiplier
	}
	if sym.kind == kindVolIndex {
		// An index has no book. Publishing a synthetic one would give the
		// validator a crossed or absurd spread to reject.
		q := domain.Quote{
			Ticker: sym.stock.Ticker, Last: round4(sym.price), LastSize: 0,
			Timestamp: s.now, Seq: s.seq, Source: "sim",
		}
		sym.lastQuote = q
		return q
	}
	// Spread widens with volatility, which is what makes the risk engine's
	// spread and liquidity checks meaningful under a vol-spike scenario.
	spreadBps := sym.spreadBps * spreadMul * (1 + 3*math.Max(0, sym.vol-sym.volTarget))
	half := sym.price * spreadBps / 20000

	smile := intradaySmile(s.now)
	size := sym.baseVol * smile * volMul * (0.6 + 0.8*sym.rng.Float64())
	sym.barVolume += size
	sym.notional += size * sym.price

	q := domain.Quote{
		Ticker:    sym.stock.Ticker,
		Bid:       round4(sym.price - half),
		Ask:       round4(sym.price + half),
		BidSize:   math.Round(size * 0.4),
		AskSize:   math.Round(size * 0.4),
		Last:      round4(sym.price),
		LastSize:  math.Round(size),
		Volume:    math.Round(sym.barVolume),
		Timestamp: s.now,
		Seq:       s.seq,
		Source:    "sim",
	}
	sym.lastQuote = q
	return q
}

// intradaySmile models the U-shaped volume profile of a US equity session.
func intradaySmile(t time.Time) float64 {
	h := float64(t.UTC().Hour()) + float64(t.UTC().Minute())/60
	// Session 13:30-20:00 UTC.
	x := (h - 13.5) / 6.5
	if x < 0 || x > 1 {
		return 0.25 // pre/post market
	}
	return 0.6 + 1.6*(math.Pow(2*x-1, 2))
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// accumulate folds a quote into the open bar, returning a completed bar when
// the interval rolls.
func (s *Sim) accumulate(sym *symState, q domain.Quote) (domain.Candle, bool) {
	d := s.cfg.Interval.Duration()
	start := q.Timestamp.Truncate(d)
	if !sym.barOpen {
		sym.bar = domain.Candle{
			Ticker: sym.stock.Ticker, Interval: s.cfg.Interval,
			Open: q.Last, High: q.Last, Low: q.Last, Close: q.Last,
			Start: start, End: start.Add(d), Adjusted: true, Source: "sim",
		}
		sym.barOpen = true
		sym.barVolume = q.LastSize
		sym.notional = q.LastSize * q.Last
		return domain.Candle{}, false
	}
	if start.After(sym.bar.Start) {
		out := sym.bar
		out.Volume = math.Round(sym.barVolume)
		if sym.barVolume > 0 {
			out.VWAP = round4(sym.notional / sym.barVolume)
		} else {
			out.VWAP = out.Close
		}
		out.Sequence = s.seq
		sym.prevClose = out.Close
		sym.bar = domain.Candle{
			Ticker: sym.stock.Ticker, Interval: s.cfg.Interval,
			Open: q.Last, High: q.Last, Low: q.Last, Close: q.Last,
			Start: start, End: start.Add(d), Adjusted: true, Source: "sim",
		}
		sym.barVolume = q.LastSize
		sym.notional = q.LastSize * q.Last
		return out, true
	}
	sym.bar.Close = q.Last
	sym.bar.High = math.Max(sym.bar.High, q.Last)
	sym.bar.Low = math.Min(sym.bar.Low, q.Last)
	sym.bar.Trades++
	return domain.Candle{}, false
}

// Warmup runs n ticks discarding output, so that indicators have history before
// the platform starts observing. It keeps determinism because it uses the same
// step function.
func (s *Sim) Warmup(n int) []domain.Candle {
	var bars []domain.Candle
	for i := 0; i < n; i++ {
		_, closed := s.Tick()
		bars = append(bars, closed...)
	}
	return bars
}

// GetQuotes implements Provider using the last generated book.
func (s *Sim) GetQuotes(_ context.Context, tickers []domain.Ticker) ([]domain.Quote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Quote, 0, len(tickers))
	for _, t := range tickers {
		st, ok := s.state[t]
		if !ok {
			continue
		}
		out = append(out, st.lastQuote)
	}
	return out, nil
}

// GetHistoricalBars synthesises history backwards from the current state using
// a separate, seed-derived generator, so the same request always returns the
// same series.
func (s *Sim) GetHistoricalBars(_ context.Context, req BarRequest) ([]domain.Candle, error) {
	if !s.Capabilities().Supports(req.Interval) {
		return nil, ErrUnsupported
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := req.Interval.Duration()
	out := []domain.Candle{}
	for _, t := range req.Tickers {
		st, ok := s.state[t]
		if !ok {
			return nil, ErrNotFound
		}
		n := int(req.End.Sub(req.Start) / d)
		if req.Limit > 0 && n > req.Limit {
			n = req.Limit
		}
		if n <= 0 {
			continue
		}
		// A truncated history must be the most *recent* bars, not the oldest.
		// The walk below starts from the current price and works backwards, so
		// anchoring the series to req.Start while capping the count would stamp
		// present-day prices with timestamps from the far end of the window —
		// and a model trained on that data would be fitted to a period it will
		// never see.
		firstStart := req.End.Add(-time.Duration(n) * d)
		if firstStart.Before(req.Start) {
			firstStart = req.Start
		}
		rng := rand.New(rand.NewSource(symbolSeed(s.cfg.Seed+7919, t)))
		price := st.price
		if price <= 0 {
			price = st.stock.ReferencePrice
		}
		series := make([]domain.Candle, n)
		// Walk backwards from the current price so history joins the live feed.
		for i := n - 1; i >= 0; i-- {
			start := firstStart.Add(time.Duration(i) * d)
			vol := st.volTarget * math.Sqrt(d.Seconds()/(252*6.5*3600))
			ret := vol * rng.NormFloat64()
			prev := price / math.Exp(ret)
			hi := math.Max(price, prev) * (1 + math.Abs(rng.NormFloat64())*vol*0.5)
			lo := math.Min(price, prev) * (1 - math.Abs(rng.NormFloat64())*vol*0.5)
			volume := math.Round(st.baseVol * (0.5 + rng.Float64()))
			series[i] = domain.Candle{
				Ticker: t, Interval: req.Interval,
				Open: round4(prev), High: round4(hi), Low: round4(lo), Close: round4(price),
				Volume: volume, VWAP: round4((hi + lo + price) / 3),
				Start: start, End: start.Add(d), Adjusted: true, Source: "sim",
			}
			price = prev
		}
		out = append(out, series...)
	}
	return out, nil
}

// GetTrades implements Provider by emitting one print per tick interval.
func (s *Sim) GetTrades(_ context.Context, req TradeRequest) ([]domain.Trade, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.state[req.Ticker]
	if !ok {
		return nil, ErrNotFound
	}
	return []domain.Trade{{
		Ticker: req.Ticker, Price: st.lastQuote.Last, Size: st.lastQuote.LastSize,
		Timestamp: s.now, Seq: s.seq, Source: "sim",
	}}, nil
}

// GetMarketStatus implements Provider with a simple US-equity session model.
func (s *Sim) GetMarketStatus(_ context.Context) (domain.MarketStatus, error) {
	s.mu.RLock()
	now := s.now
	s.mu.RUnlock()
	return domain.MarketStatus{Session: sessionFor(now), Timestamp: now, Venue: "SIM"}, nil
}

func sessionFor(t time.Time) domain.MarketSession {
	switch t.UTC().Weekday() {
	case time.Saturday, time.Sunday:
		return domain.SessionClosed
	}
	mins := t.UTC().Hour()*60 + t.UTC().Minute()
	switch {
	case mins >= 13*60+30 && mins < 20*60:
		return domain.SessionOpen
	case mins >= 8*60 && mins < 13*60+30:
		return domain.SessionPreMarket
	case mins >= 20*60 && mins < 24*60:
		return domain.SessionAfterHours
	default:
		return domain.SessionClosed
	}
}

// GetCorporateEvents implements Provider from the scheduled event list.
func (s *Sim) GetCorporateEvents(_ context.Context, req EventRequest) ([]domain.CorporateEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := map[domain.Ticker]bool{}
	for _, t := range req.Tickers {
		want[t] = true
	}
	out := []domain.CorporateEvent{}
	for _, e := range s.events {
		if len(want) > 0 && !want[e.Ticker] {
			continue
		}
		if !req.Start.IsZero() && e.ScheduledAt.Before(req.Start) {
			continue
		}
		if !req.End.IsZero() && e.ScheduledAt.After(req.End) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	return out, nil
}

// MarketContext exposes the simulated market-wide state that the regime engine
// consumes when running against the simulator.
type MarketContext struct {
	Level     float64
	Vol       float64
	VIX       float64
	Breadth   float64
	Advancers int
	Decliners int
}

// Context returns the simulated market context.
func (s *Sim) Context() MarketContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return MarketContext{
		Level: s.market.level, Vol: s.market.vol, VIX: s.market.vix,
		Breadth: s.market.breadth, Advancers: s.market.advancers, Decliners: s.market.decliners,
	}
}

// Price returns the current simulated price for a symbol.
func (s *Sim) Price(t domain.Ticker) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.state[t]
	if !ok {
		return 0, false
	}
	return st.price, true
}

// NamedScenario returns one of the built-in market conditions by name.
//
// These exist so a local operator can reach the parts of the platform a quiet
// random walk never exercises. A flat tape produces no directional edge, the
// signal engine correctly declines to act, and someone reading the dashboard
// concludes the platform is broken when it is in fact working exactly as
// designed. Being able to say "show me a trending market" is the difference
// between that and a demonstration.
func NamedScenario(name string) (Scenario, bool) {
	switch name {
	case "", "none":
		return Scenario{}, false
	case "bull":
		return Scenario{Name: "bull trend", DriftShift: 0.55, BreadthShift: 0.45, VolumeMultiplier: 1.3}, true
	case "bear":
		return Scenario{Name: "bear trend", DriftShift: -0.55, BreadthShift: -0.45, VolMultiplier: 1.4, VolumeMultiplier: 1.4}, true
	case "volatile":
		return Scenario{Name: "high volatility", VolMultiplier: 3.0, VolumeMultiplier: 2.0, SpreadMultiplier: 2.5}, true
	case "crash":
		return Scenario{Name: "risk-off shock", DriftShift: -1.6, GapPct: -0.045, VolMultiplier: 3.5,
			VolumeMultiplier: 3.0, BreadthShift: -0.85, SpreadMultiplier: 4.0}, true
	case "illiquid":
		return Scenario{Name: "illiquid tape", VolumeMultiplier: 0.15, SpreadMultiplier: 8.0}, true
	case "breakout":
		return Scenario{Name: "breakout", DriftShift: 0.9, GapPct: 0.012, VolumeMultiplier: 2.4, BreadthShift: 0.6}, true
	}
	return Scenario{}, false
}

// ScenarioNames lists the built-in scenarios, for help text.
func ScenarioNames() []string {
	return []string{"bull", "bear", "volatile", "crash", "illiquid", "breakout"}
}

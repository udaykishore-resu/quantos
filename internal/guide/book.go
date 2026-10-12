package guide

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Series is daily history per instrument, sorted by time.
type Series map[domain.Ticker][]domain.Candle

// Caps are the concentration, correlation and drawdown thresholds the risk
// engine enforces on the paper book, applied here to a real one. They come
// from the same configuration block so the two never disagree.
type Caps struct {
	MaxPosition, SoftPosition     float64
	MaxSector, SoftSector         float64
	MaxCorrelated, SoftCorrelated float64
	MaxDrawdown, DrawdownWarn     float64
	CorrelationThreshold          float64 // pairwise correlation above which two names count as one exposure
}

// DefaultCaps mirrors risk.DefaultConfig.
func DefaultCaps() Caps {
	return Caps{
		MaxPosition: 0.10, SoftPosition: 0.07,
		MaxSector: 0.35, SoftSector: 0.25,
		MaxCorrelated: 0.50, SoftCorrelated: 0.35,
		MaxDrawdown: 0.20, DrawdownWarn: 0.12,
		CorrelationThreshold: 0.60,
	}
}

// Window is a labelled date range.
type Window struct {
	Label string    `json:"label"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// DefaultStressWindows are episodes every US equity book should be asked
// about. They are not forecasts; they are what the current weights would
// have done, with the usual caveat that the names existed then in the form
// they exist now.
func DefaultStressWindows() []Window {
	d := func(y int, m time.Month, dd int) time.Time { return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC) }
	return []Window{
		{"Q4 2018 selloff", d(2018, 10, 1), d(2018, 12, 24)},
		{"COVID crash", d(2020, 2, 19), d(2020, 3, 23)},
		{"2022 bear market", d(2022, 1, 3), d(2022, 10, 12)},
		{"Apr 2025 tariff shock", d(2025, 2, 19), d(2025, 4, 8)},
	}
}

// BookConfig parameterises an assessment.
type BookConfig struct {
	Caps      Caps
	Benchmark domain.Ticker
	AsOf      time.Time // zero: the last date with data
	Stress    []Window
	// Lookup resolves sector and asset class; nil treats everything as an
	// equity of unknown sector.
	Lookup func(domain.Ticker) (domain.Stock, bool)
}

// Position is one holding, marked.
type Position struct {
	Ticker        domain.Ticker     `json:"ticker"`
	Quantity      float64           `json:"quantity"`
	Price         float64           `json:"price"`
	Value         float64           `json:"value"`
	Weight        float64           `json:"weight"`
	Sector        string            `json:"sector,omitempty"`
	AssetClass    domain.AssetClass `json:"asset_class,omitempty"`
	CostBasis     float64           `json:"cost_basis,omitempty"`
	UnrealizedPnL float64           `json:"unrealized_pnl,omitempty"`
	Vol1y         float64           `json:"vol_1y"`
	Beta1y        float64           `json:"beta_1y"`
	Return1y      float64           `json:"return_1y"`
	MaxDrawdown1y float64           `json:"max_drawdown_1y"`
	// CorrelatedExposure is the weight of *other* positions whose one-year
	// return correlation with this one exceeds the threshold: what the book
	// is really exposed to when this name moves.
	CorrelatedExposure float64         `json:"correlated_exposure"`
	CorrelatedWith     []domain.Ticker `json:"correlated_with,omitempty"`
	DataFrom           time.Time       `json:"data_from"`
}

// WindowStats is the book, at its current weights, over one window.
type WindowStats struct {
	Window          Window  `json:"window"`
	Days            int     `json:"days"`
	Return          float64 `json:"return"`
	AnnualReturn    float64 `json:"annual_return"`
	Volatility      float64 `json:"volatility"`
	Sharpe          float64 `json:"sharpe"`
	MaxDrawdown     float64 `json:"max_drawdown"`
	Beta            float64 `json:"beta"`
	BenchmarkReturn float64 `json:"benchmark_return"`
	BenchmarkMaxDD  float64 `json:"benchmark_max_drawdown"`
	Covered         bool    `json:"covered"`
}

// Pair is a pairwise correlation worth looking at.
type Pair struct {
	A           domain.Ticker `json:"a"`
	B           domain.Ticker `json:"b"`
	Correlation float64       `json:"correlation"`
	JointWeight float64       `json:"joint_weight"`
}

// Flag is one finding against the caps.
type Flag struct {
	Level  string `json:"level"` // FAIL | WARN | INFO
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

// Book is the assessment.
type Book struct {
	Name           string             `json:"name"`
	AsOf           time.Time          `json:"as_of"`
	Equity         float64            `json:"equity"`
	Cash           float64            `json:"cash"`
	CashWeight     float64            `json:"cash_weight"`
	Positions      []Position         `json:"positions"`
	SectorExposure map[string]float64 `json:"sector_exposure"`
	ETFWeight      float64            `json:"etf_weight"`
	// Exposures is the look-through: per company, the weight held directly
	// plus its share of every tracked index fund in the book, largest first.
	Exposures   []Exposure      `json:"exposures,omitempty"`
	SingleNames float64         `json:"single_name_weight"`
	HHI         float64         `json:"hhi"`
	EffectiveN  float64         `json:"effective_positions"`
	TopPairs    []Pair          `json:"top_pairs"`
	Trailing    []WindowStats   `json:"trailing"`
	Stress      []WindowStats   `json:"stress"`
	Flags       []Flag          `json:"flags"`
	Missing     []domain.Ticker `json:"missing_data,omitempty"`
	Benchmark   domain.Ticker   `json:"benchmark"`
	Notes       []string        `json:"notes"`
	Disclaimer  string          `json:"disclaimer"`
}

// Assess marks the holdings and measures the book at its current weights.
func Assess(h Holdings, bars Series, cfg BookConfig) (Book, error) {
	if cfg.Caps == (Caps{}) {
		cfg.Caps = DefaultCaps()
	}
	if cfg.Benchmark == "" {
		cfg.Benchmark = "SPY"
	}
	if cfg.Stress == nil {
		cfg.Stress = DefaultStressWindows()
	}
	base := cfg.Lookup
	if base == nil {
		base = func(t domain.Ticker) (domain.Stock, bool) { return domain.Stock{}, false }
	}
	// Common index funds are recognised even when the research universe does
	// not list them, so a fund is never mistaken for a single stock.
	lookup := func(t domain.Ticker) (domain.Stock, bool) {
		if s, ok := base(t); ok {
			return s, true
		}
		return KnownFund(t)
	}

	// Calendar: the benchmark's dates, which is what "a trading day" means
	// for a US book.
	bench := bars[cfg.Benchmark]
	if len(bench) < 2 {
		return Book{}, fmt.Errorf("guide: no history for benchmark %s", cfg.Benchmark)
	}
	asOf := cfg.AsOf
	if asOf.IsZero() {
		asOf = bench[len(bench)-1].Start
	}
	var dates []time.Time
	for _, c := range bench {
		if !c.Start.After(asOf) {
			dates = append(dates, c.Start)
		}
	}
	if len(dates) < 2 {
		return Book{}, fmt.Errorf("guide: no benchmark history on or before %s", asOf.Format("2006-01-02"))
	}
	dateIdx := make(map[time.Time]int, len(dates))
	for i, d := range dates {
		dateIdx[d] = i
	}

	// Close matrix aligned to the calendar; NaN where an instrument has no
	// bar that day.
	closes := map[domain.Ticker][]float64{}
	first := map[domain.Ticker]time.Time{}
	align := func(t domain.Ticker) bool {
		s := bars[t]
		if len(s) == 0 {
			return false
		}
		row := make([]float64, len(dates))
		for i := range row {
			row[i] = math.NaN()
		}
		seen := 0
		for _, c := range s {
			if i, ok := dateIdx[c.Start]; ok {
				row[i] = c.Close
				if seen == 0 {
					first[t] = c.Start
				}
				seen++
			}
		}
		if seen < 2 {
			return false
		}
		closes[t] = row
		return true
	}
	align(cfg.Benchmark)

	book := Book{
		Name: h.Name, AsOf: asOf, Cash: h.Cash, Benchmark: cfg.Benchmark,
		SectorExposure: map[string]float64{}, Disclaimer: domain.Disclaimer,
	}

	// Mark.
	last := len(dates) - 1
	for _, l := range h.Lines {
		if !align(l.Ticker) {
			book.Missing = append(book.Missing, l.Ticker)
			continue
		}
		px := lastValid(closes[l.Ticker], last)
		if px <= 0 {
			book.Missing = append(book.Missing, l.Ticker)
			continue
		}
		stock, _ := lookup(l.Ticker)
		p := Position{
			Ticker: l.Ticker, Quantity: l.Quantity, Price: px, Value: l.Quantity * px,
			Sector: stock.Sector, AssetClass: stock.AssetClass, CostBasis: l.CostBasis,
			DataFrom: first[l.Ticker],
		}
		if p.AssetClass == "" {
			p.AssetClass = domain.AssetEquity
		}
		if l.CostBasis > 0 {
			p.UnrealizedPnL = (px - l.CostBasis) * l.Quantity
		}
		book.Positions = append(book.Positions, p)
	}
	book.Equity = book.Cash
	for _, p := range book.Positions {
		book.Equity += p.Value
	}
	if book.Equity <= 0 {
		return Book{}, fmt.Errorf("guide: book has no value")
	}
	book.CashWeight = book.Cash / book.Equity
	hhi := 0.0
	for i := range book.Positions {
		p := &book.Positions[i]
		p.Weight = p.Value / book.Equity
		hhi += p.Weight * p.Weight
		if p.AssetClass == domain.AssetETF {
			book.ETFWeight += p.Weight
		} else {
			book.SingleNames += p.Weight
			sector := p.Sector
			if sector == "" {
				sector = "Unknown"
			}
			book.SectorExposure[sector] += p.Weight
		}
	}
	book.HHI = hhi
	if hhi > 0 {
		book.EffectiveN = 1 / hhi
	}
	book.Exposures = lookThrough(book.Positions)

	// Daily returns per instrument on the calendar.
	rets := map[domain.Ticker][]float64{}
	for t, row := range closes {
		r := make([]float64, len(row))
		r[0] = math.NaN()
		for i := 1; i < len(row); i++ {
			if math.IsNaN(row[i]) || math.IsNaN(row[i-1]) || row[i-1] <= 0 {
				r[i] = math.NaN()
			} else {
				r[i] = row[i]/row[i-1] - 1
			}
		}
		rets[t] = r
	}
	bret := rets[cfg.Benchmark]

	// Book return at current weights. A missing return counts as zero for
	// that name on that day, which understates risk before a name's
	// listing; DataFrom on each position says where that starts.
	bookRet := make([]float64, len(dates))
	for i := 1; i < len(dates); i++ {
		sum := 0.0
		for _, p := range book.Positions {
			if r := rets[p.Ticker][i]; !math.IsNaN(r) {
				sum += p.Weight * r
			}
		}
		bookRet[i] = sum
	}

	// Per-position one-year statistics and correlations.
	oneYear := max(1, len(dates)-252)
	for i := range book.Positions {
		p := &book.Positions[i]
		r := rets[p.Ticker][oneYear:]
		b := bret[oneYear:]
		p.Vol1y = annualVol(r)
		p.Beta1y = beta(r, b)
		p.Return1y = compound(r)
		p.MaxDrawdown1y = maxDrawdown(r)
	}
	n := len(book.Positions)
	corr := make([][]float64, n)
	for i := range corr {
		corr[i] = make([]float64, n)
		for j := range corr[i] {
			if i == j {
				corr[i][j] = 1
				continue
			}
			corr[i][j] = correlation(rets[book.Positions[i].Ticker][oneYear:], rets[book.Positions[j].Ticker][oneYear:])
		}
	}
	for i := range book.Positions {
		p := &book.Positions[i]
		for j := range book.Positions {
			if i != j && corr[i][j] >= cfg.Caps.CorrelationThreshold {
				p.CorrelatedExposure += book.Positions[j].Weight
				p.CorrelatedWith = append(p.CorrelatedWith, book.Positions[j].Ticker)
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if !math.IsNaN(corr[i][j]) {
				book.TopPairs = append(book.TopPairs, Pair{
					A: book.Positions[i].Ticker, B: book.Positions[j].Ticker, Correlation: corr[i][j],
					JointWeight: book.Positions[i].Weight + book.Positions[j].Weight,
				})
			}
		}
	}
	sort.Slice(book.TopPairs, func(i, j int) bool { return book.TopPairs[i].Correlation > book.TopPairs[j].Correlation })
	if len(book.TopPairs) > 8 {
		book.TopPairs = book.TopPairs[:8]
	}

	// Trailing windows.
	for _, w := range []struct {
		label string
		days  int
	}{{"trailing 1y", 252}, {"trailing 3y", 756}, {"trailing 5y", 1260}, {"full history", len(dates) - 1}} {
		if w.days <= 0 || w.days > len(dates)-1 {
			continue
		}
		from := len(dates) - 1 - w.days
		book.Trailing = append(book.Trailing, windowStats(Window{w.label, dates[from], dates[last]}, bookRet[from+1:], bret[from+1:], true))
	}
	// Stress windows.
	for _, w := range cfg.Stress {
		i0, i1 := indexAtOrAfter(dates, w.Start), indexAtOrBefore(dates, w.End)
		if i0 < 0 || i1 < 0 || i1 <= i0 {
			book.Stress = append(book.Stress, WindowStats{Window: w})
			continue
		}
		book.Stress = append(book.Stress, windowStats(w, bookRet[i0+1:i1+1], bret[i0+1:i1+1], true))
	}

	book.Flags = flags(book, cfg.Caps)
	book.Notes = append(book.Notes,
		"weights are today's; every trailing and stress figure holds them constant with daily rebalancing, so it measures the exposure you have now, not the path you took",
		"a position's return before its first bar counts as zero, which flatters volatility for recently listed names; see data_from",
		"correlated exposure counts the weight of other names whose one-year daily correlation with the position exceeds the threshold",
		"concentration counts each company through the index funds that hold it, using the funds' largest constituents as of "+LookThroughAsOf+"; funds without tracked constituents are held to no single-company cap",
	)
	return book, nil
}

func flags(b Book, caps Caps) []Flag {
	var out []Flag
	// Concentration is measured per company through the funds that hold it:
	// what matters is how much of the money rides on one business, however
	// it is held.
	for _, e := range b.Exposures {
		how := fmt.Sprintf("%.1f%% held directly", e.Direct*100)
		if e.ViaFunds > 0 {
			how += fmt.Sprintf(" + %.1f%% through %s", e.ViaFunds*100, joinTickers(e.Funds))
		}
		switch {
		case e.Total > caps.MaxPosition:
			out = append(out, Flag{"FAIL", "concentration", fmt.Sprintf("%s is %.1f%% of the book (%s); the single-company cap is %.0f%%", e.Ticker, e.Total*100, how, caps.MaxPosition*100)})
		case e.Total > caps.SoftPosition:
			out = append(out, Flag{"WARN", "concentration", fmt.Sprintf("%s is %.1f%% of the book (%s), above the %.0f%% soft limit", e.Ticker, e.Total*100, how, caps.SoftPosition*100)})
		}
	}
	for _, p := range b.Positions {
		if p.AssetClass == domain.AssetETF {
			continue
		}
		x := p.Weight + p.CorrelatedExposure
		switch {
		case x > caps.MaxCorrelated:
			out = append(out, Flag{"FAIL", "correlation", fmt.Sprintf("%s plus the names it moves with (%s) is %.0f%% of the book; the cap is %.0f%%", p.Ticker, joinTickers(p.CorrelatedWith), x*100, caps.MaxCorrelated*100)})
		case x > caps.SoftCorrelated:
			out = append(out, Flag{"WARN", "correlation", fmt.Sprintf("%s plus the names it moves with (%s) is %.0f%% of the book, above the %.0f%% soft limit", p.Ticker, joinTickers(p.CorrelatedWith), x*100, caps.SoftCorrelated*100)})
		}
	}
	for sector, w := range b.SectorExposure {
		if sector == "Unknown" {
			// A stock outside the research universe has no sector here. Holding
			// it to a sector cap would compare unrelated companies as one.
			out = append(out, Flag{"INFO", "sector", fmt.Sprintf("%.0f%% of the book is in single names with no sector on record; they are not held to the sector cap", w*100)})
			continue
		}
		switch {
		case w > caps.MaxSector:
			out = append(out, Flag{"FAIL", "sector", fmt.Sprintf("single-name %s exposure is %.0f%%; the cap is %.0f%%", sector, w*100, caps.MaxSector*100)})
		case w > caps.SoftSector:
			out = append(out, Flag{"WARN", "sector", fmt.Sprintf("single-name %s exposure is %.0f%%, above the %.0f%% soft limit", sector, w*100, caps.SoftSector*100)})
		}
	}
	for _, t := range b.Trailing {
		if t.Window.Label != "trailing 1y" {
			continue
		}
		switch {
		case t.MaxDrawdown > caps.MaxDrawdown:
			out = append(out, Flag{"FAIL", "drawdown", fmt.Sprintf("at today's weights the book fell %.0f%% peak-to-trough in the last year; the cap is %.0f%%", t.MaxDrawdown*100, caps.MaxDrawdown*100)})
		case t.MaxDrawdown > caps.DrawdownWarn:
			out = append(out, Flag{"WARN", "drawdown", fmt.Sprintf("at today's weights the book fell %.0f%% peak-to-trough in the last year, above the %.0f%% warning level", t.MaxDrawdown*100, caps.DrawdownWarn*100)})
		}
	}
	if b.EffectiveN > 0 && b.EffectiveN < 5 && b.ETFWeight < 0.5 {
		out = append(out, Flag{"WARN", "diversification", fmt.Sprintf("effective number of positions is %.1f; most of the risk sits in a handful of names", b.EffectiveN)})
	}
	if b.CashWeight > 0.25 {
		out = append(out, Flag{"INFO", "cash", fmt.Sprintf("%.0f%% of the book is cash", b.CashWeight*100)})
	}
	if len(b.Missing) > 0 {
		out = append(out, Flag{"INFO", "data", fmt.Sprintf("no history for %s; excluded from every figure", joinTickers(b.Missing))})
	}
	sort.SliceStable(out, func(i, j int) bool { return levelRank(out[i].Level) < levelRank(out[j].Level) })
	return out
}

func levelRank(l string) int {
	switch l {
	case "FAIL":
		return 0
	case "WARN":
		return 1
	default:
		return 2
	}
}

func joinTickers(ts []domain.Ticker) string {
	s := make([]string, 0, len(ts))
	for _, t := range ts {
		s = append(s, string(t))
	}
	if len(s) == 0 {
		return "none"
	}
	return joinStrings(s, ", ")
}

func joinStrings(s []string, sep string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}

// --- statistics ---------------------------------------------------------------

func windowStats(w Window, r, b []float64, covered bool) WindowStats {
	ws := WindowStats{Window: w, Days: len(r), Covered: covered}
	if len(r) == 0 {
		return ws
	}
	ws.Return = compound(r)
	ws.Volatility = annualVol(r)
	if ws.Days > 0 {
		ws.AnnualReturn = math.Pow(1+ws.Return, 252/float64(ws.Days)) - 1
	}
	if m, sd := meanStd(r); sd > 0 {
		ws.Sharpe = m / sd * math.Sqrt(252)
	}
	ws.MaxDrawdown = maxDrawdown(r)
	ws.Beta = beta(r, b)
	ws.BenchmarkReturn = compound(b)
	ws.BenchmarkMaxDD = maxDrawdown(b)
	return ws
}

func lastValid(row []float64, upto int) float64 {
	for i := upto; i >= 0 && i >= upto-10; i-- {
		if !math.IsNaN(row[i]) && row[i] > 0 {
			return row[i]
		}
	}
	return 0
}

func clean(v []float64) []float64 {
	out := make([]float64, 0, len(v))
	for _, x := range v {
		if !math.IsNaN(x) {
			out = append(out, x)
		}
	}
	return out
}

func meanStd(v []float64) (float64, float64) {
	v = clean(v)
	if len(v) < 2 {
		return 0, 0
	}
	m := 0.0
	for _, x := range v {
		m += x
	}
	m /= float64(len(v))
	ss := 0.0
	for _, x := range v {
		ss += (x - m) * (x - m)
	}
	return m, math.Sqrt(ss / float64(len(v)-1))
}

func annualVol(r []float64) float64 {
	_, sd := meanStd(r)
	return sd * math.Sqrt(252)
}

func compound(r []float64) float64 {
	g := 1.0
	for _, x := range r {
		if !math.IsNaN(x) {
			g *= 1 + x
		}
	}
	return g - 1
}

func maxDrawdown(r []float64) float64 {
	eq, peak, dd := 1.0, 1.0, 0.0
	for _, x := range r {
		if math.IsNaN(x) {
			continue
		}
		eq *= 1 + x
		if eq > peak {
			peak = eq
		}
		if d := 1 - eq/peak; d > dd {
			dd = d
		}
	}
	return dd
}

func beta(r, b []float64) float64 {
	var xs, ys []float64
	for i := range r {
		if i < len(b) && !math.IsNaN(r[i]) && !math.IsNaN(b[i]) {
			xs, ys = append(xs, b[i]), append(ys, r[i])
		}
	}
	if len(xs) < 20 {
		return 0
	}
	mx, sx := meanStd(xs)
	my, _ := meanStd(ys)
	if sx == 0 {
		return 0
	}
	cov := 0.0
	for i := range xs {
		cov += (xs[i] - mx) * (ys[i] - my)
	}
	cov /= float64(len(xs) - 1)
	return cov / (sx * sx)
}

func correlation(a, b []float64) float64 {
	var xs, ys []float64
	for i := range a {
		if i < len(b) && !math.IsNaN(a[i]) && !math.IsNaN(b[i]) {
			xs, ys = append(xs, a[i]), append(ys, b[i])
		}
	}
	if len(xs) < 20 {
		return math.NaN()
	}
	mx, sx := meanStd(xs)
	my, sy := meanStd(ys)
	if sx == 0 || sy == 0 {
		return math.NaN()
	}
	cov := 0.0
	for i := range xs {
		cov += (xs[i] - mx) * (ys[i] - my)
	}
	cov /= float64(len(xs) - 1)
	return cov / (sx * sy)
}

func indexAtOrAfter(dates []time.Time, t time.Time) int {
	for i, d := range dates {
		if !d.Before(t) {
			return i
		}
	}
	return -1
}

func indexAtOrBefore(dates []time.Time, t time.Time) int {
	for i := len(dates) - 1; i >= 0; i-- {
		if !dates[i].After(t) {
			return i
		}
	}
	return -1
}

// lookThrough sums, per company, the weight held directly and the weight held
// through tracked index funds. Companies under 0.5% of the book are dropped.
func lookThrough(ps []Position) []Exposure {
	by := map[domain.Ticker]*Exposure{}
	get := func(t domain.Ticker) *Exposure {
		e := by[t]
		if e == nil {
			e = &Exposure{Ticker: t}
			by[t] = e
		}
		return e
	}
	for _, p := range ps {
		if p.AssetClass != domain.AssetETF {
			get(p.Ticker).Direct += p.Weight
			continue
		}
		for _, fw := range FundHoldings(p.Ticker) {
			e := get(fw.Ticker)
			e.ViaFunds += p.Weight * fw.Weight
			e.Funds = append(e.Funds, p.Ticker)
		}
	}
	out := make([]Exposure, 0, len(by))
	for _, e := range by {
		e.Total = e.Direct + e.ViaFunds
		if e.Total >= 0.005 {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Ticker < out[j].Ticker
	})
	return out
}

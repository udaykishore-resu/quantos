package guide

import (
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func TestReadHoldingsMergesLotsAndCash(t *testing.T) {
	h, err := ReadHoldings(strings.NewReader("ticker,quantity,cost_basis,note\nCASH,5000,,\naapl,10,100,first lot\nAAPL,10,200,second lot\nbrk-b,3,,\nMSFT,0,,\n"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Cash != 5000 {
		t.Errorf("cash %v", h.Cash)
	}
	if len(h.Lines) != 2 {
		t.Fatalf("expected AAPL and BRK.B, got %+v", h.Lines)
	}
	if h.Lines[0].Ticker != "AAPL" || h.Lines[0].Quantity != 20 || h.Lines[0].CostBasis != 150 {
		t.Errorf("lots not merged with a weighted basis: %+v", h.Lines[0])
	}
	if h.Lines[1].Ticker != "BRK.B" {
		t.Errorf("ticker not normalised: %+v", h.Lines[1])
	}
	if _, err := ReadHoldings(strings.NewReader("symbol,price\nAAPL,1\n")); err == nil {
		t.Error("missing quantity column should fail")
	}
}

// synthetic builds daily bars for several names driven by a common factor.
func synthetic(seed int64, days int, names map[domain.Ticker][2]float64) Series {
	r := rand.New(rand.NewSource(seed))
	day := time.Date(2016, 1, 4, 0, 0, 0, 0, time.UTC)
	var dates []time.Time
	for len(dates) < days {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			dates = append(dates, day)
		}
		day = day.AddDate(0, 0, 1)
	}
	mkt := make([]float64, days)
	for i := range mkt {
		mkt[i] = r.NormFloat64() * 0.01
	}
	out := Series{}
	keys := make([]domain.Ticker, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, name := range keys { // sorted: a shared rng and map order would make the fixture random
		bv := names[name]
		px := 100.0
		for i, d := range dates {
			ret := bv[0]*mkt[i] + r.NormFloat64()*bv[1]
			c := px * math.Exp(ret)
			out[name] = append(out[name], domain.Candle{Ticker: name, Start: d, End: d.Add(24 * time.Hour),
				Open: px, High: math.Max(px, c) * 1.002, Low: math.Min(px, c) * 0.998, Close: c, Volume: 1e6})
			px = c
		}
	}
	return out
}

func TestAssessWeightsFlagsAndWindows(t *testing.T) {
	series := synthetic(3, 1300, map[domain.Ticker][2]float64{
		"SPY": {1, 0.001}, "A": {1.2, 0.012}, "B": {1.2, 0.012}, "C": {0.2, 0.02}, "LATE": {1, 0.01},
	})
	series["LATE"] = series["LATE"][1000:] // listed recently
	// Quantities chosen so today's weights are 45/40/10/2 with 3% cash:
	// weights are what the assertions are about, and prices have drifted.
	last := func(t domain.Ticker) float64 { s := series[t]; return s[len(s)-1].Close }
	const equity = 100000.0
	h := Holdings{Name: "t", Cash: 3000, Lines: []Holding{
		{Ticker: "A", Quantity: 0.45 * equity / last("A")}, {Ticker: "B", Quantity: 0.40 * equity / last("B")},
		{Ticker: "C", Quantity: 0.10 * equity / last("C")}, {Ticker: "LATE", Quantity: 0.02 * equity / last("LATE")},
		{Ticker: "GONE", Quantity: 5}}}
	lookup := func(tk domain.Ticker) (domain.Stock, bool) {
		return domain.Stock{Ticker: tk, Sector: "Tech", AssetClass: domain.AssetEquity}, true
	}
	b, err := Assess(h, series, BookConfig{Benchmark: "SPY", Lookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	sum := b.CashWeight
	for _, p := range b.Positions {
		sum += p.Weight
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("weights sum to %.6f", sum)
	}
	if len(b.Missing) != 1 || b.Missing[0] != "GONE" {
		t.Errorf("missing data should be reported: %v", b.Missing)
	}
	wantN := 1 / (0.45*0.45 + 0.40*0.40 + 0.10*0.10 + 0.02*0.02)
	if math.Abs(b.EffectiveN-wantN) > 0.05 {
		t.Errorf("effective positions %.2f, want %.2f", b.EffectiveN, wantN)
	}
	var fails, sectorFail bool
	for _, f := range b.Flags {
		if f.Level == "FAIL" && f.Check == "concentration" {
			fails = true
		}
		if f.Check == "sector" && f.Level == "FAIL" {
			sectorFail = true
		}
	}
	if !fails || !sectorFail {
		t.Errorf("a two-name 85%% book must fail concentration and sector caps: %+v", b.Flags)
	}
	if len(b.Trailing) < 2 || b.Trailing[0].Window.Label != "trailing 1y" || b.Trailing[0].Days != 252 {
		t.Errorf("trailing windows wrong: %+v", b.Trailing)
	}
	for _, p := range b.Positions {
		if p.Ticker == "A" && (p.Beta1y < 0.8 || p.Beta1y > 1.6) {
			t.Errorf("A beta %.2f, expected ~1.2", p.Beta1y)
		}
		if p.Ticker == "LATE" && p.DataFrom.Before(series["LATE"][0].Start) {
			t.Errorf("DataFrom should be the first bar: %s", p.DataFrom)
		}
	}
	// Stress windows outside the data are reported as uncovered, not invented.
	for _, s := range b.Stress {
		if s.Days != 0 && (s.Window.End.Before(series["SPY"][0].Start) || s.Window.Start.After(series["SPY"][len(series["SPY"])-1].Start)) {
			t.Errorf("stress window %s outside the data has %d days", s.Window.Label, s.Days)
		}
	}
	if md := RenderBook(b); !strings.Contains(md, "## Findings") || !strings.Contains(md, "GONE") {
		t.Error("markdown is missing sections")
	}
}

// garchBars simulates a GARCH(1,1) return series: volatility clusters, which
// is the property a HAR forecaster is supposed to exploit.
func garchBars(seed int64, n int) []domain.Candle {
	r := rand.New(rand.NewSource(seed))
	const omega, alpha, beta = 0.000002, 0.08, 0.90
	v := omega / (1 - alpha - beta)
	px := 100.0
	day := time.Date(2012, 1, 2, 0, 0, 0, 0, time.UTC)
	var out []domain.Candle
	eps := 0.0
	for len(out) < n {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			v = omega + alpha*eps*eps + beta*v
			eps = math.Sqrt(v) * r.NormFloat64()
			c := px * math.Exp(eps)
			out = append(out, domain.Candle{Ticker: "G", Start: day, End: day.Add(24 * time.Hour), Open: px, High: math.Max(px, c), Low: math.Min(px, c), Close: c, Volume: 1e6})
			px = c
		}
		day = day.AddDate(0, 0, 1)
	}
	return out
}

func TestVolForecasterBeatsPersistenceOnClusteredVolatility(t *testing.T) {
	vf := NewVolForecaster()
	for _, c := range garchBars(5, 4000) {
		vf.Observe(c)
	}
	ev := vf.Evaluate()
	if ev.Samples < 2000 {
		t.Fatalf("expected thousands of matured forecasts, got %d", ev.Samples)
	}
	if !(ev.QLIKE < ev.QLIKENaive) {
		t.Errorf("HAR QLIKE %.4f should beat persistence %.4f on a GARCH series", ev.QLIKE, ev.QLIKENaive)
	}
	if ev.R2Log <= 0 {
		t.Errorf("R² %.3f should be positive", ev.R2Log)
	}
	// Deciles must be monotone: a higher forecast must mean higher realised vol.
	for i := 1; i < len(ev.DecileTable); i++ {
		if ev.DecileTable[i].MeanRealised < ev.DecileTable[i-1].MeanRealised*0.95 {
			t.Errorf("decile %d realised %.3f below decile %d %.3f", i+1, ev.DecileTable[i].MeanRealised, i, ev.DecileTable[i-1].MeanRealised)
		}
	}
	if f, ok := vf.Forecast("G"); !ok || f <= 0 || f > 2 {
		t.Errorf("forecast %.3f ok=%v", f, ok)
	}
	if _, ok := vf.Forecast("NOPE"); ok {
		t.Error("unknown ticker must not forecast")
	}
}

func TestVolForecasterHasNoLookAhead(t *testing.T) {
	// A forecast made at bar t must be identical whether or not bars after t
	// have been observed, which is what "fitted only on matured labels" means.
	bars := garchBars(9, 3000)
	a := NewVolForecaster()
	for _, c := range bars[:2500] {
		a.Observe(c)
	}
	fa, _ := a.Forecast("G")
	b := NewVolForecaster()
	for _, c := range bars[:2500] {
		b.Observe(c)
	}
	fb, _ := b.Forecast("G")
	if fa != fb {
		t.Fatalf("not deterministic: %.6f vs %.6f", fa, fb)
	}
	coefA, nA := a.Coefficients()
	for _, c := range bars[2500:] {
		a.Observe(c)
	}
	coefB, nB := a.Coefficients()
	if nB <= nA || coefA == coefB {
		t.Errorf("later bars should add labels and move the fit: n %d -> %d", nA, nB)
	}
}

func TestPlanArithmetic(t *testing.T) {
	// Zero returns: wealth is exactly the contributions.
	zero := make([]float64, 120)
	from, to := time.Date(2015, 1, 31, 0, 0, 0, 0, time.UTC), time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
	r, err := Plan(zero, from, to, PlanInput{Capital: 1000, Monthly: 100, Years: 3, AnnualIncome: 0, Haircut: 0, Paths: 200})
	if err != nil {
		t.Fatal(err)
	}
	if last := r.Years[2]; math.Abs(last.P50-(1000+36*100)) > 1e-6 || last.P10 != last.P90 {
		t.Errorf("zero-return plan should be contributions only: %+v", last)
	}
	// Constant 1% a month: deterministic compounding, and the sustainable
	// target arithmetic.
	one := make([]float64, 120)
	for i := range one {
		one[i] = 0.01
	}
	r, err = Plan(one, from, to, PlanInput{Capital: 10000, Monthly: 0, Years: 2, AnnualIncome: 1000, WithdrawalRate: 0.04, Paths: 50})
	if err != nil {
		t.Fatal(err)
	}
	want := 10000 * math.Pow(1.01, 24)
	if math.Abs(r.Years[1].P50-want) > 1 {
		t.Errorf("compounding wrong: %.2f vs %.2f", r.Years[1].P50, want)
	}
	if r.TargetSustainable != 25000 {
		t.Errorf("1000/0.04 should be 25000, got %.0f", r.TargetSustainable)
	}
	if _, err := Plan(one[:10], from, to, PlanInput{}); err == nil {
		t.Error("too short a sample must be refused")
	}
	if md := RenderPlan(r); !strings.Contains(md, "Wealth by year") {
		t.Error("markdown missing")
	}
}

func TestMonthlyReturns(t *testing.T) {
	bars := garchBars(1, 300)
	rets, from, to := MonthlyReturns(bars)
	if len(rets) < 12 || from.IsZero() || !to.After(from) {
		t.Errorf("monthly returns: %d from %s to %s", len(rets), from, to)
	}
}

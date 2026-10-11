package guide

import (
	"math"
	"sort"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// VolForecaster predicts next-month realised volatility with a pooled HAR
// regression (Corsi 2009) fitted online, with no look-ahead:
//
//	log RV[t+1..t+21] = b0 + b1·log RV_d(t) + b2·log RV_w(t) + b3·log RV_m(t)
//
// where RV_d is today's squared log return, RV_w the mean over the last five
// days and RV_m over the last twenty-two. One set of coefficients serves
// every instrument (pooling is standard for HAR and makes the first forecast
// available after a few hundred matured labels rather than a few years per
// name). A label matures 21 bars after its features; the regression is only
// ever fitted on matured labels, so a forecast made at t has seen nothing
// after t. Every forecast is recorded and scored when its label matures,
// which is how Evaluate can report an honest out-of-sample error.
//
// Why volatility and not direction: the previous question this platform asked
// of real data — which way will the price go — came back with a measured
// negative. Volatility clusters, and a forecast of it is both well supported
// and immediately useful, because position size is the one lever that
// improves every strategy at once.
type VolForecaster struct {
	hist map[domain.Ticker]*volSeries
	xtx  [4][4]float64
	xty  [4]float64
	yty  float64
	n    int
	coef [4]float64
	// resid is the residual variance of the log-variance regression. A
	// forecast made in log space is a geometric mean; exp(l + resid/2) is the
	// arithmetic one, which is what a variance target wants.
	resid      float64
	fitted     bool
	sinceFit   int
	minLabels  int
	refitEvery int

	// Out-of-sample record.
	scored []VolOutcome
}

type volSeries struct {
	rets    []float64 // log returns
	lastAt  time.Time
	lastPx  float64
	pending []volPending // forecasts awaiting maturity, oldest first
}

type volPending struct {
	ticker domain.Ticker
	at     time.Time
	index  int // index into rets when the forecast was made
	x      [4]float64
	harLog float64
	naive  float64 // persistence forecast: RV_m
}

// VolOutcome is one matured forecast.
type VolOutcome struct {
	Ticker   domain.Ticker `json:"ticker"`
	At       time.Time     `json:"at"`
	Forecast float64       `json:"forecast_annual_vol"`
	Naive    float64       `json:"naive_annual_vol"`
	Realised float64       `json:"realised_annual_vol"`
}

const (
	volHorizon  = 21
	volFloorVar = 1e-8 // (1 bp)^2: a zero return day must not become log(0)
)

// NewVolForecaster returns a forecaster that starts predicting after 500
// matured labels and refits every 50.
func NewVolForecaster() *VolForecaster {
	return &VolForecaster{hist: map[domain.Ticker]*volSeries{}, minLabels: 500, refitEvery: 50}
}

// Observe folds in one daily bar. It is safe to call for instruments the
// caller never asks a forecast for; they still contribute labels.
func (v *VolForecaster) Observe(c domain.Candle) {
	if c.Close <= 0 {
		return
	}
	s := v.hist[c.Ticker]
	if s == nil {
		s = &volSeries{}
		v.hist[c.Ticker] = s
	}
	if s.lastPx > 0 && !c.Start.Before(s.lastAt) {
		s.rets = append(s.rets, math.Log(c.Close/s.lastPx))
	}
	s.lastPx, s.lastAt = c.Close, c.Start

	// Mature any label whose horizon has completed, for every pending
	// forecast of this ticker.
	v.mature(c.Ticker, s)

	// Build features and register a label for the current index; it
	// matures 21 returns later.
	x, ok := features(s.rets)
	if !ok {
		return
	}
	p := volPending{ticker: c.Ticker, at: c.Start, index: len(s.rets) - 1, x: x, naive: math.Exp(x[3])}
	if v.fitted {
		p.harLog = v.coef[0] + v.coef[1]*x[1] + v.coef[2]*x[2] + v.coef[3]*x[3] + v.resid/2
	} else {
		p.harLog = math.NaN()
	}
	s.pending = append(s.pending, p)
}

func (v *VolForecaster) mature(t domain.Ticker, s *volSeries) {
	kept := s.pending[:0]
	for _, p := range s.pending {
		if len(s.rets)-1 < p.index+volHorizon {
			kept = append(kept, p)
			continue
		}
		// Forward realised variance over the 21 returns after the forecast.
		sum := 0.0
		for i := p.index + 1; i <= p.index+volHorizon; i++ {
			sum += s.rets[i] * s.rets[i]
		}
		rv := math.Max(sum/volHorizon, volFloorVar)
		y := math.Log(rv)
		// Regression update.
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				v.xtx[i][j] += p.x[i] * p.x[j]
			}
			v.xty[i] += p.x[i] * y
		}
		v.yty += y * y
		v.n++
		v.sinceFit++
		if v.n >= v.minLabels && (!v.fitted || v.sinceFit >= v.refitEvery) {
			v.refit()
		}
		if !math.IsNaN(p.harLog) {
			v.scored = append(v.scored, VolOutcome{
				Ticker: p.ticker, At: p.at,
				Forecast: math.Sqrt(math.Exp(p.harLog) * 252),
				Naive:    math.Sqrt(p.naive * 252),
				Realised: math.Sqrt(rv * 252),
			})
		}
	}
	s.pending = kept
}

func features(rets []float64) ([4]float64, bool) {
	n := len(rets)
	if n < 22 {
		return [4]float64{}, false
	}
	mean2 := func(k int) float64 {
		s := 0.0
		for i := n - k; i < n; i++ {
			s += rets[i] * rets[i]
		}
		return math.Max(s/float64(k), volFloorVar)
	}
	return [4]float64{1, math.Log(mean2(1)), math.Log(mean2(5)), math.Log(mean2(22))}, true
}

// refit solves the 4x4 normal equations with a small ridge for stability.
func (v *VolForecaster) refit() {
	var a [4][4]float64
	var b [4]float64
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			a[i][j] = v.xtx[i][j]
		}
		a[i][i] += 1e-6 * float64(v.n)
		b[i] = v.xty[i]
	}
	if c, ok := solve4(a, b); ok {
		v.coef = c
		// SSE = y'y - 2 c'X'y + c'X'X c, from the sufficient statistics.
		sse := v.yty
		for i := 0; i < 4; i++ {
			sse -= 2 * c[i] * v.xty[i]
			for j := 0; j < 4; j++ {
				sse += c[i] * v.xtx[i][j] * c[j]
			}
		}
		if v.n > 5 {
			v.resid = math.Max(sse/float64(v.n-4), 0)
		}
		v.fitted = true
		v.sinceFit = 0
	}
}

func solve4(a [4][4]float64, b [4]float64) ([4]float64, bool) {
	// Gaussian elimination with partial pivoting.
	for col := 0; col < 4; col++ {
		piv := col
		for r := col + 1; r < 4; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[piv][col]) {
				piv = r
			}
		}
		if math.Abs(a[piv][col]) < 1e-12 {
			return [4]float64{}, false
		}
		a[col], a[piv] = a[piv], a[col]
		b[col], b[piv] = b[piv], b[col]
		for r := col + 1; r < 4; r++ {
			f := a[r][col] / a[col][col]
			for k := col; k < 4; k++ {
				a[r][k] -= f * a[col][k]
			}
			b[r] -= f * b[col]
		}
	}
	var x [4]float64
	for i := 3; i >= 0; i-- {
		s := b[i]
		for k := i + 1; k < 4; k++ {
			s -= a[i][k] * x[k]
		}
		x[i] = s / a[i][i]
	}
	return x, true
}

// Forecast returns the annualised volatility expected over the next 21
// trading days for an instrument, or false before the model is fitted or the
// instrument has 22 returns.
func (v *VolForecaster) Forecast(t domain.Ticker) (float64, bool) {
	s := v.hist[t]
	if s == nil || !v.fitted {
		return 0, false
	}
	x, ok := features(s.rets)
	if !ok {
		return 0, false
	}
	l := v.coef[0] + v.coef[1]*x[1] + v.coef[2]*x[2] + v.coef[3]*x[3] + v.resid/2
	return math.Sqrt(math.Exp(l) * 252), true
}

// Coefficients returns the current HAR coefficients (intercept, daily,
// weekly, monthly) and the number of labels they were fitted on.
func (v *VolForecaster) Coefficients() ([4]float64, int) { return v.coef, v.n }

// Scored returns every matured out-of-sample forecast.
func (v *VolForecaster) Scored() []VolOutcome { return append([]VolOutcome(nil), v.scored...) }

// --- evaluation ---------------------------------------------------------------

// VolEvaluation summarises forecast quality out of sample. QLIKE is the
// standard loss for variance forecasts (robust to noise in the realised
// proxy); R² is on log variance; both are reported against the persistence
// benchmark so the reader can see what the regression adds.
type VolEvaluation struct {
	Samples      int         `json:"samples"`
	From         time.Time   `json:"from"`
	To           time.Time   `json:"to"`
	R2Log        float64     `json:"r2_log"`
	R2LogNaive   float64     `json:"r2_log_naive"`
	QLIKE        float64     `json:"qlike"`
	QLIKENaive   float64     `json:"qlike_naive"`
	MAEVol       float64     `json:"mae_vol_points"`
	MAEVolNaive  float64     `json:"mae_vol_points_naive"`
	Bias         float64     `json:"bias_vol_points"`
	DecileTable  []VolDecile `json:"deciles"`
	ByYear       []VolYear   `json:"by_year"`
	Coefficients [4]float64  `json:"coefficients"`
	LabelsFitted int         `json:"labels_fitted"`
	Verdict      string      `json:"verdict"`
}

// VolDecile is one row of the forecast-vs-realised reliability table.
type VolDecile struct {
	Decile       int     `json:"decile"`
	MeanForecast float64 `json:"mean_forecast"`
	MeanRealised float64 `json:"mean_realised"`
	Count        int     `json:"count"`
}

// VolYear is forecast quality in one calendar year.
type VolYear struct {
	Year       int     `json:"year"`
	Samples    int     `json:"samples"`
	R2Log      float64 `json:"r2_log"`
	QLIKE      float64 `json:"qlike"`
	QLIKENaive float64 `json:"qlike_naive"`
}

// Evaluate scores every matured forecast.
func (v *VolForecaster) Evaluate() VolEvaluation {
	ev := VolEvaluation{Coefficients: v.coef, LabelsFitted: v.n}
	s := v.scored
	if len(s) == 0 {
		ev.Verdict = "no matured out-of-sample forecasts yet"
		return ev
	}
	sort.Slice(s, func(i, j int) bool { return s[i].At.Before(s[j].At) })
	ev.Samples = len(s)
	ev.From, ev.To = s[0].At, s[len(s)-1].At

	ev.R2Log, ev.QLIKE, ev.MAEVol, ev.Bias = volScores(s, func(o VolOutcome) float64 { return o.Forecast })
	ev.R2LogNaive, ev.QLIKENaive, ev.MAEVolNaive, _ = volScores(s, func(o VolOutcome) float64 { return o.Naive })

	// Deciles of forecast.
	byF := append([]VolOutcome(nil), s...)
	sort.Slice(byF, func(i, j int) bool { return byF[i].Forecast < byF[j].Forecast })
	for d := 0; d < 10; d++ {
		lo, hi := d*len(byF)/10, (d+1)*len(byF)/10
		if hi <= lo {
			continue
		}
		var f, r float64
		for _, o := range byF[lo:hi] {
			f += o.Forecast
			r += o.Realised
		}
		ev.DecileTable = append(ev.DecileTable, VolDecile{Decile: d + 1, MeanForecast: f / float64(hi-lo), MeanRealised: r / float64(hi-lo), Count: hi - lo})
	}
	// By year.
	years := map[int][]VolOutcome{}
	for _, o := range s {
		years[o.At.Year()] = append(years[o.At.Year()], o)
	}
	ys := make([]int, 0, len(years))
	for y := range years {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	for _, y := range ys {
		r2, q, _, _ := volScores(years[y], func(o VolOutcome) float64 { return o.Forecast })
		_, qn, _, _ := volScores(years[y], func(o VolOutcome) float64 { return o.Naive })
		ev.ByYear = append(ev.ByYear, VolYear{Year: y, Samples: len(years[y]), R2Log: r2, QLIKE: q, QLIKENaive: qn})
	}
	switch {
	case ev.R2Log > 0.3 && ev.QLIKE < ev.QLIKENaive:
		ev.Verdict = "volatility is forecastable here: the regression explains a substantial share of next-month variance and beats persistence on QLIKE"
	case ev.QLIKE < ev.QLIKENaive:
		ev.Verdict = "the regression beats persistence, modestly"
	default:
		ev.Verdict = "the regression does not beat last month's volatility as a forecast; use persistence"
	}
	return ev
}

func volScores(s []VolOutcome, pick func(VolOutcome) float64) (r2, qlike, mae, bias float64) {
	if len(s) == 0 {
		return
	}
	var ssRes, ssTot, meanY, q, m, b float64
	ys := make([]float64, len(s))
	for i, o := range s {
		ys[i] = math.Log(math.Max(o.Realised*o.Realised/252, volFloorVar))
		meanY += ys[i]
	}
	meanY /= float64(len(s))
	for i, o := range s {
		f := pick(o)
		fv := math.Max(f*f/252, volFloorVar)
		rv := math.Max(o.Realised*o.Realised/252, volFloorVar)
		e := ys[i] - math.Log(fv)
		ssRes += e * e
		ssTot += (ys[i] - meanY) * (ys[i] - meanY)
		q += rv/fv - math.Log(rv/fv) - 1
		m += math.Abs(o.Realised-f) * 100
		b += (f - o.Realised) * 100
	}
	n := float64(len(s))
	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}
	return r2, q / n, m / n, b / n
}

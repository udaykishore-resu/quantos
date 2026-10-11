package guide

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// The brief is one page a day: what the market did, what your book looks
// like against the caps, what the one forecast the platform makes says about
// each holding, what the ranking sleeve would hold, and — the part nobody
// else ships — how every previous brief's measurable statements turned out.
//
// A statement is a "call" only if it can be scored: a volatility forecast
// matures 21 sessions later against realised volatility; a sleeve slice
// matures a month later against the benchmark. Risk findings are not calls,
// because "NVDA is 7% of the book" is a fact, not a prediction. Calls are
// appended to a ledger when the brief runs and scored when the data allows,
// so the track record is the brief's own history and cannot be edited
// without editing the file.

// Call is one scorable statement.
type Call struct {
	ID        string        `json:"id"`
	Kind      string        `json:"kind"` // vol | sleeve | book
	At        time.Time     `json:"at"`
	Ticker    domain.Ticker `json:"ticker,omitempty"`
	Tickers   []string      `json:"tickers,omitempty"`
	Value     float64       `json:"value"`           // forecast vol, or sleeve weight sum (1), or book equity
	Naive     float64       `json:"naive,omitempty"` // persistence forecast for vol
	Horizon   int           `json:"horizon_days"`
	ResolveAt time.Time     `json:"resolve_at"`
	Realised  *float64      `json:"realised,omitempty"`
	Reference *float64      `json:"reference,omitempty"` // benchmark return for sleeve calls
	ScoredAt  *time.Time    `json:"scored_at,omitempty"`
}

// Ledger is the append-only record.
type Ledger struct {
	Path  string
	Calls []Call
}

// LoadLedger reads the JSON-lines file; a missing file is an empty ledger.
func LoadLedger(path string) (*Ledger, error) {
	l := &Ledger{Path: path}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Call
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("ledger %s: %w", path, err)
		}
		l.Calls = append(l.Calls, c)
	}
	return l, sc.Err()
}

// Save rewrites the file. The ledger is small; rewriting keeps scored
// entries updated in place without a second file.
func (l *Ledger) Save() error {
	if l.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o750); err != nil {
		return err
	}
	var b strings.Builder
	for _, c := range l.Calls {
		data, err := json.Marshal(c)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return os.WriteFile(l.Path, []byte(b.String()), 0o644)
}

// Has reports whether a call of this kind exists for the date (and ticker).
func (l *Ledger) Has(kind string, at time.Time, t domain.Ticker) bool {
	for _, c := range l.Calls {
		if c.Kind == kind && c.At.Equal(at) && c.Ticker == t {
			return true
		}
	}
	return false
}

// Score resolves every matured, unscored call against the series.
func (l *Ledger) Score(series Series, benchmark domain.Ticker, now time.Time) int {
	scored := 0
	for i := range l.Calls {
		c := &l.Calls[i]
		if c.ScoredAt != nil || now.Before(c.ResolveAt) {
			continue
		}
		switch c.Kind {
		case "vol":
			if rv, ok := realisedVolAfter(series[c.Ticker], c.At, c.Horizon); ok {
				c.Realised, c.ScoredAt = &rv, &now
				scored++
			}
		case "sleeve":
			var sum float64
			n := 0
			for _, t := range c.Tickers {
				if r, ok := returnBetween(series[domain.Ticker(t)], c.At, c.ResolveAt); ok {
					sum += r
					n++
				}
			}
			b, okb := returnBetween(series[benchmark], c.At, c.ResolveAt)
			if n > 0 && okb {
				avg := sum / float64(n)
				c.Realised, c.Reference, c.ScoredAt = &avg, &b, &now
				scored++
			}
		}
	}
	return scored
}

func realisedVolAfter(bars []domain.Candle, at time.Time, horizon int) (float64, bool) {
	i := sort.Search(len(bars), func(k int) bool { return bars[k].Start.After(at) })
	if i+horizon > len(bars) || i == 0 {
		return 0, false
	}
	sum := 0.0
	for k := i; k < i+horizon; k++ {
		r := math.Log(bars[k].Close / bars[k-1].Close)
		sum += r * r
	}
	return math.Sqrt(sum / float64(horizon) * 252), true
}

func returnBetween(bars []domain.Candle, from, to time.Time) (float64, bool) {
	var a, b float64
	for _, c := range bars {
		if !c.Start.After(from) {
			a = c.Close
		}
		if !c.Start.After(to) {
			b = c.Close
		}
	}
	if a <= 0 || b <= 0 {
		return 0, false
	}
	// The call must have matured: a bar at or after the resolve date.
	if bars[len(bars)-1].Start.Before(to) {
		return 0, false
	}
	return b/a - 1, true
}

// TrackRecord summarises the scored ledger.
type TrackRecord struct {
	VolCalls      int       `json:"vol_calls_scored"`
	VolMAE        float64   `json:"vol_mae_points"`
	VolMAENaive   float64   `json:"vol_mae_points_naive"`
	VolQLIKE      float64   `json:"vol_qlike"`
	VolQLIKENaive float64   `json:"vol_qlike_naive"`
	SleeveCalls   int       `json:"sleeve_calls_scored"`
	SleeveHitRate float64   `json:"sleeve_hit_rate"`
	SleeveExcess  float64   `json:"sleeve_mean_excess"`
	Pending       int       `json:"pending"`
	Since         time.Time `json:"since"`
}

// Record computes the track record from scored calls.
func (l *Ledger) Record() TrackRecord {
	var tr TrackRecord
	var mae, maeN, q, qN float64
	var hits int
	var excess float64
	for _, c := range l.Calls {
		if tr.Since.IsZero() || c.At.Before(tr.Since) {
			tr.Since = c.At
		}
		if c.ScoredAt == nil {
			if c.Kind != "book" {
				tr.Pending++
			}
			continue
		}
		switch c.Kind {
		case "vol":
			r := *c.Realised
			tr.VolCalls++
			mae += math.Abs(r-c.Value) * 100
			maeN += math.Abs(r-c.Naive) * 100
			q += qlike(r, c.Value)
			qN += qlike(r, c.Naive)
		case "sleeve":
			tr.SleeveCalls++
			e := *c.Realised - *c.Reference
			excess += e
			if e > 0 {
				hits++
			}
		}
	}
	if tr.VolCalls > 0 {
		n := float64(tr.VolCalls)
		tr.VolMAE, tr.VolMAENaive, tr.VolQLIKE, tr.VolQLIKENaive = mae/n, maeN/n, q/n, qN/n
	}
	if tr.SleeveCalls > 0 {
		tr.SleeveHitRate = float64(hits) / float64(tr.SleeveCalls)
		tr.SleeveExcess = excess / float64(tr.SleeveCalls)
	}
	return tr
}

func qlike(realised, forecast float64) float64 {
	rv := math.Max(realised*realised/252, volFloorVar)
	fv := math.Max(forecast*forecast/252, volFloorVar)
	return rv/fv - math.Log(rv/fv) - 1
}

// --- the brief ----------------------------------------------------------------

// MarketContext is where the benchmark sits. It is context, not a signal;
// the platform measured its own regime-conditioned predictions and found
// nothing, so the brief states the facts and stops.
type MarketContext struct {
	AsOf        time.Time     `json:"as_of"`
	Benchmark   domain.Ticker `json:"benchmark"`
	Close       float64       `json:"close"`
	Return1m    float64       `json:"return_1m"`
	Return3m    float64       `json:"return_3m"`
	Return12m   float64       `json:"return_12m"`
	Above200d   float64       `json:"distance_from_200d"`
	Drawdown    float64       `json:"drawdown_from_high"`
	RealisedVol float64       `json:"realised_vol_20d"`
	ForecastVol float64       `json:"forecast_vol_21d"`
	VIX         float64       `json:"vix,omitempty"`
	DataAgeDays int           `json:"data_age_days"`
}

// HoldingOutlook is the forecast-volatility line per position.
type HoldingOutlook struct {
	Ticker       domain.Ticker `json:"ticker"`
	Weight       float64       `json:"weight"`
	Price        float64       `json:"price"`
	ForecastVol  float64       `json:"forecast_vol"`
	TrailingVol  float64       `json:"trailing_vol_22d"`
	OneSigmaMove float64       `json:"one_sigma_21d_move"` // in dollars, on the position
}

// SleeveView is what the ranking sleeve would hold, with the verdict that
// goes with it.
type SleeveView struct {
	Signal   string   `json:"signal"`
	Holdings []string `json:"holdings"`
	Verdict  string   `json:"verdict"`
}

// Brief is the page.
type Brief struct {
	Date        time.Time        `json:"date"`
	Market      MarketContext    `json:"market"`
	Book        Book             `json:"book"`
	EquityDelta *float64         `json:"equity_change_since_last,omitempty"`
	Outlook     []HoldingOutlook `json:"outlook"`
	Sleeve      *SleeveView      `json:"sleeve,omitempty"`
	Record      TrackRecord      `json:"track_record"`
	Baseline    []string         `json:"baseline_record"`
	NewCalls    int              `json:"calls_recorded"`
	Scored      int              `json:"calls_scored_today"`
	Notes       []string         `json:"notes"`
	Disclaimer  string           `json:"disclaimer"`
}

// BriefInput gathers what the page needs.
type BriefInput struct {
	Holdings  Holdings
	Series    Series
	Benchmark domain.Ticker
	AsOf      time.Time
	Caps      Caps
	Lookup    func(domain.Ticker) (domain.Stock, bool)
	Vol       *VolForecaster
	Sleeve    *SleeveView
	Ledger    *Ledger
	// Baseline lines describe the historical out-of-sample record the brief
	// starts from, so day one is not an empty track record.
	Baseline []string
	Now      time.Time
}

// Compose builds the brief, scores matured calls and records today's.
func Compose(in BriefInput) (Brief, error) {
	if in.Benchmark == "" {
		in.Benchmark = "SPY"
	}
	bench := in.Series[in.Benchmark]
	if len(bench) < 260 {
		return Brief{}, fmt.Errorf("brief: need a year of %s history", in.Benchmark)
	}
	asOf := in.AsOf
	if asOf.IsZero() {
		asOf = bench[len(bench)-1].Start
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	b := Brief{Date: asOf, Disclaimer: domain.Disclaimer, Baseline: in.Baseline}

	// Market context.
	i := sort.Search(len(bench), func(k int) bool { return bench[k].Start.After(asOf) }) - 1
	if i < 250 {
		return Brief{}, fmt.Errorf("brief: not enough %s history before %s", in.Benchmark, asOf.Format("2006-01-02"))
	}
	px := func(k int) float64 { return bench[k].Close }
	mc := MarketContext{AsOf: bench[i].Start, Benchmark: in.Benchmark, Close: px(i),
		Return1m: px(i)/px(i-21) - 1, Return3m: px(i)/px(i-63) - 1, Return12m: px(i)/px(i-252) - 1,
		DataAgeDays: int(now.Sub(bench[i].Start).Hours() / 24)}
	ma, hi := 0.0, 0.0
	for k := i - 199; k <= i; k++ {
		ma += px(k)
	}
	ma /= 200
	mc.Above200d = px(i)/ma - 1
	for k := 0; k <= i; k++ {
		hi = math.Max(hi, px(k))
	}
	mc.Drawdown = 1 - px(i)/hi
	var rets []float64
	for k := i - 19; k <= i; k++ {
		rets = append(rets, math.Log(px(k)/px(k-1)))
	}
	_, sd := meanStd(rets)
	mc.RealisedVol = sd * math.Sqrt(252)
	if in.Vol != nil {
		if f, ok := in.Vol.Forecast(in.Benchmark); ok {
			mc.ForecastVol = f
		}
	}
	if vix := in.Series["VIX"]; len(vix) > 0 {
		for _, c := range vix {
			if !c.Start.After(asOf) {
				mc.VIX = c.Close
			}
		}
	}
	b.Market = mc

	// Book.
	book, err := Assess(in.Holdings, in.Series, BookConfig{Caps: in.Caps, Benchmark: in.Benchmark, AsOf: asOf, Lookup: in.Lookup})
	if err != nil {
		return Brief{}, err
	}
	b.Book = book
	if in.Ledger != nil {
		var last *Call
		for k := range in.Ledger.Calls {
			c := &in.Ledger.Calls[k]
			if c.Kind == "book" && c.At.Before(asOf) && (last == nil || c.At.After(last.At)) {
				last = c
			}
		}
		if last != nil && last.Value > 0 {
			d := book.Equity/last.Value - 1
			b.EquityDelta = &d
		}
	}

	// Outlook per holding.
	for _, p := range book.Positions {
		o := HoldingOutlook{Ticker: p.Ticker, Weight: p.Weight, Price: p.Price}
		if s := in.Series[p.Ticker]; len(s) > 23 {
			j := sort.Search(len(s), func(k int) bool { return s[k].Start.After(asOf) }) - 1
			if j >= 22 {
				sum := 0.0
				for k := j - 21; k <= j; k++ {
					r := math.Log(s[k].Close / s[k-1].Close)
					sum += r * r
				}
				o.TrailingVol = math.Sqrt(sum / 22 * 252)
			}
		}
		if in.Vol != nil {
			if f, ok := in.Vol.Forecast(p.Ticker); ok {
				o.ForecastVol = f
				o.OneSigmaMove = p.Value * f / math.Sqrt(12)
			}
		}
		b.Outlook = append(b.Outlook, o)
	}
	sort.Slice(b.Outlook, func(i, j int) bool { return b.Outlook[i].Weight > b.Outlook[j].Weight })
	b.Sleeve = in.Sleeve

	// Ledger: score, then record.
	if in.Ledger != nil {
		b.Scored = in.Ledger.Score(in.Series, in.Benchmark, asOf)
		if !in.Ledger.Has("book", asOf, "") {
			in.Ledger.Calls = append(in.Ledger.Calls, Call{ID: callID("book", asOf, ""), Kind: "book", At: asOf, Value: book.Equity})
		}
		for _, o := range b.Outlook {
			if o.ForecastVol > 0 && !in.Ledger.Has("vol", asOf, o.Ticker) {
				in.Ledger.Calls = append(in.Ledger.Calls, Call{ID: callID("vol", asOf, o.Ticker), Kind: "vol", At: asOf, Ticker: o.Ticker,
					Value: o.ForecastVol, Naive: o.TrailingVol, Horizon: 21, ResolveAt: addTradingDays(bench, asOf, 21)})
				b.NewCalls++
			}
		}
		if in.Sleeve != nil && len(in.Sleeve.Holdings) > 0 && !in.Ledger.Has("sleeve", asOf, "") {
			in.Ledger.Calls = append(in.Ledger.Calls, Call{ID: callID("sleeve", asOf, ""), Kind: "sleeve", At: asOf, Tickers: in.Sleeve.Holdings,
				Value: 1, Horizon: 21, ResolveAt: addTradingDays(bench, asOf, 21)})
			b.NewCalls++
		}
		b.Record = in.Ledger.Record()
	}

	b.Notes = append(b.Notes,
		"market context is description, not a signal: the platform's regime-conditioned predictions were measured on real data and found no edge",
		"a one-sigma move is the position's value times forecast volatility over 21 sessions; two-sigma happens about one month in twenty",
		"calls are scored against the data when they mature; the ledger is the brief's own history",
	)
	if mc.DataAgeDays > 5 {
		b.Notes = append(b.Notes, fmt.Sprintf("the last bar is %d days old; run `quantos ingest` before trusting today's figures", mc.DataAgeDays))
	}
	return b, nil
}

func callID(kind string, at time.Time, t domain.Ticker) string {
	return fmt.Sprintf("%s:%s:%s", kind, at.Format("2006-01-02"), t)
}

func addTradingDays(bench []domain.Candle, from time.Time, n int) time.Time {
	i := sort.Search(len(bench), func(k int) bool { return bench[k].Start.After(from) })
	if i+n-1 < len(bench) {
		return bench[i+n-1].Start
	}
	// Beyond the data: approximate with calendar days.
	return from.AddDate(0, 0, int(float64(n)*7/5)+1)
}

// RenderBrief writes the page.
func RenderBrief(b Brief) string {
	var sb strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&sb, f, a...) }
	pct := func(v float64) string { return fmt.Sprintf("%+.1f%%", v*100) }
	m := b.Market
	w("# Brief — %s\n\n> %s\n\n", b.Date.Format("Monday 2 January 2006"), b.Disclaimer)

	w("## Market\n\n%s %.2f: %s over a month, %s over three, %s over twelve; %s from its 200-day average, %.1f%% below its high. Realised vol %.0f%%",
		m.Benchmark, m.Close, pct(m.Return1m), pct(m.Return3m), pct(m.Return12m), pct(m.Above200d), m.Drawdown*100, m.RealisedVol*100)
	if m.ForecastVol > 0 {
		w(", forecast %.0f%% for the next month", m.ForecastVol*100)
	}
	if m.VIX > 0 {
		w("; VIX %.1f", m.VIX)
	}
	w(".\n\n")

	w("## Your book\n\nEquity $%s", money(b.Book.Equity))
	if b.EquityDelta != nil {
		w(" (%s since the last brief)", pct(*b.EquityDelta))
	}
	w(", %d positions, effective %.1f, cash %.0f%%.\n\n", len(b.Book.Positions), b.Book.EffectiveN, b.Book.CashWeight*100)
	if len(b.Book.Flags) == 0 {
		w("Nothing exceeds the caps.\n\n")
	}
	for _, f := range b.Book.Flags {
		w("- **%s** (%s): %s\n", f.Level, f.Check, f.Detail)
	}
	w("\n| position | weight | forecast vol | trailing 22d | one-sigma month |\n|---|---|---|---|---|\n")
	for _, o := range b.Outlook {
		w("| %s | %.1f%% | %.0f%% | %.0f%% | ±$%s |\n", o.Ticker, o.Weight*100, o.ForecastVol*100, o.TrailingVol*100, money(o.OneSigmaMove))
	}
	w("\n")

	if b.Sleeve != nil {
		w("## The ranking sleeve\n\n%s would hold: %s.\n\n%s\n\n", b.Sleeve.Signal, strings.Join(b.Sleeve.Holdings, ", "), b.Sleeve.Verdict)
	}

	w("## Track record\n\n")
	r := b.Record
	if r.VolCalls == 0 && r.SleeveCalls == 0 {
		w("No calls from earlier briefs have matured yet")
		if r.Pending > 0 {
			w(" (%d pending)", r.Pending)
		}
		w(". The baseline below is the historical out-of-sample record of the same methods.\n\n")
	} else {
		if r.VolCalls > 0 {
			w("- Volatility: %d calls scored since %s; MAE %.1f vol points against %.1f for last month's vol, QLIKE %.3f against %.3f.\n",
				r.VolCalls, r.Since.Format("2006-01-02"), r.VolMAE, r.VolMAENaive, r.VolQLIKE, r.VolQLIKENaive)
		}
		if r.SleeveCalls > 0 {
			w("- Sleeve: %d months scored; beat %s in %.0f%% of them, mean excess %s a month.\n", r.SleeveCalls, m.Benchmark, r.SleeveHitRate*100, pct(r.SleeveExcess))
		}
		if r.Pending > 0 {
			w("- %d calls pending.\n", r.Pending)
		}
		w("\n")
	}
	if len(b.Baseline) > 0 {
		w("Baseline, from the research reports:\n\n")
		for _, line := range b.Baseline {
			w("- %s\n", line)
		}
		w("\n")
	}
	w("Recorded today: %d calls; scored today: %d.\n\n", b.NewCalls, b.Scored)

	w("## Notes\n\n")
	for _, n := range b.Notes {
		w("- %s\n", n)
	}
	w("\n_Generated %s._\n", time.Now().UTC().Format(time.RFC3339))
	return sb.String()
}

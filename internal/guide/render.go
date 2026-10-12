package guide

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RenderBook writes the assessment as Markdown.
func RenderBook(b Book) string {
	var sb strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&sb, f, a...) }
	pct := func(v float64) string { return fmt.Sprintf("%+.1f%%", v*100) }
	upct := func(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

	w("# Book: %s\n\nMarked %s. Equity $%s, cash %s, %d positions, benchmark %s.\n\n",
		b.Name, b.AsOf.Format("2006-01-02"), money(b.Equity), upct(b.CashWeight), len(b.Positions), b.Benchmark)
	w("> %s\n\n", b.Disclaimer)

	w("## Findings\n\n")
	if len(b.Flags) == 0 {
		w("Nothing exceeds the configured caps.\n\n")
	}
	for _, f := range b.Flags {
		w("- **%s** (%s): %s\n", f.Level, f.Check, f.Detail)
	}
	w("\n")

	w("## Positions\n\n| ticker | weight | value | price | 1y return | 1y vol | 1y beta | 1y max DD | moves with | correlated exposure |\n|---|---|---|---|---|---|---|---|---|---|\n")
	ps := append([]Position(nil), b.Positions...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Weight > ps[j].Weight })
	for _, p := range ps {
		w("| %s | %s | $%s | %.2f | %s | %s | %.2f | %s | %s | %s |\n", p.Ticker, upct(p.Weight), money(p.Value), p.Price,
			pct(p.Return1y), upct(p.Vol1y), p.Beta1y, upct(p.MaxDrawdown1y), joinTickers(p.CorrelatedWith), upct(p.CorrelatedExposure))
	}
	if b.Cash > 0 {
		w("| CASH | %s | $%s | | | | | | | |\n", upct(b.CashWeight), money(b.Cash))
	}
	w("\nEffective number of positions %.1f (1/HHI). ETFs %s, single names %s.\n\n", b.EffectiveN, upct(b.ETFWeight), upct(b.SingleNames))

	if len(b.Exposures) > 0 {
		w("## Company exposure, looking through index funds\n\nConstituent weights %s.\n\n| company | total | held directly | through funds | funds |\n|---|---|---|---|---|\n", LookThroughAsOf)
		for i, e := range b.Exposures {
			if i == 10 {
				break
			}
			w("| %s | %s | %s | %s | %s |\n", e.Ticker, upct(e.Total), upct(e.Direct), upct(e.ViaFunds), joinTickers(e.Funds))
		}
		w("\n")
	}

	if len(b.SectorExposure) > 0 {
		w("## Single-name sector exposure\n\n| sector | weight |\n|---|---|\n")
		names := make([]string, 0, len(b.SectorExposure))
		for s := range b.SectorExposure {
			names = append(names, s)
		}
		sort.Slice(names, func(i, j int) bool { return b.SectorExposure[names[i]] > b.SectorExposure[names[j]] })
		for _, s := range names {
			w("| %s | %s |\n", s, upct(b.SectorExposure[s]))
		}
		w("\n")
	}

	if len(b.TopPairs) > 0 {
		w("## Most correlated pairs (1y daily)\n\n| pair | correlation | joint weight |\n|---|---|---|\n")
		for _, p := range b.TopPairs {
			w("| %s / %s | %.2f | %s |\n", p.A, p.B, p.Correlation, upct(p.JointWeight))
		}
		w("\n")
	}

	w("## At today's weights, historically\n\n| window | days | return | annualised | vol | Sharpe | max DD | beta | %s return | %s max DD |\n|---|---|---|---|---|---|---|---|---|---|\n", b.Benchmark, b.Benchmark)
	for _, t := range b.Trailing {
		w("| %s | %d | %s | %s | %s | %.2f | %s | %.2f | %s | %s |\n", t.Window.Label, t.Days, pct(t.Return), pct(t.AnnualReturn),
			upct(t.Volatility), t.Sharpe, upct(t.MaxDrawdown), t.Beta, pct(t.BenchmarkReturn), upct(t.BenchmarkMaxDD))
	}
	w("\n## Stress episodes\n\n| episode | window | book | %s | book max DD |\n|---|---|---|---|---|\n", b.Benchmark)
	for _, s := range b.Stress {
		if s.Days == 0 {
			w("| %s | %s .. %s | no data | | |\n", s.Window.Label, s.Window.Start.Format("2006-01-02"), s.Window.End.Format("2006-01-02"))
			continue
		}
		w("| %s | %s .. %s | %s | %s | %s |\n", s.Window.Label, s.Window.Start.Format("2006-01-02"), s.Window.End.Format("2006-01-02"),
			pct(s.Return), pct(s.BenchmarkReturn), upct(s.MaxDrawdown))
	}
	w("\n## Notes\n\n")
	for _, n := range b.Notes {
		w("- %s\n", n)
	}
	w("\n_Generated %s._\n", time.Now().UTC().Format(time.RFC3339))
	return sb.String()
}

// Money formats a dollar amount with thousands separators and no cents.
func Money(v float64) string { return money(v) }

func money(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.0f", v)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// RenderPlan writes the plan as Markdown.
func RenderPlan(r PlanResult) string {
	var sb strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&sb, f, a...) }
	in := r.Input
	w("# Plan: $%s now, $%s a month, %d years\n\n", money(in.Capital), money(in.Monthly), in.Years)
	w("> %s\n\n", r.Disclaimer)
	w("**%s**\n\n", r.Verdict)
	w("Returns bootstrapped from %s, %s to %s: %.1f%% a year, %.1f%% volatility, worst twelve months %.1f%%; %.1f%% used after a %.1f%% haircut.\n\n",
		in.Benchmark, r.SampleFrom.Format("2006-01"), r.SampleTo.Format("2006-01"), r.SampleAnnualReturn*100, r.SampleAnnualVol*100, r.SampleWorstYear*100, r.UsedAnnualReturn*100, in.Haircut*100)
	if r.TargetSustainable > 0 {
		w("| target | capital needed |\n|---|---|\n| $%s a year sustained at %.0f%% withdrawal | $%s |\n| $%s a year from the expected return alone | $%s |\n\n",
			money(in.AnnualIncome), in.WithdrawalRate*100, money(r.TargetSustainable), money(in.AnnualIncome), money(r.TargetFromReturns))
	}
	w("## Wealth by year\n\n| year | contributed | P10 | P25 | median | P75 | P90 | fixed %.1f%% | P(sustainable) | P(from returns) |\n|---|---|---|---|---|---|---|---|---|---|\n", r.UsedAnnualReturn*100)
	for _, y := range r.Years {
		w("| %d | $%s | $%s | $%s | $%s | $%s | $%s | $%s | %.0f%% | %.0f%% |\n", y.Year, money(y.Contributed), money(y.P10), money(y.P25), money(y.P50), money(y.P75), money(y.P90), money(y.Fixed), y.ProbSustainable*100, y.ProbFromReturns*100)
	}
	w("\n## Notes\n\n")
	for _, n := range r.Notes {
		w("- %s\n", n)
	}
	w("\n_Generated %s._\n", time.Now().UTC().Format(time.RFC3339))
	return sb.String()
}

package backtest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// RenderMarkdown writes the report as a document a person can read without
// the JSON. Numbers are shown at the precision they deserve and nothing is
// rounded in the strategy's favour.
func RenderMarkdown(rep Report) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# Walk-forward report — %s on %s bars\n\n", rep.StrategyID, rep.Interval)
	w("Out-of-sample windows %s .. %s, %d folds, %d instruments.\n\n",
		rep.Start.Format("2006-01-02"), rep.End.Format("2006-01-02"), len(rep.Folds), len(rep.Universe))
	w("> %s\n\n", rep.Disclaimer)

	w("## Verdict\n\n**%s**\n\n", rep.Verdict.Summary)
	w("| check | result |\n|---|---|\n")
	w("| survives stressed costs | %s |\n", yesNo(rep.Verdict.SurvivesCosts))
	w("| beats buy-and-hold %s | %s |\n", rep.Benchmark.Ticker, yesNo(rep.Verdict.BeatsBenchmark))
	w("| predictions beat base rate | %s |\n", yesNo(rep.Verdict.PredictiveLift))
	w("| consistent across folds | %s |\n", yesNo(rep.Verdict.Consistent))
	w("| enough samples | %s |\n\n", yesNo(rep.Verdict.EnoughSamples))
	for _, r := range rep.Verdict.Reasons {
		w("- %s\n", r)
	}
	w("\n")

	w("## Paper trading, net of costs\n\n")
	w("| cost multiplier | trades | total return | CAGR | Sharpe | deflated Sharpe | Sortino | max DD | volatility | win rate | profit factor | turnover | total costs | folds positive |\n")
	w("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range rep.Costs {
		m := c.WalkForward.Aggregate
		ds := "—"
		if rep.Trials > 1 {
			ds = fmt.Sprintf("%.2f", DeflatedSharpe(m.Sharpe, rep.Trials, dailySamples(c.WalkForward)))
		}
		w("| x%.1f | %d | %+.2f%% | %+.2f%% | %.2f | %s | %.2f | %.2f%% | %.2f%% | %.1f%% | %.2f | %.2fx | $%.0f | %.0f%% |\n",
			c.Multiplier, m.Trades, m.TotalReturn*100, m.CAGR*100, m.Sharpe, ds, m.Sortino,
			m.MaxDrawdown*100, m.Volatility*100, m.WinRate*100, m.ProfitFactor, m.Turnover, m.TotalCosts,
			c.WalkForward.Consistency*100)
	}
	if rep.Benchmark.Error == "" {
		m := rep.Benchmark.Metrics
		w("| buy-and-hold %s | — | %+.2f%% | %+.2f%% | %.2f | — | %.2f | %.2f%% | %.2f%% | — | — | — | — | — |\n",
			rep.Benchmark.Ticker, m.TotalReturn*100, m.CAGR*100, m.Sharpe, m.Sortino, m.MaxDrawdown*100, m.Volatility*100)
	} else {
		w("\nbenchmark unavailable: %s\n", rep.Benchmark.Error)
	}
	if rep.Trials > 1 {
		w("\nDeflated Sharpe assumes %d configurations were tried in total.\n", rep.Trials)
	}
	w("\n")

	if len(rep.Costs) > 0 {
		w("## Per-fold out-of-sample results (x%.1f costs)\n\n", rep.Costs[len(rep.Costs)-1].Multiplier)
		w("| fold | test window | trades | return | Sharpe | max DD | note |\n|---|---|---|---|---|---|---|\n")
		for _, f := range rep.Costs[len(rep.Costs)-1].WalkForward.Folds {
			m := f.Metrics
			w("| %d | %s .. %s | %d | %+.2f%% | %.2f | %.2f%% | %s |\n", f.Fold.Index,
				f.Fold.TestStart.Format("2006-01-02"), f.Fold.TestEnd.Format("2006-01-02"),
				m.Trades, m.TotalReturn*100, m.Sharpe, m.MaxDrawdown*100, f.Error)
		}
		w("\n")
	}

	w("## Predictions against realised outcomes\n\n")
	w("Scored on every candidate bar in the test windows, whether or not the risk engine allowed a signal. ")
	w("The base rate is the accuracy of always predicting the most common class; lift below one standard error is noise.\n\n")
	for _, c := range rep.Costs[:min(1, len(rep.Costs))] {
		horizons := make([]domain.Horizon, 0, len(c.Evaluation))
		for h := range c.Evaluation {
			horizons = append(horizons, h)
		}
		sort.Slice(horizons, func(i, j int) bool { return horizons[i].Duration() < horizons[j].Duration() })
		w("| horizon | n | accuracy | base rate | lift | Brier | Brier skill | log loss | max calibration dev | ECE |\n|---|---|---|---|---|---|---|---|---|---|\n")
		for _, h := range horizons {
			ev := c.Evaluation[h]
			w("| %s | %d | %.3f | %.3f | %+.3f | %.4f | %+.3f | %.3f | %.3f | %.3f |\n",
				h, ev.Samples, ev.Accuracy, ev.BaseRate, ev.Accuracy-ev.BaseRate, ev.BrierScore, ev.BrierSkill, ev.LogLoss,
				ev.MaxCalibrationDeviation, ev.ExpectedCalibrationError)
		}
		w("\n%d predictions tracked, %d scored, %d discarded as unresolvable.\n\n", c.Predictions, c.Outcomes, c.Discarded)

		for _, h := range horizons {
			ev := c.Evaluation[h]
			if len(ev.Calibration) == 0 {
				continue
			}
			w("### Calibration, UP class, %s\n\n| predicted | observed | n |\n|---|---|---|\n", h)
			for _, bin := range ev.Calibration {
				if bin.Count == 0 {
					continue
				}
				w("| %.2f–%.2f | %.3f | %d |\n", bin.Lower, bin.Upper, bin.Observed, bin.Count)
			}
			w("\n")
		}
		for _, h := range horizons {
			if rg, ok := c.ByRegime[h]; ok && len(rg) > 0 {
				w("### By regime, %s\n\n| regime | n | accuracy | base rate | lift |\n|---|---|---|---|---|\n", h)
				names := make([]string, 0, len(rg))
				for r := range rg {
					names = append(names, string(r))
				}
				sort.Strings(names)
				for _, r := range names {
					ev := rg[domain.Regime(r)]
					w("| %s | %d | %.3f | %.3f | %+.3f |\n", r, ev.Samples, ev.Accuracy, ev.BaseRate, ev.Accuracy-ev.BaseRate)
				}
				w("\n")
			}
		}
		for _, h := range horizons {
			vc := c.VetoCost[h]
			w("### Veto cost, %s\n\n%d allowed, %d blocked, %d watch-only. %s\n\n", h, vc.Allowed, vc.Blocked, vc.WatchOnly, vc.Interpretation)
		}
	}

	w("## Notes\n\n")
	for _, n := range rep.Notes {
		w("- %s\n", n)
	}
	w("\n_Generated %s._\n", time.Now().UTC().Format(time.RFC3339))
	return b.String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// dailySamples counts daily observations in the concatenated curve, which is
// what the deflated Sharpe's standard error should be based on.
func dailySamples(wf WalkForwardReport) int {
	n := 0
	for _, f := range wf.Folds {
		n += len(toDaily(f.Run.EquityCurve))
	}
	return n
}

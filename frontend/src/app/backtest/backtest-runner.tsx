'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';

import { DrawdownChart, LineChart } from '@/components/charts';
import { PaperBadge } from '@/components/disclaimer';
import { Card, DefinitionList, Metric } from '@/components/page';
import {
  EmptyState,
  ErrorState,
  InsufficientSamples,
  LoadingState,
} from '@/components/states';
import { useApi } from '@/hooks/use-api';
import {
  NOT_MEASURED,
  deflatedSharpePresentation,
  formatCount,
  formatMoney,
  formatNumber,
  formatPercent,
  formatPrice,
  formatSignedMoney,
  formatSignedPercent,
  formatTimestamp,
  humanizeEnum,
  isZeroTime,
  list,
} from '@/lib/format';
import type { ApiFailure } from '@/lib/api-client';
import type { BacktestMetrics, BacktestRun, Strategy } from '@/lib/types';

/**
 * The backtest configure-and-run surface.
 *
 * The one number this component treats with more ceremony than the rest is the
 * deflated Sharpe ratio, which is meaningless without its trial count: a Sharpe
 * that has been deflated for one trial has been deflated for nothing. Where the
 * trial count is absent, the deflated figure is withheld and the reason is shown
 * in its place.
 */
export function BacktestRunner({ strategies }: { strategies: Strategy[] }) {
  const now = useMemo(() => new Date(), []);
  const defaultEnd = useMemo(() => now.toISOString().slice(0, 16), [now]);
  const defaultStart = useMemo(
    () => new Date(now.getTime() - 24 * 60 * 60 * 1000).toISOString().slice(0, 16),
    [now],
  );

  const [strategyId, setStrategyId] = useState(strategies[0]?.id ?? '');
  const [name, setName] = useState('adhoc');
  const [start, setStart] = useState(defaultStart);
  const [end, setEnd] = useState(defaultEnd);
  const [interval, setInterval] = useState('1m');
  const [seed, setSeed] = useState('42');
  const [cash, setCash] = useState('100000');
  const [tickers, setTickers] = useState('');

  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const runsState = useApi<BacktestRun[] | null>(
    'backtests',
    useCallback((client, signal) => client.request<BacktestRun[] | null>('backtests', { params: { limit: 50 }, signal }), []),
    { pollMs: 5000 },
  );
  const runs = list(runsState.data);

  // Select the newest run once the list first arrives, so the page is never a
  // configured form above an empty void.
  useEffect(() => {
    if (!selectedId && runs.length > 0) setSelectedId(runs[0]?.id ?? null);
  }, [runs, selectedId]);

  const submit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      setSubmitting(true);
      setSubmitError(null);
      try {
        const res = await fetch('/api/backtests', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name,
            strategy_id: strategyId,
            start: new Date(start).toISOString(),
            end: new Date(end).toISOString(),
            interval,
            seed: Number.parseInt(seed, 10) || 0,
            starting_cash: Number.parseFloat(cash) || 0,
            tickers: tickers
              .split(/[,\s]+/)
              .map((t) => t.trim())
              .filter(Boolean),
          }),
        });
        const body = (await res.json()) as
          | { data: BacktestRun }
          | { error: { message: string; detail?: string } };
        if (!res.ok || !('data' in body)) {
          const err = 'error' in body ? body.error : { message: `HTTP ${res.status}` };
          setSubmitError(`${err.message}${'detail' in err && err.detail ? ` — ${err.detail}` : ''}`);
          return;
        }
        setSelectedId(body.data.id);
        await runsState.refetch();
      } catch (err) {
        setSubmitError(err instanceof Error ? err.message : 'the request failed');
      } finally {
        setSubmitting(false);
      }
    },
    [name, strategyId, start, end, interval, seed, cash, tickers, runsState],
  );

  return (
    <div className="space-y-4">
      <Card
        title="Configure a run"
        subtitle="A backtest is accepted asynchronously and executed off the request path; the run appears below with status QUEUED and updates as it progresses."
      >
        {strategies.length === 0 ? (
          <EmptyState
            title="No strategies are loaded"
            detail="A backtest must name a strategy that the platform has loaded. With none configured there is nothing to run."
          />
        ) : (
          <form onSubmit={submit} className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <div className="sm:col-span-2">
              <label htmlFor="bt-strategy" className="field-label">
                Strategy
              </label>
              <select
                id="bt-strategy"
                className="field-input"
                value={strategyId}
                onChange={(e) => setStrategyId(e.target.value)}
                required
              >
                {strategies.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name} ({s.id} v{s.version}){s.enabled ? '' : ' — disabled'}
                  </option>
                ))}
              </select>
            </div>

            <div>
              <label htmlFor="bt-name" className="field-label">
                Run name
              </label>
              <input
                id="bt-name"
                className="field-input"
                value={name}
                onChange={(e) => setName(e.target.value)}
                maxLength={80}
              />
            </div>

            <div>
              <label htmlFor="bt-interval" className="field-label">
                Bar interval
              </label>
              <select
                id="bt-interval"
                className="field-input"
                value={interval}
                onChange={(e) => setInterval(e.target.value)}
              >
                {['1m', '5m', '15m', '1h', '1d'].map((i) => (
                  <option key={i} value={i}>
                    {i}
                  </option>
                ))}
              </select>
            </div>

            <div>
              <label htmlFor="bt-start" className="field-label">
                Start (UTC)
              </label>
              <input
                id="bt-start"
                type="datetime-local"
                className="field-input"
                value={start}
                onChange={(e) => setStart(e.target.value)}
                required
              />
            </div>

            <div>
              <label htmlFor="bt-end" className="field-label">
                End (UTC)
              </label>
              <input
                id="bt-end"
                type="datetime-local"
                className="field-input"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
                required
              />
            </div>

            <div>
              <label htmlFor="bt-seed" className="field-label">
                Seed
              </label>
              <input
                id="bt-seed"
                type="number"
                className="field-input"
                value={seed}
                onChange={(e) => setSeed(e.target.value)}
                aria-describedby="bt-seed-help"
              />
              <p id="bt-seed-help" className="mt-0.5 text-[11px] text-ink-500">
                Given the same seed, config hash and model version, the run
                reproduces byte for byte.
              </p>
            </div>

            <div>
              <label htmlFor="bt-cash" className="field-label">
                Starting cash
              </label>
              <input
                id="bt-cash"
                type="number"
                min={1}
                className="field-input"
                value={cash}
                onChange={(e) => setCash(e.target.value)}
              />
            </div>

            <div className="sm:col-span-2 lg:col-span-4">
              <label htmlFor="bt-tickers" className="field-label">
                Tickers (optional, comma separated)
              </label>
              <input
                id="bt-tickers"
                className="field-input"
                placeholder="Leave empty to use the strategy's configured universe"
                value={tickers}
                onChange={(e) => setTickers(e.target.value)}
              />
            </div>

            <div className="sm:col-span-2 lg:col-span-4 flex flex-wrap items-center gap-3">
              <button type="submit" className="btn btn-primary" disabled={submitting}>
                {submitting ? 'Submitting…' : 'Run backtest'}
              </button>
              <PaperBadge />
              <span className="text-[11px] text-ink-500">
                Simulated against the platform&apos;s cost model. No orders are placed.
              </span>
            </div>

            {submitError ? (
              <p role="alert" className="sm:col-span-2 lg:col-span-4 rounded border border-deny bg-red-50 px-3 py-2 text-sm text-ink-800">
                <span className="font-semibold text-deny">
                  <span aria-hidden="true">✕ </span>The run was not accepted.{' '}
                </span>
                {submitError}
              </p>
            ) : null}
          </form>
        )}
      </Card>

      <Card
        title="Runs"
        subtitle={runsState.refreshing ? 'Refreshing…' : 'Polled every 5 seconds.'}
      >
        {runsState.loading ? (
          <LoadingState label="Loading backtest runs" />
        ) : runsState.error && runs.length === 0 ? (
          <ErrorState error={runsState.error as ApiFailure} what="backtest runs" onRetry={runsState.refetch} />
        ) : runs.length === 0 ? (
          <EmptyState
            title="No backtests have been run"
            detail="Configure one above. Runs are stored, so a completed run remains available for comparison."
          />
        ) : (
          <div className="table-wrap">
            <table className="data-table">
              <caption className="sr-only">Backtest runs, newest first.</caption>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Strategy</th>
                  <th scope="col">Status</th>
                  <th scope="col">Window</th>
                  <th scope="col">Trades</th>
                  <th scope="col">Total return</th>
                  <th scope="col">Sharpe</th>
                  <th scope="col">Max DD</th>
                  <th scope="col"> </th>
                </tr>
              </thead>
              <tbody>
                {runs.map((r) => (
                  <tr key={r.id} className={r.id === selectedId ? 'bg-ink-100' : undefined}>
                    <th scope="row">{r.name}</th>
                    <td className="text-xs">{r.strategy_id}</td>
                    <td className="text-xs">
                      <StatusBadge status={r.status} />
                      {r.error ? (
                        <span className="mt-0.5 block max-w-[14rem] text-[11px] leading-snug text-deny">
                          {r.error}
                        </span>
                      ) : null}
                    </td>
                    <td className="num whitespace-nowrap text-[11px]">
                      {formatTimestamp(r.start)}
                      <span className="block">{formatTimestamp(r.end)}</span>
                    </td>
                    <td className="num">{formatCount(r.metrics.trades)}</td>
                    <td className="num">{formatSignedPercent(r.metrics.total_return)}</td>
                    <td className="num">{formatNumber(r.metrics.sharpe, 2)}</td>
                    <td className="num">{formatPercent(r.metrics.max_drawdown)}</td>
                    <td>
                      <button
                        type="button"
                        className="btn px-2 py-1 text-xs"
                        onClick={() => setSelectedId(r.id)}
                        aria-pressed={r.id === selectedId}
                      >
                        {r.id === selectedId ? 'Showing' : 'Show'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {selectedId ? <RunDetail id={selectedId} /> : null}
    </div>
  );
}

function StatusBadge({ status }: { status: BacktestRun['status'] }) {
  const tone =
    status === 'COMPLETED'
      ? 'border-allow bg-teal-50 text-allow'
      : status === 'FAILED' || status === 'ABORTED'
        ? 'border-deny bg-red-50 text-deny'
        : 'border-watch bg-amber-50 text-watch';
  const glyph =
    status === 'COMPLETED' ? '✓' : status === 'FAILED' || status === 'ABORTED' ? '✕' : '◐';
  return (
    <span className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[10px] font-bold ${tone}`}>
      <span aria-hidden="true">{glyph}</span>
      {status}
    </span>
  );
}

function RunDetail({ id }: { id: string }) {
  const state = useApi<BacktestRun>(
    `backtest-${id}`,
    useCallback(
      (client, signal) => client.request<BacktestRun>(`backtests/${encodeURIComponent(id)}`, { signal }),
      [id],
    ),
    { pollMs: 5000 },
  );

  if (state.loading) return <Card title="Run detail"><LoadingState label="Loading run" /></Card>;
  if (!state.data) {
    return (
      <Card title="Run detail">
        {state.error ? (
          <ErrorState error={state.error} what="the backtest run" onRetry={state.refetch} />
        ) : (
          <EmptyState title="No run detail." />
        )}
      </Card>
    );
  }

  const run = state.data;
  const curve = list(run.equity_curve);
  const trades = list(run.trades);
  const m = run.metrics;
  const regimes = Object.entries(run.regime_metrics ?? {});

  return (
    <div className="space-y-4">
      <Card
        title={`Run: ${run.name}`}
        subtitle={`${run.strategy_id} · ${run.interval} · ${formatTimestamp(run.start)} → ${formatTimestamp(run.end)}`}
        actions={<StatusBadge status={run.status} />}
      >
        {run.status === 'QUEUED' || run.status === 'RUNNING' ? (
          <p className="rounded border border-watch bg-amber-50 px-3 py-2 text-sm text-ink-800">
            <span className="font-semibold text-watch">
              <span aria-hidden="true">◐ </span>
            </span>
            This run has not finished. Metrics below are incomplete and will change.
          </p>
        ) : null}
        {run.error ? (
          <p role="alert" className="rounded border border-deny bg-red-50 px-3 py-2 text-sm text-ink-800">
            <span className="font-semibold text-deny">
              <span aria-hidden="true">✕ </span>The run failed:{' '}
            </span>
            {run.error}
          </p>
        ) : null}

        <div className="mt-3 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <Metric label="Total return" value={formatSignedPercent(m.total_return)} sub={<PaperBadge />} />
          <Metric label="CAGR" value={formatSignedPercent(m.cagr)} />
          <Metric label="Sharpe" value={formatNumber(m.sharpe, 2)} hint="Annualised, before deflation for the number of configurations tried." />
          <Metric label="Max drawdown" value={formatPercent(m.max_drawdown)} sub={<span>{formatNumber(m.max_drawdown_days, 1)} days</span>} />
        </div>

        {/* The deflated Sharpe, with its trial count or not at all. */}
        <div className="mt-4">
          <DeflatedSharpe metrics={m} />
        </div>

        <div className="mt-4">
          <DefinitionList
            columns={3}
            items={[
              { term: 'Sortino', value: formatNumber(m.sortino, 2) },
              { term: 'Calmar', value: formatNumber(m.calmar, 2) },
              { term: 'Volatility', value: formatPercent(m.volatility) },
              { term: 'Win rate', value: formatPercent(m.win_rate) },
              { term: 'Profit factor', value: formatNumber(m.profit_factor, 2) },
              { term: 'Expectancy', value: formatMoney(m.expectancy) },
              { term: 'Turnover', value: formatNumber(m.turnover, 2) },
              { term: 'Avg exposure', value: formatPercent(m.avg_exposure) },
              { term: 'Trades', value: formatCount(m.trades) },
              { term: 'Avg hold', value: `${formatNumber(m.avg_hold_hours, 1)} h` },
              { term: 'Total costs', value: formatMoney(m.total_costs) },
              { term: 'Events processed', value: formatCount(run.events_processed) },
            ]}
          />
        </div>

        <div className="mt-4">
          <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
            Determinism inputs
          </h3>
          <DefinitionList
            columns={3}
            items={[
              { term: 'Seed', value: <span className="font-mono">{run.seed}</span> },
              { term: 'Config hash', value: <span className="font-mono text-xs">{run.config_hash || '—'}</span> },
              { term: 'Model version', value: <span className="font-mono text-xs">{run.model_version || '—'}</span> },
              { term: 'Code version', value: <span className="font-mono text-xs">{run.code_version || '—'}</span> },
              { term: 'Result hash', value: <span className="font-mono text-xs">{run.result_hash || '—'}</span> },
              { term: 'Universe', value: `${list(run.universe).length} instruments` },
            ]}
          />
          <p className="mt-1 max-w-prose text-[11px] leading-snug text-ink-500">
            Given these five values the run reproduces byte for byte. If a rerun
            produces a different result hash, something outside this list changed.
          </p>
        </div>

        <div className="mt-4">
          <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
            Cost model
          </h3>
          <DefinitionList
            columns={4}
            items={[
              { term: 'Commission', value: `${formatNumber(run.costs.commission_bps, 2)} bps` },
              { term: 'Spread cost', value: `${formatNumber(run.costs.spread_cost_bps, 2)} bps` },
              { term: 'Slippage', value: `${formatNumber(run.costs.slippage_bps, 2)} bps (${run.costs.slippage_model})` },
              { term: 'Participation cap', value: formatPercent(run.costs.participation_cap) },
            ]}
          />
          <p className="mt-1 max-w-prose text-[11px] leading-snug text-ink-500">
            Optimistic cost assumptions are the most common way a backtest lies.
            These are the assumptions this run actually used.
          </p>
        </div>
      </Card>

      <Card title="Equity and drawdown">
        {curve.length < 2 ? (
          <EmptyState
            title="No equity curve"
            detail="The run produced fewer than two equity observations, so there is no series to draw."
          />
        ) : (
          <div className="space-y-3">
            <LineChart
              points={curve
                .filter((p) => !isZeroTime(p.at))
                .map((p) => ({ x: Date.parse(p.at), y: p.equity }))
                .sort((a, b) => a.x - b.x)}
              label="Backtest equity curve"
              yFormat={(v) => formatMoney(v)}
              xFormat={(v) => new Date(v).toISOString().slice(0, 16).replace('T', ' ')}
              baseline={run.starting_cash}
              area
            />
            <DrawdownChart
              points={curve
                .filter((p) => !isZeroTime(p.at))
                .map((p) => ({ x: Date.parse(p.at), y: p.drawdown }))
                .sort((a, b) => a.x - b.x)}
              label="Backtest drawdown"
            />
          </div>
        )}
      </Card>

      <Card
        title="Per-regime breakdown"
        subtitle="A strategy that only works in one environment has not been shown to work."
      >
        {regimes.length === 0 ? (
          <EmptyState
            title="No per-regime breakdown"
            detail="The run did not partition its results by regime, or no regime accumulated enough activity to report."
          />
        ) : (
          <div className="table-wrap">
            <table className="data-table">
              <caption className="sr-only">Backtest metrics partitioned by market regime.</caption>
              <thead>
                <tr>
                  <th scope="col">Regime</th>
                  <th scope="col">Trades</th>
                  <th scope="col">Return</th>
                  <th scope="col">Sharpe</th>
                  <th scope="col">Win rate</th>
                  <th scope="col">Max DD</th>
                  <th scope="col">Profit factor</th>
                </tr>
              </thead>
              <tbody>
                {regimes.map(([regime, rm]) => {
                  const x = rm as BacktestMetrics;
                  const thin = x.trades < 20;
                  return (
                    <tr key={regime}>
                      <th scope="row">{humanizeEnum(regime)}</th>
                      <td className="num">{formatCount(x.trades)}</td>
                      <td className="num">{formatSignedPercent(x.total_return)}</td>
                      <td className="num">
                        {thin ? <span className="text-ink-400">—</span> : formatNumber(x.sharpe, 2)}
                      </td>
                      <td className="num">
                        {thin ? <span className="text-ink-400">—</span> : formatPercent(x.win_rate)}
                      </td>
                      <td className="num">{formatPercent(x.max_drawdown)}</td>
                      <td className="num">
                        {thin ? <span className="text-ink-400">—</span> : formatNumber(x.profit_factor, 2)}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            <p className="mt-2 text-[11px] text-ink-500">
              Rates are withheld for regimes with fewer than 20 trades: over a
              handful of trades a win rate is noise, and a Sharpe is undefined in
              practice.
            </p>
          </div>
        )}
      </Card>

      <Card title="Trades" subtitle={`${formatCount(trades.length)} round trips.`}>
        {trades.length === 0 ? (
          <EmptyState
            title="No trades"
            detail="The strategy produced no round trips over this window. That is a result, not an error: the entry conditions never all held at once."
          />
        ) : (
          <div className="table-wrap max-h-[30rem] overflow-y-auto">
            <table className="data-table min-w-[52rem]">
              <caption className="sr-only">Every simulated round trip in this run.</caption>
              <thead className="sticky top-0 bg-white">
                <tr>
                  <th scope="col">Ticker</th>
                  <th scope="col">Side</th>
                  <th scope="col">Entry</th>
                  <th scope="col">Exit</th>
                  <th scope="col">Qty</th>
                  <th scope="col">P&L</th>
                  <th scope="col">Return</th>
                  <th scope="col">Costs</th>
                  <th scope="col">Hold</th>
                  <th scope="col">Regime</th>
                  <th scope="col">Exit reason</th>
                </tr>
              </thead>
              <tbody>
                {trades.map((t, i) => (
                  <tr key={i}>
                    <th scope="row">{t.ticker}</th>
                    <td className="text-xs">{t.side}</td>
                    <td className="num text-xs">
                      {formatPrice(t.entry_price)}
                      <span className="block text-[10px] text-ink-500">{formatTimestamp(t.entry_at)}</span>
                    </td>
                    <td className="num text-xs">
                      {formatPrice(t.exit_price)}
                      <span className="block text-[10px] text-ink-500">{formatTimestamp(t.exit_at)}</span>
                    </td>
                    <td className="num">{formatNumber(t.quantity, 0)}</td>
                    <td className={`num font-medium ${t.pnl >= 0 ? 'text-allow' : 'text-deny'}`}>
                      <span aria-hidden="true">{t.pnl >= 0 ? '▲ ' : '▼ '}</span>
                      {formatSignedMoney(t.pnl)}
                    </td>
                    <td className="num">{formatSignedPercent(t.return_pct)}</td>
                    <td className="num">{formatMoney(t.costs)}</td>
                    <td className="num text-xs">{formatNumber(t.hold_period / 3.6e12, 1)} h</td>
                    <td className="text-xs">{t.regime ? humanizeEnum(t.regime) : '—'}</td>
                    <td className="text-xs text-ink-600">{t.exit_reason}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}

/**
 * The deflated Sharpe ratio and its trial count.
 *
 * Reporting a deflated Sharpe without saying how many configurations were tried
 * is worse than reporting nothing: it looks like the multiple-comparisons
 * problem has been handled when the reader cannot tell whether it has. So when
 * the trial count is missing, the number is withheld and the reason is given.
 */
function DeflatedSharpe({ metrics }: { metrics: BacktestMetrics }) {
  const p = deflatedSharpePresentation(metrics);
  const ds = metrics.deflated_sharpe;
  const trials = metrics.trials_considered;

  if (p.state === 'not_computed') {
    return <InsufficientSamples note={p.note} />;
  }
  if (p.state === 'withheld_no_trial_count') {
    return (
      <div className="rounded border border-watch bg-amber-50 px-3 py-2">
        <p className="text-sm font-semibold text-watch">
          <span aria-hidden="true">! </span>Deflated Sharpe withheld
        </p>
        <p className="mt-1 max-w-prose text-xs leading-relaxed text-ink-800">
          The run reports a deflated Sharpe of{' '}
          <span className="num font-medium">{formatNumber(ds, 3)}</span> but no
          trial count. {p.note}
        </p>
      </div>
    );
  }
  return (
    <div className="rounded border border-ink-400 bg-ink-50 px-3 py-2">
      <div className="flex flex-wrap items-baseline gap-3">
        <span className="metric-label">Deflated Sharpe</span>
        <span className="num text-xl font-semibold text-ink-900">
          {p.showValue ? formatNumber(ds, 3) : NOT_MEASURED}
        </span>
        <span className="text-sm text-ink-700">
          deflated for <span className="num font-semibold">{formatCount(trials)}</span>{' '}
          trial{trials === 1 ? '' : 's'}
        </span>
      </div>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-ink-600">
        Plain Sharpe was <span className="num">{formatNumber(metrics.sharpe, 2)}</span>.
        The gap between the two is the discount applied for having searched{' '}
        {formatCount(trials)} configuration{trials === 1 ? '' : 's'}: the more that
        were tried, the more likely the best one looked good by luck.
        {p.showValue && ds !== undefined && ds <= 0 ? (
          <>
            {' '}
            <span className="font-semibold text-deny">
              After deflation this run shows no skill.
            </span>
          </>
        ) : null}
      </p>
    </div>
  );
}

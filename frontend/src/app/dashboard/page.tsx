import type { Metadata } from 'next';
import Link from 'next/link';

import { LineChart } from '@/components/charts';
import { DisclaimerBanner, DisclaimerNote } from '@/components/disclaimer';
import { DistributionBar } from '@/components/distribution';
import { Card, Freshness, Metric, PageHeader } from '@/components/page';
import {
  DegradedBanner,
  EmptyState,
  ErrorState,
  InsufficientSamples,
} from '@/components/states';
import {
  DriftBadge,
  RiskDecisionBadge,
  SideBadge,
  SignalStatusBadge,
} from '@/components/verdicts';
import {
  accuracyVsBaseRate,
  formatAge,
  formatPercent,
  formatScore,
  formatTimestamp,
  humanizeEnum,
  isZeroTime,
  list,
} from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';

export const metadata: Metadata = { title: 'Dashboard' };
export const dynamic = 'force-dynamic';

export default async function DashboardPage() {
  const client = api();
  const [status, regimeR, stocksR, signalsR, alertsR, healthR] = await Promise.all([
    platformStatus(),
    client.regime(),
    client.stocks({ limit: 8 }),
    client.signals({ status: 'ACTIVE', limit: 8 }),
    client.alerts({ limit: 8 }),
    client.modelHealth(),
  ]);

  const regime = regimeR.ok ? regimeR.data : null;
  const stocks = stocksR.ok ? list(stocksR.data) : [];
  const signals = signalsR.ok ? list(signalsR.data) : [];
  const alerts = alertsR.ok ? list(alertsR.data) : [];
  const mh = healthR.ok ? healthR.data : null;

  const overall = mh?.overall;
  const acc = accuracyVsBaseRate(
    overall?.accuracy ?? mh?.health.accuracy,
    overall?.base_rate,
    overall?.samples ?? mh?.health.samples,
  );

  const regimeScores = regime?.scores ?? {};
  const scoreSeries = Object.entries(regimeScores)
    .filter(([, v]) => typeof v === 'number')
    .sort((a, b) => (b[1] as number) - (a[1] as number))
    .slice(0, 6);

  return (
    <>
      <PageHeader
        title="Dashboard"
        purpose="What the platform is seeing right now: the market regime it has classified, where it thinks the opportunities are, which signals are live, and whether its own model can be trusted today."
      />

      <div className="space-y-4">
        {status.unreachable ? (
          <div role="alert" className="rounded-md border border-deny bg-red-50 px-4 py-3">
            <p className="text-sm font-semibold text-deny">
              <span aria-hidden="true">✕ </span>The QuantOS API could not be reached
            </p>
            <p className="mt-1 text-sm text-ink-800">{status.unreachableReason}</p>
            <p className="mt-1 max-w-prose text-xs leading-relaxed text-ink-600">
              Nothing on this page is live. Start the platform with{' '}
              <code className="font-mono">go run ./cmd/quantos run</code>, or run
              the dashboard against fixtures with{' '}
              <code className="font-mono">npm run dev:mock</code>.
            </p>
          </div>
        ) : (
          <DegradedBanner
            degraded={status.degraded}
            ready={status.readiness.ready}
            reason={status.readiness.reason}
          />
        )}

        <DisclaimerBanner />

        {/* --- Regime -------------------------------------------------- */}
        <Card
          title="Market regime"
          subtitle="The environment the platform has classified. A regime describes what has been observed; it is not a forecast."
          actions={<Freshness servedAt={regimeR.ok ? regimeR.servedAt : undefined} meta={regimeR.ok ? regimeR.meta : undefined} />}
        >
          {!regimeR.ok ? (
            <ErrorState error={regimeR} what="the market regime" />
          ) : !regime?.regime ? (
            <EmptyState
              title="No regime has been classified yet"
              detail="The regime engine has not produced a classification. This is normal for the first moments after start-up, while the market-data warm-up completes."
            />
          ) : (
            <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)]">
              <div>
                <div className="flex flex-wrap items-baseline gap-3">
                  <p className="text-2xl font-bold tracking-tight text-ink-900">
                    {humanizeEnum(regime.regime)}
                  </p>
                  <span className="num rounded border border-ink-300 bg-ink-50 px-2 py-0.5 text-sm font-semibold text-ink-800">
                    confidence {formatPercent(regime.confidence)}
                  </span>
                </div>

                {regime.secondary ? (
                  <p className="mt-1 text-sm text-ink-600">
                    Runner-up classification:{' '}
                    <span className="font-medium text-ink-800">
                      {humanizeEnum(regime.secondary)}
                    </span>{' '}
                    (score{' '}
                    <span className="num">{formatScore(regime.secondary_score)}</span>).
                    The environment is close to the boundary between the two.
                  </p>
                ) : null}

                <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-2 sm:grid-cols-3">
                  <Axis label="Volatility" value={regime.volatility} range="0 to 1" />
                  <Axis label="Breadth" value={regime.breadth} range="−1 to 1" signed />
                  <Axis label="Momentum" value={regime.momentum} range="−1 to 1" signed />
                  <Axis
                    label="Risk appetite"
                    value={regime.risk_appetite}
                    range="−1 risk-off to 1 risk-on"
                    signed
                  />
                  <Axis label="Liquidity" value={regime.liquidity} range="0 to 1" />
                  <div>
                    <dt className="metric-label">Stability</dt>
                    <dd className="num text-sm font-semibold text-ink-900">
                      {regime.changed ? 'Just changed' : 'Unchanged'}
                    </dd>
                    {regime.previous_regime ? (
                      <p className="text-[11px] text-ink-500">
                        was {humanizeEnum(regime.previous_regime)}
                      </p>
                    ) : null}
                  </div>
                </dl>

                {list(regime.evidence).length > 0 ? (
                  <ul className="mt-3 space-y-1 text-xs text-ink-700">
                    {list(regime.evidence).slice(0, 4).map((e, i) => (
                      <li key={i} className="flex gap-1.5">
                        <span aria-hidden="true" className="text-ink-400">
                          ·
                        </span>
                        {e}
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>

              <div>
                <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-ink-500">
                  Score assigned to each candidate regime
                </h3>
                {scoreSeries.length === 0 ? (
                  <EmptyState title="No per-regime scores were recorded." />
                ) : (
                  <ul className="space-y-1.5">
                    {scoreSeries.map(([name, score]) => {
                      const v = score as number;
                      const winner = name === regime.regime;
                      return (
                        <li key={name} className="grid grid-cols-[9rem_1fr_3rem] items-center gap-2 text-xs">
                          <span className={winner ? 'font-semibold text-ink-900' : 'text-ink-600'}>
                            {winner ? <span aria-hidden="true">▸ </span> : null}
                            {humanizeEnum(name)}
                          </span>
                          <span className="h-3 rounded bg-ink-100">
                            <span
                              className={`block h-full rounded ${winner ? 'bg-ink-900' : 'bg-ink-400'}`}
                              style={{
                                width: `${Math.max(2, Math.min(100, v * 100))}%`,
                              }}
                            />
                          </span>
                          <span className="num text-right text-ink-800">
                            {formatScore(v * 100)}
                          </span>
                        </li>
                      );
                    })}
                  </ul>
                )}
                <p className="mt-2 text-[11px] leading-snug text-ink-500">
                  Scores are the regime engine&apos;s own weighting of each
                  candidate. A narrow gap between the top two means the
                  classification is not settled.
                </p>
              </div>
            </div>
          )}
        </Card>

        <div className="grid gap-4 xl:grid-cols-2">
          {/* --- Opportunities ----------------------------------------- */}
          <Card
            title="Top opportunities"
            subtitle="Ranked by the opportunity score. A high rank is a research classification, not a recommendation to trade."
          >
            {!stocksR.ok ? (
              <ErrorState error={stocksR} what="the ranked instrument list" />
            ) : stocks.length === 0 ? (
              <EmptyState
                title="No instruments have been scored yet"
                detail="The scoring engine produces a ranking once feature snapshots are warm. Nothing is being hidden — there is genuinely nothing scored."
              />
            ) : (
              <>
                <div className="table-wrap">
                  <table className="data-table">
                    <caption className="sr-only">
                      Instruments ranked by opportunity score, highest first.
                    </caption>
                    <thead>
                      <tr>
                        <th scope="col">Ticker</th>
                        <th scope="col">Opportunity</th>
                        <th scope="col">Bias</th>
                        <th scope="col">P(up) / P(flat) / P(down)</th>
                        <th scope="col">Risk decision</th>
                      </tr>
                    </thead>
                    <tbody>
                      {stocks.map((s) => (
                        <tr key={s.ticker}>
                          <td>
                            <Link href={`/stock/${encodeURIComponent(s.ticker)}`} className="link">
                              {s.ticker}
                            </Link>
                            <span className="block max-w-[12rem] truncate text-[11px] text-ink-500">
                              {s.company}
                            </span>
                            {s.stale ? (
                              <span className="text-[10px] font-semibold text-watch">
                                <span aria-hidden="true">! </span>stale
                              </span>
                            ) : null}
                          </td>
                          <td className="num font-semibold">{formatScore(s.opportunity)}</td>
                          <td>
                            <SideBadge side={s.bias} />
                          </td>
                          <td className="min-w-[9rem]">
                            <DistributionBar dist={s.probability} compact />
                          </td>
                          <td>
                            <RiskDecisionBadge decision={s.risk_decision} />
                            {s.rejection ? (
                              <span className="mt-1 block max-w-[16rem] text-[11px] leading-snug text-ink-500">
                                {s.rejection}
                              </span>
                            ) : null}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <DisclaimerNote className="mt-3" />
              </>
            )}
          </Card>

          {/* --- Active signals ---------------------------------------- */}
          <Card
            title="Active signals"
            subtitle="Paper-trading signals currently live. Every one has a full provenance record."
            actions={
              <Link href="/signals" className="link text-xs">
                All signals →
              </Link>
            }
          >
            {!signalsR.ok ? (
              <ErrorState error={signalsR} what="active signals" />
            ) : signals.length === 0 ? (
              <EmptyState
                title="No signals are active"
                detail="A signal is only created when the risk engine returns ALLOW_PAPER_SIGNAL and at least one invalidation condition can be attached. An empty list means the platform declined to emit, not that it failed — check the rejection reasons on the ranked list."
                action={
                  <Link href="/stocks" className="btn">
                    See why each instrument was rejected
                  </Link>
                }
              />
            ) : (
              <>
                <ul className="space-y-2">
                  {signals.map((sig) => (
                    <li key={sig.id} className="rounded border border-ink-200 p-2.5">
                      <div className="flex flex-wrap items-center gap-2">
                        <Link href={`/signals/${encodeURIComponent(sig.id)}`} className="link font-semibold">
                          {sig.ticker}
                        </Link>
                        <SideBadge side={sig.side} />
                        <SignalStatusBadge status={sig.status} />
                        <span className="num text-xs text-ink-600">
                          strength {formatScore(sig.strength)} · confidence{' '}
                          {formatPercent(sig.confidence)}
                        </span>
                      </div>
                      <p className="mt-1 text-xs text-ink-600">
                        {sig.horizon} horizon · expires {formatTimestamp(sig.expires_at)} (
                        {formatAge(sig.expires_at)}) · {list(sig.invalidations).length}{' '}
                        invalidation condition
                        {list(sig.invalidations).length === 1 ? '' : 's'}
                      </p>
                    </li>
                  ))}
                </ul>
                <DisclaimerNote className="mt-3" />
              </>
            )}
          </Card>

          {/* --- Alerts ------------------------------------------------ */}
          <Card
            title="Live alerts"
            subtitle="Emitted notifications, newest first."
            actions={
              <Link href="/alerts" className="link text-xs">
                All alerts →
              </Link>
            }
          >
            {!alertsR.ok ? (
              <ErrorState error={alertsR} what="alerts" />
            ) : alerts.length === 0 ? (
              <EmptyState
                title="No alerts have been raised"
                detail="Alerts require a persistent store. If the platform is running with the in-memory store this list stays empty by design — see Settings for the effective configuration."
              />
            ) : (
              <ul className="space-y-2">
                {alerts.map((a) => (
                  <li key={a.id} className="rounded border border-ink-200 p-2.5">
                    <div className="flex flex-wrap items-baseline gap-2">
                      <span
                        className={`rounded border px-1.5 py-0.5 text-[10px] font-bold uppercase ${
                          a.severity === 'CRITICAL'
                            ? 'border-deny bg-red-50 text-deny'
                            : a.severity === 'WARNING'
                              ? 'border-watch bg-amber-50 text-watch'
                              : 'border-ink-300 bg-ink-50 text-ink-600'
                        }`}
                      >
                        {a.severity}
                      </span>
                      <span className="text-sm font-semibold text-ink-900">{a.title}</span>
                      {a.ticker ? (
                        <Link href={`/stock/${encodeURIComponent(a.ticker)}`} className="link text-xs">
                          {a.ticker}
                        </Link>
                      ) : null}
                      <span className="num ml-auto text-[11px] text-ink-500">
                        {formatAge(a.created_at)}
                      </span>
                    </div>
                    <p className="mt-1 text-xs leading-snug text-ink-700">{a.message}</p>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          {/* --- Model health ------------------------------------------ */}
          <Card
            title="Model health at a glance"
            subtitle="Whether today's predictions can be trusted."
            actions={
              <Link href="/model-health" className="link text-xs">
                Full report →
              </Link>
            }
          >
            {!healthR.ok ? (
              <ErrorState error={healthR} what="model health" />
            ) : !mh ? (
              <EmptyState title="No model health record." />
            ) : (
              <div className="space-y-3">
                <div className="flex flex-wrap items-center gap-2">
                  <span
                    className={`inline-flex items-center gap-1.5 rounded border px-2 py-0.5 text-xs font-semibold ${
                      mh.health.healthy
                        ? 'border-allow bg-teal-50 text-allow'
                        : 'border-deny bg-red-50 text-deny'
                    }`}
                  >
                    <span aria-hidden="true">{mh.health.healthy ? '✓' : '✕'}</span>
                    {mh.health.healthy ? 'Healthy' : 'Not healthy'}
                  </span>
                  <DriftBadge severity={mh.drift.severity || mh.health.drift} />
                  {mh.health.provisional ? (
                    <span className="rounded border border-watch bg-amber-50 px-2 py-0.5 text-xs font-semibold text-watch">
                      Provisional — from the offline test split, not live outcomes
                    </span>
                  ) : null}
                </div>

                {acc.gate.sufficient ? (
                  <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
                    <Metric
                      label="Accuracy"
                      value={acc.accuracy}
                      sub={
                        <span>
                          base rate <span className="num">{acc.baseRate}</span>
                        </span>
                      }
                      hint={
                        acc.beatsBaseRate === null
                          ? undefined
                          : acc.beatsBaseRate
                            ? `Ahead of always guessing the most common class by ${acc.lift}.`
                            : `Behind always guessing the most common class by ${acc.lift}. The model is not adding information.`
                      }
                    />
                    <Metric
                      label="Brier score"
                      value={
                        overall && Number.isFinite(overall.brier_score)
                          ? overall.brier_score.toFixed(4)
                          : '—'
                      }
                      hint="Lower is better. 0 is perfect, 2 is the worst possible."
                    />
                    <Metric
                      label="Resolved"
                      value={acc.gate.samples.toLocaleString('en-US')}
                      hint="Predictions whose horizon has elapsed and been scored."
                    />
                  </div>
                ) : (
                  <InsufficientSamples note={acc.gate.note} />
                )}

                {typeof mh.pending === 'number' && mh.pending > 0 ? (
                  <p className="text-xs text-ink-600">
                    <span className="num font-semibold">{mh.pending.toLocaleString('en-US')}</span>{' '}
                    predictions are still awaiting their horizon and have not been
                    scored yet.
                  </p>
                ) : null}

                {list(mh.health.issues).length > 0 ? (
                  <ul className="space-y-1 text-xs text-ink-700">
                    {list(mh.health.issues).map((issue, i) => (
                      <li key={i}>
                        <span aria-hidden="true" className="text-watch">
                          !{' '}
                        </span>
                        {issue}
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>
            )}
          </Card>
        </div>

        {/* --- Regime confidence over time --------------------------- */}
        <RegimeHistoryCard />
      </div>
    </>
  );
}

function Axis({
  label,
  value,
  range,
  signed = false,
}: {
  label: string;
  value: number;
  range: string;
  signed?: boolean;
}) {
  const text = Number.isFinite(value)
    ? signed
      ? `${value > 0 ? '+' : ''}${value.toFixed(2)}`
      : value.toFixed(2)
    : '—';
  return (
    <div>
      <dt className="metric-label">{label}</dt>
      <dd className="num text-sm font-semibold text-ink-900">{text}</dd>
      <p className="text-[10px] text-ink-400">{range}</p>
    </div>
  );
}

async function RegimeHistoryCard() {
  const r = await api().regimeHistory({ limit: 120 });
  const history = r.ok ? list(r.data) : [];
  const points = history
    .filter((h) => !isZeroTime(h.as_of) && Number.isFinite(h.confidence))
    .map((h) => ({ x: Date.parse(h.as_of), y: h.confidence }))
    .sort((a, b) => a.x - b.x);

  return (
    <Card
      title="Regime confidence over time"
      subtitle="How settled the classification has been. A confidence that keeps collapsing means the environment is not classifiable, and downstream scores conditioned on it should be read with that in mind."
      actions={
        <Link href="/market" className="link text-xs">
          Market detail →
        </Link>
      }
    >
      {!r.ok ? (
        <ErrorState error={r} what="regime history" />
      ) : points.length === 0 ? (
        <EmptyState
          title="No regime history is stored"
          detail="Regime history comes from the persistent store. With the in-memory store configured, only the current regime is available."
        />
      ) : (
        <>
          <LineChart
            points={points}
            label="Regime classification confidence"
            yFormat={(v) => `${(v * 100).toFixed(0)}%`}
            xFormat={(v) => new Date(v).toISOString().slice(11, 16)}
            area
          />
          <p className="mt-1 text-[11px] text-ink-500">
            {points.length} observations from {formatTimestamp(history[history.length - 1]?.as_of)}{' '}
            to {formatTimestamp(history[0]?.as_of)}.
          </p>
        </>
      )}
    </Card>
  );
}

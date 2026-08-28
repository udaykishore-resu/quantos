import type { Metadata } from 'next';

import { BarChart, LineChart } from '@/components/charts';
import { Card, DefinitionList, Freshness, Metric, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import {
  formatCount,
  formatNumber,
  formatPercent,
  formatSignedPercent,
  formatTimestamp,
  humanizeEnum,
  isZeroTime,
  list,
} from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';

export const metadata: Metadata = { title: 'Market' };
export const dynamic = 'force-dynamic';

export default async function MarketPage() {
  const client = api();
  const [platform, regimeR, historyR, relR, statusR] = await Promise.all([
    platformStatus(),
    client.regime(),
    client.regimeHistory({ limit: 200 }),
    client.relationships(),
    client.marketStatus(),
  ]);

  const regime = regimeR.ok ? regimeR.data : null;
  const history = historyR.ok ? list(historyR.data) : [];
  const rel = relR.ok ? relR.data : null;
  const status = statusR.ok ? statusR.data : null;

  return (
    <>
      <PageHeader
        title="Market"
        purpose="The environment the platform believes it is operating in: the classified regime and how settled it is, cross-asset correlation, sector rotation, and where relationships that normally hold have broken down."
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />

        <Card
          title="Coverage"
          actions={<Freshness servedAt={statusR.ok ? statusR.servedAt : undefined} meta={statusR.ok ? statusR.meta : undefined} />}
        >
          {!statusR.ok ? (
            <ErrorState error={statusR} what="market status" />
          ) : !status ? (
            <EmptyState title="No market status was returned." />
          ) : (
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <Metric
                label="Regime"
                value={status.regime ? humanizeEnum(status.regime) : 'Unclassified'}
              />
              <Metric label="Confidence" value={formatPercent(status.regime_confidence)} />
              <Metric
                label="Instruments tracked"
                value={formatCount(status.instruments)}
              />
              <Metric
                label="Stale instruments"
                value={formatCount(status.stale_instruments)}
                sub={
                  status.stale_instruments > 0 ? (
                    <span className="font-semibold text-watch">
                      <span aria-hidden="true">! </span>
                      {formatPercent(
                        status.instruments > 0
                          ? status.stale_instruments / status.instruments
                          : 0,
                      )}{' '}
                      of coverage is stale
                    </span>
                  ) : (
                    <span className="text-allow">
                      <span aria-hidden="true">✓ </span>all readings current
                    </span>
                  )
                }
                hint="A stale instrument may still be displayed but may never produce a new signal."
              />
            </div>
          )}
        </Card>

        <Card
          title="Regime classification"
          subtitle="A regime describes the environment that has been observed. It is not a forecast."
        >
          {!regimeR.ok ? (
            <ErrorState error={regimeR} what="the market regime" />
          ) : !regime?.regime ? (
            <EmptyState title="No regime has been classified yet." />
          ) : (
            <div className="space-y-4">
              <div className="flex flex-wrap items-baseline gap-3">
                <p className="text-2xl font-bold text-ink-900">
                  {humanizeEnum(regime.regime)}
                </p>
                <span className="num rounded border border-ink-300 bg-ink-50 px-2 py-0.5 text-sm font-semibold">
                  confidence {formatPercent(regime.confidence)}
                </span>
                {regime.secondary ? (
                  <span className="text-sm text-ink-600">
                    runner-up {humanizeEnum(regime.secondary)} (
                    {formatNumber(regime.secondary_score, 3)})
                  </span>
                ) : null}
              </div>

              <DefinitionList
                columns={3}
                items={[
                  { term: 'Volatility', value: formatNumber(regime.volatility, 3), hint: '0 calm to 1 extreme' },
                  { term: 'Breadth', value: formatNumber(regime.breadth, 3), hint: '−1 to 1, advancers vs decliners' },
                  { term: 'Momentum', value: formatNumber(regime.momentum, 3), hint: '−1 to 1' },
                  { term: 'Risk appetite', value: formatNumber(regime.risk_appetite, 3), hint: '−1 risk-off to 1 risk-on' },
                  { term: 'Liquidity', value: formatNumber(regime.liquidity, 3), hint: '0 to 1, market-wide volume' },
                  { term: 'Version', value: regime.version || '—' },
                ]}
              />

              <div className="grid gap-4 lg:grid-cols-2">
                <div>
                  <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                    Score per candidate regime
                  </h3>
                  <BarChart
                    data={Object.entries(regime.scores ?? {}).map(([k, v]) => ({
                      label: humanizeEnum(k),
                      value: (v as number) ?? 0,
                    }))}
                    format={(v) => formatNumber(v, 3)}
                    emptyMessage="No per-regime scores were recorded, so the classification cannot be explained."
                  />
                </div>
                <div>
                  <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                    Raw inputs
                  </h3>
                  <BarChart
                    data={Object.entries(regime.inputs ?? {}).map(([k, v]) => ({
                      label: k,
                      value: v,
                    }))}
                    format={(v) => formatNumber(v, 3)}
                    emptyMessage="No raw inputs were retained for this classification."
                  />
                </div>
              </div>

              {list(regime.evidence).length > 0 ? (
                <div>
                  <h3 className="mb-1 text-xs font-bold uppercase tracking-wide text-ink-600">
                    Evidence
                  </h3>
                  <ul className="space-y-0.5 text-sm text-ink-800">
                    {list(regime.evidence).map((e, i) => (
                      <li key={i}>· {e}</li>
                    ))}
                  </ul>
                </div>
              ) : null}
            </div>
          )}
        </Card>

        <Card
          title="Regime history"
          subtitle="How the classification and its confidence have moved."
        >
          {!historyR.ok ? (
            <ErrorState error={historyR} what="regime history" />
          ) : history.length === 0 ? (
            <EmptyState
              title="No regime history is stored"
              detail="History comes from the persistent store. With the in-memory store configured, only the current classification exists."
            />
          ) : (
            <>
              <LineChart
                points={history
                  .filter((h) => !isZeroTime(h.as_of))
                  .map((h) => ({ x: Date.parse(h.as_of), y: h.confidence }))
                  .sort((a, b) => a.x - b.x)}
                label="Regime classification confidence over time"
                yFormat={(v) => `${(v * 100).toFixed(0)}%`}
                xFormat={(v) => new Date(v).toISOString().slice(11, 16)}
                area
              />
              <div className="table-wrap mt-3 max-h-72 overflow-y-auto">
                <table className="data-table">
                  <caption className="sr-only">Regime history, newest first.</caption>
                  <thead className="sticky top-0 bg-white">
                    <tr>
                      <th scope="col">As of</th>
                      <th scope="col">Regime</th>
                      <th scope="col">Confidence</th>
                      <th scope="col">Changed</th>
                      <th scope="col">Previous</th>
                    </tr>
                  </thead>
                  <tbody>
                    {history.slice(0, 60).map((h, i) => (
                      <tr key={i}>
                        <td className="num whitespace-nowrap text-xs">
                          {formatTimestamp(h.as_of)}
                        </td>
                        <td className="font-medium">{humanizeEnum(h.regime)}</td>
                        <td className="num">{formatPercent(h.confidence)}</td>
                        <td className="text-xs">
                          {h.changed ? (
                            <span className="font-semibold text-watch">
                              <span aria-hidden="true">! </span>changed
                            </span>
                          ) : (
                            <span className="text-ink-400">—</span>
                          )}
                        </td>
                        <td className="text-xs text-ink-600">
                          {h.previous_regime ? humanizeEnum(h.previous_regime) : '—'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
        </Card>

        <Card
          title="Cross-asset relationships"
          subtitle="Correlation, rotation, and where series that normally move together currently do not."
        >
          {!relR.ok ? (
            <ErrorState error={relR} what="cross-asset relationships" />
          ) : !rel ? (
            <EmptyState title="No relationship state." />
          ) : (
            <div className="space-y-4">
              <DefinitionList
                columns={3}
                items={[
                  { term: 'Rotation', value: rel.rotation ? humanizeEnum(rel.rotation) : '—' },
                  { term: 'Risk-on score', value: formatNumber(rel.risk_on_score, 3), hint: '−1 risk-off to 1 risk-on' },
                  { term: 'As of', value: formatTimestamp(rel.as_of) },
                ]}
              />

              <div className="grid gap-4 lg:grid-cols-2">
                <div>
                  <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                    Pairwise correlation
                  </h3>
                  <BarChart
                    data={Object.entries(rel.correlation ?? {})
                      .sort((a, b) => Math.abs(b[1]) - Math.abs(a[1]))
                      .slice(0, 12)
                      .map(([pair, rho]) => ({
                        label: pair,
                        value: rho,
                        note:
                          rel.correlation_change?.[pair] !== undefined
                            ? `Δ ${formatNumber(rel.correlation_change[pair] ?? 0, 2)}`
                            : undefined,
                      }))}
                    format={(v) => formatNumber(v, 3)}
                    emptyMessage="No correlations were computed."
                  />
                </div>

                <div>
                  <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                    Divergences
                  </h3>
                  {list(rel.divergences).length === 0 ? (
                    <p className="text-xs text-ink-500">
                      No divergences detected: every tracked pair is behaving
                      roughly as its history would suggest.
                    </p>
                  ) : (
                    <ul className="space-y-2">
                      {list(rel.divergences).map((d, i) => (
                        <li key={i} className="rounded border border-watch/50 bg-amber-50 p-2.5">
                          <p className="text-sm font-semibold text-ink-900">
                            {d.a} vs {d.b}
                          </p>
                          <p className="num text-xs text-ink-700">
                            expected {formatNumber(d.expected_correlation, 2)} · observed{' '}
                            {formatNumber(d.observed_correlation, 2)} · magnitude{' '}
                            {formatNumber(d.magnitude, 2)}
                          </p>
                          <p className="mt-0.5 text-xs text-ink-700">{d.note}</p>
                        </li>
                      ))}
                    </ul>
                  )}
                </div>
              </div>

              <div>
                <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                  Sector performance
                </h3>
                {list(rel.sectors).length === 0 ? (
                  <EmptyState title="No sector performance was computed." />
                ) : (
                  <div className="table-wrap">
                    <table className="data-table">
                      <caption className="sr-only">
                        Sector returns and relative strength, ranked.
                      </caption>
                      <thead>
                        <tr>
                          <th scope="col">Rank</th>
                          <th scope="col">Sector</th>
                          <th scope="col">ETF</th>
                          <th scope="col">1D</th>
                          <th scope="col">5D</th>
                          <th scope="col">20D</th>
                          <th scope="col">Rel. strength</th>
                          <th scope="col">Breadth</th>
                        </tr>
                      </thead>
                      <tbody>
                        {list(rel.sectors)
                          .slice()
                          .sort((a, b) => a.rank - b.rank)
                          .map((s) => (
                            <tr key={s.sector}>
                              <td className="num">{s.rank}</td>
                              <th scope="row" className="font-medium">
                                {s.sector}
                              </th>
                              <td className="num text-xs">{s.etf || '—'}</td>
                              <td className="num">{formatSignedPercent(s.return_1d)}</td>
                              <td className="num">{formatSignedPercent(s.return_5d)}</td>
                              <td className="num">{formatSignedPercent(s.return_20d)}</td>
                              <td className="num">{formatNumber(s.rel_strength, 3)}</td>
                              <td className="num">{formatNumber(s.breadth, 2)}</td>
                            </tr>
                          ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {list(rel.notes).length > 0 ? (
                <ul className="space-y-0.5 text-xs text-ink-700">
                  {list(rel.notes).map((n, i) => (
                    <li key={i}>· {n}</li>
                  ))}
                </ul>
              ) : null}
            </div>
          )}
        </Card>
      </div>
    </>
  );
}

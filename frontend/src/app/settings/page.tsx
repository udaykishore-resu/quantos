import type { Metadata } from 'next';

import { Card, DefinitionList, Freshness, Hash, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import { Chip } from '@/components/verdicts';
import { formatCount, formatNumber, formatTimestamp, humanizeEnum, list } from '@/lib/format';
import { api, serverEnv } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import type { Strategy } from '@/lib/types';

export const metadata: Metadata = { title: 'Settings' };
export const dynamic = 'force-dynamic';

/**
 * Settings is read-only by design.
 *
 * The effective configuration is hashed at load time and that hash is stamped on
 * every score, risk assessment and signal, so a historical decision can be
 * re-derived after the configuration changes. Letting a dashboard mutate it
 * would break that chain silently. This page therefore shows what is in force
 * and where it came from, and offers no way to change it.
 */
export default async function SettingsPage() {
  const env = serverEnv();
  const [platform, strategiesR, auditR] = await Promise.all([
    platformStatus(),
    api().strategies(),
    api().audit({ limit: 50 }),
  ]);

  const strategies = strategiesR.ok ? list(strategiesR.data) : [];
  const audit = auditR.ok ? list(auditR.data) : [];
  const health = platform.health;

  return (
    <>
      <PageHeader
        title="Settings"
        purpose="The configuration actually in force, and the hashes that tie it to every decision the platform has recorded. This page is read-only: changing configuration here would break the link between a stored decision and the settings that produced it."
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />

        <Card title="Platform">
          {!health ? (
            <ErrorState
              error={{
                ok: false,
                code: 'unavailable',
                message: platform.unreachableReason || 'the health endpoint could not be read',
                status: 0,
                retryable: true,
              }}
              what="platform health"
            />
          ) : (
            <>
              <DefinitionList
                columns={3}
                items={[
                  { term: 'Status', value: health.status },
                  { term: 'Version', value: <span className="font-mono">{health.version}</span> },
                  { term: 'Mode', value: <span className="font-mono">{health.mode}</span>, hint: 'embedded = in-process bus and stores; compose = local Kafka/Postgres; cluster = managed services.' },
                  { term: 'Uptime', value: <span className="num">{health.uptime}</span> },
                  { term: 'Stream clients', value: formatCount(health.stream_clients) },
                  { term: 'Reported at', value: formatTimestamp(health.timestamp) },
                ]}
              />

              <div className="mt-4">
                <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                  Backing stores
                </h3>
                {!health.store || Object.keys(health.store).length === 0 ? (
                  <EmptyState title="No store health was reported." />
                ) : (
                  <div className="table-wrap">
                    <table className="data-table">
                      <caption className="sr-only">
                        Each backing store, whether it is enabled and whether it is
                        healthy.
                      </caption>
                      <thead>
                        <tr>
                          <th scope="col">Store</th>
                          <th scope="col">Enabled</th>
                          <th scope="col">Healthy</th>
                          <th scope="col">Detail</th>
                        </tr>
                      </thead>
                      <tbody>
                        {Object.entries(health.store).map(([name, s]) => (
                          <tr key={name}>
                            <th scope="row" className="font-mono text-xs">
                              {name}
                            </th>
                            <td className="text-xs">
                              {s.enabled ? (
                                <span className="text-allow">
                                  <span aria-hidden="true">✓ </span>enabled
                                </span>
                              ) : (
                                <span className="text-ink-500">
                                  <span aria-hidden="true">– </span>not configured
                                </span>
                              )}
                            </td>
                            <td className="text-xs">
                              {!s.enabled ? (
                                <span className="text-ink-400">n/a</span>
                              ) : s.healthy ? (
                                <span className="text-allow">
                                  <span aria-hidden="true">✓ </span>healthy
                                </span>
                              ) : (
                                <span className="font-semibold text-deny">
                                  <span aria-hidden="true">✕ </span>unhealthy
                                </span>
                              )}
                            </td>
                            <td className="text-xs text-ink-600">
                              {typeof s.error === 'string' && s.error ? s.error : '—'}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                    <p className="mt-2 max-w-prose text-[11px] leading-relaxed text-ink-500">
                      A store that is not configured is not a failure. It does mean
                      the endpoints backed by it — alerts, candles, historical
                      signals, backtest runs, audit — will report themselves
                      unavailable rather than returning an empty result that looks
                      like &ldquo;nothing happened&rdquo;.
                    </p>
                  </div>
                )}
              </div>
            </>
          )}
        </Card>

        <Card
          title="Dashboard configuration"
          subtitle="How this dashboard is reaching the platform. No credential is shown, because none is held in the browser."
        >
          <DefinitionList
            columns={2}
            items={[
              {
                term: 'API origin',
                value: <span className="font-mono text-xs">{env.useMocks ? 'mocks/ (no backend)' : env.apiOrigin}</span>,
              },
              {
                term: 'Data source',
                value: env.useMocks ? 'Fixtures from frontend/mocks/' : 'Live QuantOS API',
              },
              {
                term: 'Credential location',
                value: 'Next.js server process only',
                hint: 'The browser calls this app’s same-origin proxy and never receives a bearer token. The SSE stream is proxied for the same reason: EventSource cannot set headers, and putting a token in a query string would put it in the browser.',
              },
              {
                term: 'Server-side revalidation',
                value: `${env.revalidateSeconds}s`,
              },
            ]}
          />
        </Card>

        <Card
          title="Strategy files"
          subtitle="No strategy logic lives in Go source. Each file below is hashed, and that hash is stamped on every score and signal it produced."
          actions={<Freshness servedAt={strategiesR.ok ? strategiesR.servedAt : undefined} meta={strategiesR.ok ? strategiesR.meta : undefined} />}
        >
          {!strategiesR.ok ? (
            <ErrorState error={strategiesR} what="strategy files" />
          ) : strategies.length === 0 ? (
            <EmptyState title="No strategy files are loaded." />
          ) : (
            <ul className="space-y-4">
              {strategies.map((s) => (
                <StrategyCard key={s.id} strategy={s} />
              ))}
            </ul>
          )}
        </Card>

        <Card
          title="Audit log"
          subtitle="State-changing actions, with the principal that performed each one."
          actions={<Freshness servedAt={auditR.ok ? auditR.servedAt : undefined} meta={auditR.ok ? auditR.meta : undefined} />}
        >
          {!auditR.ok ? (
            <>
              <ErrorState error={auditR} what="the audit log" />
              {auditR.status === 403 ? (
                <p className="mt-2 text-xs text-ink-600">
                  The dashboard&apos;s principal does not hold the{' '}
                  <span className="font-mono">audit:read</span> scope. That scope
                  belongs to the operator and admin roles.
                </p>
              ) : null}
            </>
          ) : audit.length === 0 ? (
            <EmptyState
              title="No audit events"
              detail="Audit records are persisted. With the in-memory store configured, nothing accumulates across restarts."
            />
          ) : (
            <div className="table-wrap max-h-[28rem] overflow-y-auto">
              <table className="data-table">
                <caption className="sr-only">Recorded audit events, newest first.</caption>
                <thead className="sticky top-0 bg-white">
                  <tr>
                    <th scope="col">At</th>
                    <th scope="col">Principal</th>
                    <th scope="col">Action</th>
                    <th scope="col">Resource</th>
                    <th scope="col">Outcome</th>
                    <th scope="col">Request</th>
                  </tr>
                </thead>
                <tbody>
                  {audit.map((a) => (
                    <tr key={a.id}>
                      <td className="num whitespace-nowrap text-xs">
                        {formatTimestamp(a.at)}
                      </td>
                      <td className="font-mono text-xs">{a.principal}</td>
                      <td className="font-mono text-xs">{a.action}</td>
                      <td className="max-w-[16rem] truncate font-mono text-xs" title={a.resource}>
                        {a.resource}
                      </td>
                      <td className="text-xs">
                        <span
                          className={
                            a.outcome === 'success' || a.outcome === 'allowed'
                              ? 'text-allow'
                              : 'text-deny'
                          }
                        >
                          <span aria-hidden="true">
                            {a.outcome === 'success' || a.outcome === 'allowed' ? '✓ ' : '✕ '}
                          </span>
                          {a.outcome}
                        </span>
                      </td>
                      <td className="font-mono text-[10px] text-ink-500">
                        {a.request_id || '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </>
  );
}

function StrategyCard({ strategy: s }: { strategy: Strategy }) {
  const rules = list(s.rules);
  const counter = rules.filter((r) => r.counter);

  return (
    <li className="rounded border border-ink-200 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-semibold text-ink-900">{s.name}</span>
        <Chip label="id" value={s.id} />
        <Chip label="version" value={s.version} />
        <span
          className={`rounded border px-1.5 py-0.5 text-[10px] font-bold uppercase ${
            s.enabled
              ? 'border-allow bg-teal-50 text-allow'
              : 'border-ink-300 bg-ink-50 text-ink-500'
          }`}
        >
          <span aria-hidden="true">{s.enabled ? '✓ ' : '– '}</span>
          {s.enabled ? 'enabled' : 'disabled'}
        </span>
      </div>

      <p className="mt-1 text-xs text-ink-600">{s.description}</p>

      <div className="mt-2 flex flex-wrap gap-1.5">
        <Chip label="hash" value={<Hash value={s.hash} chars={16} />} title={s.hash} />
        <Chip label="source" value={s.source_path || '—'} />
        <Chip label="modified" value={formatTimestamp(s.modified_at)} />
        <Chip label="interval" value={s.interval} />
        <Chip label="rules" value={`${rules.length} (${counter.length} counter)`} />
        {list(s.regimes).length > 0 ? (
          <Chip label="regimes" value={list(s.regimes).map(humanizeEnum).join(', ')} mono={false} />
        ) : null}
      </div>

      <details className="mt-2 rounded border border-ink-200">
        <summary className="cursor-pointer px-3 py-1.5 text-xs font-semibold text-ink-700">
          Signal policy, weights and invalidation templates
        </summary>
        <div className="space-y-3 border-t border-ink-200 p-3">
          <div>
            <h4 className="mb-1 text-[11px] font-bold uppercase tracking-wide text-ink-500">
              Signal policy — every gate a candidate must clear
            </h4>
            <DefinitionList
              columns={3}
              items={[
                { term: 'Min rule score', value: formatNumber(s.policy.min_rule_score, 2) },
                { term: 'Min final score', value: formatNumber(s.policy.min_final_score, 2) },
                { term: 'Min confidence', value: formatNumber(s.policy.min_confidence, 3) },
                { term: 'Min directional edge', value: formatNumber(s.policy.min_directional_edge, 3) },
                { term: 'Min risk / reward', value: formatNumber(s.policy.min_risk_reward, 2) },
                { term: 'Horizon', value: s.policy.horizon },
                { term: 'Stop ATR multiple', value: formatNumber(s.policy.stop_atr_multiple, 2) },
                { term: 'Target ATR multiple', value: formatNumber(s.policy.target_atr_multiple, 2) },
                { term: 'Max weight', value: formatNumber(s.policy.max_weight, 3) },
                { term: 'Cooldown', value: `${s.policy.cooldown_minutes} min` },
                { term: 'Shorts allowed', value: s.policy.allow_short ? 'yes' : 'no' },
              ]}
            />
          </div>

          <div>
            <h4 className="mb-1 text-[11px] font-bold uppercase tracking-wide text-ink-500">
              Score weights
            </h4>
            <ul className="flex flex-wrap gap-1.5">
              {Object.entries(s.weights).map(([k, v]) => (
                <li key={k} className="rounded border border-ink-200 bg-ink-50 px-2 py-0.5 text-xs">
                  {k.replace(/_/g, ' ')}: <span className="num font-medium">{formatNumber(v, 2)}</span>
                </li>
              ))}
            </ul>
          </div>

          <div>
            <h4 className="mb-1 text-[11px] font-bold uppercase tracking-wide text-ink-500">
              Invalidation templates ({list(s.invalidation).length})
            </h4>
            {list(s.invalidation).length === 0 ? (
              <p className="text-xs text-deny">
                <span aria-hidden="true">✕ </span>
                No invalidation templates. A signal cannot be constructed without at
                least one condition, so this strategy can never emit.
              </p>
            ) : (
              <ul className="space-y-1">
                {list(s.invalidation).map((t, i) => (
                  <li key={i} className="text-xs text-ink-700">
                    <span className="rounded bg-ink-900 px-1 py-0.5 font-mono text-[10px] font-bold text-white">
                      {t.kind}
                    </span>{' '}
                    {t.description}
                    {t.ref ? <span className="text-ink-500"> (ref {t.ref})</span> : null}
                  </li>
                ))}
              </ul>
            )}
          </div>

          <div>
            <h4 className="mb-1 text-[11px] font-bold uppercase tracking-wide text-ink-500">
              Rules ({rules.length})
            </h4>
            <div className="table-wrap max-h-72 overflow-y-auto">
              <table className="data-table">
                <caption className="sr-only">
                  Every rule in this strategy, with its side, weight and whether it
                  is counter-evidence.
                </caption>
                <thead className="sticky top-0 bg-white">
                  <tr>
                    <th scope="col">Rule</th>
                    <th scope="col">Side</th>
                    <th scope="col">Weight</th>
                    <th scope="col">Kind</th>
                    <th scope="col">Regimes</th>
                    <th scope="col">Description</th>
                  </tr>
                </thead>
                <tbody>
                  {rules.map((r) => (
                    <tr key={r.id} className={r.counter ? 'bg-amber-50/50' : undefined}>
                      <th scope="row" className="font-mono text-xs font-normal">
                        {r.id}
                      </th>
                      <td className="text-xs">{r.side}</td>
                      <td className="num text-xs">{formatNumber(r.weight, 2)}</td>
                      <td className="text-xs">
                        {r.counter ? (
                          <span className="font-semibold text-watch">
                            <span aria-hidden="true">! </span>counter-evidence
                          </span>
                        ) : (
                          <span className="text-ink-600">supporting</span>
                        )}
                      </td>
                      <td className="text-[11px] text-ink-600">
                        {list(r.regimes).length === 0
                          ? 'any'
                          : list(r.regimes).map(humanizeEnum).join(', ')}
                      </td>
                      <td className="max-w-[22rem] text-xs leading-snug text-ink-700">
                        {r.description}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </details>
    </li>
  );
}

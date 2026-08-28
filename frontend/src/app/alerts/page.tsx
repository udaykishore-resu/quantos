import type { Metadata } from 'next';
import Link from 'next/link';

import { DisclaimerBanner } from '@/components/disclaimer';
import { Card, Freshness, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import { formatAge, formatNumber, formatTimestamp, humanizeEnum, list } from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import type { Alert, AlertSeverity } from '@/lib/types';

export const metadata: Metadata = { title: 'Alerts' };
export const dynamic = 'force-dynamic';

const SEVERITY_TONE: Record<AlertSeverity, string> = {
  CRITICAL: 'border-deny bg-red-50 text-deny',
  WARNING: 'border-watch bg-amber-50 text-watch',
  NOTICE: 'border-ink-400 bg-ink-50 text-ink-700',
  INFO: 'border-ink-300 bg-white text-ink-600',
};

const SEVERITY_GLYPH: Record<AlertSeverity, string> = {
  CRITICAL: '✕',
  WARNING: '!',
  NOTICE: '·',
  INFO: 'i',
};

export default async function AlertsPage({
  searchParams,
}: {
  searchParams: Promise<{ ticker?: string }>;
}) {
  const sp = await searchParams;
  const ticker = typeof sp.ticker === 'string' ? sp.ticker.toUpperCase() : '';

  const [platform, result] = await Promise.all([
    platformStatus(),
    api().alerts({ ticker, limit: 200 }),
  ]);
  const alerts = result.ok ? list(result.data) : [];

  const counts = alerts.reduce<Record<string, number>>((acc, a) => {
    acc[a.severity] = (acc[a.severity] ?? 0) + 1;
    return acc;
  }, {});

  return (
    <>
      <PageHeader
        title="Alerts"
        purpose="Every notification the platform has emitted, with the structured before-and-after values behind it. The dashboard renders from those values, never by parsing the message text, so what is displayed is always the authoritative number."
        actions={
          ticker ? (
            <Link href="/alerts" className="btn">
              Clear {ticker} filter
            </Link>
          ) : undefined
        }
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />
        <DisclaimerBanner />

        {alerts.length > 0 ? (
          <div className="flex flex-wrap gap-2">
            {(['CRITICAL', 'WARNING', 'NOTICE', 'INFO'] as AlertSeverity[]).map((s) =>
              counts[s] ? (
                <span
                  key={s}
                  className={`inline-flex items-center gap-1.5 rounded border px-2 py-1 text-xs font-semibold ${SEVERITY_TONE[s]}`}
                >
                  <span aria-hidden="true" className="font-mono">
                    {SEVERITY_GLYPH[s]}
                  </span>
                  {counts[s]} {s.toLowerCase()}
                </span>
              ) : null,
            )}
          </div>
        ) : null}

        <Card
          title={ticker ? `Alerts for ${ticker}` : 'All alerts'}
          actions={<Freshness servedAt={result.ok ? result.servedAt : undefined} meta={result.ok ? result.meta : undefined} />}
        >
          {!result.ok ? (
            <>
              <ErrorState error={result} what="alerts" />
              {result.code === 'unavailable' ? (
                <p className="mt-2 max-w-prose text-xs leading-relaxed text-ink-600">
                  Alerts are read from the persistent store. With the in-memory
                  store configured this endpoint is unavailable — that is a
                  deployment fact, not a fault. See Settings for the effective
                  configuration.
                </p>
              ) : null}
            </>
          ) : alerts.length === 0 ? (
            <EmptyState
              title="No alerts have been raised"
              detail="Alerts are persisted with a deduplication key so redelivery and replay are idempotent. An empty list means none have been emitted, or that the platform is running without a persistent store."
            />
          ) : (
            <ul className="space-y-2">
              {alerts.map((a) => (
                <AlertRow key={a.id} alert={a} />
              ))}
            </ul>
          )}
        </Card>
      </div>
    </>
  );
}

function AlertRow({ alert: a }: { alert: Alert }) {
  const before = a.before ?? {};
  const after = a.after ?? {};
  const keys = [...new Set([...Object.keys(before), ...Object.keys(after)])].sort();

  return (
    <li className="rounded border border-ink-200 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span
          className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[10px] font-bold uppercase ${
            SEVERITY_TONE[a.severity] ?? SEVERITY_TONE.INFO
          }`}
        >
          <span aria-hidden="true" className="font-mono">
            {SEVERITY_GLYPH[a.severity] ?? 'i'}
          </span>
          {a.severity}
        </span>
        <span className="rounded bg-ink-900 px-1.5 py-0.5 font-mono text-[10px] font-bold text-white">
          {a.type}
        </span>
        <span className="text-sm font-semibold text-ink-900">{a.title}</span>
        {a.ticker ? (
          <Link href={`/stock/${encodeURIComponent(a.ticker)}`} className="link text-xs">
            {a.ticker}
          </Link>
        ) : null}
        {a.status ? (
          <span className="rounded border border-ink-300 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-ink-600">
            {a.status}
          </span>
        ) : null}
        <span className="num ml-auto whitespace-nowrap text-[11px] text-ink-500">
          {formatTimestamp(a.created_at)} · {formatAge(a.created_at)}
        </span>
      </div>

      <p className="mt-1 text-sm leading-snug text-ink-700">{a.message}</p>

      {keys.length > 0 ? (
        <div className="table-wrap mt-2">
          <table className="w-full min-w-0 border-collapse text-xs">
            <caption className="sr-only">
              Structured values behind this alert, before and after.
            </caption>
            <thead>
              <tr>
                <th scope="col" className="px-2 py-1 text-left text-[10px] uppercase text-ink-500">
                  Value
                </th>
                <th scope="col" className="px-2 py-1 text-right text-[10px] uppercase text-ink-500">
                  Before
                </th>
                <th scope="col" className="px-2 py-1 text-right text-[10px] uppercase text-ink-500">
                  After
                </th>
                <th scope="col" className="px-2 py-1 text-right text-[10px] uppercase text-ink-500">
                  Change
                </th>
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => {
                const b = before[k];
                const af = after[k];
                const delta =
                  typeof b === 'number' && typeof af === 'number' ? af - b : undefined;
                return (
                  <tr key={k} className="border-t border-ink-100">
                    <th scope="row" className="px-2 py-1 text-left font-mono font-normal text-ink-700">
                      {k}
                    </th>
                    <td className="num px-2 py-1 text-right">
                      {b === undefined ? '—' : formatNumber(b, 4)}
                    </td>
                    <td className="num px-2 py-1 text-right font-medium">
                      {af === undefined ? '—' : formatNumber(af, 4)}
                    </td>
                    <td className="num px-2 py-1 text-right text-ink-600">
                      {delta === undefined
                        ? '—'
                        : `${delta > 0 ? '+' : ''}${formatNumber(delta, 4)}`}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      <p className="mt-1.5 flex flex-wrap gap-x-3 text-[11px] text-ink-500">
        {a.regime ? <span>regime {humanizeEnum(a.regime)}</span> : null}
        {a.risk_level ? <span>risk {a.risk_level}</span> : null}
        {a.signal_id ? (
          <Link href={`/signals/${encodeURIComponent(a.signal_id)}`} className="link">
            signal {a.signal_id.slice(0, 8)} →
          </Link>
        ) : null}
        <span className="font-mono">dedup {a.dedup_key}</span>
      </p>
    </li>
  );
}

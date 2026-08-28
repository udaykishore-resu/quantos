import type { Metadata } from 'next';
import Link from 'next/link';

import { DisclaimerBanner } from '@/components/disclaimer';
import { Card, Freshness, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import { SideBadge, SignalStatusBadge } from '@/components/verdicts';
import {
  formatAge,
  formatPercent,
  formatPrice,
  formatScore,
  formatTimestamp,
  list,
} from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import type { SignalStatus } from '@/lib/types';

export const metadata: Metadata = { title: 'Signals' };
export const dynamic = 'force-dynamic';

const FILTERS: { label: string; value: SignalStatus | '' }[] = [
  { label: 'Active', value: 'ACTIVE' },
  { label: 'Invalidated', value: 'INVALIDATED' },
  { label: 'Expired', value: 'EXPIRED' },
  { label: 'Completed', value: 'COMPLETED' },
  { label: 'All', value: '' },
];

export default async function SignalsPage({
  searchParams,
}: {
  searchParams: Promise<{ status?: string; ticker?: string }>;
}) {
  const sp = await searchParams;
  const status = (FILTERS.find((f) => f.value === sp.status)?.value ??
    'ACTIVE') as SignalStatus | '';
  const ticker = typeof sp.ticker === 'string' ? sp.ticker : '';

  const [platform, result] = await Promise.all([
    platformStatus(),
    api().signals({ status, ticker, limit: 100 }),
  ]);
  const signals = result.ok ? list(result.data) : [];

  return (
    <>
      <PageHeader
        title="Signals"
        purpose="Every paper-trading signal the platform has emitted, and the complete causal record behind each one. A signal is a research artifact: it says what the platform observed and what would prove it wrong, not what anyone should do."
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />
        <DisclaimerBanner />

        <nav aria-label="Filter signals by status" className="no-print flex flex-wrap gap-2">
          {FILTERS.map((f) => {
            const active = f.value === status;
            const href = f.value ? `/signals?status=${f.value}` : '/signals?status=';
            return (
              <Link
                key={f.label}
                href={href}
                aria-current={active ? 'true' : undefined}
                className={`rounded border px-3 py-1 text-sm font-medium ${
                  active
                    ? 'border-ink-900 bg-ink-900 text-white'
                    : 'border-ink-300 bg-white text-ink-700 hover:bg-ink-50'
                }`}
              >
                {f.label}
              </Link>
            );
          })}
        </nav>

        <Card
          title={`${status || 'All'} signals`}
          subtitle="Follow any row into its provenance record."
          actions={<Freshness servedAt={result.ok ? result.servedAt : undefined} meta={result.ok ? result.meta : undefined} />}
        >
          {!result.ok ? (
            <ErrorState error={result} what="the signal list" />
          ) : signals.length === 0 ? (
            <EmptyState
              title={`No ${status ? status.toLowerCase() : ''} signals`}
              detail={
                status === 'ACTIVE'
                  ? 'The platform emits a signal only when the risk engine returns ALLOW_PAPER_SIGNAL and at least one machine-checkable invalidation condition can be attached. An empty list is a decision, not a fault: the ranked instrument list records why each candidate was declined.'
                  : 'No signals with this status are stored. Historical statuses are read from the persistent store; with the in-memory store configured only active signals are available.'
              }
              action={
                <Link href="/stocks" className="btn">
                  See the rejection reason for each instrument
                </Link>
              }
            />
          ) : (
            <div className="table-wrap">
              <table className="data-table">
                <caption className="sr-only">
                  Signals with status {status || 'any'}, newest first.
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Ticker</th>
                    <th scope="col">Side</th>
                    <th scope="col">Status</th>
                    <th scope="col">Strength</th>
                    <th scope="col">Confidence</th>
                    <th scope="col">Horizon</th>
                    <th scope="col">Reference levels</th>
                    <th scope="col">Created</th>
                    <th scope="col">Provenance</th>
                  </tr>
                </thead>
                <tbody>
                  {signals.map((s) => (
                    <tr key={s.id}>
                      <td>
                        <Link href={`/stock/${encodeURIComponent(s.ticker)}`} className="link font-semibold">
                          {s.ticker}
                        </Link>
                      </td>
                      <td>
                        <SideBadge side={s.side} />
                      </td>
                      <td>
                        <SignalStatusBadge status={s.status} />
                        {s.invalidation_kind ? (
                          <span className="mt-0.5 block text-[11px] text-ink-500">
                            {s.invalidation_kind}
                          </span>
                        ) : null}
                      </td>
                      <td className="num">{formatScore(s.strength)}</td>
                      <td className="num">{formatPercent(s.confidence)}</td>
                      <td className="num text-xs">{s.horizon}</td>
                      <td className="num whitespace-nowrap text-xs">
                        <span className="block">entry {formatPrice(s.entry_reference)}</span>
                        <span className="block text-ink-600">
                          stop {formatPrice(s.stop_reference)} · target{' '}
                          {formatPrice(s.target_reference)}
                        </span>
                      </td>
                      <td className="whitespace-nowrap text-xs text-ink-600">
                        {formatTimestamp(s.created_at)}
                        <span className="block text-ink-400">{formatAge(s.created_at)}</span>
                      </td>
                      <td>
                        <Link href={`/signals/${encodeURIComponent(s.id)}`} className="link text-xs">
                          Why this signal? →
                        </Link>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        {signals.length > 0 ? (
          <Card title="Probability behind each signal" subtitle="The distribution the signal was drawn from, not a directional call.">
            <ul className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {signals.slice(0, 9).map((s) => (
                <li key={s.id} className="rounded border border-ink-200 p-3">
                  <div className="mb-2 flex items-center gap-2">
                    <Link href={`/signals/${encodeURIComponent(s.id)}`} className="link font-semibold">
                      {s.ticker}
                    </Link>
                    <SideBadge side={s.side} />
                  </div>
                  <p className="mb-1 text-[11px] text-ink-500">
                    Model {s.model_version || 'unrecorded'} · strategy {s.strategy_id || 'unrecorded'}
                  </p>
                  <p className="text-xs text-ink-700">
                    {list(s.evidence).length} supporting,{' '}
                    {list(s.counter_evidence).length} counter-evidence statements
                  </p>
                </li>
              ))}
            </ul>
          </Card>
        ) : null}
      </div>
    </>
  );
}

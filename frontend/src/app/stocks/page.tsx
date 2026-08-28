import type { Metadata } from 'next';
import Link from 'next/link';

import { DisclaimerBanner } from '@/components/disclaimer';
import { DistributionBar } from '@/components/distribution';
import { Card, Freshness, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import {
  ClassificationBadge,
  RiskDecisionBadge,
  RiskLevelBadge,
  SideBadge,
} from '@/components/verdicts';
import { formatAge, formatPercent, formatPrice, formatScore, list } from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';

export const metadata: Metadata = { title: 'Stocks' };
export const dynamic = 'force-dynamic';

export default async function StocksPage({
  searchParams,
}: {
  searchParams: Promise<{ sector?: string }>;
}) {
  const sp = await searchParams;
  const sector = typeof sp.sector === 'string' ? sp.sector : '';

  const [platform, result] = await Promise.all([
    platformStatus(),
    api().stocks({ limit: 200, sector }),
  ]);
  const rows = result.ok ? list(result.data) : [];
  const sectors = [...new Set(rows.map((r) => r.sector).filter(Boolean))].sort();
  const staleCount = rows.filter((r) => r.stale).length;
  const rejected = rows.filter((r) => r.rejection);

  return (
    <>
      <PageHeader
        title="Stocks"
        purpose="Every instrument the platform is tracking, ranked by opportunity score — and, for each one, the reason it did or did not become a signal."
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />
        <DisclaimerBanner />

        {sectors.length > 1 ? (
          <nav aria-label="Filter by sector" className="no-print flex flex-wrap gap-1.5">
            <Link
              href="/stocks"
              aria-current={!sector ? 'true' : undefined}
              className={`rounded border px-2.5 py-1 text-xs font-medium ${
                !sector
                  ? 'border-ink-900 bg-ink-900 text-white'
                  : 'border-ink-300 bg-white text-ink-700 hover:bg-ink-50'
              }`}
            >
              All sectors
            </Link>
            {sectors.map((s) => (
              <Link
                key={s}
                href={`/stocks?sector=${encodeURIComponent(s)}`}
                aria-current={sector === s ? 'true' : undefined}
                className={`rounded border px-2.5 py-1 text-xs font-medium ${
                  sector === s
                    ? 'border-ink-900 bg-ink-900 text-white'
                    : 'border-ink-300 bg-white text-ink-700 hover:bg-ink-50'
                }`}
              >
                {s}
              </Link>
            ))}
          </nav>
        ) : null}

        <Card
          title={sector ? `${sector} — ranked instruments` : 'Ranked instruments'}
          subtitle={
            rows.length > 0
              ? `${rows.length} scored${staleCount > 0 ? `, ${staleCount} on stale data` : ''}${
                  rejected.length > 0
                    ? `, ${rejected.length} with a recorded reason for not emitting a signal`
                    : ''
                }.`
              : undefined
          }
          actions={<Freshness servedAt={result.ok ? result.servedAt : undefined} meta={result.ok ? result.meta : undefined} />}
        >
          {!result.ok ? (
            <ErrorState error={result} what="the ranked instrument list" />
          ) : rows.length === 0 ? (
            <EmptyState
              title="No instruments have been scored"
              detail={
                sector
                  ? `No instrument in the ${sector} sector has a score yet.`
                  : 'The scoring engine produces a ranking once feature snapshots are warm. Immediately after start-up this is expected while the warm-up completes.'
              }
            />
          ) : (
            <div className="table-wrap">
              <table className="data-table min-w-[64rem]">
                <caption className="sr-only">
                  Instruments ranked by opportunity score, highest first. Ties break
                  on ticker so the ordering is stable between refreshes.
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Ticker</th>
                    <th scope="col">Sector</th>
                    <th scope="col">Last</th>
                    <th scope="col">Score</th>
                    <th scope="col">Opportunity</th>
                    <th scope="col">Classification</th>
                    <th scope="col">Bias</th>
                    <th scope="col">Confidence</th>
                    <th scope="col">Probability</th>
                    <th scope="col">Risk</th>
                    <th scope="col">Why no signal?</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <tr key={r.ticker}>
                      <th scope="row" className="whitespace-nowrap">
                        <Link href={`/stock/${encodeURIComponent(r.ticker)}`} className="link font-semibold">
                          {r.ticker}
                        </Link>
                        <span className="block max-w-[11rem] truncate text-[11px] font-normal text-ink-500">
                          {r.company}
                        </span>
                        {r.stale ? (
                          <span className="text-[10px] font-semibold text-watch">
                            <span aria-hidden="true">! </span>stale
                          </span>
                        ) : null}
                      </th>
                      <td className="text-xs text-ink-600">{r.sector || '—'}</td>
                      <td className="num">{formatPrice(r.last)}</td>
                      <td className="num">{formatScore(r.score)}</td>
                      <td className="num font-semibold">{formatScore(r.opportunity)}</td>
                      <td>
                        <ClassificationBadge classification={r.classification} />
                      </td>
                      <td>
                        <SideBadge side={r.bias} />
                      </td>
                      <td className="num">{formatPercent(r.confidence)}</td>
                      <td className="min-w-[10rem]">
                        <DistributionBar dist={r.probability} compact />
                      </td>
                      <td className="space-y-1">
                        <RiskLevelBadge level={r.risk} />
                        <RiskDecisionBadge decision={r.risk_decision} />
                      </td>
                      <td className="max-w-[18rem] text-xs leading-snug text-ink-600">
                        {r.rejection ? (
                          r.rejection
                        ) : r.risk_decision === 'ALLOW_PAPER_SIGNAL' ? (
                          <span className="text-allow">
                            <span aria-hidden="true">✓ </span>Eligible — risk allows a
                            paper signal
                          </span>
                        ) : (
                          <span className="text-ink-400">no reason recorded</span>
                        )}
                        <span className="mt-0.5 block text-[10px] text-ink-400">
                          {formatAge(r.updated_at)}
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card
          title="Reading this table"
          subtitle="What the columns do and do not claim."
        >
          <ul className="max-w-prose space-y-1.5 text-sm leading-relaxed text-ink-700">
            <li>
              <strong>Opportunity</strong> fuses the composite score, the
              prediction and the environment. It ranks candidates for research
              attention. It is not a ranking of expected profit.
            </li>
            <li>
              <strong>Classification</strong> is a research label from a fixed
              vocabulary. <span className="font-mono">BULLISH_SETUP</span> means the
              configured rules for that pattern matched — it is not a
              recommendation to buy anything.
            </li>
            <li>
              <strong>Probability</strong> is a distribution over three outcomes for
              the shortest predicted horizon. Every one of the three may occur.
            </li>
            <li>
              <strong>Risk decision</strong> is what is enforced.{' '}
              <span className="font-mono">WATCH_ONLY</span> and{' '}
              <span className="font-mono">BLOCK</span> both prevent a signal; only{' '}
              <span className="font-mono">ALLOW_PAPER_SIGNAL</span> permits one.
            </li>
            <li>
              <strong>Why no signal?</strong> carries the pipeline&apos;s own
              rejection reason. An empty signal list elsewhere in the dashboard is
              explained here, stage by stage.
            </li>
          </ul>
        </Card>
      </div>
    </>
  );
}

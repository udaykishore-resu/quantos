import type { Metadata } from 'next';
import Link from 'next/link';

import { Card, Freshness, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import { formatAge, formatNumber, formatPercent, formatTimestamp, list } from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import type { NewsEvent } from '@/lib/types';

export const metadata: Metadata = { title: 'News' };
export const dynamic = 'force-dynamic';

/** Mirrors `NewsEvent.Material()` in internal/domain/news.go. */
function isMaterial(n: NewsEvent): boolean {
  return n.materiality === 'high' || (n.materiality === 'medium' && n.confidence >= 0.7);
}

export default async function NewsPage({
  searchParams,
}: {
  searchParams: Promise<{ ticker?: string }>;
}) {
  const sp = await searchParams;
  const ticker = typeof sp.ticker === 'string' ? sp.ticker.toUpperCase() : '';

  const [platform, result] = await Promise.all([
    platformStatus(),
    api().news({ ticker, limit: 100 }),
  ]);
  const news = result.ok ? list(result.data) : [];
  const material = news.filter(isMaterial);

  return (
    <>
      <PageHeader
        title="News"
        purpose="Processed news events. Only the typed fields below ever cross into the decision path — the raw article text never does, and a headline reaches a score only through the single signed-impact number shown on each row."
        actions={
          ticker ? (
            <Link href="/news" className="btn">
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

        <Card
          title={ticker ? `News for ${ticker}` : 'Recent news'}
          subtitle={
            news.length > 0
              ? `${news.length} events, ${material.length} of which clear the materiality bar for affecting decisions.`
              : undefined
          }
          actions={<Freshness servedAt={result.ok ? result.servedAt : undefined} meta={result.ok ? result.meta : undefined} />}
        >
          {!result.ok ? (
            <ErrorState error={result} what="news" />
          ) : news.length === 0 ? (
            <EmptyState
              title="No news events"
              detail="The news pipeline classifies each item deterministically before any language model is involved. An empty list means no items have been ingested — not that classification failed."
            />
          ) : (
            <ul className="space-y-3">
              {news.map((n) => (
                <li
                  key={n.id}
                  className={`rounded border p-3 ${
                    isMaterial(n) ? 'border-ink-400 bg-white' : 'border-ink-200 bg-ink-50/40'
                  }`}
                >
                  <div className="flex flex-wrap items-baseline gap-2">
                    <span className="rounded bg-ink-900 px-1.5 py-0.5 font-mono text-[10px] font-bold text-white">
                      {n.category}
                    </span>
                    {n.ticker && n.ticker !== '__MACRO__' ? (
                      <Link href={`/stock/${encodeURIComponent(n.ticker)}`} className="link text-xs font-semibold">
                        {n.ticker}
                      </Link>
                    ) : (
                      <span className="text-xs font-semibold text-ink-600">Market-wide</span>
                    )}
                    {isMaterial(n) ? (
                      <span className="rounded border border-watch bg-amber-50 px-1.5 py-0.5 text-[10px] font-bold uppercase text-watch">
                        <span aria-hidden="true">! </span>Material
                      </span>
                    ) : (
                      <span className="rounded border border-ink-300 px-1.5 py-0.5 text-[10px] font-medium uppercase text-ink-500">
                        Below materiality bar
                      </span>
                    )}
                    <span className="num ml-auto whitespace-nowrap text-[11px] text-ink-500">
                      {formatTimestamp(n.timestamp)} · {formatAge(n.timestamp)}
                    </span>
                  </div>

                  <p className="mt-1.5 text-sm font-medium leading-snug text-ink-900">
                    {n.url ? (
                      <a
                        href={n.url}
                        rel="noreferrer noopener nofollow"
                        target="_blank"
                        className="link"
                      >
                        {n.headline}
                      </a>
                    ) : (
                      n.headline
                    )}
                  </p>

                  <dl className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-[11px] sm:grid-cols-5">
                    <Field
                      label="Sentiment"
                      value={`${n.sentiment} (${formatNumber(n.sentiment_score, 2)})`}
                      hint="−1 to 1"
                    />
                    <Field
                      label="Materiality"
                      value={`${n.materiality} (${formatNumber(n.materiality_score, 2)})`}
                      hint="0 to 1"
                    />
                    <Field label="Confidence" value={formatPercent(n.confidence)} />
                    <Field label="Horizon" value={n.expected_horizon || '—'} />
                    <Field
                      label="Signed impact"
                      value={formatNumber(
                        Math.max(
                          -1,
                          Math.min(1, n.sentiment_score * n.materiality_score * n.confidence),
                        ),
                        3,
                      )}
                      hint="sentiment × materiality × confidence — the only channel by which news reaches a score"
                    />
                  </dl>

                  <p className="mt-1.5 flex flex-wrap gap-x-3 text-[11px] text-ink-500">
                    <span>source {n.source}</span>
                    <span>stage {n.stage}</span>
                    {n.llm_model ? (
                      <span>
                        enriched by {n.llm_model}
                        {n.llm_agreed ? ' (agreed with the deterministic label)' : ' (disagreed with the deterministic label)'}
                      </span>
                    ) : null}
                    {n.deduplicated ? <span>deduplicated</span> : null}
                    {list(n.matched_terms).length > 0 ? (
                      <span className="font-mono">
                        matched: {list(n.matched_terms).join(', ')}
                      </span>
                    ) : null}
                  </p>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card title="How news reaches a decision">
          <ul className="max-w-prose space-y-1.5 text-sm leading-relaxed text-ink-700">
            <li>
              Each item is classified <strong>deterministically first</strong> —
              category, sentiment, materiality — before any language model sees it.
            </li>
            <li>
              A language model may then enrich the record, and whether it{' '}
              <em>agreed</em> with the deterministic label is stored and shown
              above. A disagreement is visible rather than silently resolved.
            </li>
            <li>
              Only <strong>signed impact</strong> — sentiment × materiality ×
              confidence, bounded to [−1, 1] — ever reaches the scoring engine. The
              headline text does not.
            </li>
            <li>
              An item is <strong>material</strong> when its materiality is high, or
              medium with confidence of at least 70%. Items below that bar are shown
              here but do not affect decisions.
            </li>
          </ul>
        </Card>
      </div>
    </>
  );
}

function Field({
  label,
  value,
  hint,
}: {
  label: string;
  value: string;
  hint?: string;
}) {
  return (
    <div>
      <dt className="font-medium uppercase tracking-wide text-ink-500">{label}</dt>
      <dd className="num text-ink-900" title={hint}>
        {value}
      </dd>
      {hint ? <span className="sr-only">{hint}</span> : null}
    </div>
  );
}

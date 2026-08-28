import type { Metadata } from 'next';
import Link from 'next/link';

import { BarChart, CandleChart, NoChartData } from '@/components/charts';
import { DisclaimerBanner, DisclaimerNote } from '@/components/disclaimer';
import { ScenarioCard } from '@/components/distribution';
import { Card, DefinitionList, Freshness, Hash, Metric, PageHeader } from '@/components/page';
import { RiskChecksTable } from '@/components/risk-checks';
import {
  DegradedBanner,
  EmptyState,
  ErrorState,
  StaleNotice,
} from '@/components/states';
import {
  ClassificationBadge,
  Chip,
  RiskLevelBadge,
  SideBadge,
  SignalStatusBadge,
} from '@/components/verdicts';
import {
  formatAge,
  formatBps,
  formatCompactMoney,
  formatNumber,
  formatPercent,
  formatPrice,
  formatScore,
  formatTimestamp,
  humanizeKey,
  list,
} from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import type { FeatureSnapshot, StockScore } from '@/lib/types';

export const dynamic = 'force-dynamic';

export async function generateMetadata({
  params,
}: {
  params: Promise<{ ticker: string }>;
}): Promise<Metadata> {
  const { ticker } = await params;
  return { title: decodeURIComponent(ticker).toUpperCase() };
}

/** The composite score components, in the order the Go type declares them. */
const SCORE_COMPONENTS: { key: keyof StockScore; label: string; higherIsSafer?: boolean }[] = [
  { key: 'fundamental_score', label: 'Fundamental' },
  { key: 'growth_score', label: 'Growth' },
  { key: 'quality_score', label: 'Quality' },
  { key: 'valuation_score', label: 'Valuation' },
  { key: 'technical_score', label: 'Technical' },
  { key: 'momentum_score', label: 'Momentum' },
  { key: 'sector_score', label: 'Sector' },
  { key: 'market_regime_score', label: 'Market regime' },
  { key: 'risk_score', label: 'Risk', higherIsSafer: true },
  { key: 'event_risk_score', label: 'Event risk', higherIsSafer: true },
];

export default async function StockPage({
  params,
}: {
  params: Promise<{ ticker: string }>;
}) {
  const { ticker: raw } = await params;
  const ticker = decodeURIComponent(raw).toUpperCase();

  const client = api();
  const [platform, viewR, candlesR] = await Promise.all([
    platformStatus(),
    client.stock(ticker),
    client.candles(ticker, { interval: '1m', limit: 180 }),
  ]);

  if (!viewR.ok) {
    return (
      <>
        <PageHeader title={ticker} purpose="Deep view of one instrument." />
        <ErrorState error={viewR} what={`the instrument view for ${ticker}`} />
        <p className="mt-3">
          <Link href="/stocks" className="link">
            ← Back to all instruments
          </Link>
        </p>
      </>
    );
  }

  const v = viewR.data;
  const snap = v.features ?? null;
  const score = v.score ?? null;
  const candles = candlesR.ok ? list(candlesR.data) : [];

  return (
    <>
      <PageHeader
        title={`${v.stock.ticker} — ${v.stock.company || 'unnamed instrument'}`}
        purpose="Everything the platform observed for this instrument, the score it derived, the probabilities it assigned, and every risk check it ran."
        actions={
          <Link href="/stocks" className="btn">
            ← All instruments
          </Link>
        }
      >
        <div className="flex flex-wrap items-center gap-2">
          <Chip label="sector" value={v.stock.sector || '—'} mono={false} />
          <Chip label="exchange" value={v.stock.exchange || '—'} />
          <Chip label="class" value={v.stock.asset_class || '—'} />
          <Chip label="beta" value={formatNumber(v.stock.beta, 2)} />
          <Chip label="mkt cap" value={formatCompactMoney(v.stock.market_cap)} />
          <Chip label="avg notional 30d" value={formatCompactMoney(v.stock.avg_notional_30d)} />
          {score ? <ClassificationBadge classification={score.classification} /> : null}
        </div>
      </PageHeader>

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />
        {snap?.stale ? (
          <StaleNotice reason={snap.stale_reason} asOf={formatTimestamp(snap.as_of)} />
        ) : null}
        <DisclaimerBanner />

        {/* --- Price ------------------------------------------------- */}
        <Card
          title="Price"
          subtitle={`${candles.length} bars at 1-minute interval.`}
          actions={<Freshness servedAt={viewR.servedAt} meta={viewR.meta} />}
        >
          {!candlesR.ok ? (
            <>
              <ErrorState error={candlesR} what="candles" />
              <p className="mt-2 text-xs text-ink-600">
                Candles are read from the persistent store. With the in-memory
                store configured this endpoint is unavailable, which is a
                configuration fact rather than a fault — the live quote below is
                still current.
              </p>
            </>
          ) : candles.length === 0 ? (
            <NoChartData
              message={`No bars are stored for ${ticker} at this interval. The platform may be running without a persistent candle store, or this instrument may not have traded in the window.`}
              height={220}
            />
          ) : (
            <CandleChart
              candles={candles
                .map((c) => ({
                  start: Date.parse(c.start),
                  open: c.open,
                  high: c.high,
                  low: c.low,
                  close: c.close,
                }))
                .sort((a, b) => a.start - b.start)}
              label={`${ticker} 1-minute candles`}
            />
          )}

          {snap ? (
            <div className="mt-4 grid grid-cols-2 gap-4 sm:grid-cols-4">
              <Metric label="Last" value={formatPrice(snap.last)} />
              <Metric label="Bid" value={formatPrice(snap.bid)} />
              <Metric label="Ask" value={formatPrice(snap.ask)} />
              <Metric
                label="Spread"
                value={formatBps(snap.spread_bps)}
                hint={
                  snap.bid > 0 && snap.ask > 0
                    ? undefined
                    : 'No two-sided quote is available, so the spread could not be computed.'
                }
              />
            </div>
          ) : null}
        </Card>

        {/* --- Feature snapshot -------------------------------------- */}
        <Card
          title="Feature snapshot"
          subtitle="The exact, immutable inputs the model saw. Cold features are marked: their indicator has not seen enough data to be meaningful, and they are never fed to the model."
        >
          {!snap ? (
            <EmptyState
              title="No feature snapshot"
              detail="The feature pipeline has not produced a snapshot for this instrument yet. Nothing downstream — score, prediction, risk — can exist without one."
            />
          ) : (
            <FeatureTable snapshot={snap} />
          )}
        </Card>

        <div className="grid gap-4 xl:grid-cols-2">
          {/* --- Composite score --------------------------------------- */}
          <Card
            title="Composite score"
            subtitle="Every sub-score is 0–100 and independently explainable."
          >
            {!score ? (
              <EmptyState title="No score has been computed for this instrument." />
            ) : (
              <>
                <div className="mb-3 flex flex-wrap items-baseline gap-3">
                  <p className="num text-3xl font-bold text-ink-900">
                    {formatScore(score.final_score)}
                  </p>
                  <span className="text-sm text-ink-500">/ 100</span>
                  <ClassificationBadge classification={score.classification} />
                </div>

                <BarChart
                  data={SCORE_COMPONENTS.map((c) => ({
                    label: c.higherIsSafer ? `${c.label} (higher = safer)` : c.label,
                    value: Number(score[c.key] ?? NaN),
                    note: score.weights?.[weightKey(c.key)] !== undefined
                      ? `w ${formatNumber(score.weights[weightKey(c.key)] ?? 0, 2)}`
                      : undefined,
                  }))}
                  format={(x) => formatScore(x)}
                  emptyMessage="No sub-scores were recorded."
                />

                {list(score.missing).length > 0 ? (
                  <p className="mt-3 rounded border border-watch bg-amber-50 px-3 py-2 text-xs leading-relaxed text-ink-800">
                    <span className="font-semibold text-watch">
                      <span aria-hidden="true">! </span>
                      {list(score.missing).length} component
                      {list(score.missing).length === 1 ? '' : 's'} could not be
                      computed:{' '}
                    </span>
                    <span className="font-mono">{list(score.missing).join(', ')}</span>.
                    These were excluded and the remaining weights renormalised —
                    they were not treated as zero, which would have silently
                    penalised the instrument.
                  </p>
                ) : null}

                <div className="mt-3 flex flex-wrap gap-1.5">
                  <Chip label="strategy" value={score.strategy_id || '—'} />
                  <Chip label="config" value={<Hash value={score.config_hash} />} />
                  <Chip label="computed" value={formatTimestamp(score.created_at)} />
                </div>

                {list(score.evidence).length > 0 ? (
                  <ul className="mt-3 space-y-1 text-xs text-ink-700">
                    {list(score.evidence).map((e, i) => (
                      <li key={i}>· {e}</li>
                    ))}
                  </ul>
                ) : null}

                <DisclaimerNote className="mt-3" text={score.disclaimer} />
              </>
            )}
          </Card>

          {/* --- Opportunity ------------------------------------------- */}
          <Card
            title="Opportunity decomposition"
            subtitle="The additive breakdown of the opportunity score, so the number is not a black box."
          >
            {!v.opportunity ? (
              <EmptyState title="No opportunity score was computed." />
            ) : (
              <>
                <div className="mb-3 flex flex-wrap items-center gap-3">
                  <p className="num text-3xl font-bold text-ink-900">
                    {formatScore(v.opportunity.score)}
                  </p>
                  <SideBadge side={v.opportunity.bias} />
                  <RiskLevelBadge level={v.opportunity.risk} />
                  <span className="num text-sm text-ink-700">
                    confidence {formatPercent(v.opportunity.confidence)}
                  </span>
                </div>

                <BarChart
                  data={Object.entries(v.opportunity.components ?? {}).map(
                    ([k, val]) => ({ label: humanizeKey(k), value: val }),
                  )}
                  format={(x) => formatNumber(x, 2)}
                  emptyMessage="No component decomposition was recorded, so this score cannot be explained."
                />

                <DefinitionList
                  columns={2}
                  items={[
                    { term: 'Risk / reward', value: formatNumber(v.opportunity.risk_reward, 2) },
                    { term: 'Expected move', value: formatBps(v.opportunity.expected_move_bps) },
                    { term: 'Stop distance', value: formatBps(v.opportunity.stop_distance_bps) },
                    { term: 'Target distance', value: formatBps(v.opportunity.target_distance_bps) },
                  ]}
                />
                <DisclaimerNote className="mt-3" text={v.opportunity.disclaimer} />
              </>
            )}
          </Card>
        </div>

        {/* --- Prediction -------------------------------------------- */}
        <Card
          title="Prediction"
          subtitle="Probabilities over three outcomes per horizon, each with the flat band that defines the classes."
        >
          {!v.prediction ? (
            <EmptyState
              title="No prediction has been produced"
              detail="Predictions require a warm feature snapshot. If the platform is degraded with the model subsystem unavailable, the deterministic rule prior serves in the model's place — and the record says so."
            />
          ) : (
            <>
              <div className="mb-3 flex flex-wrap gap-1.5">
                <Chip label="model" value={v.prediction.model_id || '—'} />
                <Chip label="version" value={v.prediction.model_version || '—'} />
                <Chip label="source" value={v.prediction.source} />
                <Chip label="feature hash" value={<Hash value={v.prediction.feature_hash} />} />
                <Chip label="regime" value={v.prediction.regime || '—'} />
              </div>

              {v.prediction.source !== 'model' ? (
                <p className="mb-3 rounded border border-watch bg-amber-50 px-3 py-2 text-xs leading-relaxed text-ink-800">
                  <span className="font-semibold text-watch">
                    <span aria-hidden="true">! </span>These are not model outputs.{' '}
                  </span>
                  The distributions below came from{' '}
                  <span className="font-mono">{v.prediction.source}</span> — the
                  deterministic rule prior, used when no model artifact is available
                  to serve. They carry no learned information.
                </p>
              ) : null}

              <div className="grid gap-3 lg:grid-cols-3">
                {list(v.prediction.scenarios).map((s) => (
                  <ScenarioCard key={s.horizon} scenario={s} />
                ))}
              </div>
              {list(v.prediction.scenarios).length === 0 ? (
                <EmptyState title="The prediction record contains no scenarios." />
              ) : null}

              {list(v.prediction.degraded).length > 0 ? (
                <p className="mt-3 text-xs text-ink-700">
                  Produced while these subsystems were unavailable:{' '}
                  <span className="font-mono">{list(v.prediction.degraded).join(', ')}</span>.
                </p>
              ) : null}

              <DisclaimerNote className="mt-3" text={v.prediction.disclaimer} />
            </>
          )}
        </Card>

        {/* --- Risk -------------------------------------------------- */}
        <Card
          title="Risk assessment — every check"
          subtitle="The decision is what is enforced; the level is descriptive. A skipped check protected nothing."
        >
          <RiskChecksTable assessment={v.risk} />
        </Card>

        {/* --- Signals ----------------------------------------------- */}
        <Card title="Signals for this instrument">
          {list(v.signals).length === 0 ? (
            <EmptyState
              title="No signals"
              detail="A signal exists only where the risk engine allowed one and an invalidation condition could be attached."
            />
          ) : (
            <ul className="space-y-2">
              {list(v.signals).map((s) => (
                <li key={s.id} className="flex flex-wrap items-center gap-2 rounded border border-ink-200 p-2.5">
                  <SideBadge side={s.side} />
                  <SignalStatusBadge status={s.status} />
                  <span className="num text-xs text-ink-600">
                    strength {formatScore(s.strength)} · confidence {formatPercent(s.confidence)} ·{' '}
                    {s.horizon}
                  </span>
                  <Link href={`/signals/${encodeURIComponent(s.id)}`} className="link ml-auto text-xs">
                    Why this signal? →
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Card>

        {/* --- Evidence and explanation ------------------------------ */}
        <div className="grid gap-4 xl:grid-cols-2">
          <Card title="Rule evidence">
            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-allow">
                  <span aria-hidden="true">✓ </span>Supporting
                </h3>
                {list(v.evidence).length === 0 ? (
                  <p className="text-xs text-ink-500">No supporting rule fired.</p>
                ) : (
                  <ul className="space-y-1 text-sm text-ink-800">
                    {list(v.evidence).map((e, i) => (
                      <li key={i}>· {e}</li>
                    ))}
                  </ul>
                )}
              </div>
              <div>
                <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-watch">
                  <span aria-hidden="true">! </span>Counter-evidence
                </h3>
                {list(v.counter_evidence).length === 0 ? (
                  <p className="text-xs text-ink-500">No counter-evidence rule fired.</p>
                ) : (
                  <ul className="space-y-1 text-sm text-ink-800">
                    {list(v.counter_evidence).map((e, i) => (
                      <li key={i}>· {e}</li>
                    ))}
                  </ul>
                )}
              </div>
            </div>
          </Card>

          <Card
            title="AI explanation"
            subtitle="Written after the decision from the structured record. It never influences the decision."
          >
            {!v.explanation ? (
              <EmptyState
                title="No explanation has been generated"
                detail="The narrative is produced outside the decision path and is never required. Everything above is the actual record."
              />
            ) : (
              <div className="space-y-3">
                {v.explanation.rejected ? (
                  <p className="rounded border border-deny bg-red-50 px-3 py-2 text-xs leading-relaxed text-ink-800">
                    <span className="font-semibold text-deny">
                      <span aria-hidden="true">✕ </span>A generated narrative was
                      discarded:{' '}
                    </span>
                    {v.explanation.rejected}. It is surfaced rather than hidden — a
                    silently replaced explanation would be a trust problem.
                  </p>
                ) : null}

                <p className="text-sm leading-relaxed text-ink-800">
                  {v.explanation.summary}
                </p>

                {v.explanation.uncertainty ? (
                  <div>
                    <h4 className="text-xs font-bold uppercase tracking-wide text-ink-600">
                      Stated uncertainty
                    </h4>
                    <p className="text-sm text-ink-800">{v.explanation.uncertainty}</p>
                  </div>
                ) : null}

                {list(v.explanation.risks).length > 0 ? (
                  <div>
                    <h4 className="text-xs font-bold uppercase tracking-wide text-ink-600">
                      Risks
                    </h4>
                    <ul className="text-sm text-ink-800">
                      {list(v.explanation.risks).map((r, i) => (
                        <li key={i}>· {r}</li>
                      ))}
                    </ul>
                  </div>
                ) : null}

                {list(v.explanation.scenarios).length > 0 ? (
                  <div>
                    <h4 className="text-xs font-bold uppercase tracking-wide text-ink-600">
                      Scenarios
                    </h4>
                    <ul className="text-sm text-ink-800">
                      {list(v.explanation.scenarios).map((s, i) => (
                        <li key={i}>· {s}</li>
                      ))}
                    </ul>
                  </div>
                ) : null}

                <p className="text-[11px] text-ink-500">
                  Source: <span className="font-mono">{v.explanation.source}</span>
                  {v.explanation.model ? (
                    <>
                      {' '}
                      · model <span className="font-mono">{v.explanation.model}</span>
                    </>
                  ) : null}
                </p>
                <DisclaimerNote text={v.explanation.disclaimer} />
              </div>
            )}
          </Card>
        </div>

        {/* --- Invalidation conditions across signals ---------------- */}
        <Card
          title="Invalidation conditions"
          subtitle="What would prove the current signals wrong. Governance rule G-6 requires at least one per signal."
        >
          {list(v.signals).flatMap((s) => list(s.invalidations)).length === 0 ? (
            <EmptyState
              title="No invalidation conditions"
              detail="There are no active signals for this instrument, so there is nothing to invalidate."
            />
          ) : (
            <ul className="space-y-2">
              {list(v.signals).flatMap((s) =>
                list(s.invalidations).map((inv, i) => (
                  <li key={`${s.id}-${i}`} className="rounded border border-ink-200 p-2.5">
                    <div className="flex flex-wrap items-baseline gap-2">
                      <span className="rounded bg-ink-900 px-1.5 py-0.5 font-mono text-[10px] font-bold text-white">
                        {inv.kind}
                      </span>
                      {inv.threshold ? (
                        <span className="num text-sm font-semibold">
                          {formatNumber(inv.threshold, 4)}
                        </span>
                      ) : null}
                      <Link href={`/signals/${encodeURIComponent(s.id)}`} className="link ml-auto text-[11px]">
                        signal {s.id.slice(0, 8)}
                      </Link>
                    </div>
                    <p className="mt-1 text-sm text-ink-700">{inv.description}</p>
                  </li>
                )),
              )}
            </ul>
          )}
        </Card>

        {/* --- News and events --------------------------------------- */}
        <div className="grid gap-4 xl:grid-cols-2">
          <Card title="News">
            {list(v.news).length === 0 ? (
              <EmptyState title="No news events recorded for this instrument." />
            ) : (
              <ul className="space-y-2">
                {list(v.news).slice(0, 10).map((n) => (
                  <li key={n.id} className="border-b border-ink-100 pb-2 last:border-0">
                    <p className="text-sm font-medium text-ink-900">{n.headline}</p>
                    <p className="mt-0.5 flex flex-wrap gap-x-3 text-[11px] text-ink-500">
                      <span>{n.category}</span>
                      <span>
                        sentiment {n.sentiment} ({formatNumber(n.sentiment_score, 2)})
                      </span>
                      <span>materiality {n.materiality}</span>
                      <span>confidence {formatPercent(n.confidence)}</span>
                      <span className="num">{formatAge(n.timestamp)}</span>
                    </p>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card title="Corporate events">
            {list(v.events).length === 0 ? (
              <EmptyState title="No scheduled corporate events." />
            ) : (
              <ul className="space-y-2">
                {list(v.events).map((e) => (
                  <li key={e.id} className="flex flex-wrap items-baseline gap-2 border-b border-ink-100 pb-2 last:border-0">
                    <span className="rounded bg-ink-900 px-1.5 py-0.5 text-[10px] font-bold text-white">
                      {e.type}
                    </span>
                    <span className="num text-sm">{formatTimestamp(e.scheduled_at)}</span>
                    <span
                      className={`text-[11px] font-semibold ${e.confirmed ? 'text-allow' : 'text-watch'}`}
                    >
                      <span aria-hidden="true">{e.confirmed ? '✓ ' : '! '}</span>
                      {e.confirmed ? 'confirmed' : 'estimated'}
                    </span>
                    {e.detail ? (
                      <span className="w-full text-xs text-ink-600">{e.detail}</span>
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      </div>
    </>
  );
}

/** Maps a score field name to its key in the recorded weight map. */
function weightKey(field: keyof StockScore): string {
  return String(field).replace(/_score$/, '');
}

function FeatureTable({ snapshot }: { snapshot: FeatureSnapshot }) {
  const values = snapshot.values ?? {};
  const warm = snapshot.warm ?? {};
  const names = Object.keys(values).sort();
  const cold = names.filter((n) => warm[n] === false);

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap gap-1.5">
        <Chip label="hash" value={<Hash value={snapshot.hash} chars={16} />} />
        <Chip label="as of" value={formatTimestamp(snapshot.as_of)} />
        <Chip label="age" value={formatAge(snapshot.as_of)} mono={false} />
        <Chip label="interval" value={snapshot.interval} />
        <Chip label="schema" value={`v${snapshot.schema_version}`} />
        <Chip label="features" value={`${names.length} (${cold.length} cold)`} />
      </div>

      {cold.length > 0 ? (
        <p className="rounded border border-watch bg-amber-50 px-3 py-2 text-xs leading-relaxed text-ink-800">
          <span className="font-semibold text-watch">
            <span aria-hidden="true">! </span>
            {cold.length} of {names.length} features are cold.{' '}
          </span>
          Their indicators have not seen enough bars to be meaningful. Cold
          features are never fed to the model, so anything depending on them is
          being decided without that information:{' '}
          <span className="font-mono">{cold.slice(0, 8).join(', ')}</span>
          {cold.length > 8 ? ` and ${cold.length - 8} more` : ''}.
        </p>
      ) : (
        <p className="text-xs text-allow">
          <span aria-hidden="true">✓ </span>
          Every feature in this snapshot is warm.
        </p>
      )}

      {names.length === 0 ? (
        <EmptyState title="The snapshot contains no feature values." />
      ) : (
        <div className="table-wrap max-h-96 overflow-y-auto">
          <table className="data-table">
            <caption className="sr-only">
              Every feature in the snapshot with its value and warm state.
            </caption>
            <thead className="sticky top-0 bg-white">
              <tr>
                <th scope="col">Feature</th>
                <th scope="col">Value</th>
                <th scope="col">State</th>
              </tr>
            </thead>
            <tbody>
              {names.map((n) => {
                const isCold = warm[n] === false;
                return (
                  <tr key={n} className={isCold ? 'bg-amber-50/60' : undefined}>
                    <th scope="row" className="font-mono text-xs font-normal text-ink-800">
                      {n}
                    </th>
                    <td className="num text-xs">{formatNumber(values[n] ?? NaN, 6)}</td>
                    <td className="text-xs">
                      {isCold ? (
                        <span className="font-semibold text-watch">
                          <span aria-hidden="true">! </span>Cold — excluded from the model
                        </span>
                      ) : (
                        <span className="text-allow">
                          <span aria-hidden="true">✓ </span>Warm
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <p className="text-[11px] leading-snug text-ink-500">
        This snapshot is the unit of reproducibility: its hash plus a model
        artifact digest is sufficient to recompute the prediction exactly.
        Feature names are shown exactly as the platform records them, so they can
        be matched against the model&apos;s declared feature list without
        guesswork.
      </p>
    </div>
  );
}

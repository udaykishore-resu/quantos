import type { Metadata } from 'next';
import Link from 'next/link';

import { DisclaimerBanner } from '@/components/disclaimer';
import { DistributionBar, ScenarioCard } from '@/components/distribution';
import { Card, Freshness, Hash, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import { Chip } from '@/components/verdicts';
import {
  formatAge,
  formatBps,
  formatNumber,
  formatPercent,
  formatPrice,
  formatTimestamp,
  humanizeEnum,
  list,
  outcomeLabel,
} from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import type { Prediction, PredictionOutcome } from '@/lib/types';

export const metadata: Metadata = { title: 'Predictions' };
export const dynamic = 'force-dynamic';

/**
 * The predictions page.
 *
 * Its purpose is to make the model falsifiable in public: the distribution and
 * the resolved outcome are shown side by side, so a reader can watch the model
 * be wrong. Anything that made errors harder to find here — hiding wrong calls,
 * summarising accuracy without the base rate, showing only the winning class —
 * would defeat the point of the page.
 */
export default async function PredictionsPage({
  searchParams,
}: {
  searchParams: Promise<{ ticker?: string }>;
}) {
  const sp = await searchParams;
  const ticker = typeof sp.ticker === 'string' ? sp.ticker.toUpperCase() : '';

  const client = api();
  const [platform, predsR, evalR] = await Promise.all([
    platformStatus(),
    client.predictions({ ticker, limit: 60 }),
    client.evaluations({ limit: 500 }),
  ]);

  const predictions = predsR.ok ? list(predsR.data) : [];
  const outcomes = evalR.ok ? list(evalR.data.outcomes) : [];

  // Index resolved outcomes by prediction id so each forecast can be shown
  // beside what actually happened.
  const byPrediction = new Map<string, PredictionOutcome[]>();
  for (const o of outcomes) {
    const existing = byPrediction.get(o.prediction_id) ?? [];
    existing.push(o);
    byPrediction.set(o.prediction_id, existing);
  }

  const resolved = predictions.filter((p) => byPrediction.has(p.id)).length;

  return (
    <>
      <PageHeader
        title="Predictions"
        purpose="Every forecast the platform has made, and — where the horizon has elapsed — what actually happened. Wrong calls are shown as prominently as right ones; that is the point of this page."
        actions={
          ticker ? (
            <Link href="/predictions" className="btn">
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

        <Card
          title="Resolved outcomes"
          subtitle="Predictions whose horizon has elapsed and whose realised move has been measured."
          actions={<Freshness servedAt={evalR.ok ? evalR.servedAt : undefined} meta={evalR.ok ? evalR.meta : undefined} />}
        >
          {!evalR.ok ? (
            <ErrorState error={evalR} what="resolved outcomes" />
          ) : outcomes.length === 0 ? (
            <EmptyState
              title="No predictions have resolved yet"
              detail="An outcome is recorded once a prediction's horizon has elapsed and the realised move has been compared against the flat band. Until then the model is unscored — which is a fact about the platform's age, not about the model's quality."
            />
          ) : (
            <>
              <div className="table-wrap max-h-[32rem] overflow-y-auto">
                <table className="data-table min-w-[56rem]">
                  <caption className="sr-only">
                    Resolved predictions: the distribution that was forecast beside
                    the outcome that occurred.
                  </caption>
                  <thead className="sticky top-0 bg-white">
                    <tr>
                      <th scope="col">Ticker</th>
                      <th scope="col">Horizon</th>
                      <th scope="col">Forecast distribution</th>
                      <th scope="col">Predicted</th>
                      <th scope="col">Actual</th>
                      <th scope="col">Result</th>
                      <th scope="col">Return</th>
                      <th scope="col">Brier</th>
                      <th scope="col">Log loss</th>
                      <th scope="col">Regime</th>
                      <th scope="col">Risk decision</th>
                      <th scope="col">Resolved</th>
                    </tr>
                  </thead>
                  <tbody>
                    {outcomes
                      .slice()
                      .sort((a, b) => Date.parse(b.resolved_at) - Date.parse(a.resolved_at))
                      .map((o, i) => (
                        <tr key={`${o.prediction_id}-${o.horizon}-${i}`} className={o.correct ? undefined : 'bg-red-50/40'}>
                          <th scope="row">
                            <Link href={`/stock/${encodeURIComponent(o.ticker)}`} className="link">
                              {o.ticker}
                            </Link>
                          </th>
                          <td className="num text-xs">{o.horizon}</td>
                          <td className="min-w-[10rem]">
                            <DistributionBar dist={o.distribution} compact />
                          </td>
                          <td className="font-semibold">{outcomeLabel(o.predicted)}</td>
                          <td className="font-semibold">{outcomeLabel(o.actual)}</td>
                          <td>
                            <span
                              className={`inline-flex items-center gap-1 whitespace-nowrap rounded border px-1.5 py-0.5 text-xs font-semibold ${
                                o.correct
                                  ? 'border-allow bg-teal-50 text-allow'
                                  : 'border-deny bg-red-50 text-deny'
                              }`}
                            >
                              <span aria-hidden="true">{o.correct ? '✓' : '✕'}</span>
                              {o.correct ? 'Correct' : 'Wrong'}
                            </span>
                          </td>
                          <td className="num text-xs">
                            {formatBps(o.return_bps)}
                            <span className="block text-[10px] text-ink-500">
                              {formatPrice(o.entry_price)} → {formatPrice(o.exit_price)}
                            </span>
                          </td>
                          <td className="num text-xs">{formatNumber(o.brier_score, 4)}</td>
                          <td className="num text-xs">{formatNumber(o.log_loss, 4)}</td>
                          <td className="text-xs">{o.regime ? humanizeEnum(o.regime) : '—'}</td>
                          <td className="font-mono text-[10px]">{o.risk_decision || '—'}</td>
                          <td className="num whitespace-nowrap text-xs text-ink-600">
                            {formatTimestamp(o.resolved_at)}
                          </td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
              <p className="mt-2 text-xs text-ink-600">
                {outcomes.filter((o) => !o.correct).length} of {outcomes.length}{' '}
                resolved predictions were wrong. Rows for wrong calls are shaded so
                they are not easy to skip past.
              </p>
            </>
          )}
        </Card>

        <Card
          title={ticker ? `Open predictions for ${ticker}` : 'Open predictions'}
          subtitle={
            predictions.length > 0
              ? `${predictions.length} recorded, ${resolved} of which have a resolved outcome.`
              : undefined
          }
          actions={<Freshness servedAt={predsR.ok ? predsR.servedAt : undefined} meta={predsR.ok ? predsR.meta : undefined} />}
        >
          {!predsR.ok ? (
            <ErrorState error={predsR} what="predictions" />
          ) : predictions.length === 0 ? (
            <EmptyState
              title="No predictions"
              detail="Predictions require warm feature snapshots. If the model subsystem is degraded, the deterministic rule prior serves in its place and each record says so."
            />
          ) : (
            <ul className="grid gap-4 xl:grid-cols-2">
              {predictions.slice(0, 20).map((p) => (
                <PredictionCard
                  key={p.id}
                  prediction={p}
                  outcomes={byPrediction.get(p.id) ?? []}
                />
              ))}
            </ul>
          )}
        </Card>
      </div>
    </>
  );
}

function PredictionCard({
  prediction: p,
  outcomes,
}: {
  prediction: Prediction;
  outcomes: PredictionOutcome[];
}) {
  const scenarios = list(p.scenarios);
  return (
    <li className="rounded-lg border border-ink-200 bg-white p-4">
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <Link href={`/stock/${encodeURIComponent(p.ticker)}`} className="link text-base font-semibold">
          {p.ticker}
        </Link>
        <Chip label="source" value={p.source} />
        <Chip label="model" value={p.model_version || '—'} />
        <Chip label="features" value={<Hash value={p.feature_hash} chars={10} />} />
        <span className="num ml-auto text-[11px] text-ink-500">
          {formatAge(p.created_at)}
        </span>
      </div>

      {p.source !== 'model' ? (
        <p className="mb-2 rounded border border-watch bg-amber-50 px-2 py-1 text-[11px] leading-snug text-ink-800">
          <span className="font-semibold text-watch">
            <span aria-hidden="true">! </span>
          </span>
          Produced by <span className="font-mono">{p.source}</span>, not by a
          trained model. The deterministic rule prior carries no learned
          information.
        </p>
      ) : null}

      {scenarios.length === 0 ? (
        <EmptyState title="This prediction records no scenarios." />
      ) : (
        <div className="space-y-3">
          {scenarios.map((s) => (
            <ScenarioCard key={s.horizon} scenario={s} />
          ))}
        </div>
      )}

      {outcomes.length > 0 ? (
        <div className="mt-3 rounded border border-ink-300 bg-ink-50 p-2.5">
          <h4 className="text-xs font-bold uppercase tracking-wide text-ink-600">
            What happened
          </h4>
          <ul className="mt-1 space-y-1">
            {outcomes.map((o, i) => (
              <li key={i} className="flex flex-wrap items-center gap-2 text-xs">
                <span className="num font-medium">{o.horizon}</span>
                <span>
                  said <strong>{outcomeLabel(o.predicted)}</strong>, got{' '}
                  <strong>{outcomeLabel(o.actual)}</strong>
                </span>
                <span
                  className={`font-semibold ${o.correct ? 'text-allow' : 'text-deny'}`}
                >
                  <span aria-hidden="true">{o.correct ? '✓ ' : '✕ '}</span>
                  {o.correct ? 'correct' : 'wrong'}
                </span>
                <span className="num text-ink-600">{formatBps(o.return_bps)}</span>
                <span className="num text-ink-500">
                  Brier {formatNumber(o.brier_score, 3)}
                </span>
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <p className="mt-3 text-[11px] text-ink-500">
          Not yet resolved — the horizon has not elapsed, so there is no outcome to
          compare against.
        </p>
      )}

      <p className="mt-2 text-[11px] text-ink-500">
        Regime at forecast time:{' '}
        <span className="font-medium">{p.regime ? humanizeEnum(p.regime) : '—'}</span>{' '}
        (confidence {formatPercent(p.regime_confidence)}) · blend weight{' '}
        <span className="num">{formatNumber(p.blend_weight, 2)}</span>
      </p>
    </li>
  );
}

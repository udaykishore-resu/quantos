import type { Metadata } from 'next';
import Link from 'next/link';

import { BarChart } from '@/components/charts';
import { DisclaimerBanner, DisclaimerNote } from '@/components/disclaimer';
import { ScenarioCard, horizonPhrase } from '@/components/distribution';
import { Card, DefinitionList, Hash, PageHeader } from '@/components/page';
import { RiskChecksTable } from '@/components/risk-checks';
import { DegradedBanner, EmptyState, ErrorState, StaleNotice } from '@/components/states';
import { Chip, SideBadge, SignalStatusBadge } from '@/components/verdicts';
import {
  formatAge,
  formatBps,
  formatNumber,
  formatPercent,
  formatPrice,
  formatScore,
  formatTimestamp,
  humanizeEnum,
  isZeroTime,
  list,
  outcomeLabel,
} from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import type { Provenance } from '@/lib/types';

export const dynamic = 'force-dynamic';

export async function generateMetadata({
  params,
}: {
  params: Promise<{ id: string }>;
}): Promise<Metadata> {
  const { id } = await params;
  return { title: `Signal ${id.slice(0, 8)}` };
}

/**
 * The provenance screen.
 *
 * The platform's central claim is that every signal is explainable end to end.
 * This page is where that claim is either honoured or exposed, so it is
 * organised as the causal chain itself, in order:
 *
 *   data → features (and their hash) → rules fired → counter-evidence →
 *   model and version → regime → risk decision and every check → invalidation
 *
 * Two things it refuses to do. It never presents a partial record as a complete
 * one: `reproducible: false` and the `missing` list are rendered at the top,
 * loudly, because a chain with a hole in it cannot support the claim. And it
 * never lets an AI-written explanation stand in for the record — the narrative
 * is shown last, labelled as narrative, after the evidence a reader can check.
 */
export default async function SignalDetailPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  const [platform, result] = await Promise.all([
    platformStatus(),
    api().provenance(id),
  ]);

  if (!result.ok) {
    return (
      <>
        <PageHeader
          title="Signal provenance"
          purpose="The complete causal record behind one signal."
        />
        <ErrorState error={result} what={`the provenance record for signal ${id}`} />
        <p className="mt-3">
          <Link href="/signals" className="link">
            ← Back to signals
          </Link>
        </p>
      </>
    );
  }

  const p: Provenance = result.data;
  const sig = p.signal;
  const missing = list(p.missing);

  return (
    <>
      <PageHeader
        title={`${sig.ticker} — signal provenance`}
        purpose="Why the platform generated this signal: every input, in the order it was consumed, ending with what would prove it wrong."
        actions={
          <Link href="/signals" className="btn">
            ← All signals
          </Link>
        }
      >
        <div className="flex flex-wrap items-center gap-2">
          <SideBadge side={sig.side} />
          <SignalStatusBadge status={sig.status} />
          <Chip label="signal" value={sig.id} />
          <Chip label="correlation" value={sig.correlation_id || '—'} />
        </div>
      </PageHeader>

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />

        {/* --- Reproducibility verdict, first ------------------------- */}
        <div
          role={p.reproducible ? undefined : 'alert'}
          className={`rounded-md border px-4 py-3 ${
            p.reproducible ? 'border-allow bg-teal-50' : 'border-deny bg-red-50'
          }`}
        >
          <p
            className={`text-sm font-semibold ${
              p.reproducible ? 'text-allow' : 'text-deny'
            }`}
          >
            <span aria-hidden="true">{p.reproducible ? '✓ ' : '✕ '}</span>
            {p.reproducible
              ? 'This record is complete and the decision is reproducible'
              : 'This record is incomplete — the decision cannot be fully reproduced'}
          </p>
          <p className="mt-1 max-w-prose text-sm leading-relaxed text-ink-800">{p.note}</p>
          {missing.length > 0 ? (
            <p className="mt-1 text-sm text-ink-800">
              Missing inputs:{' '}
              <span className="font-mono font-medium">{missing.join(', ')}</span>
            </p>
          ) : null}
          {p.feature_snapshot_error ? (
            <p className="mt-1 break-words font-mono text-xs text-ink-600">
              feature snapshot error: {p.feature_snapshot_error}
            </p>
          ) : null}
        </div>

        <DisclaimerBanner text={sig.disclaimer} />

        {/* --- The chain --------------------------------------------- */}
        <ol className="space-y-4">
          <Step n={1} title="The instrument and the moment">
            <DefinitionList
              columns={3}
              items={[
                {
                  term: 'Ticker',
                  value: (
                    <Link href={`/stock/${encodeURIComponent(sig.ticker)}`} className="link">
                      {sig.ticker}
                    </Link>
                  ),
                },
                { term: 'Created', value: formatTimestamp(sig.created_at), hint: formatAge(sig.created_at) },
                {
                  term: 'Expires',
                  value: formatTimestamp(sig.expires_at),
                  hint: `${horizonPhrase(sig.horizon)} horizon`,
                },
                { term: 'Strength', value: `${formatScore(sig.strength)} / 100` },
                {
                  term: 'Confidence',
                  value: formatPercent(sig.confidence),
                  hint: 'Margin of the leading outcome over an even split.',
                },
                {
                  term: 'Suggested weight',
                  value: formatPercent(sig.suggested_weight),
                  hint: 'Fraction of simulated paper equity. No real-money position is ever taken.',
                },
              ]}
            />
            <div className="mt-3 grid gap-3 sm:grid-cols-3">
              <RefLevel label="Entry reference" value={sig.entry_reference} />
              <RefLevel label="Stop reference" value={sig.stop_reference} />
              <RefLevel label="Target reference" value={sig.target_reference} />
            </div>
            <p className="mt-2 max-w-prose text-[11px] leading-snug text-ink-500">
              These are reference levels recorded with the signal for later
              evaluation. They are not order instructions, and no order is placed
              from them.
            </p>
          </Step>

          <Step n={2} title="The data it saw — the feature snapshot">
            {!p.feature_snapshot ? (
              <EmptyState
                title="The feature snapshot could not be retrieved"
                detail={
                  p.feature_snapshot_error ||
                  'Without the snapshot the prediction cannot be recomputed, so the reproducibility claim does not hold for this signal.'
                }
              />
            ) : (
              <FeatureSnapshotView snapshot={p.feature_snapshot} signalHash={sig.feature_hash} />
            )}
          </Step>

          <Step n={3} title="The rules that fired">
            <div className="grid gap-4 lg:grid-cols-2">
              <div>
                <h4 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-allow">
                  <span aria-hidden="true">✓ </span>Supporting evidence
                </h4>
                {list(sig.evidence).length === 0 ? (
                  <p className="text-xs text-ink-500">
                    No supporting statements were recorded. A signal without
                    recorded evidence cannot be explained, only trusted.
                  </p>
                ) : (
                  <ul className="space-y-1 text-sm text-ink-800">
                    {list(sig.evidence).map((e, i) => (
                      <li key={i} className="flex gap-2">
                        <span aria-hidden="true" className="text-allow">
                          ✓
                        </span>
                        <span>{e}</span>
                      </li>
                    ))}
                  </ul>
                )}
                {list(sig.rules_fired).length > 0 ? (
                  <p className="mt-2 text-[11px] text-ink-600">
                    Rule ids:{' '}
                    <span className="font-mono">{list(sig.rules_fired).join(', ')}</span>
                  </p>
                ) : null}
              </div>

              <div>
                <h4 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-watch">
                  <span aria-hidden="true">! </span>Counter-evidence
                </h4>
                {list(sig.counter_evidence).length === 0 ? (
                  <p className="text-xs text-ink-500">
                    No counter-evidence rule fired. That is not the same as
                    &ldquo;nothing argues against this&rdquo; — it means none of
                    the configured counter-rules matched.
                  </p>
                ) : (
                  <ul className="space-y-1 text-sm text-ink-800">
                    {list(sig.counter_evidence).map((e, i) => (
                      <li key={i} className="flex gap-2">
                        <span aria-hidden="true" className="text-watch">
                          !
                        </span>
                        <span>{e}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>

            {p.strategy ? (
              <div className="mt-3 rounded border border-ink-200 bg-ink-50 p-3">
                <p className="text-xs font-semibold text-ink-800">
                  Strategy: {p.strategy.name}{' '}
                  <span className="font-mono font-normal text-ink-600">
                    ({p.strategy.id} v{p.strategy.version})
                  </span>
                </p>
                <p className="mt-0.5 text-xs text-ink-600">{p.strategy.description}</p>
                <div className="mt-1.5 flex flex-wrap gap-1.5">
                  <Chip label="hash" value={<Hash value={p.strategy.hash} />} />
                  <Chip label="source" value={p.strategy.source_path || '—'} />
                  <Chip label="rules" value={list(p.strategy.rules).length} />
                  <Chip
                    label="min confidence"
                    value={formatPercent(p.strategy.policy.min_confidence)}
                  />
                  <Chip
                    label="min edge"
                    value={formatNumber(p.strategy.policy.min_directional_edge, 3)}
                  />
                </div>
              </div>
            ) : (
              <p className="mt-3 text-xs text-ink-500">
                Strategy <span className="font-mono">{sig.strategy_id}</span> is not
                in the loaded registry, so the rule definitions behind this signal
                cannot be shown.
              </p>
            )}
          </Step>

          <Step n={4} title="The model and the probabilities it produced">
            {!p.prediction ? (
              <EmptyState
                title="The prediction record could not be retrieved"
                detail={`The signal references prediction ${sig.prediction_id || '(none recorded)'}, which is not available. The probabilities behind this signal cannot be shown.`}
              />
            ) : (
              <>
                <div className="mb-3 flex flex-wrap gap-1.5">
                  <Chip label="model" value={p.prediction.model_id || '—'} />
                  <Chip label="version" value={p.prediction.model_version || '—'} />
                  <Chip label="source" value={p.prediction.source} />
                  <Chip label="artifact" value={<Hash value={p.prediction.artifact_sha} />} />
                  <Chip label="blend weight" value={formatNumber(p.prediction.blend_weight, 2)} />
                </div>

                {p.prediction.source !== 'model' ? (
                  <p className="mb-3 rounded border border-watch bg-amber-50 px-3 py-2 text-xs leading-relaxed text-ink-800">
                    <span className="font-semibold text-watch">
                      <span aria-hidden="true">! </span>Not a model output.{' '}
                    </span>
                    These probabilities came from{' '}
                    <span className="font-mono">{p.prediction.source}</span>. A
                    rules-fallback distribution is the deterministic prior, produced
                    when no model artifact was available to serve — it should not be
                    read as a learned prediction.
                  </p>
                ) : null}

                <div className="grid gap-3 lg:grid-cols-2">
                  {list(p.prediction.scenarios).map((s) => (
                    <ScenarioCard
                      key={s.horizon}
                      scenario={s}
                      highlight={s.horizon === sig.horizon}
                    />
                  ))}
                </div>
                {list(p.prediction.scenarios).length === 0 ? (
                  <EmptyState title="The prediction contains no scenarios." />
                ) : null}

                <div className="mt-4 grid gap-4 lg:grid-cols-2">
                  <div>
                    <h4 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                      Rule prior vs model output
                    </h4>
                    <BarChart
                      data={[
                        { label: 'prior P(up)', value: p.prediction.rule_prior.up },
                        { label: 'model P(up)', value: p.prediction.model_output.up },
                        { label: 'prior P(flat)', value: p.prediction.rule_prior.flat },
                        { label: 'model P(flat)', value: p.prediction.model_output.flat },
                        { label: 'prior P(down)', value: p.prediction.rule_prior.down },
                        { label: 'model P(down)', value: p.prediction.model_output.down },
                      ]}
                      format={(v) => formatPercent(v)}
                      emptyMessage="Neither a rule prior nor a model output was recorded."
                    />
                    <p className="mt-1.5 text-[11px] leading-snug text-ink-500">
                      The served distribution is a blend of the two at weight{' '}
                      <span className="num">{formatNumber(p.prediction.blend_weight, 2)}</span>.
                    </p>
                  </div>

                  <div>
                    <h4 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                      Feature contributions to the model&apos;s log-odds
                    </h4>
                    {list(p.prediction.contributions).length === 0 ? (
                      <p className="text-xs text-ink-500">
                        No per-feature contributions were recorded for this
                        prediction, so the model&apos;s output cannot be attributed
                        to individual features here.
                      </p>
                    ) : (
                      <BarChart
                        data={list(p.prediction.contributions)
                          .slice()
                          .sort((a, b) => Math.abs(b.contribution) - Math.abs(a.contribution))
                          .slice(0, 8)
                          .map((c) => ({
                            label: c.feature,
                            value: c.contribution,
                            note: `= ${formatNumber(c.value, 3)}`,
                          }))}
                        format={(v) => formatNumber(v, 3)}
                      />
                    )}
                  </div>
                </div>
              </>
            )}

            {p.model_version ? (
              <div className="mt-4 rounded border border-ink-200 bg-ink-50 p-3">
                <p className="text-xs font-semibold text-ink-800">
                  Registry record for {p.model_version.version}
                </p>
                <DefinitionList
                  columns={3}
                  items={[
                    { term: 'Stage', value: p.model_version.stage || '—' },
                    { term: 'Family', value: p.model_version.family || '—' },
                    { term: 'Horizon', value: p.model_version.horizon || '—' },
                    {
                      term: 'Flat band',
                      value: formatBps(p.model_version.flat_band_bps),
                    },
                    {
                      term: 'Artifact digest',
                      value: <Hash value={p.model_version.artifact_sha} />,
                    },
                    {
                      term: 'Trained',
                      value: formatTimestamp(p.model_version.trained_at),
                    },
                    {
                      term: 'Test window',
                      value: `${formatTimestamp(p.model_version.test_start)} → ${formatTimestamp(p.model_version.test_end)}`,
                    },
                    {
                      term: 'Out-of-sample accuracy',
                      value: (
                        <>
                          {formatPercent(p.model_version.out_of_sample.accuracy)}{' '}
                          <span className="text-ink-500">
                            vs base rate{' '}
                            {formatPercent(p.model_version.out_of_sample.base_rate)}
                          </span>
                        </>
                      ),
                      hint: `over ${p.model_version.out_of_sample.samples.toLocaleString('en-US')} samples`,
                    },
                    {
                      term: 'Training commit',
                      value: p.model_version.training_commit || '—',
                    },
                  ]}
                />
              </div>
            ) : sig.model_version ? (
              <p className="mt-3 text-xs text-ink-500">
                Model version <span className="font-mono">{sig.model_version}</span> is
                not present in the registry, so its training window and
                out-of-sample record cannot be shown alongside this signal.
              </p>
            ) : null}
          </Step>

          <Step n={5} title="The regime in force">
            {!p.regime?.regime ? (
              <EmptyState
                title="No regime was recorded"
                detail="Signals store the regime in force so that per-regime performance can be measured later. Without it, this signal cannot be partitioned in evaluation."
              />
            ) : (
              <>
                <div className="flex flex-wrap items-baseline gap-3">
                  <p className="text-lg font-bold text-ink-900">
                    {humanizeEnum(p.regime.regime)}
                  </p>
                  <span className="num text-sm text-ink-700">
                    confidence {formatPercent(p.regime.confidence)}
                  </span>
                  {sig.regime && sig.regime !== p.regime.regime ? (
                    <span className="rounded border border-watch bg-amber-50 px-2 py-0.5 text-xs font-semibold text-watch">
                      <span aria-hidden="true">! </span>
                      The regime has changed since this signal was created (it was{' '}
                      {humanizeEnum(sig.regime)})
                    </span>
                  ) : null}
                </div>
                <p className="mt-1 max-w-prose text-xs leading-snug text-ink-500">
                  The regime shown here is the platform&apos;s <em>current</em>{' '}
                  classification. The signal itself records{' '}
                  <span className="font-mono">{sig.regime || 'none'}</span> as the
                  regime in force at the moment it was created.
                </p>
              </>
            )}
          </Step>

          <Step n={6} title="The risk decision and every check behind it">
            <RiskChecksTable assessment={p.risk_assessment} />
            {!p.risk_assessment && sig.risk_assessment_id ? (
              <p className="mt-2 text-xs text-ink-500">
                The signal references risk assessment{' '}
                <span className="font-mono">{sig.risk_assessment_id}</span>, which
                could not be retrieved. A signal cannot be constructed without an
                allowing assessment, so one existed — it is simply no longer
                available to show.
              </p>
            ) : null}
          </Step>

          <Step n={7} title="What would prove this wrong">
            {list(sig.invalidations).length === 0 ? (
              <div role="alert" className="rounded border border-deny bg-red-50 px-3 py-2 text-sm text-ink-800">
                <span className="font-semibold text-deny">
                  <span aria-hidden="true">✕ </span>No invalidation conditions.{' '}
                </span>
                Governance rule G-6 requires at least one, and{' '}
                <span className="font-mono">domain.NewSignal</span> refuses to build
                a signal without one. A signal displaying none here indicates a
                corrupted record.
              </div>
            ) : (
              <ul className="space-y-2">
                {list(sig.invalidations).map((inv, i) => (
                  <li key={i} className="rounded border border-ink-200 p-3">
                    <div className="flex flex-wrap items-baseline gap-2">
                      <span className="rounded bg-ink-900 px-1.5 py-0.5 font-mono text-[10px] font-bold uppercase text-white">
                        {inv.kind}
                      </span>
                      {Number.isFinite(inv.threshold) && inv.threshold !== 0 ? (
                        <span className="num text-sm font-semibold text-ink-900">
                          {formatNumber(inv.threshold, 4)}
                        </span>
                      ) : null}
                      {inv.regime_from ? (
                        <span className="text-xs text-ink-600">
                          from {humanizeEnum(inv.regime_from)}
                        </span>
                      ) : null}
                      {inv.deadline && !isZeroTime(inv.deadline) ? (
                        <span className="num text-xs text-ink-600">
                          by {formatTimestamp(inv.deadline)}
                        </span>
                      ) : null}
                    </div>
                    <p className="mt-1 text-sm text-ink-700">{inv.description}</p>
                  </li>
                ))}
              </ul>
            )}

            {sig.status === 'INVALIDATED' ? (
              <div className="mt-3 rounded border border-deny bg-red-50 px-3 py-2">
                <p className="text-sm font-semibold text-deny">
                  <span aria-hidden="true">✕ </span>This signal was invalidated
                </p>
                <p className="mt-0.5 text-sm text-ink-800">
                  {sig.invalidation_kind ? (
                    <span className="font-mono">{sig.invalidation_kind}</span>
                  ) : null}{' '}
                  {sig.invalidation_note}
                </p>
                {sig.invalidated_at ? (
                  <p className="num mt-0.5 text-xs text-ink-600">
                    at {formatTimestamp(sig.invalidated_at)} ({formatAge(sig.invalidated_at)})
                  </p>
                ) : null}
              </div>
            ) : null}
          </Step>

          <Step n={8} title="What actually happened">
            {list(p.outcomes).length === 0 ? (
              <EmptyState
                title="No outcome has been resolved yet"
                detail="An outcome is recorded once the prediction's horizon has elapsed and the realised move has been measured. Until then this signal is unscored — which is the honest state, not a missing number."
              />
            ) : (
              <div className="table-wrap">
                <table className="data-table">
                  <caption className="sr-only">
                    Resolved outcomes for the prediction behind this signal.
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Horizon</th>
                      <th scope="col">Predicted</th>
                      <th scope="col">Actual</th>
                      <th scope="col">Correct</th>
                      <th scope="col">Return</th>
                      <th scope="col">Brier</th>
                      <th scope="col">Resolved</th>
                    </tr>
                  </thead>
                  <tbody>
                    {list(p.outcomes).map((o, i) => (
                      <tr key={i}>
                        <td className="num text-xs">{o.horizon}</td>
                        <td className="font-semibold">{outcomeLabel(o.predicted)}</td>
                        <td className="font-semibold">{outcomeLabel(o.actual)}</td>
                        <td>
                          <span
                            className={`inline-flex items-center gap-1 text-xs font-semibold ${
                              o.correct ? 'text-allow' : 'text-deny'
                            }`}
                          >
                            <span aria-hidden="true">{o.correct ? '✓' : '✕'}</span>
                            {o.correct ? 'Correct' : 'Wrong'}
                          </span>
                        </td>
                        <td className="num text-xs">{formatBps(o.return_bps)}</td>
                        <td className="num text-xs">{formatNumber(o.brier_score, 4)}</td>
                        <td className="num whitespace-nowrap text-xs text-ink-600">
                          {formatTimestamp(o.resolved_at)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Step>

          <Step n={9} title="The narrative explanation">
            {!p.explanation && !sig.explanation ? (
              <EmptyState
                title="No AI explanation was produced"
                detail="The narrative is generated outside the decision path and is never required for a signal to be valid. Everything above is the actual record; the narrative would only restate it in prose."
              />
            ) : (
              <>
                <blockquote className="border-l-4 border-ink-300 bg-ink-50 px-4 py-3 text-sm leading-relaxed text-ink-800">
                  {p.explanation || sig.explanation}
                </blockquote>
                <p className="mt-2 text-[11px] leading-snug text-ink-500">
                  Written by{' '}
                  <span className="font-mono">{sig.explanation_source || 'the analyst component'}</span>{' '}
                  from the structured record above, after the decision was made. It
                  did not influence the decision, and it is validated against the
                  record so it cannot introduce a number that is not there. Read the
                  evidence above rather than this paragraph if the two ever disagree.
                </p>
              </>
            )}
          </Step>
        </ol>

        {/* --- Provenance identifiers -------------------------------- */}
        <Card
          title="Provenance identifiers"
          subtitle="Everything needed to re-derive this decision from stored state."
        >
          <DefinitionList
            columns={3}
            items={[
              { term: 'Signal id', value: <span className="font-mono text-xs">{sig.id}</span> },
              {
                term: 'Correlation id',
                value: <span className="font-mono text-xs">{sig.correlation_id || '—'}</span>,
                hint: 'Ties this signal to every event it produced.',
              },
              {
                term: 'Feature hash',
                value: <Hash value={sig.feature_hash} chars={16} label="feature hash" />,
                hint: 'Content hash of the exact inputs the model saw.',
              },
              {
                term: 'Prediction id',
                value: sig.prediction_id ? (
                  <Link href={`/predictions?ticker=${encodeURIComponent(sig.ticker)}`} className="link font-mono text-xs">
                    {sig.prediction_id}
                  </Link>
                ) : (
                  '—'
                ),
              },
              {
                term: 'Risk assessment id',
                value: <span className="font-mono text-xs">{sig.risk_assessment_id || '—'}</span>,
              },
              { term: 'Score ref', value: <span className="font-mono text-xs">{sig.score_ref || '—'}</span> },
              { term: 'Strategy id', value: <span className="font-mono text-xs">{sig.strategy_id || '—'}</span> },
              { term: 'Model version', value: <span className="font-mono text-xs">{sig.model_version || '—'}</span> },
              {
                term: 'Config hash',
                value: <Hash value={sig.config_hash} chars={16} label="config hash" />,
                hint: 'The effective configuration in force, so this decision survives a config change.',
              },
            ]}
          />
          <DisclaimerNote className="mt-3" text={sig.disclaimer} />
        </Card>
      </div>
    </>
  );
}

function Step({
  n,
  title,
  children,
}: {
  n: number;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <li className="card">
      <div className="card-header">
        <h2 className="flex items-baseline gap-2 text-sm font-semibold text-ink-900">
          <span
            aria-hidden="true"
            className="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-ink-900 text-[11px] font-bold text-white"
          >
            {n}
          </span>
          {title}
        </h2>
      </div>
      <div className="card-body">{children}</div>
    </li>
  );
}

function RefLevel({ label, value }: { label: string; value: number }) {
  return (
    <div className="rounded border border-ink-200 px-3 py-2">
      <p className="metric-label">{label}</p>
      <p className="num text-lg font-semibold text-ink-900">{formatPrice(value)}</p>
    </div>
  );
}

function FeatureSnapshotView({
  snapshot,
  signalHash,
}: {
  snapshot: NonNullable<Provenance['feature_snapshot']>;
  signalHash: string;
}) {
  const values = snapshot.values ?? {};
  const warm = snapshot.warm ?? {};
  const names = Object.keys(values).sort();
  const cold = names.filter((n) => warm[n] === false);
  const hashMatches = !signalHash || signalHash === snapshot.hash;

  return (
    <div className="space-y-3">
      {snapshot.stale ? (
        <StaleNotice reason={snapshot.stale_reason} asOf={formatTimestamp(snapshot.as_of)} />
      ) : null}

      {!hashMatches ? (
        <p role="alert" className="rounded border border-deny bg-red-50 px-3 py-2 text-xs text-ink-800">
          <span className="font-semibold text-deny">
            <span aria-hidden="true">✕ </span>Hash mismatch.{' '}
          </span>
          The signal records feature hash{' '}
          <span className="font-mono">{signalHash.slice(0, 16)}…</span> but the
          retrieved snapshot hashes to{' '}
          <span className="font-mono">{snapshot.hash.slice(0, 16)}…</span>. This is
          not the data the signal was computed from.
        </p>
      ) : null}

      <div className="flex flex-wrap gap-1.5">
        <Chip label="hash" value={<Hash value={snapshot.hash} chars={16} />} />
        <Chip label="as of" value={formatTimestamp(snapshot.as_of)} />
        <Chip label="interval" value={snapshot.interval} />
        <Chip label="schema" value={`v${snapshot.schema_version}`} />
        <Chip label="last tick" value={formatTimestamp(snapshot.last_tick_at)} />
      </div>

      <DefinitionList
        columns={3}
        items={[
          { term: 'Last', value: formatPrice(snapshot.last) },
          {
            term: 'Bid / Ask',
            value: `${formatPrice(snapshot.bid)} / ${formatPrice(snapshot.ask)}`,
          },
          { term: 'Spread', value: formatBps(snapshot.spread_bps) },
        ]}
      />

      {cold.length > 0 ? (
        <p className="rounded border border-watch bg-amber-50 px-3 py-2 text-xs leading-relaxed text-ink-800">
          <span className="font-semibold text-watch">
            <span aria-hidden="true">! </span>
            {cold.length} cold feature{cold.length === 1 ? '' : 's'}.{' '}
          </span>
          A cold feature&apos;s indicator has not seen enough data to be
          meaningful. Cold features are never fed to the model — they are marked
          below so a reader can see what the model did <em>not</em> have.
        </p>
      ) : null}

      <details className="rounded border border-ink-200">
        <summary className="cursor-pointer px-3 py-2 text-xs font-semibold text-ink-700">
          All {names.length} feature values ({cold.length} cold)
        </summary>
        <div className="table-wrap border-t border-ink-200">
          <table className="data-table">
            <caption className="sr-only">
              Every feature in the snapshot, with its value and whether the
              underlying indicator was warm.
            </caption>
            <thead>
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
                          <span aria-hidden="true">! </span>Cold — not used
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
      </details>
    </div>
  );
}

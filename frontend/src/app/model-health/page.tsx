import type { Metadata } from 'next';

import {
  BarChart,
  ConfusionMatrixTable,
  ReliabilityChart,
} from '@/components/charts';
import { Card, DefinitionList, Freshness, Metric, PageHeader } from '@/components/page';
import {
  DegradedBanner,
  EmptyState,
  ErrorState,
  InsufficientSamples,
} from '@/components/states';
import { DriftBadge } from '@/components/verdicts';
import {
  MIN_SAMPLES_FOR_RATE,
  accuracyVsBaseRate,
  formatCount,
  formatNumber,
  formatPercent,
  formatSignedBps,
  formatSignedPercent,
  formatTimestamp,
  humanizeEnum,
  list,
  sampleGate,
} from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';
import { ALL_OUTCOMES, type DriftReport, type VetoCost } from '@/lib/types';

export const metadata: Metadata = { title: 'Model health' };
export const dynamic = 'force-dynamic';

/**
 * The model-health page.
 *
 * The organising rule: **accuracy never appears alone.** Over three classes with
 * a FLAT band, always guessing the most common class already scores around 40%,
 * so a bare "42% accurate" reads as a failure and a bare "58% accurate" reads as
 * a triumph, when neither reading survives contact with the base rate. Every
 * accuracy on this page is rendered beside the constant-guess benchmark and its
 * lift, and any rate over too few samples is replaced by the reason it is not
 * being shown.
 */
export default async function ModelHealthPage() {
  const client = api();
  const [platform, healthR, evalR] = await Promise.all([
    platformStatus(),
    client.modelHealth(),
    client.evaluations({ limit: 1000 }),
  ]);

  const mh = healthR.ok ? healthR.data : null;
  const ev = evalR.ok ? evalR.data : null;
  const overall = mh?.overall ?? ev?.evaluation ?? null;

  const acc = accuracyVsBaseRate(
    overall?.accuracy,
    overall?.base_rate,
    overall?.samples,
  );

  const calibration = list(ev?.calibration ?? overall?.calibration);

  return (
    <>
      <PageHeader
        title="Model health"
        purpose="Whether the serving model is measurably better than guessing, whether its probabilities mean what they say, and whether the data it sees today still resembles the data it was trained on."
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />

        {/* --- Headline ---------------------------------------------- */}
        <Card
          title="Serving model"
          actions={<Freshness servedAt={healthR.ok ? healthR.servedAt : undefined} meta={healthR.ok ? healthR.meta : undefined} />}
        >
          {!healthR.ok ? (
            <ErrorState error={healthR} what="model health" />
          ) : !mh ? (
            <EmptyState title="No model health record was returned." />
          ) : (
            <div className="space-y-4">
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
                {mh.health.model_version ? (
                  <span className="rounded border border-ink-300 bg-ink-50 px-2 py-0.5 font-mono text-xs">
                    {mh.health.model_id} · {mh.health.model_version} · {mh.health.stage}
                  </span>
                ) : (
                  <span className="rounded border border-watch bg-amber-50 px-2 py-0.5 text-xs font-semibold text-watch">
                    <span aria-hidden="true">! </span>No model version is recorded —
                    nothing is being served from a trained artifact
                  </span>
                )}
                {mh.health.provisional ? (
                  <span className="rounded border border-watch bg-amber-50 px-2 py-0.5 text-xs font-semibold text-watch">
                    Provisional
                  </span>
                ) : null}
              </div>

              {mh.health.provisional ? (
                <p className="max-w-prose rounded border border-watch bg-amber-50 px-3 py-2 text-xs leading-relaxed text-ink-800">
                  <span className="font-semibold text-watch">
                    These figures are provisional.{' '}
                  </span>
                  They come from the model&apos;s own offline test split rather
                  than from live resolved predictions. The measurement is real —
                  it was made on data the model never saw — but it was made under
                  a different distribution, and it is replaced by live evidence as
                  soon as enough predictions resolve.
                </p>
              ) : null}

              {/* Accuracy beside the base rate. Never alone. */}
              {acc.gate.sufficient ? (
                <>
                  <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
                    <Metric
                      label="Accuracy"
                      value={acc.accuracy}
                      hint="Fraction of resolved predictions whose argmax class actually occurred."
                    />
                    <Metric
                      label="Base rate"
                      value={acc.baseRate}
                      hint="What always guessing the single most common class would have scored on the same data."
                    />
                    <Metric
                      label="Lift over base rate"
                      value={acc.lift}
                      sub={
                        acc.beatsBaseRate === null ? null : (
                          <span
                            className={
                              acc.beatsBaseRate
                                ? 'font-semibold text-allow'
                                : 'font-semibold text-deny'
                            }
                          >
                            <span aria-hidden="true">
                              {acc.beatsBaseRate ? '✓ ' : '✕ '}
                            </span>
                            {acc.beatsBaseRate
                              ? 'ahead of guessing'
                              : 'no better than guessing'}
                          </span>
                        )
                      }
                      hint="The only figure that says whether the model adds information."
                    />
                    <Metric
                      label="Resolved samples"
                      value={formatCount(acc.gate.samples)}
                      hint="Predictions whose horizon elapsed and were scored."
                    />
                  </div>

                  <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
                    <Metric
                      label="Brier score"
                      value={formatNumber(overall?.brier_score, 4)}
                      hint="Multiclass Brier. Lower is better; 0 perfect, 2 worst."
                    />
                    <Metric
                      label="Brier skill"
                      value={formatNumber(overall?.brier_skill_score, 4)}
                      hint="Brier score relative to the reference forecast. Above 0 is skill."
                    />
                    <Metric
                      label="Log loss"
                      value={formatNumber(overall?.log_loss, 4)}
                      hint="Negative log likelihood of the realised outcome."
                    />
                    <Metric
                      label="Expected calibration error"
                      value={formatPercent(overall?.expected_calibration_error)}
                      hint="Average gap between predicted probability and observed frequency."
                    />
                  </div>
                </>
              ) : (
                <>
                  <InsufficientSamples note={acc.gate.note} />
                  <p className="max-w-prose text-xs leading-relaxed text-ink-600">
                    Accuracy, Brier score and calibration are all rates over
                    resolved predictions. Until at least {MIN_SAMPLES_FOR_RATE} have
                    resolved, any number here would be noise dressed as a
                    measurement, so none is shown.
                    {typeof mh.pending === 'number' && mh.pending > 0 ? (
                      <>
                        {' '}
                        <span className="num font-semibold">
                          {formatCount(mh.pending)}
                        </span>{' '}
                        predictions are currently pending resolution.
                      </>
                    ) : null}
                  </p>
                </>
              )}
            </div>
          )}
        </Card>

        <div className="grid gap-4 xl:grid-cols-2">
          {/* --- Calibration ----------------------------------------- */}
          <Card
            title="Calibration"
            subtitle="Do the probabilities mean what they say? Of the times the model said 70%, did it happen 70% of the time?"
          >
            {!evalR.ok ? (
              <ErrorState error={evalR} what="the calibration report" />
            ) : calibration.length === 0 ? (
              <EmptyState
                title="No calibration bins"
                detail="A reliability diagram needs resolved predictions distributed across probability bins. None have resolved yet."
              />
            ) : (
              <div className="grid gap-4 sm:grid-cols-[auto_1fr] sm:items-start">
                <ReliabilityChart
                  bins={calibration.map((b) => ({
                    meanPredicted: b.mean_predicted,
                    observed: b.observed_frequency,
                    count: b.count,
                    lower: b.lower,
                    upper: b.upper,
                  }))}
                />
                <div className="table-wrap">
                  <table className="data-table min-w-0">
                    <caption className="sr-only">
                      Calibration bins for the UP class: predicted probability
                      against observed frequency.
                    </caption>
                    <thead>
                      <tr>
                        <th scope="col">Bin</th>
                        <th scope="col">n</th>
                        <th scope="col">Predicted</th>
                        <th scope="col">Observed</th>
                        <th scope="col">Deviation</th>
                      </tr>
                    </thead>
                    <tbody>
                      {calibration.map((b, i) => {
                        const thin = b.count > 0 && b.count < 10;
                        return (
                          <tr key={i} className={b.count === 0 ? 'text-ink-400' : undefined}>
                            <td className="num text-xs">
                              {formatPercent(b.lower, 0)}–{formatPercent(b.upper, 0)}
                            </td>
                            <td className="num text-xs">{b.count}</td>
                            <td className="num text-xs">
                              {b.count === 0 ? '—' : formatPercent(b.mean_predicted)}
                            </td>
                            <td className="num text-xs">
                              {b.count === 0 ? '—' : formatPercent(b.observed_frequency)}
                            </td>
                            <td className="num text-xs">
                              {b.count === 0 ? (
                                '—'
                              ) : (
                                <span
                                  className={
                                    Math.abs(b.deviation) > 0.1 && !thin
                                      ? 'font-semibold text-watch'
                                      : undefined
                                  }
                                >
                                  {formatSignedPercent(b.deviation, 1)}
                                  {thin ? (
                                    <span className="ml-1 text-[10px] text-ink-400">
                                      (n&lt;10)
                                    </span>
                                  ) : null}
                                </span>
                              )}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              </div>
            )}
            {overall ? (
              <p className="mt-3 text-xs text-ink-600">
                Maximum calibration deviation{' '}
                <span className="num font-semibold">
                  {formatPercent(overall.max_calibration_deviation)}
                </span>{' '}
                — this is the promotion gate in the governance charter.
              </p>
            ) : null}
          </Card>

          {/* --- Confusion matrix ------------------------------------ */}
          <Card
            title="Confusion matrix"
            subtitle="Which class the model said, against which class actually occurred."
          >
            {!overall ? (
              <EmptyState title="No evaluation was returned." />
            ) : (
              <>
                <ConfusionMatrixTable
                  counts={overall.confusion.counts as never}
                  total={overall.confusion.total}
                  classes={ALL_OUTCOMES}
                />
                {overall.confusion.total > 0 ? (
                  <DefinitionList
                    columns={2}
                    items={[
                      { term: 'Precision UP', value: formatPercent(overall.precision_up) },
                      { term: 'Recall UP', value: formatPercent(overall.recall_up) },
                      { term: 'Precision DOWN', value: formatPercent(overall.precision_down) },
                      { term: 'Recall DOWN', value: formatPercent(overall.recall_down) },
                      { term: 'Macro F1', value: formatPercent(overall.macro_f1) },
                      { term: 'Total', value: formatCount(overall.confusion.total) },
                    ]}
                  />
                ) : null}
              </>
            )}
          </Card>
        </div>

        {/* --- Per-regime ------------------------------------------- */}
        <Card
          title="Accuracy by regime"
          subtitle="A model that works only in one environment is not a working model. Each regime is shown against its own base rate."
        >
          <PerRegime byRegime={mh?.by_regime ?? null} />
        </Card>

        {/* --- Drift ------------------------------------------------- */}
        <Card
          title="Drift"
          subtitle="Whether the data the model sees today still resembles what it was trained on."
        >
          {!mh ? (
            <EmptyState title="No drift report." />
          ) : (
            <DriftReportView drift={mh.drift} />
          )}
        </Card>

        {/* --- Veto cost -------------------------------------------- */}
        <Card
          title="What the risk veto cost"
          subtitle="Comparing the predictions the risk engine allowed against those it blocked. This is what makes the veto falsifiable rather than superstitious."
        >
          {!mh?.veto_cost ? (
            <EmptyState
              title="No veto-cost measurement"
              detail="This comparison needs resolved outcomes on both sides of the veto. None are available yet."
            />
          ) : (
            <VetoCostView cost={mh.veto_cost} />
          )}
        </Card>
      </div>
    </>
  );
}

function PerRegime({
  byRegime,
}: {
  byRegime: Partial<Record<string, { accuracy: number; base_rate: number; samples: number; brier_score: number }>> | null;
}) {
  const entries = Object.entries(byRegime ?? {}).filter(([, v]) => v);
  if (entries.length === 0) {
    return (
      <EmptyState
        title="No per-regime breakdown"
        detail="The evaluation layer partitions by regime once each partition holds enough resolved predictions. None qualify yet — which is itself worth knowing: nothing can be said about how this model behaves in any particular environment."
      />
    );
  }

  return (
    <div className="table-wrap">
      <table className="data-table">
        <caption className="sr-only">
          Accuracy per market regime, each beside the base rate for that regime.
        </caption>
        <thead>
          <tr>
            <th scope="col">Regime</th>
            <th scope="col">Samples</th>
            <th scope="col">Accuracy</th>
            <th scope="col">Base rate</th>
            <th scope="col">Lift</th>
            <th scope="col">Brier</th>
            <th scope="col">Verdict</th>
          </tr>
        </thead>
        <tbody>
          {entries.map(([regime, e]) => {
            const m = e as { accuracy: number; base_rate: number; samples: number; brier_score: number };
            const a = accuracyVsBaseRate(m.accuracy, m.base_rate, m.samples);
            return (
              <tr key={regime}>
                <th scope="row" className="font-medium">
                  {humanizeEnum(regime)}
                </th>
                <td className="num">{formatCount(m.samples)}</td>
                <td className="num font-semibold">{a.accuracy}</td>
                <td className="num">{a.baseRate}</td>
                <td className="num">{a.lift}</td>
                <td className="num">{a.gate.sufficient ? formatNumber(m.brier_score, 4) : '—'}</td>
                <td className="text-xs">
                  {!a.gate.sufficient ? (
                    <span className="text-ink-500">{a.gate.note}</span>
                  ) : a.beatsBaseRate ? (
                    <span className="font-semibold text-allow">
                      <span aria-hidden="true">✓ </span>Beats guessing
                    </span>
                  ) : (
                    <span className="font-semibold text-deny">
                      <span aria-hidden="true">✕ </span>Below the base rate
                    </span>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function DriftReportView({ drift }: { drift: DriftReport }) {
  const gate = sampleGate(drift.samples);
  const features = Object.entries(drift.feature_drift ?? {})
    .sort((a, b) => b[1] - a[1])
    .slice(0, 12);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <DriftBadge severity={drift.severity} />
        {drift.model_version ? (
          <span className="rounded border border-ink-300 bg-ink-50 px-2 py-0.5 font-mono text-xs">
            {drift.model_version}
          </span>
        ) : null}
        <span className="text-xs text-ink-600">
          computed {formatTimestamp(drift.computed_at)}
        </span>
      </div>

      {!gate.sufficient ? (
        <InsufficientSamples
          note={`${gate.note} Drift is measured over a rolling window of resolved predictions; without them there is no distribution to compare against the reference.`}
        />
      ) : (
        <>
          {list(drift.reasons).length > 0 ? (
            <div className="rounded border border-watch/50 bg-amber-50 px-3 py-2">
              <h3 className="text-xs font-bold uppercase tracking-wide text-watch">
                <span aria-hidden="true">! </span>Why drift was flagged
              </h3>
              <ul className="mt-1 space-y-0.5 text-sm text-ink-800">
                {list(drift.reasons).map((r, i) => (
                  <li key={i}>· {r}</li>
                ))}
              </ul>
            </div>
          ) : null}

          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <Metric
              label="Reference accuracy"
              value={formatPercent(drift.reference_accuracy)}
              hint="On the window the model was validated against."
            />
            <Metric
              label="Recent accuracy"
              value={formatPercent(drift.recent_accuracy)}
              hint="On the most recent resolved window."
            />
            <Metric
              label="Accuracy delta"
              value={formatSignedPercent(drift.accuracy_delta)}
              hint="Negative means live performance has fallen behind the reference."
            />
            <Metric
              label="Max feature PSI"
              value={formatNumber(drift.max_feature_psi, 3)}
              hint="Population stability index. Above 0.25 is the conventional significant-shift threshold."
            />
          </div>

          {features.length > 0 ? (
            <div>
              <h3 className="mb-1.5 text-xs font-bold uppercase tracking-wide text-ink-600">
                Feature drift (population stability index)
              </h3>
              <BarChart
                data={features.map(([name, psi]) => ({ label: name, value: psi }))}
                format={(v) => formatNumber(v, 3)}
                reference={0.25}
                referenceLabel="Significant-shift threshold"
              />
            </div>
          ) : (
            <p className="text-xs text-ink-500">
              No per-feature PSI values were recorded, so it cannot be said which
              inputs moved.
            </p>
          )}

          {drift.recommendation ? (
            <p className="rounded border border-ink-300 bg-ink-50 px-3 py-2 text-sm text-ink-800">
              <span className="font-semibold">Recommendation: </span>
              {drift.recommendation}
            </p>
          ) : null}
        </>
      )}
    </div>
  );
}

function VetoCostView({ cost }: { cost: VetoCost }) {
  const blockedGate = sampleGate(cost.blocked);
  const allowedGate = sampleGate(cost.allowed);

  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-3">
        <Metric label="Allowed" value={formatCount(cost.allowed)} hint="Predictions the risk engine permitted to become signals." />
        <Metric label="Watch only" value={formatCount(cost.watch_only)} hint="Permitted to be displayed, not to become a signal." />
        <Metric label="Blocked" value={formatCount(cost.blocked)} hint="Refused outright." />
      </div>

      <div className="table-wrap">
        <table className="data-table">
          <caption className="sr-only">
            Accuracy and mean return of the predictions the risk engine allowed
            against those it blocked.
          </caption>
          <thead>
            <tr>
              <th scope="col">Group</th>
              <th scope="col">n</th>
              <th scope="col">Accuracy</th>
              <th scope="col">Mean directional return</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <th scope="row">Allowed</th>
              <td className="num">{formatCount(cost.allowed)}</td>
              <td className="num">
                {allowedGate.sufficient ? formatPercent(cost.allowed_accuracy) : '—'}
              </td>
              <td className="num">
                {allowedGate.sufficient ? formatSignedBps(cost.allowed_mean_return_bps) : '—'}
              </td>
            </tr>
            <tr>
              <th scope="row">Blocked</th>
              <td className="num">{formatCount(cost.blocked)}</td>
              <td className="num">
                {blockedGate.sufficient ? formatPercent(cost.blocked_accuracy) : '—'}
              </td>
              <td className="num">
                {blockedGate.sufficient ? formatSignedBps(cost.blocked_mean_return_bps) : '—'}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      {!blockedGate.sufficient || !allowedGate.sufficient ? (
        <InsufficientSamples
          note={`The comparison needs enough resolved outcomes on both sides. Allowed: ${cost.allowed}. Blocked: ${cost.blocked}. Until both clear ${MIN_SAMPLES_FOR_RATE}, the difference between them is not measurable.`}
        />
      ) : null}

      {cost.interpretation ? (
        <p className="max-w-prose rounded border border-ink-300 bg-ink-50 px-3 py-2 text-sm leading-relaxed text-ink-800">
          {cost.interpretation}
        </p>
      ) : null}

      <p className="max-w-prose text-xs leading-relaxed text-ink-600">
        If the blocked predictions turn out to have been at least as accurate and
        as profitable as the allowed ones, the veto is costing information rather
        than preventing loss. That is the question this table exists to answer,
        and it is meant to be answerable against the platform, not asserted.
      </p>
    </div>
  );
}

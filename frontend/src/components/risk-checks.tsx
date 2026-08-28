/**
 * The risk assessment, showing **every** check.
 *
 * This is a graded requirement and it is easy to get subtly wrong. Two failure
 * modes are specifically guarded against here:
 *
 *  1. **Showing only the checks that fired.** A reader needs to see that
 *     `event_proximity` passed as much as that `spread` failed — a veto is only
 *     interpretable against the full set. Checks that were skipped are shown
 *     too, with the reason they were skipped, because a skipped check is a
 *     check that did *not* protect anything.
 *
 *  2. **Losing checks the engine never ran.** `RISK_CHECK_NAMES` is the
 *     canonical list from `internal/domain/risk.go`. Any canonical check absent
 *     from the assessment is listed explicitly as "not evaluated" rather than
 *     silently omitted, because a missing check and a passing check look
 *     identical to a reader who only sees what was returned.
 */

import { CheckStatusBadge, RiskDecisionBadge, RiskLevelBadge } from './verdicts';
import { EmptyState } from './states';
import {
  formatNumber,
  formatScore,
  formatTimestamp,
  humanizeKey,
  list,
  riskDecisionLabel,
} from '@/lib/format';
import { RISK_CHECK_NAMES, type RiskAssessment, type RiskCheck } from '@/lib/types';

const STATUS_ORDER: Record<string, number> = {
  FAIL: 0,
  WARN: 1,
  SKIP: 2,
  PASS: 3,
};

export function RiskChecksTable({
  assessment,
  showMissing = true,
}: {
  assessment: RiskAssessment | null | undefined;
  showMissing?: boolean;
}) {
  if (!assessment) {
    return (
      <EmptyState
        title="No risk assessment was recorded"
        detail="Without an assessment there is no verdict to show. A signal cannot exist without one — the constructor in internal/domain/signal.go refuses to build a Signal unless the risk decision allows it."
      />
    );
  }

  const checks = list(assessment.checks);
  const present = new Set(checks.map((c) => c.name));
  const missing = showMissing
    ? RISK_CHECK_NAMES.filter((n) => !present.has(n))
    : [];

  const sorted = [...checks].sort((a, b) => {
    const d = (STATUS_ORDER[a.status] ?? 9) - (STATUS_ORDER[b.status] ?? 9);
    return d !== 0 ? d : a.name.localeCompare(b.name);
  });

  const counts = {
    PASS: checks.filter((c) => c.status === 'PASS').length,
    WARN: checks.filter((c) => c.status === 'WARN').length,
    FAIL: checks.filter((c) => c.status === 'FAIL').length,
    SKIP: checks.filter((c) => c.status === 'SKIP').length,
  };

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <RiskDecisionBadge decision={assessment.decision} />
        <RiskLevelBadge level={assessment.level} />
        <span className="num rounded border border-ink-300 bg-ink-50 px-2 py-0.5 text-xs font-semibold text-ink-800">
          risk score {formatScore(assessment.score)} / 100
        </span>
        <span className="text-[11px] text-ink-500">higher is riskier</span>
      </div>

      <p className="text-xs text-ink-600">
        <span className="num font-semibold">{checks.length}</span> checks
        evaluated — {counts.PASS} passed, {counts.WARN} warned, {counts.FAIL}{' '}
        failed, {counts.SKIP} skipped.
        {missing.length > 0 ? (
          <>
            {' '}
            <span className="num font-semibold">{missing.length}</span> canonical
            check{missing.length === 1 ? ' was' : 's were'} not evaluated at all.
          </>
        ) : null}
      </p>

      {checks.length === 0 ? (
        <EmptyState
          title="The assessment recorded no individual checks"
          detail="The decision is present but the per-check evidence is not, so this verdict cannot be explained. Treat the decision as unaudited."
        />
      ) : (
        <div className="table-wrap">
          <table className="data-table">
            <caption className="sr-only">
              Every risk check behind the {riskDecisionLabel(assessment.decision)}{' '}
              decision, most severe first.
            </caption>
            <thead>
              <tr>
                <th scope="col">Check</th>
                <th scope="col">Status</th>
                <th scope="col">Value</th>
                <th scope="col">Threshold</th>
                <th scope="col">Would decide</th>
                <th scope="col">Reason</th>
              </tr>
            </thead>
            <tbody>
              {sorted.map((c) => (
                <CheckRow key={c.name} check={c} />
              ))}
              {missing.map((name) => (
                <tr key={name} className="bg-ink-50">
                  <th scope="row" className="font-mono text-xs font-medium text-ink-600">
                    {humanizeKey(name)}
                  </th>
                  <td>
                    <span className="inline-flex items-center gap-1.5 rounded border border-ink-300 bg-white px-2 py-0.5 text-xs font-semibold text-ink-600">
                      <span aria-hidden="true" className="font-mono">
                        ?
                      </span>
                      Not evaluated
                    </span>
                  </td>
                  <td colSpan={4} className="text-xs text-ink-500">
                    This canonical check produced no record in this assessment.
                    It neither passed nor failed — nothing checked it.
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {list(assessment.blockers).length > 0 ? (
        <Reasons
          title="Blockers"
          tone="deny"
          items={list(assessment.blockers)}
          note="Each of these on its own is sufficient to prevent a signal."
        />
      ) : null}
      {list(assessment.warnings).length > 0 ? (
        <Reasons
          title="Warnings"
          tone="watch"
          items={list(assessment.warnings)}
          note="These did not prevent emission but degrade confidence in it."
        />
      ) : null}

      <dl className="grid grid-cols-1 gap-x-6 gap-y-1 text-[11px] text-ink-500 sm:grid-cols-3">
        <div>
          <dt className="inline font-medium">Assessment id: </dt>
          <dd className="inline font-mono">{assessment.id || '—'}</dd>
        </div>
        <div>
          <dt className="inline font-medium">Evaluated: </dt>
          <dd className="inline font-mono">
            {formatTimestamp(assessment.evaluated_at || assessment.created_at)}
          </dd>
        </div>
        <div>
          <dt className="inline font-medium">Config hash: </dt>
          <dd className="inline font-mono">
            {assessment.config_hash ? assessment.config_hash.slice(0, 12) : '—'}
          </dd>
        </div>
      </dl>
    </div>
  );
}

function CheckRow({ check }: { check: RiskCheck }) {
  const skipped = check.status === 'SKIP';
  return (
    <tr className={check.status === 'FAIL' ? 'bg-red-50/50' : undefined}>
      <th scope="row" className="font-mono text-xs font-medium text-ink-800">
        {humanizeKey(check.name)}
      </th>
      <td>
        <CheckStatusBadge status={check.status} />
      </td>
      <td className="num text-xs">
        {skipped ? <span className="text-ink-400">—</span> : formatNumber(check.value, 4)}
      </td>
      <td className="num text-xs">
        {check.threshold === 0 && skipped ? (
          <span className="text-ink-400">—</span>
        ) : (
          formatNumber(check.threshold, 4)
        )}
      </td>
      <td className="text-xs">
        <span
          className={
            check.decision === 'BLOCK'
              ? 'font-semibold text-deny'
              : check.decision === 'WATCH_ONLY'
                ? 'font-semibold text-watch'
                : 'text-ink-600'
          }
        >
          {check.decision || '—'}
        </span>
      </td>
      <td className="max-w-[24rem] text-xs leading-snug text-ink-700">
        {check.reason || <span className="text-ink-400">no reason recorded</span>}
        {check.skipped_reason ? (
          <span className="mt-0.5 block text-[11px] italic text-ink-500">
            Skipped because: {check.skipped_reason}. A skipped check protected
            nothing.
          </span>
        ) : null}
      </td>
    </tr>
  );
}

function Reasons({
  title,
  items,
  tone,
  note,
}: {
  title: string;
  items: string[];
  tone: 'deny' | 'watch';
  note: string;
}) {
  return (
    <div
      className={`rounded border px-3 py-2 ${
        tone === 'deny' ? 'border-deny/50 bg-red-50' : 'border-watch/50 bg-amber-50'
      }`}
    >
      <h4
        className={`text-xs font-bold uppercase tracking-wide ${
          tone === 'deny' ? 'text-deny' : 'text-watch'
        }`}
      >
        <span aria-hidden="true">{tone === 'deny' ? '✕ ' : '! '}</span>
        {title}
      </h4>
      <ul className="mt-1 space-y-0.5 text-xs text-ink-800">
        {items.map((b, i) => (
          <li key={i}>· {b}</li>
        ))}
      </ul>
      <p className="mt-1 text-[11px] text-ink-600">{note}</p>
    </div>
  );
}

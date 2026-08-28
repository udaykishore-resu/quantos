/**
 * The failure and emptiness states, as first-class components.
 *
 * Every remote read in this dashboard has five outcomes that are not "here is
 * your data": loading, empty, error, stale, and degraded. Each one gets a
 * component here so that no page can accidentally render nothing and call it a
 * chart.
 */

import type { ApiFailure } from '@/lib/api-client';
import { NOT_MEASURED } from '@/lib/format';

export function LoadingState({
  label = 'Loading',
  rows = 3,
}: {
  label?: string;
  rows?: number;
}) {
  return (
    <div role="status" aria-live="polite" className="space-y-2 p-1">
      <span className="sr-only">{label}…</span>
      {Array.from({ length: rows }).map((_, i) => (
        <div
          key={i}
          aria-hidden="true"
          className="h-4 w-full animate-pulse rounded bg-ink-100"
          style={{ width: `${100 - i * 12}%` }}
        />
      ))}
      <p aria-hidden="true" className="pt-1 text-xs text-ink-500">
        {label}…
      </p>
    </div>
  );
}

/**
 * An empty result. The distinction this component insists on is between "the
 * platform looked and found none" and "the platform could not look", because a
 * reader draws opposite conclusions from them.
 */
export function EmptyState({
  title,
  detail,
  action,
}: {
  title: string;
  detail?: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="rounded-md border border-dashed border-ink-300 bg-ink-50 px-4 py-6 text-center">
      <p className="text-sm font-medium text-ink-700">{title}</p>
      {detail ? (
        <p className="mx-auto mt-1 max-w-prose text-xs leading-relaxed text-ink-500">
          {detail}
        </p>
      ) : null}
      {action ? <div className="mt-3">{action}</div> : null}
    </div>
  );
}

export function ErrorState({
  error,
  what,
  onRetry,
}: {
  error: ApiFailure;
  what: string;
  onRetry?: () => void;
}) {
  return (
    <div
      role="alert"
      className="rounded-md border border-deny/40 bg-red-50 px-4 py-3 text-sm"
    >
      <p className="font-semibold text-deny">
        <span aria-hidden="true">✕ </span>
        Could not load {what}
      </p>
      <p className="mt-1 text-ink-700">{error.message}</p>
      {error.detail ? (
        <p className="mt-1 break-words font-mono text-xs text-ink-600">{error.detail}</p>
      ) : null}
      <dl className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-500">
        <div className="flex gap-1">
          <dt>Code</dt>
          <dd className="font-mono">{error.code}</dd>
        </div>
        <div className="flex gap-1">
          <dt>HTTP</dt>
          <dd className="font-mono">{error.status || 'n/a'}</dd>
        </div>
        {error.requestId ? (
          <div className="flex gap-1">
            <dt>Request</dt>
            <dd className="font-mono">{error.requestId}</dd>
          </div>
        ) : null}
      </dl>
      {error.retryable ? (
        <p className="mt-2 text-xs text-ink-600">
          This looks transient. The platform may be starting up or a backing
          service may be unavailable.
        </p>
      ) : null}
      {onRetry ? (
        <button type="button" className="btn mt-3" onClick={onRetry}>
          Retry
        </button>
      ) : null}
    </div>
  );
}

/**
 * Marks a payload the server itself flagged as stale (`meta.stale`), or one the
 * dashboard knows is older than it should be.
 */
export function StaleNotice({
  reason,
  asOf,
  className = '',
}: {
  reason?: string;
  asOf?: string;
  className?: string;
}) {
  return (
    <p
      className={`flex flex-wrap items-baseline gap-x-2 rounded border border-watch/40 bg-amber-50 px-2 py-1 text-xs text-ink-800 ${className}`}
    >
      <span className="font-semibold text-watch">
        <span aria-hidden="true">! </span>Stale data
      </span>
      <span>
        {reason ||
          'The platform marked this reading stale; it may not reflect the current market.'}
      </span>
      {asOf ? <span className="font-mono text-ink-600">as of {asOf}</span> : null}
    </p>
  );
}

/**
 * The degraded-platform banner.
 *
 * `degraded` names the subsystems that were unavailable when the server
 * answered. It is on every envelope, and it is the difference between "the
 * model says FLAT" and "there is no model, so this is the rule prior".
 */
export function DegradedBanner({
  degraded,
  ready = true,
  reason = '',
  className = '',
}: {
  degraded: string[];
  ready?: boolean;
  reason?: string;
  className?: string;
}) {
  const has = degraded.length > 0;
  if (!has && ready) return null;

  const severe = !ready;
  return (
    <div
      role="alert"
      className={`rounded-md border px-4 py-3 ${
        severe
          ? 'border-deny bg-red-50'
          : 'border-watch bg-amber-50'
      } ${className}`}
    >
      <p
        className={`text-sm font-semibold ${severe ? 'text-deny' : 'text-watch'}`}
      >
        <span aria-hidden="true">{severe ? '✕ ' : '! '}</span>
        {severe
          ? 'Platform is not ready — signal emission is suspended'
          : 'Platform is running degraded'}
      </p>
      {severe && reason ? (
        <p className="mt-1 text-sm text-ink-800">{reason}</p>
      ) : null}
      {has ? (
        <p className="mt-1 text-sm text-ink-800">
          Unavailable {degraded.length === 1 ? 'subsystem' : 'subsystems'}:{' '}
          <span className="font-mono font-medium">{degraded.join(', ')}</span>.
        </p>
      ) : null}
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-ink-600">
        {severe
          ? 'While the platform is not ready it does not emit new signals. Anything shown below was produced before this point and is retained for reference only.'
          : 'Readings below were produced without these subsystems. Where a model is unavailable, predictions fall back to the deterministic rule prior — check each prediction’s source before reading it as a model output.'}
      </p>
    </div>
  );
}

/** A dash with an accessible explanation, for a single unmeasured value. */
export function NotMeasured({ reason }: { reason?: string }) {
  return (
    <span className="text-ink-400" title={reason ?? 'Not measured'}>
      <span aria-hidden="true">{NOT_MEASURED}</span>
      <span className="sr-only">{reason ?? 'not measured'}</span>
    </span>
  );
}

/**
 * The "not enough data" state for a metric.
 *
 * A rate over three observations is not a rate. This renders the reason in
 * place of the number, which is the only honest option.
 */
export function InsufficientSamples({
  note,
  className = '',
}: {
  note: string;
  className?: string;
}) {
  return (
    <p
      className={`rounded border border-ink-300 bg-ink-50 px-2 py-1.5 text-xs leading-snug text-ink-600 ${className}`}
    >
      <span className="font-semibold">Not enough data. </span>
      {note}
    </p>
  );
}

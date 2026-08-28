import { formatAge, formatTimestamp, isZeroTime } from '@/lib/format';
import type { Meta } from '@/lib/types';

export function PageHeader({
  title,
  purpose,
  actions,
  children,
}: {
  title: string;
  /** One sentence saying what question this page answers. */
  purpose: string;
  actions?: React.ReactNode;
  children?: React.ReactNode;
}) {
  return (
    <header className="mb-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-xl font-bold tracking-tight text-ink-900">{title}</h1>
          <p className="mt-0.5 max-w-prose text-sm leading-relaxed text-ink-600">
            {purpose}
          </p>
        </div>
        {actions ? <div className="no-print flex gap-2">{actions}</div> : null}
      </div>
      {children ? <div className="mt-3">{children}</div> : null}
    </header>
  );
}

export function Card({
  title,
  subtitle,
  actions,
  children,
  className = '',
  bodyClassName = 'card-body',
}: {
  title?: string;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  bodyClassName?: string;
}) {
  return (
    <section className={`card ${className}`}>
      {title ? (
        <div className="card-header">
          <div className="min-w-0">
            <h2 className="card-title">{title}</h2>
            {subtitle ? (
              <p className="mt-0.5 text-xs text-ink-500">{subtitle}</p>
            ) : null}
          </div>
          {actions ? <div className="shrink-0">{actions}</div> : null}
        </div>
      ) : null}
      <div className={bodyClassName}>{children}</div>
    </section>
  );
}

/**
 * A labelled metric. `hint` is always rendered for screen readers, so the
 * meaning of a number is never available only on hover.
 */
export function Metric({
  label,
  value,
  hint,
  sub,
  className = '',
}: {
  label: string;
  value: React.ReactNode;
  hint?: string;
  sub?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={className}>
      <p className="metric-label">{label}</p>
      <p className="metric-value">{value}</p>
      {sub ? <div className="mt-0.5 text-xs text-ink-600">{sub}</div> : null}
      {hint ? (
        <p className="mt-0.5 max-w-prose text-[11px] leading-snug text-ink-500">
          {hint}
        </p>
      ) : null}
    </div>
  );
}

export function DefinitionList({
  items,
  columns = 2,
}: {
  items: { term: string; value: React.ReactNode; hint?: string }[];
  columns?: 1 | 2 | 3 | 4;
}) {
  const cols =
    columns === 1
      ? 'sm:grid-cols-1'
      : columns === 3
        ? 'sm:grid-cols-3'
        : columns === 4
          ? 'sm:grid-cols-2 lg:grid-cols-4'
          : 'sm:grid-cols-2';
  return (
    <dl className={`grid grid-cols-1 gap-x-6 gap-y-2 ${cols}`}>
      {items.map((it) => (
        <div key={it.term} className="min-w-0 border-b border-ink-100 pb-1.5">
          <dt className="text-[11px] font-medium uppercase tracking-wide text-ink-500">
            {it.term}
          </dt>
          <dd className="break-words text-sm text-ink-900">{it.value}</dd>
          {it.hint ? (
            <p className="text-[11px] leading-snug text-ink-500">{it.hint}</p>
          ) : null}
        </div>
      ))}
    </dl>
  );
}

/**
 * Freshness for a payload: when it was served, and how old the underlying
 * observation is. Both matter — a response served a second ago can carry a
 * reading from an hour ago.
 */
export function Freshness({
  servedAt,
  meta,
  className = '',
}: {
  servedAt?: string;
  meta?: Meta;
  className?: string;
}) {
  const asOf = meta?.as_of;
  return (
    <p className={`flex flex-wrap gap-x-3 text-[11px] text-ink-500 ${className}`}>
      {servedAt && !isZeroTime(servedAt) ? (
        <span>
          served <span className="num">{formatTimestamp(servedAt)}</span>
        </span>
      ) : null}
      {asOf && !isZeroTime(asOf) ? (
        <span>
          data as of <span className="num">{formatTimestamp(asOf)}</span> (
          {formatAge(asOf)})
        </span>
      ) : (
        <span className="text-ink-400">no as-of timestamp reported</span>
      )}
      {typeof meta?.count === 'number' ? (
        <span className="num">{meta.count} rows</span>
      ) : null}
    </p>
  );
}

/** A monospace hash with a copy-friendly title and a truncated display. */
export function Hash({
  value,
  chars = 12,
  label,
}: {
  value: string | null | undefined;
  chars?: number;
  label?: string;
}) {
  if (!value) {
    return <span className="text-ink-400">not recorded</span>;
  }
  const short = value.length > chars ? `${value.slice(0, chars)}…` : value;
  return (
    <span className="font-mono text-xs" title={`${label ? `${label}: ` : ''}${value}`}>
      {short}
      <span className="sr-only"> (full value {value})</span>
    </span>
  );
}

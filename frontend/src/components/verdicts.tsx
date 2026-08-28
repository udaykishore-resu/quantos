/**
 * Verdict and label components.
 *
 * The rule these all obey: **colour is never the only carrier of meaning.**
 * A red/green risk decision fails WCAG 1.4.1 and fails a colour-blind reader
 * outright, so every badge here pairs its colour with a glyph and the full
 * text of the verdict. `ALLOW_PAPER_SIGNAL` reads "Allowed (paper signal)"
 * whether or not any colour renders at all.
 */

import {
  checkStatusGlyph,
  checkStatusLabel,
  humanizeEnum,
  riskDecisionGlyph,
  riskDecisionLabel,
} from '@/lib/format';
import type {
  CheckStatus,
  Classification,
  DriftSeverity,
  RiskDecision,
  RiskLevel,
  SignalStatus,
  Side,
} from '@/lib/types';

const BASE =
  'inline-flex items-center gap-1.5 rounded border px-2 py-0.5 text-xs font-semibold';

export function RiskDecisionBadge({
  decision,
  className = '',
}: {
  decision: RiskDecision | '' | null | undefined;
  className?: string;
}) {
  const tone =
    decision === 'ALLOW_PAPER_SIGNAL'
      ? 'border-allow bg-teal-50 text-allow'
      : decision === 'WATCH_ONLY'
        ? 'border-watch bg-amber-50 text-watch'
        : decision === 'BLOCK'
          ? 'border-deny bg-red-50 text-deny'
          : 'border-ink-300 bg-ink-50 text-ink-600';
  return (
    <span className={`${BASE} ${tone} ${className}`}>
      <span aria-hidden="true" className="font-mono">
        {riskDecisionGlyph(decision)}
      </span>
      {riskDecisionLabel(decision)}
    </span>
  );
}

export function CheckStatusBadge({
  status,
  className = '',
}: {
  status: CheckStatus | '' | null | undefined;
  className?: string;
}) {
  const tone =
    status === 'PASS'
      ? 'border-allow bg-teal-50 text-allow'
      : status === 'WARN'
        ? 'border-watch bg-amber-50 text-watch'
        : status === 'FAIL'
          ? 'border-deny bg-red-50 text-deny'
          : 'border-ink-300 bg-ink-50 text-ink-600';
  return (
    <span className={`${BASE} ${tone} ${className}`}>
      <span aria-hidden="true" className="font-mono">
        {checkStatusGlyph(status)}
      </span>
      {checkStatusLabel(status)}
    </span>
  );
}

export function RiskLevelBadge({ level }: { level: RiskLevel | '' | null | undefined }) {
  const glyph =
    level === 'LOW' ? '▁' : level === 'MEDIUM' ? '▄' : level === 'HIGH' ? '▆' : level === 'EXTREME' ? '█' : '?';
  const tone =
    level === 'LOW'
      ? 'border-allow bg-teal-50 text-allow'
      : level === 'MEDIUM'
        ? 'border-ink-400 bg-ink-50 text-ink-700'
        : level === 'HIGH'
          ? 'border-watch bg-amber-50 text-watch'
          : level === 'EXTREME'
            ? 'border-deny bg-red-50 text-deny'
            : 'border-ink-300 bg-ink-50 text-ink-500';
  return (
    <span className={`${BASE} ${tone}`}>
      <span aria-hidden="true" className="font-mono">
        {glyph}
      </span>
      {level ? `${humanizeEnum(level)} risk` : 'Risk not assessed'}
    </span>
  );
}

/**
 * A research classification. The wording deliberately keeps the platform's own
 * vocabulary — these are labels, not recommendations, and softening
 * `BULLISH_SETUP` into "Buy" would be exactly the misrepresentation the
 * governance rules forbid.
 */
export function ClassificationBadge({
  classification,
}: {
  classification: Classification | '' | null | undefined;
}) {
  const tone =
    classification === 'STRONG_WATCH' || classification === 'BULLISH_SETUP'
      ? 'border-allow bg-teal-50 text-allow'
      : classification === 'HIGH_RISK'
        ? 'border-watch bg-amber-50 text-watch'
        : classification === 'AVOID'
          ? 'border-deny bg-red-50 text-deny'
          : 'border-ink-300 bg-ink-50 text-ink-700';
  return (
    <span className={`${BASE} ${tone}`}>
      {classification ? humanizeEnum(classification) : 'Unclassified'}
    </span>
  );
}

export function SideBadge({ side }: { side: Side | '' | null | undefined }) {
  const glyph = side === 'LONG' ? '▲' : side === 'SHORT' ? '▼' : '■';
  const tone =
    side === 'LONG'
      ? 'border-up bg-teal-50 text-up'
      : side === 'SHORT'
        ? 'border-down bg-red-50 text-down'
        : 'border-ink-300 bg-ink-50 text-ink-600';
  return (
    <span className={`${BASE} ${tone}`}>
      <span aria-hidden="true" className="font-mono">
        {glyph}
      </span>
      {side || 'No bias'}
    </span>
  );
}

export function SignalStatusBadge({
  status,
}: {
  status: SignalStatus | '' | null | undefined;
}) {
  const glyph =
    status === 'ACTIVE' ? '●' : status === 'INVALIDATED' ? '✕' : status === 'EXPIRED' ? '○' : '✓';
  const tone =
    status === 'ACTIVE'
      ? 'border-allow bg-teal-50 text-allow'
      : status === 'INVALIDATED'
        ? 'border-deny bg-red-50 text-deny'
        : 'border-ink-300 bg-ink-50 text-ink-600';
  return (
    <span className={`${BASE} ${tone}`}>
      <span aria-hidden="true" className="font-mono">
        {glyph}
      </span>
      {status ? humanizeEnum(status) : 'Unknown'}
    </span>
  );
}

export function DriftBadge({
  severity,
}: {
  severity: DriftSeverity | '' | null | undefined;
}) {
  const glyph =
    severity === 'none' ? '✓' : severity === 'mild' ? '·' : severity === 'moderate' ? '!' : severity === 'severe' ? '✕' : '?';
  const tone =
    severity === 'none'
      ? 'border-allow bg-teal-50 text-allow'
      : severity === 'mild'
        ? 'border-ink-400 bg-ink-50 text-ink-700'
        : severity === 'moderate'
          ? 'border-watch bg-amber-50 text-watch'
          : severity === 'severe'
            ? 'border-deny bg-red-50 text-deny'
            : 'border-ink-300 bg-ink-50 text-ink-500';
  return (
    <span className={`${BASE} ${tone}`}>
      <span aria-hidden="true" className="font-mono">
        {glyph}
      </span>
      {severity ? `Drift: ${severity}` : 'Drift not computed'}
    </span>
  );
}

/** A neutral chip for provenance identifiers, hashes and versions. */
export function Chip({
  label,
  value,
  mono = true,
  title,
}: {
  label: string;
  value: React.ReactNode;
  mono?: boolean;
  title?: string;
}) {
  return (
    <span
      className="inline-flex max-w-full items-baseline gap-1.5 rounded border border-ink-200 bg-ink-50 px-2 py-0.5 text-xs"
      title={title}
    >
      <span className="shrink-0 font-medium uppercase tracking-wide text-ink-500">
        {label}
      </span>
      <span
        className={`truncate text-ink-800 ${mono ? 'font-mono' : ''}`}
      >
        {value}
      </span>
    </span>
  );
}

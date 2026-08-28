import { DISCLAIMER } from '@/lib/types';

/**
 * The research disclaimer, `domain.Disclaimer`.
 *
 * Governance rule G-9 requires it on every artifact carrying a directional
 * classification. Here that means: it is part of the layout of any surface that
 * shows a signal, a prediction or a classification — not a footer, not a
 * tooltip, not something a reader has to scroll past the numbers to find.
 *
 * The text is taken from the API response wherever one is available, so what
 * the reader sees is the string the server actually sent rather than a copy
 * that could drift from `internal/domain/signal.go`.
 */

export function DisclaimerBanner({
  text,
  className = '',
}: {
  text?: string;
  className?: string;
}) {
  return (
    <aside
      aria-label="Platform disclaimer"
      className={`rounded-md border border-ink-300 bg-ink-100 px-3 py-2 text-xs leading-relaxed text-ink-700 ${className}`}
    >
      <span className="font-semibold uppercase tracking-wide">Research only — </span>
      {text || DISCLAIMER}
    </aside>
  );
}

/**
 * The inline form, for a card that shows one probabilistic artifact. It carries
 * the same text, sized to sit under the numbers it qualifies.
 */
export function DisclaimerNote({
  text,
  className = '',
}: {
  text?: string;
  className?: string;
}) {
  return (
    <p className={`text-[11px] leading-snug text-ink-500 ${className}`}>
      {text || DISCLAIMER}
    </p>
  );
}

/**
 * A standing reminder that a number describes simulated fills against
 * historical or simulated prices. Used wherever paper P&L is displayed.
 */
export function PaperBadge({ className = '' }: { className?: string }) {
  return (
    <span
      className={`inline-flex items-center gap-1 rounded border border-ink-400 bg-white px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wider text-ink-700 ${className}`}
    >
      <span aria-hidden="true">◇</span> Paper — simulated
    </span>
  );
}

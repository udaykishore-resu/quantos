/**
 * Formatting helpers.
 *
 * Two rules govern this file:
 *
 *  1. A number the platform did not measure must never be rendered as if it
 *     had been. Go serialises "unset" as a zero value — `null` for a nil slice,
 *     `0001-01-01T00:00:00Z` for a zero `time.Time`, `""` for an unset enum —
 *     and a naive formatter turns all of those into confident-looking output.
 *     Everything here returns an explicit placeholder instead.
 *
 *  2. A probability is formatted as a probability. There is no helper in this
 *     file that turns a distribution into a single directional verdict, because
 *     the UI is not permitted to present one.
 */

import type {
  CheckStatus,
  Distribution,
  Outcome,
  RiskDecision,
  SignalStatus,
} from './types';

/** The placeholder rendered wherever a value was not measured. */
export const NOT_MEASURED = '—';

/** Go's zero `time.Time`, which `json:",omitempty"` does not omit. */
export const GO_ZERO_TIME = '0001-01-01T00:00:00Z';

export function isZeroTime(ts: string | null | undefined): boolean {
  if (!ts) return true;
  if (ts === GO_ZERO_TIME) return true;
  if (ts.startsWith('0001-01-01')) return true;
  const t = Date.parse(ts);
  return Number.isNaN(t);
}

export function isFiniteNumber(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v);
}

/** Normalises a Go nil slice (`null`) into an empty array. */
export function list<T>(v: readonly T[] | null | undefined): T[] {
  return Array.isArray(v) ? [...v] : [];
}

/** Normalises a Go nil map (`null`) into an empty object. */
export function dict<V>(
  v: Record<string, V> | null | undefined,
): Record<string, V> {
  return v ?? {};
}

// --- Numbers ----------------------------------------------------------------

export function formatNumber(
  v: number | null | undefined,
  digits = 2,
): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return v.toLocaleString('en-US', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

export function formatPrice(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return `$${formatNumber(v, 2)}`;
}

/**
 * A probability, always shown as a percentage with one decimal. Values outside
 * [0,1] are a bug upstream and are reported as such rather than clamped away.
 */
export function formatProbability(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  if (v < 0 || v > 1) return `${(v * 100).toFixed(1)}% (out of range)`;
  return `${(v * 100).toFixed(1)}%`;
}

/** A 0..1 proportion shown as a percentage, for accuracy, confidence, ECE. */
export function formatPercent(
  v: number | null | undefined,
  digits = 1,
): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return `${(v * 100).toFixed(digits)}%`;
}

/** A signed percentage, for returns and deltas. */
export function formatSignedPercent(
  v: number | null | undefined,
  digits = 2,
): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  const s = (v * 100).toFixed(digits);
  return v > 0 ? `+${s}%` : `${s}%`;
}

export function formatBps(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return `${v >= 0 ? '' : ''}${v.toFixed(1)} bps`;
}

export function formatSignedBps(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return `${v > 0 ? '+' : ''}${v.toFixed(1)} bps`;
}

export function formatMoney(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  const sign = v < 0 ? '-' : '';
  return `${sign}$${Math.abs(v).toLocaleString('en-US', {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })}`;
}

export function formatSignedMoney(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return v > 0 ? `+${formatMoney(v)}` : formatMoney(v);
}

/** Compact notional, e.g. $2.8B. */
export function formatCompactMoney(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  const abs = Math.abs(v);
  const sign = v < 0 ? '-' : '';
  if (abs >= 1e12) return `${sign}$${(abs / 1e12).toFixed(1)}T`;
  if (abs >= 1e9) return `${sign}$${(abs / 1e9).toFixed(1)}B`;
  if (abs >= 1e6) return `${sign}$${(abs / 1e6).toFixed(1)}M`;
  if (abs >= 1e3) return `${sign}$${(abs / 1e3).toFixed(1)}K`;
  return `${sign}$${abs.toFixed(0)}`;
}

export function formatCount(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return Math.round(v).toLocaleString('en-US');
}

/** A 0..100 score. */
export function formatScore(v: number | null | undefined): string {
  if (!isFiniteNumber(v)) return NOT_MEASURED;
  return v.toFixed(1);
}

// --- Time -------------------------------------------------------------------

export function formatTimestamp(ts: string | null | undefined): string {
  if (isZeroTime(ts)) return NOT_MEASURED;
  return new Date(ts as string).toISOString().replace('T', ' ').slice(0, 19) + 'Z';
}

export function formatTime(ts: string | null | undefined): string {
  if (isZeroTime(ts)) return NOT_MEASURED;
  return new Date(ts as string).toISOString().slice(11, 19) + 'Z';
}

export function formatDate(ts: string | null | undefined): string {
  if (isZeroTime(ts)) return NOT_MEASURED;
  return new Date(ts as string).toISOString().slice(0, 10);
}

/**
 * Age of a timestamp relative to `now`, as a short human string. A future
 * timestamp is reported as such rather than as "0s ago": clock skew between the
 * data source and the reader is a real condition the reader should see.
 */
export function formatAge(
  ts: string | null | undefined,
  now: number = Date.now(),
): string {
  if (isZeroTime(ts)) return NOT_MEASURED;
  const then = Date.parse(ts as string);
  const deltaMs = now - then;
  if (deltaMs < -1000) return `${formatDuration(-deltaMs)} in the future`;
  return `${formatDuration(Math.max(0, deltaMs))} ago`;
}

/** Milliseconds to a short duration string. */
export function formatDuration(ms: number): string {
  if (!isFiniteNumber(ms) || ms < 0) return NOT_MEASURED;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

/** Go marshals `time.Duration` as an integer count of nanoseconds. */
export function formatGoDuration(ns: number | null | undefined): string {
  if (!isFiniteNumber(ns)) return NOT_MEASURED;
  return formatDuration(ns / 1e6);
}

// --- Domain vocabulary ------------------------------------------------------

/**
 * Human labels for enum values. The wording never softens a verdict: BLOCK is
 * "Blocked", not "Caution".
 */
export function riskDecisionLabel(d: RiskDecision | '' | null | undefined): string {
  switch (d) {
    case 'ALLOW_PAPER_SIGNAL':
      return 'Allowed (paper signal)';
    case 'WATCH_ONLY':
      return 'Watch only — no signal';
    case 'BLOCK':
      return 'Blocked';
    default:
      return 'No decision recorded';
  }
}

/**
 * A short glyph paired with every risk decision, so the verdict survives
 * greyscale, colour-blindness and a printed page (WCAG 1.4.1).
 */
export function riskDecisionGlyph(d: RiskDecision | '' | null | undefined): string {
  switch (d) {
    case 'ALLOW_PAPER_SIGNAL':
      return '✓';
    case 'WATCH_ONLY':
      return '!';
    case 'BLOCK':
      return '✕';
    default:
      return '?';
  }
}

export function checkStatusLabel(s: CheckStatus | '' | null | undefined): string {
  switch (s) {
    case 'PASS':
      return 'Pass';
    case 'WARN':
      return 'Warn';
    case 'FAIL':
      return 'Fail';
    case 'SKIP':
      return 'Skipped';
    default:
      return 'Unknown';
  }
}

export function checkStatusGlyph(s: CheckStatus | '' | null | undefined): string {
  switch (s) {
    case 'PASS':
      return '✓';
    case 'WARN':
      return '!';
    case 'FAIL':
      return '✕';
    case 'SKIP':
      return '–';
    default:
      return '?';
  }
}

export function outcomeLabel(o: Outcome | '' | null | undefined): string {
  switch (o) {
    case 'UP':
      return 'UP';
    case 'FLAT':
      return 'FLAT';
    case 'DOWN':
      return 'DOWN';
    default:
      return NOT_MEASURED;
  }
}

export function signalStatusLabel(s: SignalStatus | '' | null | undefined): string {
  switch (s) {
    case 'ACTIVE':
      return 'Active';
    case 'INVALIDATED':
      return 'Invalidated';
    case 'EXPIRED':
      return 'Expired';
    case 'COMPLETED':
      return 'Completed';
    default:
      return 'Unknown';
  }
}

/** Turns SCREAMING_SNAKE_CASE into "Screaming snake case". */
export function humanizeEnum(v: string | null | undefined): string {
  if (!v) return NOT_MEASURED;
  const lower = v.replace(/_/g, ' ').toLowerCase();
  return lower.charAt(0).toUpperCase() + lower.slice(1);
}

/** Turns snake_case feature and check names into readable labels. */
export function humanizeKey(v: string | null | undefined): string {
  if (!v) return NOT_MEASURED;
  return v.replace(/_/g, ' ');
}

// --- Distributions ----------------------------------------------------------

/**
 * The most likely outcome and its probability. Ties resolve to FLAT, matching
 * `Distribution.Argmax` in Go — the conservative choice.
 */
export function argmax(d: Distribution | null | undefined): {
  outcome: Outcome;
  p: number;
} {
  if (!d) return { outcome: 'FLAT', p: NaN };
  let best: Outcome = 'FLAT';
  let p = d.flat;
  if (d.up > p) {
    best = 'UP';
    p = d.up;
  }
  if (d.down > p) {
    best = 'DOWN';
    p = d.down;
  }
  return { outcome: best, p };
}

/** P(up) − P(down), in [−1, 1]. Mirrors `Distribution.DirectionalEdge`. */
export function directionalEdge(d: Distribution | null | undefined): number {
  if (!d) return NaN;
  return d.up - d.down;
}

/** Shannon entropy in nats. Mirrors `Distribution.Entropy`. */
export function entropy(d: Distribution | null | undefined): number {
  if (!d) return NaN;
  let h = 0;
  for (const p of [d.up, d.flat, d.down]) {
    if (p > 0) h -= p * Math.log(p);
  }
  return h;
}

/** Normalised entropy in [0,1]; 1 means "no information". */
export function uncertainty(d: Distribution | null | undefined): number {
  const h = entropy(d);
  return Number.isNaN(h) ? NaN : h / Math.log(3);
}

/**
 * The normalised margin of the most likely outcome over uniform.
 *
 * This mirrors `Distribution.Confidence` in Go exactly, including the reason it
 * is not `1 - uncertainty`: normalised entropy over three classes reads far too
 * pessimistically to threshold on.
 */
export function distributionConfidence(
  d: Distribution | null | undefined,
): number {
  const { p } = argmax(d);
  if (!isFiniteNumber(p)) return NaN;
  const uniform = 1 / 3;
  const v = (p - uniform) / (1 - uniform);
  return Math.min(1, Math.max(0, v));
}

/** Whether the three masses sum to 1 within tolerance, as Go's `Valid` checks. */
export function distributionIsNormalised(
  d: Distribution | null | undefined,
): boolean {
  if (!d) return false;
  if (![d.up, d.flat, d.down].every(isFiniteNumber)) return false;
  if (d.up < 0 || d.flat < 0 || d.down < 0) return false;
  return Math.abs(d.up + d.flat + d.down - 1) <= 1e-6;
}

// --- Sample-size honesty ----------------------------------------------------

/**
 * The number of resolved observations below which a rate is not reported as a
 * number. It matches the smallest `minSamples` the Go evaluation layer uses for
 * per-regime partitioning (`evaluation.ByRegime(outcomes, 10, 20)`).
 */
export const MIN_SAMPLES_FOR_RATE = 20;

export interface SampleGate {
  sufficient: boolean;
  samples: number;
  note: string;
}

/**
 * Decides whether a measured rate may be shown as a number at all.
 *
 * A dashboard that prints "accuracy 100%" over three observations has told the
 * reader something false. This returns the reason instead, and callers render
 * the reason.
 */
export function sampleGate(
  samples: number | null | undefined,
  minimum: number = MIN_SAMPLES_FOR_RATE,
): SampleGate {
  const n = isFiniteNumber(samples) ? Math.max(0, Math.floor(samples)) : 0;
  if (n === 0) {
    return {
      sufficient: false,
      samples: 0,
      note: 'Not enough data: no predictions have resolved yet.',
    };
  }
  if (n < minimum) {
    return {
      sufficient: false,
      samples: n,
      note: `Not enough data: ${n} resolved ${
        n === 1 ? 'observation' : 'observations'
      }, ${minimum} needed before a rate is meaningful.`,
    };
  }
  return { sufficient: true, samples: n, note: '' };
}

/**
 * Accuracy is only interpretable next to the constant-guess benchmark, so this
 * returns both plus their difference, and refuses to produce any of them when
 * the sample is too small.
 */
export function accuracyVsBaseRate(
  accuracy: number | null | undefined,
  baseRate: number | null | undefined,
  samples: number | null | undefined,
  minimum: number = MIN_SAMPLES_FOR_RATE,
): {
  gate: SampleGate;
  accuracy: string;
  baseRate: string;
  lift: string;
  beatsBaseRate: boolean | null;
} {
  const gate = sampleGate(samples, minimum);
  if (!gate.sufficient) {
    return {
      gate,
      accuracy: NOT_MEASURED,
      baseRate: NOT_MEASURED,
      lift: NOT_MEASURED,
      beatsBaseRate: null,
    };
  }
  const a = isFiniteNumber(accuracy) ? accuracy : NaN;
  const b = isFiniteNumber(baseRate) ? baseRate : NaN;
  const beats = Number.isNaN(a) || Number.isNaN(b) ? null : a > b;
  return {
    gate,
    accuracy: formatPercent(a),
    baseRate: formatPercent(b),
    lift:
      Number.isNaN(a) || Number.isNaN(b)
        ? NOT_MEASURED
        : formatSignedPercent(a - b, 1),
    beatsBaseRate: beats,
  };
}

/** Clamps to [0,1] for chart geometry only — never for a displayed number. */
export function clamp01(v: number): number {
  if (!isFiniteNumber(v)) return 0;
  return Math.min(1, Math.max(0, v));
}

// --- Deflated Sharpe --------------------------------------------------------

export type DeflatedSharpeState =
  | 'not_computed'
  | 'withheld_no_trial_count'
  | 'reportable';

export interface DeflatedSharpePresentation {
  state: DeflatedSharpeState;
  /** True only when the figure may be shown as a headline number. */
  showValue: boolean;
  note: string;
}

/**
 * Decides whether a deflated Sharpe ratio may be presented.
 *
 * A deflated Sharpe adjusts for the number of parameter configurations that
 * were tried, so it is only interpretable against that count. Showing the
 * adjusted figure without saying what it was adjusted *for* is worse than
 * showing nothing: it looks as though the multiple-comparisons problem has been
 * handled, when the reader has no way to tell whether it has. So a deflated
 * Sharpe with no trial count is withheld, and the reason is displayed instead.
 */
export function deflatedSharpePresentation(m: {
  deflated_sharpe?: number;
  trials_considered?: number;
}): DeflatedSharpePresentation {
  const hasValue = isFiniteNumber(m.deflated_sharpe);
  const hasTrials = isFiniteNumber(m.trials_considered) && m.trials_considered > 0;

  if (!hasValue && !hasTrials) {
    return {
      state: 'not_computed',
      showValue: false,
      note:
        'No deflated Sharpe was computed for this run. Deflation adjusts for the ' +
        'number of parameter configurations tried, and is reported on sweeps rather ' +
        'than on a single run. The plain Sharpe is therefore un-adjusted for ' +
        'selection: if this configuration was chosen after trying others, it is ' +
        'optimistic.',
    };
  }
  if (hasValue && !hasTrials) {
    return {
      state: 'withheld_no_trial_count',
      showValue: false,
      note:
        'A deflated Sharpe is only interpretable against the number of ' +
        'configurations it was deflated for. Without that count the figure cannot ' +
        'be read, so it is not presented as a headline number.',
    };
  }
  return {
    state: 'reportable',
    showValue: hasValue,
    note: '',
  };
}

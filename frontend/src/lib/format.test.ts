import { describe, expect, it } from 'vitest';

import {
  GO_ZERO_TIME,
  MIN_SAMPLES_FOR_RATE,
  NOT_MEASURED,
  accuracyVsBaseRate,
  argmax,
  deflatedSharpePresentation,
  directionalEdge,
  dict,
  distributionConfidence,
  distributionIsNormalised,
  entropy,
  formatAge,
  formatBps,
  formatCompactMoney,
  formatDuration,
  formatGoDuration,
  formatMoney,
  formatNumber,
  formatPercent,
  formatProbability,
  formatScore,
  formatSignedMoney,
  formatSignedPercent,
  formatTimestamp,
  humanizeEnum,
  isZeroTime,
  list,
  riskDecisionGlyph,
  riskDecisionLabel,
  sampleGate,
  uncertainty,
} from './format';

describe('Go zero-value handling', () => {
  it('treats Go’s zero time.Time as absent', () => {
    // `json:"as_of,omitempty"` does NOT omit a zero time.Time, so the wire
    // carries this value constantly. Rendering it as a date would be a lie.
    expect(isZeroTime(GO_ZERO_TIME)).toBe(true);
    expect(isZeroTime('0001-01-01T00:00:00Z')).toBe(true);
    expect(isZeroTime('')).toBe(true);
    expect(isZeroTime(null)).toBe(true);
    expect(isZeroTime(undefined)).toBe(true);
    expect(isZeroTime('not a date')).toBe(true);
    expect(isZeroTime('2026-08-28T02:11:00Z')).toBe(false);
  });

  it('renders an unmeasured timestamp as the placeholder, not as year 1', () => {
    expect(formatTimestamp(GO_ZERO_TIME)).toBe(NOT_MEASURED);
    expect(formatTimestamp('2026-08-28T02:11:09Z')).toBe('2026-08-28 02:11:09Z');
  });

  it('normalises a Go nil slice into an empty array', () => {
    expect(list(null)).toEqual([]);
    expect(list(undefined)).toEqual([]);
    expect(list(['a', 'b'])).toEqual(['a', 'b']);
  });

  it('normalises a Go nil map into an empty object', () => {
    expect(dict(null)).toEqual({});
    expect(dict({ a: 1 })).toEqual({ a: 1 });
  });
});

describe('numeric formatting refuses to invent values', () => {
  it('returns the placeholder for anything not finite', () => {
    for (const fn of [
      formatNumber,
      formatPercent,
      formatProbability,
      formatBps,
      formatMoney,
      formatCompactMoney,
      formatScore,
      formatSignedPercent,
      formatSignedMoney,
    ]) {
      expect(fn(NaN)).toBe(NOT_MEASURED);
      expect(fn(Infinity)).toBe(NOT_MEASURED);
      expect(fn(null)).toBe(NOT_MEASURED);
      expect(fn(undefined)).toBe(NOT_MEASURED);
    }
  });

  it('formats probabilities as percentages', () => {
    expect(formatProbability(0.6312)).toBe('63.1%');
    expect(formatProbability(0)).toBe('0.0%');
    expect(formatProbability(1)).toBe('100.0%');
  });

  it('flags an out-of-range probability rather than clamping it away', () => {
    // A probability above 1 is an upstream defect. Silently clamping it would
    // hide the defect behind a plausible-looking number.
    expect(formatProbability(1.4)).toContain('out of range');
    expect(formatProbability(-0.2)).toContain('out of range');
  });

  it('signs percentages and money that can be negative', () => {
    expect(formatSignedPercent(0.0267, 2)).toBe('+2.67%');
    expect(formatSignedPercent(-0.0134, 2)).toBe('-1.34%');
    expect(formatSignedMoney(1234.5)).toBe('+$1,234.50');
    expect(formatSignedMoney(-1234.5)).toBe('-$1,234.50');
  });

  it('compacts large notionals', () => {
    expect(formatCompactMoney(2.8e9)).toBe('$2.8B');
    expect(formatCompactMoney(4.12e6)).toBe('$4.1M');
    expect(formatCompactMoney(-1.5e12)).toBe('-$1.5T');
  });
});

describe('time formatting', () => {
  it('formats durations', () => {
    expect(formatDuration(4200)).toBe('4s');
    expect(formatDuration(65_000)).toBe('1m 5s');
    expect(formatDuration(3_930_000)).toBe('1h 5m');
    expect(formatDuration(90_000_000)).toBe('1d 1h');
  });

  it('converts Go nanosecond durations', () => {
    // time.Duration marshals as an integer count of nanoseconds.
    expect(formatGoDuration(3_600_000_000_000)).toBe('1h 0m');
    expect(formatGoDuration(null)).toBe(NOT_MEASURED);
  });

  it('reports a future timestamp as future rather than as "0s ago"', () => {
    // The local simulator timestamps quotes ahead of wall clock. A reader
    // should see the skew, not a reassuring "just now".
    const now = Date.parse('2026-08-28T02:00:00Z');
    expect(formatAge('2026-08-28T02:50:00Z', now)).toBe('50m 0s in the future');
    expect(formatAge('2026-08-28T01:58:00Z', now)).toBe('2m 0s ago');
    expect(formatAge(GO_ZERO_TIME, now)).toBe(NOT_MEASURED);
  });
});

describe('distribution maths mirrors internal/domain/prediction.go', () => {
  const d = { up: 0.63, flat: 0.21, down: 0.16 };

  it('picks the argmax, resolving ties to FLAT', () => {
    expect(argmax(d)).toEqual({ outcome: 'UP', p: 0.63 });
    // Ties resolve to FLAT, matching Distribution.Argmax — the conservative choice.
    const tie = { up: 1 / 3, flat: 1 / 3, down: 1 / 3 };
    expect(argmax(tie).outcome).toBe('FLAT');
    // A tie between UP and FLAT must not go to UP either.
    expect(argmax({ up: 0.4, flat: 0.4, down: 0.2 }).outcome).toBe('FLAT');
  });

  it('computes directional edge as P(up) − P(down)', () => {
    expect(directionalEdge(d)).toBeCloseTo(0.47, 10);
    // The distinction the Go comment insists on: these two have the same
    // argmax but carry different information.
    expect(directionalEdge({ up: 0.4, flat: 0.35, down: 0.25 })).toBeCloseTo(0.15, 10);
    expect(directionalEdge({ up: 0.4, flat: 0.2, down: 0.4 })).toBeCloseTo(0, 10);
  });

  it('computes entropy and normalised uncertainty', () => {
    const uniform = { up: 1 / 3, flat: 1 / 3, down: 1 / 3 };
    expect(entropy(uniform)).toBeCloseTo(Math.log(3), 10);
    expect(uncertainty(uniform)).toBeCloseTo(1, 10);
    expect(uncertainty({ up: 1, flat: 0, down: 0 })).toBeCloseTo(0, 10);
  });

  it('uses the max-probability margin for confidence, not 1 − uncertainty', () => {
    // This is the worked example from the Go comment: entropy ratio would give
    // 0.17, which fails every sensible gate. The margin gives 0.45.
    expect(distributionConfidence(d)).toBeCloseTo(0.445, 3);
    expect(1 - uncertainty(d)).toBeCloseTo(0.17, 2);
    expect(distributionConfidence({ up: 1 / 3, flat: 1 / 3, down: 1 / 3 })).toBeCloseTo(0, 10);
    expect(distributionConfidence({ up: 1, flat: 0, down: 0 })).toBeCloseTo(1, 10);
  });

  it('detects a malformed distribution instead of normalising it away', () => {
    expect(distributionIsNormalised(d)).toBe(true);
    expect(distributionIsNormalised({ up: 0.5, flat: 0.5, down: 0.5 })).toBe(false);
    expect(distributionIsNormalised({ up: -0.1, flat: 0.6, down: 0.5 })).toBe(false);
    expect(distributionIsNormalised(null)).toBe(false);
    expect(distributionIsNormalised({ up: NaN, flat: 0.5, down: 0.5 })).toBe(false);
  });
});

describe('sample gating', () => {
  it('refuses to call zero observations a rate', () => {
    const g = sampleGate(0);
    expect(g.sufficient).toBe(false);
    expect(g.note).toContain('no predictions have resolved');
  });

  it('refuses a rate below the minimum and says how far short it is', () => {
    const g = sampleGate(3);
    expect(g.sufficient).toBe(false);
    expect(g.note).toContain('3 resolved observations');
    expect(g.note).toContain(String(MIN_SAMPLES_FOR_RATE));
  });

  it('uses the singular for exactly one observation', () => {
    expect(sampleGate(1).note).toContain('1 resolved observation,');
  });

  it('allows a rate once the minimum is met', () => {
    expect(sampleGate(MIN_SAMPLES_FOR_RATE).sufficient).toBe(true);
    expect(sampleGate(1768).sufficient).toBe(true);
  });

  it('treats a missing sample count as zero rather than as sufficient', () => {
    expect(sampleGate(undefined).sufficient).toBe(false);
    expect(sampleGate(NaN).sufficient).toBe(false);
  });
});

describe('accuracy is never returned without its base rate', () => {
  it('withholds every figure when the sample is too small', () => {
    const r = accuracyVsBaseRate(1.0, 0.39, 3);
    // 100% accuracy over three observations must not render as a number.
    expect(r.accuracy).toBe(NOT_MEASURED);
    expect(r.baseRate).toBe(NOT_MEASURED);
    expect(r.lift).toBe(NOT_MEASURED);
    expect(r.beatsBaseRate).toBeNull();
    expect(r.gate.sufficient).toBe(false);
  });

  it('returns accuracy, base rate and lift together', () => {
    // The real measurement from the running platform: barely above guessing.
    const r = accuracyVsBaseRate(0.4033, 0.3993, 1768);
    expect(r.accuracy).toBe('40.3%');
    expect(r.baseRate).toBe('39.9%');
    expect(r.lift).toBe('+0.4%');
    expect(r.beatsBaseRate).toBe(true);
  });

  it('reports a model below its base rate as not beating it', () => {
    const r = accuracyVsBaseRate(0.35, 0.3993, 500);
    expect(r.beatsBaseRate).toBe(false);
    expect(r.lift).toBe('-4.9%');
  });

  it('reports no verdict when either figure is missing', () => {
    const r = accuracyVsBaseRate(0.42, NaN, 500);
    expect(r.beatsBaseRate).toBeNull();
    expect(r.lift).toBe(NOT_MEASURED);
  });
});

describe('domain vocabulary is never softened', () => {
  it('labels risk decisions without euphemism', () => {
    expect(riskDecisionLabel('ALLOW_PAPER_SIGNAL')).toBe('Allowed (paper signal)');
    expect(riskDecisionLabel('WATCH_ONLY')).toBe('Watch only — no signal');
    expect(riskDecisionLabel('BLOCK')).toBe('Blocked');
    expect(riskDecisionLabel('')).toBe('No decision recorded');
  });

  it('pairs every decision with a distinct non-colour glyph', () => {
    const glyphs = (['ALLOW_PAPER_SIGNAL', 'WATCH_ONLY', 'BLOCK'] as const).map(
      riskDecisionGlyph,
    );
    expect(new Set(glyphs).size).toBe(3);
    expect(glyphs.every((g) => g.length > 0)).toBe(true);
  });

  it('humanises enum values for display', () => {
    expect(humanizeEnum('BULL_TREND')).toBe('Bull trend');
    expect(humanizeEnum('HIGH_VOLATILITY')).toBe('High volatility');
    expect(humanizeEnum(null)).toBe(NOT_MEASURED);
  });
});

describe('deflated Sharpe is withheld unless its trial count is present', () => {
  it('reports "not computed" when neither figure exists', () => {
    const p = deflatedSharpePresentation({});
    expect(p.state).toBe('not_computed');
    expect(p.showValue).toBe(false);
    expect(p.note).toContain('un-adjusted for');
  });

  it('withholds a deflated Sharpe that has no trial count', () => {
    // Showing an adjusted figure without saying what it was adjusted for
    // implies the multiple-comparisons problem was handled when the reader
    // cannot tell whether it was.
    const p = deflatedSharpePresentation({ deflated_sharpe: 0.42 });
    expect(p.state).toBe('withheld_no_trial_count');
    expect(p.showValue).toBe(false);
    expect(p.note).toContain('only interpretable against the number');
  });

  it('withholds when the trial count is zero', () => {
    const p = deflatedSharpePresentation({ deflated_sharpe: 0.42, trials_considered: 0 });
    expect(p.showValue).toBe(false);
  });

  it('reports the figure once the trial count is present', () => {
    const p = deflatedSharpePresentation({ deflated_sharpe: 0.1873, trials_considered: 48 });
    expect(p.state).toBe('reportable');
    expect(p.showValue).toBe(true);
    expect(p.note).toBe('');
  });

  it('still reports a deflated Sharpe of zero — that is a real result', () => {
    const p = deflatedSharpePresentation({ deflated_sharpe: 0, trials_considered: 100 });
    expect(p.state).toBe('reportable');
    expect(p.showValue).toBe(true);
  });

  it('does not show a non-finite value even with a trial count', () => {
    const p = deflatedSharpePresentation({ deflated_sharpe: NaN, trials_considered: 48 });
    expect(p.showValue).toBe(false);
  });
});

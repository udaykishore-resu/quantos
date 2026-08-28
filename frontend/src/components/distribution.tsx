/**
 * Rendering a probability distribution.
 *
 * The requirements this component is built to satisfy, all of which are safety
 * requirements rather than styling ones:
 *
 *  - A probability is shown as a probability. There is no mode of this
 *    component that collapses UP/FLAT/DOWN into "the model says up".
 *  - The horizon and the flat band are shown *with* the numbers. Without the
 *    band, "UP 63%" has no meaning: up by how much? The Go type carries
 *    `flat_band_bps` for exactly this reason and the UI must not drop it.
 *  - Uncertainty and confidence are shown, not hidden.
 *  - A distribution that does not sum to 1 is reported rather than normalised
 *    away, because that would be a real upstream defect worth seeing.
 */

import {
  distributionConfidence,
  distributionIsNormalised,
  formatBps,
  formatPercent,
  formatProbability,
  uncertainty,
} from '@/lib/format';
import type { Distribution, Horizon, ScenarioPrediction } from '@/lib/types';

const CLASSES = [
  { key: 'up' as const, label: 'UP', glyph: '▲', bar: 'bg-up', text: 'text-up' },
  { key: 'flat' as const, label: 'FLAT', glyph: '■', bar: 'bg-flat', text: 'text-flat' },
  { key: 'down' as const, label: 'DOWN', glyph: '▼', bar: 'bg-down', text: 'text-down' },
];

/**
 * The three-class bar. Each segment carries its label and percentage as text,
 * so the reading survives greyscale and a screen reader alike.
 */
export function DistributionBar({
  dist,
  compact = false,
}: {
  dist: Distribution | null | undefined;
  compact?: boolean;
}) {
  if (!dist) {
    return (
      <p className="text-xs text-ink-500">No distribution recorded.</p>
    );
  }
  const normalised = distributionIsNormalised(dist);
  const total = dist.up + dist.flat + dist.down;
  const denom = total > 0 ? total : 1;

  return (
    <div>
      <div
        className="flex h-6 w-full overflow-hidden rounded border border-ink-200"
        role="img"
        aria-label={`Probability distribution: up ${formatProbability(
          dist.up,
        )}, flat ${formatProbability(dist.flat)}, down ${formatProbability(dist.down)}`}
      >
        {CLASSES.map((c) => {
          const p = dist[c.key];
          const pct = (p / denom) * 100;
          if (pct <= 0) return null;
          return (
            <div
              key={c.key}
              className={`${c.bar} flex items-center justify-center overflow-hidden whitespace-nowrap px-1 text-[10px] font-bold text-white`}
              style={{ width: `${pct}%` }}
            >
              {pct >= 14 ? `${c.label} ${(p * 100).toFixed(0)}%` : null}
            </div>
          );
        })}
      </div>
      {!compact ? (
        <dl className="mt-1.5 grid grid-cols-3 gap-2 text-center">
          {CLASSES.map((c) => (
            <div key={c.key}>
              <dt className={`text-[10px] font-bold uppercase tracking-wide ${c.text}`}>
                <span aria-hidden="true">{c.glyph} </span>
                {c.label}
              </dt>
              <dd className="num text-sm font-semibold text-ink-900">
                {formatProbability(dist[c.key])}
              </dd>
            </div>
          ))}
        </dl>
      ) : null}
      {!normalised ? (
        <p role="alert" className="mt-1 text-[11px] font-medium text-deny">
          These probabilities sum to {(total * 100).toFixed(1)}%, not 100%. The
          distribution is malformed — treat it as unusable rather than as a
          forecast.
        </p>
      ) : null}
    </div>
  );
}

/**
 * A full scenario: the distribution plus everything needed to read it — the
 * horizon it applies over, the flat band that defines the classes, the expected
 * move, and the model's own confidence and uncertainty.
 */
export function ScenarioCard({
  scenario,
  highlight = false,
}: {
  scenario: ScenarioPrediction;
  highlight?: boolean;
}) {
  const conf = distributionConfidence(scenario.distribution);
  const unc = uncertainty(scenario.distribution);
  return (
    <div
      className={`rounded-md border p-3 ${
        highlight ? 'border-ink-400 bg-ink-50' : 'border-ink-200 bg-white'
      }`}
    >
      <div className="mb-2 flex flex-wrap items-baseline justify-between gap-2">
        <h4 className="text-sm font-semibold text-ink-900">
          Over the next {horizonPhrase(scenario.horizon)}
        </h4>
        <span className="text-[11px] text-ink-600">
          FLAT band ±{formatBps(scenario.flat_band_bps)}
        </span>
      </div>

      <DistributionBar dist={scenario.distribution} />

      <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1.5 text-xs sm:grid-cols-4">
        <Stat
          label="Confidence"
          value={formatPercent(
            Number.isFinite(scenario.confidence) && scenario.confidence > 0
              ? scenario.confidence
              : conf,
          )}
          hint="Margin of the most likely class over an even split. 0% means the model has nothing to say."
        />
        <Stat
          label="Uncertainty"
          value={formatPercent(
            Number.isFinite(scenario.uncertainty) && scenario.uncertainty > 0
              ? scenario.uncertainty
              : unc,
          )}
          hint="Normalised entropy. 100% is maximum uncertainty across the three classes."
        />
        <Stat
          label="Expected move"
          value={formatBps(scenario.expected_move_bps)}
          hint="Probability-weighted absolute move over this horizon."
        />
        <Stat
          label="Flat band"
          value={`±${formatBps(scenario.flat_band_bps)}`}
          hint="A move inside this band counts as FLAT, not as UP or DOWN."
        />
      </dl>

      <p className="mt-2 text-[11px] leading-snug text-ink-500">
        These are probabilities over three outcomes for one horizon, not a
        forecast of a particular price. Any of the three may occur.
      </p>
    </div>
  );
}

function Stat({
  label,
  value,
  hint,
}: {
  label: string;
  value: string;
  hint: string;
}) {
  return (
    <div>
      <dt className="text-[10px] font-medium uppercase tracking-wide text-ink-500">
        {label}
      </dt>
      <dd className="num font-semibold text-ink-900" title={hint}>
        {value}
      </dd>
      <span className="sr-only">{hint}</span>
    </div>
  );
}

export function horizonPhrase(h: Horizon | '' | null | undefined): string {
  switch (h) {
    case '15m':
      return '15 minutes';
    case '60m':
      return '60 minutes';
    case '1d':
      return 'trading day';
    case '5d':
      return '5 trading days';
    default:
      return 'unspecified horizon';
  }
}

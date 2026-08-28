/**
 * Hand-written SVG charts.
 *
 * No charting library: everything here is plain SVG with no runtime dependency,
 * which keeps the bundle small, the CSP tight and the build unbreakable by a
 * registry outage.
 *
 * Every chart in this file refuses to draw an empty frame. A chart with no data
 * renders an explicit message saying so — an axis with nothing between it is
 * indistinguishable from a rendering failure, and a reader cannot tell "flat" from
 * "broken".
 *
 * Accessibility: each chart is `role="img"` with a summarising `aria-label`, and
 * the series it draws is also available as a table where the numbers matter.
 */

import { clamp01, formatPercent, isFiniteNumber } from '@/lib/format';

export function NoChartData({
  message,
  height = 160,
}: {
  message: string;
  height?: number;
}) {
  return (
    <div
      className="flex items-center justify-center rounded border border-dashed border-ink-300 bg-ink-50 px-4 text-center"
      style={{ height }}
    >
      <p className="max-w-prose text-xs leading-relaxed text-ink-500">{message}</p>
    </div>
  );
}

interface Extent {
  min: number;
  max: number;
}

function extent(values: number[]): Extent {
  let min = Infinity;
  let max = -Infinity;
  for (const v of values) {
    if (!isFiniteNumber(v)) continue;
    if (v < min) min = v;
    if (v > max) max = v;
  }
  if (!Number.isFinite(min) || !Number.isFinite(max)) return { min: 0, max: 1 };
  if (min === max) {
    // A perfectly flat series still deserves a visible line rather than a
    // divide-by-zero, so give it a symmetric band.
    const pad = Math.abs(min) * 0.05 || 1;
    return { min: min - pad, max: max + pad };
  }
  return { min, max };
}

// --- Line / area ------------------------------------------------------------

export interface SeriesPoint {
  x: number;
  y: number;
}

/**
 * A line chart with an optional filled area. Used for equity curves, price
 * closes and regime confidence over time.
 */
export function LineChart({
  points,
  height = 180,
  width = 640,
  label,
  yFormat = (v: number) => v.toFixed(2),
  xFormat,
  area = false,
  baseline,
  className = '',
}: {
  points: SeriesPoint[];
  height?: number;
  width?: number;
  label: string;
  yFormat?: (v: number) => string;
  xFormat?: (v: number) => string;
  area?: boolean;
  /** Draws a reference line, e.g. starting equity. */
  baseline?: number;
  className?: string;
}) {
  const usable = points.filter((p) => isFiniteNumber(p.x) && isFiniteNumber(p.y));
  if (usable.length === 0) {
    return <NoChartData message={`No data for ${label}.`} height={height} />;
  }
  if (usable.length === 1) {
    const only = usable[0] as SeriesPoint;
    return (
      <NoChartData
        message={`Only one observation for ${label} (${yFormat(
          only.y,
        )}). A line needs at least two points; this is not yet a series.`}
        height={height}
      />
    );
  }

  const pad = { top: 8, right: 8, bottom: 22, left: 52 };
  const w = width - pad.left - pad.right;
  const h = height - pad.top - pad.bottom;

  const xs = extent(usable.map((p) => p.x));
  const ysRaw = usable.map((p) => p.y);
  if (isFiniteNumber(baseline)) ysRaw.push(baseline);
  const ys = extent(ysRaw);

  const sx = (v: number) => pad.left + ((v - xs.min) / (xs.max - xs.min)) * w;
  const sy = (v: number) => pad.top + h - ((v - ys.min) / (ys.max - ys.min)) * h;

  const d = usable
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${sx(p.x).toFixed(2)},${sy(p.y).toFixed(2)}`)
    .join(' ');

  const first = usable[0] as SeriesPoint;
  const last = usable[usable.length - 1] as SeriesPoint;
  const areaPath = `${d} L${sx(last.x).toFixed(2)},${(pad.top + h).toFixed(
    2,
  )} L${sx(first.x).toFixed(2)},${(pad.top + h).toFixed(2)} Z`;

  const ticks = [ys.min, (ys.min + ys.max) / 2, ys.max];

  return (
    <figure className={className}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="h-auto w-full"
        role="img"
        aria-label={`${label}. ${usable.length} observations, from ${yFormat(
          first.y,
        )} to ${yFormat(last.y)}, ranging between ${yFormat(ys.min)} and ${yFormat(
          ys.max,
        )}.`}
        preserveAspectRatio="none"
      >
        {ticks.map((t, i) => (
          <g key={i}>
            <line
              x1={pad.left}
              x2={pad.left + w}
              y1={sy(t)}
              y2={sy(t)}
              className="chart-grid"
              strokeDasharray={i === 0 || i === ticks.length - 1 ? undefined : '3 3'}
            />
            <text x={pad.left - 6} y={sy(t) + 3} textAnchor="end" className="chart-axis-label">
              {yFormat(t)}
            </text>
          </g>
        ))}

        {isFiniteNumber(baseline) ? (
          <line
            x1={pad.left}
            x2={pad.left + w}
            y1={sy(baseline)}
            y2={sy(baseline)}
            stroke="#b45309"
            strokeWidth={1}
            strokeDasharray="5 3"
          />
        ) : null}

        {area ? <path d={areaPath} fill="#3e4555" fillOpacity={0.1} /> : null}
        <path d={d} fill="none" stroke="#20242e" strokeWidth={1.5} />

        {xFormat ? (
          <>
            <text
              x={pad.left}
              y={height - 6}
              textAnchor="start"
              className="chart-axis-label"
            >
              {xFormat(first.x)}
            </text>
            <text
              x={pad.left + w}
              y={height - 6}
              textAnchor="end"
              className="chart-axis-label"
            >
              {xFormat(last.x)}
            </text>
          </>
        ) : null}
      </svg>
    </figure>
  );
}

// --- Candles ----------------------------------------------------------------

export interface CandlePoint {
  start: number;
  open: number;
  high: number;
  low: number;
  close: number;
}

/**
 * A candlestick chart. Direction is carried by fill *and* by an outline, so an
 * up bar and a down bar remain distinguishable without colour.
 */
export function CandleChart({
  candles,
  height = 260,
  width = 720,
  label,
  className = '',
}: {
  candles: CandlePoint[];
  height?: number;
  width?: number;
  label: string;
  className?: string;
}) {
  const usable = candles.filter(
    (c) =>
      isFiniteNumber(c.open) &&
      isFiniteNumber(c.high) &&
      isFiniteNumber(c.low) &&
      isFiniteNumber(c.close),
  );
  if (usable.length === 0) {
    return (
      <NoChartData
        message={`No bars returned for ${label}. The candle store may be empty for this interval, or the platform may be running without a persistent store — check the degraded list above.`}
        height={height}
      />
    );
  }

  const pad = { top: 8, right: 8, bottom: 22, left: 56 };
  const w = width - pad.left - pad.right;
  const h = height - pad.top - pad.bottom;
  const ys = extent(usable.flatMap((c) => [c.high, c.low]));
  const sy = (v: number) => pad.top + h - ((v - ys.min) / (ys.max - ys.min)) * h;

  const slot = w / usable.length;
  const bodyW = Math.max(1, Math.min(9, slot * 0.62));
  const ticks = [ys.min, (ys.min + ys.max) / 2, ys.max];

  const firstBar = usable[0] as CandlePoint;
  const lastBar = usable[usable.length - 1] as CandlePoint;

  return (
    <figure className={className}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="h-auto w-full"
        role="img"
        aria-label={`${label}. ${usable.length} bars. Open ${firstBar.open.toFixed(
          2,
        )}, close ${lastBar.close.toFixed(2)}, high ${ys.max.toFixed(
          2,
        )}, low ${ys.min.toFixed(2)}.`}
      >
        {ticks.map((t, i) => (
          <g key={i}>
            <line
              x1={pad.left}
              x2={pad.left + w}
              y1={sy(t)}
              y2={sy(t)}
              className="chart-grid"
              strokeDasharray={i === 1 ? '3 3' : undefined}
            />
            <text x={pad.left - 6} y={sy(t) + 3} textAnchor="end" className="chart-axis-label">
              {t.toFixed(2)}
            </text>
          </g>
        ))}

        {usable.map((c, i) => {
          const cx = pad.left + slot * (i + 0.5);
          const up = c.close >= c.open;
          const stroke = up ? '#0f766e' : '#b91c1c';
          const fill = up ? '#ffffff' : '#b91c1c';
          const yTop = sy(Math.max(c.open, c.close));
          const yBot = sy(Math.min(c.open, c.close));
          return (
            <g key={i}>
              <line
                x1={cx}
                x2={cx}
                y1={sy(c.high)}
                y2={sy(c.low)}
                stroke={stroke}
                strokeWidth={1}
              />
              <rect
                x={cx - bodyW / 2}
                y={yTop}
                width={bodyW}
                height={Math.max(1, yBot - yTop)}
                fill={fill}
                stroke={stroke}
                strokeWidth={1}
              />
            </g>
          );
        })}
      </svg>
    </figure>
  );
}

// --- Reliability / calibration ---------------------------------------------

export interface ReliabilityPoint {
  meanPredicted: number;
  observed: number;
  count: number;
  lower: number;
  upper: number;
}

/**
 * A reliability diagram: predicted probability against observed frequency, with
 * the diagonal that perfect calibration would follow.
 *
 * Bin marker size encodes sample count, and bins with too few observations are
 * drawn hollow and listed as unreliable — a bin holding two observations sitting
 * exactly on the diagonal is not evidence of calibration.
 */
export function ReliabilityChart({
  bins,
  height = 260,
  width = 300,
  minCount = 10,
  className = '',
}: {
  bins: ReliabilityPoint[];
  height?: number;
  width?: number;
  minCount?: number;
  className?: string;
}) {
  const usable = bins.filter(
    (b) => b.count > 0 && isFiniteNumber(b.meanPredicted) && isFiniteNumber(b.observed),
  );
  if (usable.length === 0) {
    return (
      <NoChartData
        message="No calibration bins have any observations yet. A reliability plot needs resolved predictions; until some have resolved there is nothing to plot."
        height={height}
      />
    );
  }

  const pad = { top: 10, right: 10, bottom: 30, left: 40 };
  const w = width - pad.left - pad.right;
  const h = height - pad.top - pad.bottom;
  const sx = (v: number) => pad.left + clamp01(v) * w;
  const sy = (v: number) => pad.top + h - clamp01(v) * h;
  const totalCount = usable.reduce((a, b) => a + b.count, 0);
  const maxCount = Math.max(...usable.map((b) => b.count));

  const thin = usable.filter((b) => b.count < minCount);

  return (
    <figure className={className}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="h-auto w-full"
        role="img"
        aria-label={`Reliability diagram over ${usable.length} bins and ${totalCount} resolved predictions. Points on the diagonal indicate predicted probabilities matching observed frequencies.`}
      >
        <rect
          x={pad.left}
          y={pad.top}
          width={w}
          height={h}
          fill="none"
          className="chart-grid"
        />
        {[0.25, 0.5, 0.75].map((t) => (
          <g key={t}>
            <line x1={sx(t)} x2={sx(t)} y1={pad.top} y2={pad.top + h} className="chart-grid" strokeDasharray="3 3" />
            <line x1={pad.left} x2={pad.left + w} y1={sy(t)} y2={sy(t)} className="chart-grid" strokeDasharray="3 3" />
          </g>
        ))}
        {/* Perfect calibration. */}
        <line
          x1={sx(0)}
          y1={sy(0)}
          x2={sx(1)}
          y2={sy(1)}
          stroke="#616b82"
          strokeWidth={1}
          strokeDasharray="4 3"
        />
        {usable.map((b, i) => {
          const r = 3 + 5 * Math.sqrt(b.count / Math.max(1, maxCount));
          const reliable = b.count >= minCount;
          return (
            <circle
              key={i}
              cx={sx(b.meanPredicted)}
              cy={sy(b.observed)}
              r={r}
              fill={reliable ? '#20242e' : 'none'}
              stroke="#20242e"
              strokeWidth={1.25}
            />
          );
        })}
        {[0, 0.5, 1].map((t) => (
          <text key={`x${t}`} x={sx(t)} y={height - 14} textAnchor="middle" className="chart-axis-label">
            {formatPercent(t, 0)}
          </text>
        ))}
        {[0, 0.5, 1].map((t) => (
          <text key={`y${t}`} x={pad.left - 5} y={sy(t) + 3} textAnchor="end" className="chart-axis-label">
            {formatPercent(t, 0)}
          </text>
        ))}
        <text x={pad.left + w / 2} y={height - 2} textAnchor="middle" className="chart-axis-label">
          predicted
        </text>
      </svg>
      <figcaption className="mt-1 text-[11px] leading-snug text-ink-500">
        Vertical axis is observed frequency; the dashed diagonal is perfect
        calibration. Marker area is proportional to the number of observations in
        the bin.{' '}
        {thin.length > 0 ? (
          <span className="text-ink-700">
            {thin.length} of {usable.length} bins hold fewer than {minCount}{' '}
            observations and are drawn hollow — their position is not yet
            evidence of anything.
          </span>
        ) : null}
      </figcaption>
    </figure>
  );
}

// --- Bars -------------------------------------------------------------------

export interface BarDatum {
  label: string;
  value: number;
  /** Rendered alongside the label, e.g. an n= count. */
  note?: string;
}

/**
 * A horizontal bar chart, used for score components, per-regime accuracy and
 * feature contributions. Handles negative values by splitting around zero.
 */
export function BarChart({
  data,
  format = (v: number) => v.toFixed(1),
  emptyMessage = 'No values to plot.',
  reference,
  referenceLabel,
  className = '',
}: {
  data: BarDatum[];
  format?: (v: number) => string;
  emptyMessage?: string;
  /** A comparison line, e.g. the base rate accuracy must be read against. */
  reference?: number;
  referenceLabel?: string;
  className?: string;
}) {
  const usable = data.filter((d) => isFiniteNumber(d.value));
  if (usable.length === 0) {
    return <NoChartData message={emptyMessage} height={120} />;
  }
  const values = usable.map((d) => d.value);
  if (isFiniteNumber(reference)) values.push(reference);
  const max = Math.max(...values.map(Math.abs), 1e-9);
  const hasNegative = usable.some((d) => d.value < 0);

  return (
    <div className={className}>
      <ul className="space-y-1.5">
        {usable.map((d) => {
          const frac = Math.abs(d.value) / max;
          const pct = Math.max(1, frac * (hasNegative ? 50 : 100));
          return (
            <li key={d.label} className="grid grid-cols-[minmax(6rem,10rem)_1fr_auto] items-center gap-2 text-xs">
              <span className="truncate text-ink-700" title={d.label}>
                {d.label}
                {d.note ? <span className="ml-1 text-ink-400">{d.note}</span> : null}
              </span>
              <span className="relative flex h-3.5 items-center rounded bg-ink-100">
                {hasNegative ? (
                  <span className="absolute left-1/2 top-0 h-full w-px bg-ink-300" aria-hidden="true" />
                ) : null}
                <span
                  className={`absolute h-full rounded ${d.value < 0 ? 'bg-down' : 'bg-ink-700'}`}
                  style={
                    hasNegative
                      ? d.value < 0
                        ? { right: '50%', width: `${pct}%` }
                        : { left: '50%', width: `${pct}%` }
                      : { left: 0, width: `${pct}%` }
                  }
                />
                {isFiniteNumber(reference) && !hasNegative ? (
                  <span
                    className="absolute top-0 h-full w-0.5 bg-watch"
                    style={{ left: `${clamp01(Math.abs(reference) / max) * 100}%` }}
                    aria-hidden="true"
                  />
                ) : null}
              </span>
              <span className="num w-16 text-right font-medium text-ink-900">
                {format(d.value)}
              </span>
            </li>
          );
        })}
      </ul>
      {isFiniteNumber(reference) && referenceLabel ? (
        <p className="mt-1.5 text-[11px] text-ink-600">
          <span aria-hidden="true" className="mr-1 inline-block h-2 w-0.5 bg-watch align-middle" />
          {referenceLabel}: <span className="num font-medium">{format(reference)}</span>
        </p>
      ) : null}
    </div>
  );
}

// --- Drawdown ---------------------------------------------------------------

/**
 * Drawdown over time, drawn downward from zero because that is the direction
 * the quantity actually moves.
 */
export function DrawdownChart({
  points,
  height = 120,
  width = 640,
  label,
  className = '',
}: {
  points: SeriesPoint[];
  height?: number;
  width?: number;
  label: string;
  className?: string;
}) {
  const usable = points.filter((p) => isFiniteNumber(p.x) && isFiniteNumber(p.y));
  if (usable.length < 2) {
    return (
      <NoChartData
        message={`Not enough points to plot ${label}. A drawdown series needs at least two equity observations.`}
        height={height}
      />
    );
  }
  const pad = { top: 8, right: 8, bottom: 18, left: 52 };
  const w = width - pad.left - pad.right;
  const h = height - pad.top - pad.bottom;
  const xs = extent(usable.map((p) => p.x));
  const worst = Math.min(0, ...usable.map((p) => -Math.abs(p.y)));
  const span = Math.abs(worst) || 1;

  const sx = (v: number) => pad.left + ((v - xs.min) / (xs.max - xs.min)) * w;
  const sy = (v: number) => pad.top + (Math.abs(v) / span) * h;

  const d = usable
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${sx(p.x).toFixed(2)},${sy(p.y).toFixed(2)}`)
    .join(' ');
  const first = usable[0] as SeriesPoint;
  const last = usable[usable.length - 1] as SeriesPoint;
  const areaPath = `${d} L${sx(last.x).toFixed(2)},${pad.top} L${sx(first.x).toFixed(2)},${pad.top} Z`;

  return (
    <figure className={className}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="h-auto w-full"
        role="img"
        aria-label={`${label}. Worst drawdown ${formatPercent(Math.abs(worst))}.`}
        preserveAspectRatio="none"
      >
        <line x1={pad.left} x2={pad.left + w} y1={pad.top} y2={pad.top} className="chart-grid" />
        <text x={pad.left - 6} y={pad.top + 3} textAnchor="end" className="chart-axis-label">
          0%
        </text>
        <text x={pad.left - 6} y={pad.top + h + 3} textAnchor="end" className="chart-axis-label">
          {formatPercent(worst, 1)}
        </text>
        <path d={areaPath} fill="#b91c1c" fillOpacity={0.14} />
        <path d={d} fill="none" stroke="#b91c1c" strokeWidth={1.25} />
      </svg>
    </figure>
  );
}

// --- Confusion matrix -------------------------------------------------------

/**
 * The 3×3 confusion matrix as a shaded table. It is a real `<table>` rather than
 * an SVG because it is tabular data: a screen reader should read it by row and
 * column headers, not as one opaque image.
 */
export function ConfusionMatrixTable({
  counts,
  total,
  classes,
}: {
  counts: Partial<Record<string, Partial<Record<string, number>>>> | null;
  total: number;
  classes: readonly string[];
}) {
  if (!counts || total === 0) {
    return (
      <NoChartData
        message="The confusion matrix is empty: no predictions have resolved yet, so there is nothing to count."
        height={140}
      />
    );
  }
  const max = Math.max(
    1,
    ...classes.flatMap((p) => classes.map((a) => counts[p]?.[a] ?? 0)),
  );
  return (
    <div className="table-wrap">
      <table className="w-full min-w-[22rem] border-collapse text-sm">
        <caption className="sr-only">
          Confusion matrix: rows are the predicted class, columns are the class
          that actually occurred, over {total} resolved predictions.
        </caption>
        <thead>
          <tr>
            <th scope="col" className="px-2 py-1 text-xs text-ink-500">
              predicted ↓ / actual →
            </th>
            {classes.map((a) => (
              <th key={a} scope="col" className="px-2 py-1 text-center text-xs text-ink-600">
                {a}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {classes.map((p) => (
            <tr key={p}>
              <th scope="row" className="px-2 py-1 text-xs font-semibold text-ink-600">
                {p}
              </th>
              {classes.map((a) => {
                const n = counts[p]?.[a] ?? 0;
                const diag = p === a;
                return (
                  <td
                    key={a}
                    className={`num border border-ink-200 px-2 py-2 text-center ${
                      diag ? 'font-semibold' : ''
                    }`}
                    style={{
                      backgroundColor: `rgba(32, 36, 46, ${(n / max) * 0.16})`,
                    }}
                  >
                    {n}
                    {diag && n > 0 ? (
                      <span aria-hidden="true" className="ml-1 text-[10px] text-allow">
                        ✓
                      </span>
                    ) : null}
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

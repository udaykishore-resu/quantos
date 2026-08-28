/**
 * Backtest submission.
 *
 * Kept out of the generic read proxy deliberately: this is the one thing the
 * browser can ask the platform to *do*, so it gets its own handler with explicit
 * validation of every field before anything is forwarded. The API answers 202
 * with a queued run — a backtest is minutes of CPU and is executed off the
 * request path.
 */

import { NextResponse, type NextRequest } from 'next/server';

import { liveApi } from '@/lib/server/api';
import type { BacktestRequest, Interval } from '@/lib/types';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';

const INTERVALS: Interval[] = ['1m', '5m', '15m', '1h', '1d'];

const DISCLAIMER =
  'QuantOS is an educational research and paper-trading platform. ' +
  'Output is probabilistic, may be wrong, and is not financial advice. ' +
  'No real-money orders are placed.';

function bad(detail: string): NextResponse {
  return NextResponse.json(
    { error: { code: 'bad_request', message: 'invalid backtest request', detail }, disclaimer: DISCLAIMER },
    { status: 400 },
  );
}

export async function POST(req: NextRequest): Promise<NextResponse> {
  let body: unknown;
  try {
    body = await req.json();
  } catch {
    return bad('the request body must be JSON');
  }
  if (typeof body !== 'object' || body === null) return bad('expected a JSON object');
  const b = body as Record<string, unknown>;

  const strategyId = typeof b.strategy_id === 'string' ? b.strategy_id.trim() : '';
  if (!strategyId) return bad('strategy_id is required');

  const start = typeof b.start === 'string' ? b.start : '';
  const end = typeof b.end === 'string' ? b.end : '';
  if (!start || Number.isNaN(Date.parse(start))) {
    return bad('start must be an RFC3339 timestamp');
  }
  if (!end || Number.isNaN(Date.parse(end))) {
    return bad('end must be an RFC3339 timestamp');
  }
  if (Date.parse(end) <= Date.parse(start)) return bad('end must be after start');

  const interval = typeof b.interval === 'string' ? b.interval : '';
  if (interval && !INTERVALS.includes(interval as Interval)) {
    return bad(`interval must be one of ${INTERVALS.join(', ')}`);
  }

  const tickers = Array.isArray(b.tickers)
    ? b.tickers
        .filter((t): t is string => typeof t === 'string')
        .map((t) => t.trim().toUpperCase())
        .filter((t) => t.length > 0 && t.length <= 24 && /^[A-Z0-9.]+$/.test(t))
    : [];

  const request: BacktestRequest = {
    name: typeof b.name === 'string' && b.name.trim() ? b.name.trim().slice(0, 80) : 'adhoc',
    strategy_id: strategyId,
    start: new Date(start).toISOString(),
    end: new Date(end).toISOString(),
  };
  if (interval) request.interval = interval as Interval;
  if (typeof b.seed === 'number' && Number.isSafeInteger(b.seed)) request.seed = b.seed;
  if (typeof b.starting_cash === 'number' && b.starting_cash > 0) {
    request.starting_cash = b.starting_cash;
  }
  if (tickers.length > 0) request.tickers = tickers;

  const result = await liveApi().createBacktest(request);
  if (result.ok) {
    return NextResponse.json(
      { data: result.data, degraded: result.degraded, disclaimer: result.disclaimer, served_at: result.servedAt },
      { status: 202 },
    );
  }
  return NextResponse.json(
    {
      error: {
        code: result.code,
        message: result.message,
        detail: result.detail,
        request_id: result.requestId,
      },
      disclaimer: DISCLAIMER,
    },
    { status: result.status === 0 ? 503 : result.status },
  );
}

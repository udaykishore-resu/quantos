/**
 * Same-origin read proxy to the QuantOS API.
 *
 * Client components need live data, but they must never hold the API
 * credential. This handler is the seam: the browser calls
 * `/api/quantos/<path>` with no credentials at all, the Next.js server attaches
 * the bearer token (or serves the mock fixtures), and the response envelope
 * comes back untouched so the client sees exactly what the Go API said —
 * including `degraded`, `meta.stale` and the disclaimer.
 *
 * The allowlist is deliberate. A `[...path]` proxy without one is an open
 * relay: any authenticated endpoint the platform ever adds becomes reachable
 * from the browser the moment it ships. Only the read routes this dashboard
 * actually renders are forwarded.
 */

import { NextResponse, type NextRequest } from 'next/server';

import { liveApi } from '@/lib/server/api';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';

/**
 * Path patterns the browser may reach, as regular expressions anchored at both
 * ends. Every one is a GET read. Nothing that mutates platform state is
 * proxied: paper order submission and backtest creation go through explicit
 * server actions / route handlers with their own validation.
 */
const ALLOWED: readonly RegExp[] = [
  /^market\/regime$/,
  /^market\/regime\/history$/,
  /^market\/relationships$/,
  /^market\/status$/,
  /^stocks$/,
  /^stocks\/[A-Za-z0-9.\-_]{1,24}$/,
  /^stocks\/[A-Za-z0-9.\-_]{1,24}\/candles$/,
  /^signals$/,
  /^signals\/[A-Za-z0-9\-_]{1,64}$/,
  /^signals\/[A-Za-z0-9\-_]{1,64}\/provenance$/,
  /^predictions$/,
  /^predictions\/[A-Za-z0-9\-_]{1,64}$/,
  /^alerts$/,
  /^risk\/[A-Za-z0-9.\-_]{1,24}$/,
  /^models$/,
  /^model-health$/,
  /^evaluations$/,
  /^strategies$/,
  /^paper\/portfolio$/,
  /^paper\/positions$/,
  /^paper\/orders$/,
  /^news$/,
  /^brief$/,
  /^backtests$/,
  /^backtests\/[A-Za-z0-9\-_]{1,64}$/,
  /^audit$/,
];

/** Query parameters the upstream understands. Anything else is dropped. */
const ALLOWED_PARAMS = new Set([
  'limit',
  'offset',
  'from',
  'to',
  'ticker',
  'sector',
  'status',
  'strategy',
  'interval',
  'top',
]);

function deny(reason: string, status = 400): NextResponse {
  return NextResponse.json(
    {
      error: { code: status === 404 ? 'not_found' : 'bad_request', message: reason },
      disclaimer:
        'QuantOS is an educational research and paper-trading platform. ' +
        'Output is probabilistic, may be wrong, and is not financial advice. ' +
        'No real-money orders are placed.',
    },
    { status },
  );
}

export async function GET(
  req: NextRequest,
  ctx: { params: Promise<{ path: string[] }> },
): Promise<NextResponse> {
  const { path } = await ctx.params;
  const target = (path ?? []).join('/');

  if (!ALLOWED.some((re) => re.test(target))) {
    return deny(`the path /${target} is not proxied by this dashboard`, 404);
  }

  const params: Record<string, string> = {};
  for (const [k, v] of req.nextUrl.searchParams.entries()) {
    if (ALLOWED_PARAMS.has(k)) params[k] = v;
  }

  const result = await liveApi().request<unknown>(target, { params });

  if (result.ok) {
    return NextResponse.json(
      {
        data: result.data,
        meta: result.meta,
        degraded: result.degraded,
        disclaimer: result.disclaimer,
        request_id: result.requestId,
        served_at: result.servedAt,
      },
      { status: 200, headers: { 'Cache-Control': 'no-store' } },
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
      disclaimer:
        'QuantOS is an educational research and paper-trading platform. ' +
        'Output is probabilistic, may be wrong, and is not financial advice. ' +
        'No real-money orders are placed.',
    },
    {
      // A transport failure has no upstream status; report it as an upstream
      // outage rather than as a client error, so the UI retries correctly.
      status: result.status === 0 ? 503 : result.status,
      headers: { 'Cache-Control': 'no-store' },
    },
  );
}

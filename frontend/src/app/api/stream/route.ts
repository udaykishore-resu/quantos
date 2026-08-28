/**
 * Server-Sent Events proxy for `/api/v1/stream`.
 *
 * `EventSource` cannot set request headers, which is why the Go hub also accepts
 * `?access_token=`. This dashboard uses neither: putting the token in a query
 * string puts it in the browser, in `document.location`, and in every access log
 * between here and there. Instead the browser opens a same-origin `EventSource`
 * against this route, and the server holds the token.
 *
 * The upstream body is piped through **byte for byte**. That matters: the SSE
 * contract in `internal/httpx/sse.go` is carried entirely in the frame text —
 * the `id:` values that drive `Last-Event-ID` resume, the `event: resync` and
 * `event: overflow` control frames, the `retry:` hint and the `: heartbeat`
 * comments. Re-encoding here would risk losing one of them, and losing `resync`
 * in particular would mean the dashboard silently renders a state with a gap in
 * it.
 *
 * `Last-Event-ID` is forwarded from the browser so the hub can replay. Because
 * the browser sets that header itself on an automatic reconnect, resume works
 * through the proxy exactly as it would directly.
 */

import type { NextRequest } from 'next/server';

import { serverEnv } from '@/lib/server/env';
import { authHeaders } from '@/lib/server/token';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';
/** The stream is long-lived by design; this route must never be cut short. */
export const maxDuration = 3600;

const SSE_HEADERS: Record<string, string> = {
  'Content-Type': 'text/event-stream; charset=utf-8',
  'Cache-Control': 'no-cache, no-transform',
  Connection: 'keep-alive',
  // Same reason the Go handler sets it: without this nginx buffers events until
  // its buffer fills, which is indistinguishable from a broken stream.
  'X-Accel-Buffering': 'no',
};

function controlStream(event: string, reason: string, status = 200): Response {
  const body = `event: ${event}\ndata: ${JSON.stringify({ reason })}\n\nretry: 5000\n\n`;
  return new Response(body, { status, headers: SSE_HEADERS });
}

/**
 * In mock mode there is no hub. Rather than leave the browser retrying against
 * a dead route, emit a single explanatory frame and close: the dashboard's
 * disconnected-stream state is then exercised honestly instead of being faked.
 */
function mockStream(): Response {
  return controlStream(
    'overflow',
    'The dashboard is running against mocks/, so there is no live event ' +
      'stream. Values shown are fixtures and will not update.',
  );
}

export async function GET(req: NextRequest): Promise<Response> {
  const cfg = serverEnv();
  if (cfg.useMocks) return mockStream();

  const upstream = new URL(`${cfg.apiBase}/stream`);
  const topics = req.nextUrl.searchParams.get('topics');
  if (topics && /^[a-z_,]{1,200}$/.test(topics)) {
    upstream.searchParams.set('topics', topics);
  }

  const headers: Record<string, string> = {
    Accept: 'text/event-stream',
    ...(await authHeaders()),
  };
  // Resume. The browser sets this itself on reconnect; the query fallback
  // matches the hub's own, for a client that reconnects manually.
  const lastEventId =
    req.headers.get('last-event-id') ??
    req.nextUrl.searchParams.get('last_event_id');
  if (lastEventId && /^\d{1,20}$/.test(lastEventId)) {
    headers['Last-Event-ID'] = lastEventId;
  }

  let res: Response;
  try {
    res = await fetch(upstream, {
      headers,
      cache: 'no-store',
      signal: req.signal,
      // Node's fetch buffers the whole body without this.
      // @ts-expect-error -- undici-only option, not in the DOM RequestInit type.
      duplex: 'half',
    });
  } catch (err) {
    if (req.signal.aborted) return new Response(null, { status: 499 });
    return controlStream(
      'overflow',
      `The QuantOS event stream could not be reached: ${
        err instanceof Error ? err.message : String(err)
      }`,
      502,
    );
  }

  if (!res.ok || !res.body) {
    return controlStream(
      'overflow',
      `The QuantOS event stream answered HTTP ${res.status}. ` +
        (res.status === 401 || res.status === 403
          ? 'The dashboard is not authorised to subscribe; check its credentials.'
          : 'Reconnecting.'),
      res.status === 401 || res.status === 403 ? res.status : 502,
    );
  }

  return new Response(res.body, { status: 200, headers: SSE_HEADERS });
}

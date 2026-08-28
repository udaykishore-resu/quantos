import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  QuantosClient,
  buildQuery,
  interpretResponse,
  isFailure,
  joinUrl,
} from './api-client';
import { DISCLAIMER } from './types';

const MOCK_DIR = path.join(process.cwd(), 'mocks');
const fixture = (name: string): unknown =>
  JSON.parse(readFileSync(path.join(MOCK_DIR, `${name}.json`), 'utf8'));

/** A `fetch` that answers with a fixed status and body. */
function stubFetch(status: number, body: unknown, capture?: (url: string, init?: RequestInit) => void) {
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    capture?.(String(input), init);
    return new Response(typeof body === 'string' ? body : JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  }) as typeof fetch;
}

describe('URL construction', () => {
  it('joins base and path without doubling slashes', () => {
    expect(joinUrl('http://localhost:8080/api/v1', 'stocks')).toBe('http://localhost:8080/api/v1/stocks');
    expect(joinUrl('http://localhost:8080/api/v1/', '/stocks')).toBe('http://localhost:8080/api/v1/stocks');
    expect(joinUrl('/api/quantos', 'signals/abc')).toBe('/api/quantos/signals/abc');
  });

  it('refuses an absolute path, which would escape the configured host', () => {
    expect(() => joinUrl('http://localhost:8080/api/v1', 'http://evil.test/x')).toThrow();
    expect(() => joinUrl('/api/quantos', 'https://evil.test/x')).toThrow();
  });

  it('refuses path traversal', () => {
    expect(() => joinUrl('http://localhost:8080/api/v1', '../../admin')).toThrow();
  });

  it('omits empty and nullish query parameters', () => {
    expect(buildQuery({ limit: 10, ticker: 'BA' })).toBe('?limit=10&ticker=BA');
    expect(buildQuery({ limit: 10, ticker: '', sector: undefined, status: null })).toBe('?limit=10');
    expect(buildQuery({})).toBe('');
    expect(buildQuery(undefined)).toBe('');
  });

  it('encodes parameter values', () => {
    expect(buildQuery({ sector: 'Consumer Discretionary' })).toContain(
      'sector=Consumer+Discretionary',
    );
  });
});

describe('envelope interpretation', () => {
  it('unwraps a success envelope', () => {
    const r = interpretResponse<{ status: string }>(200, {
      data: { status: 'ready' },
      meta: { count: 3, limit: 10 },
      degraded: ['model'],
      disclaimer: DISCLAIMER,
      request_id: 'abc123',
      served_at: '2026-08-28T02:15:00Z',
    });
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.data).toEqual({ status: 'ready' });
    expect(r.meta?.count).toBe(3);
    expect(r.degraded).toEqual(['model']);
    expect(r.disclaimer).toBe(DISCLAIMER);
    expect(r.requestId).toBe('abc123');
  });

  it('always yields an array for degraded, even when the field is absent', () => {
    const r = interpretResponse<null>(200, {
      data: null,
      disclaimer: DISCLAIMER,
      served_at: '2026-08-28T02:15:00Z',
    });
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.degraded).toEqual([]);
  });

  it('preserves a null data payload — a Go nil slice is not an error', () => {
    const r = interpretResponse<string[] | null>(200, {
      data: null,
      disclaimer: DISCLAIMER,
      served_at: '2026-08-28T02:15:00Z',
    });
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.data).toBeNull();
  });

  it('maps a QuantOS error body to its stable code', () => {
    const r = interpretResponse(404, {
      error: {
        code: 'not_found',
        message: 'the resource does not exist',
        detail: 'unknown ticker ZZZZ',
        request_id: 'req-1',
      },
      disclaimer: DISCLAIMER,
    });
    expect(isFailure(r)).toBe(true);
    if (r.ok) return;
    expect(r.code).toBe('not_found');
    expect(r.detail).toBe('unknown ticker ZZZZ');
    expect(r.status).toBe(404);
    expect(r.requestId).toBe('req-1');
    expect(r.retryable).toBe(false);
  });

  it('marks 503, 429 and 5xx retryable and 4xx not', () => {
    const err = (code: string) => ({ error: { code, message: 'x' }, disclaimer: DISCLAIMER });
    expect((interpretResponse(503, err('unavailable')) as { retryable: boolean }).retryable).toBe(true);
    expect((interpretResponse(429, err('rate_limited')) as { retryable: boolean }).retryable).toBe(true);
    expect((interpretResponse(500, err('internal')) as { retryable: boolean }).retryable).toBe(true);
    expect((interpretResponse(400, err('bad_request')) as { retryable: boolean }).retryable).toBe(false);
    expect((interpretResponse(403, err('forbidden')) as { retryable: boolean }).retryable).toBe(false);
  });

  it('rejects a 200 that is not a QuantOS envelope', () => {
    // A proxy or captive portal answering 200 with HTML must not be rendered
    // as data.
    const r = interpretResponse(200, { hello: 'world' });
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.code).toBe('malformed_response');
  });

  it('falls back to an HTTP code when the error body is not recognisable', () => {
    const r = interpretResponse(502, '<html>bad gateway</html>');
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.code).toBe('http_502');
    expect(r.retryable).toBe(true);
  });
});

describe('client transport', () => {
  it('attaches the supplied auth header', async () => {
    let seenInit: RequestInit | undefined;
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      headers: () => ({ Authorization: 'Bearer tok-123' }),
      fetchImpl: stubFetch(200, { data: [], disclaimer: DISCLAIMER, served_at: '' }, (_u, i) => {
        seenInit = i;
      }),
    });
    await client.stocks();
    expect((seenInit?.headers as Record<string, string>).Authorization).toBe('Bearer tok-123');
  });

  it('sends no Authorization header when none is configured', async () => {
    let seenInit: RequestInit | undefined;
    const client = new QuantosClient({
      baseUrl: '/api/quantos',
      fetchImpl: stubFetch(200, { data: [], disclaimer: DISCLAIMER, served_at: '' }, (_u, i) => {
        seenInit = i;
      }),
    });
    await client.stocks();
    expect((seenInit?.headers as Record<string, string>).Authorization).toBeUndefined();
  });

  it('builds the right URL for each route', async () => {
    const seen: string[] = [];
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: stubFetch(200, { data: null, disclaimer: DISCLAIMER, served_at: '' }, (u) => {
        seen.push(u);
      }),
    });
    await client.stock('BA');
    await client.candles('BA', { interval: '5m', limit: 100 });
    await client.provenance('sig-1');
    await client.risk('AAPL');
    await client.signals({ status: 'ACTIVE', limit: 50 });
    expect(seen[0]).toBe('http://api.test/api/v1/stocks/BA');
    expect(seen[1]).toBe('http://api.test/api/v1/stocks/BA/candles?interval=5m&limit=100');
    expect(seen[2]).toBe('http://api.test/api/v1/signals/sig-1/provenance');
    expect(seen[3]).toBe('http://api.test/api/v1/risk/AAPL');
    expect(seen[4]).toBe('http://api.test/api/v1/signals?status=ACTIVE&limit=50');
  });

  it('URL-encodes path segments', async () => {
    const seen: string[] = [];
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: stubFetch(200, { data: null, disclaimer: DISCLAIMER, served_at: '' }, (u) => {
        seen.push(u);
      }),
    });
    await client.stock('BRK.B');
    await client.signal('a/b');
    expect(seen[0]).toBe('http://api.test/api/v1/stocks/BRK.B');
    expect(seen[1]).toBe('http://api.test/api/v1/signals/a%2Fb');
  });

  it('addresses /healthz and /readyz outside the versioned base', async () => {
    const seen: string[] = [];
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: stubFetch(200, { data: { status: 'ready' }, disclaimer: DISCLAIMER, served_at: '' }, (u) => {
        seen.push(u);
      }),
    });
    await client.health();
    await client.readiness();
    expect(seen[0]).toBe('http://api.test/healthz');
    expect(seen[1]).toBe('http://api.test/readyz');
  });

  it('does not mutate the base URL when addressing a root route', async () => {
    // A concurrency bug worth guarding: an implementation that swapped
    // `baseUrl` in place would corrupt a parallel request.
    const seen: string[] = [];
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: stubFetch(200, { data: null, disclaimer: DISCLAIMER, served_at: '' }, (u) => {
        seen.push(u);
      }),
    });
    await Promise.all([client.health(), client.stocks(), client.readiness(), client.signals()]);
    expect(seen).toContain('http://api.test/healthz');
    expect(seen).toContain('http://api.test/readyz');
    expect(seen).toContain('http://api.test/api/v1/stocks');
    expect(seen).toContain('http://api.test/api/v1/signals');
    expect(client.baseUrl).toBe('http://api.test/api/v1');
  });

  it('treats a 503 from /readyz as a real answer, not a client error', async () => {
    // `/readyz` answers 503 with the reason when the platform cannot serve its
    // purpose. That is information the banner needs, not a failed request.
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: stubFetch(503, {
        error: {
          code: 'unavailable',
          message: 'a required backing service is unavailable',
          detail: 'market data is stale; signal emission is suspended',
        },
        disclaimer: DISCLAIMER,
      }),
    });
    const r = await client.readiness();
    expect(r.ready).toBe(false);
    expect(r.reason).toBe('market data is stale; signal emission is suspended');
  });

  it('reports a network failure as retryable with status 0', async () => {
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: (async () => {
        throw new Error('ECONNREFUSED');
      }) as typeof fetch,
    });
    const r = await client.stocks();
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.code).toBe('network_error');
    expect(r.status).toBe(0);
    expect(r.retryable).toBe(true);
  });

  it('reports a timeout distinctly', async () => {
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      timeoutMs: 10,
      fetchImpl: ((_u: unknown, init?: RequestInit) =>
        new Promise((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => {
            const e = new Error('aborted');
            e.name = 'AbortError';
            reject(e);
          });
        })) as typeof fetch,
    });
    const r = await client.stocks();
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.code).toBe('timeout');
    expect(r.retryable).toBe(true);
  });

  it('reports a non-JSON body without throwing', async () => {
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: stubFetch(200, 'not json at all'),
    });
    const r = await client.stocks();
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.code).toBe('malformed_response');
  });

  it('POSTs a backtest with a JSON content type', async () => {
    let seenInit: RequestInit | undefined;
    const client = new QuantosClient({
      baseUrl: 'http://api.test/api/v1',
      fetchImpl: stubFetch(202, { data: { id: 'bt-1' }, disclaimer: DISCLAIMER, served_at: '' }, (_u, i) => {
        seenInit = i;
      }),
    });
    const r = await client.createBacktest({
      name: 'x',
      strategy_id: 'breakout',
      start: '2026-08-01T00:00:00Z',
      end: '2026-08-02T00:00:00Z',
    });
    expect(r.ok).toBe(true);
    expect(seenInit?.method).toBe('POST');
    expect((seenInit?.headers as Record<string, string>)['Content-Type']).toBe('application/json');
    expect(JSON.parse(String(seenInit?.body)).strategy_id).toBe('breakout');
  });
});

describe('captured fixtures parse through the real client path', () => {
  const cases: [string, string][] = [
    ['healthz', 'health'],
    ['market-regime', 'regime'],
    ['market-status', 'market status'],
    ['market-relationships', 'relationships'],
    ['stocks', 'ranked stocks'],
    ['predictions', 'predictions'],
    ['model-health', 'model health'],
    ['evaluations', 'evaluations'],
    ['strategies', 'strategies'],
    ['paper-portfolio', 'portfolio'],
    ['signals', 'signals'],
    ['alerts', 'alerts'],
    ['news', 'news'],
    ['backtests', 'backtests'],
    ['brief', 'brief'],
    ['audit', 'audit'],
    ['models', 'models'],
  ];

  for (const [name, label] of cases) {
    it(`accepts the ${label} fixture as a valid envelope`, () => {
      const r = interpretResponse(200, fixture(name));
      expect(r.ok, `${name} is not a valid QuantOS envelope`).toBe(true);
      if (!r.ok) return;
      // Every envelope carries the research disclaimer (governance rule G-9).
      expect(r.disclaimer).toBe(DISCLAIMER);
    });
  }

  it('carries the degraded list through from a real captured response', () => {
    // The captured platform was running without a model artifact.
    const r = interpretResponse(200, fixture('model-health'));
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(Array.isArray(r.degraded)).toBe(true);
  });
});

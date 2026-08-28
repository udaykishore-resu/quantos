/**
 * A `fetch` implementation backed by `frontend/mocks/`.
 *
 * This exists so the dashboard can be developed, demonstrated and tested with no
 * backend running, while exercising the *same* client code path as production:
 * the fixtures are complete response envelopes captured from the real Go API, so
 * they go through `interpretResponse` exactly as a live response does. A mock
 * layer that bypassed the envelope would let envelope bugs reach production.
 *
 * Every fixture in `mocks/` was captured from a running
 * `go run ./cmd/quantos run`, or hand-built to the same shape from the Go source
 * where the local run could not produce that state (see mocks/README.md).
 */

import { promises as fs } from 'node:fs';
import path from 'node:path';

const MOCK_DIR = path.join(process.cwd(), 'mocks');

/** In-process fixture cache; fixtures are immutable for the process lifetime. */
const cache = new Map<string, unknown>();

async function readFixture(name: string): Promise<unknown | null> {
  if (cache.has(name)) return cache.get(name) ?? null;
  try {
    const raw = await fs.readFile(path.join(MOCK_DIR, `${name}.json`), 'utf8');
    const parsed: unknown = JSON.parse(raw);
    cache.set(name, parsed);
    return parsed;
  } catch {
    cache.set(name, null);
    return null;
  }
}

function envelope(data: unknown, meta?: Record<string, unknown>): unknown {
  return {
    data,
    meta,
    disclaimer:
      'QuantOS is an educational research and paper-trading platform. ' +
      'Output is probabilistic, may be wrong, and is not financial advice. ' +
      'No real-money orders are placed.',
    request_id: 'mock',
    served_at: new Date().toISOString(),
  };
}

function notFound(detail: string): { status: number; body: unknown } {
  return {
    status: 404,
    body: {
      error: {
        code: 'not_found',
        message: 'the resource does not exist',
        detail,
        request_id: 'mock',
      },
      disclaimer:
        'QuantOS is an educational research and paper-trading platform. ' +
        'Output is probabilistic, may be wrong, and is not financial advice. ' +
        'No real-money orders are placed.',
    },
  };
}

function envelopeData(v: unknown): unknown {
  if (v && typeof v === 'object' && 'data' in (v as Record<string, unknown>)) {
    return (v as Record<string, unknown>).data;
  }
  return v;
}

function findById(items: unknown, id: string, key = 'id'): unknown | null {
  if (!Array.isArray(items)) return null;
  for (const it of items) {
    if (
      it &&
      typeof it === 'object' &&
      String((it as Record<string, unknown>)[key]) === id
    ) {
      return it;
    }
  }
  return null;
}

/**
 * Resolves an API path to a mock response.
 *
 * Exported for the unit tests, which assert that every route the client can
 * call has a fixture — a missing fixture should fail the test run, not surface
 * as an empty page during a demo.
 */
export async function resolveMock(
  pathname: string,
  method = 'GET',
): Promise<{ status: number; body: unknown }> {
  // Strip both the origin-level and versioned prefixes.
  const clean = pathname.replace(/^\/+/, '').replace(/^api\/v1\/?/, '');
  const segs = clean.split('/').filter(Boolean);
  const [a, b, c] = segs;

  if (method === 'POST' && a === 'backtests') {
    const run = envelopeData(await readFixture('backtest-queued'));
    if (run) return { status: 202, body: envelope(run) };
    return notFound('mocks/backtest-queued.json is missing');
  }

  const direct: Record<string, string> = {
    healthz: 'healthz',
    readyz: 'readyz',
    stocks: 'stocks',
    signals: 'signals',
    predictions: 'predictions',
    alerts: 'alerts',
    models: 'models',
    'model-health': 'model-health',
    evaluations: 'evaluations',
    strategies: 'strategies',
    news: 'news',
    brief: 'brief',
    audit: 'audit',
    backtests: 'backtests',
  };

  if (segs.length === 1 && a && direct[a]) {
    const fixture = await readFixture(direct[a] as string);
    if (fixture === null) return notFound(`mocks/${direct[a]}.json is missing`);
    return { status: a === 'readyz' ? readyStatus(fixture) : 200, body: fixture };
  }

  if (a === 'market') {
    if (b === 'regime' && c === 'history') return served('market-regime-history');
    if (b === 'regime') return served('market-regime');
    if (b === 'relationships') return served('market-relationships');
    if (b === 'status') return served('market-status');
  }

  if (a === 'stocks' && b) {
    const ticker = decodeURIComponent(b).toUpperCase();
    if (c === 'candles') {
      const perTicker = await readFixture(`candles-${ticker}`);
      if (perTicker) return { status: 200, body: perTicker };
      return served('candles');
    }
    const perTicker = await readFixture(`stock-${ticker}`);
    if (perTicker) return { status: 200, body: perTicker };
    // Fall back to synthesising a view from the ranked list so any ticker in
    // the mock universe resolves rather than 404-ing mid-demo.
    const ranked = envelopeData(await readFixture('stocks'));
    if (Array.isArray(ranked) && findById(ranked, ticker, 'ticker')) {
      const generic = await readFixture('stock');
      if (generic) return { status: 200, body: generic };
    }
    return notFound(`unknown ticker ${ticker}`);
  }

  if (a === 'signals' && b) {
    const signals = envelopeData(await readFixture('signals'));
    const sig = findById(signals, b);
    if (c === 'provenance') {
      const perId = await readFixture(`provenance-${b}`);
      if (perId) return { status: 200, body: perId };
      return served('provenance');
    }
    if (sig) return { status: 200, body: envelope(sig) };
    return notFound(`unknown signal ${b}`);
  }

  if (a === 'predictions' && b) {
    const preds = envelopeData(await readFixture('predictions'));
    const p = findById(preds, b);
    if (p) return { status: 200, body: envelope(p) };
    return notFound(`unknown prediction ${b}`);
  }

  if (a === 'risk' && b) {
    const ticker = decodeURIComponent(b).toUpperCase();
    const perTicker = await readFixture(`risk-${ticker}`);
    if (perTicker) return { status: 200, body: perTicker };
    const view = envelopeData(await readFixture(`stock-${ticker}`));
    if (view && typeof view === 'object' && 'risk' in (view as object)) {
      const risk = (view as Record<string, unknown>).risk;
      if (risk) return { status: 200, body: envelope(risk) };
    }
    return notFound(`no risk assessment recorded for ${ticker}`);
  }

  if (a === 'paper') {
    if (b === 'portfolio') return served('paper-portfolio');
    if (b === 'positions') return served('paper-positions');
    if (b === 'orders') return served('paper-orders');
  }

  if (a === 'backtests' && b) {
    const perId = await readFixture(`backtest-${b}`);
    if (perId) return { status: 200, body: perId };
    const runs = envelopeData(await readFixture('backtests'));
    const run = findById(runs, b);
    if (run) return { status: 200, body: envelope(run) };
    return notFound(`unknown backtest ${b}`);
  }

  return notFound(`no mock fixture for /${clean}`);

  async function served(name: string): Promise<{ status: number; body: unknown }> {
    const fixture = await readFixture(name);
    if (fixture === null) return notFound(`mocks/${name}.json is missing`);
    return { status: 200, body: fixture };
  }
}

/**
 * `/readyz` answers 503 when the platform cannot serve its purpose. The fixture
 * says which it is, so that the degraded-mode banner can be demonstrated
 * against mocks by editing one file.
 */
function readyStatus(fixture: unknown): number {
  const data = envelopeData(fixture);
  if (data && typeof data === 'object') {
    const status = (data as Record<string, unknown>).status;
    if (status === 'ready') return 200;
  }
  if (fixture && typeof fixture === 'object' && 'error' in (fixture as object)) {
    return 503;
  }
  return 200;
}

/** A `fetch` that serves `mocks/` and never touches the network. */
export const mockFetch: typeof fetch = async (input, init) => {
  const url =
    typeof input === 'string'
      ? input
      : input instanceof URL
        ? input.toString()
        : input.url;
  const { pathname } = new URL(url, 'http://mocks.local');
  const method = (init?.method ?? 'GET').toUpperCase();
  const { status, body } = await resolveMock(pathname, method);
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
  });
};

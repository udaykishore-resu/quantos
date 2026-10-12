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
  body?: string,
): Promise<{ status: number; body: unknown }> {
  // Strip both the origin-level and versioned prefixes.
  const clean = pathname.replace(/^\/+/, '').replace(/^api\/v1\/?/, '');
  const segs = clean.split('/').filter(Boolean);
  const [a, b, c] = segs;

  if (a === 'execution') return mockExecution(method, segs.slice(1), body);

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
  const sent = typeof init?.body === 'string' ? init.body : undefined;
  const { status, body } = await resolveMock(pathname, method, sent);
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
  });
};

// --- Execution desk -------------------------------------------------------------
//
// A tiny in-memory desk over the execution-*.json fixtures, so the approval
// page can be exercised end to end with no backend: approve, reject, halt and
// resume change what the next poll returns, for the life of the dev server.
// The step-up password is `operator`, matching the shipped dev principal.
// Open proposals are re-anchored to when the server started, so their
// countdowns are live rather than long expired.

const MOCK_PASSWORD = 'operator';
const mockEpoch = Date.now();
const execPatches = new Map<string, Record<string, unknown>>();
let execHalt: Record<string, unknown> = { halted: false, at: '0001-01-01T00:00:00Z' };

function errorBody(status: number, code: string, message: string, detail?: string) {
  return {
    status,
    body: {
      error: { code, message, detail, request_id: 'mock' },
      disclaimer:
        'QuantOS is an educational research and paper-trading platform. ' +
        'Output is probabilistic, may be wrong, and is not financial advice. ' +
        'No real-money orders are placed.',
    },
  };
}

async function mockTickets(): Promise<Record<string, unknown>[]> {
  const raw = envelopeData(await readFixture('execution-tickets'));
  const base = Array.isArray(raw) ? (raw as Record<string, unknown>[]) : [];
  let open = 0;
  return base.map((t) => {
    const patched = execPatches.get(String(t.id));
    if (t.status !== 'PROPOSED') return patched ?? t;
    // Count every fixture proposal, decided or not, so deciding one does not
    // shift the others' expiry.
    const slot = open++;
    if (patched) return patched;
    const created = mockEpoch - (3 + slot * 2) * 60_000;
    const expires = mockEpoch + (11 - slot * 7) * 60_000;
    return {
      ...t,
      created_at: new Date(created).toISOString(),
      expires_at: new Date(expires).toISOString(),
      history: [{ at: new Date(created).toISOString(), status: 'PROPOSED', by: t.source === 'signal' ? 'quantos' : 'operator' }],
    };
  });
}

function parseBody(body: string | undefined): Record<string, unknown> {
  try {
    const v: unknown = body ? JSON.parse(body) : {};
    return v && typeof v === 'object' ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

async function mockExecution(
  method: string,
  rest: string[],
  rawBody: string | undefined,
): Promise<{ status: number; body: unknown }> {
  const [b, c, d] = rest;
  const now = new Date().toISOString();

  if (method === 'GET') {
    if (b === 'status') {
      const st = envelopeData(await readFixture('execution-status'));
      if (!st || typeof st !== 'object') return notFound('mocks/execution-status.json is missing');
      const tickets = await mockTickets();
      return {
        status: 200,
        body: envelope({
          ...(st as Record<string, unknown>),
          halt: execHalt,
          pending: tickets.filter((t) => t.status === 'PROPOSED').length,
          working: tickets.filter((t) => ['APPROVED', 'SUBMITTED', 'PARTIALLY_FILLED'].includes(String(t.status))).length,
        }),
      };
    }
    if (b === 'positions') {
      const fx = await readFixture('execution-positions');
      return fx ? { status: 200, body: fx } : notFound('mocks/execution-positions.json is missing');
    }
    if (b === 'tickets' && !c) {
      const tickets = await mockTickets();
      return { status: 200, body: envelope(tickets, { count: tickets.length }) };
    }
    if (b === 'tickets' && c) {
      const t = findById(await mockTickets(), c);
      return t ? { status: 200, body: envelope(t) } : notFound(`unknown ticket ${c}`);
    }
    return notFound(`no mock fixture for /execution/${rest.join('/')}`);
  }

  const body = parseBody(rawBody);

  if (b === 'halt') {
    execHalt = { halted: true, by: 'operator', at: now, reason: String(body.reason ?? '') };
    return { status: 200, body: envelope({ halt: execHalt }) };
  }
  if (b === 'resume') {
    if (body.password !== MOCK_PASSWORD) {
      return errorBody(401, 'unauthorized', 'authentication required', 're-enter your password to confirm (mock: operator)');
    }
    execHalt = { halted: false, at: '0001-01-01T00:00:00Z' };
    return { status: 200, body: envelope({ halt: execHalt }) };
  }

  if (b === 'tickets' && c && (d === 'approve' || d === 'reject')) {
    const t = findById(await mockTickets(), c) as Record<string, unknown> | null;
    if (!t) return notFound(`unknown ticket ${c}`);
    if (t.status !== 'PROPOSED') return errorBody(409, 'conflict', 'the ticket has already been decided');
    const history = [...((t.history as unknown[]) ?? [])];

    if (d === 'reject') {
      const reason = String(body.reason ?? '');
      history.push({ at: now, status: 'REJECTED', by: 'operator', ...(reason ? { reason } : {}) });
      const out = { ...t, status: 'REJECTED', status_reason: reason, decided_by: 'operator', decided_at: now, history };
      execPatches.set(c, out);
      return { status: 200, body: envelope(out) };
    }

    if (body.password !== MOCK_PASSWORD) {
      return errorBody(401, 'unauthorized', 'authentication required', 're-enter your password to confirm (mock: operator)');
    }
    if (execHalt.halted) return errorBody(409, 'conflict', 'execution: the desk is halted');
    if (Date.parse(String(t.expires_at)) < Date.now()) {
      history.push({ at: now, status: 'EXPIRED', by: 'operator', reason: 'approved after expiry' });
      const out = { ...t, status: 'EXPIRED', status_reason: 'approved after expiry', decided_by: 'operator', decided_at: now, history };
      execPatches.set(c, out);
      return { status: 409, body: envelope(out) };
    }
    const qty = Number(t.quantity);
    const limit = Number(t.limit_price);
    const notional = qty * limit;
    // The one guardrail the fixtures can trip without live data, enforced
    // here the way the desk does so the blocked path can be seen in mocks.
    if (notional > 1000) {
      const reason = `order_notional: $${notional.toFixed(2)} is over the $1,000.00 per-order cap`;
      history.push({ at: now, status: 'BLOCKED', by: 'operator', reason });
      const out = {
        ...t,
        status: 'BLOCKED',
        status_reason: reason,
        decided_by: 'operator',
        decided_at: now,
        checks: [
          { name: 'allowlist', passed: true, detail: `${String(t.ticker)} is on the allowlist` },
          { name: 'order_notional', passed: false, detail: `$${notional.toFixed(2)} > $1,000.00` },
        ],
        history,
      };
      execPatches.set(c, out);
      return { status: 409, body: envelope(out) };
    }
    const checks = [
      { name: 'allowlist', passed: true, detail: `${String(t.ticker)} is on the allowlist` },
      { name: 'order_notional', passed: true, detail: `$${(qty * limit).toFixed(2)} ≤ $1,000.00` },
      { name: 'price_drift', passed: true, detail: '3.2 bps ≤ 50 bps' },
      { name: 'position_weight', passed: true, detail: 'within 10.0% after the order' },
      { name: 'daily_loss', passed: true, detail: 'day −0.3% > −2.0%' },
    ];
    history.push({ at: now, status: 'APPROVED', by: 'operator' }, { at: now, status: 'SUBMITTED', by: 'desk' });
    const out = {
      ...t,
      status: 'SUBMITTED',
      decided_by: 'operator',
      decided_at: now,
      checks,
      order: {
        id: `mock-${c}`,
        client_order_id: c,
        ticker: t.ticker,
        side: t.side,
        quantity: qty,
        limit_price: limit,
        status: 'PENDING',
        venue_status: 'accepted',
        filled_qty: 0,
        avg_fill_price: 0,
        submitted_at: now,
      },
      history,
    };
    execPatches.set(c, out);
    return { status: 200, body: envelope(out) };
  }

  return notFound(`no mock route for ${method} /execution/${rest.join('/')}`);
}

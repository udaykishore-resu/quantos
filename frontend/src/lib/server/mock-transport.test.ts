import { describe, expect, it } from 'vitest';

import { interpretResponse } from '../api-client';
import { resolveMock } from './mock-transport';

/**
 * Every route the dashboard can call must resolve to a fixture.
 *
 * A missing fixture should fail the test run rather than surface as an empty
 * page in the middle of a demo, which is the failure mode this file exists to
 * prevent.
 */
const ROUTES: string[] = [
  '/healthz',
  '/readyz',
  '/api/v1/market/regime',
  '/api/v1/market/regime/history',
  '/api/v1/market/relationships',
  '/api/v1/market/status',
  '/api/v1/stocks',
  '/api/v1/signals',
  '/api/v1/predictions',
  '/api/v1/alerts',
  '/api/v1/models',
  '/api/v1/model-health',
  '/api/v1/evaluations',
  '/api/v1/strategies',
  '/api/v1/paper/portfolio',
  '/api/v1/paper/positions',
  '/api/v1/paper/orders',
  '/api/v1/news',
  '/api/v1/brief',
  '/api/v1/audit',
  '/api/v1/backtests',
];

describe('mock fixture coverage', () => {
  for (const route of ROUTES) {
    it(`serves ${route}`, async () => {
      const { status, body } = await resolveMock(route);
      expect(status, `${route} did not resolve to a fixture`).toBe(200);
      const r = interpretResponse(status, body);
      expect(r.ok, `${route} returned a body that is not a valid envelope`).toBe(true);
    });
  }
});

describe('dynamic routes', () => {
  it('resolves an instrument view for a ticker with a fixture', async () => {
    const { status, body } = await resolveMock('/api/v1/stocks/BA');
    expect(status).toBe(200);
    const r = interpretResponse<{ stock: { ticker: string } }>(status, body);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.data.stock.ticker).toBe('BA');
  });

  it('is case-insensitive on tickers', async () => {
    const { status } = await resolveMock('/api/v1/stocks/ba');
    expect(status).toBe(200);
  });

  it('404s an unknown ticker rather than serving another instrument', async () => {
    const { status } = await resolveMock('/api/v1/stocks/ZZZZ');
    expect(status).toBe(404);
  });

  it('serves candles for a ticker', async () => {
    const { status, body } = await resolveMock('/api/v1/stocks/BA/candles');
    expect(status).toBe(200);
    const r = interpretResponse<unknown[]>(status, body);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(Array.isArray(r.data)).toBe(true);
    expect((r.data as unknown[]).length).toBeGreaterThan(1);
  });

  it('resolves a signal and its provenance by id', async () => {
    const listRes = await resolveMock('/api/v1/signals');
    const listEnv = interpretResponse<{ id: string }[]>(listRes.status, listRes.body);
    expect(listEnv.ok).toBe(true);
    if (!listEnv.ok) return;
    expect(listEnv.data.length).toBeGreaterThan(0);

    for (const sig of listEnv.data) {
      const one = await resolveMock(`/api/v1/signals/${sig.id}`);
      expect(one.status, `signal ${sig.id} not resolvable`).toBe(200);

      const prov = await resolveMock(`/api/v1/signals/${sig.id}/provenance`);
      expect(prov.status, `provenance for ${sig.id} not resolvable`).toBe(200);
      const p = interpretResponse<{ signal: { id: string }; reproducible: boolean }>(
        prov.status,
        prov.body,
      );
      expect(p.ok).toBe(true);
      if (!p.ok) return;
      expect(p.data.signal.id).toBe(sig.id);
    }
  });

  it('includes both a reproducible and a non-reproducible provenance record', async () => {
    // The provenance screen must be exercisable in both states: a complete
    // chain, and one with holes it has to refuse to present as complete.
    const listRes = await resolveMock('/api/v1/signals');
    const listEnv = interpretResponse<{ id: string }[]>(listRes.status, listRes.body);
    expect(listEnv.ok).toBe(true);
    if (!listEnv.ok) return;

    const flags: boolean[] = [];
    for (const sig of listEnv.data) {
      const prov = await resolveMock(`/api/v1/signals/${sig.id}/provenance`);
      const p = interpretResponse<{ reproducible: boolean; missing?: string[] }>(
        prov.status,
        prov.body,
      );
      if (p.ok) flags.push(p.data.reproducible);
    }
    expect(flags).toContain(true);
    expect(flags).toContain(false);
  });

  it('404s an unknown signal', async () => {
    const { status } = await resolveMock('/api/v1/signals/does-not-exist');
    expect(status).toBe(404);
  });

  it('resolves each backtest run by id', async () => {
    const listRes = await resolveMock('/api/v1/backtests');
    const listEnv = interpretResponse<{ id: string; status: string }[]>(
      listRes.status,
      listRes.body,
    );
    expect(listEnv.ok).toBe(true);
    if (!listEnv.ok) return;
    for (const run of listEnv.data) {
      const one = await resolveMock(`/api/v1/backtests/${run.id}`);
      expect(one.status, `backtest ${run.id} not resolvable`).toBe(200);
    }
    // Both a finished and an in-flight run, so both states are demonstrable.
    const statuses = listEnv.data.map((r) => r.status);
    expect(statuses).toContain('COMPLETED');
    expect(statuses).toContain('RUNNING');
  });

  it('accepts a POSTed backtest with 202, as the API does', async () => {
    const { status, body } = await resolveMock('/api/v1/backtests', 'POST');
    expect(status).toBe(202);
    const r = interpretResponse<{ status: string }>(status, body);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.data.status).toBe('QUEUED');
  });

  it('serves a risk assessment for a ticker', async () => {
    const { status, body } = await resolveMock('/api/v1/risk/BA');
    expect(status).toBe(200);
    const r = interpretResponse<{ decision: string; checks: unknown[] }>(status, body);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.data.decision).toBe('ALLOW_PAPER_SIGNAL');
    expect(r.data.checks.length).toBeGreaterThan(0);
  });

  it('404s an unmapped path rather than serving something arbitrary', async () => {
    const { status } = await resolveMock('/api/v1/not-a-route');
    expect(status).toBe(404);
  });
});

describe('fixture content honours the safety requirements', () => {
  it('the model-health fixture reports accuracy alongside a base rate', async () => {
    const { status, body } = await resolveMock('/api/v1/model-health');
    const r = interpretResponse<{
      overall: { accuracy: number; base_rate: number; samples: number };
    }>(status, body);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(typeof r.data.overall.base_rate).toBe('number');
    expect(r.data.overall.samples).toBeGreaterThan(0);
  });

  it('the completed backtest reports a deflated Sharpe with its trial count', async () => {
    const listRes = await resolveMock('/api/v1/backtests');
    const listEnv = interpretResponse<
      { status: string; metrics: { deflated_sharpe?: number; trials_considered?: number } }[]
    >(listRes.status, listRes.body);
    expect(listEnv.ok).toBe(true);
    if (!listEnv.ok) return;
    const done = listEnv.data.find((r) => r.status === 'COMPLETED');
    expect(done).toBeDefined();
    expect(typeof done?.metrics.deflated_sharpe).toBe('number');
    expect(done?.metrics.trials_considered).toBeGreaterThan(1);
  });

  it('the feature snapshot marks at least one cold feature', async () => {
    const listRes = await resolveMock('/api/v1/signals');
    const listEnv = interpretResponse<{ id: string }[]>(listRes.status, listRes.body);
    expect(listEnv.ok).toBe(true);
    if (!listEnv.ok) return;
    const first = listEnv.data[0];
    expect(first).toBeDefined();
    const prov = await resolveMock(`/api/v1/signals/${first?.id}/provenance`);
    const p = interpretResponse<{ feature_snapshot: { warm: Record<string, boolean> } }>(
      prov.status,
      prov.body,
    );
    expect(p.ok).toBe(true);
    if (!p.ok) return;
    const cold = Object.values(p.data.feature_snapshot.warm).filter((w) => w === false);
    expect(cold.length).toBeGreaterThan(0);
  });

  it('every signal carries at least one invalidation condition', async () => {
    // Governance rule G-6: domain.NewSignal refuses to build a signal without
    // one, so a fixture without one would be an impossible state.
    const { status, body } = await resolveMock('/api/v1/signals');
    const r = interpretResponse<{ id: string; invalidations: unknown[] }[]>(status, body);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    for (const sig of r.data) {
      expect(sig.invalidations.length, `signal ${sig.id} has no invalidation`).toBeGreaterThan(0);
    }
  });

  it('the risk assessment records every check status the UI must render', async () => {
    const { status, body } = await resolveMock('/api/v1/risk/BA');
    const r = interpretResponse<{ checks: { status: string }[] }>(status, body);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    const seen = new Set(r.data.checks.map((c) => c.status));
    expect(seen.has('PASS')).toBe(true);
    expect(seen.has('WARN')).toBe(true);
    expect(seen.has('SKIP')).toBe(true);
  });
});

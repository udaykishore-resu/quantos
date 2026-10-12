import { describe, expect, it } from 'vitest';

import { resolveMock } from './mock-transport';

type Env = { data: Record<string, unknown> & { status?: string } };

async function tickets(): Promise<Record<string, unknown>[]> {
  const r = await resolveMock('/api/v1/execution/tickets');
  return (r.body as { data: Record<string, unknown>[] }).data;
}

describe('mock execution desk', () => {
  it('serves proposals whose expiry is in the future', async () => {
    const open = (await tickets()).filter((t) => t.status === 'PROPOSED');
    expect(open.length).toBeGreaterThan(0);
    for (const t of open) expect(Date.parse(String(t.expires_at))).toBeGreaterThan(Date.now());
  });

  it('refuses an approval with the wrong password and leaves the ticket open', async () => {
    const id = String((await tickets()).find((t) => t.status === 'PROPOSED')?.id);
    const r = await resolveMock(`/api/v1/execution/tickets/${id}/approve`, 'POST', '{"password":"nope"}');
    expect(r.status).toBe(401);
    expect((await tickets()).find((t) => t.id === id)?.status).toBe('PROPOSED');
  });

  it('blocks an over-cap order with the failing check and returns the ticket with 409', async () => {
    const big = (await tickets()).find(
      (t) => t.status === 'PROPOSED' && Number(t.quantity) * Number(t.limit_price) > 1000,
    );
    expect(big).toBeDefined();
    const r = await resolveMock(`/api/v1/execution/tickets/${String(big?.id)}/approve`, 'POST', '{"password":"operator"}');
    expect(r.status).toBe(409);
    const t = (r.body as Env).data as { status: string; checks: { name: string; passed: boolean }[] };
    expect(t.status).toBe('BLOCKED');
    expect(t.checks.find((c) => c.name === 'order_notional')?.passed).toBe(false);
  });

  it('approves with the password, and the next read shows it submitted', async () => {
    const id = String((await tickets()).find((t) => t.status === 'PROPOSED')?.id);
    const r = await resolveMock(`/api/v1/execution/tickets/${id}/approve`, 'POST', '{"password":"operator"}');
    expect(r.status).toBe(200);
    expect((r.body as Env).data.status).toBe('SUBMITTED');
    expect((await tickets()).find((t) => t.id === id)?.status).toBe('SUBMITTED');
    const again = await resolveMock(`/api/v1/execution/tickets/${id}/approve`, 'POST', '{"password":"operator"}');
    expect(again.status).toBe(409);
  });

  it('halts without a password and needs one to resume', async () => {
    expect((await resolveMock('/api/v1/execution/halt', 'POST', '{}')).status).toBe(200);
    const st = (await resolveMock('/api/v1/execution/status')).body as { data: { halt: { halted: boolean } } };
    expect(st.data.halt.halted).toBe(true);

    const open = (await tickets()).find((t) => t.status === 'PROPOSED');
    if (open) {
      const blocked = await resolveMock(`/api/v1/execution/tickets/${String(open.id)}/approve`, 'POST', '{"password":"operator"}');
      expect(blocked.status).toBe(409);
    }

    expect((await resolveMock('/api/v1/execution/resume', 'POST', '{"password":"x"}')).status).toBe(401);
    expect((await resolveMock('/api/v1/execution/resume', 'POST', '{"password":"operator"}')).status).toBe(200);
  });
});

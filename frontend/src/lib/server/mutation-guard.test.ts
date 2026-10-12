import { describe, expect, it } from 'vitest';

import { TICKET_ID, mutationRefusal, readJsonObject } from './mutation-guard';

function req(headers: Record<string, string>, method = 'POST') {
  return { method, headers: new Headers(headers) };
}

const sameOrigin = {
  'content-type': 'application/json',
  origin: 'http://localhost:3000',
  host: 'localhost:3000',
  'sec-fetch-site': 'same-origin',
};

describe('mutationRefusal', () => {
  it('accepts a same-origin JSON POST from this page', () => {
    expect(mutationRefusal(req(sameOrigin))).toBeNull();
  });

  it('accepts one from a browser that sends no Sec-Fetch-Site', () => {
    const { 'sec-fetch-site': _, ...rest } = sameOrigin;
    expect(mutationRefusal(req(rest))).toBeNull();
  });

  it('accepts the forwarded host behind a port-forward or ingress', () => {
    expect(
      mutationRefusal(
        req({ ...sameOrigin, origin: 'https://quantos.example', host: 'dashboard:3000', 'x-forwarded-host': 'quantos.example' }),
      ),
    ).toBeNull();
  });

  // The attack these routes exist to stop: another site making the reader's
  // browser POST here so the server attaches the operator's token.
  it('refuses a cross-site request even with a JSON content type', () => {
    expect(mutationRefusal(req({ ...sameOrigin, 'sec-fetch-site': 'cross-site' }))).toMatch(/cross-site/);
  });

  it('refuses a request whose Origin names another host', () => {
    const { 'sec-fetch-site': _, ...rest } = sameOrigin;
    expect(mutationRefusal(req({ ...rest, origin: 'https://evil.example' }))).toMatch(/cross-origin/);
  });

  it('refuses a request with no Origin', () => {
    const { origin: _, ...rest } = sameOrigin;
    expect(mutationRefusal(req(rest))).toMatch(/no Origin/);
  });

  it('refuses a form post (the one cross-site write a browser allows without preflight)', () => {
    expect(mutationRefusal(req({ ...sameOrigin, 'content-type': 'text/plain' }))).toMatch(/application\/json/);
    expect(
      mutationRefusal(req({ ...sameOrigin, 'content-type': 'application/x-www-form-urlencoded' })),
    ).toMatch(/application\/json/);
  });

  it('refuses anything but POST', () => {
    expect(mutationRefusal(req(sameOrigin, 'GET'))).toMatch(/POST/);
  });
});

describe('TICKET_ID', () => {
  it('admits desk ids and nothing that could walk the API path', () => {
    expect(TICKET_ID.test('tk-01a7d1f2-aapl')).toBe(true);
    expect(TICKET_ID.test('01a7d1f2_9e')).toBe(true);
    for (const bad of ['', '../halt', 'a/b', 'a%2Fb', 'x'.repeat(65), 'id?x=1']) {
      expect(TICKET_ID.test(bad)).toBe(false);
    }
  });
});

describe('readJsonObject', () => {
  const body = (text: string) => ({ text: async () => text });

  it('reads an object, treats an empty body as {}, and refuses the rest', async () => {
    expect(await readJsonObject(body('{"password":"p"}'))).toEqual({ password: 'p' });
    expect(await readJsonObject(body(''))).toEqual({});
    expect(await readJsonObject(body('[1,2]'))).toBeNull();
    expect(await readJsonObject(body('not json'))).toBeNull();
    expect(await readJsonObject(body(JSON.stringify({ x: 'y'.repeat(5000) })))).toBeNull();
  });
});

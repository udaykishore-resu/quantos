/**
 * Forwarding for the execution desk's state-changing routes.
 *
 * These do not go through `QuantosClient`, on purpose. The desk answers a
 * blocked or expired approval with HTTP 409 *and the ticket* in a normal
 * envelope, so the owner can see which guardrail stopped the order. The shared
 * client treats any non-2xx envelope as an anonymous failure and drops the
 * body. Here the upstream status and body are passed through untouched.
 *
 * The password the owner re-enters for approve and resume travels in the
 * request body from the browser to this process and on to the API. It is never
 * logged, cached or echoed back.
 */

import 'server-only';

import { NextResponse } from 'next/server';

import { DISCLAIMER } from '../types';
import { serverEnv } from './env';
import { mockFetch } from './mock-transport';
import { authHeaders } from './token';

export function refuse(status: number, code: string, message: string, detail?: string): NextResponse {
  return NextResponse.json(
    { error: { code, message, detail }, disclaimer: DISCLAIMER },
    { status, headers: { 'Cache-Control': 'no-store' } },
  );
}

/** POSTs `body` to `/api/v1/<path>` and relays the response as-is. */
export async function forwardPost(path: string, body: Record<string, unknown>): Promise<NextResponse> {
  const cfg = serverEnv();
  const url = cfg.useMocks ? `http://mocks.local/api/v1/${path}` : `${cfg.apiBase}/${path}`;
  const doFetch = cfg.useMocks ? mockFetch : globalThis.fetch.bind(globalThis);

  let res: Response;
  try {
    res = await doFetch(url, {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        ...(cfg.useMocks ? {} : await authHeaders()),
      },
      body: JSON.stringify(body),
      cache: 'no-store',
      // An approval is already claimed on the server by the time it is
      // submitting; give it time to finish rather than reporting a timeout
      // for an order that was in fact placed.
      signal: AbortSignal.timeout(30_000),
    });
  } catch (err) {
    return refuse(
      503,
      'unavailable',
      'the QuantOS API could not be reached',
      err instanceof Error ? err.message : String(err),
    );
  }

  const text = await res.text();
  let parsed: unknown;
  try {
    parsed = text ? JSON.parse(text) : {};
  } catch {
    return refuse(502, 'malformed_response', 'the API returned a body that is not JSON', text.slice(0, 200));
  }
  return NextResponse.json(parsed, { status: res.status, headers: { 'Cache-Control': 'no-store' } });
}

/**
 * The server-side QuantOS client factory.
 *
 * Server components and route handlers call `api()` and get a fully configured
 * `QuantosClient`: base URL resolved, bearer token attached, or the whole thing
 * pointed at `mocks/` when `QUANTOS_USE_MOCKS=1`.
 */

import 'server-only';

import { QuantosClient } from '../api-client';
import { serverEnv } from './env';
import { mockFetch } from './mock-transport';
import { authHeaders } from './token';

/**
 * Builds a client for one render or one request.
 *
 * It is cheap — no connections are opened until a method is called — so there is
 * no pooling here, and no shared mutable state between requests.
 */
export function api(opts?: { revalidateSeconds?: number }): QuantosClient {
  const cfg = serverEnv();
  if (cfg.useMocks) {
    return new QuantosClient({
      baseUrl: 'http://mocks.local/api/v1',
      fetchImpl: mockFetch,
      cache: 'no-store',
    });
  }
  const revalidate = opts?.revalidateSeconds ?? cfg.revalidateSeconds;
  return new QuantosClient({
    baseUrl: cfg.apiBase,
    headers: authHeaders,
    ...(revalidate > 0
      ? { revalidateSeconds: revalidate }
      : { cache: 'no-store' as RequestCache }),
  });
}

/** A client that never serves a cached response — for live/interactive reads. */
export function liveApi(): QuantosClient {
  const cfg = serverEnv();
  if (cfg.useMocks) {
    return new QuantosClient({
      baseUrl: 'http://mocks.local/api/v1',
      fetchImpl: mockFetch,
      cache: 'no-store',
    });
  }
  return new QuantosClient({
    baseUrl: cfg.apiBase,
    headers: authHeaders,
    cache: 'no-store',
  });
}

export { serverEnv };

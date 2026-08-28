/**
 * Server-side token acquisition.
 *
 * QuantOS authenticates with OAuth2/OIDC bearer tokens (`internal/auth`). Two
 * shapes are supported here, matching the two the platform itself supports:
 *
 *  - **A supplied token** (`QUANTOS_API_TOKEN`). This is what a real deployment
 *    uses: the dashboard is given a token minted by the identity provider and
 *    never sees a credential.
 *
 *  - **The local dev/demo login** (`POST /api/v1/auth/login`). In `embedded` and
 *    `compose` mode QuantOS issues its own HS256 tokens to the principals listed
 *    under `auth.dev_users` in config/quantos.yaml — `demo/demo` and
 *    `operator/operator` as shipped. This path exists so `make dev` works with
 *    no ceremony, exactly as the Go side intends.
 *
 * In both cases the token lives only in this process's memory. It is refreshed
 * ahead of its expiry, and a refresh failure is surfaced rather than retried
 * silently, because a dashboard rendering stale data behind a dead token is
 * worse than one saying it cannot authenticate.
 */

import { serverEnv } from './env';

interface CachedToken {
  token: string;
  /** Epoch ms. `0` when the token has no known expiry (supplied statically). */
  expiresAt: number;
}

let cache: CachedToken | null = null;
let inFlight: Promise<string> | null = null;

/** Refresh this far before the token actually expires. */
const REFRESH_MARGIN_MS = 60_000;

interface LoginEnvelope {
  data?: {
    token?: string;
    principal?: { expires_at?: string; sub?: string; scopes?: string[] };
  };
}

/** Decodes a JWT's `exp` without verifying it — the server verifies, not us. */
export function jwtExpiryMs(token: string): number {
  const parts = token.split('.');
  if (parts.length !== 3) return 0;
  try {
    const payload = parts[1] as string;
    const b64 = payload.replace(/-/g, '+').replace(/_/g, '/');
    const json = Buffer.from(b64, 'base64').toString('utf8');
    const claims = JSON.parse(json) as { exp?: unknown };
    return typeof claims.exp === 'number' ? claims.exp * 1000 : 0;
  } catch {
    return 0;
  }
}

function valid(c: CachedToken | null, now: number): boolean {
  if (!c) return false;
  if (c.expiresAt === 0) return true;
  return c.expiresAt - REFRESH_MARGIN_MS > now;
}

async function login(): Promise<string> {
  const cfg = serverEnv();
  const res = await fetch(`${cfg.apiBase}/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ subject: cfg.devSubject, password: cfg.devPassword }),
    cache: 'no-store',
  });
  if (!res.ok) {
    throw new Error(
      `QuantOS login failed with HTTP ${res.status}. Set QUANTOS_API_TOKEN, or ` +
        `check QUANTOS_DEV_SUBJECT / QUANTOS_DEV_PASSWORD against auth.dev_users ` +
        `in config/quantos.yaml.`,
    );
  }
  const body = (await res.json()) as LoginEnvelope;
  const token = body.data?.token ?? '';
  if (!token) {
    // With `auth.enabled: false` the API answers 200 with an anonymous
    // principal and no token. That is a valid configuration: requests then
    // carry no Authorization header at all.
    return '';
  }
  return token;
}

/**
 * Returns a bearer token for the Go API, or `''` when the deployment has
 * authentication disabled.
 *
 * Concurrent callers share one in-flight login so a cold page render with a
 * dozen parallel fetches does not produce a dozen logins.
 */
export async function apiToken(now: number = Date.now()): Promise<string> {
  const cfg = serverEnv();
  if (cfg.useMocks) return '';
  if (cfg.token) {
    cache = { token: cfg.token, expiresAt: jwtExpiryMs(cfg.token) };
    return cfg.token;
  }
  if (valid(cache, now)) return (cache as CachedToken).token;
  if (inFlight) return inFlight;

  inFlight = (async () => {
    try {
      const token = await login();
      cache = { token, expiresAt: token ? jwtExpiryMs(token) : 0 };
      return token;
    } finally {
      inFlight = null;
    }
  })();
  return inFlight;
}

/** Builds the Authorization header, or an empty object when auth is disabled. */
export async function authHeaders(): Promise<Record<string, string>> {
  const token = await apiToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

/** Test seam. */
export function resetTokenCache(): void {
  cache = null;
  inFlight = null;
}

/**
 * Server-only configuration.
 *
 * Nothing in this file may be imported from a client component. The token this
 * module obtains is the reason: it is held in the Next.js server process, sent
 * to the Go API as an `Authorization` header, and never serialised into a page,
 * a prop, a cookie readable by script, or a response body.
 *
 * The dashboard therefore has no notion of "the browser's credentials" at all.
 * Browser code talks to this app's own `/api/quantos/*` proxy, which is
 * same-origin and unauthenticated from the browser's point of view.
 */

if (typeof window !== 'undefined') {
  throw new Error(
    'lib/server/env.ts was imported into browser code. It holds the API ' +
      'credentials and must stay on the server.',
  );
}

export interface ServerEnv {
  /** Origin of the Go API, no trailing slash, e.g. http://localhost:8080 */
  apiOrigin: string;
  /** Versioned base, e.g. http://localhost:8080/api/v1 */
  apiBase: string;
  /** Serve from `frontend/mocks/` instead of a live backend. */
  useMocks: boolean;
  /**
   * A pre-issued bearer token. Preferred in any deployment: the dashboard then
   * holds no password at all.
   */
  token: string;
  /**
   * Development credentials for the local `dev_users` login flow, matching
   * `auth.dev_users` in config/quantos.yaml. Used only when no token is set.
   */
  devSubject: string;
  devPassword: string;
  /** Seconds of server-side cache for list endpoints. */
  revalidateSeconds: number;
}

function env(name: string, fallback = ''): string {
  const v = process.env[name];
  return v === undefined || v === '' ? fallback : v;
}

let cached: ServerEnv | null = null;

export function serverEnv(): ServerEnv {
  if (cached) return cached;
  const origin = env('QUANTOS_API_URL', 'http://localhost:8080').replace(/\/+$/, '');
  const revalidate = Number.parseInt(env('QUANTOS_REVALIDATE_SECONDS', '5'), 10);
  cached = {
    apiOrigin: origin,
    apiBase: `${origin}/api/v1`,
    useMocks: env('QUANTOS_USE_MOCKS') === '1' || env('QUANTOS_USE_MOCKS') === 'true',
    token: env('QUANTOS_API_TOKEN'),
    devSubject: env('QUANTOS_DEV_SUBJECT', 'operator'),
    devPassword: env('QUANTOS_DEV_PASSWORD', 'operator'),
    revalidateSeconds: Number.isFinite(revalidate) ? revalidate : 5,
  };
  return cached;
}

/** Test seam: forget the memoised environment. */
export function resetServerEnv(): void {
  cached = null;
}

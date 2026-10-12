/**
 * The checks every state-changing route in this dashboard runs before it
 * forwards anything to the platform.
 *
 * The dashboard holds the API credential on the server and the browser holds
 * none, so from the Go API's point of view every request this app forwards is
 * the operator's. That makes these routes the thing a hostile page would aim
 * at: if evil.example could make the reader's browser POST to
 * localhost:3000/api/execution/halt, the server would attach the token and the
 * desk would obey. Approval also needs the password, but halting deliberately
 * does not, and nothing here should rely on that.
 *
 * So a mutation is accepted only when the browser itself says it came from
 * this origin:
 *
 *  - `Content-Type: application/json`. A cross-site HTML form cannot send it,
 *    and a cross-site fetch that sets it triggers a CORS preflight this app
 *    never answers.
 *  - `Sec-Fetch-Site`, when present, is `same-origin`. Every current browser
 *    sends it and a page cannot forge it.
 *  - `Origin`, which browsers attach to every cross-origin request and to every
 *    POST, must name this host. A missing Origin is refused: browsers always
 *    send it on a POST, so its absence means the caller is not a browser on
 *    this page.
 *
 * Kept free of `server-only` and Next imports so it can be unit-tested.
 */

/** IDs the desk issues: UUID-ish, letters, digits, hyphens and underscores. */
export const TICKET_ID = /^[A-Za-z0-9\-_]{1,64}$/;

export interface GuardInput {
  method: string;
  headers: Headers;
}

/** Returns null when the request may proceed, or the reason it may not. */
export function mutationRefusal(req: GuardInput): string | null {
  if (req.method !== 'POST') return 'only POST is accepted';

  const ctype = (req.headers.get('content-type') ?? '').toLowerCase();
  if (!ctype.startsWith('application/json')) {
    return 'the request must be application/json';
  }

  const site = req.headers.get('sec-fetch-site');
  if (site !== null && site !== 'same-origin') {
    return `cross-site request refused (sec-fetch-site: ${site})`;
  }

  const origin = req.headers.get('origin');
  if (!origin) return 'the request has no Origin header';
  let originHost: string;
  try {
    originHost = new URL(origin).host;
  } catch {
    return 'the Origin header is not a URL';
  }
  // Behind kubectl port-forward or an ingress the Host header can be the
  // in-cluster name while the browser saw the forwarded one; accept either.
  const hosts = [req.headers.get('x-forwarded-host'), req.headers.get('host')]
    .filter((h): h is string => typeof h === 'string' && h.length > 0)
    .flatMap((h) => h.split(',').map((s) => s.trim()));
  if (!hosts.includes(originHost)) {
    return `cross-origin request refused (origin ${originHost})`;
  }
  return null;
}

/** Reads a small JSON object body, or returns null when it is not one. */
export async function readJsonObject(
  req: { text(): Promise<string> },
  maxBytes = 4096,
): Promise<Record<string, unknown> | null> {
  let text: string;
  try {
    text = await req.text();
  } catch {
    return null;
  }
  if (text.length > maxBytes) return null;
  if (text.trim() === '') return {};
  try {
    const v: unknown = JSON.parse(text);
    return typeof v === 'object' && v !== null && !Array.isArray(v)
      ? (v as Record<string, unknown>)
      : null;
  } catch {
    return null;
  }
}

/** A trimmed string field, capped, or '' when absent or not a string. */
export function stringField(
  body: Record<string, unknown>,
  key: string,
  max: number,
): string {
  const v = body[key];
  return typeof v === 'string' ? v.trim().slice(0, max) : '';
}

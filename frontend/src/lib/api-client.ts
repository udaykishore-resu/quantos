/**
 * The typed QuantOS API client.
 *
 * This is the *only* module that knows the shape of the response envelope, the
 * base URL, or the authorisation header. Everything else in the dashboard sees
 * a `Result<T>` and nothing more.
 *
 * The client is transport-agnostic on purpose: it takes a `fetch`, a base URL
 * and an optional header supplier. That is what lets the same code run in the
 * Next.js server (talking to the Go API with a bearer token that never leaves
 * the server) and in the browser (talking to this app's own proxy route, which
 * needs no token at all).
 */

import type {
  Alert,
  ApiErrorBody,
  AuditEvent,
  BacktestRequest,
  BacktestRun,
  Brief,
  Candle,
  Envelope,
  EvaluationsResponse,
  HealthResponse,
  InstrumentView,
  MarketRegime,
  MarketStatusResponse,
  Meta,
  ModelHealthResponse,
  ModelInfo,
  NewsEvent,
  PaperOrder,
  PaperPosition,
  PortfolioResponse,
  Prediction,
  Provenance,
  Ranked,
  Readiness,
  RelationshipState,
  RiskAssessment,
  Signal,
  SignalStatus,
  Strategy,
} from './types';

// --- Result -----------------------------------------------------------------

export interface ApiFailure {
  ok: false;
  /** The stable code from the Go error body, or a transport-level code. */
  code: string;
  message: string;
  detail?: string;
  status: number;
  requestId?: string;
  /** True when retrying later is the right response (503, 429, network). */
  retryable: boolean;
}

export interface ApiSuccess<T> {
  ok: true;
  data: T;
  meta: Meta | undefined;
  /** Subsystems the server reported as unavailable when it answered. */
  degraded: string[];
  disclaimer: string;
  requestId: string | undefined;
  servedAt: string;
}

export type Result<T> = ApiSuccess<T> | ApiFailure;

export function isFailure<T>(r: Result<T>): r is ApiFailure {
  return !r.ok;
}

// --- Configuration ----------------------------------------------------------

export interface ClientConfig {
  /** Base URL including the `/api/v1` prefix. */
  baseUrl: string;
  fetchImpl?: typeof fetch;
  /**
   * Extra headers per request — this is where a bearer token is attached. It is
   * a function rather than a value so a token can be refreshed without
   * rebuilding the client, and so that a client constructed in the browser can
   * simply not supply one.
   */
  headers?: () => Promise<Record<string, string>> | Record<string, string>;
  /** Milliseconds before a request is abandoned. */
  timeoutMs?: number;
  /** Passed through to `fetch` on the server to control Next.js caching. */
  cache?: RequestCache;
  revalidateSeconds?: number;
}

export const DEFAULT_TIMEOUT_MS = 12_000;

export interface QueryParams {
  [key: string]: string | number | boolean | undefined | null;
}

export function buildQuery(params?: QueryParams): string {
  if (!params) return '';
  const usp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue;
    usp.set(k, String(v));
  }
  const s = usp.toString();
  return s ? `?${s}` : '';
}

/**
 * Joins the base URL and a path without producing a double slash and without
 * letting a caller-supplied path escape the base (`..`, or an absolute URL).
 */
export function joinUrl(baseUrl: string, path: string): string {
  if (/^[a-z]+:\/\//i.test(path)) {
    throw new Error(`api: absolute path is not allowed: ${path}`);
  }
  const base = baseUrl.replace(/\/+$/, '');
  const rel = path.replace(/^\/+/, '');
  if (rel.split('/').some((seg) => seg === '..')) {
    throw new Error(`api: path traversal is not allowed: ${path}`);
  }
  return `${base}/${rel}`;
}

// --- Envelope handling ------------------------------------------------------

function isEnvelope<T>(v: unknown): v is Envelope<T> {
  return (
    typeof v === 'object' &&
    v !== null &&
    'data' in v &&
    'disclaimer' in (v as Record<string, unknown>)
  );
}

function isErrorBody(v: unknown): v is ApiErrorBody {
  if (typeof v !== 'object' || v === null) return false;
  const e = (v as Record<string, unknown>).error;
  return typeof e === 'object' && e !== null && 'code' in e;
}

/**
 * Turns a parsed body and a status into a `Result`.
 *
 * Exported so it can be unit-tested against captured fixtures without a socket.
 */
export function interpretResponse<T>(
  status: number,
  body: unknown,
): Result<T> {
  if (status >= 200 && status < 300) {
    if (!isEnvelope<T>(body)) {
      return {
        ok: false,
        code: 'malformed_response',
        message: 'the server returned a body that is not a QuantOS envelope',
        status,
        retryable: false,
      };
    }
    return {
      ok: true,
      data: body.data,
      meta: body.meta,
      degraded: Array.isArray(body.degraded) ? body.degraded : [],
      disclaimer: body.disclaimer,
      requestId: body.request_id,
      servedAt: body.served_at,
    };
  }
  if (isErrorBody(body)) {
    return {
      ok: false,
      code: body.error.code,
      message: body.error.message,
      detail: body.error.detail,
      status,
      requestId: body.error.request_id,
      retryable: status === 503 || status === 429 || status >= 500,
    };
  }
  return {
    ok: false,
    code: `http_${status}`,
    message: `the server returned HTTP ${status}`,
    status,
    retryable: status === 503 || status === 429 || status >= 500,
  };
}

// --- Client -----------------------------------------------------------------

export class QuantosClient {
  private readonly cfg: Required<Pick<ClientConfig, 'baseUrl' | 'timeoutMs'>> &
    ClientConfig;

  constructor(cfg: ClientConfig) {
    this.cfg = { timeoutMs: DEFAULT_TIMEOUT_MS, ...cfg };
  }

  get baseUrl(): string {
    return this.cfg.baseUrl;
  }

  private get fetchImpl(): typeof fetch {
    return this.cfg.fetchImpl ?? globalThis.fetch;
  }

  async request<T>(
    path: string,
    opts: {
      params?: QueryParams;
      method?: 'GET' | 'POST';
      body?: unknown;
      signal?: AbortSignal;
      /** Overrides the versioned base — used only for `/healthz` and `/readyz`. */
      base?: string;
    } = {},
  ): Promise<Result<T>> {
    let url: string;
    try {
      url = joinUrl(opts.base ?? this.cfg.baseUrl, path) + buildQuery(opts.params);
    } catch (err) {
      return {
        ok: false,
        code: 'bad_request',
        message: err instanceof Error ? err.message : 'invalid request path',
        status: 0,
        retryable: false,
      };
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.cfg.timeoutMs);
    if (opts.signal) {
      if (opts.signal.aborted) controller.abort();
      else opts.signal.addEventListener('abort', () => controller.abort(), { once: true });
    }

    try {
      const extra = this.cfg.headers ? await this.cfg.headers() : {};
      const headers: Record<string, string> = { Accept: 'application/json', ...extra };
      if (opts.body !== undefined) headers['Content-Type'] = 'application/json';

      const init: RequestInit & { next?: { revalidate: number } } = {
        method: opts.method ?? 'GET',
        headers,
        signal: controller.signal,
      };
      if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
      if (this.cfg.cache) init.cache = this.cfg.cache;
      if (this.cfg.revalidateSeconds !== undefined) {
        init.next = { revalidate: this.cfg.revalidateSeconds };
      }

      const res = await this.fetchImpl(url, init);
      const text = await res.text();
      let parsed: unknown = null;
      if (text.length > 0) {
        try {
          parsed = JSON.parse(text);
        } catch {
          return {
            ok: false,
            code: 'malformed_response',
            message: 'the server returned a body that is not valid JSON',
            detail: text.slice(0, 200),
            status: res.status,
            retryable: false,
          };
        }
      }
      return interpretResponse<T>(res.status, parsed);
    } catch (err) {
      const aborted =
        (err instanceof Error && err.name === 'AbortError') || controller.signal.aborted;
      return {
        ok: false,
        code: aborted ? 'timeout' : 'network_error',
        message: aborted
          ? `the API did not respond within ${this.cfg.timeoutMs}ms`
          : 'the API could not be reached',
        detail: err instanceof Error ? err.message : String(err),
        status: 0,
        retryable: true,
      };
    } finally {
      clearTimeout(timer);
    }
  }

  // --- Health -------------------------------------------------------------
  // `/healthz` and `/readyz` sit outside `/api/v1`, so they are addressed
  // relative to the origin rather than the versioned base.

  /** The origin, with the `/api/v1` suffix stripped. */
  get rootBase(): string {
    return this.cfg.baseUrl.replace(/\/+$/, '').replace(/\/api\/v1$/, '');
  }

  health(): Promise<Result<HealthResponse>> {
    return this.request<HealthResponse>('healthz', { base: this.rootBase });
  }

  /**
   * Readiness, collapsed to a single value.
   *
   * `/readyz` answers 200 when the platform can serve its purpose and 503 with
   * an explanatory detail when it cannot. Both are legitimate answers, so
   * neither is treated as a client error here.
   */
  async readiness(): Promise<Readiness> {
    const r = await this.request<{ status: string }>('readyz', {
      base: this.rootBase,
    });
    if (r.ok) return { ready: true, reason: '', degraded: r.degraded };
    return {
      ready: false,
      reason: r.detail || r.message,
      degraded: [],
    };
  }

  // --- Market -------------------------------------------------------------

  regime() {
    return this.request<MarketRegime>('market/regime');
  }

  regimeHistory(params?: { limit?: number; from?: string }) {
    return this.request<MarketRegime[] | null>('market/regime/history', { params });
  }

  relationships() {
    return this.request<RelationshipState>('market/relationships');
  }

  marketStatus() {
    return this.request<MarketStatusResponse>('market/status');
  }

  stocks(params?: { limit?: number; sector?: string }) {
    return this.request<Ranked[] | null>('stocks', { params });
  }

  stock(ticker: string) {
    return this.request<InstrumentView>(`stocks/${encodeURIComponent(ticker)}`);
  }

  candles(
    ticker: string,
    params?: { interval?: string; limit?: number; from?: string; to?: string },
  ) {
    return this.request<Candle[] | null>(
      `stocks/${encodeURIComponent(ticker)}/candles`,
      { params },
    );
  }

  news(params?: { ticker?: string; limit?: number }) {
    return this.request<NewsEvent[] | null>('news', { params });
  }

  brief(params?: { top?: number }) {
    return this.request<Brief>('brief', { params });
  }

  // --- Signals ------------------------------------------------------------

  signals(params?: {
    status?: SignalStatus | '';
    ticker?: string;
    strategy?: string;
    limit?: number;
    offset?: number;
    from?: string;
  }) {
    return this.request<Signal[] | null>('signals', { params });
  }

  signal(id: string) {
    return this.request<Signal>(`signals/${encodeURIComponent(id)}`);
  }

  provenance(id: string) {
    return this.request<Provenance>(
      `signals/${encodeURIComponent(id)}/provenance`,
    );
  }

  predictions(params?: { ticker?: string; limit?: number; from?: string }) {
    return this.request<Prediction[] | null>('predictions', { params });
  }

  prediction(id: string) {
    return this.request<Prediction>(`predictions/${encodeURIComponent(id)}`);
  }

  alerts(params?: { ticker?: string; limit?: number; offset?: number; from?: string }) {
    return this.request<Alert[] | null>('alerts', { params });
  }

  risk(ticker: string) {
    return this.request<RiskAssessment>(`risk/${encodeURIComponent(ticker)}`);
  }

  // --- Models -------------------------------------------------------------

  models() {
    return this.request<ModelInfo[] | null>('models');
  }

  modelHealth() {
    return this.request<ModelHealthResponse>('model-health');
  }

  evaluations(params?: { limit?: number }) {
    return this.request<EvaluationsResponse>('evaluations', { params });
  }

  strategies() {
    return this.request<Strategy[] | null>('strategies');
  }

  // --- Paper trading ------------------------------------------------------

  portfolio() {
    return this.request<PortfolioResponse>('paper/portfolio');
  }

  positions() {
    return this.request<PaperPosition[] | null>('paper/positions');
  }

  orders(params?: { limit?: number }) {
    return this.request<PaperOrder[] | null>('paper/orders', { params });
  }

  // --- Backtests ----------------------------------------------------------

  backtests(params?: { limit?: number }) {
    return this.request<BacktestRun[] | null>('backtests', { params });
  }

  backtest(id: string) {
    return this.request<BacktestRun>(`backtests/${encodeURIComponent(id)}`);
  }

  /** Accepted asynchronously: the API answers 202 with the queued run. */
  createBacktest(req: BacktestRequest) {
    return this.request<BacktestRun>('backtests', { method: 'POST', body: req });
  }

  // --- Audit --------------------------------------------------------------

  audit(params?: { limit?: number }) {
    return this.request<AuditEvent[] | null>('audit', { params });
  }
}

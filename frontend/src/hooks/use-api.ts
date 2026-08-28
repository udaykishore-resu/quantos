'use client';

/**
 * The client-side typed fetch layer.
 *
 * Deliberately small. It exists to give browser components the same four states
 * every remote read really has — loading, empty, error, stale — as explicit
 * values rather than as `data === undefined` guesswork, because "a blank chart
 * is a bug" is only enforceable if the blank cases are nameable.
 *
 * It talks to this app's own `/api/quantos/*` proxy, so no credential is
 * involved on this side of the wire.
 */

import { useCallback, useEffect, useRef, useState } from 'react';

import {
  QuantosClient,
  type ApiFailure,
  type QueryParams,
  type Result,
} from '@/lib/api-client';
import type { Meta } from '@/lib/types';

/** The browser-side client. Same code path as the server, no auth header. */
export const browserClient = new QuantosClient({
  baseUrl: '/api/quantos',
  cache: 'no-store',
});

export interface RemoteState<T> {
  /** True until the first response of any kind has arrived. */
  loading: boolean;
  /** True while a refetch is in flight over data already on screen. */
  refreshing: boolean;
  data: T | null;
  meta: Meta | undefined;
  degraded: string[];
  error: ApiFailure | null;
  /** When the current `data` was served, or null if there is none. */
  servedAt: string | null;
  /** Server-reported staleness (`meta.stale`) for this payload. */
  stale: boolean;
  staleReason: string;
  refetch: () => Promise<void>;
}

export interface UseApiOptions {
  /** Milliseconds between automatic refetches. 0 disables polling. */
  pollMs?: number;
  /** Set false to skip the request entirely. */
  enabled?: boolean;
}

/**
 * Runs one client request and tracks its state.
 *
 * `fetcher` must be stable — wrap it in `useCallback` — or pass a `key` that
 * changes only when the request should actually be re-issued.
 */
export function useApi<T>(
  key: string,
  fetcher: (client: QuantosClient, signal: AbortSignal) => Promise<Result<T>>,
  options: UseApiOptions = {},
): RemoteState<T> {
  const { pollMs = 0, enabled = true } = options;

  const [loading, setLoading] = useState(enabled);
  const [refreshing, setRefreshing] = useState(false);
  const [data, setData] = useState<T | null>(null);
  const [meta, setMeta] = useState<Meta | undefined>(undefined);
  const [degraded, setDegraded] = useState<string[]>([]);
  const [error, setError] = useState<ApiFailure | null>(null);
  const [servedAt, setServedAt] = useState<string | null>(null);

  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const hasDataRef = useRef(false);
  const abortRef = useRef<AbortController | null>(null);

  const run = useCallback(async () => {
    if (!enabled) return;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    if (hasDataRef.current) setRefreshing(true);
    else setLoading(true);

    const result = await fetcherRef.current(browserClient, controller.signal);
    if (controller.signal.aborted) return;

    if (result.ok) {
      setData(result.data);
      setMeta(result.meta);
      setDegraded(result.degraded);
      setServedAt(result.servedAt);
      setError(null);
      hasDataRef.current = true;
    } else {
      // Keep the last good payload on screen and surface the error alongside
      // it. Blanking the page on a transient 503 loses information the reader
      // still needs; what they must not lose is the fact that it is old.
      setError(result);
    }
    setLoading(false);
    setRefreshing(false);
  }, [enabled]);

  useEffect(() => {
    hasDataRef.current = false;
    void run();
    return () => abortRef.current?.abort();
    // `key` is the request identity: changing it re-issues the request.
  }, [key, run]);

  useEffect(() => {
    if (!enabled || pollMs <= 0) return;
    const id = setInterval(() => void run(), pollMs);
    return () => clearInterval(id);
  }, [enabled, pollMs, run]);

  return {
    loading,
    refreshing,
    data,
    meta,
    degraded,
    error,
    servedAt,
    stale: meta?.stale === true,
    staleReason: meta?.stale_reason ?? '',
    refetch: run,
  };
}

/** Convenience for the common "GET a path with params" case. */
export function useApiPath<T>(
  path: string,
  params?: QueryParams,
  options: UseApiOptions = {},
): RemoteState<T> {
  const key = `${path}?${JSON.stringify(params ?? {})}`;
  const fetcher = useCallback(
    (client: QuantosClient, signal: AbortSignal) =>
      client.request<T>(path, { params, signal }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key],
  );
  return useApi<T>(key, fetcher, options);
}

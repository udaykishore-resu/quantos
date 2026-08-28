'use client';

/**
 * Subscribes to the QuantOS event stream.
 *
 * All of the contract reasoning lives in `lib/sse-reducer.ts`; this hook is the
 * plumbing that connects a browser `EventSource` to it.
 *
 * The one behaviour worth reading carefully is the resync path. When the server
 * says `resync` (the replay buffer no longer covers our last event id) or
 * `overflow` (we were too slow and were dropped), the reducer raises
 * `needsResync`. This hook then calls the caller's `onResync` — which is how the
 * page refetches over REST — and only dispatches `resynced` once that refetch
 * has actually resolved. Until then every consumer can see, from
 * `health.healthy === false`, that what is on screen may be missing events.
 */

import { useCallback, useEffect, useReducer, useRef, useState } from 'react';

import {
  initialStreamState,
  isStreamTopic,
  reconnectDelayMs,
  streamHealth,
  streamReducer,
  type StreamHealth,
  type StreamState,
} from '@/lib/sse-reducer';
import type { StreamTopic } from '@/lib/types';

export interface UseStreamOptions {
  /** Topics to subscribe to. Empty means every topic. */
  topics?: readonly StreamTopic[];
  /** Set false to leave the stream closed (e.g. mock mode, or a static page). */
  enabled?: boolean;
  /**
   * Called when the stream reports a gap. Must refetch the page's data over
   * REST and resolve once it has. Rejecting leaves the gap flagged, which is
   * the correct outcome: the UI keeps saying it may be missing events.
   */
  onResync?: () => Promise<void> | void;
}

export interface UseStreamResult {
  state: StreamState;
  health: StreamHealth;
  /** Force a reconnect — used by the "Reconnect" control on the status pill. */
  reconnect: () => void;
}

export function useStream(options: UseStreamOptions = {}): UseStreamResult {
  const { topics, enabled = true, onResync } = options;
  const [state, dispatch] = useReducer(streamReducer, undefined, () =>
    initialStreamState(),
  );
  // A tick that re-renders the status pill so "silent for 42s" stays truthful
  // without any event arriving.
  const [, setTick] = useState(0);
  const [generation, setGeneration] = useState(0);

  const sourceRef = useRef<EventSource | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const attemptsRef = useRef(0);
  const resyncingRef = useRef(false);
  const onResyncRef = useRef(onResync);
  onResyncRef.current = onResync;

  const topicsKey = topics && topics.length > 0 ? [...topics].sort().join(',') : '';

  const reconnect = useCallback(() => {
    setGeneration((g) => g + 1);
  }, []);

  useEffect(() => {
    if (!enabled) {
      dispatch({ type: 'closed' });
      return;
    }
    if (typeof window === 'undefined' || typeof EventSource === 'undefined') return;

    let cancelled = false;

    const open = () => {
      if (cancelled) return;
      dispatch({ type: 'connecting' });

      // Same-origin, no credentials in the URL: the token is attached by the
      // proxy route on the server. `Last-Event-ID` is set by the browser
      // itself on its automatic reconnects, and forwarded upstream by the proxy.
      const url = topicsKey
        ? `/api/stream?topics=${encodeURIComponent(topicsKey)}`
        : '/api/stream';
      const es = new EventSource(url);
      sourceRef.current = es;

      es.onopen = () => {
        attemptsRef.current = 0;
        dispatch({ type: 'open', at: Date.now() });
      };

      es.onerror = () => {
        // `EventSource` reconnects on its own using the server's `retry:` hint,
        // so this is a status report, not a reconnect trigger — except when the
        // connection is CLOSED, which means it gave up.
        if (es.readyState === EventSource.CLOSED) {
          attemptsRef.current += 1;
          dispatch({
            type: 'error',
            message: 'the connection was closed by the browser',
            at: Date.now(),
          });
          es.close();
          if (!cancelled) {
            const delay = reconnectDelayMs(attemptsRef.current);
            timerRef.current = setTimeout(open, delay);
          }
        } else {
          dispatch({
            type: 'error',
            message: 'the connection dropped',
            at: Date.now(),
          });
        }
      };

      const onTopic = (topic: StreamTopic) => (raw: MessageEvent<string>) => {
        let data: unknown = null;
        try {
          data = raw.data ? JSON.parse(raw.data) : null;
        } catch {
          // A frame we cannot parse is still evidence of liveness, but its
          // payload is not usable. Record the heartbeat, drop the payload.
          dispatch({ type: 'heartbeat', at: Date.now() });
          return;
        }
        dispatch({
          type: 'event',
          topic,
          id: raw.lastEventId,
          data,
          at: Date.now(),
        });
      };

      for (const topic of [
        'quote',
        'regime',
        'signal',
        'signal_invalidated',
        'alert',
        'prediction',
        'portfolio',
        'health',
      ] as const) {
        es.addEventListener(topic, onTopic(topic) as EventListener);
      }

      // Anything unnamed arrives as `message`. The hub always names its frames,
      // so this only fires for a proxy or middlebox that rewrote one.
      es.onmessage = (raw: MessageEvent<string>) => {
        const t = (raw as unknown as { type?: string }).type ?? '';
        if (isStreamTopic(t)) {
          onTopic(t)(raw);
          return;
        }
        dispatch({ type: 'heartbeat', at: Date.now() });
      };

      const control =
        (kind: 'resync' | 'overflow') => (raw: MessageEvent<string>) => {
          let reason = '';
          try {
            const parsed = raw.data ? (JSON.parse(raw.data) as { reason?: string }) : {};
            reason = typeof parsed.reason === 'string' ? parsed.reason : '';
          } catch {
            reason = '';
          }
          dispatch({ type: kind, reason, at: Date.now() });
        };

      es.addEventListener('resync', control('resync') as EventListener);
      es.addEventListener('overflow', control('overflow') as EventListener);
    };

    open();

    return () => {
      cancelled = true;
      if (timerRef.current) clearTimeout(timerRef.current);
      sourceRef.current?.close();
      sourceRef.current = null;
    };
  }, [enabled, topicsKey, generation]);

  // Honour the gap: refetch over REST, and only then clear the flag.
  useEffect(() => {
    if (!state.needsResync || resyncingRef.current) return;
    const handler = onResyncRef.current;
    if (!handler) return;
    resyncingRef.current = true;
    void (async () => {
      try {
        await handler();
        dispatch({ type: 'resynced', at: Date.now() });
      } catch {
        // Leave `needsResync` set. The UI keeps saying it may be missing
        // events, which is the honest state until a refetch succeeds.
      } finally {
        resyncingRef.current = false;
      }
    })();
  }, [state.needsResync, state.resyncCount]);

  // Keep the "silent for Ns" reading honest between frames.
  useEffect(() => {
    if (!enabled) return;
    const id = setInterval(() => setTick((t) => t + 1), 5000);
    return () => clearInterval(id);
  }, [enabled]);

  return { state, health: streamHealth(state), reconnect };
}

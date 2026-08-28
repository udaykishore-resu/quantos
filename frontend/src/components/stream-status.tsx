'use client';

/**
 * The live-stream status pill.
 *
 * This is the dashboard's honesty indicator. If the stream is disconnected,
 * stale, or has a gap the server told us about, the reader must be able to see
 * that at a glance from anywhere in the app — otherwise a page that stopped
 * updating an hour ago looks exactly like a market that stopped moving.
 *
 * `router.refresh()` is what closes a resync gap: it re-runs the server
 * components for the current route, which refetches everything over REST. That
 * is precisely the action `resync` asks for.
 */

import { useRouter } from 'next/navigation';
import { useCallback } from 'react';

import { useStream } from '@/hooks/use-stream';

export function StreamStatus({ enabled = true }: { enabled?: boolean }) {
  const router = useRouter();

  const onResync = useCallback(async () => {
    router.refresh();
    // `router.refresh()` is fire-and-forget. Give the server render a moment to
    // land before declaring the gap closed, so the indicator does not flash
    // green over data that has not arrived yet.
    await new Promise((resolve) => setTimeout(resolve, 750));
  }, [router]);

  const { state, health, reconnect } = useStream({ enabled, onResync });

  const tone = health.healthy
    ? 'border-allow bg-teal-50 text-allow'
    : state.needsResync || state.status === 'closed'
      ? 'border-deny bg-red-50 text-deny'
      : 'border-watch bg-amber-50 text-watch';

  const glyph = health.healthy
    ? '●'
    : state.status === 'connecting' || state.status === 'reconnecting'
      ? '◐'
      : '○';

  return (
    <div className="flex flex-wrap items-center gap-2">
      <span
        className={`inline-flex items-center gap-1.5 rounded border px-2 py-1 text-xs font-semibold ${tone}`}
        role="status"
        aria-live="polite"
      >
        <span aria-hidden="true" className="font-mono">
          {glyph}
        </span>
        {health.label}
        <span className="sr-only">. {health.description}</span>
      </span>

      {state.eventCount > 0 ? (
        <span className="num text-[11px] text-ink-500">
          {state.eventCount} event{state.eventCount === 1 ? '' : 's'}
          {state.lastEventId > 0 ? ` · id ${state.lastEventId}` : ''}
        </span>
      ) : null}

      {state.resyncCount > 0 || state.overflowCount > 0 ? (
        <span className="text-[11px] text-ink-600">
          {state.resyncCount > 0 ? `${state.resyncCount} resync` : ''}
          {state.resyncCount > 0 && state.overflowCount > 0 ? ' · ' : ''}
          {state.overflowCount > 0 ? `${state.overflowCount} overflow` : ''}
        </span>
      ) : null}

      {!health.healthy && !state.needsResync ? (
        <button type="button" onClick={reconnect} className="btn no-print px-2 py-1 text-xs">
          Reconnect
        </button>
      ) : null}

      {!health.healthy ? (
        <p className="w-full max-w-prose text-[11px] leading-snug text-ink-600">
          {health.description}
        </p>
      ) : null}
    </div>
  );
}

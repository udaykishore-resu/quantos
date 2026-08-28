/**
 * The live-stream state machine.
 *
 * This file is deliberately pure: it holds no `EventSource`, no timers and no
 * React. All of the reasoning about the QuantOS stream contract lives here so it
 * can be tested exhaustively without a socket, and the hook in
 * `hooks/use-stream.ts` is left with nothing but plumbing.
 *
 * The contract it implements is `internal/httpx/sse.go`:
 *
 *  - Every real event carries `id:` (a monotonically increasing `uint64`),
 *    `event:` (one of the eight topic names) and a JSON `data:` payload. The
 *    highest id seen is what the browser sends back as `Last-Event-ID`, and it
 *    is what the hub replays from.
 *
 *  - On reconnect the hub replays everything after `Last-Event-ID` that its
 *    bounded ring buffer still holds. If the client fell outside that window the
 *    hub first emits a `resync` event. `resync` means: **the stream has a gap
 *    you cannot fill; discard your incremental state and refetch over REST.**
 *    Treating it as informational is the specific bug this reducer exists to
 *    prevent — the UI would keep rendering a state that silently missed events.
 *
 *  - `overflow` means this connection could not keep up and the server closed
 *    it. The next connection is a fresh one, and because events were dropped
 *    while the buffer was full it must also be treated as a gap.
 *
 *  - Neither control event carries an `id:` field, so neither advances
 *    `Last-Event-ID`. The reducer must not let them.
 *
 *  - Heartbeats arrive as SSE comment frames (`: heartbeat <unix>`). The browser
 *    never surfaces them as events, so liveness is tracked from the timestamp of
 *    the last thing received of any kind.
 */

import type { StreamTopic } from './types';
import { STREAM_TOPICS } from './types';

export type ConnectionStatus =
  | 'idle'
  | 'connecting'
  | 'open'
  | 'reconnecting'
  | 'closed';

export interface StreamEvent {
  /** The SSE `id:` field. 0 for control events, which carry no id. */
  id: number;
  topic: StreamTopic;
  data: unknown;
  /** When the browser received it. */
  receivedAt: number;
}

export interface StreamState {
  status: ConnectionStatus;
  /** Highest event id accepted. Sent as `Last-Event-ID` on reconnect. */
  lastEventId: number;
  /** Most recent payload per topic — what the UI actually renders from. */
  latest: Partial<Record<StreamTopic, StreamEvent>>;
  /** Bounded ring of recent events, newest first. */
  recent: StreamEvent[];
  /**
   * True when the server told us the incremental state is untrustworthy
   * (`resync` or `overflow`). The UI must refetch over REST and must say so
   * until it has, rather than rendering possibly-stale state as if it were live.
   */
  needsResync: boolean;
  /** Why a resync is required, verbatim from the server where it supplied one. */
  resyncReason: string;
  /** Count of resyncs this session — surfaced so a flapping stream is visible. */
  resyncCount: number;
  /** Count of overflow disconnects — a slow-consumer signal worth showing. */
  overflowCount: number;
  /** Timestamp of the last frame of any kind, including heartbeats. */
  lastMessageAt: number | null;
  /** Consecutive failed connection attempts; drives reconnect backoff. */
  reconnectAttempts: number;
  /** Last transport error, if the stream is not currently open. */
  error: string | null;
  /** Total events accepted this session. */
  eventCount: number;
}

export const MAX_RECENT_EVENTS = 200;

export function initialStreamState(
  lastEventId = 0,
): StreamState {
  return {
    status: 'idle',
    lastEventId,
    latest: {},
    recent: [],
    needsResync: false,
    resyncReason: '',
    resyncCount: 0,
    overflowCount: 0,
    lastMessageAt: null,
    reconnectAttempts: 0,
    error: null,
    eventCount: 0,
  };
}

export type StreamAction =
  | { type: 'connecting' }
  | { type: 'open'; at: number }
  | {
      type: 'event';
      topic: StreamTopic;
      /** The raw SSE `lastEventId`; may be empty for control frames. */
      id: string;
      data: unknown;
      at: number;
    }
  | { type: 'resync'; reason: string; at: number }
  | { type: 'overflow'; reason: string; at: number }
  | { type: 'heartbeat'; at: number }
  | { type: 'error'; message: string; at: number }
  | { type: 'resynced'; at: number }
  | { type: 'closed' };

export function isStreamTopic(v: string): v is StreamTopic {
  return (STREAM_TOPICS as readonly string[]).includes(v);
}

/**
 * Parses an SSE id field. Ids are `uint64` on the wire; anything that is not a
 * non-negative integer is treated as absent rather than coerced, so a malformed
 * frame can never move `Last-Event-ID` backwards or to NaN.
 */
export function parseEventId(raw: string | null | undefined): number {
  if (!raw) return 0;
  if (!/^\d+$/.test(raw.trim())) return 0;
  const n = Number(raw.trim());
  return Number.isSafeInteger(n) && n >= 0 ? n : 0;
}

export function streamReducer(
  state: StreamState,
  action: StreamAction,
): StreamState {
  switch (action.type) {
    case 'connecting':
      return {
        ...state,
        status: state.reconnectAttempts > 0 ? 'reconnecting' : 'connecting',
      };

    case 'open':
      return {
        ...state,
        status: 'open',
        error: null,
        reconnectAttempts: 0,
        lastMessageAt: action.at,
      };

    case 'event': {
      const id = parseEventId(action.id);
      const ev: StreamEvent = {
        id,
        topic: action.topic,
        data: action.data,
        receivedAt: action.at,
      };
      // Ids only ever move forward. A replayed event the client already has
      // (the hub replays inclusively from its own bookkeeping, and a browser
      // may reconnect before the server registered the last delivery) is
      // recorded but must not rewind Last-Event-ID.
      const lastEventId = id > state.lastEventId ? id : state.lastEventId;
      return {
        ...state,
        status: 'open',
        lastEventId,
        latest: { ...state.latest, [action.topic]: ev },
        recent: [ev, ...state.recent].slice(0, MAX_RECENT_EVENTS),
        lastMessageAt: action.at,
        eventCount: state.eventCount + 1,
        error: null,
      };
    }

    case 'resync':
      // A gap. The incremental state is not trustworthy and the UI must refetch.
      // Note that `lastEventId` is deliberately preserved: the events the hub
      // *can* still replay follow the resync frame, and dropping the id would
      // ask for a replay of the entire buffer on the next reconnect.
      return {
        ...state,
        needsResync: true,
        resyncReason:
          action.reason ||
          'the replay buffer no longer covers your last event id; refetch state over REST',
        resyncCount: state.resyncCount + 1,
        lastMessageAt: action.at,
      };

    case 'overflow':
      // The server closed us for being slow, having already dropped events.
      // That is a gap too, so it sets the same flag as `resync`.
      return {
        ...state,
        status: 'reconnecting',
        needsResync: true,
        resyncReason:
          action.reason ||
          'this connection could not keep up and was closed; reconnect to resume',
        overflowCount: state.overflowCount + 1,
        lastMessageAt: action.at,
      };

    case 'heartbeat':
      return { ...state, lastMessageAt: action.at };

    case 'error':
      return {
        ...state,
        status: 'reconnecting',
        error: action.message,
        reconnectAttempts: state.reconnectAttempts + 1,
      };

    case 'resynced':
      // The UI has refetched over REST. Only now is the gap closed.
      return {
        ...state,
        needsResync: false,
        resyncReason: '',
        lastMessageAt: action.at,
      };

    case 'closed':
      return { ...state, status: 'closed' };

    default:
      return state;
  }
}

// --- Derived views ----------------------------------------------------------

/**
 * How long the browser may go without hearing anything before the stream is
 * presented as stale. The server heartbeats every 20s, so 60s is three missed
 * beats — long enough not to flap, short enough that a dead stream is visible.
 */
export const STALE_STREAM_MS = 60_000;

export interface StreamHealth {
  /** Live, receiving, and not missing events. */
  healthy: boolean;
  /** Connected but nothing has arrived for longer than the heartbeat allows. */
  stale: boolean;
  /** A short phrase for a status pill. */
  label: string;
  /** A full sentence explaining the state, for the accessible description. */
  description: string;
  /** ms since the last frame, or null if nothing has ever arrived. */
  silentForMs: number | null;
}

export function streamHealth(
  state: StreamState,
  now: number = Date.now(),
): StreamHealth {
  const silentForMs =
    state.lastMessageAt === null ? null : Math.max(0, now - state.lastMessageAt);
  const stale =
    state.status === 'open' &&
    silentForMs !== null &&
    silentForMs > STALE_STREAM_MS;

  if (state.needsResync) {
    return {
      healthy: false,
      stale,
      label: 'Resyncing',
      description: `Live updates have a gap: ${state.resyncReason} Showing data refetched over the REST API.`,
      silentForMs,
    };
  }
  switch (state.status) {
    case 'open':
      if (stale) {
        return {
          healthy: false,
          stale: true,
          label: 'Stale',
          description: `The stream is connected but nothing has arrived for ${Math.round(
            (silentForMs ?? 0) / 1000,
          )}s, past the ${STALE_STREAM_MS / 1000}s heartbeat window. Values on this page may be out of date.`,
          silentForMs,
        };
      }
      return {
        healthy: true,
        stale: false,
        label: 'Live',
        description: 'Connected to the QuantOS event stream and receiving updates.',
        silentForMs,
      };
    case 'connecting':
      return {
        healthy: false,
        stale: false,
        label: 'Connecting',
        description: 'Opening the QuantOS event stream.',
        silentForMs,
      };
    case 'reconnecting':
      return {
        healthy: false,
        stale: false,
        label: 'Reconnecting',
        description: state.error
          ? `The event stream dropped (${state.error}) and is being reopened. Values on this page are from the last successful load.`
          : 'The event stream dropped and is being reopened. Values on this page are from the last successful load.',
        silentForMs,
      };
    case 'closed':
      return {
        healthy: false,
        stale: false,
        label: 'Disconnected',
        description:
          'Live updates are off. Values on this page are from the last successful load and will not change.',
        silentForMs,
      };
    default:
      return {
        healthy: false,
        stale: false,
        label: 'Not connected',
        description: 'The event stream has not been opened.',
        silentForMs,
      };
  }
}

/**
 * Reconnect delay with exponential backoff and full jitter.
 *
 * The server sends `retry: 3000`, which a native `EventSource` honours on its
 * own. This is for the manual reconnect path, and it backs off so that a server
 * restart is not met with a reconnect storm from every open dashboard.
 */
export function reconnectDelayMs(
  attempts: number,
  baseMs = 3000,
  maxMs = 30_000,
  random: () => number = Math.random,
): number {
  const capped = Math.min(maxMs, baseMs * 2 ** Math.max(0, attempts - 1));
  return Math.round(capped / 2 + random() * (capped / 2));
}

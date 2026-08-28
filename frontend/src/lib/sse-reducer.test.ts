import { describe, expect, it } from 'vitest';

import {
  MAX_RECENT_EVENTS,
  STALE_STREAM_MS,
  initialStreamState,
  isStreamTopic,
  parseEventId,
  reconnectDelayMs,
  streamHealth,
  streamReducer,
  type StreamAction,
  type StreamState,
} from './sse-reducer';

const AT = 1_800_000_000_000;

function run(actions: StreamAction[], from = initialStreamState()): StreamState {
  return actions.reduce(streamReducer, from);
}

function ev(id: string, topic: Parameters<typeof isStreamTopic>[0] = 'quote', at = AT): StreamAction {
  return { type: 'event', topic: topic as never, id, data: { p: 1 }, at };
}

describe('event id parsing', () => {
  it('accepts a uint64-shaped id', () => {
    expect(parseEventId('1')).toBe(1);
    expect(parseEventId('  42 ')).toBe(42);
  });

  it('treats anything malformed as absent rather than coercing it', () => {
    // A malformed frame must never move Last-Event-ID backwards or to NaN.
    for (const bad of ['', null, undefined, 'abc', '-1', '1.5', '0x10', '1e3']) {
      expect(parseEventId(bad)).toBe(0);
    }
  });

  it('rejects an id beyond safe integer range', () => {
    expect(parseEventId('99999999999999999999')).toBe(0);
  });
});

describe('topic vocabulary', () => {
  it('recognises exactly the hub’s eight topics', () => {
    for (const t of [
      'quote',
      'regime',
      'signal',
      'signal_invalidated',
      'alert',
      'prediction',
      'portfolio',
      'health',
    ]) {
      expect(isStreamTopic(t)).toBe(true);
    }
    expect(isStreamTopic('message')).toBe(false);
    expect(isStreamTopic('resync')).toBe(false);
    expect(isStreamTopic('overflow')).toBe(false);
  });
});

describe('event accumulation', () => {
  it('tracks the highest id as Last-Event-ID', () => {
    const s = run([{ type: 'open', at: AT }, ev('1'), ev('2'), ev('3')]);
    expect(s.lastEventId).toBe(3);
    expect(s.eventCount).toBe(3);
    expect(s.status).toBe('open');
  });

  it('never rewinds Last-Event-ID on a replayed or out-of-order event', () => {
    // The hub replays inclusively from its own bookkeeping, and a browser can
    // reconnect before the server registered the last delivery. Rewinding
    // would ask for the same window again forever.
    const s = run([ev('5'), ev('3'), ev('4')]);
    expect(s.lastEventId).toBe(5);
    // The replayed events are still recorded — they are real data.
    expect(s.eventCount).toBe(3);
  });

  it('keeps the latest payload per topic', () => {
    const s = run([
      ev('1', 'quote'),
      ev('2', 'regime'),
      { type: 'event', topic: 'quote', id: '3', data: { p: 99 }, at: AT },
    ]);
    expect(s.latest.quote?.id).toBe(3);
    expect(s.latest.quote?.data).toEqual({ p: 99 });
    expect(s.latest.regime?.id).toBe(2);
  });

  it('bounds the recent-event ring', () => {
    const actions = Array.from({ length: MAX_RECENT_EVENTS + 50 }, (_, i) =>
      ev(String(i + 1)),
    );
    const s = run(actions);
    expect(s.recent.length).toBe(MAX_RECENT_EVENTS);
    // Newest first.
    expect(s.recent[0]?.id).toBe(MAX_RECENT_EVENTS + 50);
  });
});

describe('resync — the gap the server tells us about', () => {
  it('raises needsResync and records the server’s reason', () => {
    const s = run([
      { type: 'open', at: AT },
      ev('10'),
      { type: 'resync', reason: 'the replay buffer no longer covers your last event id', at: AT },
    ]);
    expect(s.needsResync).toBe(true);
    expect(s.resyncReason).toContain('replay buffer');
    expect(s.resyncCount).toBe(1);
  });

  it('supplies the contract wording when the server sends none', () => {
    const s = run([{ type: 'resync', reason: '', at: AT }]);
    expect(s.resyncReason).toContain('refetch state over REST');
  });

  it('does not advance Last-Event-ID — control frames carry no id', () => {
    const s = run([ev('10'), { type: 'resync', reason: 'gap', at: AT }]);
    expect(s.lastEventId).toBe(10);
  });

  it('preserves Last-Event-ID across the gap', () => {
    // The events the hub *can* still replay follow the resync frame. Zeroing
    // the id here would request a replay of the entire buffer on reconnect.
    const s = run([
      ev('10'),
      { type: 'resync', reason: 'gap', at: AT },
      ev('40'),
      ev('41'),
    ]);
    expect(s.lastEventId).toBe(41);
    // Still flagged: receiving newer events does not fill the gap.
    expect(s.needsResync).toBe(true);
  });

  it('stays flagged until the UI confirms it refetched', () => {
    let s = run([ev('10'), { type: 'resync', reason: 'gap', at: AT }, ev('40')]);
    expect(s.needsResync).toBe(true);
    s = streamReducer(s, { type: 'resynced', at: AT + 1 });
    expect(s.needsResync).toBe(false);
    expect(s.resyncReason).toBe('');
  });

  it('counts repeated resyncs so a flapping stream is visible', () => {
    const s = run([
      { type: 'resync', reason: 'a', at: AT },
      { type: 'resynced', at: AT },
      { type: 'resync', reason: 'b', at: AT },
    ]);
    expect(s.resyncCount).toBe(2);
    expect(s.needsResync).toBe(true);
  });
});

describe('overflow — dropped by the server for being slow', () => {
  it('is treated as a gap, exactly like resync', () => {
    // Events were dropped while the buffer was full, so the incremental state
    // is just as untrustworthy as after a resync.
    const s = run([
      { type: 'open', at: AT },
      ev('7'),
      { type: 'overflow', reason: 'this connection could not keep up', at: AT },
    ]);
    expect(s.needsResync).toBe(true);
    expect(s.overflowCount).toBe(1);
    expect(s.status).toBe('reconnecting');
    expect(s.lastEventId).toBe(7);
  });

  it('supplies the contract wording when the server sends none', () => {
    const s = run([{ type: 'overflow', reason: '', at: AT }]);
    expect(s.resyncReason).toContain('could not keep up');
  });
});

describe('connection lifecycle', () => {
  it('distinguishes a first connection from a reconnection', () => {
    let s = streamReducer(initialStreamState(), { type: 'connecting' });
    expect(s.status).toBe('connecting');
    s = streamReducer(s, { type: 'error', message: 'dropped', at: AT });
    expect(s.status).toBe('reconnecting');
    expect(s.reconnectAttempts).toBe(1);
    s = streamReducer(s, { type: 'connecting' });
    expect(s.status).toBe('reconnecting');
  });

  it('clears the error and attempt count once open', () => {
    const s = run([
      { type: 'error', message: 'dropped', at: AT },
      { type: 'error', message: 'dropped', at: AT },
      { type: 'open', at: AT },
    ]);
    expect(s.status).toBe('open');
    expect(s.error).toBeNull();
    expect(s.reconnectAttempts).toBe(0);
  });

  it('records a heartbeat as liveness without counting it as an event', () => {
    const s = run([{ type: 'open', at: AT }, { type: 'heartbeat', at: AT + 5000 }]);
    expect(s.lastMessageAt).toBe(AT + 5000);
    expect(s.eventCount).toBe(0);
  });
});

describe('derived health', () => {
  it('reports a receiving stream as healthy', () => {
    const s = run([{ type: 'open', at: AT }, ev('1')]);
    const h = streamHealth(s, AT + 1000);
    expect(h.healthy).toBe(true);
    expect(h.label).toBe('Live');
  });

  it('reports staleness past the heartbeat window', () => {
    // The server heartbeats every 20s; three missed beats is not a blip.
    const s = run([{ type: 'open', at: AT }]);
    const h = streamHealth(s, AT + STALE_STREAM_MS + 1000);
    expect(h.healthy).toBe(false);
    expect(h.stale).toBe(true);
    expect(h.label).toBe('Stale');
    expect(h.description).toContain('may be out of date');
  });

  it('does not report staleness inside the heartbeat window', () => {
    const s = run([{ type: 'open', at: AT }]);
    expect(streamHealth(s, AT + 30_000).healthy).toBe(true);
  });

  it('reports a gap as unhealthy even while frames keep arriving', () => {
    // The specific bug this guards: a stream that is connected and busy, but
    // missing events, must not read as healthy.
    const s = run([
      { type: 'open', at: AT },
      { type: 'resync', reason: 'buffer gap', at: AT },
      ev('99', 'quote', AT + 500),
    ]);
    const h = streamHealth(s, AT + 1000);
    expect(h.healthy).toBe(false);
    expect(h.label).toBe('Resyncing');
    expect(h.description).toContain('refetched over the REST API');
  });

  it('says values will not change when disconnected', () => {
    const h = streamHealth(streamReducer(initialStreamState(), { type: 'closed' }), AT);
    expect(h.label).toBe('Disconnected');
    expect(h.description).toContain('will not change');
  });

  it('reports silence duration, or null when nothing ever arrived', () => {
    expect(streamHealth(initialStreamState(), AT).silentForMs).toBeNull();
    const s = run([{ type: 'open', at: AT }]);
    expect(streamHealth(s, AT + 4000).silentForMs).toBe(4000);
  });
});

describe('reconnect backoff', () => {
  it('grows exponentially and is capped', () => {
    const noJitter = () => 1;
    expect(reconnectDelayMs(1, 3000, 30_000, noJitter)).toBe(3000);
    expect(reconnectDelayMs(2, 3000, 30_000, noJitter)).toBe(6000);
    expect(reconnectDelayMs(3, 3000, 30_000, noJitter)).toBe(12_000);
    expect(reconnectDelayMs(99, 3000, 30_000, noJitter)).toBe(30_000);
  });

  it('applies full jitter so reconnects do not synchronise', () => {
    // Every open dashboard reconnecting at the same instant after a server
    // restart is a self-inflicted thundering herd.
    const low = reconnectDelayMs(3, 3000, 30_000, () => 0);
    const high = reconnectDelayMs(3, 3000, 30_000, () => 1);
    expect(low).toBe(6000);
    expect(high).toBe(12_000);
    expect(low).toBeLessThan(high);
  });

  it('never returns a negative delay for attempt zero', () => {
    expect(reconnectDelayMs(0, 3000, 30_000, () => 0)).toBeGreaterThan(0);
  });
});

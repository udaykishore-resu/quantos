'use client';

/**
 * The approval surface for the execution desk.
 *
 * Three rules shape it:
 *
 *  1. The confirmation restates the order in words — side, quantity, ticker,
 *     the worst price, the most it can cost — next to the password field, so
 *     what is approved is what was read, not what a button was near.
 *  2. Nothing here is optimistic. The page shows what the desk answered: a
 *     blocked approval comes back with the guardrail that stopped it, and the
 *     lists refresh from the server rather than being edited locally.
 *  3. Halting is one click and needs no password; resuming needs it. Stopping
 *     must be easy and starting must not be.
 *
 * The password lives in component state only for the length of one request
 * and is cleared whatever the outcome.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';

import { Card, DefinitionList, Freshness, Metric } from '@/components/page';
import { EmptyState, ErrorState, LoadingState } from '@/components/states';
import { useApiPath } from '@/hooks/use-api';
import {
  formatDuration,
  formatGoDuration,
  formatMoney,
  formatNumber,
  formatPercent,
  formatPrice,
  formatSignedMoney,
  formatSignedPercent,
  formatTimestamp,
  humanizeEnum,
  isZeroTime,
  list,
} from '@/lib/format';
import type {
  ExecutionLimits,
  ExecutionStatus,
  HaltState,
  Ticket,
  TicketStatus,
  VenuePosition,
} from '@/lib/types';

const POLL_MS = 5_000;

// --- Talking to the action routes ---------------------------------------------

interface Outcome {
  ok: boolean;
  status: number;
  ticket?: Ticket;
  halt?: HaltState;
  message: string;
  detail?: string;
}

async function act(path: string, body: Record<string, unknown>): Promise<Outcome> {
  let res: Response;
  try {
    res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify(body),
      cache: 'no-store',
    });
  } catch (err) {
    return {
      ok: false,
      status: 0,
      message: 'The dashboard server could not be reached.',
      detail: err instanceof Error ? err.message : String(err),
    };
  }
  const json = (await res.json().catch(() => null)) as
    | { data?: unknown; error?: { message?: string; detail?: string } }
    | null;
  const data = json?.data;
  const ticket =
    data && typeof data === 'object' && 'status' in data && 'ticker' in data
      ? (data as Ticket)
      : undefined;
  const halt =
    data && typeof data === 'object' && 'halt' in data
      ? ((data as { halt: HaltState }).halt)
      : undefined;
  if (res.ok) return { ok: true, status: res.status, ticket, halt, message: 'Done.' };
  return {
    ok: false,
    status: res.status,
    ticket,
    message:
      json?.error?.message ??
      (ticket ? `The desk answered ${humanizeEnum(ticket.status)}.` : `HTTP ${res.status}`),
    detail: json?.error?.detail ?? ticket?.status_reason,
  };
}

// --- Small pieces -------------------------------------------------------------

function useNow(intervalMs = 1_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}

type Tone = 'allow' | 'watch' | 'deny' | 'flat';

const STATUS_TONE: Record<TicketStatus, [Tone, string]> = {
  PROPOSED: ['flat', '•'],
  APPROVED: ['watch', '◐'],
  SUBMITTED: ['watch', '◐'],
  PARTIALLY_FILLED: ['watch', '◐'],
  FILLED: ['allow', '✓'],
  REJECTED: ['flat', '–'],
  EXPIRED: ['flat', '–'],
  CANCELLED: ['flat', '–'],
  BLOCKED: ['deny', '✕'],
  VENUE_REJECTED: ['deny', '✕'],
  FAILED: ['deny', '✕'],
};

const TONE_CLASS: Record<Tone, string> = {
  allow: 'border-allow/40 bg-teal-50 text-allow',
  watch: 'border-watch/40 bg-amber-50 text-watch',
  deny: 'border-deny/40 bg-red-50 text-deny',
  flat: 'border-ink-300 bg-ink-50 text-ink-700',
};

function StatusPill({ status }: { status: TicketStatus }) {
  const [tone, glyph] = STATUS_TONE[status] ?? ['flat', '•'];
  return (
    <span
      className={`inline-flex items-center gap-1 whitespace-nowrap rounded border px-1.5 py-0.5 text-[11px] font-semibold ${TONE_CLASS[tone]}`}
    >
      <span aria-hidden="true">{glyph}</span>
      {humanizeEnum(status)}
    </span>
  );
}

function SideLabel({ side }: { side: Ticket['side'] }) {
  return (
    <span className={`font-bold ${side === 'BUY' ? 'text-up' : 'text-down'}`}>
      <span aria-hidden="true">{side === 'BUY' ? '▲ ' : '▼ '}</span>
      {side === 'BUY' ? 'Buy' : 'Sell'}
    </span>
  );
}

function orderInWords(t: Ticket): string {
  const verb = t.side === 'BUY' ? 'Buy' : 'Sell';
  const bound = t.side === 'BUY' ? 'no more than' : 'no less than';
  return `${verb} ${formatNumber(t.quantity, t.quantity % 1 === 0 ? 0 : 4)} ${t.ticker} at ${bound} ${formatPrice(t.limit_price)} per share`;
}

function Checks({ ticket }: { ticket: Ticket }) {
  const checks = list(ticket.checks);
  if (checks.length === 0) return null;
  return (
    <ul className="mt-2 space-y-1 text-xs">
      {checks.map((c) => (
        <li key={c.name} className="flex gap-2">
          <span
            aria-hidden="true"
            className={`w-3 shrink-0 font-bold ${c.passed ? 'text-allow' : 'text-deny'}`}
          >
            {c.passed ? '✓' : '✕'}
          </span>
          <span>
            <span className="font-semibold">{humanizeEnum(c.name)}</span>
            <span className="sr-only">{c.passed ? ' passed' : ' failed'}</span>
            {c.detail ? <span className="text-ink-600"> — {c.detail}</span> : null}
          </span>
        </li>
      ))}
    </ul>
  );
}

function OutcomeNote({ outcome }: { outcome: Outcome }) {
  const t = outcome.ticket;
  const good = outcome.ok && (!t || !['BLOCKED', 'FAILED', 'VENUE_REJECTED'].includes(t.status));
  return (
    <div
      role={good ? 'status' : 'alert'}
      className={`mt-3 rounded-md border px-3 py-2 text-sm ${good ? TONE_CLASS.allow : TONE_CLASS.deny}`}
    >
      <p className="font-semibold">
        <span aria-hidden="true">{good ? '✓ ' : '✕ '}</span>
        {t ? (
          <>
            {orderInWords(t)}: <StatusPill status={t.status} />
          </>
        ) : (
          outcome.message
        )}
      </p>
      {outcome.detail ? <p className="mt-1 text-ink-700">{outcome.detail}</p> : null}
      {t ? <Checks ticket={t} /> : null}
    </div>
  );
}

// --- One proposal awaiting a decision -------------------------------------------

function ProposalCard({
  ticket,
  limits,
  locked,
  lockedReason,
  now,
  onChanged,
  onDecided,
}: {
  ticket: Ticket;
  limits: ExecutionLimits | null;
  locked: boolean;
  lockedReason: string;
  now: number;
  onChanged: () => void;
  /** A decided ticket leaves this list on the next poll, taking this card
   *  with it, so its outcome is handed up to outlive the card. */
  onDecided: (o: Outcome) => void;
}) {
  const [mode, setMode] = useState<'idle' | 'approve' | 'reject'>('idle');
  const [password, setPassword] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | null>(null);

  const msLeft = Date.parse(ticket.expires_at) - now;
  const expired = Number.isFinite(msLeft) && msLeft <= 0;
  const notional = ticket.quantity * ticket.limit_price;
  const premiumBps =
    ticket.reference_price > 0
      ? ((ticket.limit_price - ticket.reference_price) / ticket.reference_price) * 10_000
      : NaN;
  const titleId = `ticket-${ticket.id}`;

  // Advance notice of the guardrails this page can evaluate from what it
  // already has. The desk re-checks all of them, plus the ones that need live
  // data (drift, position weight, daily loss), at approval; these only save
  // typing a password to learn something already knowable.
  const foreseen: string[] = [];
  if (limits && limits.MaxOrderNotional > 0 && notional > limits.MaxOrderNotional) {
    foreseen.push(
      `${formatMoney(notional)} is over the ${formatMoney(limits.MaxOrderNotional)} per-order cap`,
    );
  }
  const allow = list(limits?.Allowlist);
  if (limits && !allow.includes(ticket.ticker)) {
    foreseen.push(`${ticket.ticker} is not on the tradable list`);
  }

  const approve = async () => {
    setBusy(true);
    const pw = password;
    setPassword('');
    const o = await act(`/api/execution/tickets/${encodeURIComponent(ticket.id)}/approve`, { password: pw });
    setBusy(false);
    // A wrong password or a refused request leaves the ticket open, and the
    // answer belongs on this card. Anything carrying a ticket decided it.
    if (o.ticket) {
      setOutcome(null);
      setMode('idle');
      onDecided(o);
    } else {
      setOutcome(o);
    }
    onChanged();
  };

  const reject = async () => {
    setBusy(true);
    const o = await act(`/api/execution/tickets/${encodeURIComponent(ticket.id)}/reject`, { reason });
    setBusy(false);
    setMode('idle');
    if (o.ticket) {
      setOutcome(null);
      onDecided(o);
    } else {
      setOutcome(o);
    }
    onChanged();
  };

  return (
    <article aria-labelledby={titleId} className="rounded-lg border border-ink-200 bg-white p-4 shadow-sm">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 id={titleId} className="text-lg font-semibold text-ink-900">
          <SideLabel side={ticket.side} />{' '}
          <span className="num">{formatNumber(ticket.quantity, ticket.quantity % 1 === 0 ? 0 : 4)}</span>{' '}
          <span className="font-mono">{ticket.ticker}</span>
        </h3>
        <p className={`text-xs ${expired ? 'font-semibold text-deny' : msLeft < 120_000 ? 'font-semibold text-watch' : 'text-ink-500'}`}>
          {expired ? 'Expired — no longer approvable' : `Expires in ${formatDuration(msLeft)}`}
        </p>
      </div>

      <DefinitionList
        columns={4}
        items={[
          {
            term: 'Limit price',
            value: <span className="num">{formatPrice(ticket.limit_price)}</span>,
            hint: ticket.side === 'BUY' ? 'the most you pay per share' : 'the least you accept per share',
          },
          {
            term: 'Up to',
            value: <span className="num">{formatMoney(notional)}</span>,
            hint: limits ? `cap ${formatMoney(limits.MaxOrderNotional)} per order` : undefined,
          },
          {
            term: 'Price when proposed',
            value: <span className="num">{formatPrice(ticket.reference_price)}</span>,
            hint: Number.isFinite(premiumBps)
              ? premiumBps === 0
                ? 'limit equals it'
                : `limit is ${formatNumber(Math.abs(premiumBps), 1)} bps ${premiumBps > 0 ? 'above' : 'below'}`
              : undefined,
          },
          {
            term: 'Source',
            value:
              ticket.source === 'signal'
                ? `Signal${typeof ticket.confidence === 'number' && ticket.confidence > 0 ? ` · ${formatPercent(ticket.confidence, 0)} conf.` : ''}`
                : 'Manual',
            hint: ticket.source === 'signal' ? 'model-health gate applies' : 'you proposed this',
          },
        ]}
      />

      <div className="mt-3 rounded-md bg-ink-50 px-3 py-2">
        <p className="text-[11px] font-medium uppercase tracking-wide text-ink-500">Why</p>
        <p className="whitespace-pre-line text-sm leading-relaxed text-ink-800">{ticket.rationale}</p>
      </div>

      {foreseen.length > 0 && !expired ? (
        <p role="note" className={`mt-3 rounded-md border px-3 py-2 text-sm ${TONE_CLASS.watch}`}>
          <span aria-hidden="true">! </span>
          The desk will block this: {foreseen.join('; ')}. Reject it, or propose a smaller order.
        </p>
      ) : null}

      {mode === 'idle' ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <button
            type="button"
            className="btn btn-primary"
            disabled={locked || expired || busy}
            onClick={() => {
              setOutcome(null);
              setMode('approve');
            }}
          >
            Approve…
          </button>
          <button
            type="button"
            className="btn"
            disabled={expired || busy}
            onClick={() => {
              setOutcome(null);
              setMode('reject');
            }}
          >
            Reject…
          </button>
          {locked ? <span className="text-xs text-ink-600">{lockedReason}</span> : null}
        </div>
      ) : null}

      {mode === 'approve' ? (
        <form
          className="mt-3 rounded-md border-2 border-ink-900 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            void approve();
          }}
        >
          <p className="text-sm font-semibold text-ink-900">{orderInWords(ticket)}.</p>
          <p className="mt-0.5 text-xs text-ink-600">
            Paper limit order, up to {formatMoney(notional)}. The desk re-checks the price, your
            position size and today&apos;s loss before sending; if any check fails, nothing is sent.
          </p>
          <label className="field-label mt-3" htmlFor={`pw-${ticket.id}`}>
            Your password
          </label>
          <div className="flex flex-wrap gap-2">
            <input
              id={`pw-${ticket.id}`}
              type="password"
              autoComplete="current-password"
              className="field-input max-w-xs"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              disabled={busy}
              required
              autoFocus
            />
            <button type="submit" className="btn btn-primary" disabled={busy || !password}>
              {busy ? 'Sending…' : 'Confirm approval'}
            </button>
            <button
              type="button"
              className="btn"
              disabled={busy}
              onClick={() => {
                setPassword('');
                setMode('idle');
              }}
            >
              Cancel
            </button>
          </div>
        </form>
      ) : null}

      {mode === 'reject' ? (
        <form
          className="mt-3 rounded-md border border-ink-300 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            void reject();
          }}
        >
          <label className="field-label" htmlFor={`why-${ticket.id}`}>
            Reason (optional, kept in the audit trail)
          </label>
          <div className="flex flex-wrap gap-2">
            <input
              id={`why-${ticket.id}`}
              className="field-input max-w-md"
              maxLength={280}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              disabled={busy}
              autoFocus
            />
            <button type="submit" className="btn" disabled={busy}>
              {busy ? 'Rejecting…' : 'Confirm rejection'}
            </button>
            <button type="button" className="btn" disabled={busy} onClick={() => setMode('idle')}>
              Cancel
            </button>
          </div>
        </form>
      ) : null}

      {outcome ? <OutcomeNote outcome={outcome} /> : null}
    </article>
  );
}

// --- The kill switch ----------------------------------------------------------

function HaltControl({ halt, onChanged }: { halt: HaltState; onChanged: () => void }) {
  const [confirming, setConfirming] = useState(false);
  const [reason, setReason] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | null>(null);

  if (halt.halted) {
    return (
      <div role="alert" className={`rounded-md border px-3 py-3 text-sm ${TONE_CLASS.deny}`}>
        <p className="font-semibold">
          <span aria-hidden="true">■ </span>Desk halted — no order will be sent
        </p>
        <p className="mt-1 text-ink-700">
          {halt.by ? `By ${halt.by}` : 'Halted'}
          {halt.at && !isZeroTime(halt.at) ? ` at ${formatTimestamp(halt.at)}` : ''}
          {halt.reason ? ` — ${halt.reason}` : ''}. Open orders were cancelled at the venue.
        </p>
        <form
          className="mt-3 flex flex-wrap items-end gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            const pw = password;
            setPassword('');
            const o = await act('/api/execution/resume', { password: pw });
            setOutcome(o.ok ? null : o);
            setBusy(false);
            onChanged();
          }}
        >
          <div>
            <label className="field-label" htmlFor="resume-pw">
              Password to resume
            </label>
            <input
              id="resume-pw"
              type="password"
              autoComplete="current-password"
              className="field-input w-56"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              disabled={busy}
              required
            />
          </div>
          <button type="submit" className="btn" disabled={busy || !password}>
            {busy ? 'Resuming…' : 'Resume desk'}
          </button>
        </form>
        {outcome ? <OutcomeNote outcome={outcome} /> : null}
      </div>
    );
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      {!confirming ? (
        <button
          type="button"
          className="btn border-deny text-deny hover:bg-red-50"
          onClick={() => setConfirming(true)}
        >
          <span aria-hidden="true">■</span> Halt all trading
        </button>
      ) : (
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            const o = await act('/api/execution/halt', { reason });
            setOutcome(o.ok ? null : o);
            setBusy(false);
            setConfirming(false);
            setReason('');
            onChanged();
          }}
        >
          <label className="sr-only" htmlFor="halt-reason">
            Reason for halting
          </label>
          <input
            id="halt-reason"
            className="field-input w-64"
            placeholder="Reason (optional)"
            maxLength={280}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            disabled={busy}
            autoFocus
          />
          <button type="submit" className="btn border-deny bg-deny text-white hover:bg-red-800" disabled={busy}>
            {busy ? 'Halting…' : 'Halt now'}
          </button>
          <button type="button" className="btn" disabled={busy} onClick={() => setConfirming(false)}>
            Cancel
          </button>
        </form>
      )}
      <span className="text-xs text-ink-500">Cancels open orders and refuses new ones. No password needed.</span>
      {outcome ? <OutcomeNote outcome={outcome} /> : null}
    </div>
  );
}

// --- Tables -------------------------------------------------------------------

function TicketTable({ tickets, empty }: { tickets: Ticket[]; empty: string }) {
  const [open, setOpen] = useState<string | null>(null);
  if (tickets.length === 0) return <EmptyState title={empty} />;
  return (
    <div className="table-wrap">
      <table className="data-table">
        <thead>
          <tr>
            <th className="text-left">Created</th>
            <th className="text-left">Order</th>
            <th className="text-right">Limit</th>
            <th className="text-left">Status</th>
            <th className="text-right">Filled</th>
            <th className="text-left">Decided by</th>
            <th className="text-left">
              <span className="sr-only">Details</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {tickets.map((t) => {
            const expanded = open === t.id;
            const o = t.order;
            return [
              <tr key={t.id}>
                <td className="num whitespace-nowrap text-xs">{formatTimestamp(t.created_at)}</td>
                <td className="whitespace-nowrap">
                  <SideLabel side={t.side} /> <span className="num">{formatNumber(t.quantity, t.quantity % 1 === 0 ? 0 : 4)}</span>{' '}
                  <span className="font-mono font-semibold">{t.ticker}</span>
                </td>
                <td className="num text-right">{formatPrice(t.limit_price)}</td>
                <td>
                  <StatusPill status={t.status} />
                </td>
                <td className="num whitespace-nowrap text-right text-xs">
                  {o && o.filled_qty > 0
                    ? `${formatNumber(o.filled_qty, o.filled_qty % 1 === 0 ? 0 : 4)} @ ${formatPrice(o.avg_fill_price)}`
                    : '—'}
                </td>
                <td className="text-xs">{t.decided_by || '—'}</td>
                <td>
                  <button
                    type="button"
                    className="link text-xs"
                    aria-expanded={expanded}
                    aria-controls={`detail-${t.id}`}
                    onClick={() => setOpen(expanded ? null : t.id)}
                  >
                    {expanded ? 'Hide' : 'Details'}
                  </button>
                </td>
              </tr>,
              expanded ? (
                <tr key={`${t.id}-detail`} id={`detail-${t.id}`}>
                  <td colSpan={7} className="bg-ink-50">
                    <div className="grid gap-4 py-1 md:grid-cols-2">
                      <div>
                        <p className="text-[11px] font-medium uppercase tracking-wide text-ink-500">Why</p>
                        <p className="text-sm text-ink-800">{t.rationale}</p>
                        {t.status_reason ? (
                          <p className="mt-2 text-sm">
                            <span className="font-semibold">Outcome:</span> {t.status_reason}
                          </p>
                        ) : null}
                        <Checks ticket={t} />
                        {o ? (
                          <p className="mt-2 text-xs text-ink-600">
                            Venue order <span className="font-mono">{o.id}</span> · {o.venue_status || humanizeEnum(o.status)}
                          </p>
                        ) : null}
                      </div>
                      <div>
                        <p className="text-[11px] font-medium uppercase tracking-wide text-ink-500">Audit trail</p>
                        <ol className="mt-1 space-y-1 text-xs">
                          {list(t.history).map((h, i) => (
                            <li key={`${h.at}-${i}`} className="flex flex-wrap gap-x-2">
                              <span className="num text-ink-500">{formatTimestamp(h.at)}</span>
                              <span className="font-semibold">{humanizeEnum(h.status)}</span>
                              <span className="text-ink-600">by {h.by || 'system'}</span>
                              {h.reason ? <span className="text-ink-600">— {h.reason}</span> : null}
                            </li>
                          ))}
                        </ol>
                      </div>
                    </div>
                  </td>
                </tr>
              ) : null,
            ];
          })}
        </tbody>
      </table>
    </div>
  );
}

function PositionsTable({ positions }: { positions: VenuePosition[] }) {
  if (positions.length === 0) {
    return <EmptyState title="The paper account holds no positions." />;
  }
  return (
    <div className="table-wrap">
      <table className="data-table">
        <thead>
          <tr>
            <th className="text-left">Ticker</th>
            <th className="text-right">Quantity</th>
            <th className="text-right">Avg price</th>
            <th className="text-right">Last</th>
            <th className="text-right">Value</th>
            <th className="text-right">Unrealized</th>
          </tr>
        </thead>
        <tbody>
          {positions.map((p) => (
            <tr key={p.ticker}>
              <td className="font-mono font-semibold">{p.ticker}</td>
              <td className="num text-right">{formatNumber(p.quantity, p.quantity % 1 === 0 ? 0 : 4)}</td>
              <td className="num text-right">{formatPrice(p.avg_price)}</td>
              <td className="num text-right">{formatPrice(p.market_price)}</td>
              <td className="num text-right">{formatMoney(p.market_value)}</td>
              <td className={`num text-right ${p.unrealized_pnl >= 0 ? 'text-up' : 'text-down'}`}>
                {formatSignedMoney(p.unrealized_pnl)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// --- The page body ------------------------------------------------------------

const WORKING: TicketStatus[] = ['APPROVED', 'SUBMITTED', 'PARTIALLY_FILLED'];

export function ExecutionDesk() {
  const status = useApiPath<ExecutionStatus>('execution/status', undefined, { pollMs: POLL_MS });
  const tickets = useApiPath<Ticket[] | null>('execution/tickets', { limit: 200 }, { pollMs: POLL_MS });
  const positions = useApiPath<VenuePosition[] | null>('execution/positions', undefined, {
    pollMs: POLL_MS * 3,
    enabled: status.data !== null,
  });
  const now = useNow();
  const [decisions, setDecisions] = useState<Outcome[]>([]);
  const onDecided = useCallback((o: Outcome) => setDecisions((d) => [o, ...d].slice(0, 5)), []);

  const refetchAll = useCallback(() => {
    void status.refetch();
    void tickets.refetch();
    void positions.refetch();
  }, [status, tickets, positions]);

  const { pending, working, decided } = useMemo(() => {
    const all = list(tickets.data);
    const byCreatedDesc = (a: Ticket, b: Ticket) => Date.parse(b.created_at) - Date.parse(a.created_at);
    return {
      pending: all
        .filter((t) => t.status === 'PROPOSED')
        .sort((a, b) => Date.parse(a.expires_at) - Date.parse(b.expires_at)),
      working: all.filter((t) => WORKING.includes(t.status)).sort(byCreatedDesc),
      decided: all
        .filter((t) => t.status !== 'PROPOSED' && !WORKING.includes(t.status))
        .sort(byCreatedDesc)
        .slice(0, 50),
    };
  }, [tickets.data]);

  if (status.loading) return <LoadingState label="Loading the execution desk" rows={4} />;

  if (!status.data) {
    const err = status.error;
    if (err && err.status === 503 && /desk is not running/i.test(err.detail ?? '')) {
      return (
        <EmptyState
          title="The execution desk is not running."
          detail="Start QuantOS with execution.enabled: true, a non-empty execution.allowlist, and your Alpaca PAPER keys in QUANTOS_ALPACA_KEY_ID and QUANTOS_ALPACA_SECRET_KEY. Proposals appear here once it is up."
        />
      );
    }
    return err ? (
      <ErrorState error={err} what="the execution desk" onRetry={() => void status.refetch()} />
    ) : (
      <EmptyState title="The desk returned no status." />
    );
  }

  const s = status.data;
  const acct = s.account;
  const dayPnl = acct ? acct.equity - acct.last_equity : NaN;
  const dayPct = acct && acct.last_equity > 0 ? dayPnl / acct.last_equity : NaN;

  // Approval is refused server-side in every one of these cases too; the page
  // says so up front instead of letting the owner type a password to find out.
  let lockedReason = '';
  if (!s.paper) lockedReason = 'The venue is not a paper account. Approvals are disabled.';
  else if (s.halt.halted) lockedReason = 'The desk is halted. Resume it to approve.';
  else if (acct?.blocked) lockedReason = `The venue is not accepting orders (${acct.status}).`;

  return (
    <div className="space-y-4">
      {!s.paper ? (
        <p role="alert" className="rounded border border-deny bg-red-50 px-3 py-2 text-sm font-semibold text-deny">
          <span aria-hidden="true">✕ </span>
          The desk reports a venue that is not a paper account. This contradicts its own start-up check;
          treat everything below as suspect and halt it.
        </p>
      ) : null}

      <Card
        title="Desk"
        subtitle={`${s.venue}${s.auto_propose ? ' · proposals from new signals are on' : ' · manual proposals only'}`}
        actions={<Freshness servedAt={status.servedAt ?? undefined} />}
      >
        <div className="space-y-4">
          {acct ? (
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <Metric label="Paper equity" value={formatMoney(acct.equity)} />
              <Metric label="Buying power" value={formatMoney(acct.buying_power)} />
              <Metric
                label="Today"
                value={formatSignedMoney(dayPnl)}
                sub={
                  Number.isFinite(dayPct) ? (
                    <span className={dayPnl >= 0 ? 'text-up' : 'text-down'}>
                      <span aria-hidden="true">{dayPnl >= 0 ? '▲ ' : '▼ '}</span>
                      {formatSignedPercent(dayPct)}
                    </span>
                  ) : undefined
                }
                hint={`New buys stop at −${formatPercent(s.limits.MaxDailyLossPct)} on the day.`}
              />
              <Metric label="Waiting / working" value={`${s.pending} / ${s.working}`} />
            </div>
          ) : (
            <p role="alert" className={`rounded-md border px-3 py-2 text-sm ${TONE_CLASS.watch}`}>
              <span aria-hidden="true">! </span>
              The venue account could not be read{s.account_error ? `: ${s.account_error}` : '.'} Approvals will
              fail their checks until it can.
            </p>
          )}
          <HaltControl halt={s.halt} onChanged={refetchAll} />
        </div>
      </Card>

      <Card
        title={`Waiting for you${pending.length ? ` (${pending.length})` : ''}`}
        subtitle="Soonest to expire first. An unanswered proposal expires; nothing is ever sent by default."
        actions={<Freshness servedAt={tickets.servedAt ?? undefined} />}
      >
        {decisions.length > 0 ? (
          <div aria-live="polite" className="mb-4">
            <div className="flex items-baseline justify-between">
              <h3 className="text-xs font-bold uppercase tracking-wide text-ink-600">Your decisions just now</h3>
              <button type="button" className="link text-xs" onClick={() => setDecisions([])}>
                Dismiss
              </button>
            </div>
            {decisions.map((o, i) => (
              <OutcomeNote key={`${o.ticket?.id ?? 'x'}-${i}`} outcome={o} />
            ))}
          </div>
        ) : null}
        {tickets.error && !tickets.data ? (
          <ErrorState error={tickets.error} what="the proposals" onRetry={() => void tickets.refetch()} />
        ) : pending.length === 0 ? (
          <EmptyState
            title="Nothing is waiting for approval."
            detail={
              s.auto_propose
                ? 'New signals that pass the model-health gate will appear here.'
                : 'Auto-propose is off. Propose an order with POST /api/v1/execution/tickets, or enable execution.auto_propose against a real market-data feed.'
            }
          />
        ) : (
          <div className="space-y-3">
            {pending.map((t) => (
              <ProposalCard
                key={t.id}
                ticket={t}
                limits={s.limits}
                locked={lockedReason !== ''}
                lockedReason={lockedReason}
                now={now}
                onChanged={refetchAll}
                onDecided={onDecided}
              />
            ))}
          </div>
        )}
      </Card>

      {working.length > 0 ? (
        <Card title="Working at the venue" subtitle="Approved and sent; the desk reconciles fills from the venue.">
          <TicketTable tickets={working} empty="No working orders." />
        </Card>
      ) : null}

      <Card title="Paper account positions" actions={<Freshness servedAt={positions.servedAt ?? undefined} />}>
        {positions.error && !positions.data ? (
          <ErrorState error={positions.error} what="the venue positions" />
        ) : positions.loading || (positions.data === null && !positions.error) ? (
          <LoadingState label="Loading positions" rows={2} />
        ) : (
          <PositionsTable positions={list(positions.data)} />
        )}
      </Card>

      <Card title="Decided" subtitle="The last 50 decisions, newest first, with the guardrail results recorded at approval.">
        <TicketTable tickets={decided} empty="No decisions yet." />
      </Card>

      <Card title="Guardrails" subtitle="Enforced by the desk at approval time, whatever this page shows.">
        <DefinitionList
          columns={3}
          items={[
            {
              term: 'Tradable tickers',
              value: list(s.limits.Allowlist).length ? list(s.limits.Allowlist).join(', ') : 'none — the desk trades nothing',
            },
            { term: 'Max per order', value: formatMoney(s.limits.MaxOrderNotional) },
            { term: 'Max position', value: `${formatPercent(s.limits.MaxPositionWeight, 0)} of equity` },
            { term: 'Daily loss stop', value: formatPercent(s.limits.MaxDailyLossPct) },
            { term: 'Max price drift', value: `${formatNumber(s.limits.MaxDriftBps, 0)} bps since proposal` },
            { term: 'Proposal lifetime', value: formatGoDuration(s.limits.TicketTTL) },
            { term: 'Short selling', value: s.limits.AllowShort ? 'allowed' : 'not allowed' },
          ]}
        />
      </Card>
    </div>
  );
}

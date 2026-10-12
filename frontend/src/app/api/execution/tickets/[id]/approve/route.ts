/**
 * Approve a proposed ticket. The owner re-enters their password; the API
 * verifies it against the principal the dashboard runs as, runs every
 * guardrail, and submits a paper limit order only if all of them pass.
 */

import type { NextRequest, NextResponse } from 'next/server';

import { forwardPost, refuse } from '@/lib/server/execution';
import { TICKET_ID, mutationRefusal, readJsonObject } from '@/lib/server/mutation-guard';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';

export async function POST(
  req: NextRequest,
  ctx: { params: Promise<{ id: string }> },
): Promise<NextResponse> {
  const why = mutationRefusal(req);
  if (why) return refuse(403, 'forbidden', why);
  const { id } = await ctx.params;
  if (!TICKET_ID.test(id)) return refuse(400, 'bad_request', 'invalid ticket id');

  const body = await readJsonObject(req);
  if (!body) return refuse(400, 'bad_request', 'the body must be a small JSON object');
  const password = typeof body.password === 'string' ? body.password : '';
  if (!password) return refuse(400, 'bad_request', 're-enter your password to approve');

  return forwardPost(`execution/tickets/${encodeURIComponent(id)}/approve`, { password });
}

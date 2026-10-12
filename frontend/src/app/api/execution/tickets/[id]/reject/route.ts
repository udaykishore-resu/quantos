/** Reject a proposed ticket. No password: declining an order is always safe. */

import type { NextRequest, NextResponse } from 'next/server';

import { forwardPost, refuse } from '@/lib/server/execution';
import { TICKET_ID, mutationRefusal, readJsonObject, stringField } from '@/lib/server/mutation-guard';

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
  return forwardPost(`execution/tickets/${encodeURIComponent(id)}/reject`, {
    reason: stringField(body, 'reason', 280),
  });
}

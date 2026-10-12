/**
 * The kill switch. No password, by design: stopping must be one click. It
 * still passes the same-origin guard, so another site cannot pull it.
 */

import type { NextRequest, NextResponse } from 'next/server';

import { forwardPost, refuse } from '@/lib/server/execution';
import { mutationRefusal, readJsonObject, stringField } from '@/lib/server/mutation-guard';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';

export async function POST(req: NextRequest): Promise<NextResponse> {
  const why = mutationRefusal(req);
  if (why) return refuse(403, 'forbidden', why);
  const body = (await readJsonObject(req)) ?? {};
  return forwardPost('execution/halt', {
    reason: stringField(body, 'reason', 280) || 'halted from the dashboard',
  });
}

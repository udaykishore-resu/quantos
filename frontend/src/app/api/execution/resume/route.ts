/** Lift the kill switch. Unlike halting, this needs the password again. */

import type { NextRequest, NextResponse } from 'next/server';

import { forwardPost, refuse } from '@/lib/server/execution';
import { mutationRefusal, readJsonObject } from '@/lib/server/mutation-guard';

export const dynamic = 'force-dynamic';
export const runtime = 'nodejs';

export async function POST(req: NextRequest): Promise<NextResponse> {
  const why = mutationRefusal(req);
  if (why) return refuse(403, 'forbidden', why);
  const body = await readJsonObject(req);
  if (!body) return refuse(400, 'bad_request', 'the body must be a small JSON object');
  const password = typeof body.password === 'string' ? body.password : '';
  if (!password) return refuse(400, 'bad_request', 're-enter your password to resume');
  return forwardPost('execution/resume', { password });
}

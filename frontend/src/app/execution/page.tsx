import type { Metadata } from 'next';

import { PageHeader } from '@/components/page';

import { ExecutionDesk } from './execution-desk';

export const metadata: Metadata = { title: 'Approvals' };
export const dynamic = 'force-dynamic';

export default function ExecutionPage() {
  return (
    <>
      <PageHeader
        title="Approvals"
        purpose="Orders QuantOS proposes for your paper brokerage account. Nothing is sent until you approve it here with your password, and every guardrail is re-checked at the moment you do."
      >
        <div
          role="note"
          className="flex flex-wrap items-center gap-3 rounded-md border-2 border-ink-900 bg-ink-100 px-4 py-3"
        >
          <span className="inline-flex items-center gap-1 rounded border border-ink-400 bg-white px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wider text-ink-700">
            <span aria-hidden="true">◇</span> Paper account only
          </span>
          <p className="max-w-prose text-sm font-medium leading-snug text-ink-900">
            Approved orders go to a <em>paper</em> brokerage account: real market
            prices, no real money. The desk refuses to start against a live
            account. A proposal is a probabilistic suggestion, not advice — the
            decision to approve is yours.
          </p>
        </div>
      </PageHeader>
      <ExecutionDesk />
    </>
  );
}

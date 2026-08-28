import type { Metadata } from 'next';

import { PageHeader } from '@/components/page';
import { DegradedBanner, ErrorState } from '@/components/states';
import { list } from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';

import { BacktestRunner } from './backtest-runner';

export const metadata: Metadata = { title: 'Backtest' };
export const dynamic = 'force-dynamic';

export default async function BacktestPage() {
  const [platform, strategiesR] = await Promise.all([
    platformStatus(),
    api().strategies(),
  ]);
  const strategies = strategiesR.ok ? list(strategiesR.data) : [];

  return (
    <>
      <PageHeader
        title="Backtest"
        purpose="Run a deterministic historical simulation and read its result honestly: the equity curve, the drawdown, how it behaved in each regime, every trade it took, and the Sharpe ratio deflated for the number of configurations tried."
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />

        <div
          role="note"
          className="rounded-md border border-ink-400 bg-ink-100 px-4 py-3 text-sm leading-relaxed text-ink-800"
        >
          <span className="font-semibold">How to read a backtest. </span>
          A backtest is a simulation of a strategy against recorded history under
          an assumed cost model. It is not evidence that the strategy would have
          made money, and it is certainly not evidence that it will. The two
          numbers that most often mislead are the Sharpe ratio — which flatters a
          configuration chosen after trying many — and the cost assumptions, which
          are shown in full on every run below so they can be argued with.
        </div>

        {!strategiesR.ok ? (
          <ErrorState error={strategiesR} what="the strategy list" />
        ) : (
          <BacktestRunner strategies={strategies} />
        )}
      </div>
    </>
  );
}

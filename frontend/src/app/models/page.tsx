import type { Metadata } from 'next';
import Link from 'next/link';

import { Card, DefinitionList, Freshness, Hash, PageHeader } from '@/components/page';
import { DegradedBanner, EmptyState, ErrorState } from '@/components/states';
import { Chip } from '@/components/verdicts';
import { MODEL_FEATURE_SET } from '@/lib/types';
import { list } from '@/lib/format';
import { api } from '@/lib/server/api';
import { platformStatus } from '@/lib/server/platform';

export const metadata: Metadata = { title: 'Models' };
export const dynamic = 'force-dynamic';

export default async function ModelsPage() {
  const [platform, modelsR, strategiesR] = await Promise.all([
    platformStatus(),
    api().models(),
    api().strategies(),
  ]);

  const models = modelsR.ok ? list(modelsR.data) : [];
  const strategies = strategiesR.ok ? list(strategiesR.data) : [];

  return (
    <>
      <PageHeader
        title="Models"
        purpose="Which inference artifacts are loaded and serving, what features each declares, and which configured strategies are producing the rules those models are blended with."
      />

      <div className="space-y-4">
        <DegradedBanner
          degraded={platform.degraded}
          ready={platform.readiness.ready}
          reason={platform.readiness.reason}
        />

        <Card
          title="Loaded model artifacts"
          actions={<Freshness servedAt={modelsR.ok ? modelsR.servedAt : undefined} meta={modelsR.ok ? modelsR.meta : undefined} />}
        >
          {!modelsR.ok ? (
            <ErrorState error={modelsR} what="the model registry" />
          ) : models.length === 0 ? (
            <EmptyState
              title="No model artifacts are loaded"
              detail="The platform is serving predictions from the deterministic rule prior alone. Every prediction produced in this state is labelled rules-fallback, and none of it carries learned information. The most common causes are a missing artifact directory and an artifact that failed validation — the platform refuses to serve a model whose out-of-sample accuracy and base rate are not recorded, since accuracy without the constant-guess benchmark is not a measurement."
              action={
                <Link href="/model-health" className="btn">
                  See what this means for prediction quality
                </Link>
              }
            />
          ) : (
            <ul className="space-y-3">
              {models.map((m) => (
                <li key={`${m.id}-${m.version}`} className="rounded border border-ink-200 p-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-sm font-semibold text-ink-900">{m.id}</span>
                    <Chip label="version" value={m.version} />
                    <Chip label="family" value={m.family} />
                    <Chip label="horizon" value={m.horizon} />
                    <Chip label="artifact" value={<Hash value={m.artifact_sha} chars={16} />} />
                  </div>

                  <div className="mt-2">
                    <h3 className="text-[11px] font-semibold uppercase tracking-wide text-ink-500">
                      Declared features ({list(m.features).length})
                    </h3>
                    <p className="mt-0.5 break-words font-mono text-xs text-ink-700">
                      {list(m.features).join(', ') || 'none declared'}
                    </p>
                    <FeatureContractNote features={list(m.features)} />
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card
          title="Expected feature contract"
          subtitle="The ordered list the shipped direction model must declare, from internal/domain/features.go."
        >
          <ol className="grid grid-cols-2 gap-x-4 gap-y-0.5 font-mono text-xs text-ink-700 sm:grid-cols-4">
            {MODEL_FEATURE_SET.map((f, i) => (
              <li key={f}>
                <span className="text-ink-400">{String(i + 1).padStart(2, '0')} </span>
                {f}
              </li>
            ))}
          </ol>
          <p className="mt-2 max-w-prose text-xs leading-relaxed text-ink-600">
            Feature ordering is asserted at load time. A model whose declared list
            does not match this one exactly is rejected rather than served against a
            silently reordered vector — which is the failure mode that produces
            confident, wrong predictions with no visible symptom.
          </p>
        </Card>

        <Card
          title="Configured strategies"
          subtitle="No strategy logic lives in Go source. Every rule below comes from a configuration file with its own hash."
          actions={<Freshness servedAt={strategiesR.ok ? strategiesR.servedAt : undefined} meta={strategiesR.ok ? strategiesR.meta : undefined} />}
        >
          {!strategiesR.ok ? (
            <ErrorState error={strategiesR} what="strategies" />
          ) : strategies.length === 0 ? (
            <EmptyState title="No strategies are loaded." />
          ) : (
            <ul className="space-y-3">
              {strategies.map((s) => (
                <li key={s.id} className="rounded border border-ink-200 p-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-sm font-semibold text-ink-900">{s.name}</span>
                    <Chip label="id" value={s.id} />
                    <Chip label="version" value={s.version} />
                    <span
                      className={`rounded border px-1.5 py-0.5 text-[10px] font-bold uppercase ${
                        s.enabled
                          ? 'border-allow bg-teal-50 text-allow'
                          : 'border-ink-300 bg-ink-50 text-ink-500'
                      }`}
                    >
                      <span aria-hidden="true">{s.enabled ? '✓ ' : '– '}</span>
                      {s.enabled ? 'enabled' : 'disabled'}
                    </span>
                  </div>
                  <p className="mt-1 text-xs text-ink-600">{s.description}</p>
                  <div className="mt-1.5 flex flex-wrap gap-1.5">
                    <Chip label="rules" value={list(s.rules).length} />
                    <Chip
                      label="counter rules"
                      value={list(s.rules).filter((r) => r.counter).length}
                    />
                    <Chip label="interval" value={s.interval} />
                    <Chip label="hash" value={<Hash value={s.hash} />} />
                  </div>
                  <div className="mt-2">
                    <DefinitionList
                      columns={3}
                      items={[
                        { term: 'Min rule score', value: String(s.policy.min_rule_score) },
                        { term: 'Min final score', value: String(s.policy.min_final_score) },
                        { term: 'Min confidence', value: String(s.policy.min_confidence) },
                        { term: 'Min directional edge', value: String(s.policy.min_directional_edge) },
                        { term: 'Min risk / reward', value: String(s.policy.min_risk_reward) },
                        { term: 'Horizon', value: s.policy.horizon },
                      ]}
                    />
                  </div>
                  <p className="mt-2 text-[11px] text-ink-500">
                    A candidate must clear every threshold above before it can become
                    a signal. These are the gates the &ldquo;why no signal?&rdquo;
                    reasons on the stocks page refer to.
                  </p>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </>
  );
}

function FeatureContractNote({ features }: { features: string[] }) {
  const expected = MODEL_FEATURE_SET;
  const matches =
    features.length === expected.length && features.every((f, i) => f === expected[i]);
  if (features.length === 0) return null;
  return matches ? (
    <p className="mt-1 text-[11px] font-medium text-allow">
      <span aria-hidden="true">✓ </span>
      Matches the expected feature contract exactly, in order.
    </p>
  ) : (
    <p className="mt-1 text-[11px] font-medium text-watch">
      <span aria-hidden="true">! </span>
      This declared list differs from the shipped direction model&apos;s expected
      contract. That is legitimate for a model with a different feature set, but
      worth confirming — a mismatch in ordering is invisible at runtime.
    </p>
  );
}

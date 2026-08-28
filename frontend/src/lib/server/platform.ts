import 'server-only';

import { api } from './api';
import type { HealthResponse, Readiness } from '../types';

/**
 * The platform's own state, as every page's banner needs it.
 *
 * `/readyz` and `/healthz` answer different questions and the dashboard needs
 * both: readiness says whether the platform can serve its purpose at all (and
 * therefore whether it is still emitting signals), health says which subsystems
 * are missing. A page that showed only one of them would either cry wolf over a
 * missing optional subsystem or stay silent while signal emission is suspended.
 */
export interface PlatformStatus {
  readiness: Readiness;
  health: HealthResponse | null;
  /** Union of everything reported degraded, deduplicated. */
  degraded: string[];
  /** True when the health probe itself could not be reached. */
  unreachable: boolean;
  unreachableReason: string;
}

export async function platformStatus(): Promise<PlatformStatus> {
  const client = api({ revalidateSeconds: 0 });
  const [readiness, health] = await Promise.all([
    client.readiness(),
    client.health(),
  ]);

  const degraded = new Set<string>(readiness.degraded);
  if (health.ok) {
    for (const d of health.degraded) degraded.add(d);
    for (const d of health.data.degraded ?? []) degraded.add(d);
  }

  return {
    readiness,
    health: health.ok ? health.data : null,
    degraded: [...degraded],
    unreachable: !health.ok && health.status === 0,
    unreachableReason: health.ok ? '' : `${health.message}${health.detail ? ` (${health.detail})` : ''}`,
  };
}

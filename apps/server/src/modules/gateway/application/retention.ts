import { QuotaDay } from '../domain/quota-day.ts';
import type { GatewayDeps } from './ports.ts';

/**
 * Daily housekeeping: call-log entries older than `callLogRetentionDays` and quota counters of
 * days before yesterday (UTC) are deleted. Returns how many of each.
 */
export async function pruneGatewayHistory(
  deps: Pick<GatewayDeps, 'callLog' | 'quota' | 'clock'>,
  opts: { callLogRetentionDays: number },
): Promise<{ calls: number; quotaDays: number }> {
  const now = deps.clock.now();
  const cutoff = new Date(now.getTime() - opts.callLogRetentionDays * 86_400_000);
  const calls = await deps.callLog.deleteOlderThan(cutoff);
  const quotaDays = await deps.quota.deleteDaysBefore(QuotaDay.oldestKept(now));
  return { calls, quotaDays };
}

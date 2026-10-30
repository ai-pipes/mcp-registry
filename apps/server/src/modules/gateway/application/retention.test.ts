import { describe, expect, it } from 'vite-plus/test';
import { pruneGatewayHistory } from './retention.ts';

describe('pruneGatewayHistory', () => {
  it('deletes calls older than the retention and quota days before yesterday (UTC)', async () => {
    const calls: Date[] = [];
    const days: string[] = [];
    const r = await pruneGatewayHistory(
      {
        clock: { now: () => new Date('2026-10-30T01:00:00Z') },
        callLog: {
          record: () => Promise.resolve(),
          list: () => Promise.resolve([]),
          deleteOlderThan: (d) => {
            calls.push(d);
            return Promise.resolve(4);
          },
        },
        quota: {
          tryConsume: () => Promise.resolve({ ok: true }),
          deleteDaysBefore: (day) => {
            days.push(day);
            return Promise.resolve(2);
          },
        },
      },
      { callLogRetentionDays: 90 },
    );
    expect(calls).toEqual([new Date('2026-08-01T01:00:00Z')]);
    expect(days).toEqual(['2026-10-29']);
    expect(r).toEqual({ calls: 4, quotaDays: 2 });
  });
});

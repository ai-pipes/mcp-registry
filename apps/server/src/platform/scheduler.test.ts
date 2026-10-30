import { afterEach, beforeEach, describe, expect, it, vi } from 'vite-plus/test';
import { makeCatalogDeps } from '../../test/fakes/in-memory-catalog.ts';
import { createServer, type Principal, type SyncDeps } from '../modules/catalog/index.ts';
import type { CallLog, Quota } from '../modules/gateway/index.ts';
import { ok, type UpstreamClient, type UpstreamTarget } from '../shared-kernel/index.ts';
import { createLogger } from './logger.ts';
import { startScheduler } from './scheduler.ts';

const HOUR = 3_600_000;
const admin: Principal = { userId: 'admin-1', role: 'admin', groups: [] };

let calls: UpstreamTarget[];
let deps: SyncDeps;
let lines: Record<string, unknown>[];
let deletedBefore: Date[];
let callLog: CallLog;
let quota: Quota;
let quotaDaysBefore: string[];
let sessionCutoffs: { expiresAtOrBefore: Date; lastSeenBefore: Date }[];
const deleteDead = (cutoffs: { expiresAtOrBefore: Date; lastSeenBefore: Date }) => {
  sessionCutoffs.push(cutoffs);
  return Promise.resolve(4);
};
const DAY = 24 * HOUR;
const NOW = new Date('2026-10-30T03:00:00Z');

function setup(syncIntervalHours: number) {
  const logger = createLogger('info', {
    write: (line: string) => {
      const parsed: unknown = JSON.parse(line);
      if (parsed !== null && typeof parsed === 'object') lines.push({ ...parsed });
    },
  });
  // Fixed clock: whatever the fake timers do, "now" is NOW.
  return startScheduler({
    catalogSync: deps,
    gateway: { callLog, quota },
    identity: { sessions: { deleteDead } },
    clock: { now: () => NOW },
    config: { syncIntervalHours, callLogRetentionDays: 90 },
    logger,
  });
}

beforeEach(async () => {
  vi.useFakeTimers();
  calls = [];
  lines = [];
  deletedBefore = [];
  quotaDaysBefore = [];
  sessionCutoffs = [];
  quota = {
    tryConsume: () => Promise.resolve({ ok: true }),
    deleteDaysBefore: (day) => {
      quotaDaysBefore.push(day);
      return Promise.resolve(1);
    },
  };
  callLog = {
    record: () => Promise.resolve(),
    list: () => Promise.resolve([]),
    deleteOlderThan: (d) => {
      deletedBefore.push(d);
      return Promise.resolve(3);
    },
  };
  const upstream: UpstreamClient = {
    listTools: (t) => {
      calls.push(t);
      return Promise.resolve([{ name: 'a', inputSchema: { type: 'object' } }]);
    },
    callTool: () => Promise.reject(new Error('unused')),
  };
  const h = makeCatalogDeps();
  deps = {
    ...h.deps,
    upstream,
    credentials: {
      isActiveAdmin: () => Promise.resolve(true),
      personalSecret: () => Promise.resolve(ok('unused')),
    },
    maxResponseBytes: 100,
  };
  const r = await createServer(deps, {
    principal: admin,
    slug: 'alpha',
    name: 'Alpha',
    description: '',
    url: 'https://alpha.example.com/mcp',
    auth: { kind: 'none' },
    access: { kind: 'all' },
  });
  if (!r.ok) throw r.error;
});
afterEach(() => {
  vi.useRealTimers();
});

describe('startScheduler', () => {
  it('synchronises at start and then every syncIntervalHours', async () => {
    const s = setup(6);
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(6 * HOUR - 1);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(6 * HOUR);
    expect(calls).toHaveLength(3);
    await s.stop();
  });

  it('an interval beyond the setTimeout limit (~24.8 days) is honoured, not fired at once', async () => {
    const s = setup(1000);
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(999 * HOUR);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(HOUR);
    expect(calls).toHaveLength(2);
    await s.stop();
  });

  it('stop cancels the next run and waits for the running one', async () => {
    const s = setup(1);
    await vi.advanceTimersByTimeAsync(0);
    await s.stop();
    await vi.advanceTimersByTimeAsync(10 * HOUR);
    expect(calls).toHaveLength(1);
  });

  it('logs each run without secrets, and survives a failing run', async () => {
    const list = deps.servers.list.bind(deps.servers);
    deps.servers.list = () => Promise.reject(new Error('db down'));
    const s = setup(1);
    await vi.advanceTimersByTimeAsync(0);
    expect(lines.some((l) => l.msg === 'catalog sync: run failed')).toBe(true);
    deps.servers.list = list;
    await vi.advanceTimersByTimeAsync(HOUR);
    expect(lines.some((l) => l.msg === 'catalog sync: done')).toBe(true);
    await s.stop();
  });

  it('deletes old call-log entries and quota days at start and then daily', async () => {
    const s = setup(6);
    await vi.advanceTimersByTimeAsync(0);
    const cutoff = new Date('2026-08-01T03:00:00Z');
    expect(deletedBefore).toEqual([cutoff]);
    await vi.advanceTimersByTimeAsync(DAY - 1);
    expect(deletedBefore).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(deletedBefore).toEqual([cutoff, cutoff]);
    expect(quotaDaysBefore).toEqual(['2026-10-29', '2026-10-29']);
    expect(
      lines.some((l) => l.msg === 'gateway retention: done' && l.calls === 3 && l.quotaDays === 1),
    ).toBe(true);
    await s.stop();
    await vi.advanceTimersByTimeAsync(10 * DAY);
    expect(deletedBefore).toHaveLength(2);
  });

  it('deletes expired sessions at start and then daily, through identity (M9)', async () => {
    const s = setup(6);
    await vi.advanceTimersByTimeAsync(0);
    const cutoffs = {
      expiresAtOrBefore: NOW,
      lastSeenBefore: new Date('2026-10-29T15:00:00Z'),
    };
    expect(sessionCutoffs).toEqual([cutoffs]);
    await vi.advanceTimersByTimeAsync(DAY);
    expect(sessionCutoffs).toEqual([cutoffs, cutoffs]);
    expect(lines.some((l) => l.msg === 'session retention: done' && l.sessions === 4)).toBe(true);
    await s.stop();
  });

  it('a failing retention run is logged and the schedule goes on', async () => {
    callLog.deleteOlderThan = () => Promise.reject(new Error('db down'));
    const s = setup(6);
    await vi.advanceTimersByTimeAsync(0);
    expect(lines.some((l) => l.msg === 'gateway retention: run failed')).toBe(true);
    callLog.deleteOlderThan = (d) => {
      deletedBefore.push(d);
      return Promise.resolve(0);
    };
    await vi.advanceTimersByTimeAsync(DAY);
    expect(deletedBefore).toHaveLength(1);
    await s.stop();
  });
});

import { describe, expect, it } from 'vite-plus/test';
import { listCalls } from './list-calls.ts';
import type { CallLogFilter } from './ports.ts';

const admin = { userId: 'a', role: 'admin' as const, groups: [] };
const member = { userId: 'm', role: 'member' as const, groups: [] };

function fakeLog() {
  const seen: CallLogFilter[] = [];
  return {
    seen,
    callLog: {
      record: () => Promise.resolve(),
      list: (f: CallLogFilter) => {
        seen.push(f);
        return Promise.resolve([]);
      },
      deleteOlderThan: () => Promise.resolve(0),
    },
  };
}

describe('listCalls', () => {
  it('refuses a member without reading the log', async () => {
    const log = fakeLog();
    const r = await listCalls(log, { principal: member });
    expect(r.ok).toBe(false);
    expect(log.seen).toEqual([]);
  });

  it('defaults the limit to 100 and passes only the filters given', async () => {
    const log = fakeLog();
    await listCalls(log, { principal: admin, server: 'gitea' });
    await listCalls(log, { principal: admin, userId: 'u', outcome: 'ok', limit: 5 });
    expect(log.seen).toEqual([
      { server: 'gitea', limit: 100 },
      { userId: 'u', outcome: 'ok', limit: 5 },
    ]);
  });
});

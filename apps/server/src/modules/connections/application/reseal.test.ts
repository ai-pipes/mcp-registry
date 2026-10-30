import { describe, expect, it } from 'vite-plus/test';
import { makeConnectionsDeps } from '../../../../test/fakes/in-memory-connections.ts';
import type { SecretBox } from '../../../shared-kernel/index.ts';
import { Connection } from '../domain/connection.ts';
import { resealConnections } from './reseal.ts';

/** A box per key id: sealed bytes are `<id>:<plain>`; open refuses another id. */
const box = (id: string): SecretBox => ({
  seal: (plain) => new TextEncoder().encode(`${id}:${plain}`),
  open: (sealed) => {
    const text = new TextDecoder().decode(sealed);
    if (!text.startsWith(`${id}:`)) throw new Error('wrong key');
    return text.slice(id.length + 1);
  },
});
const OLD = box('old');
const NEW = box('new');
const now = new Date('2026-10-30T12:00:00Z');

function seeded() {
  const f = makeConnectionsDeps({ value: now });
  const put = (c: Connection) => f.store.rows.set(`${c.userId}\u0000${c.server}`, c.snapshot());
  put(
    Connection.connectOAuth({
      userId: 'u1',
      server: 'gitea',
      access: OLD.seal('A1'),
      refresh: OLD.seal('R1'),
      expiresAt: new Date('2026-10-30T13:00:00Z'),
      issuedFor: 'fp',
      revokeUrl: null,
      now,
    }),
  );
  put(
    Connection.connectOAuth({
      userId: 'u2',
      server: 'youtrack',
      access: OLD.seal('A2'),
      refresh: null,
      expiresAt: null,
      issuedFor: 'fp',
      revokeUrl: null,
      now,
    }),
  );
  put(
    Connection.connectPat({
      userId: 'u1',
      server: 'jira',
      token: OLD.seal('PAT'),
      issuedFor: 'fp',
      now,
    }),
  );
  return { ...f, put };
}

const all = (f: ReturnType<typeof seeded>) => [...f.store.rows.values()];

describe('resealConnections', () => {
  it('re-seals access, refresh and PAT tokens; the old key opens none of them', async () => {
    const f = seeded();
    const r = await resealConnections(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 3, current: 0, failed: [] });
    const plain = all(f).map((s) => [
      s.server,
      NEW.open(s.access),
      s.refresh && NEW.open(s.refresh),
    ]);
    expect(plain).toEqual([
      ['gitea', 'A1', 'R1'],
      ['youtrack', 'A2', null],
      ['jira', 'PAT', null],
    ]);
    for (const s of all(f)) {
      expect(() => OLD.open(s.access)).toThrow();
      if (s.refresh) expect(() => OLD.open(s.refresh!)).toThrow();
      expect(s.updatedAt).toEqual(now);
    }
  });

  it('a repeated run changes nothing', async () => {
    const f = seeded();
    await resealConnections(f.deps, { from: OLD, to: NEW });
    expect(await resealConnections(f.deps, { from: OLD, to: NEW })).toEqual({
      resealed: 0,
      current: 3,
      failed: [],
    });
  });

  it('leaves a connection with a token neither key opens, and names it', async () => {
    const f = seeded();
    f.put(
      Connection.connectOAuth({
        userId: 'u1',
        server: 'gitea',
        access: OLD.seal('A1'),
        refresh: box('lost').seal('R1'),
        expiresAt: null,
        issuedFor: 'fp',
        revokeUrl: null,
        now,
      }),
    );
    const r = await resealConnections(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 2, current: 0, failed: ['u1/gitea'] });
    expect(OLD.open(f.store.rows.get('u1\u0000gitea')!.access)).toBe('A1');
  });

  it('works under the connection lock and skips one deleted meanwhile', async () => {
    const f = seeded();
    const keys = await f.store.listKeys();
    expect(keys).toHaveLength(3);
    f.store.listKeys = () => Promise.resolve([...keys, { userId: 'gone', server: 'gitea' }]);
    const r = await resealConnections(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 3, current: 0, failed: [] });
  });
});

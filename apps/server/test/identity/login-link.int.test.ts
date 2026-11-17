import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import {
  completeLinkLogin,
  createPgIdentity,
  issueLoginLink,
  provisionUser,
  type IdentityDeps,
} from '../../src/modules/identity/index.ts';
import { PgLoginLinkRepository } from '../../src/modules/identity/infrastructure/pg-repositories.ts';
import { startPg, truncateAll } from '../pg.ts';

let pg: Awaited<ReturnType<typeof startPg>>;
let deps: IdentityDeps;
let repo: PgLoginLinkRepository;

const NOW = new Date('2026-06-01T12:00:00Z');

beforeAll(async () => {
  pg = await startPg();
  repo = new PgLoginLinkRepository(pg.sql);
  deps = {
    ...createPgIdentity(pg.sql),
    oidc: {
      authorizationUrl: () => Promise.reject(new Error('unused')),
      exchange: () => Promise.reject(new Error('unused')),
    },
    policy: { loginGroup: 'mcp-users', adminGroup: 'mcp-admins' },
    audit: { record: () => Promise.resolve() },
    clock: { now: () => NOW },
  };
}, 120_000);

afterAll(async () => {
  await pg.stop();
});

beforeEach(async () => {
  await truncateAll(pg.sql);
});

async function person(sub: string): Promise<string> {
  const r = await provisionUser(deps, { sub, name: sub, admin: false });
  if (!r.ok) throw r.error;
  return r.value.id;
}

const record = (codeHash: string, userId: string, expiresAt: Date) => ({
  codeHash,
  userId,
  createdAt: NOW,
  expiresAt,
});

const count = async (): Promise<number> => {
  const [r] = await pg.sql<{ n: number }[]>`select count(*)::int as n from identity.login_links`;
  return r?.n ?? 0;
};

describe('login links in Postgres', () => {
  it('consume returns the record once', async () => {
    const userId = await person('alice');
    const expiresAt = new Date(NOW.getTime() + 60_000);
    await repo.insert(record('h1', userId, expiresAt));
    expect(await repo.consume('h1')).toEqual(record('h1', userId, expiresAt));
    expect(await repo.consume('h1')).toBeNull();
  });

  it('two concurrent redemptions create exactly one session', async () => {
    await person('alice');
    const issued = await issueLoginLink(deps, { sub: 'alice' });
    if (!issued.ok) throw issued.error;
    const { code } = issued.value;
    const results = await Promise.all([
      completeLinkLogin(deps, { code, ip: '1.1.1.1' }),
      completeLinkLogin(deps, { code, ip: '1.1.1.1' }),
    ]);
    expect(results.filter((r) => r.ok)).toHaveLength(1);
    const failed = results.find((r) => !r.ok);
    expect(failed && !failed.ok && failed.error.code).toBe('invalid_link');
    const [row] = await pg.sql<{ n: number }[]>`select count(*)::int as n from identity.sessions`;
    expect(row?.n).toBe(1);
  });

  it('deleteExpired removes only links at or past expires_at', async () => {
    const userId = await person('alice');
    await repo.insert(record('past', userId, new Date(NOW.getTime() - 1)));
    await repo.insert(record('at', userId, NOW));
    await repo.insert(record('future', userId, new Date(NOW.getTime() + 1)));
    expect(await repo.deleteExpired(NOW)).toBe(2);
    expect(await repo.consume('future')).not.toBeNull();
    expect(await count()).toBe(0);
  });

  it("deleteByUser removes that person's links only", async () => {
    const alice = await person('alice');
    const bob = await person('bob');
    const expiresAt = new Date(NOW.getTime() + 60_000);
    await repo.insert(record('a1', alice, expiresAt));
    await repo.insert(record('a2', alice, expiresAt));
    await repo.insert(record('b1', bob, expiresAt));
    await repo.deleteByUser(alice);
    expect(await repo.consume('a1')).toBeNull();
    expect(await repo.consume('a2')).toBeNull();
    expect(await repo.consume('b1')).not.toBeNull();
  });

  it('a link sign-in sets last_login_at and leaves groups and role untouched', async () => {
    await person('alice');
    const [before] = await pg.sql<{ groups: string[]; role: string }[]>`
      select groups, role from identity.users where oidc_sub = 'alice'`;
    const issued = await issueLoginLink(deps, { sub: 'alice' });
    if (!issued.ok) throw issued.error;
    const later = new Date(NOW.getTime() + 60_000);
    const r = await completeLinkLogin(
      { ...deps, clock: { now: () => later } },
      { code: issued.value.code, ip: '1.1.1.1' },
    );
    expect(r.ok).toBe(true);
    const [after] = await pg.sql<{ groups: string[]; role: string; last_login_at: Date }[]>`
      select groups, role, last_login_at from identity.users where oidc_sub = 'alice'`;
    expect(after?.last_login_at).toEqual(later);
    expect(after?.groups).toEqual(before?.groups);
    expect(after?.role).toBe(before?.role);
  });
});

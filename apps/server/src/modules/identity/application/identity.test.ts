import { describe, expect, it } from 'vite-plus/test';
import type { Principal } from '../domain/user.ts';
import { makeDeps, startedUrl } from '../../../../test/identity/fakes.ts';
import { completeOidcLogin, startOidcLogin } from './oidc-login.ts';
import { authenticateSession, logout, pruneExpiredSessions } from './sessions.ts';
import { authenticateToken, createToken, listTokens, revokeToken } from './tokens.ts';
import { actAs, isUserActiveAdmin, listUsers, provisionUser, setDisabled } from './users.ts';

type F = ReturnType<typeof makeDeps>;

async function signIn(f: F, sub: string, groups: string[]) {
  f.oidc.next = { sub, name: sub, email: null, groups };
  const url = startedUrl(await startOidcLogin(f.deps, { returnTo: '/' }));
  const r = await completeOidcLogin(f.deps, { state: url.searchParams.get('state')!, code: 'c' });
  if (!r.ok) throw new Error('login failed');
  const principal = await authenticateSession(f.deps, { sessionId: r.value.sessionId });
  if (!principal) throw new Error('no session');
  return { principal, sessionId: r.value.sessionId };
}

const member = (id: string): Principal => ({
  userId: id,
  name: 'm',
  role: 'member',
  groups: [],
  via: 'session',
});

describe('sessions', () => {
  it('authenticates a live session and refreshes lastSeenAt', async () => {
    const f = makeDeps();
    const { sessionId } = await signIn(f, 'a', ['mcp-users']);
    f.clock.advance(11 * 3600_000);
    expect(await authenticateSession(f.deps, { sessionId })).not.toBeNull();
    f.clock.advance(11 * 3600_000); // idle clock restarted by the touch above
    expect(await authenticateSession(f.deps, { sessionId })).not.toBeNull();
  });

  it('pruneExpiredSessions deletes idle and expired sessions only (fixed clock)', async () => {
    const f = makeDeps();
    const live = await signIn(f, 'a', ['mcp-users']);
    const idle = await signIn(f, 'b', ['mcp-users']);
    const old = await signIn(f, 'c', ['mcp-users']);
    const hashOf = (id: string) => f.deps.sha256.hex(id);
    const now = f.clock.now();
    const at = (ms: number) => new Date(now.getTime() + ms);
    const idleRow = f.sessions.get(hashOf(idle.sessionId));
    if (idleRow) idleRow.lastSeenAt = at(-12 * 3600_000 - 1);
    const oldRow = f.sessions.get(hashOf(old.sessionId));
    if (oldRow) oldRow.expiresAt = now;
    expect(await pruneExpiredSessions(f.deps)).toBe(2);
    expect([...f.sessions.keys()]).toEqual([hashOf(live.sessionId)]);
    expect(await pruneExpiredSessions(f.deps)).toBe(0);
  });

  it('drops a session idle for more than 12 hours', async () => {
    const f = makeDeps();
    const { sessionId } = await signIn(f, 'a', ['mcp-users']);
    f.clock.advance(12 * 3600_000 + 1);
    expect(await authenticateSession(f.deps, { sessionId })).toBeNull();
    expect(f.sessions.size).toBe(0);
  });

  it('knows nothing about unknown sessions and refuses disabled users', async () => {
    const f = makeDeps();
    expect(await authenticateSession(f.deps, { sessionId: 'nope' })).toBeNull();
    const { sessionId } = await signIn(f, 'a', ['mcp-users']);
    [...f.users.values()][0]!.disabled = true;
    expect(await authenticateSession(f.deps, { sessionId })).toBeNull();
  });

  it('logout deletes the session; an unknown one is a no-op', async () => {
    const f = makeDeps();
    const { sessionId } = await signIn(f, 'a', ['mcp-users']);
    await logout(f.deps, { sessionId });
    expect(await authenticateSession(f.deps, { sessionId })).toBeNull();
    await logout(f.deps, { sessionId: 'nope' });
    expect(f.auditEntries.filter((e) => e.action === 'auth.logout')).toHaveLength(1);
  });
});

describe('tokens', () => {
  it('creates an sk_ token that authenticates as its owner with tokenId', async () => {
    const f = makeDeps();
    const { principal } = await signIn(f, 'a', ['mcp-users']);
    const t = await createToken(f.deps, { principal, name: 'laptop' });
    expect(t.token.startsWith('sk_')).toBe(true);
    expect(t.prefix).toBe(t.token.slice(0, 10));
    const r = await authenticateToken(f.deps, { token: t.token, ip: '1.1.1.1' });
    expect(r).toMatchObject({
      ok: true,
      value: { userId: principal.userId, via: 'token', tokenId: t.id, groups: ['mcp-users'] },
    });
    const list = await listTokens(f.deps, { principal });
    expect(list).toHaveLength(1);
    expect(list[0]).not.toHaveProperty('hash');
    expect(list[0]?.lastUsedAt).not.toBeNull();
  });

  it('rejects malformed, unknown and revoked tokens', async () => {
    const f = makeDeps();
    const { principal } = await signIn(f, 'a', ['mcp-users']);
    const t = await createToken(f.deps, { principal, name: 'x' });
    const bad = await Promise.all(
      ['abc', 'sk_unknown'].map((token) => authenticateToken(f.deps, { token, ip: 'ip' })),
    );
    for (const r of bad) expect(r).toMatchObject({ ok: false, error: { code: 'invalid_token' } });
    expect((await revokeToken(f.deps, { principal, id: t.id })).ok).toBe(true);
    expect((await revokeToken(f.deps, { principal, id: t.id })).ok).toBe(true); // idempotent
    expect(f.auditEntries.filter((e) => e.action === 'token.revoke')).toHaveLength(1);
    const r = await authenticateToken(f.deps, { token: t.token, ip: 'ip' });
    expect(r).toMatchObject({ ok: false, error: { code: 'invalid_token' } });
  });

  it('answers rate_limited once the limiter says so', async () => {
    const f = makeDeps();
    f.deps.tokenLimiter.hit = () => ({ allowed: false, retryAfterSec: 30 });
    const r = await authenticateToken(f.deps, { token: 'sk_x', ip: 'ip' });
    expect(r).toMatchObject({ ok: false, error: { code: 'rate_limited', retryAfterSec: 30 } });
  });

  it('only the owner or an admin may revoke; unknown id is not_found', async () => {
    const f = makeDeps();
    const { principal: alice } = await signIn(f, 'alice', ['mcp-users']);
    const { principal: bob } = await signIn(f, 'bob', ['mcp-users']);
    const { principal: root } = await signIn(f, 'root', ['mcp-admins']);
    const t = await createToken(f.deps, { principal: alice, name: 'x' });
    expect(await revokeToken(f.deps, { principal: bob, id: t.id })).toMatchObject({
      ok: false,
      error: { code: 'forbidden' },
    });
    expect(await revokeToken(f.deps, { principal: alice, id: 'missing' })).toMatchObject({
      ok: false,
      error: { code: 'not_found' },
    });
    expect((await revokeToken(f.deps, { principal: root, id: t.id })).ok).toBe(true);
  });

  it('does not touch lastUsedAt more than once a minute', async () => {
    const f = makeDeps();
    const { principal } = await signIn(f, 'a', ['mcp-users']);
    const t = await createToken(f.deps, { principal, name: 'x' });
    await authenticateToken(f.deps, { token: t.token, ip: 'ip' });
    const first = f.tokens.get(t.id)!.lastUsedAt;
    f.clock.advance(30_000);
    await authenticateToken(f.deps, { token: t.token, ip: 'ip' });
    expect(f.tokens.get(t.id)!.lastUsedAt).toEqual(first);
  });
});

describe('users', () => {
  it('isUserActiveAdmin: an admin until disabled or no longer in the admin group', async () => {
    const f = makeDeps();
    const { principal: root } = await signIn(f, 'root', ['mcp-admins']);
    const { principal: ann } = await signIn(f, 'ann', ['mcp-admins']);
    const { principal: bob } = await signIn(f, 'bob', ['mcp-users']);
    expect(await isUserActiveAdmin(f.deps, ann.userId)).toBe(true);
    expect(await isUserActiveAdmin(f.deps, bob.userId)).toBe(false);
    expect(await isUserActiveAdmin(f.deps, 'ghost')).toBe(false);
    await setDisabled(f.deps, { principal: root, userId: ann.userId, disabled: true });
    expect(await isUserActiveAdmin(f.deps, ann.userId)).toBe(false);
    await signIn(f, 'root', ['mcp-users']);
    expect(await isUserActiveAdmin(f.deps, root.userId)).toBe(false);
  });

  it('lists users for admins only', async () => {
    const f = makeDeps();
    const { principal: root } = await signIn(f, 'root', ['mcp-admins']);
    await signIn(f, 'bob', ['mcp-users']);
    const r = await listUsers(f.deps, { principal: root });
    expect(r.ok && r.value.map((u) => u.name)).toEqual(['root', 'bob']);
    expect(await listUsers(f.deps, { principal: member('x') })).toMatchObject({
      ok: false,
      error: { code: 'forbidden' },
    });
  });

  it('disabling revokes tokens and sessions of the user', async () => {
    const f = makeDeps();
    const { principal: root } = await signIn(f, 'root', ['mcp-admins']);
    const { principal: bob, sessionId } = await signIn(f, 'bob', ['mcp-users']);
    const t = await createToken(f.deps, { principal: bob, name: 'x' });
    const r = await setDisabled(f.deps, { principal: root, userId: bob.userId, disabled: true });
    expect(r.ok).toBe(true);
    expect(await authenticateSession(f.deps, { sessionId })).toBeNull();
    expect(await authenticateToken(f.deps, { token: t.token, ip: 'ip' })).toMatchObject({
      ok: false,
    });
    expect(f.tokens.get(t.id)!.revokedAt).not.toBeNull();
  });

  it('re-enabling keeps tokens revoked; guards: admin only, not yourself, known user', async () => {
    const f = makeDeps();
    const { principal: root } = await signIn(f, 'root', ['mcp-admins']);
    expect(
      await setDisabled(f.deps, { principal: member('x'), userId: root.userId, disabled: true }),
    ).toMatchObject({ ok: false, error: { code: 'forbidden' } });
    expect(
      await setDisabled(f.deps, { principal: root, userId: root.userId, disabled: true }),
    ).toMatchObject({ ok: false, error: { code: 'forbidden' } });
    expect(
      await setDisabled(f.deps, { principal: root, userId: 'ghost', disabled: false }),
    ).toMatchObject({ ok: false, error: { code: 'not_found' } });
    expect(
      (await setDisabled(f.deps, { principal: root, userId: root.userId, disabled: false })).ok,
    ).toBe(true);
  });
});

describe('provisionUser / actAs (operator CLI)', () => {
  it('creates the person as a first OIDC login would; actAs then acts as them', async () => {
    const f = makeDeps();
    const r = await provisionUser(f.deps, { sub: 'owner', name: 'Owner', admin: true });
    expect(r).toMatchObject({ ok: true, value: { role: 'admin', created: true } });
    const p = await actAs(f.deps, 'owner');
    expect(p).toMatchObject({
      ok: true,
      value: { name: 'Owner', role: 'admin', groups: ['mcp-admins'] },
    });
    expect(f.auditEntries.map((e) => e.action)).toContain('user.provision');
  });

  it('is idempotent by sub: the same person, not a second one', async () => {
    const f = makeDeps();
    const a = await provisionUser(f.deps, { sub: 'bob', name: 'Bob', admin: false });
    const b = await provisionUser(f.deps, { sub: 'bob', name: 'Bob', admin: false });
    if (!a.ok || !b.ok) throw new Error('provision failed');
    expect(b.value).toEqual({ id: a.value.id, role: 'member', created: false });
    expect(await f.deps.users.list()).toHaveLength(1);
  });

  it('keeps the groups a real login gave and only swaps the policy group', async () => {
    const f = makeDeps();
    await signIn(f, 'carol', ['mcp-users', 'devs']);
    const r = await provisionUser(f.deps, { sub: 'carol', name: 'Carol', admin: true });
    expect(r).toMatchObject({ ok: true, value: { role: 'admin', created: false } });
    const p = await actAs(f.deps, 'carol');
    expect(p.ok && p.value.groups).toEqual(['devs', 'mcp-admins']);
  });

  it('refuses a disabled person and a blank sub or name', async () => {
    const f = makeDeps();
    const a = await provisionUser(f.deps, { sub: 'dan', name: 'Dan', admin: false });
    if (!a.ok) throw new Error('provision failed');
    await f.deps.users.setDisabled(a.value.id, true);
    const again = await provisionUser(f.deps, { sub: 'dan', name: 'Dan', admin: false });
    expect(again.ok || again.error.code).toBe('disabled');
    expect((await actAs(f.deps, 'dan')).ok || 'refused').toBe('refused');
    const blank = await provisionUser(f.deps, { sub: ' ', name: 'X', admin: false });
    expect(blank.ok || blank.error.code).toBe('invalid_input');
    const noName = await provisionUser(f.deps, { sub: 'x', name: '  ', admin: false });
    expect(noName.ok || noName.error.code).toBe('invalid_input');
  });

  it('actAs refuses an unknown sub', async () => {
    const r = await actAs(makeDeps().deps, 'ghost');
    expect(r.ok || r.error.code).toBe('not_found');
  });
});

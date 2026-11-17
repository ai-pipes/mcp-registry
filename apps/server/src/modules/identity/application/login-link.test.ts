import { describe, expect, it } from 'vite-plus/test';
import { makeDeps } from '../../../../test/identity/fakes.ts';
import { sessionExpiry } from '../domain/session.ts';
import {
  bootstrapLocalAdmin,
  completeLinkLogin,
  issueLoginLink,
  LOCAL_ADMIN_SUB,
} from './login-link.ts';
import { pruneExpiredSessions } from './sessions.ts';
import { provisionUser, setDisabled } from './users.ts';

type F = ReturnType<typeof makeDeps>;

async function person(f: F, sub = 'alice', admin = false) {
  const r = await provisionUser(f.deps, { sub, name: sub, admin });
  if (!r.ok) throw r.error;
  return r.value;
}

async function issue(f: F, sub = 'alice') {
  const r = await issueLoginLink(f.deps, { sub });
  if (!r.ok) throw r.error;
  return r.value;
}

describe('issueLoginLink', () => {
  it('issues a link whose hash, not the code, is stored', async () => {
    const f = makeDeps();
    const user = await person(f);
    const { code, expiresAt } = await issue(f);
    expect(code.startsWith('ll_')).toBe(true);
    const stored = f.loginLinks.get(f.deps.sha256.hex(code));
    expect(stored?.codeHash).toBe(f.deps.sha256.hex(code));
    expect(stored?.userId).toBe(user.id);
    expect(expiresAt).toEqual(new Date(f.clock.now().getTime() + 15 * 60_000));
    expect(stored?.expiresAt).toEqual(expiresAt);
    const entry = f.auditEntries.find((e) => e.action === 'auth.link_issued');
    expect(entry?.target).toBe(user.id);
    expect(JSON.stringify(entry)).not.toContain(code);
  });

  it("issuing a new link invalidates the person's earlier link", async () => {
    const f = makeDeps();
    await person(f);
    const a = await issue(f);
    const b = await issue(f);
    const first = await completeLinkLogin(f.deps, { code: a.code, ip: '1.1.1.1' });
    expect(!first.ok && first.error.code).toBe('invalid_link');
    const second = await completeLinkLogin(f.deps, { code: b.code, ip: '1.1.1.1' });
    expect(second.ok).toBe(true);
  });

  it('refuses an unknown sub with not_found and a disabled person with disabled', async () => {
    const f = makeDeps();
    const unknown = await issueLoginLink(f.deps, { sub: 'nobody' });
    expect(!unknown.ok && unknown.error.code).toBe('not_found');
    const user = await person(f);
    const row = f.users.get(user.id);
    if (row) row.disabled = true;
    const disabled = await issueLoginLink(f.deps, { sub: 'alice' });
    expect(!disabled.ok && disabled.error.code).toBe('disabled');
    expect(f.loginLinks.size).toBe(0);
  });
});

describe('completeLinkLogin', () => {
  it('signs in once', async () => {
    const f = makeDeps();
    const user = await person(f);
    const { code } = await issue(f);
    f.clock.advance(60_000);
    const first = await completeLinkLogin(f.deps, { code, ip: '1.1.1.1' });
    expect(first.ok).toBe(true);
    expect(f.users.get(user.id)?.lastLoginAt).toEqual(f.clock.now());
    expect(f.users.get(user.id)?.lastLoginAt).not.toEqual(f.users.get(user.id)?.createdAt);
    if (!first.ok) return;
    const session = f.sessions.get(f.deps.sha256.hex(first.value.sessionId));
    expect(session?.userId).toBe(user.id);
    expect(session?.expiresAt).toEqual(sessionExpiry(f.clock.now()));
    const login = f.auditEntries.find((e) => e.action === 'auth.login');
    expect(login?.meta).toEqual({ via: 'link' });
    expect(JSON.stringify(f.auditEntries)).not.toContain(code);
    const second = await completeLinkLogin(f.deps, { code, ip: '1.1.1.1' });
    expect(!second.ok && second.error.code).toBe('invalid_link');
  });

  it('rejects an expired link as invalid_link', async () => {
    const f = makeDeps();
    await person(f);
    const { code } = await issue(f);
    f.clock.advance(15 * 60_000);
    const r = await completeLinkLogin(f.deps, { code, ip: '1.1.1.1' });
    expect(!r.ok && r.error.code).toBe('invalid_link');
    expect(f.sessions.size).toBe(0);
    const failed = f.auditEntries.filter((e) => e.action === 'auth.login_failed');
    expect(failed.map((e) => e.meta)).toEqual([{ reason: 'invalid_link' }]);
  });

  it('writes no audit for a malformed or unknown code', async () => {
    const f = makeDeps();
    await person(f);
    const before = f.auditEntries.length;
    await completeLinkLogin(f.deps, { code: 'sk_x', ip: '1.1.1.1' });
    await completeLinkLogin(f.deps, { code: 'll_unknown', ip: '1.1.1.1' });
    expect(f.auditEntries.length).toBe(before);
  });

  it('rejects a link of a person disabled after issue with disabled', async () => {
    const f = makeDeps();
    const user = await person(f);
    const { code } = await issue(f);
    const row = f.users.get(user.id);
    if (row) row.disabled = true;
    const r = await completeLinkLogin(f.deps, { code, ip: '1.1.1.1' });
    expect(!r.ok && r.error.code).toBe('disabled');
    expect(f.sessions.size).toBe(0);
    const failed = f.auditEntries.filter((e) => e.action === 'auth.login_failed');
    expect(failed.map((e) => e.meta)).toEqual([{ reason: 'disabled' }]);
  });

  it('counts failures per IP', async () => {
    const f = makeDeps();
    await person(f);
    const { code } = await issue(f);
    const hits: string[] = [];
    f.deps.linkLimiter.hit = (key) => {
      hits.push(key);
      return { allowed: false, retryAfterSec: 30 };
    };
    const bad = await completeLinkLogin(f.deps, { code: 'll_unknown', ip: '2.2.2.2' });
    expect(!bad.ok && bad.error.code).toBe('rate_limited');
    expect(!bad.ok && bad.error.retryAfterSec).toBe(30);
    // A valid code is still accepted and is not counted.
    const good = await completeLinkLogin(f.deps, { code, ip: '2.2.2.2' });
    expect(good.ok).toBe(true);
    expect(hits).toEqual(['2.2.2.2']);
  });

  it('rejects a malformed code without touching the store', async () => {
    const f = makeDeps();
    f.deps.loginLinks.consume = () => {
      throw new Error('store must not be touched');
    };
    const results = await Promise.all(
      ['sk_x', ''].map((code) => completeLinkLogin(f.deps, { code, ip: '1.1.1.1' })),
    );
    for (const r of results) expect(!r.ok && r.error.code).toBe('invalid_link');
  });

  it('rejects a link whose person no longer exists as invalid_link', async () => {
    const f = makeDeps();
    await person(f);
    const { code } = await issue(f);
    f.users.clear();
    const r = await completeLinkLogin(f.deps, { code, ip: '1.1.1.1' });
    expect(!r.ok && r.error.code).toBe('invalid_link');
    const failed = f.auditEntries.filter((e) => e.action === 'auth.login_failed');
    expect(failed.map((e) => e.meta)).toEqual([{ reason: 'invalid_link' }]);
  });
});

describe('bootstrapLocalAdmin', () => {
  it('creates local-admin as admin and a link when there are no people', async () => {
    const f = makeDeps();
    const r = await bootstrapLocalAdmin(f.deps);
    expect(r?.code.startsWith('ll_')).toBe(true);
    const [user] = [...f.users.values()];
    expect(f.users.size).toBe(1);
    expect(user?.oidcSub).toBe(LOCAL_ADMIN_SUB);
    expect(LOCAL_ADMIN_SUB).toBe('local-admin');
    expect(user?.name).toBe('Admin');
    expect(user?.role).toBe('admin');
    expect(f.loginLinks.size).toBe(1);
  });

  it('issues a new link when local-admin exists but never signed in', async () => {
    const f = makeDeps();
    const first = await bootstrapLocalAdmin(f.deps);
    const second = await bootstrapLocalAdmin(f.deps);
    expect(second?.code.startsWith('ll_')).toBe(true);
    expect(second?.code).not.toBe(first?.code);
    expect(f.users.size).toBe(1);
    expect(f.loginLinks.size).toBe(1);
  });

  it('returns null once local-admin has signed in', async () => {
    const f = makeDeps();
    const first = await bootstrapLocalAdmin(f.deps);
    if (!first) throw new Error('no link');
    f.clock.advance(60_000);
    const r = await completeLinkLogin(f.deps, { code: first.code, ip: '1.1.1.1' });
    expect(r.ok).toBe(true);
    expect(await bootstrapLocalAdmin(f.deps)).toBeNull();
  });

  it('throws when provisioning fails', async () => {
    const f = makeDeps();
    f.deps.users.upsertFromLogin = () => Promise.reject(new Error('db down'));
    await expect(bootstrapLocalAdmin(f.deps)).rejects.toThrow('db down');
  });

  it('returns null when any other person exists', async () => {
    const f = makeDeps();
    await person(f, 'alice');
    expect(await bootstrapLocalAdmin(f.deps)).toBeNull();
    expect(f.loginLinks.size).toBe(0);
  });

  it('returns null when local-admin is disabled', async () => {
    const f = makeDeps();
    await bootstrapLocalAdmin(f.deps);
    for (const u of f.users.values()) u.disabled = true;
    expect(await bootstrapLocalAdmin(f.deps)).toBeNull();
  });

  it('returns null when anyone exists', async () => {
    const f = makeDeps();
    const user = await person(f);
    const row = f.users.get(user.id);
    if (row) row.disabled = true;
    expect(await bootstrapLocalAdmin(f.deps)).toBeNull();
    expect(f.users.size).toBe(1);
    expect(f.loginLinks.size).toBe(0);
  });
});

describe('login link housekeeping', () => {
  it("setDisabled removes the person's pending links", async () => {
    const f = makeDeps();
    const admin = await person(f, 'root', true);
    const user = await person(f);
    await issue(f);
    await issue(f, 'root');
    const r = await setDisabled(f.deps, {
      principal: { userId: admin.id, name: 'root', role: 'admin', groups: [], via: 'session' },
      userId: user.id,
      disabled: true,
    });
    expect(r.ok).toBe(true);
    expect([...f.loginLinks.values()].map((l) => l.userId)).toEqual([admin.id]);
  });

  it('pruneExpiredSessions also deletes expired links', async () => {
    const f = makeDeps();
    await person(f);
    await issue(f);
    await pruneExpiredSessions(f.deps);
    expect(f.loginLinks.size).toBe(1);
    f.clock.advance(15 * 60_000);
    await pruneExpiredSessions(f.deps);
    expect(f.loginLinks.size).toBe(0);
  });
});

import { describe, expect, it } from 'vite-plus/test';
import type { OidcClaims, User } from '../domain/user.ts';
import { completeOidcLogin, startOidcLogin } from './oidc-login.ts';
import { authenticateSession } from './sessions.ts';
import { makeDeps, startedUrl } from '../../../../test/identity/fakes.ts';

const claims = (over: Partial<OidcClaims> = {}): OidcClaims => ({
  sub: 'sub-1',
  name: 'Anna',
  email: 'anna@example.com',
  groups: ['mcp-users'],
  ...over,
});

function onlyUser(f: ReturnType<typeof makeDeps>): User {
  const user = [...f.users.values()][0];
  if (!user) throw new Error('no user');
  return user;
}

async function login(f: ReturnType<typeof makeDeps>, c: OidcClaims, returnTo = '/servers') {
  f.oidc.next = c;
  const url = startedUrl(await startOidcLogin(f.deps, { returnTo }));
  const state = url.searchParams.get('state') ?? '';
  return { state, result: await completeOidcLogin(f.deps, { state, code: 'code-1' }) };
}

describe('an unreachable identity provider', () => {
  it('start: idp_unavailable and nothing remembered', async () => {
    const f = makeDeps();
    f.oidc.unavailable = true;
    const r = await startOidcLogin(f.deps, { returnTo: '/' });
    expect(r.ok || r.error.code).toBe('idp_unavailable');
    expect(f.pendingLogins.size).toBe(0);
  });

  it('callback: idp_unavailable, not audited as a rejected login', async () => {
    const f = makeDeps();
    const url = startedUrl(await startOidcLogin(f.deps, { returnTo: '/' }));
    f.oidc.unavailable = true;
    const r = await completeOidcLogin(f.deps, {
      state: url.searchParams.get('state') ?? '',
      code: 'c',
    });
    expect(r.ok || r.error.code).toBe('idp_unavailable');
    expect(f.auditEntries).toEqual([]);
  });

  it('a start error that is not about the provider propagates', async () => {
    const f = makeDeps();
    f.deps.oidc = { ...f.deps.oidc, authorizationUrl: () => Promise.reject(new Error('bug')) };
    await expect(startOidcLogin(f.deps, { returnTo: '/' })).rejects.toThrow('bug');
  });
});

describe('startOidcLogin', () => {
  it('stores a pending login and sends a PKCE S256 challenge, state and nonce to the provider', async () => {
    const f = makeDeps();
    const url = startedUrl(await startOidcLogin(f.deps, { returnTo: '/tokens' }));
    const state = url.searchParams.get('state');
    expect(state).toBeTruthy();
    expect(url.searchParams.get('nonce')).toBeTruthy();
    const pending = [...f.pendingLogins.values()][0];
    expect(pending?.returnTo).toBe('/tokens');
    expect(url.searchParams.get('code_challenge')).toBe(
      f.deps.sha256.base64url(pending!.codeVerifier),
    );
  });

  it('turns an off-site returnTo into /', async () => {
    const f = makeDeps();
    startedUrl(await startOidcLogin(f.deps, { returnTo: 'https://evil.example/' }));
    startedUrl(await startOidcLogin(f.deps, { returnTo: '//evil' }));
    expect([...f.pendingLogins.values()].map((p) => p.returnTo)).toEqual(['/', '/']);
  });
});

describe('completeOidcLogin', () => {
  it('creates a member for the login group and opens a session', async () => {
    const f = makeDeps();
    const { result } = await login(f, claims());
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.value.returnTo).toBe('/servers');
    const principal = await authenticateSession(f.deps, { sessionId: result.value.sessionId });
    expect(principal).toMatchObject({ role: 'member', groups: ['mcp-users'], via: 'session' });
    expect(f.users.size).toBe(1);
  });

  it('updates groups and role on the next login of the same sub', async () => {
    const f = makeDeps();
    await login(f, claims());
    const first = onlyUser(f);
    await login(f, claims({ groups: ['mcp-users', 'mcp-admins'] }));
    expect(f.users.size).toBe(1);
    const again = onlyUser(f);
    expect(again.id).toBe(first.id);
    expect(again.role).toBe('admin');
    expect(again.groups).toEqual(['mcp-users', 'mcp-admins']);
  });

  it('refuses groups outside the policy and creates nobody', async () => {
    const f = makeDeps();
    const { result } = await login(f, claims({ groups: ['family'] }));
    expect(result).toMatchObject({ ok: false, error: { code: 'not_allowed' } });
    expect(f.users.size).toBe(0);
    expect(f.sessions.size).toBe(0);
  });

  it('treats an absent groups claim as not allowed', async () => {
    const f = makeDeps();
    const { result } = await login(f, claims({ groups: [] }));
    expect(result).toMatchObject({ ok: false, error: { code: 'not_allowed' } });
  });

  it('refuses a disabled user without creating a session', async () => {
    const f = makeDeps();
    await login(f, claims());
    const user = onlyUser(f);
    user.disabled = true;
    f.sessions.clear();
    const { result } = await login(f, claims());
    expect(result).toMatchObject({ ok: false, error: { code: 'disabled' } });
    expect(f.sessions.size).toBe(0);
  });

  it('passes the callback iss on to the provider', async () => {
    const f = makeDeps();
    f.oidc.next = claims();
    const url = startedUrl(await startOidcLogin(f.deps, { returnTo: '/' }));
    await completeOidcLogin(f.deps, {
      state: url.searchParams.get('state') ?? '',
      code: 'c',
      iss: 'https://id.example',
    });
    expect(f.oidc.lastExchange).toEqual({
      code: 'c',
      codeVerifier: expect.any(String),
      nonce: expect.any(String),
      iss: 'https://id.example',
    });
  });

  it('rejects an unknown state', async () => {
    const f = makeDeps();
    const r = await completeOidcLogin(f.deps, { state: 'nope', code: 'c' });
    expect(r).toMatchObject({ ok: false, error: { code: 'invalid_state' } });
  });

  it('rejects a state older than 10 minutes', async () => {
    const f = makeDeps();
    f.oidc.next = claims();
    const url = startedUrl(await startOidcLogin(f.deps, { returnTo: '/' }));
    f.clock.advance(10 * 60_000);
    const r = await completeOidcLogin(f.deps, { state: url.searchParams.get('state')!, code: 'c' });
    expect(r).toMatchObject({ ok: false, error: { code: 'invalid_state' } });
  });

  it('accepts a state only once', async () => {
    const f = makeDeps();
    const { state, result } = await login(f, claims());
    expect(result.ok).toBe(true);
    const replay = await completeOidcLogin(f.deps, { state, code: 'c' });
    expect(replay).toMatchObject({ ok: false, error: { code: 'invalid_state' } });
  });

  it('reports a failed code exchange as invalid_state and audits it', async () => {
    const f = makeDeps();
    f.oidc.next = null;
    const url = startedUrl(await startOidcLogin(f.deps, { returnTo: '/' }));
    const r = await completeOidcLogin(f.deps, { state: url.searchParams.get('state')!, code: 'c' });
    expect(r).toMatchObject({ ok: false, error: { code: 'invalid_state' } });
    expect(f.auditEntries.map((e) => e.action)).toContain('auth.login_failed');
  });
});

import { beforeEach, describe, expect, it } from 'vite-plus/test';
import type { Result } from '../../../shared-kernel/index.ts';
import { makeConnectionsDeps } from '../../../../test/fakes/in-memory-connections.ts';
import { Connection } from '../domain/connection.ts';
import { serverFingerprint } from '../domain/server-fingerprint.ts';
import { completeConnect, disconnect, savePat, startConnect } from './connect.ts';
import { listConnections } from './list.ts';
import { type ConnectionsDeps, type Principal, TokenEndpointError } from './ports.ts';
import { markExpired, resolveCredential } from './resolve.ts';
import { createSingleFlight } from './single-flight.ts';

const t0 = new Date('2026-10-30T12:00:00.000Z');
const at = (ms: number) => new Date(t0.getTime() + ms);
const who = (userId: string, ...groups: string[]): Principal => ({
  userId,
  name: userId,
  role: 'member',
  groups,
  via: 'session',
});
const alice = who('alice', 'dev');
const bob = who('bob', 'dev');

const OAUTH = {
  authorizeUrl: 'https://git.example.com/login/oauth/authorize',
  tokenUrl: 'https://git.example.com/login/oauth/access_token',
  clientId: 'cid',
  scopes: [],
};
const GITEA_URL = 'https://mcp.example.com/gitea';
const GITEA_FP = serverFingerprint({ url: GITEA_URL, authKind: 'oauth_user', oauth: OAUTH });

let now: { value: Date };
let h: ReturnType<typeof makeConnectionsDeps>;
beforeEach(() => {
  now = { value: t0 };
  h = makeConnectionsDeps(now);
  h.directory.servers.push(
    {
      slug: 'gitea',
      name: 'Gitea',
      authKind: 'oauth_user',
      groups: [],
      url: GITEA_URL,
      oauth: OAUTH,
      clientSecret: 'client-secret',
    },
    {
      slug: 'yt',
      name: 'YouTrack',
      authKind: 'pat_user',
      groups: [],
      url: 'https://mcp.example.com/yt',
      oauth: null,
      clientSecret: null,
    },
    {
      slug: 'secret',
      name: 'Secret',
      authKind: 'oauth_user',
      groups: ['ops'],
      url: 'https://mcp.example.com/secret',
      oauth: OAUTH,
      clientSecret: 'client-secret',
    },
  );
});

const code = (r: Result<unknown, string | { code: string }>) =>
  r.ok ? 'ok' : typeof r.error === 'string' ? r.error : r.error.code;

/** A promise the test resolves when it wants to: holds refresh open so callers overlap. */
function gate(): { promise: Promise<void>; resolve: () => void } {
  const resolvers: (() => void)[] = [];
  const promise = new Promise<void>((r) => resolvers.push(r));
  return { promise, resolve: () => resolvers.forEach((r) => r()) };
}

/** Changes gitea in the fake catalog, as an admin would. */
function changeGitea(change: (s: NonNullable<ReturnType<typeof h.directory.find>>) => void): void {
  const gitea = h.directory.find('gitea');
  if (gitea) change(gitea);
}

/** Points gitea's upstream URL elsewhere. */
function moveGitea(): void {
  changeGitea((g) => (g.url = 'https://evil.example.com/mcp'));
}

/** Gives gitea's OAuth client a revoke URL (not part of the fingerprint). */
function withRevokeUrl(): void {
  changeGitea((g) => (g.oauth = { ...OAUTH, revokeUrl: 'https://git.example.com/revoke' }));
}

const bytes = (s: string) => new TextEncoder().encode(s);
const contains = (hay: Uint8Array, needle: string) =>
  Buffer.from(hay).includes(Buffer.from(needle, 'utf8'));

/** Gitea's config fingerprint as the fake catalog has it now. */
function giteaFingerprint(): string {
  const gitea = h.directory.find('gitea');
  return gitea ? serverFingerprint(gitea) : GITEA_FP;
}

/** Stores an OAuth connection for alice/gitea as if she had connected earlier. */
function seedOAuth(expiresAt: Date | null, refresh: string | null = 'R1', user = 'alice') {
  const c = Connection.connectOAuth({
    userId: user,
    server: 'gitea',
    access: h.deps.secretBox.seal('A1'),
    refresh: refresh === null ? null : h.deps.secretBox.seal(refresh),
    expiresAt,
    issuedFor: giteaFingerprint(),
    revokeUrl: h.directory.find('gitea')?.oauth?.revokeUrl ?? null,
    now: t0,
  });
  h.store.rows.set(`${user}\u0000gitea`, c.snapshot());
}

async function connectAlice(deps: ConnectionsDeps = h.deps): Promise<void> {
  const url = await startConnect(deps, { principal: alice, server: 'gitea' });
  if (!url.ok) throw url.error;
  const state = url.value.searchParams.get('state') ?? '';
  const r = await completeConnect(deps, { principal: alice, state, code: 'the-code' });
  if (!r.ok) throw r.error;
}

describe('startConnect', () => {
  it('remembers the attempt for 10 min and sends PKCE S256 with our redirect URI', async () => {
    const r = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    const state = r.value.searchParams.get('state') ?? '';
    const pending = h.pending.rows.get(state);
    expect(pending).toMatchObject({ userId: 'alice', server: 'gitea', expiresAt: at(600_000) });
    expect(r.value.searchParams.get('code_challenge')).toBe(`S256(${pending?.codeVerifier})`);
    expect(r.value.searchParams.get('redirect_uri')).toBe(h.deps.redirectUri);
  });

  it('refuses a PAT server, a server the person may not use, and an unknown one', async () => {
    expect(code(await startConnect(h.deps, { principal: alice, server: 'yt' }))).toBe(
      'invalid_auth_kind',
    );
    expect(code(await startConnect(h.deps, { principal: alice, server: 'secret' }))).toBe(
      'not_found',
    );
    expect(code(await startConnect(h.deps, { principal: alice, server: 'nope' }))).toBe(
      'not_found',
    );
    expect(h.pending.rows.size).toBe(0);
  });

  it('refuses an oauth_user server whose OAuth client is gone', async () => {
    h.directory.servers.push({
      slug: 'bare',
      name: 'Bare',
      authKind: 'oauth_user',
      groups: [],
      url: 'https://mcp.example.com/bare',
      oauth: null,
      clientSecret: null,
    });
    expect(code(await startConnect(h.deps, { principal: alice, server: 'bare' }))).toBe(
      'not_found',
    );
  });
});

describe('completeConnect', () => {
  it('exchanges the code with the verifier and stores the tokens sealed', async () => {
    await connectAlice();
    expect(h.oauth.exchangeCalls).toEqual([
      {
        code: 'the-code',
        codeVerifier: expect.any(String),
        redirectUri: h.deps.redirectUri,
        secret: 'client-secret',
      },
    ]);
    const row = h.store.rows.get('alice\u0000gitea');
    expect(row).toMatchObject({ kind: 'oauth', status: 'ready' });
    expect(contains(row?.access ?? new Uint8Array(), 'A1')).toBe(false);
    expect(contains(row?.refresh ?? new Uint8Array(), 'R1')).toBe(false);
    expect(h.audit).toContainEqual(
      expect.objectContaining({ actorId: 'alice', action: 'connection.connect', target: 'gitea' }),
    );
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: true,
      value: 'A1',
    });
  });

  it('a foreign or expired state is invalid_state even for a server the caller cannot use', async () => {
    const url = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!url.ok) throw url.error;
    changeGitea((g) => (g.groups = ['dev']));
    const outsider = who('carol', 'ops');
    const state = url.value.searchParams.get('state') ?? '';
    expect(code(await completeConnect(h.deps, { principal: outsider, state, code: 'c' }))).toBe(
      'invalid_state',
    );
    const late = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!late.ok) throw late.error;
    changeGitea((g) => (g.groups = ['ops']));
    now.value = at(600_000);
    const lateState = late.value.searchParams.get('state') ?? '';
    expect(
      code(await completeConnect(h.deps, { principal: alice, state: lateState, code: 'c' })),
    ).toBe('invalid_state');
  });

  it("someone else's state fails and saves nothing; a used state fails again", async () => {
    const url = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!url.ok) throw url.error;
    const state = url.value.searchParams.get('state') ?? '';
    expect(code(await completeConnect(h.deps, { principal: bob, state, code: 'c' }))).toBe(
      'invalid_state',
    );
    expect(h.oauth.exchangeCalls).toEqual([]);
    expect(h.store.rows.size).toBe(0);
    // The attempt is spent: not even its owner can finish it now.
    expect(code(await completeConnect(h.deps, { principal: alice, state, code: 'c' }))).toBe(
      'invalid_state',
    );

    await connectAlice();
    const again = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!again.ok) throw again.error;
    const s2 = again.value.searchParams.get('state') ?? '';
    expect(code(await completeConnect(h.deps, { principal: alice, state: s2, code: 'c' }))).toBe(
      'ok',
    );
    expect(code(await completeConnect(h.deps, { principal: alice, state: s2, code: 'c' }))).toBe(
      'invalid_state',
    );
  });

  it('an attempt older than 10 min fails', async () => {
    const url = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!url.ok) throw url.error;
    now.value = at(600_000);
    const state = url.value.searchParams.get('state') ?? '';
    expect(code(await completeConnect(h.deps, { principal: alice, state, code: 'c' }))).toBe(
      'invalid_state',
    );
  });

  it('the provider refusing the code fails without storing anything', async () => {
    h.oauth.exchangeFails = true;
    const url = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!url.ok) throw url.error;
    const state = url.value.searchParams.get('state') ?? '';
    expect(code(await completeConnect(h.deps, { principal: alice, state, code: 'c' }))).toBe(
      'exchange_failed',
    );
    expect(h.store.rows.size).toBe(0);
  });

  it('access lost between start and callback fails', async () => {
    const url = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!url.ok) throw url.error;
    changeGitea((g) => (g.disabled = true));
    const state = url.value.searchParams.get('state') ?? '';
    expect(code(await completeConnect(h.deps, { principal: alice, state, code: 'c' }))).toBe(
      'not_found',
    );
    expect(h.oauth.exchangeCalls).toEqual([]);
  });
});

describe('savePat', () => {
  it('stores a PAT sealed for a pat_user server', async () => {
    const r = await savePat(h.deps, { principal: alice, server: 'yt', token: '  perm:xyz  ' });
    expect(code(r)).toBe('ok');
    const row = h.store.rows.get('alice\u0000yt');
    expect(row).toMatchObject({ kind: 'pat', status: 'ready', refresh: null, expiresAt: null });
    expect(contains(row?.access ?? new Uint8Array(), 'perm:xyz')).toBe(false);
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'yt' })).toEqual({
      ok: true,
      value: 'perm:xyz',
    });
    expect(h.audit).toContainEqual(
      expect.objectContaining({ actorId: 'alice', action: 'connection.pat_saved', target: 'yt' }),
    );
  });

  it('refuses an oauth_user server, an unusable server and an empty token', async () => {
    expect(code(await savePat(h.deps, { principal: alice, server: 'gitea', token: 't' }))).toBe(
      'invalid_auth_kind',
    );
    expect(code(await savePat(h.deps, { principal: alice, server: 'secret', token: 't' }))).toBe(
      'not_found',
    );
    expect(code(await savePat(h.deps, { principal: alice, server: 'yt', token: '  ' }))).toBe(
      'invalid_pat',
    );
    expect(h.store.rows.size).toBe(0);
  });
});

describe('resolveCredential', () => {
  it('no connection → needs_connection', async () => {
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'needs_connection',
    });
  });

  it('30 s before expiry: one refresh, the new token is returned and stored sealed', async () => {
    seedOAuth(at(30_000));
    h.oauth.next = { accessToken: 'A2', refreshToken: null, expiresAt: at(3_600_000) };
    const r = await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' });
    expect(r).toEqual({ ok: true, value: 'A2' });
    expect(h.oauth.refreshCalls).toEqual(['R1']);
    const row = h.store.rows.get('alice\u0000gitea');
    expect(contains(row?.access ?? new Uint8Array(), 'A2')).toBe(false);
    expect(h.deps.secretBox.open(row?.access ?? new Uint8Array())).toBe('A2');
    // Hub and Gitea do not rotate by default: the old refresh token is kept.
    expect(h.deps.secretBox.open(row?.refresh ?? new Uint8Array())).toBe('R1');
    expect(row?.expiresAt).toEqual(at(3_600_000));
    // Fresh now: no second refresh.
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual(r);
    expect(h.oauth.refreshCalls).toHaveLength(1);
  });

  it('a fresh token is used without refreshing', async () => {
    seedOAuth(at(3_600_000));
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: true,
      value: 'A1',
    });
    expect(h.oauth.refreshCalls).toEqual([]);
  });

  it('Review Focus 2: two parallel calls on an expired token → exactly one refresh', async () => {
    seedOAuth(at(-1000));
    const g = gate();
    h.oauth.refreshGate = g.promise;
    h.oauth.next = { accessToken: 'A2', refreshToken: 'R2', expiresAt: at(3_600_000) };
    const calls = [
      resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }),
      resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }),
    ];
    await new Promise((r) => setTimeout(r, 10));
    g.resolve();
    expect(await Promise.all(calls)).toEqual([
      { ok: true, value: 'A2' },
      { ok: true, value: 'A2' },
    ]);
    expect(h.oauth.refreshCalls).toEqual(['R1']);
    expect(h.deps.secretBox.open(h.store.rows.get('alice\u0000gitea')?.refresh ?? bytes(''))).toBe(
      'R2',
    );
  });

  it('two processes (separate single-flights): the row lock still lets only one refresh', async () => {
    seedOAuth(at(-1000));
    const g = gate();
    h.oauth.refreshGate = g.promise;
    h.oauth.next = { accessToken: 'A2', refreshToken: 'R2', expiresAt: at(3_600_000) };
    const replica: ConnectionsDeps = { ...h.deps, flights: createSingleFlight() };
    const calls = [
      resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }),
      resolveCredential(replica, { userId: 'alice', server: 'gitea' }),
    ];
    await new Promise((r) => setTimeout(r, 10));
    g.resolve();
    expect(await Promise.all(calls)).toEqual([
      { ok: true, value: 'A2' },
      { ok: true, value: 'A2' },
    ]);
    expect(h.oauth.refreshCalls).toEqual(['R1']);
  });

  it('forceRefresh refreshes a fresh token; a forced call that lost the race takes the new one', async () => {
    seedOAuth(at(3_600_000));
    h.oauth.next = { accessToken: 'A2', refreshToken: null, expiresAt: at(3_600_000) };
    expect(
      await resolveCredential(h.deps, { userId: 'alice', server: 'gitea', forceRefresh: true }),
    ).toEqual({ ok: true, value: 'A2' });
    expect(h.oauth.refreshCalls).toEqual(['R1']);

    // Another process: it read A2's row before, then waits on the lock while we refresh again.
    const g = gate();
    h.oauth.refreshGate = g.promise;
    h.oauth.next = { accessToken: 'A3', refreshToken: null, expiresAt: at(3_600_000) };
    now.value = at(1000);
    const replica: ConnectionsDeps = { ...h.deps, flights: createSingleFlight() };
    const calls = [
      resolveCredential(h.deps, { userId: 'alice', server: 'gitea', forceRefresh: true }),
      resolveCredential(replica, { userId: 'alice', server: 'gitea', forceRefresh: true }),
    ];
    await new Promise((r) => setTimeout(r, 10));
    g.resolve();
    expect(await Promise.all(calls)).toEqual([
      { ok: true, value: 'A3' },
      { ok: true, value: 'A3' },
    ]);
    expect(h.oauth.refreshCalls).toEqual(['R1', 'R1']);
  });

  it('refresh fails → expired; the next call fails without trying again', async () => {
    seedOAuth(at(-1000));
    h.oauth.refreshFails = true;
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'expired',
    });
    expect(h.store.rows.get('alice\u0000gitea')?.status).toBe('expired');
    expect(h.audit).toContainEqual(
      expect.objectContaining({ actorId: 'alice', action: 'connection.expired', target: 'gitea' }),
    );
    h.oauth.refreshFails = false;
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'expired',
    });
    expect(h.oauth.refreshCalls).toHaveLength(1);
  });

  it('expired without a refresh token → expired, no refresh attempted', async () => {
    seedOAuth(at(-1), null);
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'expired',
    });
    expect(h.store.rows.get('alice\u0000gitea')?.status).toBe('expired');
    expect(h.oauth.refreshCalls).toEqual([]);
  });

  it('the server is disabled: needs_connection, the connection is left alone', async () => {
    seedOAuth(at(-1));
    changeGitea((g) => (g.disabled = true));
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'needs_connection',
    });
    expect(h.store.rows.get('alice\u0000gitea')?.status).toBe('ready');
  });

  it('the connection disappeared while waiting for the lock → needs_connection', async () => {
    seedOAuth(at(-1));
    const original = h.store.withLock.bind(h.store);
    h.store.withLock = async (userId, server, fn) => {
      await h.store.delete(userId, server);
      return original(userId, server, fn);
    };
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'needs_connection',
    });
  });

  it('connections are per person', async () => {
    seedOAuth(at(3_600_000));
    expect(code(await resolveCredential(h.deps, { userId: 'bob', server: 'gitea' }))).toBe(
      'needs_connection',
    );
  });
});

describe('refresh failures', () => {
  it('no answer from the provider (timeout, network): unavailable, nothing saved, retried later', async () => {
    seedOAuth(at(-1000));
    const before = h.store.rows.get('alice\u0000gitea');
    h.oauth.refreshThrows = new Error('fetch failed');
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'unavailable',
    });
    expect(h.store.rows.get('alice\u0000gitea')).toEqual(before);
    expect(h.audit.filter((a) => a.action === 'connection.expired')).toEqual([]);
    h.oauth.refreshThrows = null;
    h.oauth.next = { accessToken: 'A2', refreshToken: null, expiresAt: at(3_600_000) };
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: true,
      value: 'A2',
    });
    expect(h.oauth.refreshCalls).toEqual(['R1', 'R1']);
  });

  it('provider down inside the refresh-ahead window: the still-valid token is used', async () => {
    seedOAuth(at(30_000));
    h.oauth.refreshThrows = new TokenEndpointError(503, null);
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: true,
      value: 'A1',
    });
    // Once it has run out, or when upstream has just rejected it, there is nothing to fall back on.
    now.value = at(30_000);
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }))).toBe(
      'unavailable',
    );
    now.value = t0;
    expect(
      code(
        await resolveCredential(h.deps, { userId: 'alice', server: 'gitea', forceRefresh: true }),
      ),
    ).toBe('unavailable');
    expect(h.store.rows.get('alice\u0000gitea')?.status).toBe('ready');
  });

  it('a wrong client secret (invalid_client) is unavailable: nothing saved, nobody expires', async () => {
    seedOAuth(at(-1000));
    const before = h.store.rows.get('alice\u0000gitea');
    const attempt = async (failure: TokenEndpointError) => {
      h.oauth.refreshThrows = failure;
      return code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }));
    };
    expect(await attempt(new TokenEndpointError(401, 'invalid_client'))).toBe('unavailable');
    expect(await attempt(new TokenEndpointError(400, 'invalid_client'))).toBe('unavailable');
    expect(await attempt(new TokenEndpointError(401, 'unauthorized_client'))).toBe('unavailable');
    expect(h.store.rows.get('alice\u0000gitea')).toEqual(before);
    expect(h.audit.filter((a) => a.action === 'connection.expired')).toEqual([]);
    // Once the admin fixes the secret, the same grant works again.
    h.oauth.refreshThrows = null;
    h.oauth.next = { accessToken: 'A2', refreshToken: null, expiresAt: at(3_600_000) };
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: true,
      value: 'A2',
    });
  });

  it('a 5xx without an OAuth error is unavailable; 401 or invalid_grant expire', async () => {
    seedOAuth(at(-1000));
    h.oauth.refreshThrows = new TokenEndpointError(503, null);
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }))).toBe(
      'unavailable',
    );
    expect(h.store.rows.get('alice\u0000gitea')?.status).toBe('ready');
    h.oauth.refreshThrows = new TokenEndpointError(401, null);
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }))).toBe(
      'expired',
    );
    expect(h.store.rows.get('alice\u0000gitea')?.status).toBe('expired');
    expect(h.audit).toContainEqual(
      expect.objectContaining({
        action: 'connection.expired',
        meta: { reason: 'refresh_rejected' },
      }),
    );
  });
});

describe('the server config changed after connecting', () => {
  it('the token is not handed out: the connection expires, nothing is refreshed', async () => {
    seedOAuth(at(3_600_000));
    moveGitea();
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' })).toEqual({
      ok: false,
      error: 'expired',
    });
    expect(h.store.rows.get('alice\u0000gitea')?.status).toBe('expired');
    expect(h.oauth.refreshCalls).toEqual([]);
    expect(h.audit).toContainEqual(
      expect.objectContaining({
        actorId: 'alice',
        action: 'connection.expired',
        target: 'gitea',
        meta: { reason: 'config_changed' },
      }),
    );
  });

  it('a new token URL or client id, or a PAT server switched to OAuth, count the same', async () => {
    seedOAuth(at(3_600_000));
    changeGitea((g) => (g.oauth = { ...OAUTH, tokenUrl: 'https://evil.example.com/token' }));
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }))).toBe(
      'expired',
    );
    await savePat(h.deps, { principal: alice, server: 'yt', token: 'p' });
    const yt = h.directory.find('yt');
    if (yt) {
      yt.authKind = 'oauth_user';
      yt.oauth = OAUTH;
    }
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'yt' }))).toBe(
      'expired',
    );
  });

  it('listConnections shows it as expired', async () => {
    await connectAlice();
    moveGitea();
    expect((await listConnections(h.deps, { principal: alice }))[0]).toMatchObject({
      server: 'gitea',
      status: 'expired',
    });
  });

  it('a changed revoke URL alone: still ready and usable; disconnect sends nothing to the new URL', async () => {
    withRevokeUrl();
    await connectAlice();
    changeGitea((g) => {
      if (g.oauth) g.oauth = { ...g.oauth, revokeUrl: 'https://evil.example.com/revoke' };
    });
    expect((await listConnections(h.deps, { principal: alice }))[0]?.status).toBe('ready');
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }))).toBe('ok');
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.store.rows.size).toBe(0);
    expect(h.oauth.revokeCalls).toEqual([]);
  });

  it('turning revocation on after connecting expires nobody; that connection is not revoked', async () => {
    await connectAlice();
    withRevokeUrl();
    expect((await listConnections(h.deps, { principal: alice }))[0]?.status).toBe('ready');
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }))).toBe('ok');
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.store.rows.size).toBe(0);
    expect(h.oauth.revokeCalls).toEqual([]);
    // Connecting again binds the revoke URL now in place.
    await connectAlice();
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.oauth.revokeCalls).toEqual(['R1']);
  });

  it('disconnect deletes it but sends no revoke to the changed provider', async () => {
    withRevokeUrl();
    await connectAlice();
    changeGitea((g) => {
      if (g.oauth) g.oauth = { ...g.oauth, tokenUrl: 'https://evil.example.com/token' };
    });
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.store.rows.size).toBe(0);
    expect(h.oauth.revokeCalls).toEqual([]);
  });

  it('a connect started before the change is not finished with the new provider', async () => {
    const url = await startConnect(h.deps, { principal: alice, server: 'gitea' });
    if (!url.ok) throw url.error;
    changeGitea((g) => (g.oauth = { ...OAUTH, tokenUrl: 'https://evil.example.com/token' }));
    const state = url.value.searchParams.get('state') ?? '';
    expect(code(await completeConnect(h.deps, { principal: alice, state, code: 'c' }))).toBe(
      'invalid_state',
    );
    expect(h.oauth.exchangeCalls).toEqual([]);
  });

  it('an OAuth connection bound to a server config without an OAuth client → needs_connection', async () => {
    changeGitea((g) => (g.oauth = null));
    const c = Connection.connectOAuth({
      userId: 'alice',
      server: 'gitea',
      access: h.deps.secretBox.seal('A1'),
      refresh: h.deps.secretBox.seal('R1'),
      expiresAt: at(-1),
      issuedFor: serverFingerprint({ url: GITEA_URL, authKind: 'oauth_user', oauth: null }),
      revokeUrl: null,
      now: t0,
    });
    h.store.rows.set('alice\u0000gitea', c.snapshot());
    expect(code(await resolveCredential(h.deps, { userId: 'alice', server: 'gitea' }))).toBe(
      'needs_connection',
    );
    expect(h.oauth.refreshCalls).toEqual([]);
  });
});

describe('under the connection lock', () => {
  it('nothing but the locked row and the provider is used: catalog and audit stay outside', async () => {
    let inLock = false;
    const original = h.store.withLock.bind(h.store);
    h.store.withLock = (userId, server, fn) =>
      original(userId, server, async (current, save) => {
        inLock = true;
        try {
          return await fn(current, save);
        } finally {
          inLock = false;
        }
      });
    const touched: string[] = [];
    const guard =
      <A extends unknown[], R>(name: string, f: (...a: A) => R) =>
      (...a: A): R => {
        if (inLock) touched.push(name);
        return f(...a);
      };
    const dir = h.deps.servers;
    const audit = h.deps.audit;
    const deps: ConnectionsDeps = {
      ...h.deps,
      servers: {
        personalServersFor: guard('personalServersFor', dir.personalServersFor.bind(dir)),
        config: guard('config', dir.config.bind(dir)),
        clientSecret: guard('clientSecret', dir.clientSecret.bind(dir)),
      },
      audit: { record: guard('audit', audit.record.bind(audit)) },
    };

    seedOAuth(at(-1000));
    expect(code(await resolveCredential(deps, { userId: 'alice', server: 'gitea' }))).toBe('ok');
    seedOAuth(at(-1000));
    h.oauth.refreshFails = true;
    expect(code(await resolveCredential(deps, { userId: 'alice', server: 'gitea' }))).toBe(
      'expired',
    );
    seedOAuth(at(-1), null);
    expect(code(await resolveCredential(deps, { userId: 'alice', server: 'gitea' }))).toBe(
      'expired',
    );
    await savePat(deps, { principal: alice, server: 'yt', token: 'p' });
    await markExpired(deps, { userId: 'alice', server: 'yt' });
    await connectAlice(deps);
    await disconnect(deps, { principal: alice, server: 'gitea' });
    expect(touched).toEqual([]);
    expect(h.audit.filter((a) => a.action === 'connection.expired')).toHaveLength(3);
  });
});

describe('markExpired', () => {
  it('turns a PAT rejected by upstream into expired; unknown connections are ignored', async () => {
    await savePat(h.deps, { principal: alice, server: 'yt', token: 'p' });
    await markExpired(h.deps, { userId: 'alice', server: 'yt' });
    expect(await resolveCredential(h.deps, { userId: 'alice', server: 'yt' })).toEqual({
      ok: false,
      error: 'expired',
    });
    await markExpired(h.deps, { userId: 'bob', server: 'yt' });
    expect(h.store.rows.has('bob\u0000yt')).toBe(false);
  });
});

describe('listConnections', () => {
  it('every usable personal server with ready / expired / missing', async () => {
    await connectAlice();
    await savePat(h.deps, { principal: alice, server: 'yt', token: 'p' });
    await markExpired(h.deps, { userId: 'alice', server: 'yt' });
    expect(await listConnections(h.deps, { principal: alice })).toEqual([
      { server: 'gitea', name: 'Gitea', authKind: 'oauth_user', status: 'ready' },
      { server: 'yt', name: 'YouTrack', authKind: 'pat_user', status: 'expired' },
    ]);
    expect(await listConnections(h.deps, { principal: bob })).toEqual([
      { server: 'gitea', name: 'Gitea', authKind: 'oauth_user', status: 'missing' },
      { server: 'yt', name: 'YouTrack', authKind: 'pat_user', status: 'missing' },
    ]);
  });
});

describe('disconnect', () => {
  it('deletes the connection and revokes the refresh token when a revoke URL is set', async () => {
    withRevokeUrl();
    await connectAlice();
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.store.rows.size).toBe(0);
    expect(h.oauth.revokeCalls).toEqual(['R1']);
    expect(h.audit).toContainEqual(
      expect.objectContaining({
        actorId: 'alice',
        action: 'connection.disconnect',
        target: 'gitea',
      }),
    );
  });

  it('a failing revoke is ignored; no revoke URL, a PAT or nothing stored → no revoke', async () => {
    withRevokeUrl();
    h.oauth.revokeFails = true;
    await connectAlice();
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.store.rows.size).toBe(0);
    expect(h.oauth.revokeCalls).toEqual(['R1']);

    h.oauth.revokeCalls = [];
    seedOAuth(at(3_600_000), null);
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.oauth.revokeCalls).toEqual(['A1']);

    h.oauth.revokeCalls = [];
    await savePat(h.deps, { principal: alice, server: 'yt', token: 'p' });
    await disconnect(h.deps, { principal: alice, server: 'yt' });
    await disconnect(h.deps, { principal: alice, server: 'nothing' });
    expect(h.oauth.revokeCalls).toEqual([]);
  });

  it('without a revoke URL nothing is revoked', async () => {
    await connectAlice();
    await disconnect(h.deps, { principal: alice, server: 'gitea' });
    expect(h.oauth.revokeCalls).toEqual([]);
  });
});

describe('single-flight', () => {
  it('joins calls with the same key and runs again once the first settled', async () => {
    const f = createSingleFlight<number>();
    let runs = 0;
    const job = () => Promise.resolve(++runs);
    expect(await Promise.all([f.run('k', job), f.run('k', job), f.run('other', job)])).toEqual([
      1, 1, 2,
    ]);
    expect(await f.run('k', job)).toBe(3);
    await expect(f.run('k', () => Promise.reject(new Error('x')))).rejects.toThrow('x');
    expect(await f.run('k', job)).toBe(4);
  });
});

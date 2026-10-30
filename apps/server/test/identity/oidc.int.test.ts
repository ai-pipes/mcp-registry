import { randomBytes } from 'node:crypto';
import { CreateTokenResponse, MeResponse, TokenSummary, UserSummary } from '@mcp/contracts';
import { Hono } from 'hono';
import { z } from 'zod';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { DEFAULT_MIN_SIMILARITY } from '../../src/modules/search/index.ts';
import {
  createToken,
  type IdentityEnv,
  OpenidProvider,
  requireBearerToken,
} from '../../src/modules/identity/index.ts';
import { principalOf } from '../../src/modules/identity/domain/user.ts';
import type { Config } from '../../src/platform/config.ts';
import { buildContainer, type Container } from '../../src/platform/container.ts';
import { createHttpApp } from '../../src/platform/http/app.ts';
import { createLogger } from '../../src/platform/logger.ts';
import { startPg, truncateAll } from '../pg.ts';
import { FakeIdp, type FakeClaims } from './fake-idp.ts';

const ORIGIN = 'http://mcp.test';

let pg: Awaited<ReturnType<typeof startPg>>;
let idp: FakeIdp;
let container: Container;
let app: Hono;
const warnings: { fields: object; msg: string }[] = [];

function configFor(issuer: string): Config {
  return {
    port: 3000,
    databaseUrl: 'postgres://unused',
    publicUrl: ORIGIN,
    allowedHosts: [],
    trustProxy: false,
    trustProxyHops: 1,
    cookieSecure: false,
    secretsKey: new Uint8Array(randomBytes(32)),
    oidc: {
      issuer,
      clientId: idp.clientId,
      clientSecret: idp.clientSecret,
      loginGroup: 'mcp-users',
      adminGroup: 'mcp-admins',
    },
    syncIntervalHours: 6,
    callLogRetentionDays: 90,
    upstreamMaxResponseBytes: 20 * 1024 * 1024,
    mcpMaxBodyBytes: 10 * 1024 * 1024,
    embedder: 'local',
    modelDir: '/nonexistent',
    searchMinSimilarity: DEFAULT_MIN_SIMILARITY,
    logLevel: 'silent',
  };
}

beforeAll(async () => {
  pg = await startPg();
  idp = new FakeIdp();
  await idp.start();
  const config = configFor(idp.issuer);
  const oidc = new OpenidProvider({
    issuer: config.oidc.issuer,
    clientId: config.oidc.clientId,
    clientSecret: config.oidc.clientSecret,
    redirectUri: `${ORIGIN}/api/auth/callback`,
    logger: { warn: (fields, msg) => warnings.push({ fields, msg }) },
  });
  container = buildContainer(config, pg.sql, { logger: createLogger('silent'), oidc });
  const platform = createHttpApp(container, {
    remoteAddress: () => '192.0.2.1',
    webRoot: '/nonexistent',
  }).app;
  // Stand-in for the gateway's /mcp endpoint, which authenticates with the Bearer middleware.
  const mcp = new Hono<IdentityEnv>();
  mcp.use(async (c, next) => {
    c.set('clientIp', '192.0.2.1');
    await next();
  });
  mcp.get('/mcp', requireBearerToken(container.identity), (c) => c.text('ok'));
  app = new Hono().route('/', mcp).route('/', platform);
});
afterAll(async () => {
  await idp.stop();
  await pg.stop();
});
beforeEach(async () => {
  await truncateAll(pg.sql);
});

/** Walks the browser through the whole flow; returns the callback response. */
async function signIn(claims: FakeClaims, returnTo = '/servers') {
  const start = await app.request(`/api/auth/login?returnTo=${encodeURIComponent(returnTo)}`);
  expect(start.status).toBe(302);
  const location = new URL(start.headers.get('location') ?? '');
  expect(location.origin).toBe(idp.issuer);
  const code = idp.authorize(location, claims);
  const state = location.searchParams.get('state') ?? '';
  const binding =
    (setCookies(start).find((c) => c.startsWith('mcp_login=')) ?? '').split(';')[0] ?? '';
  expect(binding).toMatch(/^mcp_login=.+/);
  const callback = await app.request(
    `/api/auth/callback?code=${code}&state=${encodeURIComponent(state)}&iss=${encodeURIComponent(idp.issuer)}`,
    { headers: { cookie: binding } },
  );
  // The one-time binding cookie is cleared whatever the outcome.
  const cleared = setCookies(callback).find((c) => c.startsWith('mcp_login=')) ?? '';
  expect(cleared).toMatch(/^mcp_login=;/);
  expect(cleared).toMatch(/Max-Age=0/i);
  return { callback, code, state, binding };
}

function setCookies(res: Response): string[] {
  return res.headers.getSetCookie();
}

function cookieOf(res: Response): string {
  const raw = setCookies(res).find((c) => c.startsWith('mcp_session=')) ?? '';
  const pair = raw.split(';')[0] ?? '';
  expect(pair).toMatch(/^mcp_session=.+/);
  return pair;
}

const post = (path: string, cookie: string, body: unknown = {}) =>
  app.request(path, {
    method: 'POST',
    headers: { origin: ORIGIN, cookie, 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });

async function userCount(): Promise<number> {
  const rows = await pg.sql<{ n: number }[]>`select count(*)::int as n from identity.users`;
  return rows[0]?.n ?? -1;
}

const member: FakeClaims = {
  sub: 'sub-member',
  name: 'Anna',
  email: 'anna@example.com',
  groups: ['mcp-users'],
};
const admin: FakeClaims = { sub: 'sub-admin', name: 'Boris', groups: ['mcp-users', 'mcp-admins'] };

async function attempt(iss: string | null) {
  const start = await app.request('/api/auth/login');
  const binding =
    (setCookies(start).find((c) => c.startsWith('mcp_login=')) ?? '').split(';')[0] ?? '';
  const location = new URL(start.headers.get('location') ?? '');
  const code = idp.authorize(location, member);
  const state = encodeURIComponent(location.searchParams.get('state') ?? '');
  const issPart = iss === null ? '' : `&iss=${encodeURIComponent(iss)}`;
  return app.request(`/api/auth/callback?code=${code}&state=${state}${issPart}`, {
    headers: { cookie: binding },
  });
}

describe('OIDC login through the in-process provider', () => {
  it('signs in, sets the session cookie, redirects to returnTo and serves /me', async () => {
    const { callback } = await signIn(member);
    expect(callback.status).toBe(302);
    expect(callback.headers.get('location')).toBe('/servers');
    const cookie = cookieOf(callback);
    const sessionCookie = setCookies(callback).find((c) => c.startsWith('mcp_session=')) ?? '';
    expect(sessionCookie).toMatch(/HttpOnly/i);
    expect(sessionCookie).toMatch(/SameSite=Strict/i);
    expect(sessionCookie).toMatch(/Path=\//);

    const me = await app.request('/api/auth/me', { headers: { cookie } });
    expect(me.status).toBe(200);
    expect(await me.json()).toMatchObject({ name: 'Anna', role: 'member', groups: ['mcp-users'] });
  });

  it('an unexpected failure (database down) redirects with loginError=internal, not JSON 500', async () => {
    const users = container.identity.users;
    const upsert = users.upsertFromLogin.bind(users);
    users.upsertFromLogin = () => Promise.reject(new Error('connection terminated'));
    try {
      const { callback } = await signIn(member);
      expect(callback.status).toBe(302);
      expect(callback.headers.get('location')).toBe('/?loginError=internal');
      expect(setCookies(callback).some((c) => c.startsWith('mcp_session='))).toBe(false);
    } finally {
      users.upsertFromLogin = upsert;
    }
  });

  it('PgSessionRepository.deleteDead removes idle and expired sessions only (M9)', async () => {
    const { callback } = await signIn(member);
    expect(callback.status).toBe(302);
    const now = new Date('2026-10-30T12:00:00Z');
    await pg.sql`update identity.sessions set last_seen_at = ${now},
      expires_at = ${new Date(now.getTime() + 3_600_000)}`;
    const [user] = await pg.sql<{ id: string }[]>`select id from identity.users limit 1`;
    const insert = (id: string, lastSeen: Date, expires: Date) =>
      container.identity.sessions.insert({
        idHash: id,
        userId: user?.id ?? '',
        createdAt: lastSeen,
        lastSeenAt: lastSeen,
        expiresAt: expires,
      });
    await insert('idle', new Date(now.getTime() - 13 * 3_600_000), new Date(now.getTime() + 1));
    await insert('expired', now, now);
    const n = await container.identity.sessions.deleteDead({
      expiresAtOrBefore: now,
      lastSeenBefore: new Date(now.getTime() - 12 * 3_600_000),
    });
    expect(n).toBe(2);
    const left = await pg.sql`select id_hash from identity.sessions`;
    expect(left).toHaveLength(1);
  });

  it('makes members of the admin group admins and refreshes groups on the next login', async () => {
    await signIn({ ...member, groups: ['mcp-users'] });
    const again = await signIn({ ...member, groups: ['mcp-users', 'mcp-admins'] });
    const me = await app.request('/api/auth/me', { headers: { cookie: cookieOf(again.callback) } });
    expect(await me.json()).toMatchObject({ role: 'admin', groups: ['mcp-users', 'mcp-admins'] });
    expect(await userCount()).toBe(1);
  });

  it('refuses people outside the allowed groups and creates nobody', async () => {
    const { callback } = await signIn({ sub: 'x', name: 'X', groups: ['family'] });
    expect(callback.status).toBe(302);
    expect(callback.headers.get('location')).toBe('/?loginError=not_allowed');
    expect(setCookies(callback).some((c) => c.startsWith('mcp_session='))).toBe(false);
    expect(await userCount()).toBe(0);
  });

  it('keeps returnTo on this site and accepts a state only once', async () => {
    const { callback, code, state, binding } = await signIn(member, 'https://evil.example/');
    expect(callback.headers.get('location')).toBe('/');
    const replay = await app.request(`/api/auth/callback?code=${code}&state=${state}`, {
      headers: { cookie: binding },
    });
    expect(replay.headers.get('location')).toBe('/?loginError=invalid_state');
  });

  it('refuses a callback that did not start in this browser (login CSRF)', async () => {
    const start = await app.request('/api/auth/login');
    const location = new URL(start.headers.get('location') ?? '');
    const code = idp.authorize(location, member);
    const state = location.searchParams.get('state') ?? '';
    const url = `/api/auth/callback?code=${code}&state=${encodeURIComponent(state)}&iss=${encodeURIComponent(idp.issuer)}`;
    const noCookie = await app.request(url);
    expect(noCookie.headers.get('location')).toBe('/?loginError=invalid_state');
    expect(setCookies(noCookie).some((c) => c.startsWith('mcp_session='))).toBe(false);
    expect(await userCount()).toBe(0);
  });

  it('refuses a callback whose binding cookie belongs to another attempt', async () => {
    const other = await app.request('/api/auth/login');
    const otherBinding =
      (setCookies(other).find((c) => c.startsWith('mcp_login=')) ?? '').split(';')[0] ?? '';
    const start = await app.request('/api/auth/login');
    const location = new URL(start.headers.get('location') ?? '');
    const code = idp.authorize(location, member);
    const state = location.searchParams.get('state') ?? '';
    const res = await app.request(
      `/api/auth/callback?code=${code}&state=${encodeURIComponent(state)}&iss=${encodeURIComponent(idp.issuer)}`,
      { headers: { cookie: otherBinding } },
    );
    expect(res.headers.get('location')).toBe('/?loginError=invalid_state');
    expect(setCookies(res).some((c) => c.startsWith('mcp_session='))).toBe(false);
    expect(await userCount()).toBe(0);
  });

  it('refuses a callback with a wrong or missing iss (the provider advertises iss support)', async () => {
    for (const res of [await attempt('https://evil.example'), await attempt(null)]) {
      expect(res.headers.get('location')).toBe('/?loginError=invalid_state');
    }
    expect(await userCount()).toBe(0);
  });

  it('treats an absent groups claim (also at UserInfo) as not allowed', async () => {
    const { callback } = await signIn({ sub: 'nogroups', name: 'N' });
    expect(callback.headers.get('location')).toBe('/?loginError=not_allowed');
    expect(await userCount()).toBe(0);
  });

  it('denies and logs a warning when UserInfo fails, without leaking tokens or claims', async () => {
    idp.groupsOnlyInUserInfo = true;
    idp.userinfoFails = true;
    warnings.length = 0;
    try {
      const { callback } = await signIn(member);
      expect(callback.headers.get('location')).toBe('/?loginError=not_allowed');
    } finally {
      idp.groupsOnlyInUserInfo = false;
      idp.userinfoFails = false;
    }
    expect(await userCount()).toBe(0);
    expect(warnings).toHaveLength(1);
    expect(warnings[0]?.msg).toMatch(/userinfo failed/);
    const logged = JSON.stringify(warnings);
    expect(logged).toContain(member.sub);
    expect(logged).not.toContain('access-');
    expect(logged).not.toContain('anna@example.com');
  });

  it('falls back to UserInfo when the ID token has no groups', async () => {
    idp.groupsOnlyInUserInfo = true;
    try {
      const { callback } = await signIn(member);
      expect(callback.headers.get('location')).toBe('/servers');
    } finally {
      idp.groupsOnlyInUserInfo = false;
    }
  });

  it('rejects an unknown state and a callback without parameters', async () => {
    const unknown = await app.request('/api/auth/callback?code=c&state=nope');
    expect(unknown.headers.get('location')).toBe('/?loginError=invalid_state');
    const bare = await app.request('/api/auth/callback');
    expect(bare.headers.get('location')).toBe('/?loginError=invalid_state');
  });

  it('logout ends the session', async () => {
    const cookie = cookieOf((await signIn(member)).callback);
    expect((await post('/api/auth/logout', cookie)).status).toBe(204);
    expect((await app.request('/api/auth/me', { headers: { cookie } })).status).toBe(401);
  });
});

describe('cookie flags and CSRF', () => {
  it('marks session and binding cookies Secure when COOKIE_SECURE is on', async () => {
    const secureApp = createHttpApp(
      buildContainer({ ...configFor(idp.issuer), cookieSecure: true }, pg.sql, {
        logger: createLogger('silent'),
        oidc: container.identity.oidc,
      }),
      { remoteAddress: () => '192.0.2.1', webRoot: '/nonexistent' },
    ).app;
    const start = await secureApp.request('/api/auth/login');
    const binding = setCookies(start).find((c) => c.startsWith('mcp_login=')) ?? '';
    expect(binding).toMatch(/Secure/i);
    expect(binding).toMatch(/SameSite=Lax/i);
    expect(binding).toMatch(/HttpOnly/i);
    const location = new URL(start.headers.get('location') ?? '');
    const code = idp.authorize(location, member);
    const state = encodeURIComponent(location.searchParams.get('state') ?? '');
    const cb = await secureApp.request(
      `/api/auth/callback?code=${code}&state=${state}&iss=${encodeURIComponent(idp.issuer)}`,
      { headers: { cookie: binding.split(';')[0] ?? '' } },
    );
    expect(setCookies(cb).find((c) => c.startsWith('mcp_session='))).toMatch(/Secure/i);
  });

  it('refuses mutating /api/auth and /api/tokens calls without the right Origin', async () => {
    const cookie = cookieOf((await signIn(member)).callback);
    const noOrigin = (path: string, method: string) =>
      app.request(path, {
        method,
        headers: { cookie, 'content-type': 'application/json' },
        body: method === 'DELETE' ? null : '{"name":"x"}',
      });
    expect((await noOrigin('/api/auth/logout', 'POST')).status).toBe(403);
    expect((await noOrigin('/api/tokens', 'POST')).status).toBe(403);
    expect(
      (await noOrigin('/api/tokens/5b4d9c0e-0000-4000-8000-000000000000', 'DELETE')).status,
    ).toBe(403);
    const wrong = await app.request('/api/tokens', {
      method: 'POST',
      headers: { cookie, origin: 'https://evil.example', 'content-type': 'application/json' },
      body: '{"name":"x"}',
    });
    expect(wrong.status).toBe(403);
    // the session survived all of it
    expect((await app.request('/api/auth/me', { headers: { cookie } })).status).toBe(200);
  });
});

describe('tokens and user administration', () => {
  it('an sk_ token works on /mcp but never on /api/**', async () => {
    const cookie = cookieOf((await signIn(member)).callback);
    const created = await post('/api/tokens', cookie, { name: 'laptop' });
    expect(created.status).toBe(201);
    const { token, id } = CreateTokenResponse.parse(await created.json());
    expect(token).toMatch(/^sk_/);

    const bearer = { authorization: `Bearer ${token}` };
    expect((await app.request('/api/auth/me', { headers: bearer })).status).toBe(401);
    expect((await app.request('/api/auth/me', { headers: { ...bearer, cookie } })).status).toBe(
      401,
    );
    expect((await app.request('/api/tokens', { headers: bearer })).status).toBe(401);
    expect((await app.request('/mcp', { headers: bearer })).status).toBe(200);

    const list = z
      .array(TokenSummary)
      .parse(await (await app.request('/api/tokens', { headers: { cookie } })).json());
    expect(list.map((t) => t.id)).toEqual([id]);
    expect(list[0]?.lastUsedAt).not.toBeNull();
    expect(JSON.stringify(list)).not.toContain(token);

    const del = await app.request(`/api/tokens/${id}`, {
      method: 'DELETE',
      headers: { origin: ORIGIN, cookie },
    });
    expect(del.status).toBe(204);
    expect((await app.request('/mcp', { headers: bearer })).status).toBe(401);
  });

  it('disabling a user ends their sessions and tokens at once', async () => {
    const memberCookie = cookieOf((await signIn(member)).callback);
    const adminCookie = cookieOf((await signIn(admin)).callback);
    const created = await post('/api/tokens', memberCookie, { name: 'laptop' });
    const { token } = CreateTokenResponse.parse(await created.json());
    const bearer = { authorization: `Bearer ${token}` };
    expect((await app.request('/mcp', { headers: bearer })).status).toBe(200);

    const memberMe = MeResponse.parse(
      await (await app.request('/api/auth/me', { headers: { cookie: memberCookie } })).json(),
    );

    // members cannot administer users
    expect((await app.request('/api/users', { headers: { cookie: memberCookie } })).status).toBe(
      403,
    );
    const users = await app.request('/api/users', { headers: { cookie: adminCookie } });
    expect(
      z
        .array(UserSummary)
        .parse(await users.json())
        .map((u) => u.name)
        .toSorted(),
    ).toEqual(['Anna', 'Boris']);

    const off = await post(`/api/users/${memberMe.userId}/disabled`, adminCookie, {
      disabled: true,
    });
    expect(off.status).toBe(204);
    expect((await app.request('/mcp', { headers: bearer })).status).toBe(401);
    expect((await app.request('/api/auth/me', { headers: { cookie: memberCookie } })).status).toBe(
      401,
    );

    // a disabled person cannot sign in again
    const retry = await signIn(member);
    expect(retry.callback.headers.get('location')).toBe('/?loginError=disabled');
    expect(setCookies(retry.callback).some((c) => c.startsWith('mcp_session='))).toBe(false);
  });

  it('answers 404 for a malformed user id and 401 without a session', async () => {
    const adminCookie = cookieOf((await signIn(admin)).callback);
    const bad = await post('/api/users/not-a-uuid/disabled', adminCookie, { disabled: true });
    expect(bad.status).toBe(404);
    expect((await app.request('/api/users')).status).toBe(401);
  });

  it('rejects /mcp without a bearer token', async () => {
    const res = await app.request('/mcp');
    expect(res.status).toBe(401);
    expect(res.headers.get('www-authenticate')).toBe('Bearer');
  });
});

describe('an identity provider that is down at startup', () => {
  it('the server works without it, and sign-in works once it is back', async () => {
    idp.discoveryDown = true;
    try {
      const config = configFor(idp.issuer);
      const lazy = new OpenidProvider({
        issuer: config.oidc.issuer,
        clientId: config.oidc.clientId,
        clientSecret: config.oidc.clientSecret,
        redirectUri: `${ORIGIN}/api/auth/callback`,
      });
      const c = buildContainer(config, pg.sql, { logger: createLogger('silent'), oidc: lazy });
      const server = createHttpApp(c, {
        remoteAddress: () => '192.0.2.1',
        webRoot: '/nonexistent',
      }).app;

      expect((await server.request('/healthz')).status).toBe(200);

      const user = await c.identity.users.upsertFromLogin({
        claims: { sub: 'ops', name: 'Ops', email: null, groups: ['mcp-users'] },
        role: 'member',
        at: new Date(),
      });
      const { token } = await createToken(c.identity, {
        principal: principalOf(user, 'session'),
        name: 't',
      });
      const init = await server.request(`${ORIGIN}/mcp`, {
        method: 'POST',
        headers: {
          host: 'mcp.test',
          authorization: `Bearer ${token}`,
          'content-type': 'application/json',
          accept: 'application/json, text/event-stream',
        },
        body: JSON.stringify({
          jsonrpc: '2.0',
          id: 1,
          method: 'initialize',
          params: {
            protocolVersion: '2025-06-18',
            capabilities: {},
            clientInfo: { name: 't', version: '1' },
          },
        }),
      });
      expect(init.status).toBe(200);
      expect(await init.text()).toContain('"serverInfo"');

      const down = await server.request('/api/auth/login?returnTo=/');
      expect(down.status).toBe(302);
      expect(down.headers.get('location')).toBe('/?loginError=idp_unavailable');
      idp.discoveryDown = false;
      const up = await server.request('/api/auth/login?returnTo=/');
      expect(up.status).toBe(302);
      expect(new URL(up.headers.get('location') ?? '').origin).toBe(idp.issuer);
    } finally {
      idp.discoveryDown = false;
    }
  });
});

describe('an identity provider that fails during the code exchange', () => {
  it('a 5xx from the token endpoint is idp_unavailable, not a rejected login', async () => {
    idp.tokenStatus = 503;
    try {
      const { callback } = await signIn(member);
      expect(callback.headers.get('location')).toBe('/?loginError=idp_unavailable');
      const failed = await pg.sql`select 1 from platform.audit_log
        where action = 'auth.login_failed'`;
      expect(failed).toHaveLength(0);
    } finally {
      idp.tokenStatus = null;
    }
  });

  it('an OAuth error answer stays a rejected login', async () => {
    idp.tokenStatus = 400;
    try {
      const { callback } = await signIn(member);
      expect(callback.headers.get('location')).toBe('/?loginError=invalid_state');
    } finally {
      idp.tokenStatus = null;
    }
  });
});

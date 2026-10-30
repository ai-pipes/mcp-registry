import { ConnectionSummary, StartConnectResponse } from '@mcp/contracts';
import type { Hono } from 'hono';
import postgres from 'postgres';
import { z } from 'zod';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { createServer, syncServer, updateServer } from '../../src/modules/catalog/index.ts';
import {
  type ConnectionsDeps,
  createSingleFlight,
  resolveCredential,
} from '../../src/modules/connections/index.ts';
import { loadConfig } from '../../src/platform/config.ts';
import { buildContainer, type Container } from '../../src/platform/container.ts';
import { createHttpApp } from '../../src/platform/http/app.ts';
import type { AppEnv } from '../../src/platform/http/env.ts';
import { type FakeUpstream, startFakeUpstream } from '../fake-upstream.ts';
import { startPg, truncateAll } from '../pg.ts';
import { FakeOAuthServer } from './fake-oauth.ts';

const ORIGIN = 'http://mcp.test';

let pg: Awaited<ReturnType<typeof startPg>>;
let provider: FakeOAuthServer;
let up: FakeUpstream;
let container: Container;
let config: ReturnType<typeof loadConfig>;
let app: Hono<AppEnv>;

type Person = { id: string; cookie: string };
let admin: Person;
let member: Person;

beforeAll(async () => {
  provider = new FakeOAuthServer();
  [pg, up] = await Promise.all([startPg(), startFakeUpstream(), provider.start()]);
  config = loadConfig({
    DATABASE_URL: pg.url,
    PUBLIC_URL: ORIGIN,
    SECRETS_KEY: Buffer.alloc(32, 9).toString('base64'),
    OIDC_ISSUER: 'https://id.example.com',
    OIDC_CLIENT_ID: 'x',
    OIDC_CLIENT_SECRET: 'y',
    LOG_LEVEL: 'silent',
  });
  container = buildContainer(config, pg.sql);
  app = createHttpApp(container, { remoteAddress: () => '192.0.2.1', webRoot: '/nonexistent' }).app;
});
afterAll(async () => {
  await Promise.all([pg.stop(), up.close(), provider.stop()]);
});

async function person(sub: string, role: 'admin' | 'member'): Promise<Person> {
  const at = new Date();
  const user = await container.identity.users.upsertFromLogin({
    claims: {
      sub,
      name: sub,
      email: null,
      groups: [role === 'admin' ? 'mcp-admins' : 'mcp-users'],
    },
    role,
    at,
  });
  const sessionId = `session-${sub}-${Math.random()}`;
  await container.identity.sessions.insert({
    idHash: container.identity.sha256.hex(sessionId),
    userId: user.id,
    createdAt: at,
    lastSeenAt: at,
    expiresAt: new Date(at.getTime() + 3_600_000),
  });
  return { id: user.id, cookie: `mcp_session=${sessionId}` };
}

beforeEach(async () => {
  await truncateAll(pg.sql);
  provider.refreshes.length = 0;
  provider.revoked.length = 0;
  provider.refreshFails = false;
  provider.refreshUnavailable = false;
  provider.refreshDelayMs = 0;
  up.reset();
  up.requests.length = 0;
  admin = await person('admin', 'admin');
  member = await person('member', 'member');
  const principal = { userId: admin.id, role: 'admin' as const, groups: [] };
  const gitea = await createServer(container.catalog, {
    principal,
    slug: 'gitea',
    name: 'Gitea',
    description: 'Git hosting',
    url: up.url,
    auth: {
      kind: 'oauth_user',
      oauth: {
        authorizeUrl: `${provider.url}/authorize`,
        tokenUrl: `${provider.url}/token`,
        revokeUrl: `${provider.url}/revoke`,
        clientId: provider.clientId,
        scopes: ['repo'],
        extraAuthorizeParams: { access_type: 'offline' },
      },
      header: 'Authorization',
      template: 'Bearer {secret}',
    },
    access: { kind: 'all' },
    timeoutMs: 5000,
    secret: provider.clientSecret,
  });
  if (!gitea.ok) throw gitea.error;
  const yt = await createServer(container.catalog, {
    principal,
    slug: 'yt',
    name: 'YouTrack',
    description: 'Issues',
    url: up.url,
    auth: { kind: 'pat_user', header: 'Authorization', template: 'Bearer {secret}' },
    access: { kind: 'all' },
    timeoutMs: 5000,
  });
  if (!yt.ok) throw yt.error;
});

function call(who: Person | null, method: string, path: string, body?: unknown) {
  const headers: Record<string, string> = { origin: ORIGIN };
  if (who) headers['cookie'] = who.cookie;
  if (body !== undefined) headers['content-type'] = 'application/json';
  return app.request(path, {
    method,
    headers,
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

async function startAt(who: Person, server = 'gitea'): Promise<URL> {
  const res = await call(who, 'POST', `/api/connections/${server}/start`);
  expect(res.status).toBe(200);
  return new URL(StartConnectResponse.parse(await res.json()).url);
}

async function callback(who: Person | null, query: Record<string, string>) {
  const res = await call(
    who,
    'GET',
    `/api/connections/callback?${new URLSearchParams(query).toString()}`,
  );
  expect(res.status).toBe(302);
  return res.headers.get('location');
}

async function connect(who: Person): Promise<void> {
  const url = await startAt(who);
  const code = provider.authorize(url);
  expect(await callback(who, { state: url.searchParams.get('state') ?? '', code })).toBe(
    '/connections?connected=gitea',
  );
}

async function list(who: Person) {
  const res = await call(who, 'GET', '/api/connections');
  expect(res.status).toBe(200);
  return z.array(ConnectionSummary).parse(await res.json());
}

const statusOf = async (who: Person, server: string) =>
  (await list(who)).find((c) => c.server === server)?.status;

const row = async (userId: string, server: string) => {
  const [r] = await pg.sql<
    { access: Buffer; refresh: Buffer | null; status: string; expires_at: Date | null }[]
  >`select access, refresh, status, expires_at from connections.connections
    where user_id = ${userId} and server = ${server}`;
  return r;
};

describe('OAuth connect over HTTP', () => {
  it('start → callback → ready → disconnect → missing', async () => {
    expect(await list(member)).toEqual([
      { server: 'gitea', name: 'Gitea', authKind: 'oauth_user', status: 'missing' },
      { server: 'yt', name: 'YouTrack', authKind: 'pat_user', status: 'missing' },
    ]);

    const url = await startAt(member);
    expect(url.origin + url.pathname).toBe(`${provider.url}/authorize`);
    expect(url.searchParams.get('redirect_uri')).toBe(`${ORIGIN}/api/connections/callback`);
    expect(url.searchParams.get('code_challenge_method')).toBe('S256');
    expect(url.searchParams.get('scope')).toBe('repo');
    expect(url.searchParams.get('access_type')).toBe('offline');
    // The client secret has `+`, `%` and `:`: the code exchange below only works if the gateway
    // form-encodes it before Basic and the provider decodes it back.
    expect(provider.clientSecret).toMatch(/\+.*%.*:/);

    const code = provider.authorize(url);
    expect(await callback(member, { state: url.searchParams.get('state') ?? '', code })).toBe(
      '/connections?connected=gitea',
    );
    expect(await statusOf(member, 'gitea')).toBe('ready');

    // Stored sealed: neither token is in the row in plain text.
    const token = await resolveCredential(container.connections, {
      userId: member.id,
      server: 'gitea',
    });
    if (!token.ok) throw new Error(token.error);
    const stored = await row(member.id, 'gitea');
    expect(stored?.access.includes(Buffer.from(token.value))).toBe(false);
    expect(stored?.access.includes(Buffer.from('access-'))).toBe(false);
    expect(stored?.refresh?.includes(Buffer.from('refresh-'))).toBe(false);
    expect(stored?.expires_at).not.toBeNull();

    const del = await call(member, 'DELETE', '/api/connections/gitea');
    expect(del.status).toBe(204);
    expect(provider.revoked).toHaveLength(1);
    expect(provider.revoked[0]).toMatch(/^refresh-/);
    expect(await statusOf(member, 'gitea')).toBe('missing');
  });

  it('the callback: someone else’s state, reuse, no session and no code all fail', async () => {
    const url = await startAt(member);
    const state = url.searchParams.get('state') ?? '';
    const code = provider.authorize(url);
    expect(await callback(null, { state, code })).toBe('/connections?error=unauthenticated');
    expect(await callback(admin, { state, code })).toBe('/connections?error=invalid_state');
    expect(await callback(member, { state, code })).toBe('/connections?error=invalid_state');
    expect(await callback(member, { state: 'x', error: 'access_denied' })).toBe(
      '/connections?error=denied',
    );
    expect(await statusOf(member, 'gitea')).toBe('missing');
    expect(await statusOf(admin, 'gitea')).toBe('missing');
  });

  it('an unexpected failure (database down) → error=internal, not JSON 500', async () => {
    const url = await startAt(member);
    const code = provider.authorize(url);
    const store = container.connections.connections;
    const withLock = store.withLock.bind(store);
    store.withLock = () => Promise.reject(new Error('connection terminated'));
    try {
      expect(await callback(member, { state: url.searchParams.get('state') ?? '', code })).toBe(
        '/connections?error=internal',
      );
    } finally {
      store.withLock = withLock;
    }
  });

  it('the provider refusing the code → exchange_failed', async () => {
    const url = await startAt(member);
    expect(
      await callback(member, { state: url.searchParams.get('state') ?? '', code: 'bogus' }),
    ).toBe('/connections?error=exchange_failed');
  });

  it('start for a PAT server or an unknown one is refused', async () => {
    expect((await call(member, 'POST', '/api/connections/yt/start')).status).toBe(422);
    expect((await call(member, 'POST', '/api/connections/nope/start')).status).toBe(404);
  });
});

describe('refresh against Postgres', () => {
  it('two processes, expired token: one refresh at the provider, both get the new token', async () => {
    await connect(member);
    const before = await resolveCredential(container.connections, {
      userId: member.id,
      server: 'gitea',
    });
    await pg.sql`update connections.connections set expires_at = now() - interval '1 minute'`;
    provider.refreshDelayMs = 100;
    // Separate single-flights stand for separate replicas: only the row lock is shared.
    const replica: ConnectionsDeps = { ...container.connections, flights: createSingleFlight() };
    const [a, b] = await Promise.all([
      resolveCredential(container.connections, { userId: member.id, server: 'gitea' }),
      resolveCredential(replica, { userId: member.id, server: 'gitea' }),
    ]);
    expect(provider.refreshes).toHaveLength(1);
    expect(a.ok && b.ok).toBe(true);
    expect(a).toEqual(b);
    expect(a).not.toEqual(before);
    expect((await row(member.id, 'gitea'))?.expires_at?.getTime()).toBeGreaterThan(Date.now());
  });

  it('a pool of 2 connections: 3 concurrent renewals for different people all finish', async () => {
    const people = [member, admin, await person('third', 'member')];
    await Promise.all(people.map(connect));
    await pg.sql`update connections.connections set expires_at = now() - interval '1 minute'`;
    provider.refreshDelayMs = 200;
    // Every renewal holds one pool connection for its lock; anything else it needed from the
    // same pool while holding it (catalog reads, audit) would wait forever with max = 2.
    const small = postgres(pg.url, { max: 2, onnotice: () => undefined });
    try {
      const tight = buildContainer(config, small).connections;
      const results = await Promise.all(
        people.map((p) => resolveCredential(tight, { userId: p.id, server: 'gitea' })),
      );
      expect(results.map((r) => r.ok)).toEqual([true, true, true]);
      expect(provider.refreshes).toHaveLength(3);
    } finally {
      await small.end({ timeout: 1 });
    }
  }, 15_000);

  it('the provider down (503): unavailable, the connection stays ready and works later', async () => {
    await connect(member);
    await pg.sql`update connections.connections set expires_at = now()`;
    provider.refreshUnavailable = true;
    expect(
      await resolveCredential(container.connections, { userId: member.id, server: 'gitea' }),
    ).toEqual({ ok: false, error: 'unavailable' });
    expect(await statusOf(member, 'gitea')).toBe('ready');
    provider.refreshUnavailable = false;
    const r = await resolveCredential(container.connections, {
      userId: member.id,
      server: 'gitea',
    });
    expect(r.ok).toBe(true);
    expect(provider.refreshes).toHaveLength(2);
  });

  it('a failed refresh marks the connection expired', async () => {
    await connect(member);
    await pg.sql`update connections.connections set expires_at = now()`;
    provider.refreshFails = true;
    expect(
      await resolveCredential(container.connections, { userId: member.id, server: 'gitea' }),
    ).toEqual({ ok: false, error: 'expired' });
    expect(await statusOf(member, 'gitea')).toBe('expired');
  });
});

describe('the server config changes after connecting', () => {
  it('a new upstream URL: the token is not handed out, shown expired, no revoke on disconnect', async () => {
    await connect(member);
    const principal = { userId: admin.id, role: 'admin' as const, groups: [] };
    const moved = await updateServer(container.catalog, {
      principal,
      slug: 'gitea',
      changes: { url: 'http://127.0.0.1:9/elsewhere' },
    });
    expect(moved.ok).toBe(true);
    expect(await statusOf(member, 'gitea')).toBe('expired');
    expect(
      await resolveCredential(container.connections, { userId: member.id, server: 'gitea' }),
    ).toEqual({ ok: false, error: 'expired' });
    expect((await row(member.id, 'gitea'))?.status).toBe('expired');
    expect(provider.refreshes).toEqual([]);
    expect((await call(member, 'DELETE', '/api/connections/gitea')).status).toBe(204);
    expect(provider.revoked).toEqual([]);
  });

  it('a new revoke URL: the connection stays ready, disconnect does not call the new URL', async () => {
    await connect(member);
    const principal = { userId: admin.id, role: 'admin' as const, groups: [] };
    // Same fake endpoint, other URL: a revoke sent to it would still be recorded.
    const changed = await updateServer(container.catalog, {
      principal,
      slug: 'gitea',
      changes: {
        auth: {
          kind: 'oauth_user',
          oauth: {
            authorizeUrl: `${provider.url}/authorize`,
            tokenUrl: `${provider.url}/token`,
            revokeUrl: `${provider.url}/revoke?v=2`,
            clientId: provider.clientId,
            scopes: ['repo'],
            extraAuthorizeParams: { access_type: 'offline' },
          },
          header: 'Authorization',
          template: 'Bearer {secret}',
        },
      },
    });
    expect(changed.ok).toBe(true);
    expect(await statusOf(member, 'gitea')).toBe('ready');
    expect(
      (await resolveCredential(container.connections, { userId: member.id, server: 'gitea' })).ok,
    ).toBe(true);
    expect((await call(member, 'DELETE', '/api/connections/gitea')).status).toBe(204);
    expect(provider.revoked).toEqual([]);
  });
});

describe('PAT', () => {
  it('PUT stores it sealed; an empty token and an OAuth server are refused', async () => {
    expect(
      (await call(member, 'PUT', '/api/connections/yt/pat', { token: 'perm:abc' })).status,
    ).toBe(204);
    expect(await statusOf(member, 'yt')).toBe('ready');
    expect((await row(member.id, 'yt'))?.access.includes(Buffer.from('perm:abc'))).toBe(false);
    expect(
      await resolveCredential(container.connections, { userId: member.id, server: 'yt' }),
    ).toEqual({ ok: true, value: 'perm:abc' });
    expect((await call(member, 'PUT', '/api/connections/yt/pat', { token: '   ' })).status).toBe(
      422,
    );
    expect((await call(member, 'PUT', '/api/connections/gitea/pat', { token: 't' })).status).toBe(
      422,
    );
  });
});

describe('web API rules', () => {
  it('needs a session, refuses Bearer tokens and cross-origin mutations', async () => {
    expect((await call(null, 'GET', '/api/connections')).status).toBe(401);
    const bearer = await app.request('/api/connections', {
      headers: { cookie: member.cookie, authorization: 'Bearer sk_x' },
    });
    expect(bearer.status).toBe(401);
    const foreign = await app.request('/api/connections/gitea/start', {
      method: 'POST',
      headers: { cookie: member.cookie, origin: 'https://evil.example' },
    });
    expect(foreign.status).toBe(403);
  });
});

describe('catalog synchronisation with the admin’s connection', () => {
  it('syncs an oauth_user server with the admin’s own access token', async () => {
    const principal = { userId: admin.id, role: 'admin' as const, groups: [] };
    const before = await syncServer(container.catalogSync, { principal, slug: 'gitea' });
    expect(before.ok ? 'ok' : before.error.code).toBe('needs_connection');

    await connect(admin);
    up.setTools([{ name: 'a' }]);
    const r = await syncServer(container.catalogSync, { principal, slug: 'gitea' });
    expect(r.ok).toBe(true);
    const token = await resolveCredential(container.connections, {
      userId: admin.id,
      server: 'gitea',
    });
    if (!token.ok) throw new Error(token.error);
    expect(up.requests.length).toBeGreaterThan(0);
    for (const req of up.requests)
      expect(req.headers['authorization']).toBe(`Bearer ${token.value}`);

    // The scheduler uses the last successful admin while they are still an enabled admin.
    expect((await syncServer(container.catalogSync, { principal: null, slug: 'gitea' })).ok).toBe(
      true,
    );
    await pg.sql`update identity.users set role = 'member' where id = ${admin.id}`;
    const demoted = await syncServer(container.catalogSync, { principal: null, slug: 'gitea' });
    expect(demoted.ok ? 'ok' : demoted.error.code).toBe('needs_connection');
    await pg.sql`update identity.users set role = 'admin', disabled = true where id = ${admin.id}`;
    const disabled = await syncServer(container.catalogSync, { principal: null, slug: 'gitea' });
    expect(disabled.ok ? 'ok' : disabled.error.code).toBe('needs_connection');

    // An expired connection stops it too; an unreachable provider is a transport failure.
    await pg.sql`update identity.users set disabled = false where id = ${admin.id}`;
    await pg.sql`update connections.connections set expires_at = now()`;
    provider.refreshUnavailable = true;
    const down = await syncServer(container.catalogSync, { principal: null, slug: 'gitea' });
    expect(down.ok ? 'ok' : down.error.code).toBe('sync_failed');
    const [syncState] = await pg.sql<{ last_sync_error: string | null }[]>`
      select last_sync_error from catalog.servers where slug = 'gitea'`;
    expect(syncState?.last_sync_error).toBe('The OAuth provider is unavailable');
    provider.refreshUnavailable = false;
    await pg.sql`update connections.connections set status = 'expired'`;
    const scheduled = await syncServer(container.catalogSync, { principal: null, slug: 'gitea' });
    expect(scheduled.ok ? 'ok' : scheduled.error.code).toBe('needs_connection');
  });
});

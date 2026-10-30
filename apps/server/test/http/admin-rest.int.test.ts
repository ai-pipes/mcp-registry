import {
  AdminToolList,
  AuditList,
  CallList,
  CatalogResponse,
  ErrorBody,
  ServerList,
  ServerView,
  SyncResult,
} from '@mcp/contracts';
import type { Hono } from 'hono';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { loadConfig } from '../../src/platform/config.ts';
import { buildContainer, type Container } from '../../src/platform/container.ts';
import { createHttpApp } from '../../src/platform/http/app.ts';
import type { AppEnv } from '../../src/platform/http/env.ts';
import { type FakeUpstream, startFakeUpstream } from '../fake-upstream.ts';
import { startPg, truncateAll } from '../pg.ts';

const ORIGIN = 'http://mcp.test';
const SECRET = 'sk-live-TOPSECRET-7f3a9c';
const CLIENT_SECRET = 'oauth-client-SECRET-91bd';

let pg: Awaited<ReturnType<typeof startPg>>;
let up: FakeUpstream;
let container: Container;
let app: Hono<AppEnv>;

type Person = { id: string; cookie: string };
let admin: Person;
let member: Person;

beforeAll(async () => {
  [pg, up] = await Promise.all([
    startPg(),
    startFakeUpstream([
      { name: 'list_repos', title: 'List repos', description: 'Lists repositories' },
      {
        name: 'delete_repo',
        description: 'Deletes a repository',
        annotations: { destructiveHint: true },
      },
    ]),
  ]);
  const config = loadConfig({
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
  await Promise.all([pg.stop(), up.close()]);
});

async function person(sub: string, role: 'admin' | 'member', groups: string[]): Promise<Person> {
  const at = new Date();
  const user = await container.identity.users.upsertFromLogin({
    claims: { sub, name: sub, email: null, groups },
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
  up.reset();
  admin = await person('admin', 'admin', ['mcp-admins']);
  member = await person('member', 'member', ['mcp-users']);
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

const sharedInput = (over: Record<string, unknown> = {}) => ({
  slug: 'gitea',
  name: 'Gitea',
  description: 'Git hosting',
  url: up.url,
  auth: { kind: 'shared_secret', header: 'Authorization', template: 'Bearer {secret}' },
  access: { kind: 'all' },
  secret: SECRET,
  ...over,
});

const oauthInput = () => ({
  slug: 'hub',
  name: 'Hub',
  description: 'Issues',
  url: up.url,
  auth: {
    kind: 'oauth_user',
    header: 'Authorization',
    template: 'Bearer {secret}',
    oauth: {
      authorizeUrl: 'https://hub.example.com/authorize',
      tokenUrl: 'https://hub.example.com/token',
      clientId: 'cid',
      scopes: ['read'],
      extraAuthorizeParams: { access_type: 'offline' },
    },
  },
  access: { kind: 'all' },
  secret: CLIENT_SECRET,
});

async function create(input: Record<string, unknown> = sharedInput()): Promise<ServerView> {
  const res = await call(admin, 'POST', '/api/servers', input);
  expect(res.status).toBe(201);
  return ServerView.parse(await res.json());
}

describe('admin routes refuse members', () => {
  it('answers 403 on every admin route, before parsing the body', async () => {
    await create();
    const routes: [string, string, unknown?][] = [
      ['GET', '/api/servers'],
      ['POST', '/api/servers', {}],
      ['GET', '/api/servers/gitea'],
      ['PUT', '/api/servers/gitea', {}],
      ['PUT', '/api/servers/gitea/secret', {}],
      ['POST', '/api/servers/gitea/status', {}],
      ['POST', '/api/servers/gitea/sync'],
      ['GET', '/api/servers/gitea/tools'],
      ['PUT', '/api/servers/gitea/tools/list_repos/hidden', {}],
      ['GET', '/api/calls'],
      ['GET', '/api/audit'],
    ];
    await Promise.all(
      routes.map(async ([method, path, body]) => {
        const res = await call(member, method, path, body);
        expect(res.status, `${method} ${path}`).toBe(403);
        expect(ErrorBody.parse(await res.json()).error.code).toBe('forbidden');
      }),
    );
  });

  it('answers 401 without a session', async () => {
    await Promise.all(
      ['/api/servers', '/api/catalog', '/api/calls', '/api/audit'].map(async (path) => {
        expect((await call(null, 'GET', path)).status, path).toBe(401);
      }),
    );
  });

  it('refuses a Bearer sk_ token on /api/servers, even next to a valid cookie', async () => {
    const res = await app.request('/api/servers', {
      headers: { authorization: 'Bearer sk_abcdef', cookie: admin.cookie },
    });
    expect(res.status).toBe(401);
  });

  it('refuses a mutation with a foreign Origin', async () => {
    const res = await app.request('/api/servers', {
      method: 'POST',
      headers: {
        origin: 'http://evil.test',
        cookie: admin.cookie,
        'content-type': 'application/json',
      },
      body: JSON.stringify(sharedInput()),
    });
    expect(res.status).toBe(403);
  });
});

describe('POST /api/servers', () => {
  it('synchronises a shared_secret server at once: the catalog is filled', async () => {
    const view = await create();
    expect(view).toMatchObject({
      slug: 'gitea',
      secretSet: true,
      toolCount: 2,
      lastSyncError: null,
    });
    expect(view.lastSyncAt).not.toBeNull();
    const tools = AdminToolList.parse(
      await (await call(admin, 'GET', '/api/servers/gitea/tools')).json(),
    );
    expect(tools.map((t) => t.ref).toSorted()).toEqual(['gitea.delete_repo', 'gitea.list_repos']);
    const audit = AuditList.parse(await (await call(admin, 'GET', '/api/audit')).json());
    expect(audit.map((a) => a.action)).toEqual(
      expect.arrayContaining(['server.create', 'server.sync']),
    );
  });

  it('creates an oauth_user server without a connection and records why it is not synced', async () => {
    const view = await create(oauthInput());
    expect(view.toolCount).toBe(0);
    expect(view.lastSyncError).toBe('An administrator needs to connect this server');
  });

  it('answers 409 for a taken slug and 422 for an invalid one', async () => {
    await create();
    expect((await call(admin, 'POST', '/api/servers', sharedInput())).status).toBe(409);
    const bad = await call(
      admin,
      'POST',
      '/api/servers',
      sharedInput({ slug: 'Bad Slug', secret: null }),
    );
    expect(bad.status).toBe(422);
  });

  it('rejects out-of-range input (422): timeout, daily limit, description, URL scheme', async () => {
    const invalid = [
      { timeoutMs: 999 },
      { timeoutMs: 600_001 },
      { dailyLimit: 0 },
      { dailyLimit: 100_001 },
      { description: 'x'.repeat(501) },
      { url: 'ftp://example.com' },
    ];
    await Promise.all(
      invalid.map(async (over) => {
        const res = await call(admin, 'POST', '/api/servers', sharedInput(over));
        expect(res.status, JSON.stringify(over)).toBe(422);
      }),
    );
  });
});

describe('secrets never leave', () => {
  it('no /api/servers* response contains the shared secret or the OAuth client secret', async () => {
    await create();
    await create(oauthInput());
    const bodies: string[] = [];
    const record = async (res: Response) => {
      bodies.push(await res.text());
    };
    await record(await call(admin, 'GET', '/api/servers'));
    await record(await call(admin, 'GET', '/api/servers/gitea'));
    await record(await call(admin, 'GET', '/api/servers/hub'));
    await record(await call(admin, 'GET', '/api/servers/gitea/tools'));
    await record(await call(admin, 'PUT', '/api/servers/gitea/secret', { secret: `${SECRET}-2` }));
    await record(await call(admin, 'POST', '/api/servers/gitea/sync'));
    await record(await call(admin, 'POST', '/api/servers/gitea/status', { status: 'disabled' }));
    await record(await call(admin, 'GET', '/api/audit'));
    await record(await call(admin, 'GET', '/api/catalog'));
    const all = bodies.join('\n');
    for (const secret of [SECRET, CLIENT_SECRET, `${SECRET}-2`]) expect(all).not.toContain(secret);
    expect(all).toContain('"secretSet":true');
  });

  it('PUT /secret sets and clears (null); the flag follows', async () => {
    await create(sharedInput({ secret: null }));
    const get = async () =>
      ServerView.parse(await (await call(admin, 'GET', '/api/servers/gitea')).json());
    expect((await get()).secretSet).toBe(false);
    const set = await call(admin, 'PUT', '/api/servers/gitea/secret', { secret: SECRET });
    expect(set.status).toBe(204);
    expect((await get()).secretSet).toBe(true);
    await call(admin, 'PUT', '/api/servers/gitea/secret', { secret: null });
    expect((await get()).secretSet).toBe(false);
  });
});

describe('PUT /api/servers/:slug', () => {
  it('changing access emits catalog.server_changed and an audit record', async () => {
    await create();
    await pg.sql`delete from platform.outbox`;
    const res = await call(admin, 'PUT', '/api/servers/gitea', {
      name: 'Gitea',
      description: 'Git hosting',
      url: up.url,
      auth: { kind: 'shared_secret', header: 'Authorization', template: 'Bearer {secret}' },
      access: { kind: 'groups', groups: ['devs'] },
    });
    expect(res.status).toBe(200);
    expect(ServerView.parse(await res.json()).access).toEqual({ kind: 'groups', groups: ['devs'] });
    const events =
      await pg.sql`select type from platform.outbox where type = 'catalog.server_changed'`;
    expect(events).toHaveLength(1);
    const audit = AuditList.parse(await (await call(admin, 'GET', '/api/audit')).json());
    const entry = audit.find((a) => a.action === 'server.update');
    expect(entry).toMatchObject({ target: 'gitea', actorId: admin.id });
    expect(entry?.meta['fields']).toContain('access');
  });

  it('409 server_conflict when another admin changed the server meanwhile; nothing written', async () => {
    await create();
    const secretOf = async () =>
      (await pg.sql<{ secret: Buffer }[]>`select secret from catalog.servers`)[0]?.secret;
    const before = await secretOf();
    const servers = container.catalogSync.servers;
    const findBySlug = servers.findBySlug.bind(servers);
    servers.findBySlug = async (slug) => {
      const loaded = await findBySlug(slug);
      // Another admin's change commits between this request's read and its write.
      await pg.sql`update catalog.servers set version = version + 1, description = 'theirs'`;
      return loaded;
    };
    try {
      const res = await call(admin, 'PUT', '/api/servers/gitea/secret', { secret: 'for-old-url' });
      expect(res.status).toBe(409);
      expect(ErrorBody.parse(await res.json()).error.code).toBe('server_conflict');
    } finally {
      servers.findBySlug = findBySlug;
    }
    const [row] = await pg.sql<{ description: string }[]>`select description from catalog.servers`;
    expect(row?.description).toBe('theirs');
    expect(await secretOf()).toEqual(before);
  });

  it('404 for an unknown server', async () => {
    expect((await call(admin, 'GET', '/api/servers/nope')).status).toBe(404);
  });
});

describe('catalog, status, sync, tools', () => {
  it('GET /api/catalog shows members only accessible active servers and visible tools', async () => {
    await create();
    await create(
      sharedInput({ slug: 'closed', name: 'Closed', access: { kind: 'groups', groups: ['ops'] } }),
    );
    expect(
      (await call(admin, 'PUT', '/api/servers/gitea/tools/delete_repo/hidden', { hidden: true }))
        .status,
    ).toBe(204);
    const body = await (await call(member, 'GET', '/api/catalog')).text();
    const catalog = CatalogResponse.parse(JSON.parse(body));
    expect(catalog.map((s) => s.slug)).toEqual(['gitea']);
    expect(catalog[0]?.tools.map((t) => t.ref)).toEqual(['gitea.list_repos']);
    expect(body).not.toContain(up.url);
    // The admin still lists the hidden tool, flagged.
    const tools = AdminToolList.parse(
      await (await call(admin, 'GET', '/api/servers/gitea/tools')).json(),
    );
    expect(tools.find((t) => t.name === 'delete_repo')?.hidden).toBe(true);
  });

  it('disabling a server removes it from the catalog; sync of a disabled one is 404', async () => {
    await create();
    expect(
      (await call(admin, 'POST', '/api/servers/gitea/status', { status: 'disabled' })).status,
    ).toBe(204);
    expect(CatalogResponse.parse(await (await call(member, 'GET', '/api/catalog')).json())).toEqual(
      [],
    );
    expect((await call(admin, 'POST', '/api/servers/gitea/sync')).status).toBe(404);
    const list = ServerList.parse(await (await call(admin, 'GET', '/api/servers')).json());
    expect(list[0]?.status).toBe('disabled');
  });

  it('POST /sync returns the counts; an upstream failure is an error and lands in lastSyncError', async () => {
    await create();
    up.setTools([{ name: 'list_repos' }, { name: 'new_tool' }]);
    const ok = await call(admin, 'POST', '/api/servers/gitea/sync');
    expect(SyncResult.parse(await ok.json())).toEqual({ added: 1, changed: 1, removed: 1 });
    up.status(500);
    const bad = await call(admin, 'POST', '/api/servers/gitea/sync');
    expect(bad.status).toBe(502);
    const view = ServerView.parse(await (await call(admin, 'GET', '/api/servers/gitea')).json());
    expect(view.lastSyncError).not.toBeNull();
    up.reset();
  });

  it('hiding a tool of an unknown server or tool is 404', async () => {
    await create();
    const res = await call(admin, 'PUT', '/api/servers/gitea/tools/nope/hidden', { hidden: true });
    expect(res.status).toBe(404);
  });
});

const at = (m: number) => new Date(Date.UTC(2026, 9, 30, 12, m));

describe('GET /api/calls', () => {
  it('lists newest first with filters, default limit 100, without arguments', async () => {
    await Promise.all(
      [0, 1, 2].map((i) =>
        container.gateway.callLog.record({
          userId: member.id,
          tokenId: null,
          server: i === 0 ? 'other' : 'gitea',
          tool: 'list_repos',
          startedAt: at(i),
          durationMs: 5,
          outcome: i === 2 ? 'denied:quota' : 'ok',
        }),
      ),
    );
    const list = async (q: string) => {
      const res = await call(admin, 'GET', `/api/calls${q}`);
      expect(res.status).toBe(200);
      return CallList.parse(await res.json());
    };
    expect((await list('')).map((e) => e.startedAt)).toEqual([
      at(2).toISOString(),
      at(1).toISOString(),
      at(0).toISOString(),
    ]);
    expect(await list('?server=other')).toHaveLength(1);
    expect(await list('?outcome=denied:quota')).toHaveLength(1);
    expect(await list(`?user=${member.id}&limit=2`)).toHaveLength(2);
    expect(await list(`?before=${at(1).toISOString()}`)).toHaveLength(1);
    expect((await call(admin, 'GET', '/api/calls?outcome=bogus')).status).toBe(422);
    expect((await call(admin, 'GET', '/api/calls?limit=0')).status).toBe(422);
  });
});

describe('GET /api/audit', () => {
  it('pages back with before and limit', async () => {
    await create();
    await call(admin, 'POST', '/api/servers/gitea/status', { status: 'disabled' });
    const first = AuditList.parse(await (await call(admin, 'GET', '/api/audit?limit=1')).json());
    expect(first).toHaveLength(1);
    const rest = AuditList.parse(
      await (await call(admin, 'GET', `/api/audit?before=${first[0]?.id}`)).json(),
    );
    expect(rest.every((a) => Number(a.id) < Number(first[0]?.id))).toBe(true);
    expect(rest.length).toBeGreaterThan(0);
  });
});

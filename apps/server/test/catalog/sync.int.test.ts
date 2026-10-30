import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import {
  createServer,
  type Principal,
  setToolHidden,
  type SyncDeps,
  syncServer,
} from '../../src/modules/catalog/index.ts';
import { buildContainer } from '../../src/platform/container.ts';
import { loadConfig } from '../../src/platform/config.ts';
import { type FakeUpstream, startFakeUpstream } from '../fake-upstream.ts';
import { startPg, truncateAll } from '../pg.ts';

const ADMIN: Principal = {
  userId: '33333333-3333-3333-3333-333333333333',
  role: 'admin',
  groups: [],
};

let pg: Awaited<ReturnType<typeof startPg>>;
let up: FakeUpstream;
let deps: SyncDeps;

beforeAll(async () => {
  [pg, up] = await Promise.all([startPg(), startFakeUpstream()]);
  const config = loadConfig({
    DATABASE_URL: pg.url,
    PUBLIC_URL: 'http://localhost:3000',
    SECRETS_KEY: Buffer.alloc(32, 7).toString('base64'),
    OIDC_ISSUER: 'https://id.example.com',
    OIDC_CLIENT_ID: 'x',
    OIDC_CLIENT_SECRET: 'y',
    UPSTREAM_MAX_RESPONSE_BYTES: String(1024 * 1024),
    LOG_LEVEL: 'silent',
  });
  deps = buildContainer(config, pg.sql).catalogSync;
});
afterAll(async () => {
  await Promise.all([pg.stop(), up.close()]);
});
beforeEach(async () => {
  await truncateAll(pg.sql);
  up.reset();
  up.requests.length = 0;
});

async function seed(auth: Parameters<typeof createServer>[1]['auth'], secret?: string) {
  const r = await createServer(deps, {
    principal: ADMIN,
    slug: 'fake',
    name: 'Fake',
    description: 'test upstream',
    url: up.url,
    auth,
    access: { kind: 'all' },
    timeoutMs: 5000,
    ...(secret === undefined ? {} : { secret }),
  });
  if (!r.ok) throw r.error;
}

const tools = () =>
  pg.sql<{ name: string; hidden: boolean; removed: boolean }[]>`
    select name, hidden, removed_at is not null as removed from catalog.tools order by name`;
const server = async () => {
  const [row] = await pg.sql<
    { last_sync_at: Date | null; last_sync_error: string | null; sync_user_id: string | null }[]
  >`select last_sync_at, last_sync_error, sync_user_id from catalog.servers where slug = 'fake'`;
  return row;
};

describe('syncServer over Postgres and an MCP upstream (composition)', () => {
  it('shared_secret: pages through tools/list with the secret header, stores the snapshot', async () => {
    await seed({ kind: 'shared_secret', header: 'X-API-Key', template: 'key {secret}' }, 'k-1');
    up.setTools(
      [{ name: 'a' }, { name: 'b', annotations: { readOnlyHint: true } }, { name: 'c.d' }],
      2,
    );
    const r = await syncServer(deps, { principal: ADMIN, slug: 'fake' });
    expect(r).toEqual({ ok: true, value: { added: 3, changed: 0, removed: 0 } });
    expect(await tools()).toEqual([
      { name: 'a', hidden: false, removed: false },
      { name: 'b', hidden: false, removed: false },
      { name: 'c.d', hidden: false, removed: false },
    ]);
    expect(up.requests.length).toBeGreaterThan(0);
    for (const req of up.requests) expect(req.headers['x-api-key']).toBe('key k-1');
    expect(await server()).toMatchObject({ last_sync_error: null, sync_user_id: ADMIN.userId });
    const events = await pg.sql<{ type: string }[]>`
      select type from platform.outbox where type like 'catalog.tools%'`;
    expect(events.map((e) => e.type)).toEqual(['catalog.tools_changed']);
  });

  it('a vanished tool is removed; it returns still hidden', async () => {
    await seed({ kind: 'none' });
    up.setTools([{ name: 'a' }, { name: 'drop' }]);
    await syncServer(deps, { principal: ADMIN, slug: 'fake' });
    await setToolHidden(deps, { principal: ADMIN, ref: 'fake.drop', hidden: true });
    up.setTools([{ name: 'a' }]);
    expect(await syncServer(deps, { principal: null, slug: 'fake' })).toMatchObject({
      value: { removed: 1 },
    });
    up.setTools([{ name: 'a' }, { name: 'drop' }]);
    await syncServer(deps, { principal: null, slug: 'fake' });
    expect(await tools()).toEqual([
      { name: 'a', hidden: false, removed: false },
      { name: 'drop', hidden: true, removed: false },
    ]);
  });

  it('upstream 401: last_sync_error is set and the tools stay as they were', async () => {
    await seed({ kind: 'none' });
    up.setTools([{ name: 'a' }]);
    await syncServer(deps, { principal: ADMIN, slug: 'fake' });
    up.status(401);
    up.setTools([]);
    const r = await syncServer(deps, { principal: null, slug: 'fake' });
    expect(r.ok ? 'ok' : r.error.code).toBe('sync_failed');
    expect(await tools()).toEqual([{ name: 'a', hidden: false, removed: false }]);
    expect((await server())?.last_sync_error).toBe('Upstream rejected the credentials (HTTP 401)');
  });

  it('oauth_user without the admin’s connection: needs_connection, upstream not called', async () => {
    await seed({
      kind: 'oauth_user',
      oauth: {
        authorizeUrl: 'https://git.example.com/a',
        tokenUrl: 'https://git.example.com/t',
        clientId: 'c',
        scopes: [],
      },
      header: 'Authorization',
      template: 'Bearer {secret}',
    });
    const r = await syncServer(deps, { principal: ADMIN, slug: 'fake' });
    expect(r.ok ? 'ok' : r.error.code).toBe('needs_connection');
    expect(up.requests).toEqual([]);
    expect((await server())?.last_sync_error).toBe('An administrator needs to connect this server');
  });
});

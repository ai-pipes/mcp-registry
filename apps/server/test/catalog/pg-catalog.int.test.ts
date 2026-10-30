import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { createPgCatalog } from '../../src/modules/catalog/index.ts';
import { Server } from '../../src/modules/catalog/domain/server.ts';
import { SyncRecord } from '../../src/modules/catalog/domain/sync.ts';
import { Tool, type ToolSnapshot } from '../../src/modules/catalog/domain/tool.ts';
import { CatalogError } from '../../src/modules/catalog/domain/errors.ts';
import { startPg, truncateAll } from '../pg.ts';

let pg: Awaited<ReturnType<typeof startPg>>;
let servers: ReturnType<typeof createPgCatalog>['servers'];
let tools: ReturnType<typeof createPgCatalog>['tools'];
beforeAll(async () => {
  pg = await startPg();
  ({ servers, tools } = createPgCatalog(pg.sql));
});
afterAll(async () => {
  await pg.stop();
});
beforeEach(async () => {
  await truncateAll(pg.sql);
});

const t0 = new Date('2026-10-30T10:00:00.000Z');
const t1 = new Date('2026-10-30T11:00:00.000Z');

function newServer(slug = 'gitea', over: Partial<Parameters<typeof Server.create>[0]> = {}) {
  const r = Server.create({
    slug,
    name: 'Gitea',
    description: 'Git hosting',
    url: 'https://mcp.example.com/gitea',
    auth: {
      kind: 'oauth_user',
      oauth: {
        authorizeUrl: 'https://git.example.com/authorize',
        tokenUrl: 'https://git.example.com/token',
        clientId: 'cid',
        scopes: ['read', 'write'],
        extraAuthorizeParams: { access_type: 'offline' },
      },
      header: 'Authorization',
      template: 'Bearer {secret}',
    },
    access: { kind: 'groups', groups: ['dev'] },
    timeoutMs: 30_000,
    dailyLimit: 10,
    now: t0,
    ...over,
  });
  if (!r.ok) throw r.error;
  return r.value;
}

const snap = (name: string, over: Partial<ToolSnapshot> = {}): ToolSnapshot => ({
  name,
  title: `T ${name}`,
  description: `does ${name}`,
  inputSchema: { type: 'object', properties: { b: { type: 'string' }, a: { type: 'number' } } },
  outputSchema: null,
  annotations: { readOnlyHint: true },
  ...over,
});

const outbox = () =>
  pg.sql<
    { type: string; payload: unknown }[]
  >`select type, payload from platform.outbox order by id`;

describe('PgServerRepository', () => {
  it('round-trips a server, including the sealed secret bytes', async () => {
    const s = newServer();
    s.setSealedSecret(new Uint8Array([0, 1, 2, 255]), t0);
    await servers.add(s);
    const got = await servers.findBySlug('gitea');
    expect(got).not.toBeNull();
    expect(got?.auth).toEqual(s.auth);
    expect(got?.access).toEqual({ kind: 'groups', groups: ['dev'] });
    expect(got?.sealedSecret).toEqual(new Uint8Array([0, 1, 2, 255]));
    expect(got).toMatchObject({
      name: 'Gitea',
      timeoutMs: 30_000,
      dailyLimit: 10,
      status: 'active',
      lastSyncAt: null,
      createdAt: t0,
    });
    expect(await servers.findBySlug('nope')).toBeNull();
  });

  it('stores the sealed bytes verbatim', async () => {
    const s = newServer();
    s.setSealedSecret(new TextEncoder().encode('opaque-bytes'), t0);
    await servers.add(s);
    const [row] = await pg.sql<{ secret: Buffer | null }[]>`select secret from catalog.servers`;
    expect(row?.secret?.toString()).toBe('opaque-bytes'); // sealing is the use case's job
  });

  it('writes events to the outbox in the same call', async () => {
    await servers.add(newServer());
    expect(await outbox()).toEqual([
      { type: 'catalog.server_changed', payload: { server: 'gitea' } },
    ]);
  });

  it('a taken slug throws server_exists and writes no event', async () => {
    await servers.add(newServer());
    await expect(servers.add(newServer())).rejects.toMatchObject({ code: 'server_exists' });
    expect(await outbox()).toHaveLength(1);
  });

  it('save updates fields but leaves synchronisation state alone', async () => {
    await servers.add(newServer());
    await pg.sql`update catalog.servers set last_sync_at = ${t1}, last_sync_error = 'boom',
      sync_user_id = '11111111-1111-1111-1111-111111111111'`;
    const s = await servers.findBySlug('gitea');
    s?.update({ name: 'Gitea 2', dailyLimit: null }, t1);
    s?.setStatus('disabled', t1);
    s?.setSealedSecret(new Uint8Array([7]), t1);
    await servers.save(s!);
    const got = await servers.findBySlug('gitea');
    expect(got).toMatchObject({
      name: 'Gitea 2',
      dailyLimit: null,
      status: 'disabled',
      lastSyncAt: t1,
      lastSyncError: 'boom',
      syncUserId: '11111111-1111-1111-1111-111111111111',
      updatedAt: t1,
    });
    expect(got?.sealedSecret).toEqual(new Uint8Array([7]));
    expect((await outbox()).map((e) => e.type)).toEqual([
      'catalog.server_changed',
      'catalog.server_changed',
    ]);
  });

  it('a stale save is refused, not merged: a secret set meanwhile is not undone (M2)', async () => {
    const s = newServer('yt', {
      auth: { kind: 'shared_secret', header: 'X-Key', template: '{secret}' },
    });
    s.setSealedSecret(new Uint8Array([1]), t0);
    await servers.add(s);
    // Admin A's updateServer (new URL) loads the server; admin B's setServerSecret commits first.
    const a = await servers.findBySlug('yt');
    const b = await servers.findBySlug('yt');
    b?.setSealedSecret(new Uint8Array([2]), t1);
    await servers.save(b!);
    a?.update({ url: 'https://elsewhere.example.com/mcp' }, t1);
    await expect(servers.save(a!)).rejects.toMatchObject({ code: 'server_conflict' });
    const got = await servers.findBySlug('yt');
    expect(got?.url).toBe('https://mcp.example.com/gitea');
    expect(got?.sealedSecret).toEqual(new Uint8Array([2]));
  });

  it('the other way round: a secret entered for the old URL is refused after the URL changed', async () => {
    const s = newServer('yt', {
      auth: { kind: 'shared_secret', header: 'X-Key', template: '{secret}' },
    });
    s.setSealedSecret(new Uint8Array([1]), t0);
    await servers.add(s);
    // Admin B loads to set a secret for the current URL; admin A's URL change commits first.
    const b = await servers.findBySlug('yt');
    const a = await servers.findBySlug('yt');
    a?.update({ url: 'https://elsewhere.example.com/mcp' }, t1);
    await servers.save(a!);
    b?.setSealedSecret(new Uint8Array([2]), t1);
    await expect(servers.save(b!)).rejects.toMatchObject({ code: 'server_conflict' });
    const got = await servers.findBySlug('yt');
    expect(got?.url).toBe('https://elsewhere.example.com/mcp');
    // The secret meant for the old upstream is not bound to the new one.
    expect(got?.sealedSecret).toEqual(new Uint8Array([1]));
    // Reloaded, the change goes through.
    const fresh = await servers.findBySlug('yt');
    fresh?.setSealedSecret(new Uint8Array([3]), t1);
    await servers.save(fresh!);
    expect((await servers.findBySlug('yt'))?.sealedSecret).toEqual(new Uint8Array([3]));
  });

  it('save after a key rotation re-sealed the secret keeps the new ciphertext (M2)', async () => {
    const s = newServer('yt', {
      auth: { kind: 'shared_secret', header: 'X-Key', template: '{secret}' },
    });
    s.setSealedSecret(new Uint8Array([1]), t0);
    await servers.add(s);
    const stale = await servers.findBySlug('yt');
    expect(await servers.replaceSealedSecret('yt', new Uint8Array([1]), new Uint8Array([9]))).toBe(
      true,
    );
    stale?.setStatus('disabled', t1);
    await servers.save(stale!);
    expect((await servers.findBySlug('yt'))?.sealedSecret).toEqual(new Uint8Array([9]));
  });

  it('a change of auth kind still clears the stored secret on save', async () => {
    const s = newServer('yt', {
      auth: { kind: 'shared_secret', header: 'X-Key', template: '{secret}' },
    });
    s.setSealedSecret(new Uint8Array([1]), t0);
    await servers.add(s);
    const loaded = await servers.findBySlug('yt');
    loaded?.update({ auth: { kind: 'none' } }, t1);
    await servers.save(loaded!);
    expect((await servers.findBySlug('yt'))?.sealedSecret).toBeNull();
  });

  it('save of an unknown server throws not_found', async () => {
    await expect(servers.save(newServer())).rejects.toBeInstanceOf(CatalogError);
  });

  it('saveSyncState writes only the synchronisation state, no event', async () => {
    const admin = '22222222-2222-2222-2222-222222222222';
    await servers.add(newServer());
    await pg.sql`truncate platform.outbox`;
    await servers.saveSyncState('gitea', SyncRecord.success(admin, t1));
    expect(await servers.findBySlug('gitea')).toMatchObject({
      name: 'Gitea',
      lastSyncAt: t1,
      lastSyncError: null,
      syncUserId: admin,
      updatedAt: t0,
    });
    // A later failure (and a scheduler success) keep the admin.
    const t2 = new Date('2026-10-30T12:00:00.000Z');
    await servers.saveSyncState('gitea', SyncRecord.failure('needs_connection', t2));
    expect(await servers.findBySlug('gitea')).toMatchObject({
      lastSyncAt: t2,
      lastSyncError: 'An administrator needs to connect this server',
      syncUserId: admin,
    });
    expect(await outbox()).toEqual([]);
  });

  it('saveSyncState ignores an attempt older than the stored one', async () => {
    const a = '22222222-2222-2222-2222-222222222222';
    const b = '44444444-4444-4444-4444-444444444444';
    await servers.add(newServer());
    await servers.saveSyncState('gitea', SyncRecord.success(b, t1));
    await servers.saveSyncState('gitea', SyncRecord.success(a, t0));
    await servers.saveSyncState('gitea', SyncRecord.failure('timeout', t0));
    expect(await servers.findBySlug('gitea')).toMatchObject({
      lastSyncAt: t1,
      lastSyncError: null,
      syncUserId: b,
    });
  });

  it('saveSyncState of an unknown server throws not_found', async () => {
    await expect(servers.saveSyncState('nope', SyncRecord.success(null, t0))).rejects.toMatchObject(
      { code: 'not_found' },
    );
  });

  it('lists ordered by slug with counts of visible tools only', async () => {
    await servers.add(newServer('beta'));
    await servers.add(newServer('alpha'));
    await tools.applySnapshot('alpha', [snap('a'), snap('b'), snap('c')], t0);
    const hidden = await tools.find('alpha', 'b');
    hidden?.setHidden(true, t1);
    await tools.save(hidden!);
    await tools.applySnapshot('alpha', [snap('a'), snap('b')], t1); // c removed
    const list = await servers.list();
    expect(list.map((l) => [l.server.slug, l.toolCount])).toEqual([
      ['alpha', 1],
      ['beta', 0],
    ]);
  });
});

describe('PgToolRepository', () => {
  beforeEach(async () => {
    await servers.add(newServer());
    await pg.sql`truncate platform.outbox`;
  });

  it('stores a snapshot and reads it back with jsonb intact', async () => {
    const r = await tools.applySnapshot('gitea', [snap('issues.create'), snap('list')], t0);
    expect(r).toEqual({
      changedRefs: ['gitea.issues.create', 'gitea.list'],
      addedRefs: ['gitea.issues.create', 'gitea.list'],
      removedRefs: [],
    });
    const t = await tools.find('gitea', 'issues.create');
    expect(t).toBeInstanceOf(Tool);
    expect(t).toMatchObject({
      ref: 'gitea.issues.create',
      title: 'T issues.create',
      outputSchema: null,
      annotations: { readOnlyHint: true },
      hidden: false,
      removedAt: null,
      syncedAt: t0,
    });
    expect(t?.inputSchema).toEqual(snap('x').inputSchema);
    expect((await tools.listByServer('gitea')).map((x) => x.name)).toEqual([
      'issues.create',
      'list',
    ]);
    expect(await tools.find('gitea', 'zz')).toBeNull();
    expect(await outbox()).toEqual([
      {
        type: 'catalog.tools_changed',
        payload: { server: 'gitea', refs: ['gitea.issues.create', 'gitea.list'] },
      },
    ]);
  });

  it('a second identical snapshot is a no-op (jsonb key order does not matter)', async () => {
    await tools.applySnapshot('gitea', [snap('a')], t0);
    await pg.sql`truncate platform.outbox`;
    const r = await tools.applySnapshot('gitea', [snap('a')], t1);
    expect(r).toEqual({ changedRefs: [], addedRefs: [], removedRefs: [] });
    expect(await outbox()).toEqual([]);
    expect((await tools.find('gitea', 'a'))?.syncedAt).toEqual(t0);
  });

  it('hidden survives sync, removal and return', async () => {
    await tools.applySnapshot('gitea', [snap('a'), snap('b')], t0);
    const a = await tools.find('gitea', 'a');
    a?.setHidden(true, t0);
    await tools.save(a!);
    expect((await outbox()).map((e) => e.type)).toEqual([
      'catalog.tools_changed',
      'catalog.tools_changed',
    ]);
    await pg.sql`truncate platform.outbox`;
    a?.setHidden(true, t0); // already hidden: nothing new
    await tools.save(a!);
    expect(await outbox()).toEqual([]);

    await tools.applySnapshot('gitea', [snap('a', { description: 'changed' })], t1);
    expect(await tools.find('gitea', 'a')).toMatchObject({ hidden: true, description: 'changed' });
    expect(await tools.find('gitea', 'b')).toMatchObject({ removedAt: t1 });
    expect((await outbox()).map((e) => e.type)).toEqual([
      'catalog.tools_changed',
      'catalog.tools_removed',
    ]);

    await tools.applySnapshot('gitea', [snap('a'), snap('b')], t1);
    expect(await tools.find('gitea', 'b')).toMatchObject({ removedAt: null, hidden: false });
  });

  it('applySnapshot for an unknown server throws not_found', async () => {
    await expect(tools.applySnapshot('nope', [snap('a')], t0)).rejects.toMatchObject({
      code: 'not_found',
    });
  });

  it('deleting nothing: tools of other servers are untouched', async () => {
    await servers.add(newServer('other'));
    await tools.applySnapshot('other', [snap('z')], t0);
    await tools.applySnapshot('gitea', [], t1);
    expect(await tools.find('other', 'z')).toMatchObject({ removedAt: null });
  });
});

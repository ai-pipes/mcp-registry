import { beforeEach, describe, expect, it } from 'vite-plus/test';
import { makeCatalogDeps } from '../../../../test/fakes/in-memory-catalog.ts';
import { Tool, type ToolSnapshot } from '../domain/tool.ts';
import {
  createServer,
  setServerSecret,
  setServerStatus,
  updateServer,
} from './commands/servers.ts';
import { applyToolSnapshot, setToolHidden } from './commands/tools.ts';
import type { CatalogDeps } from './deps.ts';
import type { Principal } from './ports.ts';
import {
  getServerForUpstream,
  getServerSecret,
  listAllServers,
  listServersFor,
} from './queries/servers.ts';
import { listServerSlugs, listToolsForIndex } from './queries/index-source.ts';
import { getToolFor, listServerTools } from './queries/tools.ts';

const admin: Principal = { userId: 'admin-1', role: 'admin', groups: [] };
const member = (...groups: string[]): Principal => ({ userId: 'u-1', role: 'member', groups });

let h: ReturnType<typeof makeCatalogDeps>;
let deps: CatalogDeps;
beforeEach(() => {
  h = makeCatalogDeps();
  deps = h.deps;
});

const snap = (name: string): ToolSnapshot => ({
  name,
  title: null,
  description: `does ${name}`,
  inputSchema: { type: 'object' },
  outputSchema: null,
  annotations: {},
});

const newServer = (over: Record<string, unknown> = {}) =>
  createServer(deps, {
    principal: admin,
    slug: 'gitea',
    name: 'Gitea',
    description: 'Git hosting',
    url: 'https://mcp.example.com/gitea',
    auth: { kind: 'none' },
    access: { kind: 'all' },
    ...over,
  });

const code = (r: { ok: boolean; error?: { code: string } }) => (r.ok ? 'ok' : r.error?.code);

const SHARED = { kind: 'shared_secret', header: 'X-API-Key', template: '{secret}' } as const;

async function seed(slug = 'gitea', access: Record<string, unknown> = { kind: 'all' }) {
  const r = await newServer({ slug, access, auth: SHARED });
  if (!r.ok) throw r.error;
  await applyToolSnapshot(deps, { server: slug, tools: [snap('issues.create'), snap('list')] });
  h.outbox.length = 0;
  h.audit.length = 0;
}

describe('admin-only commands', () => {
  it('refuse a member with forbidden and change nothing', async () => {
    await seed();
    const p = member();
    expect(code(await newServer({ principal: p, slug: 'other' }))).toBe('forbidden');
    expect(
      code(await updateServer(deps, { principal: p, slug: 'gitea', changes: { name: 'x' } })),
    ).toBe('forbidden');
    expect(code(await setServerSecret(deps, { principal: p, slug: 'gitea', secret: 's' }))).toBe(
      'forbidden',
    );
    expect(
      code(await setServerStatus(deps, { principal: p, slug: 'gitea', status: 'disabled' })),
    ).toBe('forbidden');
    expect(code(await setToolHidden(deps, { principal: p, ref: 'gitea.list', hidden: true }))).toBe(
      'forbidden',
    );
    expect(code(await listAllServers(deps, { principal: p }))).toBe('forbidden');
    expect(code(await listServerTools(deps, { principal: p, slug: 'gitea' }))).toBe('forbidden');
    expect(h.servers.rows.size).toBe(1);
    expect(h.outbox).toEqual([]);
    expect(h.audit).toEqual([]);
  });
});

describe('createServer', () => {
  it('creates, audits and emits server_changed', async () => {
    const r = await newServer({ timeoutMs: 5000, dailyLimit: 3 });
    expect(r.ok && r.value).toMatchObject({
      slug: 'gitea',
      status: 'active',
      secretSet: false,
      timeoutMs: 5000,
      dailyLimit: 3,
      toolCount: 0,
    });
    expect(h.audit).toEqual([
      { actorId: 'admin-1', action: 'server.create', target: 'gitea', meta: { auth: 'none' } },
    ]);
    expect(h.outbox.map((e) => e.type)).toEqual(['catalog.server_changed']);
  });

  it('seals an initial secret and audits server.secret_set without its value', async () => {
    const r = await newServer({
      auth: { kind: 'shared_secret', header: 'X-API-Key', template: '{secret}' },
      secret: 'hunter2',
    });
    expect(r.ok && r.value.secretSet).toBe(true);
    expect(h.audit.map((a) => a.action)).toEqual(['server.create', 'server.secret_set']);
    expect(JSON.stringify(h.audit)).not.toContain('hunter2');
    expect(JSON.stringify(r)).not.toContain('hunter2');
    expect(await getServerSecret(deps, 'gitea')).toBe('hunter2');
  });

  it('reports domain validation errors', async () => {
    expect(code(await newServer({ slug: 'Bad' }))).toBe('invalid_slug');
    expect(code(await newServer({ url: 'nope' }))).toBe('invalid_server');
    expect(code(await newServer({ auth: { kind: 'pat_user', header: 'A', template: 'no' } }))).toBe(
      'invalid_auth',
    );
    expect(code(await newServer({ auth: SHARED, secret: '  ' }))).toBe('invalid_server');
    expect(code(await newServer({ secret: 'k' }))).toBe('invalid_server'); // auth none: no secret
    expect(h.servers.rows.size).toBe(0);
  });

  it('rejects a taken slug', async () => {
    await newServer();
    expect(code(await newServer())).toBe('server_exists');
  });
});

describe('updateServer', () => {
  it('applies changes, audits changed field names only', async () => {
    await seed();
    const r = await updateServer(deps, {
      principal: admin,
      slug: 'gitea',
      changes: { name: 'Gitea!', dailyLimit: 7 },
    });
    expect(r.ok && r.value).toMatchObject({ name: 'Gitea!', dailyLimit: 7, toolCount: 2 });
    expect(h.audit).toEqual([
      {
        actorId: 'admin-1',
        action: 'server.update',
        target: 'gitea',
        meta: { fields: ['name', 'dailyLimit'] },
      },
    ]);
    expect(h.outbox.map((e) => e.type)).toEqual(['catalog.server_changed']);
  });

  it('is not_found for an unknown server and returns validation errors', async () => {
    expect(code(await updateServer(deps, { principal: admin, slug: 'x1', changes: {} }))).toBe(
      'not_found',
    );
    await seed();
    expect(
      code(await updateServer(deps, { principal: admin, slug: 'gitea', changes: { url: 'nope' } })),
    ).toBe('invalid_server');
    expect(h.audit).toEqual([]);
  });
});

describe('updateServer and the secret', () => {
  it('drops the secret on an auth-kind change and audits server.secret_clear', async () => {
    await seed();
    await setServerSecret(deps, { principal: admin, slug: 'gitea', secret: 'old-key' });
    h.audit.length = 0;
    const r = await updateServer(deps, {
      principal: admin,
      slug: 'gitea',
      changes: { auth: { kind: 'pat_user', header: 'Authorization', template: 'Bearer {secret}' } },
    });
    expect(r.ok && r.value.secretSet).toBe(false);
    expect(await getServerSecret(deps, 'gitea')).toBeNull();
    expect(h.audit.map((a) => a.action)).toEqual(['server.update', 'server.secret_clear']);
    expect(JSON.stringify(h.audit)).not.toContain('old-key');
  });

  it('keeps the secret while the auth kind stays', async () => {
    await seed();
    await setServerSecret(deps, { principal: admin, slug: 'gitea', secret: 'k' });
    h.audit.length = 0;
    await updateServer(deps, {
      principal: admin,
      slug: 'gitea',
      changes: { auth: { ...SHARED, header: 'X-Other' } },
    });
    expect(await getServerSecret(deps, 'gitea')).toBe('k');
    expect(h.audit.map((a) => a.action)).toEqual(['server.update']);
  });

  it('an access change emits server_changed', async () => {
    await seed();
    await updateServer(deps, {
      principal: admin,
      slug: 'gitea',
      changes: { access: { kind: 'groups', groups: ['dev'] } },
    });
    expect(h.outbox.map((e) => e.type)).toEqual(['catalog.server_changed']);
  });
});

describe('setServerSecret', () => {
  it('refuses a secret for a server whose auth mode has none', async () => {
    await newServer({ slug: 'plain' });
    expect(
      code(await setServerSecret(deps, { principal: admin, slug: 'plain', secret: 'k' })),
    ).toBe('invalid_server');
    expect(h.servers.rows.get('plain')?.secretSet).toBe(false);
    expect(
      code(await setServerSecret(deps, { principal: admin, slug: 'plain', secret: null })),
    ).toBe('ok');
  });

  it('sets and clears, never auditing the value', async () => {
    await seed();
    expect(
      code(await setServerSecret(deps, { principal: admin, slug: 'gitea', secret: 'topsecret' })),
    ).toBe('ok');
    expect(h.servers.rows.get('gitea')?.secretSet).toBe(true);
    expect(await getServerSecret(deps, 'gitea')).toBe('topsecret');
    expect(
      code(await setServerSecret(deps, { principal: admin, slug: 'gitea', secret: null })),
    ).toBe('ok');
    expect(await getServerSecret(deps, 'gitea')).toBeNull();
    expect(h.audit).toEqual([
      { actorId: 'admin-1', action: 'server.secret_set', target: 'gitea' },
      { actorId: 'admin-1', action: 'server.secret_clear', target: 'gitea' },
    ]);
    expect(JSON.stringify(h.audit)).not.toContain('topsecret');
  });

  it('rejects a blank secret and an unknown server', async () => {
    await seed();
    expect(code(await setServerSecret(deps, { principal: admin, slug: 'gitea', secret: '' }))).toBe(
      'invalid_server',
    );
    expect(code(await setServerSecret(deps, { principal: admin, slug: 'zz', secret: 'a' }))).toBe(
      'not_found',
    );
  });
});

describe('setServerStatus', () => {
  it('disables and enables, audits and emits only on change', async () => {
    await seed();
    expect(
      code(await setServerStatus(deps, { principal: admin, slug: 'gitea', status: 'disabled' })),
    ).toBe('ok');
    expect(h.audit).toEqual([
      {
        actorId: 'admin-1',
        action: 'server.status',
        target: 'gitea',
        meta: { status: 'disabled' },
      },
    ]);
    expect(h.outbox.map((e) => e.type)).toEqual(['catalog.server_changed']);
    await setServerStatus(deps, { principal: admin, slug: 'gitea', status: 'disabled' });
    expect(h.audit).toHaveLength(1);
    expect(h.outbox).toHaveLength(1);
    expect(
      code(await setServerStatus(deps, { principal: admin, slug: 'zz', status: 'active' })),
    ).toBe('not_found');
  });
});

describe('server views', () => {
  it('never contain the secret', async () => {
    await newServer({
      auth: {
        kind: 'oauth_user',
        oauth: {
          authorizeUrl: 'https://a.example/a',
          tokenUrl: 'https://a.example/t',
          clientId: 'cid',
          scopes: [],
        },
        header: 'Authorization',
        template: 'Bearer {secret}',
      },
      secret: 'client-secret-value',
    });
    const all = await listAllServers(deps, { principal: admin });
    const mine = await listServersFor(deps, { principal: member() });
    const created = JSON.stringify([all, mine]);
    expect(Object.keys(mine[0] ?? {}).toSorted()).toEqual([
      'authKind',
      'description',
      'name',
      'slug',
      'toolCount',
    ]);
    expect(mine[0]?.authKind).toBe('oauth_user');
    expect(created).not.toContain('client-secret-value');
    expect(created).not.toContain('sealed');
    expect(all.ok && all.value[0]?.secretSet).toBe(true);
  });

  it('listServersFor shows only active servers the principal can use', async () => {
    await seed('open');
    await seed('devs', { kind: 'groups', groups: ['dev'] });
    await seed('ops', { kind: 'groups', groups: ['ops'] });
    await seed('off');
    await setServerStatus(deps, { principal: admin, slug: 'off', status: 'disabled' });
    const slugs = async (p: Principal) =>
      (await listServersFor(deps, { principal: p })).map((s) => s.slug);
    expect(await slugs(member('dev'))).toEqual(['devs', 'open']);
    expect(await slugs(member())).toEqual(['open']);
    expect(await slugs(admin)).toEqual(['devs', 'open', 'ops']);
    const all = await listAllServers(deps, { principal: admin });
    expect(all.ok && all.value.map((s) => s.slug)).toEqual(['devs', 'off', 'open', 'ops']);
  });
});

describe('getToolFor', () => {
  beforeEach(async () => {
    await seed('gitea');
    await seed('priv', { kind: 'groups', groups: ['dev'] });
  });

  it('returns the view of a visible tool', async () => {
    const r = await getToolFor(deps, { principal: member(), ref: 'gitea.issues.create' });
    expect(r.ok && r.value).toEqual({
      server: 'gitea',
      name: 'issues.create',
      ref: 'gitea.issues.create',
      title: null,
      description: 'does issues.create',
      inputSchema: { type: 'object' },
      outputSchema: null,
      annotations: {},
      hidden: false,
    });
  });

  it('answers not_found for hidden, removed, unknown, disabled and out-of-group', async () => {
    await setToolHidden(deps, { principal: admin, ref: 'gitea.list', hidden: true });
    await applyToolSnapshot(deps, { server: 'gitea', tools: [snap('list')] });
    const get = (ref: string, p: Principal = member()) => getToolFor(deps, { principal: p, ref });
    expect(code(await get('gitea.list'))).toBe('not_found'); // hidden
    expect(code(await get('gitea.issues.create'))).toBe('not_found'); // removed by sync
    expect(code(await get('gitea.nope'))).toBe('not_found');
    expect(code(await get('nope.list'))).toBe('not_found');
    expect(code(await get('priv.list', member('mcp-users')))).toBe('not_found');
    expect(code(await get('priv.list', member('dev')))).toBe('ok');
    expect(code(await get('priv.list', admin))).toBe('ok');
    await setServerStatus(deps, { principal: admin, slug: 'priv', status: 'disabled' });
    expect(code(await get('priv.list', admin))).toBe('not_found');
    expect(code(await get('priv.list', member('dev')))).toBe('not_found');
  });

  it('rejects a malformed ref', async () => {
    expect(code(await getToolFor(deps, { principal: member(), ref: 'gitea' }))).toBe(
      'invalid_tool_ref',
    );
  });
});

describe('tools', () => {
  it('setToolHidden toggles, audits and emits tools_changed once per change', async () => {
    await seed();
    expect(
      code(await setToolHidden(deps, { principal: admin, ref: 'gitea.list', hidden: true })),
    ).toBe('ok');
    expect(h.audit).toEqual([{ actorId: 'admin-1', action: 'tool.hide', target: 'gitea.list' }]);
    expect(h.outbox).toEqual([
      expect.objectContaining({
        type: 'catalog.tools_changed',
        payload: { server: 'gitea', refs: ['gitea.list'] },
      }),
    ]);
    await setToolHidden(deps, { principal: admin, ref: 'gitea.list', hidden: true });
    expect(h.outbox).toHaveLength(1);
    await setToolHidden(deps, { principal: admin, ref: 'gitea.list', hidden: false });
    expect(h.audit.map((a) => a.action)).toEqual(['tool.hide', 'tool.unhide']);
  });

  it('setToolHidden: not_found / invalid ref / removed tool', async () => {
    await seed();
    await applyToolSnapshot(deps, { server: 'gitea', tools: [snap('list')] }); // issues.create removed
    expect(
      code(
        await setToolHidden(deps, { principal: admin, ref: 'gitea.issues.create', hidden: true }),
      ),
    ).toBe('not_found');
    expect(
      code(await setToolHidden(deps, { principal: admin, ref: 'gitea.zz', hidden: true })),
    ).toBe('not_found');
    expect(code(await setToolHidden(deps, { principal: admin, ref: 'bad', hidden: true }))).toBe(
      'invalid_tool_ref',
    );
  });

  it('listServerTools gives admin all non-removed tools including hidden', async () => {
    await seed();
    await setToolHidden(deps, { principal: admin, ref: 'gitea.list', hidden: true });
    await applyToolSnapshot(deps, { server: 'gitea', tools: [snap('list'), snap('new')] });
    const r = await listServerTools(deps, { principal: admin, slug: 'gitea' });
    expect(r.ok && r.value.map((t) => [t.name, t.hidden])).toEqual([
      ['list', true],
      ['new', false],
    ]);
    expect(code(await listServerTools(deps, { principal: admin, slug: 'zz' }))).toBe('not_found');
  });

  it('applyToolSnapshot reports changes and emits events', async () => {
    await seed();
    const r = await applyToolSnapshot(deps, { server: 'gitea', tools: [snap('list'), snap('x')] });
    expect(r).toEqual({
      changedRefs: ['gitea.x'],
      addedRefs: ['gitea.x'],
      removedRefs: ['gitea.issues.create'],
    });
    expect(h.outbox.map((e) => e.type)).toEqual(['catalog.tools_changed', 'catalog.tools_removed']);
    expect(h.tools.rows.size).toBe(3);
    expect([...h.tools.rows.values()].every((t) => t instanceof Tool)).toBe(true);
  });
});

describe('upstream queries', () => {
  it('getServerForUpstream returns call parameters without a secret', async () => {
    await newServer({ timeoutMs: 9000, dailyLimit: 4, secret: undefined });
    const r = await getServerForUpstream(deps, 'gitea');
    expect(r).toEqual({
      slug: 'gitea',
      name: 'Gitea',
      url: 'https://mcp.example.com/gitea',
      auth: { kind: 'none' },
      timeoutMs: 9000,
      dailyLimit: 4,
    });
    expect(await getServerForUpstream(deps, 'zz')).toBeNull();
  });

  it('getServerForUpstream is null for a disabled server', async () => {
    await newServer();
    await setServerStatus(deps, { principal: admin, slug: 'gitea', status: 'disabled' });
    expect(await getServerForUpstream(deps, 'gitea')).toBeNull();
  });

  it('getServerSecret is null for unknown or secretless servers', async () => {
    await newServer();
    expect(await getServerSecret(deps, 'gitea')).toBeNull();
    expect(await getServerSecret(deps, 'zz')).toBeNull();
  });
});

describe('listToolsForIndex', () => {
  it('returns visible tools with the server name and description', async () => {
    await seed();
    await setToolHidden(deps, { principal: admin, ref: 'gitea.list', hidden: true });
    const src = await listToolsForIndex(deps, 'gitea');
    expect(src).toMatchObject({ slug: 'gitea', name: 'Gitea', description: 'Git hosting' });
    expect(src?.tools.map((t) => t.ref)).toEqual(['gitea.issues.create']);
  });

  it('omits removed tools', async () => {
    await seed();
    await applyToolSnapshot(deps, { server: 'gitea', tools: [snap('list')] });
    expect((await listToolsForIndex(deps, 'gitea'))?.tools.map((t) => t.ref)).toEqual([
      'gitea.list',
    ]);
  });

  it('gives a disabled server no tools and an unknown one null', async () => {
    await seed();
    await setServerStatus(deps, { principal: admin, slug: 'gitea', status: 'disabled' });
    expect((await listToolsForIndex(deps, 'gitea'))?.tools).toEqual([]);
    expect(await listToolsForIndex(deps, 'ghost')).toBeNull();
  });
});

describe('listServerSlugs', () => {
  it('lists every server, disabled included', async () => {
    await seed('aa');
    await seed('bb');
    await setServerStatus(deps, { principal: admin, slug: 'bb', status: 'disabled' });
    expect(await listServerSlugs(deps)).toEqual(['aa', 'bb']);
  });
});

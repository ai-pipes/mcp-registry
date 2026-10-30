import { beforeEach, describe, expect, it } from 'vite-plus/test';
import { makeCatalogDeps } from '../../../../../test/fakes/in-memory-catalog.ts';
import {
  err,
  ok,
  type UpstreamClient,
  UpstreamError,
  type UpstreamTarget,
  type UpstreamTool,
} from '../../../../shared-kernel/index.ts';
import type { AuthMode } from '../../domain/auth-mode.ts';
import { createServer, setServerStatus } from './servers.ts';
import type { SyncDeps } from '../deps.ts';
import type { Principal, SyncCredentials } from '../ports.ts';
import { syncAllServers, syncServer } from './sync.ts';
import { setToolHidden } from './tools.ts';

const admin: Principal = { userId: 'admin-1', role: 'admin', groups: [] };
const admin2: Principal = { userId: 'admin-2', role: 'admin', groups: [] };
const member: Principal = { userId: 'u-1', role: 'member', groups: [] };

const SHARED: AuthMode = { kind: 'shared_secret', header: 'X-API-Key', template: 'k {secret}' };
const OAUTH: AuthMode = {
  kind: 'oauth_user',
  oauth: {
    authorizeUrl: 'https://git.example.com/authorize',
    tokenUrl: 'https://git.example.com/token',
    clientId: 'cid',
    scopes: [],
  },
  header: 'Authorization',
  template: 'Bearer {secret}',
};

const tool = (name: string, over: Partial<UpstreamTool> = {}): UpstreamTool => ({
  name,
  description: `does ${name}`,
  inputSchema: { type: 'object' },
  ...over,
});

/**
 * A scripted upstream: answers `tools`, or throws `failure`; remembers each target. With `hold()`
 * the next call waits until the returned `release(answer)` is called.
 */
class FakeUpstream implements UpstreamClient {
  tools: UpstreamTool[] = [];
  failure: Error | null = null;
  targets: UpstreamTarget[] = [];
  private held: ((answer: Promise<UpstreamTool[]>) => void) | null = null;
  private holdNext = false;

  hold(): (answer: UpstreamTool[] | Error) => void {
    this.holdNext = true;
    return (answer) => {
      this.held?.(answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer));
    };
  }

  listTools(t: UpstreamTarget): Promise<UpstreamTool[]> {
    this.targets.push(t);
    if (this.holdNext) {
      this.holdNext = false;
      return new Promise((resolve) => {
        this.held = resolve;
      });
    }
    return this.failure ? Promise.reject(this.failure) : Promise.resolve(this.tools);
  }
  callTool(): Promise<never> {
    return Promise.reject(new Error('not used by sync'));
  }
}

/** Personal tokens by user id; anyone else fails with `failure`. Both admins are active. */
class FakeCredentials implements SyncCredentials {
  tokens = new Map<string, string>();
  activeAdmins = new Set(['admin-1', 'admin-2']);
  failure: 'needs_connection' | 'expired' | 'unavailable' = 'needs_connection';
  asked: string[] = [];
  isActiveAdmin(userId: string) {
    return Promise.resolve(this.activeAdmins.has(userId));
  }
  personalSecret(_server: unknown, userId: string) {
    this.asked.push(userId);
    const token = this.tokens.get(userId);
    return Promise.resolve(token === undefined ? err(this.failure) : ok(token));
  }
}

let h: ReturnType<typeof makeCatalogDeps>;
let upstream: FakeUpstream;
let credentials: FakeCredentials;
let deps: SyncDeps;

beforeEach(() => {
  h = makeCatalogDeps();
  upstream = new FakeUpstream();
  credentials = new FakeCredentials();
  deps = { ...h.deps, upstream, credentials, maxResponseBytes: 1234 };
});

async function seed(auth: AuthMode = { kind: 'none' }, slug = 'gitea', secret?: string) {
  const r = await createServer(deps, {
    principal: admin,
    slug,
    name: 'Gitea',
    description: 'Git hosting',
    url: `https://mcp.example.com/${slug}`,
    auth,
    access: { kind: 'all' },
    timeoutMs: 5000,
    ...(secret === undefined ? {} : { secret }),
  });
  if (!r.ok) throw r.error;
  h.outbox.length = 0;
  h.audit.length = 0;
}

const server = (slug = 'gitea') => {
  const s = h.servers.rows.get(slug);
  if (!s) throw new Error('no server');
  return s;
};
const toolRow = (name: string) => h.tools.rows.get(`gitea\u0000${name}`);
const events = () => h.outbox.map((e) => [e.type, e.payload]);

describe('syncServer', () => {
  it('adds new tools and emits tools_changed for them', async () => {
    await seed();
    upstream.tools = [tool('a'), tool('b', { annotations: { readOnlyHint: true } })];
    const r = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(r).toEqual({ ok: true, value: { added: 2, changed: 0, removed: 0 } });
    expect(events()).toEqual([
      ['catalog.tools_changed', { server: 'gitea', refs: ['gitea.a', 'gitea.b'] }],
    ]);
    expect(toolRow('b')?.annotations).toEqual({ readOnlyHint: true });
    expect(upstream.targets).toEqual([
      {
        url: 'https://mcp.example.com/gitea',
        headers: {},
        timeoutMs: 5000,
        maxResponseBytes: 1234,
      },
    ]);
    expect(server().lastSyncAt).toEqual(h.clock.current);
    expect(server().lastSyncError).toBeNull();
    expect(h.audit).toEqual([
      {
        actorId: 'admin-1',
        action: 'server.sync',
        target: 'gitea',
        meta: { added: 2, changed: 0, removed: 0 },
      },
    ]);
  });

  it('a changed description emits tools_changed for that tool only', async () => {
    await seed();
    upstream.tools = [tool('a'), tool('b')];
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    h.outbox.length = 0;
    upstream.tools = [tool('a'), tool('b', { description: 'new text' })];
    const r = await syncServer(deps, { principal: null, slug: 'gitea' });
    expect(r).toEqual({ ok: true, value: { added: 0, changed: 1, removed: 0 } });
    expect(events()).toEqual([['catalog.tools_changed', { server: 'gitea', refs: ['gitea.b'] }]]);
    expect(toolRow('b')?.description).toBe('new text');
  });

  it('marks a vanished tool removed and emits tools_removed', async () => {
    await seed();
    upstream.tools = [tool('a'), tool('b')];
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    h.outbox.length = 0;
    upstream.tools = [tool('a')];
    const r = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(r).toEqual({ ok: true, value: { added: 0, changed: 0, removed: 1 } });
    expect(toolRow('b')?.removedAt).toEqual(h.clock.current);
    expect(events()).toEqual([['catalog.tools_removed', { server: 'gitea', refs: ['gitea.b'] }]]);
  });

  it('a returning tool keeps hidden: true', async () => {
    await seed();
    upstream.tools = [tool('a'), tool('drop_repo')];
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    await setToolHidden(deps, { principal: admin, ref: 'gitea.drop_repo', hidden: true });
    upstream.tools = [tool('a')];
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    upstream.tools = [tool('a'), tool('drop_repo')];
    const r = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(r).toEqual({ ok: true, value: { added: 1, changed: 0, removed: 0 } });
    expect(toolRow('drop_repo')).toMatchObject({ hidden: true, removedAt: null });
  });

  it('an upstream failure fills last_sync_error and leaves the tools alone', async () => {
    await seed();
    upstream.tools = [tool('a')];
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    h.outbox.length = 0;
    h.audit.length = 0;
    const before = toolRow('a');
    upstream.failure = new UpstreamError('timeout');
    upstream.tools = [];
    h.clock.current = new Date('2026-10-30T12:00:00Z');
    const r = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.code).toBe('sync_failed');
    expect(server().lastSyncError).toBe('Upstream did not respond in time');
    expect(server().lastSyncAt).toEqual(h.clock.current);
    expect(toolRow('a')).toBe(before);
    expect(h.outbox).toEqual([]);
    expect(h.audit).toEqual([
      { actorId: 'admin-1', action: 'server.sync', target: 'gitea', meta: { error: 'timeout' } },
    ]);
  });

  it('a JSON-RPC error to tools/list: its own text, no upstream message stored', async () => {
    await seed();
    upstream.failure = UpstreamError.rejected(-32601, 'Method not found: tools/list secret-xyz');
    const r = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(r.ok).toBe(false);
    expect(server().lastSyncError).toBe('Upstream answered tools/list with a JSON-RPC error');
    expect(JSON.stringify([r, h.audit, server()])).not.toContain('secret-xyz');
  });

  it('a later success clears last_sync_error', async () => {
    await seed();
    upstream.failure = new UpstreamError('transport');
    await syncServer(deps, { principal: null, slug: 'gitea' });
    expect(server().lastSyncError).not.toBeNull();
    upstream.failure = null;
    await syncServer(deps, { principal: null, slug: 'gitea' });
    expect(server().lastSyncError).toBeNull();
  });

  it('does not swallow a non-upstream error', async () => {
    await seed();
    upstream.failure = new Error('bug');
    await expect(syncServer(deps, { principal: admin, slug: 'gitea' })).rejects.toThrow('bug');
  });

  it('shared_secret: sends the server secret in the configured header', async () => {
    await seed(SHARED, 'gitea', 's3cret');
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(upstream.targets[0]?.headers).toEqual({ 'X-API-Key': 'k s3cret' });
  });

  it('shared_secret without a secret: fails without calling upstream', async () => {
    await seed(SHARED);
    const r = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(r.ok).toBe(false);
    expect(upstream.targets).toEqual([]);
    expect(server().lastSyncError).toBe('The server secret is not set');
  });

  it('oauth_user without the admin’s connection: needs_connection, catalog untouched', async () => {
    await seed(OAUTH);
    upstream.tools = [tool('a')];
    const r = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.code).toBe('needs_connection');
    expect(credentials.asked).toEqual(['admin-1']);
    expect(upstream.targets).toEqual([]);
    expect(toolRow('a')).toBeUndefined();
    expect(h.outbox).toEqual([]);
    expect(server().lastSyncError).toBe('An administrator needs to connect this server');
  });

  it('pat_user: catalog builds the header from the personal secret with the template', async () => {
    await seed({ kind: 'pat_user', header: 'Authorization', template: 'token {secret}' });
    credentials.tokens.set('admin-1', 'pat-1');
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(upstream.targets[0]?.headers).toEqual({ Authorization: 'token pat-1' });
  });

  it('oauth_user: uses the starting admin’s token, and remembers them for the scheduler', async () => {
    await seed(OAUTH);
    credentials.tokens.set('admin-1', 'tok-1');
    credentials.tokens.set('admin-2', 'tok-2');
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(server().syncUserId).toBe('admin-1');
    await syncServer(deps, { principal: admin2, slug: 'gitea' });
    expect(server().syncUserId).toBe('admin-2');
    await syncServer(deps, { principal: null, slug: 'gitea' });
    expect(credentials.asked).toEqual(['admin-1', 'admin-2', 'admin-2']);
    expect(upstream.targets.map((t) => t.headers)).toEqual([
      { Authorization: 'Bearer tok-1' },
      { Authorization: 'Bearer tok-2' },
      { Authorization: 'Bearer tok-2' },
    ]);
    expect(server().syncUserId).toBe('admin-2');
  });

  it('scheduler on an oauth_user server nobody synchronised: needs_connection', async () => {
    await seed(OAUTH);
    const r = await syncServer(deps, { principal: null, slug: 'gitea' });
    expect(r.ok).toBe(false);
    expect(credentials.asked).toEqual([]);
    expect(server().lastSyncError).toBe('An administrator needs to connect this server');
    expect(h.audit).toEqual([]);
  });

  it('the remembered admin, demoted or disabled since: their connection is not used', async () => {
    await seed(OAUTH);
    credentials.tokens.set('admin-1', 'tok-1');
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    credentials.activeAdmins.delete('admin-1');
    const r = await syncServer(deps, { principal: null, slug: 'gitea' });
    expect(r.ok ? 'ok' : r.error.code).toBe('needs_connection');
    expect(credentials.asked).toEqual(['admin-1']);
    expect(upstream.targets).toHaveLength(1);
    expect(server().lastSyncError).toBe('An administrator needs to connect this server');
  });

  it('an expired connection needs a new one; an unreachable provider is its own failure', async () => {
    await seed(OAUTH);
    credentials.failure = 'expired';
    const expired = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(expired.ok ? 'ok' : expired.error.code).toBe('needs_connection');
    credentials.failure = 'unavailable';
    const down = await syncServer(deps, { principal: admin, slug: 'gitea' });
    expect(down.ok ? 'ok' : down.error.code).toBe('sync_failed');
    expect(server().lastSyncError).toBe('The OAuth provider is unavailable');
    expect(upstream.targets).toEqual([]);
  });

  it('a failed sync does not replace the remembered admin', async () => {
    await seed(OAUTH);
    credentials.tokens.set('admin-1', 'tok-1');
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    await syncServer(deps, { principal: admin2, slug: 'gitea' });
    expect(server().syncUserId).toBe('admin-1');
  });

  it('is for admins only, and only for known, active servers', async () => {
    await seed();
    const r1 = await syncServer(deps, { principal: member, slug: 'gitea' });
    expect(r1.ok ? 'ok' : r1.error.code).toBe('forbidden');
    const r2 = await syncServer(deps, { principal: admin, slug: 'nope' });
    expect(r2.ok ? 'ok' : r2.error.code).toBe('not_found');
    await setServerStatus(deps, { principal: admin, slug: 'gitea', status: 'disabled' });
    const r3 = await syncServer(deps, { principal: null, slug: 'gitea' });
    expect(r3.ok ? 'ok' : r3.error.code).toBe('not_found');
    expect(upstream.targets).toEqual([]);
  });
});

/** Lets every pending promise chain run up to its next real wait. */
const settle = () =>
  new Promise<void>((resolve) => {
    setTimeout(resolve, 0);
  });

describe('syncServer: overlapping runs', () => {
  const t1 = new Date('2026-10-30T11:00:00Z');
  const t2 = new Date('2026-10-30T12:00:00Z');

  it('a scheduler run that finishes after an admin’s newer sync does not undo it', async () => {
    await seed(OAUTH);
    credentials.tokens.set('admin-1', 'tok-1');
    credentials.tokens.set('admin-2', 'tok-2');
    await syncServer(deps, { principal: admin, slug: 'gitea' });
    h.clock.current = t1;
    const release = upstream.hold();
    const scheduled = syncServer(deps, { principal: null, slug: 'gitea' });
    await settle();
    h.clock.current = t2;
    await syncServer(deps, { principal: admin2, slug: 'gitea' });
    release([tool('a')]);
    expect((await scheduled).ok).toBe(true);
    expect(server()).toMatchObject({ syncUserId: 'admin-2', lastSyncAt: t2, lastSyncError: null });
  });

  it('a late failure of an older run does not overwrite a newer success', async () => {
    await seed();
    h.clock.current = t1;
    const release = upstream.hold();
    const old = syncServer(deps, { principal: admin, slug: 'gitea' });
    await settle();
    h.clock.current = t2;
    await syncServer(deps, { principal: admin2, slug: 'gitea' });
    release(new UpstreamError('timeout'));
    expect((await old).ok).toBe(false);
    expect(server()).toMatchObject({ syncUserId: 'admin-2', lastSyncAt: t2, lastSyncError: null });
  });
});

describe('syncAllServers', () => {
  it('synchronises every active server and reports each outcome', async () => {
    await seed({ kind: 'none' }, 'alpha');
    await seed(OAUTH, 'beta');
    await seed({ kind: 'none' }, 'gamma');
    await setServerStatus(deps, { principal: admin, slug: 'gamma', status: 'disabled' });
    upstream.tools = [tool('a')];
    const out = await syncAllServers(deps);
    expect(out).toEqual([
      { slug: 'alpha', ok: true },
      { slug: 'beta', ok: false, code: 'needs_connection' },
    ]);
    expect(upstream.targets.map((t) => t.url)).toEqual(['https://mcp.example.com/alpha']);
  });

  it('an unexpected error on one server is recorded as internal; the rest still run (M3)', async () => {
    await seed({ kind: 'none' }, 'alpha');
    await seed(SHARED, 'beta', 's3cret');
    await seed({ kind: 'none' }, 'gamma');
    upstream.tools = [tool('a')];
    const logged: Record<string, unknown>[] = [];
    // beta's secret cannot be opened (e.g. sealed under a lost key).
    const open = deps.secretBox.open.bind(deps.secretBox);
    const withBadSecret: SyncDeps = {
      ...deps,
      secretBox: {
        ...deps.secretBox,
        open: (sealed) => {
          const plain = open(sealed);
          if (plain === 's3cret') throw new Error('unable to authenticate data: s3cret');
          return plain;
        },
      },
      logger: { error: (fields) => logged.push(fields) },
    };
    const out = await syncAllServers(withBadSecret);
    expect(out).toEqual([
      { slug: 'alpha', ok: true },
      { slug: 'beta', ok: false, code: 'internal' },
      { slug: 'gamma', ok: true },
    ]);
    expect(server('beta').lastSyncError).toBe('Internal sync error');
    expect(logged).toHaveLength(1);
    expect(logged[0]).toMatchObject({ server: 'beta' });
    expect(JSON.stringify(logged)).not.toContain('s3cret');
  });
});

import { describe, expect, it } from 'vite-plus/test';
import { makeCatalogDeps } from '../../../../../test/fakes/in-memory-catalog.ts';
import { err, type UpstreamClient, UpstreamError } from '../../../../shared-kernel/index.ts';
import type { SyncDeps } from '../deps.ts';
import type { Principal } from '../ports.ts';
import { registerServer } from './register.ts';

const admin: Principal = { userId: 'admin-1', role: 'admin', groups: [] };
const member: Principal = { userId: 'u-1', role: 'member', groups: [] };

const input = {
  slug: 'gitea',
  name: 'Gitea',
  description: '',
  url: 'https://git.example.com/mcp',
  auth: { kind: 'none' as const },
  access: { kind: 'all' as const },
  timeoutMs: 5000,
};

function syncDeps(
  upstream: UpstreamClient,
): { deps: SyncDeps } & ReturnType<typeof makeCatalogDeps> {
  const base = makeCatalogDeps();
  const deps: SyncDeps = {
    ...base.deps,
    upstream,
    credentials: {
      isActiveAdmin: () => Promise.resolve(true),
      personalSecret: () => Promise.resolve(err('needs_connection' as const)),
    },
    maxResponseBytes: 1000,
  };
  return { ...base, deps };
}

describe('registerServer', () => {
  it('keeps the server and stores a generic error when the sync throws unexpectedly', async () => {
    const logged: Record<string, unknown>[] = [];
    const base = syncDeps({
      listTools: () => Promise.reject(new TypeError('boom token=sekret')),
      callTool: () => Promise.reject(new Error('unused')),
    });
    const deps: SyncDeps = { ...base.deps, logger: { error: (f) => logged.push(f) } };
    const r = await registerServer(deps, { principal: admin, ...input });
    expect(r.ok && r.value).toMatchObject({
      slug: 'gitea',
      lastSyncError: 'Internal sync error',
    });
    expect(JSON.stringify(logged)).not.toContain('sekret');
    expect(logged).toHaveLength(1);
  });

  it('creates the server and synchronises it as the creating admin', async () => {
    const { deps, audit } = syncDeps({
      listTools: () => Promise.resolve([{ name: 'ping', inputSchema: { type: 'object' } }]),
      callTool: () => Promise.reject(new Error('unused')),
    });
    const r = await registerServer(deps, { principal: admin, ...input });
    expect(r.ok && r.value).toMatchObject({ slug: 'gitea', toolCount: 1, lastSyncError: null });
    expect(audit.map((a) => a.action)).toEqual(['server.create', 'server.sync']);
  });

  it('keeps the server when synchronisation fails and reports the error in the view', async () => {
    const { deps } = syncDeps({
      listTools: () => Promise.reject(new UpstreamError('transport', 'down')),
      callTool: () => Promise.reject(new Error('unused')),
    });
    const r = await registerServer(deps, { principal: admin, ...input });
    expect(r.ok && r.value.toolCount).toBe(0);
    expect(r.ok && r.value.lastSyncError).not.toBeNull();
  });

  it('creates nothing for a member', async () => {
    const { deps, servers } = syncDeps({
      listTools: () => Promise.resolve([]),
      callTool: () => Promise.reject(new Error('unused')),
    });
    const r = await registerServer(deps, { principal: member, ...input });
    expect(r.ok).toBe(false);
    expect(servers.rows.size).toBe(0);
  });
});

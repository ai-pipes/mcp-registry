import { describe, expect, it } from 'vite-plus/test';
import { makeCatalogDeps } from '../../../../../test/fakes/in-memory-catalog.ts';
import { createServer, setServerStatus } from '../commands/servers.ts';
import { applyToolSnapshot, setToolHidden } from '../commands/tools.ts';
import type { Access } from '../../domain/access.ts';
import type { Principal } from '../ports.ts';
import { listCatalogFor } from './catalog.ts';

const admin: Principal = { userId: 'admin-1', role: 'admin', groups: [] };
const dev: Principal = { userId: 'u-1', role: 'member', groups: ['devs'] };
const other: Principal = { userId: 'u-2', role: 'member', groups: [] };

const snap = (name: string) => ({
  name,
  title: null,
  description: `does ${name}`,
  inputSchema: {},
  outputSchema: null,
  annotations: { readOnlyHint: true },
});

async function seed() {
  const { deps } = makeCatalogDeps();
  const servers: [string, Access][] = [
    ['open', { kind: 'all' }],
    ['closed', { kind: 'groups', groups: ['devs'] }],
    ['off', { kind: 'all' }],
  ];
  await Promise.all(
    servers.map(async ([slug, access]) => {
      await createServer(deps, {
        principal: admin,
        slug,
        name: slug,
        description: '',
        url: 'https://x.example.com/mcp',
        auth: { kind: 'none' },
        access,
        timeoutMs: 5000,
      });
      await applyToolSnapshot(deps, { server: slug, tools: [snap('a'), snap('b'), snap('gone')] });
    }),
  );
  await applyToolSnapshot(deps, { server: 'open', tools: [snap('a'), snap('b')] });
  await setToolHidden(deps, { principal: admin, ref: 'open.b', hidden: true });
  await setServerStatus(deps, { principal: admin, slug: 'off', status: 'disabled' });
  return deps;
}

describe('listCatalogFor', () => {
  it('lists usable servers with visible tools only (no hidden, no removed)', async () => {
    const deps = await seed();
    const r = await listCatalogFor(deps, { principal: dev });
    expect(r.map((s) => s.slug)).toEqual(['closed', 'open']);
    expect(r.find((s) => s.slug === 'open')?.tools).toEqual([
      { ref: 'open.a', title: null, description: 'does a', annotations: { readOnlyHint: true } },
    ]);
  });

  it('hides group-closed servers from people outside the group; disabled ones from everyone', async () => {
    const deps = await seed();
    expect((await listCatalogFor(deps, { principal: other })).map((s) => s.slug)).toEqual(['open']);
    expect((await listCatalogFor(deps, { principal: admin })).map((s) => s.slug)).toEqual([
      'closed',
      'open',
    ]);
  });
});

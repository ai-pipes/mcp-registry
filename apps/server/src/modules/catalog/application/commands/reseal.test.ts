import { describe, expect, it } from 'vite-plus/test';
import { makeCatalogDeps } from '../../../../../test/fakes/in-memory-catalog.ts';
import type { SecretBox } from '../../../../shared-kernel/index.ts';
import { getServerSecret } from '../queries/servers.ts';
import { resealServerSecrets } from './reseal.ts';
import { createServer } from './servers.ts';

/** A box per key id: sealed bytes are `<id>:<plain>`; open refuses another id. */
const box = (id: string): SecretBox => ({
  seal: (plain) => new TextEncoder().encode(`${id}:${plain}`),
  open: (sealed) => {
    const text = new TextDecoder().decode(sealed);
    if (!text.startsWith(`${id}:`)) throw new Error('wrong key');
    return text.slice(id.length + 1);
  },
});
const OLD = box('old');
const NEW = box('new');
const admin = { userId: 'admin-1', role: 'admin', groups: [] } as const;
const SHARED = { kind: 'shared_secret', header: 'X-API-Key', template: '{secret}' } as const;

async function seeded() {
  const f = makeCatalogDeps();
  f.deps.secretBox = OLD;
  const add = async (slug: string, secret: string | null) => {
    const r = await createServer(f.deps, {
      principal: { ...admin, groups: [] },
      slug,
      name: slug,
      description: '',
      url: `https://${slug}.example.com/mcp`,
      auth: secret === null ? { kind: 'none' } : SHARED,
      access: { kind: 'all' },
      secret,
    });
    if (!r.ok) throw r.error;
  };
  await add('alpha', 'alpha-key');
  await add('beta', 'beta-key');
  await add('open', null);
  return f;
}

describe('resealServerSecrets', () => {
  it('re-seals every server secret under the new key; the old key opens none of them', async () => {
    const f = await seeded();
    const before = new Map(
      [...f.servers.rows.values()].map((s) => [s.slug, s.updatedAt.getTime()]),
    );
    const events = f.outbox.length;
    const r = await resealServerSecrets(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 2, current: 0, failed: [] });
    // Nothing the search index cares about changed.
    expect(f.outbox).toHaveLength(events);
    expect(await getServerSecret({ ...f.deps, secretBox: NEW }, 'alpha')).toBe('alpha-key');
    expect(await getServerSecret({ ...f.deps, secretBox: NEW }, 'beta')).toBe('beta-key');
    for (const s of f.servers.rows.values()) {
      if (s.sealedSecret) expect(() => OLD.open(s.sealedSecret!)).toThrow();
      // A re-encryption is not an edit.
      expect(s.updatedAt.getTime()).toBe(before.get(s.slug));
    }
  });

  it('a repeated run changes nothing', async () => {
    const f = await seeded();
    await resealServerSecrets(f.deps, { from: OLD, to: NEW });
    const r = await resealServerSecrets(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 0, current: 2, failed: [] });
  });

  it('reports a secret neither key opens and leaves it as it is', async () => {
    const f = await seeded();
    const beta = f.servers.rows.get('beta')!;
    beta.replaceSealedSecret(box('lost').seal('x'));
    const r = await resealServerSecrets(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 1, current: 0, failed: ['beta'] });
    expect(new TextDecoder().decode(beta.sealedSecret ?? new Uint8Array())).toBe('lost:x');
  });

  it('a secret replaced meanwhile (by the app, under the new key) is not overwritten', async () => {
    const f = await seeded();
    const alpha = f.servers.rows.get('alpha')!;
    const replace = f.servers.replaceSealedSecret.bind(f.servers);
    let raced = false;
    f.servers.replaceSealedSecret = async (slug, current, next) => {
      if (slug === 'alpha' && !raced) {
        raced = true;
        alpha.replaceSealedSecret(NEW.seal('admin-set-meanwhile'));
      }
      return replace(slug, current, next);
    };
    const r = await resealServerSecrets(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 1, current: 1, failed: [] });
    expect(await getServerSecret({ ...f.deps, secretBox: NEW }, 'alpha')).toBe(
      'admin-set-meanwhile',
    );
  });

  it('gives up on a secret that keeps changing, and skips a server deleted meanwhile', async () => {
    const f = await seeded();
    f.servers.replaceSealedSecret = () => Promise.resolve(false);
    const find = f.servers.findBySlug.bind(f.servers);
    f.servers.findBySlug = (slug) => (slug === 'beta' ? Promise.resolve(null) : find(slug));
    const r = await resealServerSecrets(f.deps, { from: OLD, to: NEW });
    expect(r).toEqual({ resealed: 0, current: 0, failed: ['alpha'] });
  });
});

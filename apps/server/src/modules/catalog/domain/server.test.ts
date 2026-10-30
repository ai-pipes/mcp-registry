import { describe, expect, it } from 'vite-plus/test';
import { Server } from './server.ts';
import { SyncRecord } from './sync.ts';

const t0 = new Date('2026-10-30T10:00:00Z');
const t1 = new Date('2026-10-30T11:00:00Z');

const base = {
  slug: 'gitea',
  name: 'Gitea',
  description: 'Git hosting',
  url: 'https://mcp.example.com/gitea',
  auth: { kind: 'none' } as const,
  access: { kind: 'all' } as const,
  now: t0,
};

function make(over: Partial<Parameters<typeof Server.create>[0]> = {}): Server {
  const r = Server.create({ ...base, ...over });
  if (!r.ok) throw r.error;
  return r.value;
}

describe('Server.create', () => {
  it('applies defaults: 120 s timeout, no limit, active, no secret', () => {
    const s = make();
    expect(s.slug).toBe('gitea');
    expect(s.timeoutMs).toBe(120_000);
    expect(s.dailyLimit).toBeNull();
    expect(s.status).toBe('active');
    expect(s.secretSet).toBe(false);
    expect(s.sealedSecret).toBeNull();
  });

  it('emits server_changed', () => {
    expect(make().pullEvents()).toEqual([
      { type: 'catalog.server_changed', occurredAt: t0, payload: { server: 'gitea' } },
    ]);
  });

  it('validates slug, name, description, url, timeout, limit, auth and access', () => {
    const bad: Partial<Parameters<typeof Server.create>[0]>[] = [
      { slug: 'Gitea' },
      { name: ' ' },
      { name: 'x'.repeat(101) },
      { description: 'x'.repeat(2001) },
      { url: 'ftp://x.example' },
      { url: 'not a url' },
      { timeoutMs: 0 },
      { timeoutMs: 1.5 },
      { timeoutMs: 3_600_001 },
      { dailyLimit: 0 },
      { dailyLimit: 1.2 },
      { auth: { kind: 'shared_secret', header: 'X', template: 'no placeholder' } },
      { access: { kind: 'groups', groups: [] } },
    ];
    for (const over of bad) {
      const r = Server.create({ ...base, ...over });
      expect(r.ok, JSON.stringify(over)).toBe(false);
    }
  });

  it('accepts explicit timeout and limit', () => {
    const s = make({ timeoutMs: 5000, dailyLimit: 10 });
    expect(s.timeoutMs).toBe(5000);
    expect(s.dailyLimit).toBe(10);
  });

  it('trims name and description', () => {
    const s = make({ name: '  Gitea ', description: ' d ' });
    expect(s.name).toBe('Gitea');
    expect(s.description).toBe('d');
  });
});

describe('Server.update', () => {
  it('changes fields and emits server_changed when name or description changed', () => {
    const s = make();
    s.pullEvents();
    const r = s.update({ name: 'Gitea 2' }, t1);
    expect(r.ok).toBe(true);
    expect(s.name).toBe('Gitea 2');
    expect(s.updatedAt).toEqual(t1);
    expect(s.pullEvents()).toHaveLength(1);
  });

  it('emits server_changed when access changes, not when it is set to the same value', () => {
    const s = make();
    s.pullEvents();
    s.update({ access: { kind: 'all' } }, t1);
    expect(s.pullEvents()).toEqual([]);
    s.update({ access: { kind: 'groups', groups: ['dev'] } }, t1);
    expect(s.pullEvents()).toHaveLength(1);
  });

  it('does not emit for non-searchable changes (url, timeout, limit)', () => {
    const s = make();
    s.pullEvents();
    const r = s.update({ url: 'https://other.example/mcp', timeoutMs: 1000, dailyLimit: 5 }, t1);
    expect(r.ok).toBe(true);
    expect(s.url).toBe('https://other.example/mcp');
    expect(s.dailyLimit).toBe(5);
    expect(s.pullEvents()).toEqual([]);
  });

  it('can clear the daily limit', () => {
    const s = make({ dailyLimit: 5 });
    expect(s.update({ dailyLimit: null }, t1).ok).toBe(true);
    expect(s.dailyLimit).toBeNull();
  });

  it('changes auth and access with validation, leaving state untouched on error', () => {
    const s = make();
    const bad = s.update({ auth: { kind: 'pat_user', header: 'A', template: 'x' } }, t1);
    expect(bad.ok).toBe(false);
    expect(s.auth).toEqual({ kind: 'none' });
    expect(s.update({ access: { kind: 'groups', groups: ['dev'] } }, t1).ok).toBe(true);
    expect(s.access).toEqual({ kind: 'groups', groups: ['dev'] });
  });

  it('is atomic: an invalid field leaves everything unchanged', () => {
    const s = make();
    expect(s.update({ name: 'New', timeoutMs: 0 }, t1).ok).toBe(false);
    expect(s.name).toBe('Gitea');
  });
});

describe('Server events', () => {
  it('coalesces several changes before a save into one server_changed', () => {
    const s = make();
    s.pullEvents();
    s.update({ name: 'N' }, t1);
    s.setStatus('disabled', t1);
    expect(s.pullEvents()).toHaveLength(1);
  });
});

const shared = { kind: 'shared_secret', header: 'X-Key', template: '{secret}' } as const;

describe('Server secret', () => {
  it('sets and clears the sealed secret', () => {
    const s = make({ auth: shared });
    s.pullEvents();
    expect(s.setSealedSecret(new Uint8Array([1, 2, 3]), t1).ok).toBe(true);
    expect(s.secretSet).toBe(true);
    expect(s.sealedSecret).toEqual(new Uint8Array([1, 2, 3]));
    expect(s.setSealedSecret(null, t1).ok).toBe(true);
    expect(s.secretSet).toBe(false);
    expect(s.pullEvents()).toEqual([]);
  });

  it('refuses a secret for auth modes that do not use one, but allows clearing', () => {
    for (const auth of [
      { kind: 'none' },
      { kind: 'pat_user', header: 'Authorization', template: 'Bearer {secret}' },
    ] as const) {
      const s = make({ auth });
      const r = s.setSealedSecret(new Uint8Array([1]), t1);
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('invalid_server');
      expect(s.secretSet).toBe(false);
      expect(s.setSealedSecret(null, t1).ok).toBe(true);
    }
  });

  it('drops the secret when the auth kind changes and says so', () => {
    const s = make({ auth: shared });
    s.setSealedSecret(new Uint8Array([1]), t0);
    const r = s.update({ auth: { kind: 'pat_user', header: 'A', template: '{secret}' } }, t1);
    expect(r).toEqual({ ok: true, value: { secretCleared: true } });
    expect(s.secretSet).toBe(false);
  });

  it('keeps the secret when the kind stays the same, or when there was none', () => {
    const s = make({ auth: shared });
    s.setSealedSecret(new Uint8Array([1]), t0);
    const same = s.update({ auth: { ...shared, header: 'X-Other' } }, t1);
    expect(same).toEqual({ ok: true, value: { secretCleared: false } });
    expect(s.secretSet).toBe(true);
    const bare = make();
    expect(bare.update({ auth: shared }, t1)).toEqual({
      ok: true,
      value: { secretCleared: false },
    });
  });
});

function loaded(): Server {
  return Server.rehydrate({
    slug: 'gitea',
    version: 3,
    name: 'G',
    description: '',
    url: 'https://x.example',
    auth: shared,
    sealedSecret: new Uint8Array([1]),
    access: { kind: 'all' },
    timeoutMs: 1000,
    dailyLimit: null,
    status: 'active',
    lastSyncAt: null,
    lastSyncError: null,
    syncUserId: null,
    createdAt: t0,
    updatedAt: t0,
  });
}

describe('Server.secretChanged', () => {
  it('a loaded server has not changed its secret; an edit of other fields does not either', () => {
    const s = loaded();
    expect(s.secretChanged).toBe(false);
    s.update({ name: 'Other', auth: { ...shared, header: 'X-Other' } }, t1);
    s.setStatus('disabled', t1);
    expect(s.secretChanged).toBe(false);
  });

  it('setting, clearing, re-sealing it, or a change of auth kind that drops it, does', () => {
    const set = loaded();
    set.setSealedSecret(new Uint8Array([2]), t1);
    expect(set.secretChanged).toBe(true);
    const cleared = loaded();
    cleared.setSealedSecret(null, t1);
    expect(cleared.secretChanged).toBe(true);
    const resealed = loaded();
    resealed.replaceSealedSecret(new Uint8Array([3]));
    expect(resealed.secretChanged).toBe(true);
    const dropped = loaded();
    dropped.update({ auth: { kind: 'pat_user', header: 'A', template: '{secret}' } }, t1);
    expect(dropped.secretChanged).toBe(true);
  });

  it('a new server holds whatever secret it was given', () => {
    expect(make().secretChanged).toBe(true);
    expect(make().version).toBe(0);
  });
});

describe('Server status', () => {
  it('toggles and emits only on a real change', () => {
    const s = make();
    s.pullEvents();
    expect(s.setStatus('active', t1)).toBe(false);
    expect(s.pullEvents()).toEqual([]);
    expect(s.setStatus('disabled', t1)).toBe(true);
    expect(s.status).toBe('disabled');
    expect(s.pullEvents()).toEqual([
      { type: 'catalog.server_changed', occurredAt: t1, payload: { server: 'gitea' } },
    ]);
  });
});

describe('Server.rehydrate', () => {
  it('restores state without events', () => {
    const s = Server.rehydrate({
      slug: 'gitea',
      version: 3,
      name: 'G',
      description: '',
      url: 'https://x.example',
      auth: { kind: 'none' },
      sealedSecret: new Uint8Array([1]),
      access: { kind: 'all' },
      timeoutMs: 1000,
      dailyLimit: null,
      status: 'disabled',
      lastSyncAt: t0,
      lastSyncError: 'boom',
      syncUserId: null,
      createdAt: t0,
      updatedAt: t1,
    });
    expect(s.secretSet).toBe(true);
    expect(s.version).toBe(3);
    expect(s.lastSyncError).toBe('boom');
    expect(s.pullEvents()).toEqual([]);
  });
});

describe('Server synchronisation state', () => {
  const t2 = new Date('2026-10-30T12:00:00Z');

  it('a success clears the error and remembers the admin who ran it', () => {
    const s = make();
    s.applySync(SyncRecord.failure('timeout', t0));
    expect(s.applySync(SyncRecord.success('admin-1', t1))).toBe(true);
    expect(s.lastSyncAt).toEqual(t1);
    expect(s.lastSyncError).toBeNull();
    expect(s.syncUserId).toBe('admin-1');
  });

  it('a scheduled success keeps the last admin', () => {
    const s = make();
    s.applySync(SyncRecord.success('admin-1', t0));
    s.applySync(SyncRecord.success(null, t1));
    expect(s.syncUserId).toBe('admin-1');
    expect(s.lastSyncAt).toEqual(t1);
  });

  it('a failure stores a short Russian reason and keeps the admin', () => {
    const s = make();
    s.applySync(SyncRecord.success('admin-1', t0));
    s.applySync(SyncRecord.failure('needs_connection', t1));
    expect(s.lastSyncError).toBe('An administrator needs to connect this server');
    expect(s.lastSyncAt).toEqual(t1);
    expect(s.syncUserId).toBe('admin-1');
  });

  it('a run that started before the recorded one does not overwrite it', () => {
    const s = make();
    s.applySync(SyncRecord.success('admin-2', t2));
    expect(s.applySync(SyncRecord.failure('timeout', t1))).toBe(false);
    expect(s.applySync(SyncRecord.success('admin-1', t0))).toBe(false);
    expect(s.lastSyncAt).toEqual(t2);
    expect(s.lastSyncError).toBeNull();
    expect(s.syncUserId).toBe('admin-2');
    expect(s.applySync(SyncRecord.failure('timeout', t2))).toBe(true);
  });

  it('whose credentials: the admin who started it, else the last successful admin', () => {
    const s = make();
    expect(s.syncCredentialsOwner(null)).toBeNull();
    expect(s.syncCredentialsOwner('admin-2')).toBe('admin-2');
    s.applySync(SyncRecord.success('admin-1', t0));
    expect(s.syncCredentialsOwner(null)).toBe('admin-1');
    expect(s.syncCredentialsOwner('admin-2')).toBe('admin-2');
  });

  it('sync state changes emit no events', () => {
    const s = make();
    s.pullEvents();
    s.applySync(SyncRecord.success('admin-1', t0));
    s.applySync(SyncRecord.failure('transport', t1));
    expect(s.pullEvents()).toEqual([]);
  });
});

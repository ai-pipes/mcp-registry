import { describe, expect, it } from 'vite-plus/test';
import { reconcileTools, snapshotFromUpstream, Tool, type ToolSnapshot } from './tool.ts';

const t0 = new Date('2026-10-30T10:00:00Z');
const t1 = new Date('2026-10-30T11:00:00Z');

const snap = (name: string, over: Partial<ToolSnapshot> = {}): ToolSnapshot => ({
  name,
  title: null,
  description: `does ${name}`,
  inputSchema: { type: 'object' },
  outputSchema: null,
  annotations: {},
  ...over,
});

const stored = (name: string, over: Partial<Parameters<typeof Tool.rehydrate>[0]> = {}) =>
  Tool.rehydrate({
    server: 'gitea',
    ...snap(name),
    hidden: false,
    removedAt: null,
    syncedAt: t0,
    ...over,
  });

describe('Tool', () => {
  it('has a ref and is callable unless hidden or removed', () => {
    const t = stored('issues.create');
    expect(t.ref).toBe('gitea.issues.create');
    expect(t.visible).toBe(true);
    expect(stored('a', { hidden: true }).visible).toBe(false);
    expect(stored('a', { removedAt: t0 }).visible).toBe(false);
  });

  it('setHidden emits tools_changed only on a change', () => {
    const t = stored('a');
    expect(t.setHidden(false, t1)).toEqual({ ok: true, value: false });
    expect(t.pullEvents()).toEqual([]);
    expect(t.setHidden(true, t1)).toEqual({ ok: true, value: true });
    expect(t.hidden).toBe(true);
    expect(t.pullEvents()).toEqual([
      {
        type: 'catalog.tools_changed',
        occurredAt: t1,
        payload: { server: 'gitea', refs: ['gitea.a'] },
      },
    ]);
  });
});

describe('removed tools', () => {
  it('know they are removed and refuse to be hidden or unhidden', () => {
    const t = stored('a', { removedAt: t0 });
    expect(t.isRemoved).toBe(true);
    expect(stored('a').isRemoved).toBe(false);
    for (const hidden of [true, false]) {
      const r = t.setHidden(hidden, t1);
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('not_found');
    }
    expect(t.pullEvents()).toEqual([]);
  });
});

describe('reconcileTools', () => {
  it('adds new tools, reports them as changed', () => {
    const r = reconcileTools('gitea', [], [snap('a'), snap('b')], t1);
    expect(r.upserts.map((t) => t.name)).toEqual(['a', 'b']);
    expect(r.changedRefs).toEqual(['gitea.a', 'gitea.b']);
    expect(r.addedRefs).toEqual(['gitea.a', 'gitea.b']);
    expect(r.removedRefs).toEqual([]);
    expect(r.events).toEqual([
      {
        type: 'catalog.tools_changed',
        occurredAt: t1,
        payload: { server: 'gitea', refs: ['gitea.a', 'gitea.b'] },
      },
    ]);
  });

  it('leaves an unchanged tool alone (no event, no upsert)', () => {
    const r = reconcileTools('gitea', [stored('a')], [snap('a')], t1);
    expect(r.upserts).toEqual([]);
    expect(r.events).toEqual([]);
  });

  it('updates a changed tool and keeps hidden', () => {
    const r = reconcileTools(
      'gitea',
      [stored('a', { hidden: true })],
      [snap('a', { description: 'new' })],
      t1,
    );
    expect(r.upserts).toHaveLength(1);
    expect(r.upserts[0]).toMatchObject({ description: 'new', hidden: true, syncedAt: t1 });
    expect(r.changedRefs).toEqual(['gitea.a']);
    expect(r.addedRefs).toEqual([]);
  });

  it('marks vanished tools removed once', () => {
    const r = reconcileTools('gitea', [stored('a'), stored('b')], [snap('a')], t1);
    expect(r.upserts.map((t) => [t.name, t.removedAt])).toEqual([['b', t1]]);
    expect(r.removedRefs).toEqual(['gitea.b']);
    expect(r.events).toEqual([
      {
        type: 'catalog.tools_removed',
        occurredAt: t1,
        payload: { server: 'gitea', refs: ['gitea.b'] },
      },
    ]);
    const again = reconcileTools('gitea', [stored('b', { removedAt: t0 })], [], t1);
    expect(again.upserts).toEqual([]);
    expect(again.events).toEqual([]);
  });

  it('revives a returned tool, keeping hidden', () => {
    const r = reconcileTools(
      'gitea',
      [stored('a', { removedAt: t0, hidden: true })],
      [snap('a')],
      t1,
    );
    expect(r.upserts[0]).toMatchObject({ removedAt: null, hidden: true });
    expect(r.changedRefs).toEqual(['gitea.a']);
    expect(r.addedRefs).toEqual(['gitea.a']);
  });

  it('ignores duplicate names in the snapshot (first wins)', () => {
    const r = reconcileTools('gitea', [], [snap('a'), snap('a', { description: 'dup' })], t1);
    expect(r.upserts).toHaveLength(1);
    expect(r.upserts[0]?.description).toBe('does a');
  });
});

describe('snapshotFromUpstream', () => {
  it('fills absent fields: no title, empty description, no output schema, no hints', () => {
    expect(snapshotFromUpstream({ name: 'a', inputSchema: { type: 'object' } })).toEqual({
      name: 'a',
      title: null,
      description: '',
      inputSchema: { type: 'object' },
      outputSchema: null,
      annotations: {},
    });
  });

  it('keeps title, description, output schema and only the boolean hints', () => {
    const s = snapshotFromUpstream({
      name: 'a',
      title: 'A',
      description: 'does a',
      inputSchema: { type: 'object' },
      outputSchema: { type: 'object' },
      annotations: {
        title: 'ignored',
        readOnlyHint: true,
        destructiveHint: false,
        idempotentHint: 'yes',
        openWorldHint: true,
      },
    });
    expect(s).toEqual({
      name: 'a',
      title: 'A',
      description: 'does a',
      inputSchema: { type: 'object' },
      outputSchema: { type: 'object' },
      annotations: { readOnlyHint: true, destructiveHint: false, openWorldHint: true },
    });
  });
});

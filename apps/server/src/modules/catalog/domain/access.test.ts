import { describe, expect, it } from 'vite-plus/test';
import { Access, canUse, type ServerStatus } from './access.ts';

const groupsServer: { access: Access; status: ServerStatus } = {
  access: { kind: 'groups', groups: ['dev'] },
  status: 'active',
};
const member = (groups: string[]) => ({ role: 'member' as const, groups });

describe('canUse', () => {
  it('lets only listed groups use a groups server', () => {
    expect(canUse(groupsServer, member(['dev']))).toBe(true);
    expect(canUse(groupsServer, member(['mcp-users']))).toBe(false);
    expect(canUse(groupsServer, member([]))).toBe(false);
  });

  it('lets an admin use any active server', () => {
    expect(canUse(groupsServer, { role: 'admin', groups: [] })).toBe(true);
  });

  it('lets any member use an all-access server', () => {
    expect(canUse({ access: { kind: 'all' }, status: 'active' }, member([]))).toBe(true);
  });

  it('lets nobody use a disabled server', () => {
    const off: typeof groupsServer = { ...groupsServer, status: 'disabled' };
    expect(canUse(off, member(['dev']))).toBe(false);
    expect(canUse(off, { role: 'admin', groups: [] })).toBe(false);
    expect(canUse({ access: { kind: 'all' }, status: 'disabled' }, member([]))).toBe(false);
  });
});

describe('Access.parse', () => {
  it('accepts all', () => {
    expect(Access.parse({ kind: 'all' })).toEqual({ ok: true, value: { kind: 'all' } });
  });

  it('trims and de-duplicates groups', () => {
    expect(Access.parse({ kind: 'groups', groups: [' dev ', 'dev', 'ops'] })).toEqual({
      ok: true,
      value: { kind: 'groups', groups: ['dev', 'ops'] },
    });
  });

  it('rejects an empty group list and blank names', () => {
    for (const groups of [[], [' '], ['dev', '']]) {
      const r = Access.parse({ kind: 'groups', groups });
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('invalid_access');
    }
  });

  it('rejects an unknown kind', () => {
    expect(Access.parse(JSON.parse('{"kind":"x"}')).ok).toBe(false);
  });
});

describe('Access.isAdmin', () => {
  it('is true only for admin role', () => {
    expect(Access.isAdmin({ role: 'admin' })).toBe(true);
    expect(Access.isAdmin({ role: 'member' })).toBe(false);
  });
});

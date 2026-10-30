import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { CatalogError } from './errors.ts';

export type Access = { kind: 'all' } | { kind: 'groups'; groups: string[] };
export type ServerStatus = 'active' | 'disabled';

/** The part of a Principal the catalog rules need. */
export type Actor = { role: 'admin' | 'member'; groups: string[] };

export const Access = {
  parse(access: Access): Result<Access, CatalogError> {
    if (access.kind === 'all') return ok({ kind: 'all' });
    if (access.kind !== 'groups') {
      return err(new CatalogError('invalid_access', 'unknown access kind'));
    }
    const groups = [...new Set(access.groups.map((g) => g.trim()))];
    if (groups.length === 0 || groups.some((g) => g.length === 0)) {
      return err(new CatalogError('invalid_access', 'list at least one non-empty group'));
    }
    return ok({ kind: 'groups', groups });
  },

  isAdmin(actor: Pick<Actor, 'role'>): boolean {
    return actor.role === 'admin';
  },
};

/** An admin can use any active server; a member only an active one open to all or to their group; a disabled one nobody. */
export function canUse(server: { access: Access; status: ServerStatus }, actor: Actor): boolean {
  if (server.status !== 'active') return false;
  if (Access.isAdmin(actor)) return true;
  if (server.access.kind === 'all') return true;
  const allowed = server.access.groups;
  return actor.groups.some((g) => allowed.includes(g));
}

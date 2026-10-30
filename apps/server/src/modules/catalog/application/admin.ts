import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { Access } from '../domain/access.ts';
import { CatalogError } from '../domain/errors.ts';
import type { Principal } from './ports.ts';

/** Catalog administration is for admins only. */
export function requireAdmin(principal: Principal): Result<void, CatalogError> {
  return Access.isAdmin(principal)
    ? ok(undefined)
    : err(new CatalogError('forbidden', 'administrators only'));
}

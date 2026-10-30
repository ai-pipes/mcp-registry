import { err, ok, type Result } from '../../../../shared-kernel/index.ts';
import { canUse } from '../../domain/access.ts';
import { CatalogError } from '../../domain/errors.ts';
import { ToolRef } from '../../domain/tool-ref.ts';
import { requireAdmin } from '../admin.ts';
import type { CatalogDeps } from '../deps.ts';
import type { Principal, ToolView } from '../ports.ts';
import { toToolView } from '../views.ts';

/**
 * A tool the principal may use right now. One `not_found` for everything else — unknown, hidden,
 * removed, server disabled or closed to the principal — so presence is not revealed.
 */
export async function getToolFor(
  deps: Pick<CatalogDeps, 'servers' | 'tools'>,
  q: { principal: Principal; ref: string },
): Promise<Result<ToolView, CatalogError>> {
  const ref = ToolRef.parse(q.ref);
  if (!ref.ok) return ref;
  const notFound = err(new CatalogError('not_found', `tool "${q.ref}" not found`));
  const server = await deps.servers.findBySlug(ref.value.server);
  if (!server || !canUse(server, q.principal)) return notFound;
  const tool = await deps.tools.find(ref.value.server, ref.value.tool);
  if (!tool?.visible) return notFound;
  return ok(toToolView(tool));
}

/** Admin screen: a server's tools, hidden ones included, removed ones not. */
export async function listServerTools(
  deps: Pick<CatalogDeps, 'servers' | 'tools'>,
  q: { principal: Principal; slug: string },
): Promise<Result<ToolView[], CatalogError>> {
  const admin = requireAdmin(q.principal);
  if (!admin.ok) return admin;
  if (!(await deps.servers.findBySlug(q.slug))) {
    return err(new CatalogError('not_found', `server "${q.slug}" not found`));
  }
  const tools = await deps.tools.listByServer(q.slug);
  return ok(tools.filter((t) => !t.isRemoved).map(toToolView));
}

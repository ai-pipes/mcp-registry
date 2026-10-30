import { err, ok, type Result } from '../../../../shared-kernel/index.ts';
import { CatalogError } from '../../domain/errors.ts';
import { ToolRef } from '../../domain/tool-ref.ts';
import type { ToolSnapshot } from '../../domain/tool.ts';
import { requireAdmin } from '../admin.ts';
import type { CatalogDeps } from '../deps.ts';
import type { Principal, SnapshotResult } from '../ports.ts';

export async function setToolHidden(
  deps: CatalogDeps,
  cmd: { principal: Principal; ref: string; hidden: boolean },
): Promise<Result<void, CatalogError>> {
  const admin = requireAdmin(cmd.principal);
  if (!admin.ok) return admin;
  const ref = ToolRef.parse(cmd.ref);
  if (!ref.ok) return ref;
  const tool = await deps.tools.find(ref.value.server, ref.value.tool);
  if (!tool) return err(new CatalogError('not_found', `tool "${cmd.ref}" not found`));
  const changed = tool.setHidden(cmd.hidden, deps.clock.now());
  if (!changed.ok) return changed;
  if (changed.value) {
    await deps.tools.save(tool);
    await deps.audit.record({
      actorId: cmd.principal.userId,
      action: cmd.hidden ? 'tool.hide' : 'tool.unhide',
      target: tool.ref,
    });
  }
  return ok(undefined);
}

/**
 * Stores an upstream `tools/list` snapshot (the synchroniser calls this; it is not an admin action).
 * Emits tools_changed / tools_removed for the search index.
 */
export function applyToolSnapshot(
  deps: CatalogDeps,
  cmd: { server: string; tools: readonly ToolSnapshot[] },
): Promise<SnapshotResult> {
  return deps.tools.applySnapshot(cmd.server, cmd.tools, deps.clock.now());
}

import { ok, type Result } from '../../../../shared-kernel/index.ts';
import type { CatalogError } from '../../domain/errors.ts';
import type { SyncDeps } from '../deps.ts';
import type { ServerView } from '../ports.ts';
import { toServerView } from '../views.ts';
import { createServer } from './servers.ts';
import { syncServerRecordingFailure } from './sync.ts';

/**
 * Creates a server and, as the creating admin, synchronises it at once. A failed
 * synchronisation (`needs_connection`, upstream down…) does not undo the creation: it is
 * recorded as the server's `lastSyncError`, which the returned view carries.
 */
export async function registerServer(
  deps: SyncDeps,
  cmd: Parameters<typeof createServer>[1],
): Promise<Result<ServerView, CatalogError>> {
  const created = await createServer(deps, cmd);
  if (!created.ok) return created;
  const slug = created.value.slug;
  // The server exists: an unexpected failure must not turn the creation into an error.
  await syncServerRecordingFailure(deps, { principal: cmd.principal, slug });
  const listing = (await deps.servers.list()).find((l) => l.server.slug === slug);
  return ok(listing ? toServerView(listing.server, listing.toolCount) : created.value);
}

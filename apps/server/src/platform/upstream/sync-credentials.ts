import type { SyncCredentials } from '../../modules/catalog/index.ts';
import { type ConnectionsDeps, resolveCredential } from '../../modules/connections/index.ts';
import { type IdentityDeps, isUserActiveAdmin } from '../../modules/identity/index.ts';

/**
 * Wiring only: the catalog's synchronisation port over the public APIs of identity (is the
 * owner still an enabled admin) and connections (the person's own secret). The rules — whose
 * connection may serve and what a failure means — are the catalog's (`SyncOwner`).
 */
export function personalSyncCredentials(deps: {
  connections: ConnectionsDeps;
  identity: Pick<IdentityDeps, 'users'>;
}): SyncCredentials {
  return {
    isActiveAdmin: (userId) => isUserActiveAdmin(deps.identity, userId),
    personalSecret: (server, userId) =>
      resolveCredential(deps.connections, { userId, server: server.slug }),
  };
}

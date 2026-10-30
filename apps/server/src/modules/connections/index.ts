// Public API of the connections module: the only file other modules may import.
import type { Sql } from '../../platform/db.ts';
import type { ConnectionsDeps } from './application/ports.ts';
import { PgConnectionStore, PgPendingConnects } from './infrastructure/pg-connections.ts';

export { completeConnect, disconnect, savePat, startConnect } from './application/connect.ts';
export { listConnections } from './application/list.ts';
export type { ConnectionSummary } from './application/list.ts';
export type {
  ConnectionsDeps,
  CredentialFailure,
  OAuthClient,
  ServerDirectory,
  TokenSet,
} from './application/ports.ts';
export { markExpired, resolveCredential } from './application/resolve.ts';
export { resealConnections } from './application/reseal.ts';
export { createSingleFlight } from './application/single-flight.ts';
export { ConnectionError } from './domain/errors.ts';
export type { ConnectionErrorCode } from './domain/errors.ts';
export { createCatalogServerDirectory } from './infrastructure/catalog-servers.ts';
export { OpenidOAuthClient } from './infrastructure/oauth-client.ts';
export { nodePkce } from './infrastructure/pkce.ts';
export { connectionRoutes } from './interfaces/http/connections.ts';

export function createPgConnections(sql: Sql): Pick<ConnectionsDeps, 'connections' | 'pending'> {
  return { connections: new PgConnectionStore(sql), pending: new PgPendingConnects(sql) };
}

// Public API of the catalog module: the only file other modules may import.
import type { Sql } from '../../platform/db.ts';
import type { CatalogDeps } from './application/deps.ts';
import { PgServerRepository, PgToolRepository } from './infrastructure/pg-catalog.ts';

export {
  createServer,
  setServerSecret,
  setServerStatus,
  updateServer,
} from './application/commands/servers.ts';
export { registerServer } from './application/commands/register.ts';
export { resealServerSecrets } from './application/commands/reseal.ts';
export type { ResealSummary } from './application/commands/reseal.ts';
export { applyToolSnapshot, setToolHidden } from './application/commands/tools.ts';
export { syncAllServers, syncServer } from './application/commands/sync.ts';
export type { SyncCounts, SyncOutcome } from './application/commands/sync.ts';
export type { CatalogDeps, SyncDeps } from './application/deps.ts';
export type {
  MemberServerView,
  Principal,
  ServerView,
  SyncCredentials,
  ToolView,
  ServerCallTarget,
} from './application/ports.ts';
export {
  getServerForUpstream,
  getServerSecret,
  listAllServers,
  listServersFor,
} from './application/queries/servers.ts';
export { listCatalogFor } from './application/queries/catalog.ts';
export type { CatalogEntry } from './application/queries/catalog.ts';
export { listServerSlugs, listToolsForIndex } from './application/queries/index-source.ts';
export type { IndexedToolSource, ServerIndexSource } from './application/queries/index-source.ts';
export { getToolFor, listServerTools } from './application/queries/tools.ts';
export { Access, canUse } from './domain/access.ts';
export type { ServerStatus } from './domain/access.ts';
export { AuthMode } from './domain/auth-mode.ts';
export type { OAuthClientConfig } from './domain/auth-mode.ts';
export { CatalogError } from './domain/errors.ts';
export type { CatalogErrorCode } from './domain/errors.ts';
export { ServerSlug } from './domain/server-slug.ts';
export { ToolRef } from './domain/tool-ref.ts';
export type { ToolAnnotations, ToolSnapshot } from './domain/tool.ts';

export function createPgCatalog(sql: Sql): Pick<CatalogDeps, 'servers' | 'tools'> {
  return { servers: new PgServerRepository(sql), tools: new PgToolRepository(sql) };
}
export { serverRoutes, toAuthMode } from './interfaces/http/servers.ts';
export { toolRoutes } from './interfaces/http/tools.ts';

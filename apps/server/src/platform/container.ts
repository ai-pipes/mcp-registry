import { type CatalogDeps, createPgCatalog, type SyncDeps } from '../modules/catalog/index.ts';
import {
  createCatalogToolReader,
  createPgSearch,
  type Embedder,
  OpenAiCompatibleEmbedder,
  type SearchDeps,
  TransformersEmbedder,
} from '../modules/search/index.ts';
import {
  type ConnectionsDeps,
  createCatalogServerDirectory,
  createPgConnections,
  createSingleFlight,
  nodePkce,
  OpenidOAuthClient,
} from '../modules/connections/index.ts';
import {
  AjvArgsValidator,
  createCatalogToolCatalog,
  createConnectionCredentials,
  createPgGateway,
  type GatewayDeps,
} from '../modules/gateway/index.ts';
import {
  createPgIdentity,
  type IdentityDeps,
  type OidcProvider,
} from '../modules/identity/index.ts';
import {
  type AuditLog,
  type Clock,
  systemClock,
  type UpstreamClient,
} from '../shared-kernel/index.ts';
import { PgAuditLog } from './audit.ts';
import type { Config } from './config.ts';
import { AesGcmSecretBox } from './crypto/secret-box.ts';
import type { Sql } from './db.ts';
import { createLogger, type Logger } from './logger.ts';
import { McpUpstreamClient } from './upstream/mcp-upstream-client.ts';
import { personalSyncCredentials } from './upstream/sync-credentials.ts';

/**
 * Everything the entry points need. Modules (identity, catalog, connections, search, gateway)
 * add their dependency bundles here as they appear.
 */
export type Container = {
  audit: AuditLog;
  catalog: CatalogDeps;
  /** The catalog plus what synchronisation with upstream needs. */
  catalogSync: SyncDeps;
  /** Personal OAuth/PAT credentials to upstream servers. */
  connections: ConnectionsDeps;
  clock: Clock;
  upstream: UpstreamClient;
  search: SearchDeps;
  /** `call_tool`: catalog and connections through their public APIs, call log, daily limits. */
  gateway: GatewayDeps;
  logger: Logger;
  config: Config;
  sql: Sql;
  identity: IdentityDeps;
};

export type ContainerOptions = {
  logger?: Logger;
  clock?: Clock;
  /** The identity provider; `main.ts` discovers it at startup. Without one, sign-in is refused. */
  oidc?: OidcProvider;
  /** Tests inject a deterministic embedder; otherwise it follows `EMBEDDER`. */
  embedder?: Embedder;
};

const unconfiguredOidc: OidcProvider = {
  authorizationUrl: () => Promise.reject(new Error('OIDC provider is not configured')),
  exchange: () => Promise.reject(new Error('OIDC provider is not configured')),
};

function createEmbedder(config: Config): Embedder {
  if (config.embedder === 'openai-compatible') {
    if (!config.embedderUrl || !config.embedderModel) {
      throw new Error('EMBEDDER=openai-compatible needs EMBEDDER_URL and EMBEDDER_MODEL');
    }
    return new OpenAiCompatibleEmbedder({ url: config.embedderUrl, model: config.embedderModel });
  }
  return new TransformersEmbedder({ modelDir: config.modelDir, allowRemote: false });
}

/** Composition root: wires every module's ports by hand. Does not start the outbox worker. */
export function buildContainer(config: Config, sql: Sql, opts: ContainerOptions = {}): Container {
  const audit = new PgAuditLog(sql);
  const clock = opts.clock ?? systemClock;
  const upstream = new McpUpstreamClient();
  const logger = opts.logger ?? createLogger(config.logLevel);
  const secretBox = new AesGcmSecretBox(
    config.secretsKey,
    config.secretsKeyOld ? { previous: config.secretsKeyOld } : {},
  );
  const catalog: CatalogDeps = { ...createPgCatalog(sql), secretBox, clock, audit };
  const identity: IdentityDeps = {
    ...createPgIdentity(sql),
    oidc: opts.oidc ?? unconfiguredOidc,
    policy: { loginGroup: config.oidc.loginGroup, adminGroup: config.oidc.adminGroup },
    audit,
    clock,
  };
  const connections: ConnectionsDeps = {
    ...createPgConnections(sql),
    servers: createCatalogServerDirectory(catalog),
    oauth: new OpenidOAuthClient(clock),
    pkce: nodePkce,
    secretBox,
    flights: createSingleFlight(),
    clock,
    audit,
    redirectUri: new URL('/api/connections/callback', config.publicUrl).toString(),
  };
  const gateway: GatewayDeps = {
    ...createPgGateway(sql),
    catalog: createCatalogToolCatalog(catalog),
    credentials: createConnectionCredentials(connections),
    validator: new AjvArgsValidator(logger),
    upstream,
    clock,
    logger,
    publicUrl: config.publicUrl.replace(/\/+$/, ''),
    maxResponseBytes: config.upstreamMaxResponseBytes,
  };
  return {
    audit,
    catalog,
    catalogSync: {
      ...catalog,
      upstream,
      credentials: personalSyncCredentials({ connections, identity }),
      maxResponseBytes: config.upstreamMaxResponseBytes,
      logger,
    },
    connections,
    clock,
    upstream,
    search: {
      logger,
      ...createPgSearch(sql),
      tools: createCatalogToolReader(catalog),
      embedder: opts.embedder ?? createEmbedder(config),
      minSimilarity: config.searchMinSimilarity,
    },
    gateway,
    logger,
    config,
    sql,
    identity,
  };
}
